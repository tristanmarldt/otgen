package main

import (
	"fmt"
	mathrand "math/rand/v2"
	"strings"
	"time"

	collectorlogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	collectormetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	collectortracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

type emissionPayloads struct {
	Traces  []byte
	Metrics []byte
	Logs    []byte
}

type generatedServiceSpan struct {
	Service Service
	Span    *tracepb.Span
	Failed  bool
}

type generatedTrace struct {
	TraceID      []byte
	Config       Config
	ServiceSpans []generatedServiceSpan
	Groups       map[string][]*tracepb.Span
	Resources    map[string][]*commonpb.KeyValue
	Order        []string
}

func buildEmissionPayloads(cfg Config, root Service) (emissionPayloads, error) {
	cfg = normalizeConfig(cfg)
	if err := validateConfig(cfg); err != nil {
		return emissionPayloads{}, err
	}
	configuredRoot, ok := indexServices(cfg)[root.Name]
	if !ok {
		return emissionPayloads{}, fmt.Errorf("service %q is not configured", root.Name)
	}
	root = configuredRoot
	now := time.Now()
	var out emissionPayloads

	var trace *generatedTrace
	if root.hasSignal(signalSpans) {
		var err error
		trace, err = buildTrace(cfg, root, now)
		if err != nil {
			return emissionPayloads{}, err
		}
		out.Traces, err = marshalTrace(trace)
		if err != nil {
			return emissionPayloads{}, err
		}
	}

	if root.hasSignal(signalMetrics) {
		var err error
		out.Metrics, err = marshalMetrics(cfg, root, now)
		if err != nil {
			return emissionPayloads{}, err
		}
	}

	if root.hasSignal(signalLogs) {
		var err error
		if trace != nil {
			out.Logs, err = marshalCorrelatedLogs(cfg, trace, now)
		} else {
			out.Logs, err = marshalStandaloneLog(cfg, root, now)
		}
		if err != nil {
			return emissionPayloads{}, err
		}
	}
	return out, nil
}

func buildTrace(cfg Config, root Service, now time.Time) (*generatedTrace, error) {
	services := indexServices(cfg)
	if _, ok := services[root.Name]; !ok {
		return nil, fmt.Errorf("service %q is not configured", root.Name)
	}
	trace := &generatedTrace{
		TraceID:   randomBytes(16),
		Config:    cfg,
		Groups:    make(map[string][]*tracepb.Span),
		Resources: make(map[string][]*commonpb.KeyValue),
	}
	stack := make(map[string]bool)
	count := 0

	var build func(Service, []byte, time.Time) (*tracepb.Span, bool, error)
	build = func(svc Service, parentID []byte, start time.Time) (*tracepb.Span, bool, error) {
		if stack[svc.Name] {
			return nil, false, fmt.Errorf("service call cycle while generating %q", svc.Name)
		}
		stack[svc.Name] = true
		defer delete(stack, svc.Name)

		newCount := func() error {
			count++
			if count > 256 {
				return fmt.Errorf("generated trace exceeds 256 spans")
			}
			return nil
		}
		if err := newCount(); err != nil {
			return nil, false, err
		}
		failed := mathrand.IntN(100) < svc.FailureRate
		serviceID := randomBytes(8)
		span := newSpan(svc, trace.TraceID, serviceID, start, start, failed)
		span.ParentSpanId = append([]byte(nil), parentID...)
		trace.addServiceSpan(svc, span, failed)

		cursor := start.Add(5 * time.Millisecond)
		for i := 0; i < svc.ChildSpans; i++ {
			if err := newCount(); err != nil {
				return nil, false, err
			}
			childStart := cursor
			childEnd := childStart.Add(randomChildDuration())
			child := newChildSpan(svc.Name, trace.TraceID, randomBytes(8), serviceID, childStart, childEnd, i, failed)
			trace.addSpan(svc, child)
			cursor = childEnd.Add(2 * time.Millisecond)
		}

		for _, call := range svc.DownstreamCalls {
			target, ok := services[call]
			if !ok {
				return nil, false, fmt.Errorf("unknown downstream service %q", call)
			}
			if err := newCount(); err != nil {
				return nil, false, err
			}
			clientID := randomBytes(8)
			clientStart := cursor
			overhead := time.Duration(1+mathrand.IntN(5)) * time.Millisecond
			targetStart := clientStart.Add(overhead)
			targetSpan, targetFailed, err := build(target, clientID, targetStart)
			if err != nil {
				return nil, false, err
			}
			clientEnd := time.Unix(0, int64(targetSpan.EndTimeUnixNano)).Add(overhead)
			client := &tracepb.Span{
				TraceId:           trace.TraceID,
				SpanId:            clientID,
				ParentSpanId:      serviceID,
				Name:              "call " + target.Name,
				Kind:              tracepb.Span_SPAN_KIND_CLIENT,
				StartTimeUnixNano: uint64(clientStart.UnixNano()),
				EndTimeUnixNano:   uint64(clientEnd.UnixNano()),
				Attributes:        downstreamCallAttrs(target, targetFailed),
			}
			applyFailure(client, targetFailed)
			trace.addSpan(svc, client)
			cursor = clientEnd.Add(2 * time.Millisecond)
		}

		end := cursor.Add(5 * time.Millisecond)
		span.EndTimeUnixNano = uint64(end.UnixNano())
		span.StartTimeUnixNano = uint64(start.UnixNano())
		return span, failed, nil
	}

	if _, _, err := build(root, nil, now); err != nil {
		return nil, err
	}
	return trace, nil
}

func downstreamCallAttrs(target Service, failed bool) []*commonpb.KeyValue {
	attrs := []*commonpb.KeyValue{
		stringAttr("peer.service", target.Name),
		stringAttr("server.address", target.Name+".service"),
		stringAttr("otgen.target.service", target.Name),
	}
	switch target.Template {
	case "http-server", "http-client":
		status := int64(200)
		if failed {
			status = 502
		}
		attrs = append(attrs,
			stringAttr("http.request.method", "GET"),
			stringAttr("url.full", "http://"+target.Name+".service/"),
			intAttr("http.response.status_code", status),
			stringAttr("network.protocol.version", "1.1"),
		)
	case "grpc":
		status := int64(0)
		if failed {
			status = 13
		}
		attrs = append(attrs,
			stringAttr("rpc.system", "grpc"),
			stringAttr("rpc.service", target.Name),
			stringAttr("rpc.method", "Handle"),
			intAttr("rpc.grpc.status_code", status),
		)
	}
	return attrs
}

func (t *generatedTrace) addServiceSpan(svc Service, span *tracepb.Span, failed bool) {
	t.ServiceSpans = append(t.ServiceSpans, generatedServiceSpan{Service: svc, Span: span, Failed: failed})
	t.addSpan(svc, span)
}

func (t *generatedTrace) addSpan(svc Service, span *tracepb.Span) {
	if _, ok := t.Groups[svc.Name]; !ok {
		t.Order = append(t.Order, svc.Name)
		t.Resources[svc.Name] = svcAttributes(t.Config, svc)
	}
	t.Groups[svc.Name] = append(t.Groups[svc.Name], span)
}

func traceExportRequest(trace *generatedTrace) *collectortracepb.ExportTraceServiceRequest {
	request := &collectortracepb.ExportTraceServiceRequest{}
	scope := &commonpb.InstrumentationScope{Name: "otgen", Version: "1.0.0"}
	for _, name := range trace.Order {
		request.ResourceSpans = append(request.ResourceSpans, &tracepb.ResourceSpans{
			Resource:   &resourcepb.Resource{Attributes: trace.Resources[name]},
			ScopeSpans: []*tracepb.ScopeSpans{{Scope: scope, Spans: trace.Groups[name]}},
		})
	}
	return request
}

func marshalTrace(trace *generatedTrace) ([]byte, error) {
	return proto.Marshal(traceExportRequest(trace))
}

func metricsExportRequest(cfg Config, svc Service, now time.Time) (*collectormetricspb.ExportMetricsServiceRequest, error) {
	failed := mathrand.IntN(100) < svc.FailureRate
	metrics := make([]*metricspb.Metric, 0, len(svc.Metrics)+1)
	for _, effective := range effectiveMetricConfigs(svc) {
		metric, err := syntheticMetric(effective, now)
		if err != nil {
			return nil, err
		}
		metrics = append(metrics, metric)
	}
	if svc.Mesh {
		metrics = append(metrics, istioMetrics(svc, now, failed)...)
	}
	// Infra templates that map to Dynatrace entities must also emit a metric
	// whose key matches system.* / process.*; the extension routes on the key.
	metrics = append(metrics, infraMetrics(svc, now)...)
	return &collectormetricspb.ExportMetricsServiceRequest{ResourceMetrics: []*metricspb.ResourceMetrics{{
		Resource: &resourcepb.Resource{Attributes: svcAttributes(cfg, svc)},
		ScopeMetrics: []*metricspb.ScopeMetrics{{
			Scope:   &commonpb.InstrumentationScope{Name: "otgen", Version: "1.0.0"},
			Metrics: metrics,
		}},
	}}}, nil
}

func syntheticMetric(effective MetricConfig, now time.Time) (*metricspb.Metric, error) {
	metric := &metricspb.Metric{Name: effective.Name, Unit: effective.Unit}
	switch effective.Type {
	case "sum":
		metric.Data = &metricspb.Metric_Sum{Sum: &metricspb.Sum{
			AggregationTemporality: metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA,
			IsMonotonic:            true,
			DataPoints: []*metricspb.NumberDataPoint{{
				TimeUnixNano: uint64(now.UnixNano()),
				Value:        &metricspb.NumberDataPoint_AsInt{AsInt: int64(now.UnixNano()%7 + 1)},
			}},
		}}
	case "gauge":
		metric.Data = &metricspb.Metric_Gauge{Gauge: &metricspb.Gauge{DataPoints: []*metricspb.NumberDataPoint{{
			TimeUnixNano: uint64(now.UnixNano()),
			Value:        &metricspb.NumberDataPoint_AsDouble{AsDouble: float64(mathrand.IntN(101))},
		}}}}
	case "histogram":
		observation := float64(5 + mathrand.IntN(496))
		bounds := []float64{10, 25, 50, 100, 250, 500}
		buckets := make([]uint64, len(bounds)+1)
		bucket := len(bounds)
		for i, bound := range bounds {
			if observation <= bound {
				bucket = i
				break
			}
		}
		buckets[bucket] = 1
		metric.Data = &metricspb.Metric_Histogram{Histogram: &metricspb.Histogram{
			AggregationTemporality: metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA,
			DataPoints: []*metricspb.HistogramDataPoint{{
				TimeUnixNano:   uint64(now.UnixNano()),
				Count:          1,
				Sum:            &observation,
				ExplicitBounds: bounds,
				BucketCounts:   buckets,
			}},
		}}
	default:
		return nil, fmt.Errorf("unsupported metric type: %s", effective.Type)
	}
	return metric, nil
}

func marshalMetrics(cfg Config, svc Service, now time.Time) ([]byte, error) {
	req, err := metricsExportRequest(cfg, svc, now)
	if err != nil {
		return nil, err
	}
	return proto.Marshal(req)
}

func logsExportRequest(cfg Config, trace *generatedTrace, svc Service, now time.Time) (*collectorlogspb.ExportLogsServiceRequest, error) {
	if trace != nil {
		groups := make(map[string][]*logspb.LogRecord)
		order := make([]string, 0, len(trace.Order))
		for _, generated := range trace.ServiceSpans {
			if !generated.Service.hasSignal(signalLogs) {
				continue
			}
			name := generated.Service.Name
			if _, ok := groups[name]; !ok {
				order = append(order, name)
			}
			severity, text := logSeverity(generated.Service, generated.Failed)
			body := generated.Span.Name + " completed"
			event := "otgen.service.completed"
			outcome := "success"
			if generated.Failed {
				body = generated.Span.Name + " failed: simulated failure"
				event = "otgen.service.failed"
				outcome = "failure"
			}
			body = effectiveLogMessage(generated.Service, body)
			groups[name] = append(groups[name], &logspb.LogRecord{
				TimeUnixNano:         generated.Span.EndTimeUnixNano,
				ObservedTimeUnixNano: uint64(now.UnixNano()),
				TraceId:              append([]byte(nil), generated.Span.TraceId...),
				SpanId:               append([]byte(nil), generated.Span.SpanId...),
				SeverityNumber:       severity,
				SeverityText:         text,
				Body:                 &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: body}},
				Attributes:           []*commonpb.KeyValue{stringAttr("otgen.outcome", outcome), stringAttr("event.name", event)},
			})
		}
		request := &collectorlogspb.ExportLogsServiceRequest{}
		scope := &commonpb.InstrumentationScope{Name: "otgen", Version: "1.0.0"}
		for _, name := range order {
			request.ResourceLogs = append(request.ResourceLogs, &logspb.ResourceLogs{
				Resource:  &resourcepb.Resource{Attributes: resourceForTraceService(cfg, trace, name)},
				ScopeLogs: []*logspb.ScopeLogs{{Scope: scope, LogRecords: groups[name]}},
			})
		}
		return request, nil
	}
	// standalone path
	severity, text := logSeverity(svc, false)
	return &collectorlogspb.ExportLogsServiceRequest{ResourceLogs: []*logspb.ResourceLogs{{
		Resource: &resourcepb.Resource{Attributes: svcAttributes(cfg, svc)},
		ScopeLogs: []*logspb.ScopeLogs{{Scope: &commonpb.InstrumentationScope{Name: "otgen", Version: "1.0.0"}, LogRecords: []*logspb.LogRecord{{
			TimeUnixNano:         uint64(now.UnixNano()),
			ObservedTimeUnixNano: uint64(now.UnixNano()),
			SeverityNumber:       severity,
			SeverityText:         text,
			Body:                 &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: effectiveLogMessage(svc, svc.Name+" synthetic log")}},
		}}}},
	}}}, nil
}

func marshalCorrelatedLogs(cfg Config, trace *generatedTrace, now time.Time) ([]byte, error) {
	req, err := logsExportRequest(cfg, trace, Service{}, now)
	if err != nil {
		return nil, err
	}
	return proto.Marshal(req)
}

func resourceForTraceService(cfg Config, trace *generatedTrace, name string) []*commonpb.KeyValue {
	if attrs, ok := trace.Resources[name]; ok && len(attrs) > 0 {
		return attrs
	}
	if svc, ok := indexServices(cfg)[name]; ok {
		return svcAttributes(cfg, svc)
	}
	return nil
}

func marshalStandaloneLog(cfg Config, svc Service, now time.Time) ([]byte, error) {
	req, err := logsExportRequest(cfg, nil, svc, now)
	if err != nil {
		return nil, err
	}
	return proto.Marshal(req)
}

// buildOTLPJSON builds a sample OTLP payload for root and returns each signal
// as pretty-printed protojson. cfg must contain root in its Services list.
func buildOTLPJSON(cfg Config, root Service) (traces, metrics, logs string, err error) {
	cfg = normalizeConfig(cfg)
	if err = validateConfig(cfg); err != nil {
		return
	}
	configuredRoot, ok := indexServices(cfg)[root.Name]
	if !ok {
		err = fmt.Errorf("service %q is not configured", root.Name)
		return
	}
	root = configuredRoot
	now := time.Now()

	opts := protojson.MarshalOptions{Multiline: true, Indent: "  ", EmitUnpopulated: false}

	var trace *generatedTrace
	if root.hasSignal(signalSpans) {
		trace, err = buildTrace(cfg, root, now)
		if err != nil {
			return
		}
		var b []byte
		b, err = opts.Marshal(traceExportRequest(trace))
		if err != nil {
			return
		}
		traces = string(b)
	}

	if root.hasSignal(signalMetrics) {
		var req *collectormetricspb.ExportMetricsServiceRequest
		req, err = metricsExportRequest(cfg, root, now)
		if err != nil {
			return
		}
		var b []byte
		b, err = opts.Marshal(req)
		if err != nil {
			return
		}
		metrics = string(b)
	}

	if root.hasSignal(signalLogs) {
		var req *collectorlogspb.ExportLogsServiceRequest
		req, err = logsExportRequest(cfg, trace, root, now)
		if err != nil {
			return
		}
		var b []byte
		b, err = opts.Marshal(req)
		if err != nil {
			return
		}
		logs = string(b)
	}
	return
}

func logSeverity(svc Service, failed bool) (logspb.SeverityNumber, string) {
	severity := effectiveLogSeverity(svc)
	if failed && severity != "error" {
		severity = "error"
	}
	switch strings.ToLower(severity) {
	case "debug":
		return logspb.SeverityNumber_SEVERITY_NUMBER_DEBUG, "DEBUG"
	case "warn":
		return logspb.SeverityNumber_SEVERITY_NUMBER_WARN, "WARN"
	case "error":
		return logspb.SeverityNumber_SEVERITY_NUMBER_ERROR, "ERROR"
	default:
		return logspb.SeverityNumber_SEVERITY_NUMBER_INFO, "INFO"
	}
}
