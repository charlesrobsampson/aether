package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/charlesrobsampson/aether/internal/config"
	"github.com/charlesrobsampson/aether/internal/metrics"
)

// fakePinger stands in for *store.Store in health checks.
type fakePinger struct {
	err   error
	calls int
}

func (p *fakePinger) Ping(context.Context) error {
	p.calls++
	return p.err
}

// TestHealthMetrics covers the operational endpoints: /healthz reports 200 only
// when a configuration is loaded and the store answers Ping, /metrics serves the
// injected registry, and every route is registered exactly once.
func TestHealthMetrics(t *testing.T) {
	loaded := func() *config.Config { return &config.Config{} }

	tests := []struct {
		name     string
		config   func() *config.Config
		store    Pinger
		wantCode int
	}{
		{
			name:     "config loaded and store reachable",
			config:   loaded,
			store:    &fakePinger{},
			wantCode: http.StatusOK,
		},
		{
			name:     "config not loaded",
			config:   func() *config.Config { return nil },
			store:    &fakePinger{},
			wantCode: http.StatusServiceUnavailable,
		},
		{
			name:     "config getter missing",
			config:   nil,
			store:    &fakePinger{},
			wantCode: http.StatusServiceUnavailable,
		},
		{
			name:     "store ping fails",
			config:   loaded,
			store:    &fakePinger{err: errors.New("database is down")},
			wantCode: http.StatusServiceUnavailable,
		},
		{
			name:     "store missing",
			config:   loaded,
			store:    nil,
			wantCode: http.StatusServiceUnavailable,
		},
	}

	for _, tc := range tests {
		t.Run("healthz/"+tc.name, func(t *testing.T) {
			server := NewServer(ServerOptions{
				Config:  tc.config,
				Store:   tc.store,
				Metrics: metrics.New(),
			})

			rec := httptest.NewRecorder()
			server.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

			if rec.Code != tc.wantCode {
				t.Fatalf("/healthz status = %d, want %d (body %q)", rec.Code, tc.wantCode, rec.Body.String())
			}
			if tc.wantCode == http.StatusOK && !strings.Contains(rec.Body.String(), "ok") {
				t.Errorf("/healthz body = %q, want an ok marker", rec.Body.String())
			}
			if tc.wantCode == http.StatusServiceUnavailable {
				for _, leak := range []string{"database is down", "sql"} {
					if strings.Contains(rec.Body.String(), leak) {
						t.Errorf("/healthz leaked store detail: %q", rec.Body.String())
					}
				}
			}
		})
	}

	t.Run("metrics serves the injected registry", func(t *testing.T) {
		registry := metrics.New()
		registry.AddOutboundJobs(7)
		registry.AddDropped(2)

		server := NewServer(ServerOptions{
			Config:  loaded,
			Store:   &fakePinger{},
			Metrics: registry,
		})

		rec := httptest.NewRecorder()
		server.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

		if rec.Code != http.StatusOK {
			t.Fatalf("/metrics status = %d, want 200", rec.Code)
		}
		if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/plain") {
			t.Fatalf("/metrics Content-Type = %q, want text/plain", got)
		}
		body := rec.Body.String()
		for _, want := range []string{
			"# TYPE aether_outbound_jobs_total counter",
			"aether_outbound_jobs_total 7",
			"aether_dropped_total 2",
		} {
			if !strings.Contains(body, want) {
				t.Errorf("/metrics body is missing %q:\n%s", want, body)
			}
		}
	})

	t.Run("every route is registered once", func(t *testing.T) {
		options := ServerOptions{
			Config:  loaded,
			Store:   &fakePinger{},
			Metrics: metrics.New(),
		}

		// A duplicate registration on the same mux panics inside net/http.
		// Building twice also proves no package-level mux is shared between
		// servers.
		first := NewServer(options)
		second := NewServer(options)

		for name, server := range map[string]http.Handler{"first": first, "second": second} {
			for _, tc := range []struct {
				method string
				path   string
				body   string
				want   int
			}{
				{http.MethodGet, "/healthz", "", http.StatusOK},
				{http.MethodGet, "/metrics", "", http.StatusOK},
				// Reaching the handlers (not a 404) proves the route exists.
				{http.MethodPost, "/receive", `{"sender":`, http.StatusBadRequest},
				{http.MethodPost, "/message", `{}`, http.StatusUnauthorized},
			} {
				rec := httptest.NewRecorder()
				req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
				server.ServeHTTP(rec, req)
				if rec.Code != tc.want {
					t.Errorf("%s: %s %s status = %d, want %d", name, tc.method, tc.path, rec.Code, tc.want)
				}
			}
		}

		// Unknown paths still 404; a catch-all route would mask a bad
		// registration.
		rec := httptest.NewRecorder()
		first.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/nope", nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("unknown path status = %d, want 404", rec.Code)
		}
	})
}
