// Command backfill copies historical kostal solar data from InfluxDB v2 (via the
// v1 /query InfluxQL API, Token-header auth) into VictoriaMetrics, under the same
// metric names the live daemon writes. It walks the range one UTC day at a time,
// is resumable via a cursor file, and tolerates days with no data.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// msfRename maps the derived InfluxDB fields to the daemon's VM metric names.
var msfRename = map[string]string{
	"OwnConsumed_W":  "kostal_own_consumed_watts",
	"GridConsumed_W": "kostal_grid_consumed_watts",
	"GridInjected_W": "kostal_grid_injected_watts",
	"TotalPower_W":   "kostal_total_power_watts",
}

// sanitizeMetricName mirrors the daemon (victoriametrics.go) so raw fields map to
// identical series: e.g. AC_Power_W -> kostal_AC_Power_W, Derating_% -> kostal_Derating_percent.
var invalidMetricChars = regexp.MustCompile(`[^a-zA-Z0-9_:]`)

func sanitizeMetricName(name string) string {
	name = strings.ReplaceAll(name, "%", "percent")
	name = strings.ReplaceAll(name, ".", "_")
	name = invalidMetricChars.ReplaceAllString(name, "_")
	name = strings.TrimLeft(name, "0123456789")
	return name
}

func rawNamer(field string) (string, bool) { return sanitizeMetricName("kostal_" + field), true }
func msfNamer(field string) (string, bool) { n, ok := msfRename[field]; return n, ok }

var (
	influxAddr string
	influxDB   string
	influxTok  string
	httpc      = &http.Client{Timeout: 120 * time.Second}
)

func main() {
	var vmAddr, fromStr, toStr, cursorPath string
	var chunk int
	var dryRun bool
	flag.StringVar(&influxAddr, "influx-addr", "http://localhost:8086", "InfluxDB address")
	flag.StringVar(&influxDB, "influx-db", "alfeizerao", "InfluxDB v1 database (bucket)")
	flag.StringVar(&vmAddr, "vm-addr", "http://localhost:8428", "VictoriaMetrics address")
	flag.StringVar(&fromStr, "from", "", "start day YYYY-MM-DD inclusive (default: cursor+1, else earliest)")
	flag.StringVar(&toStr, "to", "", "end day YYYY-MM-DD exclusive (default: today UTC)")
	flag.StringVar(&cursorPath, "cursor", "", "cursor file for resume (optional)")
	flag.IntVar(&chunk, "chunk", 50000, "max lines per VM import POST")
	flag.BoolVar(&dryRun, "dry-run", false, "read+map but do not write to VM")
	flag.Parse()

	influxTok = os.Getenv("INFLUX_TOKEN")
	if influxTok == "" {
		log.Fatal("INFLUX_TOKEN env var required")
	}

	to := truncDay(time.Now().UTC())
	if toStr != "" {
		to = mustDay(toStr)
	}
	from := pickStart(fromStr, cursorPath)
	if !from.Before(to) {
		log.Fatalf("nothing to do: from %s >= to %s", from.Format("2006-01-02"), to.Format("2006-01-02"))
	}
	log.Printf("backfill %s .. %s (exclusive), vm=%s dry-run=%v", from.Format("2006-01-02"), to.Format("2006-01-02"), vmAddr, dryRun)

	b := &batcher{vmAddr: vmAddr, chunk: chunk, dry: dryRun}
	days, withData := 0, 0
	for d := from; d.Before(to); d = d.AddDate(0, 0, 1) {
		next := d.AddDate(0, 0, 1)
		n1 := must(importMeasurement(b, "kostal_inverter_raw", rawNamer, d, next))
		n2 := must(importMeasurement(b, "kostal_inverter_msf", msfNamer, d, next))
		must0(b.flush()) // persist the day before advancing the cursor
		if cursorPath != "" {
			must0(os.WriteFile(cursorPath, []byte(d.Format("2006-01-02")), 0o644))
		}
		days++
		if n1+n2 > 0 {
			withData++
			log.Printf("%s raw=%d msf=%d", d.Format("2006-01-02"), n1, n2)
		}
	}
	log.Printf("done: %d days scanned, %d with data, %d points imported", days, withData, b.total)
}

func pickStart(fromStr, cursorPath string) time.Time {
	if fromStr != "" {
		return mustDay(fromStr)
	}
	if cursorPath != "" {
		if data, err := os.ReadFile(cursorPath); err == nil {
			last := mustDay(strings.TrimSpace(string(data)))
			return last.AddDate(0, 0, 1) // resume the day after the last completed one
		}
	}
	return truncDay(queryEarliest())
}

// importMeasurement streams one UTC day of a measurement into the batcher and
// returns the number of points written. An empty day yields 0, no error.
func importMeasurement(b *batcher, measurement string, namer func(string) (string, bool), t0, t1 time.Time) (int, error) {
	q := fmt.Sprintf(`SELECT * FROM %q WHERE time >= '%s' AND time < '%s' GROUP BY *`,
		measurement, t0.Format(time.RFC3339), t1.Format(time.RFC3339))
	resp, err := influxQuery(q)
	if err != nil {
		return 0, err
	}
	cnt := 0
	for _, s := range resp.series() {
		device := s.Tags["DeviceName"]
		for _, row := range s.Values {
			if len(row) == 0 || row[0] == nil {
				continue
			}
			ts := int64(asFloat(row[0]))
			for j := 1; j < len(s.Columns) && j < len(row); j++ {
				f, ok := row[j].(float64)
				if !ok {
					continue // null or non-numeric
				}
				name, keep := namer(s.Columns[j])
				if !keep {
					continue
				}
				if err := b.add(name, device, f, ts); err != nil {
					return cnt, err
				}
				cnt++
			}
		}
	}
	return cnt, nil
}

// batcher buffers prometheus-exposition lines and flushes to VM in chunks.
type batcher struct {
	vmAddr string
	chunk  int
	dry    bool
	buf    bytes.Buffer
	lines  int
	total  int
}

func (b *batcher) add(name, device string, value float64, tsMillis int64) error {
	fmt.Fprintf(&b.buf, "%s{device=%q} %s %d\n", name, device, strconv.FormatFloat(value, 'g', -1, 64), tsMillis)
	b.lines++
	b.total++
	if b.lines >= b.chunk {
		return b.flush()
	}
	return nil
}

func (b *batcher) flush() error {
	if b.lines == 0 {
		return nil
	}
	if !b.dry {
		resp, err := httpc.Post(b.vmAddr+"/api/v1/import/prometheus", "text/plain", bytes.NewReader(b.buf.Bytes()))
		if err != nil {
			return err
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode >= 300 {
			return fmt.Errorf("vm import status %d: %s", resp.StatusCode, body)
		}
	}
	b.buf.Reset()
	b.lines = 0
	return nil
}

// --- InfluxDB v1 query (Token-header auth) ---

type influxSeries struct {
	Tags    map[string]string `json:"tags"`
	Columns []string          `json:"columns"`
	Values  [][]any           `json:"values"`
}

type influxResp struct {
	Results []struct {
		Series []influxSeries `json:"series"`
		Error  string         `json:"error"`
	} `json:"results"`
	Error string `json:"error"`
}

func (r *influxResp) series() []influxSeries {
	if len(r.Results) == 0 {
		return nil
	}
	return r.Results[0].Series
}

func influxQuery(q string) (*influxResp, error) {
	v := url.Values{"db": {influxDB}, "epoch": {"ms"}, "q": {q}}
	req, _ := http.NewRequest(http.MethodGet, influxAddr+"/query?"+v.Encode(), nil)
	req.Header.Set("Authorization", "Token "+influxTok)
	resp, err := httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("influx status %d: %s", resp.StatusCode, body)
	}
	var ir influxResp
	if err := json.Unmarshal(body, &ir); err != nil {
		return nil, err
	}
	if ir.Error != "" {
		return nil, fmt.Errorf("influx error: %s", ir.Error)
	}
	if len(ir.Results) > 0 && ir.Results[0].Error != "" {
		return nil, fmt.Errorf("influx query error: %s", ir.Results[0].Error)
	}
	return &ir, nil
}

func queryEarliest() time.Time {
	r, err := influxQuery(`SELECT first("AC_Power_W") FROM kostal_inverter_raw`)
	if err != nil {
		log.Fatalf("earliest probe: %v", err)
	}
	s := r.series()
	if len(s) == 0 || len(s[0].Values) == 0 {
		log.Fatal("no data found to determine earliest timestamp; pass --from")
	}
	return time.UnixMilli(int64(asFloat(s[0].Values[0][0]))).UTC()
}

// --- helpers ---

func asFloat(v any) float64 {
	f, _ := v.(float64)
	return f
}

func truncDay(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

func mustDay(s string) time.Time {
	t, err := time.ParseInLocation("2006-01-02", s, time.UTC)
	if err != nil {
		log.Fatalf("bad date %q (want YYYY-MM-DD): %v", s, err)
	}
	return t
}

func must(n int, err error) int {
	if err != nil {
		log.Fatal(err)
	}
	return n
}

func must0(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
