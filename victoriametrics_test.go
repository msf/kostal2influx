package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func statsWith(device string, ms ...Measurement) *Root {
	var r Root
	r.Device.Name = device
	r.Device.Measurements.Measurement = ms
	return &r
}

func TestBuildVMPayload(t *testing.T) {
	stats := statsWith("test-device", Measurement{Value: 223.3, Unit: "V", Type: "AC_Voltage"})
	power := kostalPower{gridConsumed: 1000, ownConsumed: 500} // consistent → derived metrics included
	ts := time.UnixMilli(1700000000000).UTC()

	want := `kostal_AC_Voltage_V{device="test-device"} 223.3 1700000000000
kostal_total_power_watts{device="test-device"} 1500 1700000000000
kostal_own_consumed_watts{device="test-device"} 500 1700000000000
kostal_grid_consumed_watts{device="test-device"} 1000 1700000000000
kostal_grid_injected_watts{device="test-device"} 0 1700000000000
`
	require.Equal(t, want, string(buildVMPayload(stats, power, ts)))
}

func TestBuildVMPayloadInconsistentPowerOmitsDerived(t *testing.T) {
	stats := statsWith("dev", Measurement{Value: 50.0, Unit: "Hz", Type: "AC_Frequency"})
	power := kostalPower{} // all zero → inconsistent → no derived metrics
	ts := time.UnixMilli(1700000000000).UTC()

	got := string(buildVMPayload(stats, power, ts))
	require.Equal(t, "kostal_AC_Frequency_Hz{device=\"dev\"} 50 1700000000000\n", got)
	require.NotContains(t, got, "kostal_total_power_watts")
}

func TestSanitizeMetricName(t *testing.T) {
	tests := []struct{ in, want string }{
		{"kostal_AC_Voltage_V", "kostal_AC_Voltage_V"},
		{"Power/W", "Power_W"},
		{"Temp %", "Temp_percent"},
		{"metric-with-hyphens", "metric_with_hyphens"},
		{"123leading_digits", "leading_digits"},
	}
	for _, tt := range tests {
		require.Equal(t, tt.want, sanitizeMetricName(tt.in), tt.in)
	}
}

func TestPostMetrics(t *testing.T) {
	var got string
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		got = string(body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	hostPort := strings.TrimPrefix(srv.URL, "http://")
	host, port, _ := strings.Cut(hostPort, ":")
	c := newVMClient(vmConfig{host: host, port: port, token: "tok", timeout: 5 * time.Second}, slog.New(slog.DiscardHandler))

	err := c.PostMetrics(context.Background(), []byte("metric 1 2\n"))
	require.NoError(t, err)
	require.Equal(t, "metric 1 2\n", got)
	require.Equal(t, "Bearer tok", auth)
}

func TestPostMetricsRetriesThenFails(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	hostPort := strings.TrimPrefix(srv.URL, "http://")
	host, port, _ := strings.Cut(hostPort, ":")
	c := newVMClient(vmConfig{host: host, port: port, retries: 1, timeout: time.Second}, slog.New(slog.DiscardHandler))

	err := c.PostMetrics(context.Background(), []byte("x 1 2\n"))
	require.Error(t, err)
	require.Equal(t, 2, calls) // initial + 1 retry
}
