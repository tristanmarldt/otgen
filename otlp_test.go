package main

import (
	"crypto/md5"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	collectormetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	collectortracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

func stringAttrValue(attrs []*commonpb.KeyValue, key string) string {
	for _, attr := range attrs {
		if attr.GetKey() == key {
			return attr.GetValue().GetStringValue()
		}
	}
	return ""
}

func payloadFor(t *testing.T, cfg Config, svc Service, kind signalKind) []byte {
	t.Helper()
	svc.Signals = []string{string(kind)}
	cfg.Services = []Service{svc}
	payloads, err := buildEmissionPayloads(normalizeConfig(cfg), svc)
	if err != nil {
		t.Fatalf("buildEmissionPayloads: %v", err)
	}
	switch kind {
	case signalSpans:
		return payloads.Traces
	case signalMetrics:
		return payloads.Metrics
	case signalLogs:
		return payloads.Logs
	default:
		t.Fatalf("unsupported signal %s", kind)
		return nil
	}
}

func TestBuildPayloadCreatesSpanForService(t *testing.T) {
	cfg := Config{Endpoint: "https://example.com"}
	svc := Service{
		Name:        "svc",
		SpanKind:    "server",
		FailureRate: 100,
		Signals:     []string{"spans"},
		Attributes: map[string]AttrValue{
			"mystr":  strAttrVal("hello"),
			"mybool": boolAttrVal(true),
		},
	}

	payload := payloadFor(t, cfg, svc, signalSpans)

	var req collectortracepb.ExportTraceServiceRequest
	if err := proto.Unmarshal(payload, &req); err != nil {
		t.Fatalf("failed to unmarshal trace payload: %v", err)
	}

	resourceSpans := req.GetResourceSpans()
	if len(resourceSpans) != 1 {
		t.Fatalf("expected 1 resource span, got %d", len(resourceSpans))
	}

	spans := resourceSpans[0].GetScopeSpans()[0].GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span (no children), got %d", len(spans))
	}

	span := spans[0]
	if span.GetKind() != tracepb.Span_SPAN_KIND_SERVER {
		t.Fatalf("expected server span kind, got %v", span.GetKind())
	}
	if span.GetStatus().GetCode() != tracepb.Status_STATUS_CODE_ERROR {
		t.Fatalf("expected error status (failureRate=100), got %v", span.GetStatus().GetCode())
	}

	// Check resource has service.name="svc"
	resource := resourceSpans[0].GetResource()
	found := false
	for _, attr := range resource.GetAttributes() {
		if attr.GetKey() == "service.name" {
			if sv := attr.GetValue().GetStringValue(); sv == "svc" {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("expected resource attribute service.name=svc")
	}
}

// TestK8sInfraTemplatesCarryDynatraceAttributes checks that every k8s-family
// template emits the attribute set Dynatrace's k8sattributesprocessor extracts
// and the Dynatrace Operator injects, so the payload maps onto the same
// Kubernetes entities as a real in-cluster collector would produce.
func TestK8sInfraTemplatesCarryDynatraceAttributes(t *testing.T) {
	required := []string{
		"k8s.cluster.name", "k8s.cluster.uid",
		"k8s.namespace.name", "k8s.node.name",
		"k8s.pod.name", "k8s.pod.uid", "k8s.pod.ip",
		"k8s.container.name",
		"k8s.deployment.name", "k8s.replicaset.name",
		"k8s.workload.kind", "k8s.workload.name",
	}

	for _, template := range []string{"k8s", "eks", "gke", "aks", "openshift"} {
		t.Run(template, func(t *testing.T) {
			attrs := infraDefaults(Service{Name: "svc", InfraTemplate: template})
			for _, key := range required {
				v, ok := attrs[key]
				if !ok {
					t.Errorf("missing %s", key)
					continue
				}
				if v.Type != "string" || v.Str == "" {
					t.Errorf("%s should be a non-empty string, got %+v", key, v)
				}
			}
			// Deployment → ReplicaSet → Pod names must stay consistent.
			rs := attrs["k8s.replicaset.name"].Str
			if !strings.HasPrefix(rs, attrs["k8s.deployment.name"].Str+"-") {
				t.Errorf("replicaset %q is not derived from deployment %q", rs, attrs["k8s.deployment.name"].Str)
			}
			if pod := attrs["k8s.pod.name"].Str; !strings.HasPrefix(pod, rs+"-") {
				t.Errorf("pod %q is not derived from replicaset %q", pod, rs)
			}
			if kind := attrs["k8s.workload.kind"].Str; kind != "Deployment" {
				t.Errorf("k8s.workload.kind = %q, want Deployment", kind)
			}
		})
	}
}

// md5hex returns the MD5 hex digest of s — mirrors hostID in otlp.go.
func md5hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

// TestOtelInfraTemplatesCarryEntityIdentityAttributes verifies that each OTel
// infra template includes the resource attributes Dynatrace requires to extract
// OTEL_HOST and OTEL_PROCESS Smartscape entities.
func TestOtelInfraTemplatesCarryEntityIdentityAttributes(t *testing.T) {
	hostRequired := []string{"host.id", "host.name", "telemetry.sdk.name"}
	processRequired := append(hostRequired, "process.executable.name")

	cases := []struct {
		template string
		required []string
	}{
		{"otel-host", hostRequired},
		{"otel-host-process", processRequired},
	}

	for _, tc := range cases {
		t.Run(tc.template, func(t *testing.T) {
			attrs := infraDefaults(Service{Name: "my-svc", InfraTemplate: tc.template})
			for _, key := range tc.required {
				v, ok := attrs[key]
				if !ok {
					t.Errorf("missing %s", key)
					continue
				}
				if v.Type != "string" || v.Str == "" {
					t.Errorf("%s should be a non-empty string, got %+v", key, v)
				}
			}
			// host.id must be MD5 of host.name
			wantID := md5hex(attrs["host.name"].Str)
			if got := attrs["host.id"].Str; got != wantID {
				t.Errorf("host.id = %q, want MD5(%q) = %q", got, attrs["host.name"].Str, wantID)
			}
			if _, hasProc := attrs["process.executable.name"]; hasProc {
				if attrs["process.executable.name"].Str != "my-svc" {
					t.Errorf("process.executable.name = %q, want my-svc", attrs["process.executable.name"].Str)
				}
			}
		})
	}
}

// TestHostCategoryTemplatesHaveHostID verifies that all four host-category
// templates now carry host.id (a 32-char MD5 of host.name).
func TestHostCategoryTemplatesHaveHostID(t *testing.T) {
	for _, tmpl := range []string{"host", "process", "otel-host", "otel-host-process"} {
		t.Run(tmpl, func(t *testing.T) {
			attrs := infraDefaults(Service{Name: "svc", InfraTemplate: tmpl})
			hn, ok := attrs["host.name"]
			if !ok {
				t.Fatal("missing host.name")
			}
			id, ok := attrs["host.id"]
			if !ok {
				t.Fatal("missing host.id")
			}
			if want := md5hex(hn.Str); id.Str != want {
				t.Errorf("host.id = %q, want MD5(%q) = %q", id.Str, hn.Str, want)
			}
		})
	}
}

// TestCustomHostAndProcessName verifies that HostName and ProcessName struct
// fields propagate correctly through infraDefaults.
func TestCustomHostAndProcessName(t *testing.T) {
	processTemplates := []string{"process", "otel-host-process"}
	hostTemplates := []string{"host", "otel-host"}

	for _, tmpl := range append(hostTemplates, processTemplates...) {
		t.Run(tmpl+"/custom-host", func(t *testing.T) {
			svc := Service{Name: "svc", InfraTemplate: tmpl, HostName: "my-custom-host"}
			attrs := infraDefaults(svc)
			if got := attrs["host.name"].Str; got != "my-custom-host" {
				t.Errorf("host.name = %q, want my-custom-host", got)
			}
			if want := md5hex("my-custom-host"); attrs["host.id"].Str != want {
				t.Errorf("host.id = %q, want MD5(my-custom-host) = %q", attrs["host.id"].Str, want)
			}
		})
	}

	for _, tmpl := range processTemplates {
		t.Run(tmpl+"/custom-process", func(t *testing.T) {
			svc := Service{Name: "svc", InfraTemplate: tmpl, ProcessName: "my-proc"}
			attrs := infraDefaults(svc)
			if got := attrs["process.executable.name"].Str; got != "my-proc" {
				t.Errorf("process.executable.name = %q, want my-proc", got)
			}
			if got := attrs["process.command_line"].Str; !strings.Contains(got, "my-proc") {
				t.Errorf("process.command_line = %q, want to contain my-proc", got)
			}
		})
		t.Run(tmpl+"/default-process-falls-back-to-service-name", func(t *testing.T) {
			svc := Service{Name: "svc", InfraTemplate: tmpl}
			attrs := infraDefaults(svc)
			if got := attrs["process.executable.name"].Str; got != "svc" {
				t.Errorf("process.executable.name = %q, want svc", got)
			}
		})
	}
}

// TestTemplateSpanAttributes verifies that each template produces the expected
// semantic-convention attributes on the root span.
func TestTemplateSpanAttributes(t *testing.T) {
	cfg := Config{Endpoint: "https://example.com"}

	cases := []struct {
		template    string
		wantAttrKey string // one mandatory attribute key per template
	}{
		{"http-server", "http.request.method"},
		{"http-client", "url.full"},
		{"db", "db.system.name"},
		{"messaging", "messaging.system"},
		{"grpc", "rpc.system"},
	}

	for _, tc := range cases {
		t.Run(tc.template, func(t *testing.T) {
			svc := Service{
				Name:     "tsvc",
				Template: tc.template,
				SpanKind: "client",
				Signals:  []string{"spans"},
			}
			payload := payloadFor(t, cfg, svc, signalSpans)
			var req collectortracepb.ExportTraceServiceRequest
			if err := proto.Unmarshal(payload, &req); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			spans := req.GetResourceSpans()[0].GetScopeSpans()[0].GetSpans()
			if len(spans) == 0 {
				t.Fatal("expected at least one span")
			}
			span := spans[0]
			// span name must NOT be the generic "<svc>.request"
			if strings.HasSuffix(span.GetName(), ".request") {
				t.Errorf("expected template span name, got %q", span.GetName())
			}
			// must contain the template-specific attribute
			found := false
			for _, attr := range span.GetAttributes() {
				if attr.GetKey() == tc.wantAttrKey {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("expected span attribute %q for template %q", tc.wantAttrKey, tc.template)
			}
		})
	}
}

func TestIstioSemanticsAddWorkloadContext(t *testing.T) {
	payload := payloadFor(t, Config{}, Service{Name: "checkout", Mesh: true}, signalSpans)
	var req collectortracepb.ExportTraceServiceRequest
	if err := proto.Unmarshal(payload, &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	resource := req.GetResourceSpans()[0].GetResource().GetAttributes()
	if got := stringAttrValue(resource, "service.istio.io/canonical-name"); got != "checkout" {
		t.Fatalf("canonical resource name = %q, want checkout", got)
	}
	span := req.GetResourceSpans()[0].GetScopeSpans()[0].GetSpans()[0]
	if got := stringAttrValue(span.GetAttributes(), "destination.workload.name"); got != "checkout" {
		t.Fatalf("destination workload = %q, want checkout", got)
	}
	if got := stringAttrValue(span.GetAttributes(), "connection.security_policy"); got != "mutual_tls" {
		t.Fatalf("security policy = %q, want mutual_tls", got)
	}
}

func TestIstioMetricsAddStandardMeshMetrics(t *testing.T) {
	payload := payloadFor(t, Config{}, Service{Name: "checkout", Mesh: true}, signalMetrics)
	var req collectormetricspb.ExportMetricsServiceRequest
	if err := proto.Unmarshal(payload, &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	metrics := req.GetResourceMetrics()[0].GetScopeMetrics()[0].GetMetrics()
	want := map[string]bool{
		"checkout.requests.total":             false,
		"istio_requests_total":                false,
		"istio_request_duration_milliseconds": false,
		"istio_request_bytes":                 false,
		"istio_response_bytes":                false,
	}
	for _, metric := range metrics {
		if _, ok := want[metric.GetName()]; ok {
			want[metric.GetName()] = true
		}
		if metric.GetName() == "istio_requests_total" {
			points := metric.GetSum().GetDataPoints()
			if got := stringAttrValue(points[0].GetAttributes(), "destination_service"); got != "checkout.default.svc.cluster.local" {
				t.Errorf("destination service = %q", got)
			}
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("missing metric %q", name)
		}
	}
}

// TestInfraTemplatesEmitEntityMetrics is the regression guard for the failure
// that made otel-host look correct on the wire but create no Dynatrace entity:
// the OTel host extension routes on the metric KEY, so a template with perfect
// resource attributes and no system.*/process.* metric extracts nothing.
func TestInfraTemplatesEmitEntityMetrics(t *testing.T) {
	now := time.Now()
	cases := []struct {
		template    string
		wantSystem  bool
		wantProcess bool
	}{
		{"otel-host", true, false},
		{"otel-host-process", true, true},
		{"k8s", false, false},
		{"", false, false},
	}

	for _, tc := range cases {
		t.Run(tc.template, func(t *testing.T) {
			var gotSystem, gotProcess bool
			for _, m := range infraMetrics(Service{Name: "svc", InfraTemplate: tc.template}, now) {
				if strings.HasPrefix(m.Name, "system.") {
					gotSystem = true
				}
				if strings.HasPrefix(m.Name, "process.") {
					gotProcess = true
				}
			}
			if gotSystem != tc.wantSystem {
				t.Errorf("system.* metric emitted = %v, want %v (otel:host extraction depends on it)", gotSystem, tc.wantSystem)
			}
			if gotProcess != tc.wantProcess {
				t.Errorf("process.* metric emitted = %v, want %v (otel:process extraction depends on it)", gotProcess, tc.wantProcess)
			}
		})
	}
}

// TestInfraMetricsAreStateless pins the shapes that let infraMetrics stay a
// pure function of (svc, now): gauges carry no start time, and sums are
// non-monotonic cumulative. A delta monotonic sum here would need its start
// timestamp chained to the previous emission, which otgen does not track.
func TestInfraMetricsAreStateless(t *testing.T) {
	for _, m := range infraMetrics(Service{Name: "svc", InfraTemplate: "otel-host-process"}, time.Now()) {
		switch data := m.Data.(type) {
		case *metricspb.Metric_Gauge:
			for _, dp := range data.Gauge.DataPoints {
				if dp.StartTimeUnixNano != 0 {
					t.Errorf("%s: gauge must not set StartTimeUnixNano", m.Name)
				}
			}
		case *metricspb.Metric_Sum:
			if data.Sum.IsMonotonic {
				t.Errorf("%s: monotonic sums need per-emission delta state; use a gauge or a non-monotonic sum", m.Name)
			}
			if data.Sum.AggregationTemporality != metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE {
				t.Errorf("%s: temporality = %v, want CUMULATIVE", m.Name, data.Sum.AggregationTemporality)
			}
			for _, dp := range data.Sum.DataPoints {
				if dp.StartTimeUnixNano == 0 {
					t.Errorf("%s: cumulative sum must set StartTimeUnixNano", m.Name)
				}
			}
		default:
			t.Errorf("%s: unexpected data type %T", m.Name, m.Data)
		}
		if m.Unit == "" {
			t.Errorf("%s: missing unit", m.Name)
		}
	}
}

// TestHostMemoryMetricsAgree guards the one cross-metric invariant: usage and
// utilization are derived from a single draw, so a host never reports 80% used
// on one chart and 30% on another.
func TestHostMemoryMetricsAgree(t *testing.T) {
	now := time.Now()
	var usedBytes, totalBytes int64
	var usedFraction float64
	for _, m := range infraMetrics(Service{Name: "svc", InfraTemplate: "otel-host"}, now) {
		switch m.Name {
		case "system.memory.usage":
			for _, dp := range m.GetSum().DataPoints {
				totalBytes += dp.GetAsInt()
				for _, kv := range dp.Attributes {
					if kv.Key == "state" && kv.Value.GetStringValue() == "used" {
						usedBytes = dp.GetAsInt()
					}
				}
			}
		case "system.memory.utilization":
			for _, dp := range m.GetGauge().DataPoints {
				for _, kv := range dp.Attributes {
					if kv.Key == "state" && kv.Value.GetStringValue() == "used" {
						usedFraction = dp.GetAsDouble()
					}
				}
			}
		}
	}
	if totalBytes != hostMemoryBytes {
		t.Errorf("memory states sum to %d, want %d", totalBytes, hostMemoryBytes)
	}
	if got := float64(usedBytes) / float64(hostMemoryBytes); got < usedFraction-0.001 || got > usedFraction+0.001 {
		t.Errorf("usage implies %.4f utilization, but utilization reports %.4f", got, usedFraction)
	}
}

// TestInfraMetricNamesMatchInfraMetrics keeps the UI's name list in step with
// what is actually emitted. infraMetricNames exists so the editor can describe
// a template without building throwaway protos on every render, and a stale
// copy would quietly advertise metrics the generator no longer sends.
func TestInfraMetricNamesMatchInfraMetrics(t *testing.T) {
	for _, template := range []string{"otel-host", "otel-host-process", "k8s", ""} {
		t.Run(template, func(t *testing.T) {
			var emitted []string
			for _, m := range infraMetrics(Service{Name: "svc", InfraTemplate: template}, time.Now()) {
				emitted = append(emitted, m.Name)
			}
			declared := infraMetricNames(template)
			if len(declared) != len(emitted) {
				t.Fatalf("infraMetricNames lists %d metrics, infraMetrics emits %d\n  listed:  %v\n  emitted: %v",
					len(declared), len(emitted), declared, emitted)
			}
			for i := range emitted {
				if declared[i] != emitted[i] {
					t.Errorf("index %d: listed %q, emitted %q", i, declared[i], emitted[i])
				}
			}
		})
	}
}

// TestIstioMetricNamesMatchIstioMetrics is the same guard for the mesh series.
func TestIstioMetricNamesMatchIstioMetrics(t *testing.T) {
	var emitted []string
	for _, m := range istioMetrics(Service{Name: "svc"}, time.Now(), false) {
		emitted = append(emitted, m.Name)
	}
	if len(istioMetricNames) != len(emitted) {
		t.Fatalf("istioMetricNames = %v, istioMetrics emits %v", istioMetricNames, emitted)
	}
	for i := range emitted {
		if istioMetricNames[i] != emitted[i] {
			t.Errorf("index %d: listed %q, emitted %q", i, istioMetricNames[i], emitted[i])
		}
	}
}

// TestOtelHostsDoNotCollideByDefault guards the failure mode the shared
// "otel-host-01" default produced once these templates started creating
// entities: host.id is an MD5 of host.name, so two otel-host services left at
// their default merged into one Dynatrace entity fed by two independent
// system.* streams, which charts as noise.
func TestOtelHostsDoNotCollideByDefault(t *testing.T) {
	for _, tmpl := range []string{"otel-host", "otel-host-process"} {
		t.Run(tmpl, func(t *testing.T) {
			a := infraDefaults(Service{Name: "checkout", InfraTemplate: tmpl})
			b := infraDefaults(Service{Name: "inventory", InfraTemplate: tmpl})
			if a["host.name"].Str == b["host.name"].Str {
				t.Errorf("two services share host.name %q by default", a["host.name"].Str)
			}
			if a["host.id"].Str == b["host.id"].Str {
				t.Error("two services share host.id by default — they would merge into one entity")
			}
		})
	}

	// Sharing a host stays possible, it just has to be asked for: this is how
	// you model several processes on one machine.
	shared := "shared-host"
	a := infraDefaults(Service{Name: "checkout", InfraTemplate: "otel-host-process", HostName: shared})
	b := infraDefaults(Service{Name: "inventory", InfraTemplate: "otel-host-process", HostName: shared})
	if a["host.id"].Str != b["host.id"].Str {
		t.Error("explicit identical host names should produce one shared host entity")
	}
	if a["process.executable.name"].Str == b["process.executable.name"].Str {
		t.Error("processes on a shared host must stay distinct")
	}
}

// TestNameOverridesClearedForUnrelatedTemplates stops host/process names being
// persisted onto templates that never read them.
func TestNameOverridesClearedForUnrelatedTemplates(t *testing.T) {
	svc := normalizeService(Service{
		Name: "svc", InfraTemplate: "k8s",
		HostName: "leftover-host", ProcessName: "leftover-proc",
	})
	if svc.HostName != "" || svc.ProcessName != "" {
		t.Errorf("k8s kept HostName=%q ProcessName=%q", svc.HostName, svc.ProcessName)
	}

	// A host template keeps its host name but not a process name it cannot use.
	svc = normalizeService(Service{
		Name: "svc", InfraTemplate: "otel-host",
		HostName: "web-01", ProcessName: "leftover-proc",
	})
	if svc.HostName != "web-01" {
		t.Errorf("HostName = %q, want web-01", svc.HostName)
	}
	if svc.ProcessName != "" {
		t.Errorf("otel-host has no process, but kept ProcessName=%q", svc.ProcessName)
	}

	// The template that uses both keeps both.
	svc = normalizeService(Service{
		Name: "svc", InfraTemplate: "otel-host-process",
		HostName: "web-01", ProcessName: "java",
	})
	if svc.HostName != "web-01" || svc.ProcessName != "java" {
		t.Errorf("otel-host-process dropped names: %+v", svc)
	}
}
