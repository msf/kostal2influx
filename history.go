package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"time"
)

const (
	historySource           = "history_10m_avg"
	maxHistoryResponseBytes = 16 << 20
)

type historyResponse struct {
	DayCurves historyCurves `json:"DayCurves"`
}

type historyCurves struct {
	Unit          string           `json:"Unit"`
	IncrementUnit string           `json:"IncrementUnit"`
	IncrementStep int              `json:"IncrementStep"`
	Datasets      []historyDataset `json:"Datasets"`
}

type historyDataset struct {
	Type    string          `json:"Type"`
	Default float64         `json:"Default"`
	Data    []historyPeriod `json:"Data"`
}

type historyPeriod struct {
	Timestamp string    `json:"Timestamp"`
	Offset    int       `json:"Offset"`
	Total     int       `json:"Total"`
	Data      []float64 `json:"Data"`
}

type historyPoint struct {
	metric    string
	value     float64
	timestamp int64
}

var historyInputMetrics = []struct {
	typeName   string
	metricName string
}{
	{typeName: "Produced", metricName: "kostal_AC_Power_W"},
	{typeName: "Consumed", metricName: "kostal_grid_consumed_watts"},
	{typeName: "Injected", metricName: "kostal_grid_injected_watts"},
}

func importInverterHistory(ctx context.Context, kostalHost string, timestampOffset time.Duration, vmc *vmClient) (int, error) {
	stats, err := getMeasurements(kostalHost)
	if err != nil {
		return 0, fmt.Errorf("read inverter clock: %w", err)
	}
	deviceTime, err := time.ParseInLocation("2006-01-02T15:04:05", stats.Device.DateTime, time.UTC)
	if err != nil {
		return 0, fmt.Errorf("parse inverter time %q: %w", stats.Device.DateTime, err)
	}

	history, err := getHistory(ctx, kostalHost)
	if err != nil {
		return 0, err
	}
	points, err := buildHistoryPoints(history, deviceTime, timestampOffset)
	if err != nil {
		return 0, err
	}

	pointsByMetric := make(map[string][]historyPoint)
	for _, point := range points {
		pointsByMetric[point.metric] = append(pointsByMetric[point.metric], point)
	}
	var payload bytes.Buffer
	for metric, metricPoints := range pointsByMetric {
		existing, err := vmc.existingBuckets(ctx, metric, stats.Device.Name, metricPoints[0].timestamp, metricPoints[len(metricPoints)-1].timestamp)
		if err != nil {
			return 0, fmt.Errorf("find existing %s buckets: %w", metric, err)
		}
		for _, point := range metricPoints {
			if !existing[point.timestamp] {
				appendHistoryMetric(&payload, point, stats.Device.Name)
			}
		}
	}
	if payload.Len() == 0 {
		return 0, nil
	}
	if err := vmc.PostMetrics(ctx, payload.Bytes()); err != nil {
		return 0, err
	}
	return bytes.Count(payload.Bytes(), []byte{'\n'}), nil
}

func getHistory(ctx context.Context, kostalHost string) (*historyResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+kostalHost+"/yields.json?day=1", nil)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Timeout: 2 * time.Minute}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("get inverter history: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("get inverter history: status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxHistoryResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read inverter history: %w", err)
	}
	if len(body) > maxHistoryResponseBytes {
		return nil, fmt.Errorf("inverter history exceeds %d bytes", maxHistoryResponseBytes)
	}
	var history historyResponse
	if err := json.Unmarshal(body, &history); err != nil {
		return nil, fmt.Errorf("decode inverter history: %w", err)
	}
	return &history, nil
}

type historyPowerSample struct {
	values  [3]float64
	present [3]bool
}

func buildHistoryPoints(history *historyResponse, deviceTime time.Time, timestampOffset time.Duration) ([]historyPoint, error) {
	curves := history.DayCurves
	if curves.Unit != "W" || curves.IncrementUnit != "m" || curves.IncrementStep <= 0 {
		return nil, fmt.Errorf("unsupported day curves: unit=%q increment=%d%s", curves.Unit, curves.IncrementStep, curves.IncrementUnit)
	}
	step := time.Duration(curves.IncrementStep) * time.Minute
	currentBucket := deviceTime.Truncate(step)
	samples := make(map[int64]*historyPowerSample)
	datasets := make(map[string]historyDataset, len(curves.Datasets))
	for _, dataset := range curves.Datasets {
		datasets[dataset.Type] = dataset
	}

	for metricIndex, metric := range historyInputMetrics {
		dataset, ok := datasets[metric.typeName]
		if !ok {
			return nil, fmt.Errorf("history dataset %q is missing", metric.typeName)
		}
		for _, period := range dataset.Data {
			day, err := time.ParseInLocation("2006-01-02", period.Timestamp, time.UTC)
			if err != nil {
				return nil, fmt.Errorf("parse %s day %q: %w", metric.typeName, period.Timestamp, err)
			}
			if period.Total <= 0 || period.Offset < 0 || period.Offset > period.Total || len(period.Data) > period.Total-period.Offset {
				return nil, fmt.Errorf("invalid %s day %s: offset=%d total=%d values=%d", metric.typeName, period.Timestamp, period.Offset, period.Total, len(period.Data))
			}
			for i := 0; i < period.Total; i++ {
				timestamp := day.Add(time.Duration(i) * step)
				if !timestamp.Before(currentBucket) {
					break
				}
				value := dataset.Default
				if i >= period.Offset && i < period.Offset+len(period.Data) {
					value = period.Data[i-period.Offset]
				}
				millis := timestamp.Add(timestampOffset).UnixMilli()
				sample := samples[millis]
				if sample == nil {
					sample = &historyPowerSample{}
					samples[millis] = sample
				}
				sample.values[metricIndex] = value
				sample.present[metricIndex] = true
			}
		}
	}

	timestamps := make([]int64, 0, len(samples))
	for timestamp := range samples {
		timestamps = append(timestamps, timestamp)
	}
	sort.Slice(timestamps, func(i, j int) bool { return timestamps[i] < timestamps[j] })

	points := make([]historyPoint, 0, len(timestamps)*4)
	for _, timestamp := range timestamps {
		sample := samples[timestamp]
		if !sample.present[0] || !sample.present[1] || !sample.present[2] {
			return nil, fmt.Errorf("incomplete history sample at %d", timestamp)
		}
		for i, metric := range historyInputMetrics {
			points = append(points, historyPoint{metric: metric.metricName, value: sample.values[i], timestamp: timestamp})
		}
		points = append(points, historyPoint{
			metric:    "kostal_own_consumed_watts",
			value:     math.Max(sample.values[0]-sample.values[2], 0),
			timestamp: timestamp,
		})
	}
	return points, nil
}

func (c *vmClient) existingBuckets(ctx context.Context, metric, device string, firstTimestamp, lastTimestamp int64) (map[int64]bool, error) {
	const halfBucket = 5 * time.Minute
	endpoint, err := url.Parse(c.baseURL + "/api/v1/query_range")
	if err != nil {
		return nil, err
	}
	query := endpoint.Query()
	query.Set("query", fmt.Sprintf("count_over_time(%s{device=%q,source=\"\"}[10m])", metric, device))
	query.Set("start", strconv.FormatInt(firstTimestamp+halfBucket.Milliseconds(), 10))
	query.Set("end", strconv.FormatInt(lastTimestamp+halfBucket.Milliseconds(), 10))
	query.Set("step", "10m")
	endpoint.RawQuery = query.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxHistoryResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxHistoryResponseBytes {
		return nil, fmt.Errorf("VictoriaMetrics query exceeds %d bytes", maxHistoryResponseBytes)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, body)
	}

	var result struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Data   struct {
			Result []struct {
				Values [][]json.RawMessage `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}
	if result.Status != "success" {
		return nil, fmt.Errorf("query failed: %s", result.Error)
	}

	existing := make(map[int64]bool)
	for _, series := range result.Data.Result {
		for _, value := range series.Values {
			if len(value) != 2 {
				continue
			}
			var seconds float64
			var count string
			if json.Unmarshal(value[0], &seconds) != nil || json.Unmarshal(value[1], &count) != nil {
				continue
			}
			parsedCount, err := strconv.ParseFloat(count, 64)
			if err == nil && parsedCount > 0 {
				timestamp := int64(math.Round(seconds*1000)) - halfBucket.Milliseconds()
				existing[timestamp] = true
			}
		}
	}
	return existing, nil
}

func appendHistoryMetric(b *bytes.Buffer, point historyPoint, device string) {
	fmt.Fprintf(b, "%s{device=%q,source=%q} %s %d\n", point.metric, device, historySource, strconv.FormatFloat(point.value, 'g', -1, 64), point.timestamp)
}
