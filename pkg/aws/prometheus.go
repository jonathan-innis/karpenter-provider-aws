package sdk

import (
	"context"

	"github.com/aws/smithy-go/metrics"
	"github.com/prometheus/client_golang/prometheus"
)

type int64Counter struct {
	counter *prometheus.CounterVec
}

var _ metrics.Int64Counter = (*int64Counter)(nil)
var _ metrics.Int64UpDownCounter = (*int64Counter)(nil)

func (i *int64Counter) Add(_ context.Context, v int64, opts ...metrics.RecordMetricOption) {
	i.counter.With(withMetricProps(opts...)).Add(float64(v))
}

type int64Gauge struct {
	gauge *prometheus.GaugeVec
}

var _ metrics.Int64Gauge = (*int64Gauge)(nil)

func (i *int64Gauge) Sample(_ context.Context, v int64, opts ...metrics.RecordMetricOption) {
	i.gauge.With(withMetricProps(opts...)).Set(float64(v))
}

type int64Histogram struct {
	histogram *prometheus.HistogramVec
}

var _ metrics.Int64Histogram = (*int64Histogram)(nil)

func (i *int64Histogram) Record(ctx context.Context, v int64, opts ...metrics.RecordMetricOption) {
	i.histogram.With(withMetricProps(opts...)).Observe(float64(v))
}

type float64Counter struct {
	counter *prometheus.CounterVec
}

var _ metrics.Float64Counter = (*float64Counter)(nil)
var _ metrics.Float64UpDownCounter = (*float64Counter)(nil)

func (i *float64Counter) Add(_ context.Context, v float64, opts ...metrics.RecordMetricOption) {
	i.counter.With(withMetricProps(opts...)).Add(v)
}

type float64Gauge struct {
	gauge *prometheus.GaugeVec
}

var _ metrics.Float64Gauge = (*float64Gauge)(nil)

func (i *float64Gauge) Sample(_ context.Context, v float64, opts ...metrics.RecordMetricOption) {
	i.gauge.With(withMetricProps(opts...)).Set(v)
}

type float64Histogram struct {
	histogram *prometheus.HistogramVec
}

var _ metrics.Float64Histogram = (*float64Histogram)(nil)

func (i *float64Histogram) Record(_ context.Context, v float64, opts ...metrics.RecordMetricOption) {
	i.histogram.With(withMetricProps(opts...)).Observe(v)
}

func withMetricProps(opts ...metrics.RecordMetricOption) prometheus.Labels {
	var o metrics.RecordMetricOptions
	for _, opt := range opts {
		opt(&o)
	}
	return toPrometheusLabels(o.Properties)
}
