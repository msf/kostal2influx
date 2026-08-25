package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestBuildHistoryPoints(t *testing.T) {
	period := func(values ...float64) []historyPeriod {
		return []historyPeriod{{Timestamp: "2026-08-24", Total: 4, Data: values}}
	}
	history := &historyResponse{DayCurves: historyCurves{
		Unit: "W", IncrementUnit: "m", IncrementStep: 10,
		Datasets: []historyDataset{
			{Type: "Produced", Data: period(100, 200, 300, 400)},
			{Type: "Consumed", Data: period(10, 20, 30, 40)},
			{Type: "Injected", Data: period(0, 50, 100, 150)},
		},
	}}

	points, err := buildHistoryPoints(
		history,
		time.Date(2026, 8, 24, 0, 25, 0, 0, time.UTC),
		5*time.Minute,
	)
	require.NoError(t, err)
	first := time.Date(2026, 8, 24, 0, 5, 0, 0, time.UTC).UnixMilli()
	second := first + (10 * time.Minute).Milliseconds()
	src := source10mAvg
	require.Equal(t, []historyPoint{
		{metric: "kostal_AC_Power_W", source: src, value: 100, timestamp: first},
		{metric: "kostal_grid_consumed_watts", source: src, value: 10, timestamp: first},
		{metric: "kostal_grid_injected_watts", source: src, value: 0, timestamp: first},
		{metric: "kostal_own_consumed_watts", source: src, value: 100, timestamp: first},
		{metric: "kostal_AC_Power_W", source: src, value: 200, timestamp: second},
		{metric: "kostal_grid_consumed_watts", source: src, value: 20, timestamp: second},
		{metric: "kostal_grid_injected_watts", source: src, value: 50, timestamp: second},
		{metric: "kostal_own_consumed_watts", source: src, value: 150, timestamp: second},
	}, points)
}

func TestBuildHistoryPointsRejectsInvalidCompressedDay(t *testing.T) {
	history := &historyResponse{DayCurves: historyCurves{
		Unit: "W", IncrementUnit: "m", IncrementStep: 10,
		Datasets: []historyDataset{{
			Type: "Produced",
			Data: []historyPeriod{{Timestamp: "2026-08-24", Offset: 2, Total: 3, Data: []float64{1, 2}}},
		}},
	}}

	_, err := buildHistoryPoints(history, time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC), 0)
	require.ErrorContains(t, err, "invalid Produced day")
}

func TestExistingBuckets(t *testing.T) {
	first := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC).UnixMilli()
	last := first + (20 * time.Minute).Milliseconds()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/query_range", r.URL.Path)
		require.Equal(t, "Bearer tok", r.Header.Get("Authorization"))
		require.Equal(t, `count_over_time(kostal_AC_Power_W{device="dev",source=""}[10m])`, r.URL.Query().Get("query"))
		require.Equal(t, strconv.FormatInt(first+(5*time.Minute).Milliseconds(), 10), r.URL.Query().Get("start"))
		require.Equal(t, "10m", r.URL.Query().Get("step"))
		fmt.Fprintf(w, `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{},"values":[[%v,"3"],[%v,"0"]]}]}}`,
			float64(first+(5*time.Minute).Milliseconds())/1000,
			float64(last+(5*time.Minute).Milliseconds())/1000,
		)
	}))
	defer srv.Close()

	client := &vmClient{baseURL: srv.URL, token: "tok", http: srv.Client()}
	existing, err := client.existingBuckets(t.Context(), "kostal_AC_Power_W", "dev", first, last)
	require.NoError(t, err)
	require.Equal(t, map[int64]bool{first: true}, existing)
}

func energyCurvesFor(unit, incrementUnit string, periods map[string][3][]float64) energyCurves {
	curves := energyCurves{Unit: unit, IncrementUnit: incrementUnit, IncrementStep: 1}
	for i, metric := range historyInputMetrics {
		dataset := energyDataset{Type: metric.typeName}
		for timestamp, values := range periods {
			dataset.Data = append(dataset.Data, energyPeriod{Timestamp: timestamp, Data: values[i]})
		}
		curves.Datasets = append(curves.Datasets, dataset)
	}
	return curves
}

func TestBuildDayEnergyCandidates(t *testing.T) {
	// April has 30 days; the 31st slot is padding and must be ignored.
	april := [3][]float64{
		append([]float64{9000}, make([]float64, 30)...),
		append([]float64{4000}, make([]float64, 30)...),
		append([]float64{3000}, make([]float64, 30)...),
	}
	april[0][29], april[1][29], april[2][29] = 100, 200, 300
	april[0][30] = 999999 // padding slot beyond 2026-04-30

	// 2026 monthly totals: only February is usable (March/April are detailed above).
	year := [3][]float64{make([]float64, 12), make([]float64, 12), make([]float64, 12)}
	year[0][1], year[1][1], year[2][1] = 28000, 14000, 2800 // February, 28 days
	year[0][3] = 1                                          // April, must lose to the per-day figures

	history := &historyResponse{
		MonthCurves: energyCurvesFor("Wh", "M", map[string][3][]float64{"2026-04": april}),
		YearCurves:  energyCurvesFor("Wh", "Y", map[string][3][]float64{"2026": year}),
		DayCurves: historyCurves{Datasets: []historyDataset{
			{Type: "Produced", Data: []historyPeriod{{Timestamp: "2026-04-30"}}},
		}},
	}

	candidates, err := buildDayEnergyCandidates(history, time.Date(2026, 5, 2, 13, 0, 0, 0, time.UTC))
	require.NoError(t, err)

	day := func(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, time.UTC) }
	require.Equal(t, dayEnergy{wh: [3]float64{9000, 4000, 3000}, source: sourceDailyAvg}, candidates[day(2026, 4, 1)])
	require.Equal(t, dayEnergy{wh: [3]float64{1000, 500, 100}, source: sourceMonthlyAvg}, candidates[day(2026, 2, 10)])
	require.NotContains(t, candidates, day(2026, 4, 30), "day still detailed by DayCurves")
	require.NotContains(t, candidates, day(2026, 5, 1), "no data for May")
	require.NotContains(t, candidates, day(2026, 4, 2), "all-zero day carries no information")
	require.Len(t, candidates, 28+1, "February plus 2026-04-01")
}

func TestBuildMissingDayPointsReproducesInverterEnergy(t *testing.T) {
	produced := make([]float64, 31)
	produced[0], produced[1] = 24000, 48000
	consumed, injected := make([]float64, 31), make([]float64, 31)
	consumed[0], consumed[1] = 2400, 2400
	injected[0], injected[1] = 12000, 12000
	history := &historyResponse{MonthCurves: energyCurvesFor("Wh", "M", map[string][3][]float64{
		"2026-04": {produced, consumed, injected},
	})}

	covered := time.Date(2026, 4, 2, 0, 0, 0, 0, time.UTC).Add(24 * time.Hour)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Contains(t, r.URL.Query().Get("query"), `{__name__=~"(kostal_AC_Power_W|`)
		require.Equal(t, "1d", r.URL.Query().Get("step"))
		fmt.Fprintf(w, `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{},"values":[[%d,"17280"]]}]}}`,
			covered.Unix())
	}))
	defer srv.Close()
	client := &vmClient{baseURL: srv.URL, http: srv.Client()}

	points, stats, err := buildMissingDayPoints(t.Context(), client, "dev", history, time.Date(2026, 4, 20, 8, 0, 0, 0, time.UTC), 0)
	require.NoError(t, err)
	require.Zero(t, stats.monthly)
	require.Equal(t, hoursPerDay*len(historyMetricNames), stats.daily, "only 2026-04-01 is missing")

	energy := map[string]float64{}
	for _, p := range points {
		require.Equal(t, sourceDailyAvg, p.source)
		energy[p.metric] += p.value // one sample per hour, so Wh == sum of W
	}
	require.Equal(t, map[string]float64{
		"kostal_AC_Power_W":          24000,
		"kostal_grid_consumed_watts": 2400,
		"kostal_grid_injected_watts": 12000,
		"kostal_own_consumed_watts":  12000,
	}, energy)
}

func TestBuildDayEnergyCandidatesFromInverterSample(t *testing.T) {
	body, err := os.ReadFile("yields.pretty.json")
	require.NoError(t, err)
	var history historyResponse
	require.NoError(t, json.Unmarshal(body, &history))

	candidates, err := buildDayEnergyCandidates(&history, time.Date(2022, 1, 3, 14, 30, 0, 0, time.UTC))
	require.NoError(t, err)
	require.NotEmpty(t, candidates)

	var monthly, daily int
	for day, energy := range candidates {
		require.True(t, day.Before(time.Date(2022, 1, 3, 0, 0, 0, 0, time.UTC)))
		switch energy.source {
		case sourceDailyAvg:
			daily++
		case sourceMonthlyAvg:
			monthly++
		}
	}
	require.Positive(t, daily)
	// This inverter's YearCurves only reach back to 2021-03, which MonthCurves
	// still details day by day, so the monthly tier has nothing left to fill.
	require.Zero(t, monthly)

	require.Equal(t,
		dayEnergy{wh: [3]float64{11022, 1416, 699}, source: sourceDailyAvg},
		candidates[time.Date(2021, 3, 31, 0, 0, 0, 0, time.UTC)])
	require.NotContains(t, candidates, time.Date(2021, 12, 10, 0, 0, 0, 0, time.UTC),
		"still detailed by DayCurves")
}

func TestClockCorrection(t *testing.T) {
	// The real inverter runs 3928s behind UTC; snap that onto its 10-minute grid.
	device := time.Date(2026, 8, 24, 23, 2, 16, 0, time.UTC)
	now := device.Add(3928 * time.Second)
	require.Equal(t, 70*time.Minute, clockCorrection(device, now, 10*time.Minute))
	require.Equal(t, 70*time.Minute, clockCorrection(device, now, 0), "falls back to 10m")
	require.Equal(t, time.Duration(0), clockCorrection(device, device.Add(2*time.Minute), 10*time.Minute))
	require.Equal(t, -10*time.Minute, clockCorrection(device, device.Add(-8*time.Minute), 10*time.Minute), "inverter ahead of UTC")
}
