# kostal2influx

Extract metrics from a Kostal PV inverter and write them directly to VictoriaMetrics or InfluxDB v2.

Repositories: [GitHub](https://github.com/msf/kostal2influx) · [Codeberg mirror](https://codeberg.org/mfilipe/kostal2influx)

## VictoriaMetrics

Set `VM_HOST` to write directly to VictoriaMetrics. `VM_PORT` defaults to `8428`, and `VM_TOKEN` optionally sets a bearer token. InfluxDB writes are disabled by default; set `INFLUX_ENABLED=true` to use InfluxDB alone or alongside VictoriaMetrics.

## Container

The latest published container image is [`ghcr.io/msf/kostal2influx:v0.4`](https://github.com/users/msf/packages/container/package/kostal2influx); `ghcr.io/msf/kostal2influx:latest` currently points to the same image.

```sh
docker pull ghcr.io/msf/kostal2influx:v0.4
```

## How it gets data from Kostal Inverter PIKO 4.6-2 MP plus

My inverter has an old firmware, so it doesn't have the `http://hostname/api/dxs.json`
So I couldn't use work like: [kostal-dataexporter](https://github.com/svijee/kostal-dataexporter)

So, looking at the source code of the page: `http://hostname/pages/livechart.html`
there's a XML endpoint at `http://hostname/measurements.xml` which is used to read the data.

## InfluxDB v2 data push and dashboards.

I've created a ![dashboard](dashboard-influx2.png) to monitor my system.
It uses raw data, but it also uses  synthetic metrics to at a glance read total power generation and consumption (see `kostal_inverter_msf`). Here's the [json config of the dashboard](dashboard-influx2.json)


