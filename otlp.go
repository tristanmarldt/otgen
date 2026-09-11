package main

import (
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	mathrand "math/rand/v2"
	"strings"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

// svcAttributes builds the resource attribute list for a service.
// Precedence (high → low): service.name > svc.Attributes > infraDefaults > cfg.Attributes (global).
func svcAttributes(cfg Config, svc Service) []*commonpb.KeyValue {
	merged := make(map[string]AttrValue, len(cfg.Attributes)+len(svc.Attributes)+8)
	mergeAttrs(merged, cfg.Attributes)
	mergeAttrs(merged, infraDefaults(svc))
	if svc.Mesh {
		mergeAttrs(merged, istioResourceAttrs(svc))
	}
	mergeAttrs(merged, svc.Attributes)
	merged["service.name"] = strAttrVal(svc.Name)
	return toOTLPAttributes(merged)
}

func istioResourceAttrs(svc Service) map[string]AttrValue {
	return map[string]AttrValue{
		"service.istio.io/canonical-name":     strAttrVal(svc.Name),
		"service.istio.io/canonical-revision": strAttrVal("v1"),
		"service.istio.io/workload-name":      strAttrVal(svc.Name),
	}
}

func istioWorkloads(svc Service) (source, destination string) {
	source, destination = svc.Name+"-client", svc.Name
	switch strings.ToLower(strings.TrimSpace(svc.SpanKind)) {
	case "client", "producer":
		source, destination = svc.Name, svc.Name+"-backend"
	}
	return source, destination
}

func istioSpanAttrs(svc Service) []*commonpb.KeyValue {
	source, destination := istioWorkloads(svc)
	return []*commonpb.KeyValue{
		stringAttr("source.workload.name", source),
		stringAttr("source.workload.namespace", "default"),
		stringAttr("source.workload.uid", "source-"+source),
		stringAttr("source.principal", "cluster.local/ns/default/sa/"+source),
		stringAttr("destination.workload.name", destination),
		stringAttr("destination.workload.namespace", "default"),
		stringAttr("destination.workload.uid", "destination-"+destination),
		stringAttr("destination.principal", "cluster.local/ns/default/sa/"+destination),
		stringAttr("destination.service.name", svc.Name),
		stringAttr("destination.service.namespace", "default"),
		stringAttr("source.cluster", "cluster-1"),
		stringAttr("destination.cluster", "cluster-1"),
		stringAttr("connection.security_policy", "mutual_tls"),
	}
}

func istioMetricAttrs(svc Service, failed bool) []*commonpb.KeyValue {
	source, destination := istioWorkloads(svc)
	reporter := "destination"
	if source == svc.Name {
		reporter = "source"
	}
	code := "200"
	if failed {
		code = "500"
	}
	return []*commonpb.KeyValue{
		stringAttr("reporter", reporter),
		stringAttr("source_workload", source),
		stringAttr("source_workload_namespace", "default"),
		stringAttr("source_canonical_service", source),
		stringAttr("source_canonical_revision", "v1"),
		stringAttr("source_principal", "cluster.local/ns/default/sa/"+source),
		stringAttr("destination_workload", destination),
		stringAttr("destination_workload_namespace", "default"),
		stringAttr("destination_canonical_service", destination),
		stringAttr("destination_canonical_revision", "v1"),
		stringAttr("destination_principal", "cluster.local/ns/default/sa/"+destination),
		stringAttr("destination_service", svc.Name+".default.svc.cluster.local"),
		stringAttr("destination_service_name", svc.Name),
		stringAttr("destination_service_namespace", "default"),
		stringAttr("response_code", code),
		stringAttr("response_flags", "-"),
		stringAttr("connection_security_policy", "mutual_tls"),
		stringAttr("source_cluster", "cluster-1"),
		stringAttr("destination_cluster", "cluster-1"),
	}
}

func istioMetrics(svc Service, now time.Time, failed bool) []*metricspb.Metric {
	attrs := istioMetricAttrs(svc, failed)
	duration := float64(randomRootDuration() / time.Millisecond)
	requestBytes := float64(512 + mathrand.IntN(4096))
	responseBytes := float64(1024 + mathrand.IntN(8192))
	return []*metricspb.Metric{
		{
			Name: "istio_requests_total", Unit: "1",
			Data: &metricspb.Metric_Sum{Sum: &metricspb.Sum{
				AggregationTemporality: metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA,
				IsMonotonic:            true,
				DataPoints: []*metricspb.NumberDataPoint{{
					TimeUnixNano: uint64(now.UnixNano()), Attributes: attrs,
					Value: &metricspb.NumberDataPoint_AsInt{AsInt: 1},
				}},
			}},
		},
		istioHistogram("istio_request_duration_milliseconds", "ms", now, duration, attrs),
		istioHistogram("istio_request_bytes", "By", now, requestBytes, attrs),
		istioHistogram("istio_response_bytes", "By", now, responseBytes, attrs),
	}
}

func istioHistogram(name, unit string, now time.Time, value float64, attrs []*commonpb.KeyValue) *metricspb.Metric {
	return &metricspb.Metric{
		Name: name, Unit: unit,
		Data: &metricspb.Metric_Histogram{Histogram: &metricspb.Histogram{
			AggregationTemporality: metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA,
			DataPoints: []*metricspb.HistogramDataPoint{{
				TimeUnixNano: uint64(now.UnixNano()), Attributes: attrs,
				Count: 1, Sum: &value, BucketCounts: []uint64{1}, ExplicitBounds: []float64{value},
			}},
		}},
	}
}

// k8sAttrs returns the Kubernetes resource attributes shared by every
// k8s-family template (vanilla, EKS, GKE, AKS, OpenShift).
//
// The set mirrors what Dynatrace's OTel collector extracts via the
// k8sattributesprocessor (pod name/uid/ip, workload names, namespace, node,
// cluster uid, container name) plus the k8s.workload.kind / k8s.workload.name
// pair the Dynatrace Operator injects through OTEL_RESOURCE_ATTRIBUTES.
//
// The pod is modelled as Deployment → ReplicaSet → Pod, so the names stay
// internally consistent rather than listing every workload kind at once.
func k8sAttrs(name, cluster, node string) map[string]AttrValue {
	const rsHash = "7d9f8b6c4"
	return map[string]AttrValue{
		"k8s.cluster.name":    strAttrVal(cluster),
		"k8s.cluster.uid":     strAttrVal("f47ac10b-58cc-4372-a567-0e02b2c3d479"),
		"k8s.namespace.name":  strAttrVal("default"),
		"k8s.node.name":       strAttrVal(node),
		"k8s.pod.name":        strAttrVal(name + "-" + rsHash + "-xzpqr"),
		"k8s.pod.uid":         strAttrVal("a1b2c3d4-e5f6-7890-abcd-ef1234567890"),
		"k8s.pod.ip":          strAttrVal("10.42.0.17"),
		"k8s.container.name":  strAttrVal(name),
		"k8s.deployment.name": strAttrVal(name),
		"k8s.replicaset.name": strAttrVal(name + "-" + rsHash),
		"k8s.workload.kind":   strAttrVal("Deployment"),
		"k8s.workload.name":   strAttrVal(name),
	}
}

// mergeAttrs copies extra over base and returns base.
func mergeAttrs(base, extra map[string]AttrValue) map[string]AttrValue {
	for k, v := range extra {
		base[k] = v
	}
	return base
}

// hostID returns the MD5 hex digest of name, giving a stable 32-char host.id.
func hostID(name string) string {
	sum := md5.Sum([]byte(name))
	return hex.EncodeToString(sum[:])
}

// effectiveHostName returns the user-supplied HostName when set, otherwise the
// per-template placeholder for host-category infra templates.
func effectiveHostName(svc Service) string {
	if svc.HostName != "" {
		return svc.HostName
	}
	switch svc.InfraTemplate {
	case "host":
		return "prod-server-01"
	case "process":
		return "localhost"
	default:
		return "otel-host-01"
	}
}

// effectiveProcessName returns the user-supplied ProcessName when set, otherwise
// svc.Name (the existing default for process.executable.name).
func effectiveProcessName(svc Service) string {
	if svc.ProcessName != "" {
		return svc.ProcessName
	}
	return svc.Name
}

func otelHostAttrs(hostName string) map[string]AttrValue {
	return map[string]AttrValue{
		"host.id":             strAttrVal(hostID(hostName)),
		"host.name":           strAttrVal(hostName),
		"host.arch":           strAttrVal("amd64"),
		"host.ip":             strAttrVal("192.168.1.100"),
		"host.cpu.model.name": strAttrVal("Intel(R) Xeon(R) CPU @ 2.20GHz"),
		"os.type":             strAttrVal("linux"),
		"os.name":             strAttrVal("Ubuntu"),
		"os.version":          strAttrVal("22.04"),
		"os.description":      strAttrVal("Ubuntu 22.04.3 LTS"),
		"os.build.id":         strAttrVal("22.04"),
		"telemetry.sdk.name":  strAttrVal("opentelemetry"),
	}
}

// infraDefaults returns resource attributes for the service's InfraTemplate.
// Values are deterministic so resource attributes stay stable across ticks.
// Users override specific values via svc.Attributes.
func infraDefaults(svc Service) map[string]AttrValue {
	name := svc.Name
	switch svc.InfraTemplate {

	case "k8s":
		return k8sAttrs(name, "my-cluster", "node-1")

	case "eks":
		return mergeAttrs(k8sAttrs(name, "my-eks-cluster", "ip-10-0-1-100.ec2.internal"), map[string]AttrValue{
			"cloud.provider":   strAttrVal("aws"),
			"cloud.platform":   strAttrVal("aws_eks"),
			"cloud.region":     strAttrVal("us-east-1"),
			"cloud.account.id": strAttrVal("123456789012"),
		})

	case "gke":
		return mergeAttrs(k8sAttrs(name, "my-gke-cluster", "gke-my-cluster-default-pool-abc12345-abcd"), map[string]AttrValue{
			"cloud.provider":   strAttrVal("gcp"),
			"cloud.platform":   strAttrVal("gcp_kubernetes_engine"),
			"cloud.region":     strAttrVal("us-central1"),
			"cloud.account.id": strAttrVal("my-gcp-project"),
		})

	case "aks":
		return mergeAttrs(k8sAttrs(name, "my-aks-cluster", "aks-nodepool1-12345678-vmss000000"), map[string]AttrValue{
			"cloud.provider":   strAttrVal("azure"),
			"cloud.platform":   strAttrVal("azure_aks"),
			"cloud.region":     strAttrVal("eastus"),
			"cloud.account.id": strAttrVal("12345678-1234-1234-1234-123456789012"),
		})

	case "ecs":
		return map[string]AttrValue{
			"aws.ecs.cluster.arn":    strAttrVal("arn:aws:ecs:us-east-1:123456789012:cluster/my-cluster"),
			"aws.ecs.task.arn":       strAttrVal("arn:aws:ecs:us-east-1:123456789012:task/my-cluster/abcdef1234567890abcdef12"),
			"aws.ecs.task.family":    strAttrVal(name),
			"aws.ecs.container.name": strAttrVal(name),
			"aws.ecs.container.arn":  strAttrVal("arn:aws:ecs:us-east-1:123456789012:container/my-cluster/abcdef1234567890abcdef12/a1b2c3d4-e5f6-7890-abcd-ef1234567890"),
			"cloud.provider":         strAttrVal("aws"),
			"cloud.platform":         strAttrVal("aws_ecs"),
			"cloud.region":           strAttrVal("us-east-1"),
			"cloud.account.id":       strAttrVal("123456789012"),
		}

	case "host":
		hn := effectiveHostName(svc)
		return map[string]AttrValue{
			"host.name":  strAttrVal(hn),
			"host.id":    strAttrVal(hostID(hn)),
			"host.type":  strAttrVal("m5.large"),
			"host.arch":  strAttrVal("amd64"),
			"os.type":    strAttrVal("linux"),
			"os.version": strAttrVal("5.15.0"),
		}

	case "docker":
		return map[string]AttrValue{
			"container.name":       strAttrVal(name),
			"container.id":         strAttrVal("a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2"),
			"container.image.name": strAttrVal(name),
			"container.image.tag":  strAttrVal("latest"),
			"host.name":            strAttrVal("docker-host-01"),
		}

	case "lambda":
		return map[string]AttrValue{
			"cloud.provider":   strAttrVal("aws"),
			"cloud.platform":   strAttrVal("aws_lambda"),
			"cloud.region":     strAttrVal("us-east-1"),
			"cloud.account.id": strAttrVal("123456789012"),
			"faas.name":        strAttrVal(name),
			"faas.version":     strAttrVal("$LATEST"),
			"faas.max_memory":  intAttrVal(512),
		}

	case "cloudfoundry":
		return map[string]AttrValue{
			"cloudfoundry.app.id":      strAttrVal("abc12345-def6-7890-abcd-ef1234567890"),
			"cloudfoundry.app.name":    strAttrVal(name),
			"cloudfoundry.space.name":  strAttrVal("development"),
			"cloudfoundry.org.name":    strAttrVal("my-org"),
			"cloudfoundry.instance.id": strAttrVal("0"),
		}

	case "process":
		hn := effectiveHostName(svc)
		pn := effectiveProcessName(svc)
		return map[string]AttrValue{
			"process.pid":             intAttrVal(12345),
			"process.executable.name": strAttrVal(pn),
			"process.command_line":    strAttrVal("/usr/bin/" + pn + " --config=/etc/" + pn + ".yaml"),
			"process.runtime.name":    strAttrVal("go"),
			"process.runtime.version": strAttrVal("1.24.0"),
			"host.name":               strAttrVal(hn),
			"host.id":                 strAttrVal(hostID(hn)),
		}

	case "otel-host":
		return otelHostAttrs(effectiveHostName(svc))

	case "otel-host-process":
		hn := effectiveHostName(svc)
		pn := effectiveProcessName(svc)
		return mergeAttrs(otelHostAttrs(hn), map[string]AttrValue{
			"process.executable.name": strAttrVal(pn),
			"process.pid":             intAttrVal(12345),
			"process.command_line":    strAttrVal("/usr/bin/" + pn + " --config=/etc/" + pn + ".yaml"),
		})

	case "openshift":
		return mergeAttrs(k8sAttrs(name, "my-ocp-cluster", "ocp-worker-1"), map[string]AttrValue{
			"cloud.platform": strAttrVal("openshift"),
		})

	case "containerd":
		return map[string]AttrValue{
			"container.runtime":    strAttrVal("containerd"),
			"container.name":       strAttrVal(name),
			"container.id":         strAttrVal("a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2"),
			"container.image.name": strAttrVal(name),
			"container.image.tag":  strAttrVal("latest"),
			"host.name":            strAttrVal("containerd-host-01"),
		}

	case "nomad":
		return map[string]AttrValue{
			"nomad.job.name":   strAttrVal(name),
			"nomad.task.name":  strAttrVal(name + "-task"),
			"nomad.namespace":  strAttrVal("default"),
			"nomad.datacenter": strAttrVal("dc1"),
			"nomad.alloc.id":   strAttrVal("abc12345-def6-7890-abcd-ef1234567890"),
		}

	case "azure-functions":
		return map[string]AttrValue{
			"cloud.provider":   strAttrVal("azure"),
			"cloud.platform":   strAttrVal("azure_functions"),
			"cloud.region":     strAttrVal("westeurope"),
			"cloud.account.id": strAttrVal("12345678-1234-1234-1234-123456789012"),
			"faas.name":        strAttrVal(name),
			"faas.version":     strAttrVal("1.0.0"),
		}

	case "gcp-functions":
		return map[string]AttrValue{
			"cloud.provider":   strAttrVal("gcp"),
			"cloud.platform":   strAttrVal("gcp_cloud_functions"),
			"cloud.region":     strAttrVal("europe-west1"),
			"cloud.account.id": strAttrVal("my-gcp-project"),
			"faas.name":        strAttrVal(name),
			"faas.version":     strAttrVal("1"),
		}

	case "azure-container-apps":
		return map[string]AttrValue{
			"cloud.provider":       strAttrVal("azure"),
			"cloud.platform":       strAttrVal("azure_container_apps"),
			"cloud.region":         strAttrVal("westeurope"),
			"cloud.account.id":     strAttrVal("12345678-1234-1234-1234-123456789012"),
			"container.name":       strAttrVal(name),
			"container.image.name": strAttrVal(name),
			"container.image.tag":  strAttrVal("latest"),
			"k8s.namespace.name":   strAttrVal(name + "-app"),
		}
	}

	return nil
}

func toOTLPAttributes(values map[string]AttrValue) []*commonpb.KeyValue {
	attrs := make([]*commonpb.KeyValue, 0, len(values))
	for k, v := range values {
		switch v.Type {
		case "bool":
			attrs = append(attrs, boolAttr(k, v.Bool))
		case "int":
			attrs = append(attrs, intAttr(k, v.Int))
		case "double":
			attrs = append(attrs, doubleAttr(k, v.Double))
		default:
			attrs = append(attrs, stringAttr(k, v.Str))
		}
	}
	return attrs
}

func newSpan(svc Service, traceID, spanID []byte, start, end time.Time, failed bool) *tracepb.Span {
	name, tmplAttrs := templateInfo(svc, failed)
	span := &tracepb.Span{
		TraceId:           traceID,
		SpanId:            spanID,
		Name:              name,
		Kind:              mapSpanKind(svc.SpanKind),
		StartTimeUnixNano: uint64(start.UnixNano()),
		EndTimeUnixNano:   uint64(end.UnixNano()),
	}
	applyFailure(span, failed)
	if svc.Mesh {
		tmplAttrs = append(tmplAttrs, istioSpanAttrs(svc)...)
	}
	tmplAttrs = applySpanAttrOverrides(tmplAttrs, svc.SpanAttrs)
	span.Attributes = append(span.Attributes, tmplAttrs...)
	return span
}

func applyFailure(span *tracepb.Span, failed bool) {
	if !failed {
		span.Status = &tracepb.Status{Code: tracepb.Status_STATUS_CODE_OK}
		span.Attributes = append(span.Attributes, stringAttr("otgen.outcome", "success"))
		return
	}
	span.Status = &tracepb.Status{Code: tracepb.Status_STATUS_CODE_ERROR, Message: "simulated failure"}
	span.Attributes = append(span.Attributes, stringAttr("otgen.outcome", "failure"))
}

// ── template span attributes ──────────────────────────────────────────────────

var (
	httpMethods   = []string{"GET", "POST", "PUT", "DELETE", "PATCH"}
	httpPaths     = []string{"/api/users", "/api/orders", "/api/products", "/api/auth", "/health", "/api/events", "/api/payments"}
	httpHosts     = []string{"api.example.com", "payment.internal", "user-service.internal", "order-service.internal"}
	dbSystems     = []string{"postgresql", "mysql", "mongodb", "redis", "cassandra"}
	dbNamespaces  = []string{"orders", "users", "analytics", "inventory", "sessions"}
	dbCollections = []string{"users", "orders", "products", "sessions", "events", "payments"}
	dbOps         = []string{"SELECT", "INSERT", "UPDATE", "DELETE"}
	msgSystems    = []string{"kafka", "rabbitmq", "aws_sqs"}
	msgTopics     = []string{"orders", "events", "notifications", "alerts", "payments"}
	msgOps        = []string{"publish", "receive", "process", "settle"}
	grpcSvcs      = []string{"UserService", "OrderService", "PaymentService", "InventoryService", "NotificationService"}
	grpcMethods   = map[string][]string{
		"UserService":         {"GetUser", "CreateUser", "UpdateUser", "ListUsers"},
		"OrderService":        {"CreateOrder", "GetOrder", "ListOrders", "CancelOrder"},
		"PaymentService":      {"ProcessPayment", "RefundPayment", "GetPaymentStatus"},
		"InventoryService":    {"CheckStock", "ReserveStock", "ReleaseStock"},
		"NotificationService": {"SendNotification", "GetNotificationStatus"},
	}
)

// templateDefaults returns the editable/overridable attribute defaults for a
// given template. These values are used to seed the span-attrs editor and as
// fallback when SpanAttrs is empty.
func templateDefaults(template string) map[string]AttrValue {
	switch template {
	case "http-server":
		return map[string]AttrValue{
			"server.address":           strAttrVal("my-service.internal"),
			"url.scheme":               strAttrVal("https"),
			"network.protocol.version": strAttrVal("1.1"),
		}
	case "http-client":
		return map[string]AttrValue{
			"server.address":           strAttrVal("api.example.com"),
			"server.port":              intAttrVal(443),
			"network.protocol.version": strAttrVal("1.1"),
		}
	case "db":
		return map[string]AttrValue{
			"db.system.name": strAttrVal("postgresql"),
			"db.namespace":   strAttrVal("mydb"),
			"server.address": strAttrVal("db.internal"),
			"server.port":    intAttrVal(5432),
		}
	case "messaging":
		return map[string]AttrValue{
			"messaging.system":           strAttrVal("kafka"),
			"messaging.destination.name": strAttrVal("my-topic"),
		}
	case "grpc":
		return map[string]AttrValue{
			"rpc.service": strAttrVal("MyService"),
			"rpc.method":  strAttrVal("MyMethod"),
		}
	}
	return nil
}

// templateInfo returns the span name and extra semantic-convention attributes
// for svc.Template, or a generic name and nil attrs when no template is set.
// Values in svc.SpanAttrs override randomly selected template defaults.
func templateInfo(svc Service, failed bool) (string, []*commonpb.KeyValue) {
	rnd := func(items []string) string { return items[mathrand.IntN(len(items))] }

	switch svc.Template {
	case "http-server":
		method := rnd(httpMethods)
		path := rnd(httpPaths)
		status := int64(200)
		if failed {
			status = 500
		}
		return method + " " + path, []*commonpb.KeyValue{
			stringAttr("http.request.method", method),
			stringAttr("url.path", path),
			stringAttr("url.scheme", "https"),
			intAttr("http.response.status_code", status),
			stringAttr("server.address", svc.Name+".service"),
			stringAttr("network.protocol.version", "1.1"),
		}

	case "http-client":
		method := rnd(httpMethods[:2]) // GET or POST
		host := rnd(httpHosts)
		path := rnd(httpPaths)
		status := int64(200)
		if failed {
			status = 502
		}
		return method + " " + host, []*commonpb.KeyValue{
			stringAttr("http.request.method", method),
			stringAttr("server.address", host),
			intAttr("server.port", 443),
			stringAttr("url.full", "https://"+host+path),
			intAttr("http.response.status_code", status),
			stringAttr("network.protocol.version", "1.1"),
		}

	case "db":
		system := rnd(dbSystems)
		ns := rnd(dbNamespaces)
		col := rnd(dbCollections)
		op := rnd(dbOps)
		port := int64(5432)
		switch system {
		case "mysql":
			port = 3306
		case "mongodb":
			port = 27017
		case "redis":
			port = 6379
		case "cassandra":
			port = 9042
		}
		return op + " " + col, []*commonpb.KeyValue{
			stringAttr("db.system.name", system),
			stringAttr("db.namespace", ns),
			stringAttr("db.operation.name", op),
			stringAttr("db.collection.name", col),
			stringAttr("server.address", "db.internal"),
			intAttr("server.port", port),
		}

	case "messaging":
		system := rnd(msgSystems)
		topic := rnd(msgTopics)
		op := rnd(msgOps)
		return topic + " " + op, []*commonpb.KeyValue{
			stringAttr("messaging.system", system),
			stringAttr("messaging.destination.name", topic),
			stringAttr("messaging.operation.name", op),
			stringAttr("messaging.message.id", randomHex(8)),
			stringAttr("messaging.client_id", svc.Name+"-client"),
		}

	case "grpc":
		grpcSvc := rnd(grpcSvcs)
		method := rnd(grpcMethods[grpcSvc])
		statusCode := int64(0) // OK
		if failed {
			statusCode = 13 // INTERNAL
		}
		return "/" + grpcSvc + "/" + method, []*commonpb.KeyValue{
			stringAttr("rpc.system", "grpc"),
			stringAttr("rpc.service", grpcSvc),
			stringAttr("rpc.method", method),
			intAttr("rpc.grpc.status_code", statusCode),
		}
	}

	// no template → generic name
	return svc.Name + ".request", nil
}

// applySpanAttrOverrides merges user-supplied SpanAttrs on top of the
// template-generated attribute list. Known keys are replaced; new keys appended.
func applySpanAttrOverrides(attrs []*commonpb.KeyValue, overrides map[string]AttrValue) []*commonpb.KeyValue {
	if len(overrides) == 0 {
		return attrs
	}
	// index existing attrs by key
	idx := make(map[string]int, len(attrs))
	for i, a := range attrs {
		idx[a.Key] = i
	}
	for _, ov := range toOTLPAttributes(overrides) {
		if i, exists := idx[ov.Key]; exists {
			attrs[i] = ov
		} else {
			attrs = append(attrs, ov)
		}
	}
	return attrs
}

func mapSpanKind(value string) tracepb.Span_SpanKind {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "server":
		return tracepb.Span_SPAN_KIND_SERVER
	case "client":
		return tracepb.Span_SPAN_KIND_CLIENT
	case "producer":
		return tracepb.Span_SPAN_KIND_PRODUCER
	case "consumer":
		return tracepb.Span_SPAN_KIND_CONSUMER
	default:
		return tracepb.Span_SPAN_KIND_INTERNAL
	}
}

// randomRootDuration returns a random root span duration between 50 and 500 ms.
func randomRootDuration() time.Duration {
	return time.Duration(50+mathrand.IntN(451)) * time.Millisecond
}

// randomChildDuration returns a random child span duration between 5 and 80 ms.
func randomChildDuration() time.Duration {
	return time.Duration(5+mathrand.IntN(76)) * time.Millisecond
}

// childSpanOps are the operation names cycled through for child spans.
var childSpanOps = []string{
	"db.query",
	"cache.lookup",
	"http.request",
	"queue.publish",
	"grpc.call",
	"db.transaction",
	"auth.validate",
	"storage.read",
	"storage.write",
	"event.emit",
}

func newChildSpan(svcName string, traceID, spanID, parentID []byte, start, end time.Time, idx int, failed bool) *tracepb.Span {
	op := childSpanOps[idx%len(childSpanOps)]
	status := &tracepb.Status{Code: tracepb.Status_STATUS_CODE_OK}
	if failed {
		status = &tracepb.Status{Code: tracepb.Status_STATUS_CODE_ERROR, Message: "simulated failure"}
	}
	return &tracepb.Span{
		TraceId:           traceID,
		SpanId:            spanID,
		ParentSpanId:      parentID,
		Name:              svcName + "." + op,
		Kind:              tracepb.Span_SPAN_KIND_CLIENT,
		StartTimeUnixNano: uint64(start.UnixNano()),
		EndTimeUnixNano:   uint64(end.UnixNano()),
		Status:            status,
	}
}

func randomBytes(byteLen int) []byte {
	buf := make([]byte, byteLen)
	if _, err := rand.Read(buf); err != nil {
		clear(buf)
	}
	return buf
}

func randomHex(byteLen int) string { return hex.EncodeToString(randomBytes(byteLen)) }

func stringAttr(key, value string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: key, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: value}}}
}

func intAttr(key string, value int64) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: key, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: value}}}
}

func boolAttr(key string, value bool) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: key, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: value}}}
}

func doubleAttr(key string, value float64) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: key, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: value}}}
}

// ── infra template metrics ────────────────────────────────────────────────────

// hostMemoryBytes is the memory size modelled for synthetic OTel hosts, so that
// system.memory.usage and system.memory.utilization agree with each other.
const hostMemoryBytes int64 = 16 << 30

// otgenStart stands in for host/process boot time. Cumulative sums need a start
// timestamp; a fixed one per run is what a real SDK reports and keeps metric
// generation a pure function of (svc, now).
var otgenStart = time.Now()

// infraMetrics returns the metrics an infra template must emit for Dynatrace
// entity extraction, mirroring how infraDefaults supplies its attributes.
//
// The OpenTelemetry Host Monitoring extension routes on the metric KEY, not on
// resource attributes alone: a system.* metric creates the otel:host entity and
// a process.* metric creates otel:process. Without one, a resource carrying a
// perfectly correct host.id / host.name / telemetry.sdk.name produces no entity
// at all.
//
// Only gauges and non-monotonic cumulative sums are emitted. Delta monotonic
// sums (system.cpu.time, system.network.io, …) would need StartTimeUnixNano
// chained to the previous emission, and therefore per-service state that
// buildEmissionPayloads deliberately does not carry.
func infraMetrics(svc Service, now time.Time) []*metricspb.Metric {
	switch svc.InfraTemplate {
	case "otel-host":
		return hostVitalMetrics(now)
	case "otel-host-process":
		return append(hostVitalMetrics(now), processVitalMetrics(now)...)
	}
	return nil
}

// gaugeMetric builds a single-valued Gauge. Gauges carry neither aggregation
// temporality nor a start timestamp.
func gaugeMetric(name, unit string, now time.Time, points []*metricspb.NumberDataPoint) *metricspb.Metric {
	return &metricspb.Metric{
		Name: name, Unit: unit,
		Data: &metricspb.Metric_Gauge{Gauge: &metricspb.Gauge{DataPoints: points}},
	}
}

// cumulativeSum builds a non-monotonic cumulative Sum — the shape the
// hostmetrics receiver uses for "current size" metrics such as memory usage.
// These survive the reference collector's cumulative_to_delta unchanged, which
// only converts monotonic sums.
func cumulativeSum(name, unit string, now time.Time, points []*metricspb.NumberDataPoint) *metricspb.Metric {
	for _, dp := range points {
		dp.StartTimeUnixNano = uint64(otgenStart.UnixNano())
	}
	return &metricspb.Metric{
		Name: name, Unit: unit,
		Data: &metricspb.Metric_Sum{Sum: &metricspb.Sum{
			AggregationTemporality: metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE,
			IsMonotonic:            false,
			DataPoints:             points,
		}},
	}
}

func doublePoint(now time.Time, v float64, attrs ...*commonpb.KeyValue) *metricspb.NumberDataPoint {
	return &metricspb.NumberDataPoint{
		TimeUnixNano: uint64(now.UnixNano()), Attributes: attrs,
		Value: &metricspb.NumberDataPoint_AsDouble{AsDouble: v},
	}
}

func intPoint(now time.Time, v int64, attrs ...*commonpb.KeyValue) *metricspb.NumberDataPoint {
	return &metricspb.NumberDataPoint{
		TimeUnixNano: uint64(now.UnixNano()), Attributes: attrs,
		Value: &metricspb.NumberDataPoint_AsInt{AsInt: v},
	}
}

// hostVitalMetrics returns the system.* metrics that create the otel:host
// entity and populate the extension's headline host charts. Values are derived
// from one draw so that usage and utilization stay consistent.
func hostVitalMetrics(now time.Time) []*metricspb.Metric {
	usedFraction := 0.25 + mathrand.Float64()*0.5
	usedBytes := int64(float64(hostMemoryBytes) * usedFraction)
	cpuUtilization := 0.05 + mathrand.Float64()*0.7

	return []*metricspb.Metric{
		// The reference pipeline filters out state=idle and averages across
		// cores, leaving one attribute-less value — so emit exactly that.
		gaugeMetric("system.cpu.utilization", "1", now, []*metricspb.NumberDataPoint{
			doublePoint(now, cpuUtilization),
		}),
		gaugeMetric("system.cpu.load_average.1m", "{thread}", now, []*metricspb.NumberDataPoint{
			doublePoint(now, cpuUtilization*8),
		}),
		gaugeMetric("system.memory.utilization", "1", now, []*metricspb.NumberDataPoint{
			doublePoint(now, usedFraction, stringAttr("state", "used")),
			doublePoint(now, 1-usedFraction, stringAttr("state", "free")),
		}),
		cumulativeSum("system.memory.usage", "By", now, []*metricspb.NumberDataPoint{
			intPoint(now, usedBytes, stringAttr("state", "used")),
			intPoint(now, hostMemoryBytes-usedBytes, stringAttr("state", "free")),
		}),
	}
}

// processVitalMetrics returns the process.* metrics that create the
// otel:process entity. The process is identified by the resource's
// process.executable.name, so these carry no identifying attributes of their own.
func processVitalMetrics(now time.Time) []*metricspb.Metric {
	residentBytes := int64(64<<20) + int64(mathrand.IntN(512<<20))
	return []*metricspb.Metric{
		gaugeMetric("process.cpu.utilization", "1", now, []*metricspb.NumberDataPoint{
			doublePoint(now, 0.01+mathrand.Float64()*0.3),
		}),
		cumulativeSum("process.memory.usage", "By", now, []*metricspb.NumberDataPoint{
			intPoint(now, residentBytes),
		}),
		// Virtual is reliably a multiple of resident, so the two charts track.
		cumulativeSum("process.memory.virtual", "By", now, []*metricspb.NumberDataPoint{
			intPoint(now, residentBytes*5/2),
		}),
	}
}
