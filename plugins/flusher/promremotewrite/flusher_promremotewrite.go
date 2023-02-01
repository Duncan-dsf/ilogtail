package promremotewrite

import (
	"bytes"
	"context"
	"io"
	"math"
	"net/http"
	"time"

	"github.com/VictoriaMetrics/VictoriaMetrics/lib/prompbmarshal"
	"github.com/alibaba/ilogtail/pkg/logger"
	"github.com/alibaba/ilogtail/pkg/models"
	"github.com/alibaba/ilogtail/pkg/pipeline"
	"github.com/golang/snappy"
)

const (
	alarmType = "promremotewrite"
)

type FlusherPromRemoteWrite struct {
	Endpoint    string            // RemoteURL to request
	Headers     map[string]string // Headers to append to the http request
	Query       map[string]string // Query parameters to append to the http request
	Timeout     time.Duration     // Request timeout, default is 60s
	SeriesLimit int
	// Retry       retryConfig          // Retry strategy, default is retry 3 times with delay time begin from 1second, max to 30 seconds
	Concurrency int // How many requests can be performed in concurrent

	workQueue chan []byte
	cli       *http.Client
}

// Init called for init some system resources, like socket, mutex...
func (f *FlusherPromRemoteWrite) Init(ctx pipeline.Context) error {
	f.workQueue = make(chan []byte, 100)
	if f.SeriesLimit == 0 {
		f.SeriesLimit = 1000
	}
	if f.Concurrency == 0 {
		f.Concurrency = 2
	}
	f.cli = &http.Client{
		Transport: &http.Transport{
			MaxIdleConnsPerHost: f.Concurrency * 2,
			IdleConnTimeout:     time.Minute * 5,
		},
		Timeout: time.Second,
	}
	f.consume()
	return nil
}

func (f *FlusherPromRemoteWrite) consume() {
	for i := 0; i < f.Concurrency; i++ {
		go func(i int) {
			ctx := context.Background()
			ctx = context.WithValue(ctx, "prwWorker", i)
			for req := range f.workQueue {
				if len(req) == 0 {
					continue
				}

				req, err := http.NewRequest("POST", f.Endpoint, bytes.NewReader(req))
				if err != nil {
					logger.Error(ctx, alarmType, "new http req err", err)
					continue
				}

				query := req.URL.Query()
				for k, v := range f.Query {
					query.Set(k, v)
				}
				req.URL.RawQuery = query.Encode()

				for k, v := range f.Headers {
					req.Header.Set(k, v)
				}

				req.Header.Set("X-Prometheus-Remote-Write-Version", "0.1.0")
				req.Header.Set("Content-Type", "application/x-protobuf")
				req.Header.Add("Content-Encoding", "snappy")
				req.Header.Set("User-Agent", "oneagent prom remote write flusher")

				sendRequest := func(request *http.Request) {
					response, err := f.cli.Do(request)
					if err != nil {
						logger.Error(ctx, alarmType, "do request error", err)
						return
					}
					defer response.Body.Close()
					_, err = io.ReadAll(response.Body)
					if err != nil {
						logger.Error(ctx, alarmType, "read body error", err)
						return
					}
				}
				sendRequest(req)
			}
		}(i)
	}
}

// Description returns a one-sentence description on the Input.
func (f *FlusherPromRemoteWrite) Description() string {
	panic("not implemented") // TODO: Implement
}

// IsReady checks if flusher is ready to accept more data.
// @projectName, @logstoreName, @logstoreKey: meta of the corresponding data.
// Note: If SetUrgent is called, please make some adjustment so that IsReady
//
//	can return true to accept more data in time and config instance can be
//	stopped gracefully.
func (f *FlusherPromRemoteWrite) IsReady(projectName string, logstoreName string, logstoreKey int64) bool {
	return true
}

// SetUrgent indicates the flusher that it will be destroyed soon.
// @flag indicates if main program (Logtail mostly) will exit after calling this.
//
// Note: there might be more data to flush after SetUrgent is called, and if flag
//
//	is true, these data will be passed to flusher through IsReady/Export before
//	program exits.
//
// Recommendation: set some state flags in this method to guide the behavior
//
//	of other methods.
func (f *FlusherPromRemoteWrite) SetUrgent(flag bool) {
}

// Stop stops flusher and release resources.
// It is time for flusher to do cleaning jobs, includes:
//  1. Export cached but not flushed data. For flushers that contain some kinds of
//     aggregation or buffering, it is important to flush cached out now, otherwise
//     data will lost.
//  2. Release opened resources: goroutines, file handles, connections, etc.
//  3. Maybe more, it depends.
//
// In a word, flusher should only have things that can be recycled by GC after this.
func (f *FlusherPromRemoteWrite) Stop() error {
	return nil
}

// Export data to destination, such as gRPC, console, file, etc.
// It is expected to return no error at most time because IsReady will be called
// before it to make sure there is space for next data.
func (f *FlusherPromRemoteWrite) Export(groupEvents []*models.PipelineGroupEvents, ctx pipeline.PipelineContext) error {

	sum := 0
	currentTs := int64(math.MaxInt64)
	writeReq := &prompbmarshal.WriteRequest{Timeseries: []prompbmarshal.TimeSeries{}}
	for i := range groupEvents {
		groupEvent := groupEvents[i]
		groupEvent.Group.GetMetadata()
		count := 0
		for j := range groupEvent.Events {
			event := groupEvent.Events[j]
			if event.GetType() != models.EventTypeMetric {
				continue
			}
			metricEvent, ok := event.(*models.Metric)
			if !ok {
				logger.Error(context.Background(), alarmType, "metricEvent type error", event.GetName())
				continue
			}
			if metricEvent.Timestamp > 0 && metricEvent.Timestamp < uint64(currentTs) {
				currentTs = int64(metricEvent.Timestamp)
			}
			writeReq.Timeseries = append(writeReq.Timeseries, *convEventToPrwTs(metricEvent))
			count++
			if count >= f.SeriesLimit {
				f.workQueue <- marshalData(writeReq)
				sum += count
				count = 0
			}
		}
		f.workQueue <- marshalData(writeReq)
		sum += count
	}
	logger.Info(context.Background(), "flusher total series", sum, "ts", time.Unix(currentTs/1e3, 0).String())
	return nil
}

func marshalData(req *prompbmarshal.WriteRequest) []byte {
	if len(req.Timeseries) == 0 {
		return nil
	}
	data, err := req.Marshal()
	if err != nil {
		logger.Error(context.Background(), alarmType, "pb marshal err", err)
		return nil
	}
	buf := make([]byte, len(data), cap(data))
	compressedData := snappy.Encode(buf, data)
	req.Timeseries = req.Timeseries[:0]
	return compressedData
}

func convEventToPrwTs(event *models.Metric) *prompbmarshal.TimeSeries {
	tss := &prompbmarshal.TimeSeries{
		Labels: convTagToLabels(event.Tags),
		Samples: []prompbmarshal.Sample{{
			Value:     event.Value.GetSingleValue(),
			Timestamp: int64(event.Timestamp),
		}},
	}
	return tss
}

func convTagToLabels(tags models.Tags) []prompbmarshal.Label {
	labels := make([]prompbmarshal.Label, 0, tags.Len())
	for k, v := range tags.Iterator() {

		labels = append(labels, prompbmarshal.Label{
			Name:  k,
			Value: v,
		})
	}
	return labels
}

func init() {
	pipeline.Flushers["flusher_promremotewirte"] = func() pipeline.Flusher {
		return &FlusherPromRemoteWrite{}
	}
}
