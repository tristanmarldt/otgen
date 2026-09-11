package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

type App struct {
	mu          sync.RWMutex
	cfg         Config
	status      RuntimeStatus
	runCancel   context.CancelFunc
	configPath  string
	httpTimeout time.Duration
}

func NewApp(configPath string, httpTimeout time.Duration) *App {
	return &App{
		cfg: defaultConfig(),
		status: RuntimeStatus{
			Services: map[string]ServiceStatus{},
		},
		configPath:  configPath,
		httpTimeout: httpTimeout,
	}
}

func (a *App) SetConfig(cfg Config) error {
	cfg = normalizeConfig(cfg)
	if err := validateConfig(cfg); err != nil {
		return err
	}
	a.mu.Lock()
	running := a.status.Running
	a.mu.Unlock()

	if err := a.saveConfig(cfg); err != nil {
		return err
	}
	a.mu.Lock()
	a.cfg = cfg
	a.mu.Unlock()
	if !running {
		return nil
	}
	a.Stop()
	return a.Start()
}

func (a *App) Start() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.status.Running {
		return nil
	}
	runtimeCfg := a.cfg.runtimeConfig()
	if strings.TrimSpace(runtimeCfg.Endpoint) == "" {
		return fmt.Errorf("endpoint is required")
	}
	if strings.TrimSpace(runtimeCfg.Token) == "" {
		return fmt.Errorf("API token is required")
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.runCancel = cancel
	a.status.Running = true
	for _, svc := range runtimeCfg.Services {
		if !svc.Enabled {
			continue
		}
		go a.runService(ctx, svc)
	}
	return nil
}

func (a *App) Stop() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.status.Running {
		return
	}
	if a.runCancel != nil {
		a.runCancel()
		a.runCancel = nil
	}
	a.status.Running = false
}

func (a *App) runService(ctx context.Context, svc Service) {
	interval := svc.Interval
	if interval <= 0 {
		interval = 5
	}
	ticker := time.NewTicker(time.Duration(interval) * time.Second)
	defer ticker.Stop()
	a.sendService(svc)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.sendService(svc)
		}
	}
}

func (a *App) sendService(svc Service) {
	a.mu.RLock()
	cfg := a.cfg.runtimeConfig()
	a.mu.RUnlock()
	if current, ok := indexServices(cfg)[svc.Name]; ok {
		svc = current
	}
	payloads, buildErr := buildEmissionPayloads(cfg, svc)
	for _, kind := range []signalKind{signalSpans, signalMetrics, signalLogs} {
		if !svc.hasSignal(kind) {
			continue
		}
		var payload []byte
		switch kind {
		case signalSpans:
			payload = payloads.Traces
		case signalMetrics:
			payload = payloads.Metrics
		case signalLogs:
			payload = payloads.Logs
		}
		err := buildErr
		if err == nil && len(payload) == 0 {
			err = fmt.Errorf("no payload generated for %s", kind)
		}
		if err == nil {
			err = a.postOTLP(cfg.Endpoint, cfg.Token, kind, payload)
		}
		a.updateSignalStatus(svc.Name, kind, err)
	}
}

func (a *App) postOTLP(endpoint, token string, kind signalKind, payload []byte) error {
	if strings.TrimSpace(endpoint) == "" {
		return fmt.Errorf("endpoint is empty")
	}
	req, err := http.NewRequest(http.MethodPost, endpointFor(endpoint, kind), bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("Accept", "application/x-protobuf, application/json")
	if tok := strings.TrimSpace(token); tok != "" {
		req.Header.Set("Authorization", formatToken(tok))
	}
	client := &http.Client{Timeout: a.httpTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 300 {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
}

// formatToken ensures the token is prefixed with "Api-Token " when it is not already.
func formatToken(token string) string {
	if !strings.HasPrefix(strings.ToLower(token), "api-token ") {
		return "Api-Token " + token
	}
	return token
}

func (a *App) updateSignalStatus(svcName string, kind signalKind, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	ss := a.status.Services[svcName]
	var cur *SignalStatus
	switch kind {
	case signalSpans:
		cur = &ss.Spans
	case signalMetrics:
		cur = &ss.Metrics
	case signalLogs:
		cur = &ss.Logs
	}
	if cur != nil {
		if err == nil {
			cur.SentCount++
			cur.LastError = ""
		} else {
			cur.LastError = err.Error()
		}
	}
	a.status.Services[svcName] = ss
}

// TestConnection posts a single throwaway span to the configured endpoint and
// reports whether the endpoint and token are usable. It is safe to call while
// the generator is running.
func (a *App) TestConnection() error {
	a.mu.RLock()
	cfg := a.cfg.runtimeConfig()
	a.mu.RUnlock()

	if strings.TrimSpace(cfg.Endpoint) == "" {
		return fmt.Errorf("endpoint is required")
	}
	svc := normalizeService(Service{Name: "otgen-connection-test", SpanKind: "internal"})
	testCfg := cfg
	testCfg.Services = []Service{svc}
	payloads, err := buildEmissionPayloads(testCfg, svc)
	if err != nil {
		return err
	}
	return a.postOTLP(cfg.Endpoint, cfg.Token, signalSpans, payloads.Traces)
}

// GetConfig returns a snapshot of the current config (safe to call from any goroutine).
func (a *App) GetConfig() Config {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.cfg
}

// GetStatus returns a snapshot of the current runtime status (safe to call from any goroutine).
func (a *App) GetStatus() RuntimeStatus {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.status
}
