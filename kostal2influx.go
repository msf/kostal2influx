package main

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-kit/log"
	influxdb2 "github.com/influxdata/influxdb-client-go/v2"
	"github.com/namsral/flag"
)

const (
	rawMetricName = "kostal_inverter_raw"
	msfMetricName = "kostal_inverter_msf"
)

type Root struct {
	XMLName xml.Name `xml:"root"`
	Text    string   `xml:",chardata"`
	Device  struct {
		Name              string `xml:"Name,attr"`
		Type              string `xml:"Type,attr"`
		Platform          string `xml:"Platform,attr"`
		HmiPlatform       string `xml:"HmiPlatform,attr"`
		NominalPower      string `xml:"NominalPower,attr"`
		UserPowerLimit    string `xml:"UserPowerLimit,attr"`
		CountryPowerLimit string `xml:"CountryPowerLimit,attr"`
		Serial            string `xml:"Serial,attr"`
		OEMSerial         string `xml:"OEMSerial,attr"`
		BusAddress        string `xml:"BusAddress,attr"`
		NetBiosName       string `xml:"NetBiosName,attr"`
		WebPortal         string `xml:"WebPortal,attr"`
		ManufacturerURL   string `xml:"ManufacturerURL,attr"`
		IPAddress         string `xml:"IpAddress,attr"`
		DateTime          string `xml:"DateTime,attr"`
		MilliSeconds      string `xml:"MilliSeconds,attr"`
		Measurements      struct {
			Measurement []struct {
				Value float64 `xml:"Value,attr"`
				Unit  string  `xml:"Unit,attr"`
				Type  string  `xml:"Type,attr"`
			} `xml:"Measurement"`
		} `xml:"Measurements"`
	} `xml:"Device"`
}

func getMeasurements(kostalHost string) (*Root, error) {
	resp, err := http.Get("http://" + kostalHost + "/measurements.xml")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	return parseMeasurementsXML(data)
}

func parseMeasurementsXML(data []byte) (*Root, error) {
	var root Root
	err := xml.Unmarshal(data, &root)
	return &root, err
}

type kostalPower struct {
	gridConsumed float64
	gridInjected float64
	ownConsumed  float64
}

func (k kostalPower) Total() float64 {
	if k.gridConsumed > 0 {
		return k.gridConsumed + k.ownConsumed
	}
	return k.ownConsumed + k.gridInjected
}

func (k kostalPower) Error() error {
	if k.ownConsumed < 0 || k.gridInjected < 0 || k.gridConsumed < 0 {
		return fmt.Errorf("%+v invalid, power cannot be negative", k)
	}
	if (k.gridInjected == 0 && k.gridConsumed == 0) ||
		(k.gridInjected > 0 && k.gridConsumed > 0) {
		return fmt.Errorf("%+v inconsistent, either we are injecting power from the grid or consuming from the grid", k)
	}
	return nil
}

// writeToVictoriaMetrics writes metrics to VictoriaMetrics using Prometheus exposition format
func writeToVictoriaMetrics(vmHost string, deviceName string, measurements []Measurement, power kostalPower, timestamp time.Time) error {
	if vmHost == "" {
		return nil
	}

	var buf bytes.Buffer
	ts := timestamp.UnixMilli()

	// Write raw measurements
	for _, m := range measurements {
		name := sanitizeMetricName(fmt.Sprintf("kostal_%s_%s", m.Type, m.Unit))
		fmt.Fprintf(&buf, "%s{device=\"%s\"} %v %d\n", name, deviceName, m.Value, ts)
	}

	// Write calculated power metrics
	if power.Error() == nil {
		fmt.Fprintf(&buf, "kostal_total_power_watts{device=\"%s\"} %v %d\n", deviceName, power.Total(), ts)
		fmt.Fprintf(&buf, "kostal_own_consumed_watts{device=\"%s\"} %v %d\n", deviceName, power.ownConsumed, ts)
		fmt.Fprintf(&buf, "kostal_grid_consumed_watts{device=\"%s\"} %v %d\n", deviceName, power.gridConsumed, ts)
		fmt.Fprintf(&buf, "kostal_grid_injected_watts{device=\"%s\"} %v %d\n", deviceName, power.gridInjected, ts)
	}

	req, err := http.NewRequest("POST", "http://"+vmHost+":8428/api/v1/import/prometheus", &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "text/plain")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("vm returned status %d: %s", resp.StatusCode, string(body))
	}

	return nil
}

// sanitizeMetricName creates a Prometheus-compatible metric name
func sanitizeMetricName(name string) string {
	name = strings.ReplaceAll(name, " ", "_")
	name = strings.ReplaceAll(name, "/", "_")
	name = strings.ReplaceAll(name, "%", "percent")
	return name
}

// Measurement holds a single measurement from the Kostal inverter
type Measurement struct {
	Value float64 `xml:"Value,attr"`
	Unit  string  `xml:"Unit,attr"`
	Type  string  `xml:"Type,attr"`
}

func main() {
	const defaultBucket = "alfeizerao"
	const org = "casa"
	var (
		kostalHost   string
		influxHost   string
		influxToken  string
		influxBucket string
		vmHost       string
		sleepSecs    int
	)
	flag.StringVar(&kostalHost, "kostalHost", "192.168.0.11", "hostname or IP of kostal inversor")
	flag.StringVar(&influxHost, "influxHost", "hopper-tail", "hostname of influxdb v2 server")
	flag.StringVar(&influxToken, "influxToken", "", "influxdb v2 token (or use INFLUX_TOKEN env)")
	flag.StringVar(&influxBucket, "influxBucket", defaultBucket, "influxdb v2 bucket")
	flag.StringVar(&vmHost, "vmHost", "", "VictoriaMetrics host (e.g. localhost, for double-write)")
	flag.IntVar(&sleepSecs, "sleep_secs", 5, "sleep time")
	flag.Parse()

	// Environment variables take precedence over flags
	if token := os.Getenv("INFLUX_TOKEN"); token != "" {
		influxToken = token
	}
	if host := os.Getenv("INFLUX_HOST"); host != "" {
		influxHost = host
	}
	if bucket := os.Getenv("INFLUX_BUCKET"); bucket != "" {
		influxBucket = bucket
	}
	if host := os.Getenv("VM_HOST"); host != "" {
		vmHost = host
	}

	if influxToken == "" && vmHost == "" {
		fmt.Fprintf(os.Stderr, "Error: Either InfluxDB token or VictoriaMetrics host required\n")
		os.Exit(1)
	}

	logger := log.NewLogfmtLogger(log.NewSyncWriter(os.Stderr))
	logger = log.With(logger, "ts", log.DefaultTimestampUTC)
	logger.Log("kostalHost", kostalHost,
		"influxHost", influxHost,
		"influxBucket", influxBucket,
		"vmHost", vmHost,
		"sleepSecs", sleepSecs,
	)

	client := influxdb2.NewClient("http://"+influxHost+":8086", influxToken)
	defer client.Close()
	// get non-blocking write client
	writeAPI := client.WriteAPI(org, influxBucket)
	errorsCh := writeAPI.Errors()
	// Create go proc for reading and logging errors
	go func() {
		for err := range errorsCh {
			logger.Log("influxdb", "writeAPI", "error", err.Error())
		}
	}()

	for {
		time.Sleep(time.Duration(sleepSecs) * time.Second)

		now := time.Now().UTC()
		stats, err := getMeasurements(kostalHost)
		if err != nil {
			logger.Log("err", err, "method", "getMeasurements", "kostalHost", kostalHost)
			continue
		}
		logger.Log("measurement", "ok", "time", now, "device_time", stats.Device.DateTime)

		var power kostalPower
		p := influxdb2.NewPointWithMeasurement(rawMetricName).
			AddTag("DeviceName", stats.Device.Name).
			SetTime(now)
		for _, m := range stats.Device.Measurements.Measurement {
			name := fmt.Sprintf("%s_%s", m.Type, m.Unit)
			p = p.AddField(name, m.Value)

			switch m.Type {
			case "OwnConsumedPower":
				power.ownConsumed = m.Value
			case "GridConsumedPower":
				power.gridConsumed = m.Value
			case "GridInjectedPower":
				power.gridInjected = m.Value
			}
		}
		writeAPI.WritePoint(p)

		logger.Log("total", power.Total(),
			"ownConsumed", power.ownConsumed,
			"gridConsumed", power.gridConsumed,
			"gridInjected", power.gridInjected,
			"err", power.Error(),
		)
		if power.Error() == nil {
			p := influxdb2.NewPointWithMeasurement(msfMetricName).
				AddTag("DeviceName", stats.Device.Name).
				SetTime(now).
				AddField("TotalPower_W", power.Total()).
				AddField("OwnConsumed_W", power.ownConsumed).
				AddField("GridConsumed_W", power.gridConsumed).
				AddField("GridInjected_W", power.gridInjected)
			writeAPI.WritePoint(p)
		}

		// Double-write to VictoriaMetrics if configured
		if vmHost != "" {
			// Convert measurements to slice for VM
			var measurements []Measurement
			for _, m := range stats.Device.Measurements.Measurement {
				measurements = append(measurements, Measurement{
					Value: m.Value,
					Unit:  m.Unit,
					Type:  m.Type,
				})
			}
			if err := writeToVictoriaMetrics(vmHost, stats.Device.Name, measurements, power, now); err != nil {
				logger.Log("victoriametrics", "write error", "error", err.Error())
			} else {
				logger.Log("victoriametrics", "written")
			}
		}

		writeAPI.Flush()
	}
}
