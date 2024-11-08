/*
Copyright The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package metrics

import (
	"io"

	awsmetrics "github.com/aws/aws-sdk-go-v2/aws/middleware/private/metrics"
	"github.com/aws/aws-sdk-go-v2/aws/middleware/private/metrics/emf"
	"github.com/samber/lo"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/karpenter/pkg/metrics"
)

type EMF struct {
	writer     io.Writer
	namespace  string
	name       string
	dimensions []string
}

func NewEMF(writer io.Writer, namespace, name string, dimensions []string) *EMF {
	return &EMF{writer: writer, namespace: namespace, name: name, dimensions: dimensions}
}

func (e *EMF) withProperties(entry emf.Entry, labels map[string]string) emf.Entry {
	d := sets.New(e.dimensions...)
	for k, v := range labels {
		if d.Has(k) {
			entry.AddDimension(k, v)
		} else {
			entry.AddProperty(k, v)
		}
	}
	return entry
}

type EMFCounter struct {
	*EMF
}

func NewEMFCounter(writer io.Writer, namespace, name string, dimensions []string) metrics.CounterMetric {
	return &EMFCounter{EMF: NewEMF(writer, namespace, name, dimensions)}
}

func (e *EMFCounter) Inc(labels map[string]string) {
	entry := e.withProperties(emf.NewEntry(e.namespace, awsmetrics.DefaultSerializer{}), labels)
	entry.AddMetric(e.name, 1)
	lo.Must(e.writer.Write([]byte(lo.Must(entry.Build()) + "\n")))
}

func (e *EMFCounter) Add(v float64, labels map[string]string) {
	entry := e.withProperties(emf.NewEntry(e.namespace, awsmetrics.DefaultSerializer{}), labels)
	entry.AddMetric(e.name, v)
	lo.Must(e.writer.Write([]byte(lo.Must(entry.Build()) + "\n")))
}

func (e *EMFCounter) Delete(_ map[string]string) {
	return
}

func (e *EMFCounter) DeletePartialMatch(_ map[string]string) {
	return
}

func (e *EMF) Reset() {
	return
}

type EMFGauge struct {
	*EMF
}

func NewEMFGauge(writer io.Writer, namespace, name string, dimensions []string) metrics.CounterMetric {
	return &EMFCounter{EMF: NewEMF(writer, namespace, name, dimensions)}
}

func (e *EMFGauge) Set(v float64, labels map[string]string) {
	entry := e.withProperties(emf.NewEntry(e.namespace, awsmetrics.DefaultSerializer{}), labels)
	entry.AddMetric(e.name, v)
	lo.Must(e.writer.Write([]byte(lo.Must(entry.Build()) + "\n")))
}

func (e *EMFGauge) Delete(_ map[string]string) {
	return
}

func (e *EMFGauge) DeletePartialMatch(_ map[string]string) {
	return
}

func (e *EMFGauge) Reset() {
	return
}

type EMFObservation struct {
	*EMF
}

func NewEMFObservation(writer io.Writer, namespace, name string, dimensions []string) metrics.ObservationMetric {
	return &EMFObservation{EMF: NewEMF(writer, namespace, name, dimensions)}
}

func (e *EMFObservation) Observe(v float64, labels map[string]string) {
	entry := e.withProperties(emf.NewEntry(e.namespace, awsmetrics.DefaultSerializer{}), labels)
	entry.AddMetric(e.name, v)
	lo.Must(e.writer.Write([]byte(lo.Must(entry.Build()) + "\n")))
}

func (e *EMFObservation) Delete(_ map[string]string) {
	return
}

func (e *EMFObservation) DeletePartialMatch(_ map[string]string) {
	return
}

func (e *EMFObservation) Reset() {
	return
}
