package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
)

const (
	envEndpoint = "OTGEN_ENDPOINT"
	envToken    = "OTGEN_TOKEN"
)

// AttrValue is a typed OTLP attribute value.  The four scalar types from the
// OTel spec are supported: string, bool, int (int64), double (float64).
// In JSON it serialises as the native type so that config.json is readable:
//
//	"string" → JSON string     "hello"
//	"bool"   → JSON boolean    true / false
//	"int"    → JSON integer    42
//	"double" → JSON number     3.14
type AttrValue struct {
	Type   string // "string" | "bool" | "int" | "double"
	Str    string
	Bool   bool
	Int    int64
	Double float64
}

func (a AttrValue) MarshalJSON() ([]byte, error) {
	switch a.Type {
	case "bool":
		return json.Marshal(a.Bool)
	case "int":
		return json.Marshal(a.Int)
	case "double":
		return json.Marshal(a.Double)
	default:
		return json.Marshal(a.Str)
	}
}

func (a *AttrValue) UnmarshalJSON(b []byte) error {
	if len(b) == 0 {
		return nil
	}
	switch b[0] {
	case 't', 'f': // JSON boolean literal
		var bv bool
		if err := json.Unmarshal(b, &bv); err != nil {
			return err
		}
		*a = boolAttrVal(bv)
		return nil
	case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		// JSON number: integer if no '.', 'e', or 'E'; double otherwise.
		raw := string(b)
		if !strings.ContainsAny(raw, ".eE") {
			var iv int64
			if err := json.Unmarshal(b, &iv); err != nil {
				return err
			}
			*a = intAttrVal(iv)
			return nil
		}
		var fv float64
		if err := json.Unmarshal(b, &fv); err != nil {
			return err
		}
		*a = doubleAttrVal(fv)
		return nil
	default: // JSON string
		var sv string
		if err := json.Unmarshal(b, &sv); err != nil {
			return err
		}
		*a = strAttrVal(sv)
		return nil
	}
}

// Convenience constructors for the four OTel scalar attribute types.
func strAttrVal(v string) AttrValue     { return AttrValue{Type: "string", Str: v} }
func boolAttrVal(v bool) AttrValue      { return AttrValue{Type: "bool", Bool: v} }
func intAttrVal(v int64) AttrValue      { return AttrValue{Type: "int", Int: v} }
func doubleAttrVal(v float64) AttrValue { return AttrValue{Type: "double", Double: v} }

// Service defines a named synthetic service to emit OTLP signals for.
type Service struct {
	Name            string               `json:"name"`
	Template        string               `json:"template,omitempty"`      // span semantics: "", "http-server", "http-client", "db", "messaging", "grpc"
	InfraTemplate   string               `json:"infraTemplate,omitempty"` // infra context: "", "k8s", "eks", "gke", "aks", "ecs", "host", "docker", "lambda", "cloudfoundry", "process", "otel-host", "otel-host-process"
	SpanKind        string               `json:"spanKind"`                // server|client|internal|producer|consumer
	FailureRate     int                  `json:"failureRate"`             // 0–100 %
	Interval        int                  `json:"interval"`                // seconds between sends, minimum 1
	ChildSpans      int                  `json:"childSpans"`              // additional client spans under the root, 0–10
	Signals         []string             `json:"signals"`                 // "spans","metrics","logs"; empty = all three
	Attributes      map[string]AttrValue `json:"attributes"`              // resource-level; service.name always wins
	SpanAttrs       map[string]AttrValue `json:"spanAttrs,omitempty"`     // span-level overrides for template-generated attributes
	HostName        string               `json:"hostName,omitempty"`      // custom host.name for host-category infra templates
	ProcessName     string               `json:"processName,omitempty"`   // custom process.executable.name for process infra templates
	Mesh            bool                 `json:"mesh,omitempty"`
	DownstreamCalls []string             `json:"downstreamCalls,omitempty"`
	Metrics         []MetricConfig       `json:"metrics,omitempty"`
	Metric          *MetricConfig        `json:"metric,omitempty"` // legacy: normalized into Metrics
	LogSeverity     string               `json:"logSeverity,omitempty"`
	LogMessage      string               `json:"logMessage,omitempty"`
	Enabled         bool                 `json:"enabled"`
}

type MetricConfig struct {
	Type string `json:"type,omitempty"`
	Name string `json:"name,omitempty"`
	Unit string `json:"unit,omitempty"`
}

// Config is the full application configuration schema.
type Config struct {
	Endpoint   string               `json:"endpoint"`
	Token      string               `json:"token"`
	Attributes map[string]AttrValue `json:"attributes,omitempty"` // global resource attrs merged into every service (lowest precedence)
	Services   []Service            `json:"services"`
}

// SignalStatus tracks send state for one signal kind within a service.
type SignalStatus struct {
	LastError string `json:"lastError,omitempty"`
	SentCount uint64 `json:"sentCount"`
}

// ServiceStatus groups signal statuses for one service.
type ServiceStatus struct {
	Spans   SignalStatus `json:"spans"`
	Metrics SignalStatus `json:"metrics"`
	Logs    SignalStatus `json:"logs"`
}

// RuntimeStatus is the top-level status payload returned by GET /api/status.
type RuntimeStatus struct {
	Running  bool                     `json:"running"`
	Services map[string]ServiceStatus `json:"services"`
}

type signalKind string

const (
	signalSpans   signalKind = "spans"
	signalMetrics signalKind = "metrics"
	signalLogs    signalKind = "logs"
)

// hasSignal returns true if the service should emit the given signal kind.
// An empty Signals slice means all three signals are enabled.
func (svc Service) hasSignal(kind signalKind) bool {
	if len(svc.Signals) == 0 {
		return true
	}
	for _, s := range svc.Signals {
		if signalKind(strings.ToLower(s)) == kind {
			return true
		}
	}
	return false
}

// hasToken returns true if the config has a non-blank token.
func (cfg Config) hasToken() bool {
	return strings.TrimSpace(cfg.Token) != ""
}

// runtimeConfig applies OTGEN_ENDPOINT and OTGEN_TOKEN env overrides,
// then normalizes the result.
func (cfg Config) runtimeConfig() Config {
	if endpoint := strings.TrimSpace(os.Getenv(envEndpoint)); endpoint != "" {
		cfg.Endpoint = endpoint
	}
	if token := strings.TrimSpace(os.Getenv(envToken)); token != "" {
		cfg.Token = token
	}
	return normalizeConfig(cfg)
}

func defaultConfig() Config {
	return Config{
		Services: []Service{{
			Name:        "otgen",
			SpanKind:    "server",
			FailureRate: 5,
			Interval:    5,
			ChildSpans:  0,
			Signals:     []string{"spans", "metrics", "logs"},
			Enabled:     true,
		}},
	}
}

func normalizeService(svc Service) Service {
	svc.Name = strings.TrimSpace(svc.Name)
	switch svc.Template {
	case "", "http-server", "http-client", "db", "messaging", "grpc":
		// valid
	default:
		svc.Template = ""
	}
	switch svc.InfraTemplate {
	case "", "k8s", "eks", "gke", "aks", "ecs", "host", "docker", "lambda", "cloudfoundry", "process",
		"openshift", "containerd", "nomad", "azure-functions", "gcp-functions", "azure-container-apps",
		"otel-host", "otel-host-process":
		// valid
	default:
		svc.InfraTemplate = ""
	}
	if strings.TrimSpace(svc.SpanKind) == "" {
		svc.SpanKind = "server"
	}
	if svc.FailureRate < 0 {
		svc.FailureRate = 0
	}
	if svc.FailureRate > 100 {
		svc.FailureRate = 100
	}
	if svc.Interval <= 0 {
		svc.Interval = 5
	}
	if svc.ChildSpans < 0 {
		svc.ChildSpans = 0
	}
	if svc.ChildSpans > 10 {
		svc.ChildSpans = 10
	}
	if svc.Attributes == nil {
		svc.Attributes = map[string]AttrValue{}
	}
	if svc.SpanAttrs == nil {
		svc.SpanAttrs = map[string]AttrValue{}
	}
	if len(svc.DownstreamCalls) > 0 {
		svc.DownstreamCalls = append([]string(nil), svc.DownstreamCalls...)
	}
	for i := range svc.DownstreamCalls {
		svc.DownstreamCalls[i] = strings.TrimSpace(svc.DownstreamCalls[i])
	}
	metrics := append([]MetricConfig(nil), svc.Metrics...)
	if len(metrics) == 0 && svc.Metric != nil {
		metrics = append(metrics, *svc.Metric)
	}
	for i := range metrics {
		metrics[i].Type = strings.ToLower(strings.TrimSpace(metrics[i].Type))
		metrics[i].Name = strings.TrimSpace(metrics[i].Name)
		metrics[i].Unit = strings.TrimSpace(metrics[i].Unit)
	}
	// A single empty/sum definition is the historical default. Keep omitting it
	// so old configurations remain compact and service renames update its name.
	if len(metrics) == 1 && (metrics[0].Type == "" || metrics[0].Type == "sum") && metrics[0].Name == "" && metrics[0].Unit == "" {
		metrics = nil
	}
	svc.Metrics = metrics
	svc.Metric = nil
	svc.LogSeverity = strings.ToLower(strings.TrimSpace(svc.LogSeverity))
	svc.LogMessage = strings.TrimSpace(svc.LogMessage)
	svc.HostName = strings.TrimSpace(svc.HostName)
	svc.ProcessName = strings.TrimSpace(svc.ProcessName)
	// Only host-category templates read these. Without clearing them, switching
	// a service to k8s leaves the old names in config.json as dead config and
	// makes the editor report unsaved changes for a no-op edit.
	if !infraUsesHostName(svc.InfraTemplate) {
		svc.HostName = ""
	}
	if !infraUsesProcessName(svc.InfraTemplate) {
		svc.ProcessName = ""
	}
	return svc
}

func normalizeConfig(cfg Config) Config {
	if cfg.Attributes == nil {
		cfg.Attributes = map[string]AttrValue{}
	}
	for i, svc := range cfg.Services {
		cfg.Services[i] = normalizeService(svc)
	}
	return cfg
}

func validateConfig(cfg Config) error {
	cfg = normalizeConfig(cfg)
	if len(cfg.Services) == 0 {
		return errors.New("at least one service is required")
	}
	seen := make(map[string]struct{}, len(cfg.Services))
	for i, svc := range cfg.Services {
		if svc.Name == "" {
			return fmt.Errorf("services[%d]: name is required", i)
		}
		if _, dup := seen[svc.Name]; dup {
			return fmt.Errorf("services[%d]: duplicate service name %q", i, svc.Name)
		}
		seen[svc.Name] = struct{}{}
		switch strings.ToLower(strings.TrimSpace(svc.SpanKind)) {
		case "internal", "server", "client", "producer", "consumer":
		default:
			return fmt.Errorf("services[%d]: spanKind must be one of: internal, server, client, producer, consumer", i)
		}
		for j, sig := range svc.Signals {
			switch strings.ToLower(sig) {
			case "spans", "metrics", "logs":
			default:
				return fmt.Errorf("services[%d].signals[%d]: must be one of: spans, metrics, logs", i, j)
			}
		}
		for j, metric := range svc.Metrics {
			if metric.Type == "" {
				continue
			}
			switch metric.Type {
			case "sum", "gauge", "histogram":
			default:
				return fmt.Errorf("services[%d].metrics[%d].type: must be one of: sum, gauge, histogram", i, j)
			}
		}
		metricNames := make(map[string]struct{}, len(svc.Metrics))
		for j, metric := range effectiveMetricConfigs(svc) {
			if _, duplicate := metricNames[metric.Name]; duplicate {
				return fmt.Errorf("services[%d].metrics[%d].name: duplicate metric name %q", i, j, metric.Name)
			}
			metricNames[metric.Name] = struct{}{}
		}
		if svc.LogSeverity != "" {
			switch svc.LogSeverity {
			case "debug", "info", "warn", "error":
			default:
				return fmt.Errorf("services[%d].log.severity: must be one of: debug, info, warn, error", i)
			}
		}
	}
	indices := indexServices(cfg)
	for i, svc := range cfg.Services {
		seenTargets := make(map[string]struct{}, len(svc.DownstreamCalls))
		for j, target := range svc.DownstreamCalls {
			target = strings.TrimSpace(target)
			if target == "" {
				return fmt.Errorf("services[%d].downstreamCalls[%d]: service is required", i, j)
			}
			if target == svc.Name {
				return fmt.Errorf("services[%d].downstreamCalls[%d]: service cannot call itself", i, j)
			}
			if _, ok := indices[target]; !ok {
				return fmt.Errorf("services[%d].downstreamCalls[%d]: unknown service %q", i, j, target)
			}
			if _, dup := seenTargets[target]; dup {
				return fmt.Errorf("services[%d].downstreamCalls[%d]: duplicate target %q", i, j, target)
			}
			seenTargets[target] = struct{}{}
		}
	}
	if cycle := serviceCallCycle(cfg); len(cycle) > 0 {
		return fmt.Errorf("service call cycle: %s", strings.Join(cycle, " -> "))
	}
	return nil
}

func indexServices(cfg Config) map[string]Service {
	indexed := make(map[string]Service, len(cfg.Services))
	for _, svc := range cfg.Services {
		indexed[svc.Name] = svc
	}
	return indexed
}

func renameServiceReferences(cfg *Config, oldName, newName string) {
	for i := range cfg.Services {
		for j := range cfg.Services[i].DownstreamCalls {
			if cfg.Services[i].DownstreamCalls[j] == oldName {
				cfg.Services[i].DownstreamCalls[j] = newName
			}
		}
	}
}

func serviceReferrers(cfg Config, target string) []string {
	var names []string
	for _, svc := range cfg.Services {
		for _, call := range svc.DownstreamCalls {
			if call == target {
				names = append(names, svc.Name)
				break
			}
		}
	}
	sort.Strings(names)
	return names
}

func serviceCallCycle(cfg Config) []string {
	state := make(map[string]uint8, len(cfg.Services))
	indices := indexServices(cfg)
	stack := make([]string, 0, len(cfg.Services))
	var visit func(string) []string
	visit = func(name string) []string {
		switch state[name] {
		case 1:
			for i, item := range stack {
				if item == name {
					return append(append([]string(nil), stack[i:]...), name)
				}
			}
		case 2:
			return nil
		}
		state[name] = 1
		stack = append(stack, name)
		svc := indices[name]
		for _, call := range svc.DownstreamCalls {
			if cycle := visit(call); len(cycle) > 0 {
				return cycle
			}
		}
		stack = stack[:len(stack)-1]
		state[name] = 2
		return nil
	}
	for _, svc := range cfg.Services {
		if cycle := visit(svc.Name); len(cycle) > 0 {
			return cycle
		}
	}
	return nil
}

func effectiveMetricConfigs(svc Service) []MetricConfig {
	configured := append([]MetricConfig(nil), svc.Metrics...)
	if len(configured) == 0 && svc.Metric != nil {
		configured = append(configured, *svc.Metric)
	}
	if len(configured) == 0 {
		configured = []MetricConfig{{}}
	}
	result := make([]MetricConfig, len(configured))
	for i, cfg := range configured {
		result[i] = effectiveMetricConfigForService(svc.Name, cfg)
	}
	return result
}

func effectiveMetricConfigForService(serviceName string, cfg MetricConfig) MetricConfig {
	if cfg.Type == "" {
		cfg.Type = "sum"
	}
	if cfg.Name == "" {
		switch cfg.Type {
		case "gauge":
			cfg.Name = serviceName + ".load"
		case "histogram":
			cfg.Name = serviceName + ".request.duration"
		default:
			cfg.Name = serviceName + ".requests.total"
		}
	}
	if cfg.Unit == "" {
		if cfg.Type == "histogram" {
			cfg.Unit = "ms"
		} else {
			cfg.Unit = "1"
		}
	}
	return cfg
}

// effectiveMetricConfig preserves the old single-metric helper for callers
// that only need the first/default definition.
func effectiveMetricConfig(svc Service) MetricConfig {
	return effectiveMetricConfigs(svc)[0]
}

func effectiveLogSeverity(svc Service) string {
	if svc.LogSeverity == "" {
		return "info"
	}
	return svc.LogSeverity
}

func effectiveLogMessage(svc Service, fallback string) string {
	if svc.LogMessage == "" {
		return fallback
	}
	return svc.LogMessage
}

// LoadConfig reads and applies config.json, normalizing and validating the result.
func (a *App) LoadConfig() error {
	b, err := os.ReadFile(a.configPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}

	var cfg Config
	if err := json.Unmarshal(b, &cfg); err != nil {
		return err
	}

	cfg = normalizeConfig(cfg)
	if err := validateConfig(cfg); err != nil {
		return err
	}

	a.cfg = cfg
	return nil
}

// saveConfig writes cfg to config.json as indented JSON.
func (a *App) saveConfig(cfg Config) error {
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(a.configPath, b, 0o644)
}

// endpointFor returns the full OTLP endpoint URL for the given signal kind.
// If base already contains a /v1/ path it is used as-is; otherwise the
// per-signal path is appended.
func endpointFor(base string, kind signalKind) string {
	base = strings.TrimSpace(base)
	if strings.Contains(base, "/v1/logs") || strings.Contains(base, "/v1/metrics") || strings.Contains(base, "/v1/traces") {
		return base
	}
	base = strings.TrimRight(base, "/")
	switch kind {
	case signalLogs:
		return base + "/v1/logs"
	case signalMetrics:
		return base + "/v1/metrics"
	default:
		return base + "/v1/traces"
	}
}

// endpointFromEnv reports whether OTGEN_ENDPOINT is set in the environment.
// When true the saved/UI endpoint is ignored at runtime (see runtimeConfig).
func endpointFromEnv() bool {
	return strings.TrimSpace(os.Getenv(envEndpoint)) != ""
}

// tokenFromEnv reports whether OTGEN_TOKEN is set in the environment.
// When true the saved/UI token is ignored at runtime (see runtimeConfig).
func tokenFromEnv() bool {
	return strings.TrimSpace(os.Getenv(envToken)) != ""
}
