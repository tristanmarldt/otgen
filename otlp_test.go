package main

import (
	"crypto/md5"
	"encoding/hex"
	"strings"
	"testing"

	collectormetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	collectortracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
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

// TestHostCategoryTemplatesHaveHostID verifies that all five host-category
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
