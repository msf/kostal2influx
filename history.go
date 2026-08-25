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

// source label values, in descending fidelity. Real-time samples carry no
// source label at all, so `sum without (source) (metric)` merges every tier
// into a single series while the label still identifies each backfill.
const (
	source10mAvg     = "history_10m_avg"
	sourceDailyAvg   = "history_daily_avg"
	sourceMonthlyAvg = "history_monthly_avg"
)

const (
	maxHistoryResponseBytes = 16 << 20
	maxImportBatchLines     = 50000
	hoursPerDay             = 24
)

type historyResponse struct {
	DayCurves   historyCurves `json:"DayCurves"`
	MonthCurves energyCurves  `json:"MonthCurves"`
	YearCurves  energyCurves  `json:"YearCurves"`
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

// energyCurves are the low-resolution yield curves: MonthCurves holds one Wh
// total per day of each month, YearCurves one Wh total per month of each year.
type energyCurves struct {
	Unit          string          `json:"Unit"`
	IncrementUnit string          `json:"IncrementUnit"`
	IncrementStep int             `json:"IncrementStep"`
	Datasets      []energyDataset `json:"Datasets"`
}

type energyDataset struct {
	Type string         `json:"Type"`
	Data []energyPeriod `json:"Data"`
}

type energyPeriod struct {
	Timestamp string    `json:"Timestamp"`
	Data      []float64 `json:"Data"`
}

type historyPoint struct {
	metric    string
	source    string
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

// historyMetricNames lists every metric the backfill writes, real-time included.
var historyMetricNames = []string{
	"kostal_AC_Power_W",
	"kostal_grid_consumed_watts",
	"kostal_grid_injected_watts",
	"kostal_own_consumed_watts",
}

// importStats counts the samples written per fidelity tier.
type importStats struct {
	tenMinute int
	daily     int
	monthly   int
	offset    time.Duration
}

func (s importStats) total() int { return s.tenMinute + s.daily + s.monthly }

// importInverterHistory backfills VictoriaMetrics from the inverter's own
// history, best resolution first: 10-minute average power for the rolling
// 31 days, then per-day averages for the last 13 months, then per-month
// averages for the remaining years. Lower tiers only fill calendar days that
// hold no samples at all, so no energy is ever counted twice.
func importInverterHistory(ctx context.Context, kostalHost string, offsetOverride *time.Duration, vmc *vmClient) (importStats, error) {
	var stats importStats

	measurements, err := getMeasurements(kostalHost)
	if err != nil {
		return stats, fmt.Errorf("read inverter clock: %w", err)
	}
	device := measurements.Device.Name
	deviceTime, err := time.ParseInLocation("2006-01-02T15:04:05", measurements.Device.DateTime, time.UTC)
	if err != nil {
		return stats, fmt.Errorf("parse inverter time %q: %w", measurements.Device.DateTime, err)
	}

	history, err := getHistory(ctx, kostalHost)
	if err != nil {
		return stats, err
	}

	timestampOffset := clockCorrection(deviceTime, time.Now().UTC(), time.Duration(history.DayCurves.IncrementStep)*time.Minute)
	if offsetOverride != nil {
		timestampOffset = *offsetOverride
	}
	stats.offset = timestampOffset

	tenMinute, err := buildHistoryPoints(history, deviceTime, timestampOffset)
	if err != nil {
		return stats, err
	}
	missing, err := missingTenMinutePoints(ctx, vmc, device, tenMinute)
	if err != nil {
		return stats, err
	}
	stats.tenMinute = len(missing)

	dayPoints, dayStats, err := buildMissingDayPoints(ctx, vmc, device, history, deviceTime, timestampOffset)
	if err != nil {
		return stats, err
	}
	stats.daily, stats.monthly = dayStats.daily, dayStats.monthly

	if err := postHistoryPoints(ctx, vmc, device, append(missing, dayPoints...)); err != nil {
		return stats, err
	}
	return stats, nil
}

func getHistory(ctx context.Context, kostalHost string) (*historyResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+kostalHost+"/yields.json?total=0", nil)
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

// clockCorrection maps inverter timestamps onto real UTC. The inverter has no
// NTP: its clock sits at an arbitrary offset and free-runs from there, so the
// correction is measured at every import and rounded to the curve's own bucket
// size to keep backfilled samples on clean boundaries.
func clockCorrection(deviceTime, now time.Time, step time.Duration) time.Duration {
	if step <= 0 {
		step = 10 * time.Minute
	}
	return now.Sub(deviceTime).Round(step)
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

	points := make([]historyPoint, 0, len(timestamps)*len(historyMetricNames))
	for _, timestamp := range timestamps {
		sample := samples[timestamp]
		if !sample.present[0] || !sample.present[1] || !sample.present[2] {
			return nil, fmt.Errorf("incomplete history sample at %d", timestamp)
		}
		points = append(points, powerPoints(sample.values, timestamp, source10mAvg)...)
	}
	return points, nil
}

// powerPoints renders one produced/consumed/injected triple, plus the derived
// self-consumption, as samples of the four dashboard metrics.
func powerPoints(values [3]float64, timestamp int64, source string) []historyPoint {
	points := make([]historyPoint, 0, len(historyMetricNames))
	for i, metric := range historyInputMetrics {
		points = append(points, historyPoint{metric: metric.metricName, source: source, value: values[i], timestamp: timestamp})
	}
	return append(points, historyPoint{
		metric:    "kostal_own_consumed_watts",
		source:    source,
		value:     math.Max(values[0]-values[2], 0),
		timestamp: timestamp,
	})
}

// missingTenMinutePoints drops the buckets already covered by real-time samples.
func missingTenMinutePoints(ctx context.Context, vmc *vmClient, device string, points []historyPoint) ([]historyPoint, error) {
	byMetric := make(map[string][]historyPoint)
	for _, point := range points {
		byMetric[point.metric] = append(byMetric[point.metric], point)
	}
	missing := make([]historyPoint, 0, len(points))
	for metric, metricPoints := range byMetric {
		existing, err := vmc.existingBuckets(ctx, metric, device, metricPoints[0].timestamp, metricPoints[len(metricPoints)-1].timestamp)
		if err != nil {
			return nil, fmt.Errorf("find existing %s buckets: %w", metric, err)
		}
		for _, point := range metricPoints {
			if !existing[point.timestamp] {
				missing = append(missing, point)
			}
		}
	}
	return missing, nil
}

// dayEnergy is one calendar day of inverter yields, in Wh.
type dayEnergy struct {
	wh     [3]float64
	source string
}

// buildMissingDayPoints expands the low-resolution curves into constant average
// power for every calendar day VictoriaMetrics has no sample for. One sample per
// hour is emitted so that the dashboards' `sum_over_time(avg_over_time(x[1h])[1d:1h])`
// integration reproduces the inverter's own daily energy total exactly.
func buildMissingDayPoints(ctx context.Context, vmc *vmClient, device string, history *historyResponse, deviceTime time.Time, timestampOffset time.Duration) ([]historyPoint, importStats, error) {
	var stats importStats

	candidates, err := buildDayEnergyCandidates(history, deviceTime)
	if err != nil {
		return nil, stats, err
	}
	if len(candidates) == 0 {
		return nil, stats, nil
	}

	days := make([]time.Time, 0, len(candidates))
	for day := range candidates {
		days = append(days, day)
	}
	sort.Slice(days, func(i, j int) bool { return days[i].Before(days[j]) })

	first := days[0].Add(timestampOffset)
	last := days[len(days)-1].Add(timestampOffset)
	covered, err := vmc.coveredDays(ctx, device, first, last)
	if err != nil {
		return nil, stats, fmt.Errorf("find covered days: %w", err)
	}

	points := make([]historyPoint, 0, len(days)*hoursPerDay*len(historyMetricNames))
	for _, day := range days {
		start := day.Add(timestampOffset)
		if covered[start.UnixMilli()] {
			continue
		}
		energy := candidates[day]
		var avgPower [3]float64
		for i, wh := range energy.wh {
			avgPower[i] = wh / hoursPerDay
		}
		for hour := 0; hour < hoursPerDay; hour++ {
			timestamp := start.Add(time.Duration(hour) * time.Hour).UnixMilli()
			points = append(points, powerPoints(avgPower, timestamp, energy.source)...)
		}
		if energy.source == sourceDailyAvg {
			stats.daily += hoursPerDay * len(historyMetricNames)
		} else {
			stats.monthly += hoursPerDay * len(historyMetricNames)
		}
	}
	return points, stats, nil
}

// buildDayEnergyCandidates maps each past calendar day to its best available
// energy total: the inverter's own daily figure when it still has it, otherwise
// the month's average day. Days with no yield at all are skipped.
func buildDayEnergyCandidates(history *historyResponse, deviceTime time.Time) (map[time.Time]dayEnergy, error) {
	today := deviceTime.Truncate(24 * time.Hour)
	candidates := make(map[time.Time]dayEnergy)

	// Lowest fidelity first: later tiers overwrite it.
	if err := eachEnergyValue(history.YearCurves, "Y", "2006", func(metricIndex int, year time.Time, monthIndex int, wh float64) {
		if monthIndex >= 12 {
			return
		}
		month := year.AddDate(0, monthIndex, 0)
		if month.AddDate(0, 1, 0).After(today) {
			return // current or future month: the total is still incomplete
		}
		days := daysInMonth(month)
		for day := 0; day < days; day++ {
			addDayEnergy(candidates, month.AddDate(0, 0, day), metricIndex, wh/float64(days), sourceMonthlyAvg)
		}
	}); err != nil {
		return nil, err
	}

	if err := eachEnergyValue(history.MonthCurves, "M", "2006-01", func(metricIndex int, month time.Time, dayIndex int, wh float64) {
		if dayIndex >= daysInMonth(month) {
			return // trailing slots of a short month
		}
		day := month.AddDate(0, 0, dayIndex)
		if !day.Before(today) {
			return // today is still accumulating
		}
		addDayEnergy(candidates, day, metricIndex, wh, sourceDailyAvg)
	}); err != nil {
		return nil, err
	}

	// A backfilled day of pure zeros carries no information; leave the gap visible.
	for day, energy := range candidates {
		if energy.wh[0] == 0 && energy.wh[1] == 0 && energy.wh[2] == 0 {
			delete(candidates, day)
		}
	}
	// The 10-minute tier already owns every day the inverter still details.
	for _, dataset := range history.DayCurves.Datasets {
		for _, period := range dataset.Data {
			if day, err := time.ParseInLocation("2006-01-02", period.Timestamp, time.UTC); err == nil {
				delete(candidates, day)
			}
		}
	}
	return candidates, nil
}

// eachEnergyValue walks the Produced/Consumed/Injected datasets of a Wh curve,
// calling visit with the dataset index, the period start, the slot index within
// the period, and the slot's Wh total.
func eachEnergyValue(curves energyCurves, incrementUnit, layout string, visit func(metricIndex int, period time.Time, slot int, wh float64)) error {
	if len(curves.Datasets) == 0 {
		return nil // firmware did not return this curve
	}
	if curves.Unit != "Wh" || curves.IncrementUnit != incrementUnit || curves.IncrementStep != 1 {
		return fmt.Errorf("unsupported %s curves: unit=%q increment=%d%s", incrementUnit, curves.Unit, curves.IncrementStep, curves.IncrementUnit)
	}
	datasets := make(map[string]energyDataset, len(curves.Datasets))
	for _, dataset := range curves.Datasets {
		datasets[dataset.Type] = dataset
	}
	for metricIndex, metric := range historyInputMetrics {
		dataset, ok := datasets[metric.typeName]
		if !ok {
			return fmt.Errorf("%s curves dataset %q is missing", incrementUnit, metric.typeName)
		}
		for _, period := range dataset.Data {
			start, err := time.ParseInLocation(layout, period.Timestamp, time.UTC)
			if err != nil {
				return fmt.Errorf("parse %s period %q: %w", metric.typeName, period.Timestamp, err)
			}
			for slot, wh := range period.Data {
				if wh < 0 {
					return fmt.Errorf("negative %s yield at %s slot %d", metric.typeName, period.Timestamp, slot)
				}
				visit(metricIndex, start, slot, wh)
			}
		}
	}
	return nil
}

func addDayEnergy(candidates map[time.Time]dayEnergy, day time.Time, metricIndex int, wh float64, source string) {
	energy := candidates[day]
	if energy.source != source {
		energy = dayEnergy{source: source}
	}
	energy.wh[metricIndex] = wh
	candidates[day] = energy
}

func daysInMonth(month time.Time) int {
	return time.Date(month.Year(), month.Month()+1, 0, 0, 0, 0, 0, month.Location()).Day()
}

// postHistoryPoints writes the samples in bounded batches.
func postHistoryPoints(ctx context.Context, vmc *vmClient, device string, points []historyPoint) error {
	var payload bytes.Buffer
	lines := 0
	flush := func() error {
		if lines == 0 {
			return nil
		}
		if err := vmc.PostMetrics(ctx, payload.Bytes()); err != nil {
			return err
		}
		payload.Reset()
		lines = 0
		return nil
	}
	for _, point := range points {
		appendHistoryMetric(&payload, point, device)
		if lines++; lines >= maxImportBatchLines {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	return flush()
}

// existingBuckets reports which 10-minute buckets already hold a sample from any
// tier. Backfilled buckets count as taken: re-importing them would be pointless
// when the clock correction is unchanged, and actively harmful when it is not,
// since the same reading would land twice a correction apart. Re-importing a
// tier therefore means deleting its series first.
func (c *vmClient) existingBuckets(ctx context.Context, metric, device string, firstTimestamp, lastTimestamp int64) (map[int64]bool, error) {
	const halfBucket = 5 * time.Minute
	query := fmt.Sprintf("count_over_time(%s{device=%q}[10m])", metric, device)
	series, err := c.queryRange(ctx, query, firstTimestamp+halfBucket.Milliseconds(), lastTimestamp+halfBucket.Milliseconds(), "10m")
	if err != nil {
		return nil, err
	}
	return nonEmptyBuckets(series, halfBucket.Milliseconds()), nil
}

// coveredDays reports which calendar days already hold at least one sample of
// any dashboard metric, real-time or backfilled.
func (c *vmClient) coveredDays(ctx context.Context, device string, first, last time.Time) (map[int64]bool, error) {
	const day = 24 * time.Hour
	query := fmt.Sprintf("count_over_time({__name__=~%q,device=%q}[1d])", "("+joinMetricNames(historyMetricNames)+")", device)
	series, err := c.queryRange(ctx, query, first.Add(day).UnixMilli(), last.Add(day).UnixMilli(), "1d")
	if err != nil {
		return nil, err
	}
	return nonEmptyBuckets(series, day.Milliseconds()), nil
}

func joinMetricNames(names []string) string {
	joined := ""
	for i, name := range names {
		if i > 0 {
			joined += "|"
		}
		joined += name
	}
	return joined
}

type rangeSeries struct {
	Values [][]json.RawMessage `json:"values"`
}

func (c *vmClient) queryRange(ctx context.Context, query string, startMillis, endMillis int64, step string) ([]rangeSeries, error) {
	endpoint, err := url.Parse(c.baseURL + "/api/v1/query_range")
	if err != nil {
		return nil, err
	}
	params := endpoint.Query()
	params.Set("query", query)
	params.Set("start", strconv.FormatInt(startMillis, 10))
	params.Set("end", strconv.FormatInt(endMillis, 10))
	params.Set("step", step)
	endpoint.RawQuery = params.Encode()

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
			Result []rangeSeries `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}
	if result.Status != "success" {
		return nil, fmt.Errorf("query failed: %s", result.Error)
	}
	return result.Data.Result, nil
}

// nonEmptyBuckets maps every count_over_time sample above zero back to the start
// of the window it summarises.
func nonEmptyBuckets(series []rangeSeries, windowMillis int64) map[int64]bool {
	existing := make(map[int64]bool)
	for _, s := range series {
		for _, value := range s.Values {
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
				existing[int64(math.Round(seconds*1000))-windowMillis] = true
			}
		}
	}
	return existing
}

func appendHistoryMetric(b *bytes.Buffer, point historyPoint, device string) {
	fmt.Fprintf(b, "%s{device=%q,source=%q} %s %d\n", point.metric, device, point.source, strconv.FormatFloat(point.value, 'g', -1, 64), point.timestamp)
}
