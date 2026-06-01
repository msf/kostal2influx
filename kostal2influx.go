package main

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	influxdb2 "github.com/influxdata/influxdb-client-go/v2"
	"github.com/influxdata/influxdb-client-go/v2/api"
	"github.com/influxdata/influxdb-client-go/v2/api/write"
	"github.com/namsral/flag"
)

const (
	rawMetricName = "kostal_inverter_raw"
	msfMetricName = "kostal_inverter_msf"
)

type Measurement struct {
	Value float64 `xml:"Value,attr"`
	Unit  string  `xml:"Unit,attr"`
	Type  string  `xml:"Type,attr"`
}

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
			Measurement []Measurement `xml:"Measurement"`
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

// FromMeasurements extracts the power components from the inverter readings.
func (k *kostalPower) FromMeasurements(ms []Measurement) {
	for _, m := range ms {
		switch m.Type {
		case "OwnConsumedPower":
			k.ownConsumed = m.Value
		case "GridConsumedPower":
			k.gridConsumed = m.Value
		case "GridInjectedPower":
			k.gridInjected = m.Value
		}
	}
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

// buildInfluxPoints renders the raw measurements (always) and the derived power
// point (only when the reading is consistent) as InfluxDB points.
func buildInfluxPoints(stats *Root, power kostalPower, now time.Time) []*write.Point {
	raw := influxdb2.NewPointWithMeasurement(rawMetricName).
		AddTag("DeviceName", stats.Device.Name).
		SetTime(now)
	for _, m := range stats.Device.Measurements.Measurement {
		raw.AddField(fmt.Sprintf("%s_%s", m.Type, m.Unit), m.Value)
	}
	points := []*write.Point{raw}

	if power.Error() == nil {
		points = append(points, influxdb2.NewPointWithMeasurement(msfMetricName).
			AddTag("DeviceName", stats.Device.Name).
			SetTime(now).
			AddField("TotalPower_W", power.Total()).
			AddField("OwnConsumed_W", power.ownConsumed).
			AddField("GridConsumed_W", power.gridConsumed).
			AddField("GridInjected_W", power.gridInjected))
	}
	return points
}

func main() {
	const defaultBucket = "alfeizerao"
	const org = "casa"
	var (
		kostalHost    string
		influxEnabled bool
		influxHost    string
		influxToken   string
		influxBucket  string
		vmHost        string
		vmPort        string
		vmToken       string
		vmTimeout     int
		vmRetries     int
		sleepSecs     int
	)
	flag.StringVar(&kostalHost, "kostalHost", "192.168.0.11", "hostname or IP of kostal inversor")
	flag.BoolVar(&influxEnabled, "influx", false, "enable InfluxDB writes (disabled by default; or INFLUX_ENABLED env)")
	flag.StringVar(&influxHost, "influxHost", "hopper-tail", "hostname of influxdb v2 server")
	flag.StringVar(&influxToken, "influxToken", "", "influxdb v2 token (or use INFLUX_TOKEN env)")
	flag.StringVar(&influxBucket, "influxBucket", defaultBucket, "influxdb v2 bucket")
	flag.StringVar(&vmHost, "vmHost", "", "VictoriaMetrics host for double-write (or VM_HOST env)")
	flag.StringVar(&vmPort, "vmPort", "8428", "VictoriaMetrics port")
	flag.StringVar(&vmToken, "vmToken", "", "VictoriaMetrics Bearer token (or VM_TOKEN env)")
	flag.IntVar(&vmTimeout, "vmTimeout", 10, "VictoriaMetrics HTTP timeout in seconds")
	flag.IntVar(&vmRetries, "vmRetries", 3, "VictoriaMetrics write retries")
	flag.IntVar(&sleepSecs, "sleep_secs", 5, "sleep time")
	flag.Parse()

	// Environment variables take precedence over flags.
	for env, dst := range map[string]*string{
		"INFLUX_TOKEN":  &influxToken,
		"INFLUX_HOST":   &influxHost,
		"INFLUX_BUCKET": &influxBucket,
		"VM_HOST":       &vmHost,
		"VM_PORT":       &vmPort,
		"VM_TOKEN":      &vmToken,
	} {
		if v := os.Getenv(env); v != "" {
			*dst = v
		}
	}
	if v := os.Getenv("INFLUX_ENABLED"); v == "1" || strings.EqualFold(v, "true") {
		influxEnabled = true
	}

	vmEnabled := vmHost != ""
	if influxEnabled && influxToken == "" {
		fmt.Fprintln(os.Stderr, "Error: InfluxDB enabled but no token (--influxToken or INFLUX_TOKEN)")
		os.Exit(1)
	}
	if !influxEnabled && !vmEnabled {
		fmt.Fprintln(os.Stderr, "Error: no write backend enabled (set --vmHost and/or --influx)")
		os.Exit(1)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	logger.Info("starting", "kostalHost", kostalHost, "sleepSecs", sleepSecs)
	if influxEnabled {
		logger.Info("influxdb sink ENABLED", "host", influxHost, "bucket", influxBucket)
	} else {
		logger.Info("influxdb sink disabled")
	}
	if vmEnabled {
		logger.Info("victoriametrics sink ENABLED", "host", vmHost, "port", vmPort)
	} else {
		logger.Info("victoriametrics sink disabled")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var influxClient api.WriteAPI
	if influxEnabled {
		client := influxdb2.NewClient("http://"+influxHost+":8086", influxToken)
		defer client.Close()
		influxClient = client.WriteAPI(org, influxBucket)
		go func() {
			for err := range influxClient.Errors() {
				logger.Error("influxdb write", "err", err)
			}
		}()
	}

	var vmc *vmClient
	if vmEnabled {
		vmc = newVMClient(vmConfig{
			host:    vmHost,
			port:    vmPort,
			token:   vmToken,
			timeout: time.Duration(vmTimeout) * time.Second,
			retries: vmRetries,
		}, logger)
	}

	sleep := time.Duration(sleepSecs) * time.Second
	for {
		select {
		case <-ctx.Done():
			if influxClient != nil {
				influxClient.Flush()
			}
			logger.Info("shutdown complete")
			return
		case <-time.After(sleep):
		}

		now := time.Now().UTC()
		stats, err := getMeasurements(kostalHost)
		if err != nil {
			logger.Error("getMeasurements", "err", err, "kostalHost", kostalHost)
			continue
		}

		var power kostalPower
		power.FromMeasurements(stats.Device.Measurements.Measurement)
		logger.Info("measurement",
			"device_time", stats.Device.DateTime,
			"total", power.Total(),
			"ownConsumed", power.ownConsumed,
			"gridConsumed", power.gridConsumed,
			"gridInjected", power.gridInjected,
			"err", power.Error(),
		)

		if influxClient != nil {
			for _, p := range buildInfluxPoints(stats, power, now) {
				influxClient.WritePoint(p)
			}
			influxClient.Flush()
		}

		if vmc != nil {
			if err := vmc.PostMetrics(ctx, buildVMPayload(stats, power, now)); err != nil {
				logger.Error("victoriametrics write", "err", err)
			}
		}
	}
}
