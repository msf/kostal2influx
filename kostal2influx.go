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
	logger.Log("kostalHost", kostalHost, "influxHost", influxHost, "sleepSecs", sleepSecs)

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

		var power kostalPower
		p := influxdb2.NewPointWithMeasurement("kostal_inverter_0").
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
			p := influxdb2.NewPointWithMeasurement("kostal_inverter_msf").
				AddTag("DeviceName", stats.Device.Name).
				SetTime(now).
				AddField("TotalPower_W", power.Total()).
				AddField("OwnConsumed_W", power.ownConsumed).
				AddField("GridConsumed_W", power.gridConsumed).
				AddField("GridInjected_W", power.gridInjected)
			writeAPI.WritePoint(p)
		}

		writeAPI.Flush()
	}
}
