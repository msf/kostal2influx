package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// vmConfig is the parsed command-line configuration for the VictoriaMetrics sink.
type vmConfig struct {
	host    string
	port    string
	token   string
	timeout time.Duration
	retries int
	// pace delays every request. The backfill uses its own paced client so a
	// multi-megabyte import never competes with the live 5-second writes.
	pace time.Duration
}

// vmClient is a long-lived writer to a VictoriaMetrics import endpoint.
type vmClient struct {
	baseURL string
	url     string
	token   string
	retries int
	pace    time.Duration
	http    *http.Client
	logger  *slog.Logger
}

func newVMClient(cfg vmConfig, logger *slog.Logger) *vmClient {
	port := cfg.port
	if port == "" {
		port = "8428"
	}
	baseURL := fmt.Sprintf("http://%s:%s", cfg.host, port)
	return &vmClient{
		baseURL: baseURL,
		url:     baseURL + "/api/v1/import/prometheus",
		token:   cfg.token,
		retries: cfg.retries,
		pace:    cfg.pace,
		http:    &http.Client{Timeout: cfg.timeout},
		logger:  logger,
	}
}

// buildVMPayload renders the readings as a Prometheus exposition payload: every
// raw measurement, plus the derived power metrics when the reading is consistent.
func buildVMPayload(stats *Root, power kostalPower, now time.Time) []byte {
	var b bytes.Buffer
	ts := now.UnixMilli()
	device := stats.Device.Name

	for _, m := range stats.Device.Measurements.Measurement {
		name := sanitizeMetricName(fmt.Sprintf("kostal_%s_%s", m.Type, m.Unit))
		fmt.Fprintf(&b, "%s{device=%q} %v %d\n", name, device, m.Value, ts)
	}

	if power.Error() == nil {
		fmt.Fprintf(&b, "kostal_total_power_watts{device=%q} %v %d\n", device, power.Total(), ts)
		fmt.Fprintf(&b, "kostal_own_consumed_watts{device=%q} %v %d\n", device, power.ownConsumed, ts)
		fmt.Fprintf(&b, "kostal_grid_consumed_watts{device=%q} %v %d\n", device, power.gridConsumed, ts)
		fmt.Fprintf(&b, "kostal_grid_injected_watts{device=%q} %v %d\n", device, power.gridInjected, ts)
	}

	return b.Bytes()
}

// PostMetrics submits a prebuilt payload, retrying with linear backoff. It drains
// and closes each response body so the keep-alive connection can be reused.
func (c *vmClient) PostMetrics(ctx context.Context, payload []byte) error {
	if err := c.waitPace(ctx); err != nil {
		return err
	}
	var lastErr error
	for attempt := 0; attempt <= c.retries; attempt++ {
		if attempt > 0 {
			c.logger.Warn("retrying victoriametrics write", "attempt", attempt)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt) * time.Second):
			}
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "text/plain")
		if c.token != "" {
			req.Header.Set("Authorization", "Bearer "+c.token)
		}

		resp, err := c.http.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode >= 300 {
			lastErr = fmt.Errorf("vm status %d: %s", resp.StatusCode, body)
			continue
		}
		return nil
	}
	return fmt.Errorf("victoriametrics write failed after %d attempts: %w", c.retries+1, lastErr)
}

// waitPace throttles a client that is deliberately slow, and doubles as the
// cancellation check for long backfill loops.
func (c *vmClient) waitPace(ctx context.Context) error {
	if c.pace <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(c.pace)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

var invalidMetricChars = regexp.MustCompile(`[^a-zA-Z0-9_:]`)

// sanitizeMetricName maps a raw measurement name to a valid Prometheus metric name.
func sanitizeMetricName(name string) string {
	name = strings.ReplaceAll(name, "%", "percent")
	name = strings.ReplaceAll(name, ".", "_")
	name = invalidMetricChars.ReplaceAllString(name, "_")
	name = strings.TrimLeft(name, "0123456789")
	return name
}
