package render

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRestartService_Success(t *testing.T) {
	var gotAuth, gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	c := New("tok").WithBaseURL(srv.URL)
	if err := c.RestartService(context.Background(), "svc-abc"); err != nil {
		t.Fatalf("RestartService: %v", err)
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("auth %q", gotAuth)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method %q", gotMethod)
	}
	if gotPath != "/services/svc-abc/restart" {
		t.Errorf("path %q", gotPath)
	}
}

func TestRestartService_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"boom"}`))
	}))
	defer srv.Close()

	c := New("tok").WithBaseURL(srv.URL)
	err := c.RestartService(context.Background(), "svc")
	if err == nil {
		t.Fatal("expected error")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected APIError, got %T", err)
	}
	if apiErr.StatusCode != http.StatusInternalServerError {
		t.Errorf("status %d", apiErr.StatusCode)
	}
	if !strings.Contains(apiErr.Body, "boom") {
		t.Errorf("body %s", apiErr.Body)
	}
}

func TestRestartService_EmptyID(t *testing.T) {
	c := New("tok")
	if err := c.RestartService(context.Background(), ""); err == nil {
		t.Fatal("expected error for empty id")
	}
}

// metricSeriesResponse matches the shape Render actually returns:
// `[{"labels":[...],"unit":"...","values":[{"timestamp","value"}]}]`.
func writeSeries(w http.ResponseWriter, unit string, value float64, at time.Time) {
	resp := []map[string]any{
		{
			"labels": []map[string]any{{"field": "service", "value": "svc"}},
			"unit":   unit,
			"values": []map[string]any{
				{"timestamp": at.Add(-time.Minute), "value": value * 0.5},
				{"timestamp": at, "value": value},
			},
		},
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func TestGetMetrics_Success(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// startTime / endTime query params should be present
		if q := r.URL.Query().Get("startTime"); q == "" {
			t.Errorf("expected startTime query param")
		}
		if q := r.URL.Query().Get("endTime"); q == "" {
			t.Errorf("expected endTime query param")
		}
		switch {
		case strings.Contains(r.URL.Path, "/metrics/memory-limit"):
			writeSeries(w, "bytes", 10_000_000_000, now) // 10 GB limit
		case strings.Contains(r.URL.Path, "/metrics/memory"):
			writeSeries(w, "bytes", 8_500_000_000, now) // 8.5 GB used → 85%
		case strings.Contains(r.URL.Path, "/metrics/cpu-limit"):
			writeSeries(w, "cores", 2.0, now) // 2 cores allocated
		case strings.Contains(r.URL.Path, "/metrics/cpu"):
			writeSeries(w, "cores", 1.0, now) // 1 core used → 50%
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := New("tok").WithBaseURL(srv.URL)
	m, err := c.GetMetrics(context.Background(), "svc")
	if err != nil {
		t.Fatalf("GetMetrics: %v", err)
	}
	if m.MemoryUsedBytes != 8_500_000_000 {
		t.Errorf("memory used: got %v", m.MemoryUsedBytes)
	}
	if m.MemoryLimitBytes != 10_000_000_000 {
		t.Errorf("memory limit: got %v", m.MemoryLimitBytes)
	}
	if m.MemoryPercent != 85.0 {
		t.Errorf("memory pct: got %v, want 85.0", m.MemoryPercent)
	}
	if m.CPUPercent != 50.0 {
		t.Errorf("cpu pct: got %v, want 50.0", m.CPUPercent)
	}
}

func TestGetMetrics_MissingLimitYieldsZeroPercent(t *testing.T) {
	// Limit endpoints 404 → limit treated as 0 → ratio returns 0 (safe — won't
	// trigger a restart rather than dividing by zero).
	now := time.Now().UTC().Truncate(time.Second)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "-limit"):
			w.WriteHeader(http.StatusNotFound)
		case strings.Contains(r.URL.Path, "/metrics/memory"):
			writeSeries(w, "bytes", 5_000_000_000, now)
		case strings.Contains(r.URL.Path, "/metrics/cpu"):
			writeSeries(w, "cores", 0.5, now)
		}
	}))
	defer srv.Close()

	c := New("tok").WithBaseURL(srv.URL)
	m, err := c.GetMetrics(context.Background(), "svc")
	if err != nil {
		t.Fatalf("GetMetrics: %v", err)
	}
	if m.MemoryPercent != 0 || m.CPUPercent != 0 {
		t.Errorf("expected zero percentages when limits missing, got mem=%v cpu=%v",
			m.MemoryPercent, m.CPUPercent)
	}
	if m.MemoryUsedBytes != 5_000_000_000 {
		t.Errorf("raw memory used still captured: got %v", m.MemoryUsedBytes)
	}
}

func TestGetMetrics_MultiInstanceTakesMax(t *testing.T) {
	// When a service has multiple instances, Render returns one series per
	// instance. We take the max so the restart rule reflects the worst case.
	now := time.Now().UTC().Truncate(time.Second)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := []map[string]any{
			{
				"labels": []map[string]any{{"field": "instance", "value": "a"}},
				"unit":   "bytes",
				"values": []map[string]any{{"timestamp": now, "value": 3_000_000_000.0}},
			},
			{
				"labels": []map[string]any{{"field": "instance", "value": "b"}},
				"unit":   "bytes",
				"values": []map[string]any{{"timestamp": now, "value": 9_000_000_000.0}},
			},
		}
		if strings.Contains(r.URL.Path, "-limit") {
			resp = []map[string]any{{
				"labels": []map[string]any{{"field": "instance", "value": "a"}},
				"unit":   "bytes",
				"values": []map[string]any{{"timestamp": now, "value": 10_000_000_000.0}},
			}}
		}
		if strings.Contains(r.URL.Path, "cpu") {
			resp = []map[string]any{{
				"labels": []map[string]any{{"field": "instance", "value": "a"}},
				"unit":   "cores",
				"values": []map[string]any{{"timestamp": now, "value": 0.1}},
			}}
			if strings.Contains(r.URL.Path, "-limit") {
				resp = []map[string]any{{
					"labels": []map[string]any{{"field": "instance", "value": "a"}},
					"unit":   "cores",
					"values": []map[string]any{{"timestamp": now, "value": 2.0}},
				}}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := New("tok").WithBaseURL(srv.URL)
	m, err := c.GetMetrics(context.Background(), "svc")
	if err != nil {
		t.Fatalf("GetMetrics: %v", err)
	}
	// Worst instance: 9 GB used / 10 GB limit = 90%
	if m.MemoryUsedBytes != 9_000_000_000 {
		t.Errorf("memory used (max across instances): got %v", m.MemoryUsedBytes)
	}
	if m.MemoryPercent != 90.0 {
		t.Errorf("memory pct: got %v, want 90.0", m.MemoryPercent)
	}
}
