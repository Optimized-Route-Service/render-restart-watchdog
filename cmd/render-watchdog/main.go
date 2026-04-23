// Command render-watchdog reads CPU and memory metrics for each configured
// Render service and, when either crosses the configured threshold, triggers
// a Render API restart for that service. It is a one-shot CLI intended to be
// invoked on a cron schedule; the cron cadence is the effective cooldown
// between restarts.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/xrouten/render-watchdog/internal/render"
)

const runTimeout = 60 * time.Second

type config struct {
	token        string
	serviceIDs   []string
	memThreshold float64
	cpuThreshold float64
	dryRun       bool
	webhookURL   string
}

func loadConfig() (config, error) {
	c := config{
		memThreshold: envFloat("MEM_THRESHOLD_PERCENT", 90),
		cpuThreshold: envFloat("CPU_THRESHOLD_PERCENT", 95),
		dryRun:       envBool("DRY_RUN"),
		webhookURL:   strings.TrimSpace(os.Getenv("WEBHOOK_URL")),
	}
	c.token = os.Getenv("RENDER_API_TOKEN")
	if c.token == "" {
		return c, errors.New("RENDER_API_TOKEN is required")
	}
	raw := os.Getenv("RENDER_SERVICE_IDS")
	if raw == "" {
		return c, errors.New("RENDER_SERVICE_IDS is required (comma-separated)")
	}
	for _, id := range strings.Split(raw, ",") {
		if id = strings.TrimSpace(id); id != "" {
			c.serviceIDs = append(c.serviceIDs, id)
		}
	}
	if len(c.serviceIDs) == 0 {
		return c, errors.New("RENDER_SERVICE_IDS contained no valid ids")
	}
	return c, nil
}

func envFloat(key string, def float64) float64 {
	if v, err := strconv.ParseFloat(os.Getenv(key), 64); err == nil && v > 0 {
		return v
	}
	return def
}

func envBool(key string) bool {
	switch strings.ToLower(os.Getenv(key)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// action describes what happened to a service this run.
type action int

const (
	actOK action = iota
	actWouldRestart
	actRestarted
	actMetricsFailed
	actRestartFailed
)

func (a action) label() string {
	switch a {
	case actOK:
		return "OK"
	case actWouldRestart:
		return "OVER (dry-run)"
	case actRestarted:
		return "RESTARTED"
	case actMetricsFailed:
		return "METRICS FAILED"
	case actRestartFailed:
		return "RESTART FAILED"
	}
	return "?"
}

// event returns the snake_case name used in webhook payloads.
func (a action) event() string {
	switch a {
	case actOK:
		return "ok"
	case actWouldRestart:
		return "would_restart"
	case actRestarted:
		return "restarted"
	case actMetricsFailed:
		return "metrics_failed"
	case actRestartFailed:
		return "restart_failed"
	}
	return "unknown"
}

// notifiable reports whether this action should trigger a webhook.
// OK is noisy and gets suppressed; everything else is operator-relevant.
func (a action) notifiable() bool {
	return a != actOK
}

type result struct {
	serviceID string
	metrics   render.Metrics
	reason    string
	action    action
}

func (r result) notifiable() bool { return r.action.notifiable() }

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	slog.SetDefault(logger)

	cfg, err := loadConfig()
	if err != nil {
		logger.Error("config", "err", err.Error())
		fmt.Fprintln(os.Stderr, "config error:", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
	defer cancel()

	client := render.New(cfg.token)

	logger.Info("starting",
		"services", cfg.serviceIDs,
		"mem_threshold_pct", cfg.memThreshold,
		"cpu_threshold_pct", cfg.cpuThreshold,
		"dry_run", cfg.dryRun,
	)

	results := make([]result, len(cfg.serviceIDs))
	var wg sync.WaitGroup
	for i, id := range cfg.serviceIDs {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			r := checkAndMaybeRestart(ctx, client, cfg, logger, id)
			results[i] = r
			if cfg.webhookURL != "" && r.notifiable() {
				notifyWebhook(ctx, cfg.webhookURL, r, cfg, logger)
			}
		}(i, id)
	}
	wg.Wait()

	printSummary(os.Stdout, results, cfg)

	for _, r := range results {
		if r.action == actMetricsFailed || r.action == actRestartFailed {
			os.Exit(2)
		}
	}
}

func checkAndMaybeRestart(ctx context.Context, client *render.Client, cfg config, logger *slog.Logger, id string) result {
	metrics, err := client.GetMetrics(ctx, id)
	if err != nil {
		logger.Error("metrics failed", "service", id, "err", err.Error())
		return result{serviceID: id, action: actMetricsFailed, reason: err.Error()}
	}

	reason, over := overThreshold(metrics, cfg)
	if !over {
		return result{serviceID: id, metrics: metrics, action: actOK}
	}

	if cfg.dryRun {
		logger.Warn("would restart (DRY_RUN)", "service", id, "reason", reason)
		return result{serviceID: id, metrics: metrics, reason: reason, action: actWouldRestart}
	}

	logger.Warn("restarting", "service", id, "reason", reason)
	if err := client.RestartService(ctx, id); err != nil {
		logger.Error("restart failed", "service", id, "err", err.Error())
		return result{serviceID: id, metrics: metrics, reason: reason, action: actRestartFailed}
	}
	return result{serviceID: id, metrics: metrics, reason: reason, action: actRestarted}
}

func overThreshold(m render.Metrics, cfg config) (string, bool) {
	if m.MemoryPercent >= cfg.memThreshold {
		return fmt.Sprintf("memory %.1f%% >= %.1f%%", m.MemoryPercent, cfg.memThreshold), true
	}
	if m.CPUPercent >= cfg.cpuThreshold {
		return fmt.Sprintf("cpu %.1f%% >= %.1f%%", m.CPUPercent, cfg.cpuThreshold), true
	}
	return "", false
}

func printSummary(w io.Writer, results []result, cfg config) {
	mode := ""
	if cfg.dryRun {
		mode = " (DRY_RUN)"
	}

	sorted := make([]result, len(results))
	copy(sorted, results)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].serviceID < sorted[j].serviceID })

	const sep = "────────────────────────────────────────────────────────────────────────────────"
	fmt.Fprintln(w)
	fmt.Fprintf(w, "render-watchdog%s — %d service(s), thresholds mem≥%.0f%% cpu≥%.0f%%\n",
		mode, len(results), cfg.memThreshold, cfg.cpuThreshold)
	fmt.Fprintln(w, sep)

	var restarted, wouldRestart, failed int
	for _, r := range sorted {
		fmt.Fprintln(w, formatResult(r))
		switch r.action {
		case actRestarted:
			restarted++
		case actWouldRestart:
			wouldRestart++
		case actMetricsFailed, actRestartFailed:
			failed++
		}
	}

	fmt.Fprintln(w, sep)
	fmt.Fprintf(w, "%d OK, %d restarted, %d would-restart, %d failed\n",
		len(results)-restarted-wouldRestart-failed, restarted, wouldRestart, failed)
}

func formatResult(r result) string {
	if r.action == actMetricsFailed {
		return fmt.Sprintf("  %-28s  %s — %s", r.serviceID, r.action.label(), truncate(r.reason, 80))
	}
	m := r.metrics
	line := fmt.Sprintf(
		"  %-28s  mem %5.1f%% (%s / %s)   cpu %5.2f%% (%.3f / %.0f cores)   %s",
		r.serviceID,
		m.MemoryPercent, humanBytes(m.MemoryUsedBytes), humanBytes(m.MemoryLimitBytes),
		m.CPUPercent, m.CPUUsed, m.CPULimit,
		r.action.label(),
	)
	if r.reason != "" && r.action != actOK {
		line += "\n    → " + r.reason
	}
	return line
}

func humanBytes(b float64) string {
	const (
		KiB = 1024.0
		MiB = 1024.0 * 1024
		GiB = 1024.0 * 1024 * 1024
	)
	switch {
	case b >= GiB:
		return fmt.Sprintf("%.1f GiB", b/GiB)
	case b >= MiB:
		return fmt.Sprintf("%.1f MiB", b/MiB)
	case b >= KiB:
		return fmt.Sprintf("%.1f KiB", b/KiB)
	default:
		return fmt.Sprintf("%.0f B", b)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// webhookPayload is the JSON posted to WEBHOOK_URL. Structured to be friendly
// to Zapier / Make / generic webhook consumers: flat-ish, snake_case, with
// both raw numbers and pre-formatted human strings so the consumer can pick.
type webhookPayload struct {
	Event     string  `json:"event"`
	ServiceID string  `json:"service_id"`
	Timestamp string  `json:"timestamp"`
	Reason    string  `json:"reason,omitempty"`
	DryRun    bool    `json:"dry_run"`
	Message   string  `json:"message"`
	Metrics   payloadMetrics `json:"metrics"`
	Thresholds payloadThresholds `json:"thresholds"`
}

type payloadMetrics struct {
	MemoryUsedBytes  float64 `json:"memory_used_bytes"`
	MemoryLimitBytes float64 `json:"memory_limit_bytes"`
	MemoryPercent    float64 `json:"memory_percent"`
	MemoryHuman      string  `json:"memory_human"`
	CPUUsed          float64 `json:"cpu_used"`
	CPULimit         float64 `json:"cpu_limit"`
	CPUPercent       float64 `json:"cpu_percent"`
}

type payloadThresholds struct {
	MemoryPercent float64 `json:"memory_percent"`
	CPUPercent    float64 `json:"cpu_percent"`
}

func buildMessage(r result, cfg config) string {
	switch r.action {
	case actRestarted:
		return fmt.Sprintf("Render service %s restarted — %s", r.serviceID, r.reason)
	case actWouldRestart:
		return fmt.Sprintf("Render service %s would be restarted (DRY_RUN) — %s", r.serviceID, r.reason)
	case actRestartFailed:
		return fmt.Sprintf("Render service %s RESTART FAILED — %s", r.serviceID, r.reason)
	case actMetricsFailed:
		return fmt.Sprintf("Render service %s metrics fetch failed — %s", r.serviceID, r.reason)
	}
	return fmt.Sprintf("Render service %s — %s", r.serviceID, r.action.label())
}

func notifyWebhook(ctx context.Context, url string, r result, cfg config, logger *slog.Logger) {
	payload := webhookPayload{
		Event:     r.action.event(),
		ServiceID: r.serviceID,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Reason:    r.reason,
		DryRun:    cfg.dryRun,
		Message: buildMessage(r, cfg),
		Metrics: payloadMetrics{
			MemoryUsedBytes:  r.metrics.MemoryUsedBytes,
			MemoryLimitBytes: r.metrics.MemoryLimitBytes,
			MemoryPercent:    r.metrics.MemoryPercent,
			MemoryHuman:      fmt.Sprintf("%s / %s", humanBytes(r.metrics.MemoryUsedBytes), humanBytes(r.metrics.MemoryLimitBytes)),
			CPUUsed:          r.metrics.CPUUsed,
			CPULimit:         r.metrics.CPULimit,
			CPUPercent:       r.metrics.CPUPercent,
		},
		Thresholds: payloadThresholds{
			MemoryPercent: cfg.memThreshold,
			CPUPercent:    cfg.cpuThreshold,
		},
	}

	body, err := json.Marshal(payload)
	if err != nil {
		logger.Error("webhook marshal", "err", err.Error())
		return
	}

	webhookCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(webhookCtx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		logger.Error("webhook build", "err", err.Error())
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "render-watchdog/1.0")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		// Don't log the URL itself — Zapier/Make URLs contain path tokens.
		logger.Error("webhook send failed", "service", r.serviceID, "event", payload.Event, "err", err.Error())
		return
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<14))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		logger.Error("webhook non-2xx", "service", r.serviceID, "event", payload.Event, "status", resp.StatusCode)
		return
	}
	logger.Info("webhook sent", "service", r.serviceID, "event", payload.Event, "status", resp.StatusCode)
}
