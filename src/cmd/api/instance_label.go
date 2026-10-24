package main

// Instance label on metrics and logs (SAAS-PLAN 10, K-24): several Skein
// installations — the SaaS, dedicated ones — share one monitoring stack, so
// every series and every log record says which installation produced it.

import (
	"net/http"
	"sort"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	dto "github.com/prometheus/client_model/go"
)

// instanceLabel is the metric label and slog attribute carrying
// SKEIN_INSTANCE_NAME. Not "instance": Prometheus sets that one to the scrape
// target and would rename ours to exported_instance.
const instanceLabel = "skein_instance"

// instanceGatherer adds the constant label instanceLabel=name to every metric
// g gathers.
//
// The label is applied at gather time rather than through
// prometheus.WrapRegistererWith because the metrics are registered by
// promauto at package initialisation, before main has read the instance name;
// a wrapped registerer would only reach collectors registered after it. Here
// every series of the registry carries the label, the Go runtime and process
// collectors included, whichever package registered it and whenever.
func instanceGatherer(name string, g prometheus.Gatherer) prometheus.Gatherer {
	return prometheus.GathererFunc(func() ([]*dto.MetricFamily, error) {
		mfs, err := g.Gather()
		for _, mf := range mfs {
			for _, m := range mf.GetMetric() {
				labelName, labelValue := instanceLabel, name
				m.Label = append(m.Label, &dto.LabelPair{Name: &labelName, Value: &labelValue})
				// The exposition format expects label pairs sorted by name,
				// which is how a registry returns them.
				sort.Slice(m.Label, func(i, j int) bool { return m.Label[i].GetName() < m.Label[j].GetName() })
			}
		}
		return mfs, err
	})
}

// metricsHandler serves /metrics: the default registry with the instance
// label, instrumented exactly like promhttp.Handler().
func metricsHandler(name string) http.Handler {
	return promhttp.InstrumentMetricHandler(
		prometheus.DefaultRegisterer,
		promhttp.HandlerFor(instanceGatherer(name, prometheus.DefaultGatherer), promhttp.HandlerOpts{}),
	)
}
