package sdk

import (
	"github.com/aws/smithy-go/metrics"
	"github.com/prometheus/client_golang/prometheus"
)

type PrometheusMeter struct {
	metrics.Meter
}

// Create the prometheus metrics

func (*PrometheusMeter) Int64Counter(name string, opts ...metrics.InstrumentOption) (metrics.Int64Counter, error) {
	return &int64Counter{
		counter: prometheus.NewCounterVec(
			prometheus.CounterOpts{},
			[]string{},
		),
	}, nil
}
func (*PrometheusMeter) Int64UpDownCounter(name string, opts ...metrics.InstrumentOption) (metrics.Int64UpDownCounter, error) {
	return &int64Counter{
		counter: prometheus.NewCounterVec(
			prometheus.CounterOpts{},
			[]string{},
		),
	}, nil
}

func (*PrometheusMeter) Int64Gauge(name string, opts ...metrics.InstrumentOption) (metrics.Int64Gauge, error) {
	return &int64Gauge{}, nil
}
func (*PrometheusMeter) Int64Histogram(name string, opts ...metrics.InstrumentOption) (metrics.Int64Histogram, error) {
	return &int64Histogram{}, nil
}

func (*PrometheusMeter) Float64Counter(name string, opts ...metrics.InstrumentOption) (metrics.Float64Counter, error) {
	return &float64Counter{
		counter: prometheus.NewCounterVec(
			prometheus.CounterOpts{},
			[]string{},
		),
	}, nil
}
func (*PrometheusMeter) Float64UpDownCounter(name string, opts ...metrics.InstrumentOption) (metrics.Float64UpDownCounter, error) {
	return &float64Counter{}, nil
}
func (*PrometheusMeter) Float64Gauge(name string, opts ...metrics.InstrumentOption) (metrics.Float64Gauge, error) {
	return &float64Gauge{}, nil
}
func (*PrometheusMeter) Float64Histogram(name string, opts ...metrics.InstrumentOption) (metrics.Float64Histogram, error) {
	return &float64Histogram{}, nil
}

type PrometheusMeterProvider struct {
	registry prometheus.Registerer
}

func NewPrometheusMeterProvider(registry prometheus.Registerer) *PrometheusMeterProvider {
	return &PrometheusMeterProvider{
		registry: registry,
	}
}

func (p *PrometheusMeterProvider) Meter(scope string, opts ...metrics.MeterOption) metrics.Meter {
	return &PrometheusMeter{}
}
