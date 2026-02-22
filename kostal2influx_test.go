package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestXmlParsing(t *testing.T) {
	s := `<?xml version='1.0' encoding='UTF-8'?><root><Device Name='PIKO 4.6-2 MP plus' Type='Inverter' Platform='Net16' HmiPlatform='HMI17' NominalPower='4600' UserPowerLimit='nan' CountryPowerLimit='nan' Serial='766360FJ007607750018' OEMSerial='10351317' BusAddress='1' NetBiosName='INV007607750018' WebPortal='PIKO Solar Portal' ManufacturerURL='kostal-solar-electric.com' IpAddress='192.168.0.11' DateTime='2021-03-07T21:09:38' MilliSeconds='404'><Measurements><Measurement Value='223.3' Unit='V' Type='AC_Voltage'/><Measurement Unit='A' Type='AC_Current'/><Measurement Unit='W' Type='AC_Power'/><Measurement Unit='W' Type='AC_Power_fast'/><Measurement Value='50.028' Unit='Hz' Type='AC_Frequency'/><Measurement Value='3.6' Unit='V' Type='DC_Voltage1'/><Measurement Value='3.2' Unit='V' Type='DC_Voltage2'/><Measurement Unit='A' Type='DC_Current1'/><Measurement Unit='A' Type='DC_Current2'/><Measurement Value='1.3' Unit='V' Type='LINK_Voltage'/><Measurement Value='-981.8' Unit='W' Type='GridPower'/><Measurement Value='981.8' Unit='W' Type='GridConsumedPower'/><Measurement Value='0.0' Unit='W' Type='GridInjectedPower'/><Measurement Value='0.0' Unit='W' Type='OwnConsumedPower'/><Measurement Value='43.0' Unit='%' Type='Derating'/></Measurements></Device></root>` // nolint:lll
	r, err := parseMeasurementsXML([]byte(s))
	require.Nil(t, err)
	require.Equal(t, "AC_Voltage", r.Device.Measurements.Measurement[0].Type)
	require.Equal(t, 223.3, r.Device.Measurements.Measurement[0].Value)

	require.Equal(t, "AC_Current", r.Device.Measurements.Measurement[1].Type)
	require.Equal(t, 0.0, r.Device.Measurements.Measurement[1].Value)

	require.Equal(t, 15, len(r.Device.Measurements.Measurement))
}

func TestSanitizeMetricName(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"AC Voltage", "AC_Voltage"},
		{"Power/W", "Power_W"},
		{"Temp %", "Temp_percent"},
		{"simple_name", "simple_name"},
		{"already_sanitized", "already_sanitized"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := sanitizeMetricName(tt.input)
			require.Equal(t, tt.expected, result)
		})
	}
}

func TestVictoriaMetricsOutputFormat(t *testing.T) {
	// This test verifies the Prometheus exposition format output
	measurements := []Measurement{
		{Value: 223.3, Unit: "V", Type: "AC_Voltage"},
		{Value: 50.0, Unit: "Hz", Type: "AC_Frequency"},
		{Value: 1000.0, Unit: "W", Type: "GridConsumedPower"},
		{Value: 500.0, Unit: "W", Type: "OwnConsumedPower"},
		{Value: 0.0, Unit: "W", Type: "GridInjectedPower"},
	}

	power := kostalPower{
		gridConsumed: 1000.0,
		gridInjected: 0.0,
		ownConsumed:  500.0,
	}

	timestamp := time.Date(2025, 1, 15, 10, 30, 0, 0, time.UTC)
	ts := timestamp.UnixMilli()

	// Use the actual VMClient to build the payload
	vmClient := NewVMClient("test-host")
	output := vmClient.buildPayload("test-inverter", measurements, power, ts)

	// Hardcoded expected output
	expected := `kostal_AC_Voltage_V{device="test-inverter"} 223.3 1736937000000
kostal_AC_Frequency_Hz{device="test-inverter"} 50 1736937000000
kostal_GridConsumedPower_W{device="test-inverter"} 1000 1736937000000
kostal_OwnConsumedPower_W{device="test-inverter"} 500 1736937000000
kostal_GridInjectedPower_W{device="test-inverter"} 0 1736937000000
kostal_total_power_watts{device="test-inverter"} 1500 1736937000000
kostal_own_consumed_watts{device="test-inverter"} 500 1736937000000
kostal_grid_consumed_watts{device="test-inverter"} 1000 1736937000000
kostal_grid_injected_watts{device="test-inverter"} 0 1736937000000
`

	require.Equal(t, expected, output)
}

func TestVictoriaMetricsPowerCalculation(t *testing.T) {
	tests := []struct {
		name           string
		gridConsumed   float64
		gridInjected   float64
		ownConsumed    float64
		expectedTotal  float64
		expectError    bool
	}{
		{
			name:          "consuming from grid",
			gridConsumed:  1000.0,
			gridInjected:  0.0,
			ownConsumed:   500.0,
			expectedTotal: 1500.0,
			expectError:   false,
		},
		{
			name:          "injecting to grid",
			gridConsumed:  0.0,
			gridInjected:  800.0,
			ownConsumed:   300.0,
			expectedTotal: 1100.0,
			expectError:   false,
		},
		{
			name:         "negative grid consumed",
			gridConsumed: -100.0,
			expectError:  true,
		},
		{
			name:         "both grid values positive",
			gridConsumed: 100.0,
			gridInjected: 100.0,
			expectError:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := kostalPower{
				gridConsumed: tt.gridConsumed,
				gridInjected: tt.gridInjected,
				ownConsumed:  tt.ownConsumed,
			}

			err := p.Error()
			if tt.expectError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.Equal(t, tt.expectedTotal, p.Total())
			}
		})
	}
}
