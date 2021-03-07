package main

import (
	"encoding/xml"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"time"

	"github.com/go-kit/kit/log"
	influxdb2 "github.com/influxdata/influxdb-client-go/v2"
	"github.com/namsral/flag"
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
		IpAddress         string `xml:"IpAddress,attr"`
		DateTime          string `xml:"DateTime,attr"`
		MilliSeconds      string `xml:"MilliSeconds,attr"`
		Measurements      struct {
			Measurement []struct {
				Value *float64 `xml:"Value,attr"`
				Unit  string   `xml:"Unit,attr"`
				Type  string   `xml:"Type,attr"`
			} `xml:"Measurement"`
		} `xml:"Measurements"`
	} `xml:"Device"`
}

func getMeasurements(kostal_host string) (*Root, error) {
	resp, err := http.Get("http://" + kostal_host + "/measurements.xml")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := ioutil.ReadAll(resp.Body)
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

func main() {
	const defaultToken = "GnK3erFQGnB3aLonK6mCiIRYTGenl4ShRGdxr7M3E6b2yzl51shxHUR7gJdTagJ094Vpf8fJzzotCWwhSxclHA=="
	// You can generate a Token from the "Tokens Tab" in the UI
	const bucket = "hopper"
	const org = "casa"
	var (
		kostalHost  string
		influxHost  string
		influxToken string
		sleepSecs   int
	)
	flag.StringVar(&kostalHost, "kostalHost", "192.168.0.11", "hostname or IP of kostal inversor")
	flag.StringVar(&influxHost, "influxHost", "hopper-tail", "hostname of influxdb v2 server")
	flag.StringVar(&influxToken, "influxToken", defaultToken, "influxdb v2 token")
	flag.IntVar(&sleepSecs, "sleep_secs", 5, "sleep time")
	flag.Parse()

	logger := log.NewLogfmtLogger(log.NewSyncWriter(os.Stderr))
	logger = log.With(logger, "ts", log.DefaultTimestampUTC)

	client := influxdb2.NewClient("http://"+influxHost+":8086", influxToken)
	defer client.Close()
	// get non-blocking write client
	writeAPI := client.WriteAPI(org, bucket)
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

		p := influxdb2.NewPointWithMeasurement("kostal_inverter_raw").
			AddTag("DeviceName", stats.Device.Name).
			AddTag("OEMSerial", stats.Device.OEMSerial).
			SetTime(now)
		for i, m := range stats.Device.Measurements.Measurement {
			if m.Value == nil {
				logger.Log("Measurement", m.Type, "error", "missing m.Value")
				continue
			}
			name := fmt.Sprintf("%s_%s", m.Type, m.Unit)
			logger.Log("Measurement", i, name, m.Value)
			p = p.AddField(name, m.Value)
		}
		writeAPI.WritePoint(p)

		// TODO: write another point with refined metrics created by me
		writeAPI.Flush()
	}
}
