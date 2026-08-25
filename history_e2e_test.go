//go:build e2e

// End-to-end backfill test against a real VictoriaMetrics, seeded with
// live-like data. An empty TSDB only exercises the first-import path and hides
// every interesting failure: query result alignment, per-series sample limits
// and re-import behaviour all need data that is already there.
//
// The container runs with -search.maxSamplesPerSeries lowered by ~500x and the
// seed is thinned by the same factor, so the production ratio between "one
// coverage probe" and "the limit" is reproduced in seconds instead of minutes.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const (
	vmImage = "victoriametrics/victoria-metrics:v1.133.0"
	// Production: 5s live cadence, 30M samples per series. Here: 30-minute
	// cadence over the same 5.5 years, and a limit scaled to match.
	maxSamplesPerSeries = 60000

	e2eDevice = "PIKO 4.6-2 MP plus"
	// The real inverter is 3928s behind UTC, which rounds onto its own
	// 10-minute grid as 70 minutes.
	e2eClockDrift    = 3928 * time.Second
	e2eCorrection    = 70 * time.Minute
	deepHistoryDays  = 2000
	deepCadence      = 30 * time.Minute
	liveCadence      = 2 * time.Minute
	liveWindowDays   = 20
	dayCurveDays     = 31
	monthCurveMonths = 13
)

// Synthetic yields. Every tier carries a distinct signature so a sample's
// provenance can be read off its value.
const (
	curveProducedW = 3000.0 // during production slots of DayCurves
	curveConsumedW = 100.0
	curveInjectedW = 1200.0

	monthDayProducedWh = 12000.0 // MonthCurves: per day
	monthDayConsumedWh = 500.0
	monthDayInjectedWh = 4000.0

	yearMonthProducedWh = 600000.0 // YearCurves: per month
	yearMonthConsumedWh = 30000.0
	yearMonthInjectedWh = 150000.0

	productionFirstSlot = 36 // 06:00 in 10-minute slots
	productionLastSlot  = 108
)

// gaps in the seeded live data, as day offsets before today. They mirror the
// real database: a monthly-tier hole two years back, a daily-tier hole five
// months back, and a recent hole spanning both the 10-minute and daily tiers.
type dayGap struct{ fromDaysAgo, toDaysAgo int } // [today-from, today-to)

var seedGaps = []dayGap{
	{700, 591}, // 109 days, monthly tier
	{150, 127}, // 23 days, daily tier
	{45, 20},   // 25 days, 10-minute tier (31..20) + daily tier (45..31)
}

func (g dayGap) contains(today, day time.Time) bool {
	return !day.Before(today.AddDate(0, 0, -g.fromDaysAgo)) && day.Before(today.AddDate(0, 0, -g.toDaysAgo))
}

func seedCovers(today, day time.Time) bool {
	if day.Before(today.AddDate(0, 0, -deepHistoryDays)) || !day.Before(today) {
		return false
	}
	for _, gap := range seedGaps {
		if gap.contains(today, day) {
			return false
		}
	}
	return true
}

func TestBackfillAgainstSeededVictoriaMetrics(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}
	ctx := t.Context()
	now := time.Now().UTC()
	today := now.Truncate(24 * time.Hour)

	base := startVictoriaMetrics(t)
	vmc := newVMClient(vmConfig{host: base.host, port: base.port, timeout: 2 * time.Minute, retries: 2},
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	seeded := seedLiveSamples(ctx, t, vmc, today, now)
	forceFlush(t, base.url)
	t.Logf("seeded %d samples per series", seeded)

	// Negative control for the per-series sample limit: the whole range in one
	// probe is exactly what used to break against the live database.
	_, err := vmc.queryRange(ctx,
		fmt.Sprintf("count_over_time(%s{device=%q}[1d])", coverageMetric, e2eDevice),
		today.AddDate(0, 0, -deepHistoryDays).UnixMilli(), today.UnixMilli(), "1d")
	require.ErrorContains(t, err, "samples per series", "unchunked probe must still hit the limit, otherwise the chunking assertions are vacuous")

	inverter := startFakeInverter(t, now)

	// Run 1: fresh import.
	first, err := importInverterHistory(ctx, inverter, nil, vmc)
	require.NoError(t, err)
	require.Equal(t, e2eCorrection, first.offset, "clock correction is measured, not pinned")
	require.Positive(t, first.tenMinute, "10-minute tier")
	require.Positive(t, first.daily, "daily tier")
	require.Positive(t, first.monthly, "monthly tier")
	forceFlush(t, base.url)
	t.Logf("run 1: 10m=%d daily=%d monthly=%d offset=%s", first.tenMinute, first.daily, first.monthly, first.offset)

	assertNoTierOverlap(ctx, t, vmc, today)
	monthlyDay := today.AddDate(0, 0, -650)
	assertDayEnergy(ctx, t, vmc, today, -650, sourceMonthlyAvg, yearMonthProducedWh/float64(daysInMonth(monthlyDay)))
	assertDayEnergy(ctx, t, vmc, today, -140, sourceDailyAvg, monthDayProducedWh)
	assertZeroDayLeftEmpty(ctx, t, vmc, today)
	assertTenMinuteTierFilled(ctx, t, vmc, today)

	// Run 2: nothing changed, nothing to write.
	second, err := importInverterHistory(ctx, inverter, nil, vmc)
	require.NoError(t, err)
	require.Zero(t, second.total(), "re-import must be idempotent")
	forceFlush(t, base.url)

	// Run 3: the clock got fixed. Only buckets that moved past the covered
	// range may be written; the rest must still be recognised as covered.
	shifted := e2eCorrection + 30*time.Minute
	third, err := importInverterHistory(ctx, inverter, &shifted, vmc)
	require.NoError(t, err)
	t.Logf("run 3 (correction %s): %d samples", shifted, third.total())
	require.Less(t, third.total(), first.total()/50,
		"a changed clock correction must not rewrite the history")
}

// assertNoTierOverlap checks the invariant that makes `sum without (source)`
// safe: no calendar day carries samples from more than one backfill tier, and
// no backfilled day tier lands on a day that already had live samples.
func assertNoTierOverlap(ctx context.Context, t *testing.T, vmc *vmClient, today time.Time) {
	t.Helper()
	perDay := map[time.Time][]string{}
	for _, source := range []string{sourceDailyAvg, sourceMonthlyAvg} {
		for _, day := range daysWithSamples(ctx, t, vmc, fmt.Sprintf("%s{device=%q,source=%q}", coverageMetric, e2eDevice, source), today) {
			perDay[day] = append(perDay[day], source)
		}
	}
	for _, day := range daysWithSamples(ctx, t, vmc, fmt.Sprintf("%s{device=%q,source=\"\"}", coverageMetric, e2eDevice), today) {
		perDay[day] = append(perDay[day], "live")
	}
	for day, sources := range perDay {
		require.Len(t, sources, 1, "day %s carries %v — energy would be double counted", day.Format(time.DateOnly), sources)
	}
}

// daysWithSamples returns the day buckets holding at least one sample of the
// selector, keyed by the backfill's own day boundary (midnight + correction).
func daysWithSamples(ctx context.Context, t *testing.T, vmc *vmClient, selector string, today time.Time) []time.Time {
	t.Helper()
	const day = 24 * time.Hour
	first := today.AddDate(0, 0, -deepHistoryDays).Add(e2eCorrection)
	var days []time.Time
	for start := first; start.Before(today); start = start.Add(maxCoverageWindow) {
		end := start.Add(maxCoverageWindow - day)
		if end.After(today) {
			end = today
		}
		series, err := vmc.queryRange(ctx, fmt.Sprintf("count_over_time(%s[1d])", selector),
			start.Add(day-probeSkew).UnixMilli(), end.Add(day-probeSkew).UnixMilli(), "1d")
		require.NoError(t, err)
		for millis := range nonEmptyBuckets(series, day.Milliseconds()) {
			days = append(days, time.UnixMilli(millis+probeSkew.Milliseconds()).UTC())
		}
	}
	return days
}

// assertDayEnergy replays the dashboards' own integration over one backfilled
// day and requires it to reproduce the inverter's Wh figure exactly.
func assertDayEnergy(ctx context.Context, t *testing.T, vmc *vmClient, today time.Time, daysAgo int, source string, expectedWh float64) {
	t.Helper()
	start := today.AddDate(0, 0, daysAgo).Add(e2eCorrection)
	query := fmt.Sprintf(`sum_over_time(avg_over_time(kostal_AC_Power_W{device=%q}[1h])[1d:1h])`, e2eDevice)
	got := instantQuery(ctx, t, vmc, query, start.Add(24*time.Hour-probeSkew))
	require.InDelta(t, expectedWh, got, 1, "integrated energy for %s (%s)", start.Format(time.DateOnly), source)

	sourceQuery := fmt.Sprintf(`count_over_time(kostal_AC_Power_W{device=%q,source=%q}[1d])`, e2eDevice, source)
	require.EqualValues(t, hoursPerDay, instantQuery(ctx, t, vmc, sourceQuery, start.Add(24*time.Hour-probeSkew)),
		"one sample per hour of %s", start.Format(time.DateOnly))
}

func assertZeroDayLeftEmpty(ctx context.Context, t *testing.T, vmc *vmClient, today time.Time) {
	t.Helper()
	start := zeroYieldDay(today).Add(e2eCorrection)
	query := fmt.Sprintf(`count_over_time(kostal_AC_Power_W{device=%q}[1d])`, e2eDevice)
	require.Zero(t, instantQuery(ctx, t, vmc, query, start.Add(24*time.Hour-probeSkew)),
		"a zero-yield day must stay a visible gap, not a flat zero bar")
}

// assertTenMinuteTierFilled checks the recent hole was recovered at full
// fidelity: a whole day inside it must hold one sample per 10-minute bucket.
func assertTenMinuteTierFilled(ctx context.Context, t *testing.T, vmc *vmClient, today time.Time) {
	t.Helper()
	start := today.AddDate(0, 0, -25).Add(e2eCorrection)
	query := fmt.Sprintf(`count_over_time(kostal_AC_Power_W{device=%q,source=%q}[1d])`, e2eDevice, source10mAvg)
	require.EqualValues(t, 144, instantQuery(ctx, t, vmc, query, start.Add(24*time.Hour-probeSkew)))
}

func instantQuery(ctx context.Context, t *testing.T, vmc *vmClient, query string, at time.Time) float64 {
	t.Helper()
	endpoint, err := url.Parse(vmc.baseURL + "/api/v1/query")
	require.NoError(t, err)
	endpoint.RawQuery = url.Values{
		"query":   {query},
		"time":    {strconv.FormatInt(at.UnixMilli(), 10)},
		"nocache": {"1"},
	}.Encode()
	resp, err := vmc.http.Get(endpoint.String())
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, "%s", body)

	var result struct {
		Data struct {
			Result []struct {
				Value []json.RawMessage `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(body, &result))
	if len(result.Data.Result) == 0 {
		return 0
	}
	require.Len(t, result.Data.Result, 1, "query returned %d series, expected one: %s", len(result.Data.Result), query)
	var raw string
	require.NoError(t, json.Unmarshal(result.Data.Result[0].Value[1], &raw))
	value, err := strconv.ParseFloat(raw, 64)
	require.NoError(t, err)
	return value
}

// --- seeding -----------------------------------------------------------------

// seedLiveSamples writes a live-like series: 5.5 years of coarse samples with
// the documented gaps, plus a dense recent window.
func seedLiveSamples(ctx context.Context, t *testing.T, vmc *vmClient, today, now time.Time) int {
	t.Helper()
	var payload bytes.Buffer
	lines, perSeries := 0, 0
	flush := func() {
		if lines == 0 {
			return
		}
		require.NoError(t, vmc.PostMetrics(ctx, payload.Bytes()))
		payload.Reset()
		lines = 0
	}
	write := func(at time.Time) {
		millis := at.UnixMilli()
		perSeries++
		for _, metric := range historyMetricNames {
			fmt.Fprintf(&payload, "%s{device=%q} %v %d\n", metric, e2eDevice, livePower(at), millis)
			if lines++; lines >= maxImportBatchLines {
				flush()
			}
		}
	}

	liveStart := today.AddDate(0, 0, -liveWindowDays)
	for at := today.AddDate(0, 0, -deepHistoryDays); at.Before(liveStart); at = at.Add(deepCadence) {
		if seedCovers(today, at.Truncate(24*time.Hour)) {
			write(at)
		}
	}
	for at := liveStart; at.Before(now); at = at.Add(liveCadence) {
		write(at)
	}
	flush()
	return perSeries
}

// livePower is a crude diurnal shape; the exact values do not matter, only that
// live samples are distinguishable from backfilled ones.
func livePower(at time.Time) float64 {
	if hour := at.UTC().Hour(); hour >= 7 && hour < 19 {
		return 500 + float64(hour)
	}
	return 0
}

// --- fake inverter -----------------------------------------------------------

// zeroYieldDay sits inside the daily-tier gap and yields nothing in any
// dataset: the importer must leave it empty rather than write a zero bar.
func zeroYieldDay(today time.Time) time.Time { return today.AddDate(0, 0, -135) }

// startFakeInverter serves measurements.xml and yields.json the way the real
// PIKO does, on a clock that runs behind UTC. It returns the host:port.
func startFakeInverter(t *testing.T, now time.Time) string {
	t.Helper()
	deviceNow := now.Add(-e2eClockDrift)
	yields, err := json.Marshal(buildSyntheticYields(deviceNow, now.Truncate(24*time.Hour)))
	require.NoError(t, err)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/measurements.xml":
			fmt.Fprintf(w, `<?xml version='1.0' encoding='UTF-8'?><root><Device Name='%s' Type='Inverter' DateTime='%s'>`+
				`<Measurements><Measurement Value='0' Unit='W' Type='AC_Power'/></Measurements></Device></root>`,
				e2eDevice, time.Now().UTC().Add(-e2eClockDrift).Format("2006-01-02T15:04:05"))
		case "/yields.json":
			w.Header().Set("Content-Type", "application/json")
			w.Write(yields)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

// buildSyntheticYields mirrors the firmware's three curve sets. Day boundaries
// follow the inverter's own clock; realTimeToday is only used to place the
// deliberate zero-yield day where the seeded gap is.
func buildSyntheticYields(deviceNow, realTimeToday time.Time) *historyResponse {
	deviceToday := deviceNow.Truncate(24 * time.Hour)
	zeroDay := zeroYieldDay(realTimeToday).Format(time.DateOnly)

	day := historyCurves{Unit: "W", IncrementUnit: "m", IncrementStep: 10}
	for i, metric := range historyInputMetrics {
		dataset := historyDataset{Type: metric.typeName}
		for offset := dayCurveDays - 1; offset >= 0; offset-- {
			date := deviceToday.AddDate(0, 0, -offset)
			dataset.Data = append(dataset.Data, dayCurvePeriod(date.Format(time.DateOnly), i))
		}
		day.Datasets = append(day.Datasets, dataset)
	}

	firstMonth := time.Date(deviceToday.Year(), deviceToday.Month(), 1, 0, 0, 0, 0, time.UTC).
		AddDate(0, -(monthCurveMonths - 1), 0)
	month := energyCurves{Unit: "Wh", IncrementUnit: "M", IncrementStep: 1}
	for i, metric := range historyInputMetrics {
		dataset := energyDataset{Type: metric.typeName}
		for m := 0; m < monthCurveMonths; m++ {
			start := firstMonth.AddDate(0, m, 0)
			// 31 slots always: the firmware pads short months with junk.
			values := make([]float64, 31)
			for d := range values {
				values[d] = 987654 // padding, must never be read
				if d < daysInMonth(start) {
					values[d] = [3]float64{monthDayProducedWh, monthDayConsumedWh, monthDayInjectedWh}[i]
					if start.AddDate(0, 0, d).Format(time.DateOnly) == zeroDay {
						values[d] = 0
					}
				}
			}
			dataset.Data = append(dataset.Data, energyPeriod{Timestamp: start.Format("2006-01"), Data: values})
		}
		month.Datasets = append(month.Datasets, dataset)
	}

	year := energyCurves{Unit: "Wh", IncrementUnit: "Y", IncrementStep: 1}
	firstYear := deviceToday.AddDate(0, 0, -deepHistoryDays).Year()
	for i, metric := range historyInputMetrics {
		dataset := energyDataset{Type: metric.typeName}
		for y := firstYear; y <= deviceToday.Year(); y++ {
			values := make([]float64, 12)
			for m := range values {
				values[m] = [3]float64{yearMonthProducedWh, yearMonthConsumedWh, yearMonthInjectedWh}[i]
			}
			dataset.Data = append(dataset.Data, energyPeriod{Timestamp: strconv.Itoa(y), Data: values})
		}
		year.Datasets = append(year.Datasets, dataset)
	}

	return &historyResponse{DayCurves: day, MonthCurves: month, YearCurves: year}
}

// dayCurvePeriod run-length encodes one day the way the firmware does: a
// Default outside the production window, explicit values inside it.
func dayCurvePeriod(date string, metricIndex int) historyPeriod {
	values := make([]float64, productionLastSlot-productionFirstSlot)
	for i := range values {
		values[i] = [3]float64{curveProducedW, curveConsumedW, curveInjectedW}[metricIndex]
	}
	return historyPeriod{Timestamp: date, Offset: productionFirstSlot, Total: 144, Data: values}
}

// --- container ---------------------------------------------------------------

type vmEndpoint struct{ host, port, url string }

func startVictoriaMetrics(t *testing.T) vmEndpoint {
	t.Helper()
	port := freePort(t)
	args := []string{
		"run", "-d", "--rm", "-p", fmt.Sprintf("127.0.0.1:%s:8428", port), vmImage,
		"-retentionPeriod=100y",
		fmt.Sprintf("-search.maxSamplesPerSeries=%d", maxSamplesPerSeries),
		"-search.latencyOffset=0s",
	}
	out, err := exec.Command("docker", args...).CombinedOutput()
	require.NoError(t, err, "docker run: %s", out)
	id := strings.TrimSpace(string(out))
	t.Cleanup(func() {
		if t.Failed() {
			logs, _ := exec.Command("docker", "logs", "--tail", "40", id).CombinedOutput()
			t.Logf("victoriametrics logs:\n%s", logs)
		}
		exec.Command("docker", "kill", id).Run()
	})

	endpoint := vmEndpoint{host: "127.0.0.1", port: port, url: "http://127.0.0.1:" + port}
	deadline := time.Now().Add(30 * time.Second)
	for {
		resp, err := http.Get(endpoint.url + "/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return endpoint
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("victoriametrics did not become healthy: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// forceFlush makes just-written samples immediately queryable.
func forceFlush(t *testing.T, baseURL string) {
	t.Helper()
	resp, err := http.Get(baseURL + "/internal/force_flush")
	require.NoError(t, err)
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
}

func freePort(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer listener.Close()
	_, port, err := net.SplitHostPort(listener.Addr().String())
	require.NoError(t, err)
	return port
}
