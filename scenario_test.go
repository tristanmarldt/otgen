package main

import (
	"strings"
	"testing"

	collectorlogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	collectormetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	collectortracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

func graphConfig(callsA, callsB []string) Config {
	return normalizeConfig(Config{Services: []Service{
		{Name: "a", SpanKind: "server", Signals: nil, DownstreamCalls: callsA},
		{Name: "b", SpanKind: "server", Signals: nil, DownstreamCalls: callsB},
		{Name: "c", SpanKind: "server", Signals: nil},
	}})
}

func TestServiceCallValidation(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want string
	}{
		{"unknown", graphConfig([]string{"missing"}, nil), "unknown service"},
		{"self", graphConfig([]string{"a"}, nil), "cannot call itself"},
		{"duplicate", graphConfig([]string{"b", "b"}, nil), "duplicate target"},
		{"cycle", graphConfig([]string{"b"}, []string{"a"}), "service call cycle: a -> b -> a"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateConfig(tc.cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("validateConfig error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestServiceReferencesRenameAndSort(t *testing.T) {
	cfg := graphConfig(nil, nil)
	cfg.Services[0].DownstreamCalls = []string{"c"}
	cfg.Services[1].DownstreamCalls = []string{"c"}
	if got := serviceReferrers(cfg, "c"); strings.Join(got, ",") != "a,b" {
		t.Fatalf("referrers = %v, want [a b]", got)
	}
	renameServiceReferences(&cfg, "c", "d")
	if got := cfg.Services[0].DownstreamCalls[0]; got != "d" {
		t.Fatalf("renamed reference = %q, want d", got)
	}
}

func TestDistributedTraceAndCorrelatedLogs(t *testing.T) {
	cfg := normalizeConfig(Config{Attributes: map[string]AttrValue{"env": strAttrVal("test")}, Services: []Service{
		{Name: "a", SpanKind: "server", Signals: nil, DownstreamCalls: []string{"b"}},
		{Name: "b", SpanKind: "server", Signals: nil, LogSeverity: "debug"},
	}})
	payloads, err := buildEmissionPayloads(cfg, cfg.Services[0])
	if err != nil {
		t.Fatalf("buildEmissionPayloads: %v", err)
	}
	var traces collectortracepb.ExportTraceServiceRequest
	if err := proto.Unmarshal(payloads.Traces, &traces); err != nil {
		t.Fatalf("decode traces: %v", err)
	}
	if got := len(traces.ResourceSpans); got != 2 {
		t.Fatalf("resource groups = %d, want 2", got)
	}
	aSpans := traces.ResourceSpans[0].ScopeSpans[0].Spans
	bSpans := traces.ResourceSpans[1].ScopeSpans[0].Spans
	if len(aSpans) != 2 || len(bSpans) != 1 {
		t.Fatalf("spans per resource = %d/%d, want 2/1", len(aSpans), len(bSpans))
	}
	if stringAttrValue(traces.ResourceSpans[0].Resource.Attributes, "env") != "test" {
		t.Fatal("global resource attribute missing from trace")
	}
	root, client, target := aSpans[0], aSpans[1], bSpans[0]
	if root.ParentSpanId != nil && len(root.ParentSpanId) != 0 {
		t.Fatal("root span has a parent")
	}
	if string(client.ParentSpanId) != string(root.SpanId) || string(target.ParentSpanId) != string(client.SpanId) {
		t.Fatal("distributed parent chain is not root -> client -> target")
	}
	for _, span := range []*tracepb.Span{root, client, target} {
		if string(span.TraceId) != string(root.TraceId) || len(span.SpanId) != 8 {
			t.Fatal("trace/span IDs are not shared and unique-sized")
		}
		if span.EndTimeUnixNano < span.StartTimeUnixNano {
			t.Fatal("span has negative duration")
		}
	}

	var logs collectorlogspb.ExportLogsServiceRequest
	if err := proto.Unmarshal(payloads.Logs, &logs); err != nil {
		t.Fatalf("decode logs: %v", err)
	}
	if len(logs.ResourceLogs) != 2 {
		t.Fatalf("log resource groups = %d, want 2", len(logs.ResourceLogs))
	}
	for _, group := range logs.ResourceLogs {
		for _, record := range group.ScopeLogs[0].LogRecords {
			if len(record.TraceId) != 16 || len(record.SpanId) != 8 {
				t.Fatal("correlated log IDs missing")
			}
			if record.SeverityText == "DEBUG" && record.SeverityNumber != logspb.SeverityNumber_SEVERITY_NUMBER_DEBUG {
				t.Fatal("debug log has wrong severity number")
			}
		}
	}
}

func TestMetricTypes(t *testing.T) {
	for _, typ := range []string{"sum", "gauge", "histogram"} {
		t.Run(typ, func(t *testing.T) {
			cfg := normalizeConfig(Config{Services: []Service{{Name: "svc", Signals: []string{"metrics"}, Metric: &MetricConfig{Type: typ, Name: "custom", Unit: "unit"}}}})
			payloads, err := buildEmissionPayloads(cfg, cfg.Services[0])
			if err != nil {
				t.Fatalf("buildEmissionPayloads: %v", err)
			}
			var request collectormetricspb.ExportMetricsServiceRequest
			if err := proto.Unmarshal(payloads.Metrics, &request); err != nil {
				t.Fatalf("decode metrics: %v", err)
			}
			metric := request.ResourceMetrics[0].ScopeMetrics[0].Metrics[0]
			if metric.Name != "custom" || metric.Unit != "unit" {
				t.Fatalf("metric identity = %s/%s", metric.Name, metric.Unit)
			}
			switch typ {
			case "sum":
				if metric.GetSum() == nil || !metric.GetSum().IsMonotonic {
					t.Fatal("sum metric is not monotonic")
				}
			case "gauge":
				if metric.GetGauge() == nil || metric.GetGauge().DataPoints[0].GetAsDouble() < 0 || metric.GetGauge().DataPoints[0].GetAsDouble() > 100 {
					t.Fatal("gauge value outside range")
				}
			case "histogram":
				hist := metric.GetHistogram()
				if hist == nil || hist.DataPoints[0].Count != 1 || len(hist.DataPoints[0].BucketCounts) != 7 {
					t.Fatal("histogram shape is invalid")
				}
			}
		})
	}
}

func TestMultipleMetricsAreEmitted(t *testing.T) {
	cfg := normalizeConfig(Config{Services: []Service{{
		Name:    "svc",
		Signals: []string{"metrics"},
		Metrics: []MetricConfig{
			{Type: "sum", Name: "requests.total", Unit: "1"},
			{Type: "gauge", Name: "queue.depth", Unit: "1"},
			{Type: "histogram", Name: "request.duration", Unit: "ms"},
		},
	}}})
	payloads, err := buildEmissionPayloads(cfg, cfg.Services[0])
	if err != nil {
		t.Fatalf("buildEmissionPayloads: %v", err)
	}
	var request collectormetricspb.ExportMetricsServiceRequest
	if err := proto.Unmarshal(payloads.Metrics, &request); err != nil {
		t.Fatalf("decode metrics: %v", err)
	}
	metrics := request.ResourceMetrics[0].ScopeMetrics[0].Metrics
	if len(metrics) != 3 {
		t.Fatalf("metric count = %d, want 3", len(metrics))
	}
	for i, want := range []string{"requests.total", "queue.depth", "request.duration"} {
		if metrics[i].Name != want {
			t.Errorf("metric[%d].name = %q, want %q", i, metrics[i].Name, want)
		}
	}
}

func TestCustomLogMessageIsEmitted(t *testing.T) {
	cfg := normalizeConfig(Config{Services: []Service{{
		Name:        "svc",
		Signals:     []string{"logs"},
		LogSeverity: "warn",
		LogMessage:  "checkout queue is growing",
	}}})
	payloads, err := buildEmissionPayloads(cfg, cfg.Services[0])
	if err != nil {
		t.Fatalf("buildEmissionPayloads: %v", err)
	}
	var request collectorlogspb.ExportLogsServiceRequest
	if err := proto.Unmarshal(payloads.Logs, &request); err != nil {
		t.Fatalf("decode logs: %v", err)
	}
	record := request.ResourceLogs[0].ScopeLogs[0].LogRecords[0]
	if got := record.Body.GetStringValue(); got != "checkout queue is growing" {
		t.Fatalf("log body = %q", got)
	}
	if record.SeverityText != "WARN" {
		t.Fatalf("log severity = %q, want WARN", record.SeverityText)
	}
}

func TestDownstreamCallUsesTargetProtocolTemplate(t *testing.T) {
	for _, tc := range []struct {
		template string
		key      string
		value    string
	}{
		{"http-server", "http.request.method", "GET"},
		{"grpc", "rpc.system", "grpc"},
	} {
		t.Run(tc.template, func(t *testing.T) {
			cfg := normalizeConfig(Config{Services: []Service{
				{Name: "client", Signals: []string{"spans"}, DownstreamCalls: []string{"target"}},
				{Name: "target", Template: tc.template, Signals: []string{"spans"}},
			}})
			payloads, err := buildEmissionPayloads(cfg, cfg.Services[0])
			if err != nil {
				t.Fatalf("buildEmissionPayloads: %v", err)
			}
			var request collectortracepb.ExportTraceServiceRequest
			if err := proto.Unmarshal(payloads.Traces, &request); err != nil {
				t.Fatalf("decode traces: %v", err)
			}
			clientSpans := request.ResourceSpans[0].ScopeSpans[0].Spans
			if got := stringAttrValue(clientSpans[1].Attributes, tc.key); got != tc.value {
				t.Fatalf("call %s = %q, want %q", tc.key, got, tc.value)
			}
		})
	}
}
