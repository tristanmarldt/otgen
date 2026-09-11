package main

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestStartRequiresAPIToken(t *testing.T) {
	t.Setenv(envToken, "")
	app := NewApp(filepath.Join(t.TempDir(), "config.json"), 0)
	app.cfg = normalizeConfig(Config{
		Endpoint: "https://example.live.dynatrace.com/api/v2/otlp",
		Services: []Service{{Name: "svc", Enabled: true}},
	})

	err := app.Start()
	if err == nil || !strings.Contains(err.Error(), "API token is required") {
		t.Fatalf("Start error = %v, want missing-token error", err)
	}
	if app.GetStatus().Running {
		t.Fatal("app started without an API token")
	}
}

func TestSendServicePostsOnePayloadPerEnabledSignal(t *testing.T) {
	var mu sync.Mutex
	paths := make([]string, 0, 3)
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("local listener unavailable: %v", err)
	}
	server := &httptest.Server{Listener: listener, Config: &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	})}}
	server.Start()
	defer server.Close()

	app := NewApp("/dev/null", 0)
	app.cfg = normalizeConfig(Config{Endpoint: server.URL, Services: []Service{
		{Name: "a", Signals: nil, DownstreamCalls: []string{"b"}},
		{Name: "b", Signals: nil},
	}})
	app.sendService(app.cfg.Services[0])

	mu.Lock()
	defer mu.Unlock()
	if len(paths) != 3 {
		t.Fatalf("requests = %d, want 3 (%v)", len(paths), paths)
	}
	for _, want := range []string{"/v1/traces", "/v1/metrics", "/v1/logs"} {
		found := false
		for _, path := range paths {
			if path == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing request path %s in %v", want, paths)
		}
	}
	status := app.GetStatus().Services["a"]
	if status.Spans.SentCount != 1 || status.Metrics.SentCount != 1 || status.Logs.SentCount != 1 {
		t.Fatalf("status = %+v", status)
	}
}
