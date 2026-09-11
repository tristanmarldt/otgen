package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRuntimeConfigUsesEnvEndpointAndToken(t *testing.T) {
	t.Setenv(envEndpoint, "https://env.example/api/v2/otlp")
	t.Setenv(envToken, "env-token")

	cfg := Config{}
	runtimeCfg := cfg.runtimeConfig()

	if runtimeCfg.Endpoint != "https://env.example/api/v2/otlp" {
		t.Fatalf("expected endpoint from env, got %q", runtimeCfg.Endpoint)
	}
	if runtimeCfg.Token != "env-token" {
		t.Fatalf("expected token from env, got %q", runtimeCfg.Token)
	}
}

func TestDefaultConfigStartsWithoutCustomAttributes(t *testing.T) {
	cfg := defaultConfig()
	if len(cfg.Attributes) != 0 {
		t.Fatalf("default global attributes = %v, want none", cfg.Attributes)
	}
	if got := cfg.Services[0].Attributes; len(got) != 0 {
		t.Fatalf("default service attributes = %v, want none", got)
	}
}

func TestRuntimeConfigEnvOverridesSavedValues(t *testing.T) {
	t.Setenv(envEndpoint, "https://env.example/api/v2/otlp")
	t.Setenv(envToken, "env-token")

	cfg := Config{
		Endpoint: "https://saved.example/api/v2/otlp",
		Token:    "saved-token",
	}
	runtimeCfg := cfg.runtimeConfig()

	if runtimeCfg.Endpoint != "https://env.example/api/v2/otlp" {
		t.Fatalf("expected endpoint override from env, got %q", runtimeCfg.Endpoint)
	}
	if runtimeCfg.Token != "env-token" {
		t.Fatalf("expected token override from env, got %q", runtimeCfg.Token)
	}
}

func TestEffectiveSignalDefaultsPreserveOldConfig(t *testing.T) {
	svc := normalizeService(Service{Name: "checkout"})
	metric := effectiveMetricConfig(svc)
	if metric.Type != "sum" || metric.Name != "checkout.requests.total" || metric.Unit != "1" {
		t.Fatalf("metric defaults = %+v", metric)
	}
	if got := effectiveLogSeverity(svc); got != "info" {
		t.Fatalf("log severity default = %q, want info", got)
	}
}

func TestMetricAndSeverityNormalizeCase(t *testing.T) {
	cfg := normalizeConfig(Config{Services: []Service{{
		Name:        "svc",
		Metric:      &MetricConfig{Type: " HISTOGRAM "},
		LogSeverity: " WARN ",
	}}})
	if err := validateConfig(cfg); err != nil {
		t.Fatalf("normalized config rejected: %v", err)
	}
	if len(cfg.Services[0].Metrics) != 1 || cfg.Services[0].Metrics[0].Type != "histogram" || cfg.Services[0].LogSeverity != "warn" {
		t.Fatalf("normalization = %+v", cfg.Services[0])
	}
	if cfg.Services[0].Metric != nil {
		t.Fatal("legacy metric field was not cleared after migration")
	}
}

func TestLegacyMetricJSONMigratesToMetrics(t *testing.T) {
	var svc Service
	if err := json.Unmarshal([]byte(`{"name":"svc","metric":{"type":"gauge","name":"queue.depth","unit":"1"}}`), &svc); err != nil {
		t.Fatalf("unmarshal legacy service: %v", err)
	}
	svc = normalizeService(svc)
	if len(svc.Metrics) != 1 || svc.Metrics[0].Name != "queue.depth" || svc.Metrics[0].Type != "gauge" {
		t.Fatalf("migrated metrics = %+v", svc.Metrics)
	}

	b, err := json.Marshal(svc)
	if err != nil {
		t.Fatalf("marshal migrated service: %v", err)
	}
	if strings.Contains(string(b), `"metric":`) || !strings.Contains(string(b), `"metrics":`) {
		t.Fatalf("legacy metric field survived migration: %s", b)
	}
}

func TestServiceJSONOmitsEmptySignalObjects(t *testing.T) {
	b, err := json.Marshal(Service{Name: "svc", SpanKind: "server"})
	if err != nil {
		t.Fatalf("marshal service: %v", err)
	}
	if strings.Contains(string(b), `"metric"`) || strings.Contains(string(b), `"log"`) {
		t.Fatalf("old service gained empty signal config: %s", b)
	}
}
