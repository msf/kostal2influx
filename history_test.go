package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
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
	require.Equal(t, []historyPoint{
		{metric: "kostal_AC_Power_W", value: 100, timestamp: first},
		{metric: "kostal_grid_consumed_watts", value: 10, timestamp: first},
		{metric: "kostal_grid_injected_watts", value: 0, timestamp: first},
		{metric: "kostal_own_consumed_watts", value: 100, timestamp: first},
		{metric: "kostal_AC_Power_W", value: 200, timestamp: second},
		{metric: "kostal_grid_consumed_watts", value: 20, timestamp: second},
		{metric: "kostal_grid_injected_watts", value: 50, timestamp: second},
		{metric: "kostal_own_consumed_watts", value: 150, timestamp: second},
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
