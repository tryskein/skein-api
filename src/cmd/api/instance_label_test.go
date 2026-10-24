package main

// instance_label_test.go — sk-674.18, test matrix row 9 (observability: the
// installation label, SAAS-PLAN 10, K-24).
//
// Reference: K-24 — every metric carries the constant label
// skein_instance=<SKEIN_INSTANCE_NAME>, and every log record the attribute
// skein_instance with the same value. Observed through the production
// /metrics handler (metricsHandler, what buildRouter mounts) and the
// production logger constructor (newLogger, what setupLogger installs).

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"skein-api/internal/config"
)

const labelTestInstance = "label-test-instance"

// scrape serves GET /metrics through the production handler.
func scrape(t *testing.T) string {
	t.Helper()
	rec := httptest.NewRecorder()
	metricsHandler(labelTestInstance).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /metrics = %d", rec.Code)
	}
	return rec.Body.String()
}

// TestMetrics_CarryTheInstanceLabel: a counter incremented after an
// operation is scraped with skein_instance=<instance name>, and so is every
// other series of the registry — the label sits on the registry's output,
// not on one metric.
func TestMetrics_CarryTheInstanceLabel(t *testing.T) {
	counter := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "skein_instance_label_probe_total",
		Help: "Test-only probe for the instance label.",
	})
	if err := prometheus.DefaultRegisterer.Register(counter); err != nil {
		t.Fatalf("registering the probe counter: %v", err)
	}
	t.Cleanup(func() { prometheus.DefaultRegisterer.Unregister(counter) })

	counter.Inc()
	body := scrape(t)

	want := `skein_instance_label_probe_total{skein_instance="` + labelTestInstance + `"} 1`
	if !strings.Contains(body, want+"\n") {
		t.Errorf("/metrics lacks %q — the incremented counter is not exported with the instance label", want)
	}

	samples := 0
	for line := range strings.SplitSeq(body, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		samples++
		if !strings.Contains(line, `skein_instance="`+labelTestInstance+`"`) {
			t.Errorf("series without the instance label: %s", line)
		}
	}
	if samples < 2 {
		t.Fatalf("/metrics exported %d samples — the check above saw almost nothing", samples)
	}
}

// TestLogger_RecordsCarryTheInstanceAttribute: logfmt (below error) and JSON
// (error and above) records of the service logger both carry
// skein_instance=<instance name>.
func TestLogger_RecordsCarryTheInstanceAttribute(t *testing.T) {
	var buf bytes.Buffer
	cfg := &config.Config{ServiceName: "skein-api", InstanceName: labelTestInstance}
	logger := newLogger(cfg, &buf, slog.LevelInfo)

	logger.Info("instance label probe")
	logger.Error("instance label probe failed")

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("logger wrote %d lines, want 2:\n%s", len(lines), buf.String())
	}
	if !strings.Contains(lines[0], "skein_instance="+labelTestInstance) {
		t.Errorf("info record lacks skein_instance=%s: %s", labelTestInstance, lines[0])
	}
	var record map[string]any
	if err := json.Unmarshal([]byte(lines[1]), &record); err != nil {
		t.Fatalf("error record is not JSON: %v\n%s", err, lines[1])
	}
	if record["skein_instance"] != labelTestInstance {
		t.Errorf("error record skein_instance = %v, want %q", record["skein_instance"], labelTestInstance)
	}
}

// TestLogger_ConfigFailureCarriesTheInstanceAttribute — fix of review
// finding 7: the record of a refused configuration, written before the
// service logger exists, carries the installation label too — the value of
// SKEIN_INSTANCE_NAME, or Load's default when it is blank.
func TestLogger_ConfigFailureCarriesTheInstanceAttribute(t *testing.T) {
	for _, tc := range []struct{ env, want string }{
		{labelTestInstance, labelTestInstance},
		{"   ", "skein"},
		{"", "skein"},
	} {
		t.Setenv("SKEIN_INSTANCE_NAME", tc.env)
		var buf bytes.Buffer
		logConfigFailure(&buf, errors.New("contradictory settings"))
		var record map[string]any
		if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &record); err != nil {
			t.Fatalf("SKEIN_INSTANCE_NAME=%q: the failure record is not one JSON line: %v\n%s", tc.env, err, buf.String())
		}
		if record["skein_instance"] != tc.want {
			t.Errorf("SKEIN_INSTANCE_NAME=%q: skein_instance = %v, want %q", tc.env, record["skein_instance"], tc.want)
		}
		if record["level"] != "ERROR" {
			t.Errorf("SKEIN_INSTANCE_NAME=%q: level = %v, want ERROR", tc.env, record["level"])
		}
	}
}
