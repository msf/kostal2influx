package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestXmlParsing(t *testing.T) {
	s := `<?xml version='1.0' encoding='UTF-8'?><root><Device Name='PIKO 4.6-2 MP plus' Type='Inverter' Platform='Net16' HmiPlatform='HMI17' NominalPower='4600' UserPowerLimit='nan' CountryPowerLimit='nan' Serial='766360FJ007607750018' OEMSerial='10351317' BusAddress='1' NetBiosName='INV007607750018' WebPortal='PIKO Solar Portal' ManufacturerURL='kostal-solar-electric.com' IpAddress='192.168.0.11' DateTime='2021-03-07T21:09:38' MilliSeconds='404'><Measurements><Measurement Value='223.3' Unit='V' Type='AC_Voltage'/><Measurement Unit='A' Type='AC_Current'/><Measurement Unit='W' Type='AC_Power'/><Measurement Unit='W' Type='AC_Power_fast'/><Measurement Value='50.028' Unit='Hz' Type='AC_Frequency'/><Measurement Value='3.6' Unit='V' Type='DC_Voltage1'/><Measurement Value='3.2' Unit='V' Type='DC_Voltage2'/><Measurement Unit='A' Type='DC_Current1'/><Measurement Unit='A' Type='DC_Current2'/><Measurement Value='1.3' Unit='V' Type='LINK_Voltage'/><Measurement Value='-981.8' Unit='W' Type='GridPower'/><Measurement Value='981.8' Unit='W' Type='GridConsumedPower'/><Measurement Value='0.0' Unit='W' Type='GridInjectedPower'/><Measurement Value='0.0' Unit='W' Type='OwnConsumedPower'/><Measurement Value='43.0' Unit='%' Type='Derating'/></Measurements></Device></root>`
	r, err := parseMeasurementsXML([]byte(s))
	require.Nil(t, err)
	require.Equal(t, "AC_Voltage", r.Device.Measurements.Measurement[0].Type)
	require.Equal(t, 223.3, *r.Device.Measurements.Measurement[0].Value)

	require.Equal(t, "AC_Current", r.Device.Measurements.Measurement[1].Type)
	require.Nil(t, r.Device.Measurements.Measurement[1].Value)

	require.Equal(t, 15, len(r.Device.Measurements.Measurement))
}
