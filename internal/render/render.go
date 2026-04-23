// Package render wraps the Render.com REST API calls the watchdog needs:
// reading service metrics and restarting services.
//
// NOTE: Render user API keys are account-scoped. Deploy this tool with a
// token minted for a dedicated "monitoring" Render user that only has access
// to the services you want restartable. If/when Render ships fine-grained
// workspace-scoped tokens, switch to one of those instead.
package render

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

const (
	defaultBaseURL   = "https://api.render.com/v1"
	requestTimeout   = 10 * time.Second
	bodyReadCap      = 1 << 20        // 1 MiB
	bodyLogPreview   = 2048           // bytes of body to include in info logs
	metricsLookback  = 3 * time.Minute // how far back to ask Render for samples
)

// Metrics is a snapshot of a Render service's resource usage. MemoryPercent
// and CPUPercent are computed as `(used / limit) * 100`; the raw used/limit
// values are also carried so callers can log them for diagnostics.
type Metrics struct {
	MemoryPercent    float64
	MemoryUsedBytes  float64
	MemoryLimitBytes float64

	CPUPercent float64
	CPUUsed    float64
	CPULimit   float64

	CollectedAt time.Time
}

// APIError reports a non-2xx response from Render.
type APIError struct {
	StatusCode int
	Body       string
	Op         string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("render api %s: status=%d body=%s", e.Op, e.StatusCode, e.Body)
}

// Client is a thin Render API wrapper.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
	logger  *slog.Logger
}

// New builds a Render API client with a sensible timeout. It logs each API
// call's URL, status, and a truncated response body via slog.Default().
func New(token string) *Client {
	return &Client{
		baseURL: defaultBaseURL,
		token:   token,
		http:    &http.Client{Timeout: requestTimeout},
		logger:  slog.Default(),
	}
}

// WithBaseURL is a testing hook.
func (c *Client) WithBaseURL(u string) *Client {
	c.baseURL = u
	return c
}

// RestartService triggers a restart of the given Render service.
func (c *Client) RestartService(ctx context.Context, serviceID string) error {
	if serviceID == "" {
		return errors.New("render: empty service id")
	}
	url := fmt.Sprintf("%s/services/%s/restart", c.baseURL, serviceID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(nil))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	c.setHeaders(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("restart: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, bodyReadCap))
	c.logResponse("restart", url, resp.StatusCode, body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &APIError{StatusCode: resp.StatusCode, Body: string(body), Op: "restart"}
	}
	return nil
}

// GetMetrics fetches the most recent memory and CPU usage plus their limits
// for the service and returns a Metrics with computed percentages. The four
// underlying HTTP calls are made in parallel.
func (c *Client) GetMetrics(ctx context.Context, serviceID string) (Metrics, error) {
	type result struct {
		sample metricSample
		err    error
	}

	kinds := []string{"memory", "memory-limit", "cpu", "cpu-limit"}
	results := make(map[string]result, len(kinds))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, k := range kinds {
		wg.Add(1)
		go func(k string) {
			defer wg.Done()
			s, err := c.fetchLatestSample(ctx, serviceID, k)
			mu.Lock()
			results[k] = result{s, err}
			mu.Unlock()
		}(k)
	}
	wg.Wait()

	for _, k := range kinds {
		if err := results[k].err; err != nil {
			return Metrics{}, fmt.Errorf("%s: %w", k, err)
		}
	}

	memUsed := results["memory"].sample
	memLimit := results["memory-limit"].sample
	cpuUsed := results["cpu"].sample
	cpuLimit := results["cpu-limit"].sample

	m := Metrics{
		MemoryUsedBytes:  memUsed.value,
		MemoryLimitBytes: memLimit.value,
		MemoryPercent:    ratio(memUsed.value, memLimit.value),

		CPUUsed:    cpuUsed.value,
		CPULimit:   cpuLimit.value,
		CPUPercent: ratio(cpuUsed.value, cpuLimit.value),

		CollectedAt: latestTime(memUsed.time, memLimit.time, cpuUsed.time, cpuLimit.time),
	}
	c.logger.Info("render metrics computed",
		"service", serviceID,
		"memory_used_bytes", m.MemoryUsedBytes,
		"memory_limit_bytes", m.MemoryLimitBytes,
		"memory_pct", m.MemoryPercent,
		"cpu_used", m.CPUUsed,
		"cpu_limit", m.CPULimit,
		"cpu_pct", m.CPUPercent,
	)
	return m, nil
}

func ratio(used, limit float64) float64 {
	if limit <= 0 {
		return 0
	}
	return (used / limit) * 100
}

func latestTime(ts ...time.Time) time.Time {
	var latest time.Time
	for _, t := range ts {
		if t.After(latest) {
			latest = t
		}
	}
	return latest
}

type metricSample struct {
	value float64
	unit  string
	time  time.Time
}

type metricSeries struct {
	Labels []struct {
		Field string `json:"field"`
		Value string `json:"value"`
	} `json:"labels"`
	Unit   string `json:"unit"`
	Values []struct {
		Timestamp time.Time `json:"timestamp"`
		Value     float64   `json:"value"`
	} `json:"values"`
}

// fetchLatestSample calls GET /v1/metrics/<kind>?resource=<id> and returns
// the most recent value across all series (handles multi-instance services
// by taking the max — the worst-case value is what the restart rule cares
// about).
func (c *Client) fetchLatestSample(ctx context.Context, serviceID, kind string) (metricSample, error) {
	now := time.Now().UTC()
	url := fmt.Sprintf("%s/metrics/%s?resource=%s&startTime=%s&endTime=%s",
		c.baseURL, kind, serviceID,
		now.Add(-metricsLookback).Format(time.RFC3339),
		now.Format(time.RFC3339),
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return metricSample{}, fmt.Errorf("build request: %w", err)
	}
	c.setHeaders(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return metricSample{}, fmt.Errorf("do: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, bodyReadCap))
	op := "metrics/" + kind
	c.logResponse(op, url, resp.StatusCode, body)

	if resp.StatusCode == http.StatusNotFound {
		return metricSample{time: time.Now()}, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return metricSample{}, &APIError{StatusCode: resp.StatusCode, Body: string(body), Op: op}
	}

	var series []metricSeries
	if err := json.Unmarshal(body, &series); err != nil {
		return metricSample{}, fmt.Errorf("decode: %w (body=%s)", err, truncate(body, bodyLogPreview))
	}
	if len(series) == 0 {
		return metricSample{time: time.Now()}, nil
	}

	var (
		sample metricSample
		found  bool
	)
	for _, s := range series {
		if len(s.Values) == 0 {
			continue
		}
		last := s.Values[len(s.Values)-1]
		if !found || last.Value > sample.value {
			sample = metricSample{value: last.Value, unit: s.Unit, time: last.Timestamp}
			found = true
		}
	}
	if !found {
		return metricSample{time: time.Now()}, nil
	}
	return sample, nil
}

func (c *Client) logResponse(op, url string, status int, body []byte) {
	if c.logger == nil {
		return
	}
	c.logger.Info("render api",
		"op", op,
		"url", url,
		"status", status,
		"bytes", len(body),
		"body", truncate(body, bodyLogPreview),
	)
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "...(truncated)"
}

func (c *Client) setHeaders(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
}
