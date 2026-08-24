# kostal2influx

Extract metrics from a Kostal PV inverter and write them directly to VictoriaMetrics or InfluxDB v2.

Repositories: [GitHub](https://github.com/msf/kostal2influx) · [Codeberg mirror](https://codeberg.org/mfilipe/kostal2influx)

## VictoriaMetrics

Set `VM_HOST` to write directly to VictoriaMetrics. `VM_PORT` defaults to `8428`, and `VM_TOKEN` optionally sets a bearer token. InfluxDB writes are disabled by default; set `INFLUX_ENABLED=true` to use InfluxDB alone or alongside VictoriaMetrics.

### Startup history backfill

When VictoriaMetrics is enabled, startup backfills the inverter's rolling 31-day history at its native 10-minute resolution:

1. Fetch `/yields.json?day=1` once.
2. Query VictoriaMetrics once per dashboard metric to find empty 10-minute buckets.
3. Submit one bulk import containing only completed, empty buckets.

Backfilled samples keep the existing metric names and add `source="history_10m_avg"`. The metrics are `kostal_AC_Power_W`, `kostal_grid_consumed_watts`, `kostal_grid_injected_watts`, and `kostal_own_consumed_watts`. Existing real-time samples have no `source` label.

Set `HISTORY_OFFSET` to a Go duration such as `1h5m` when the inverter clock differs from UTC. Repeated imports require VictoriaMetrics exact-timestamp deduplication (`-dedup.minScrapeInterval=1ms`). Lower-resolution daily, monthly, and yearly history is intentionally not imported yet.

#### Grafana query change

The current VictoriaMetrics dashboard is [`grafana-dashboard.json`](grafana-dashboard.json). The extra `source` label creates a second Prometheus series. Aggregate it away so real-time and backfilled buckets render as one series. Change every direct selector from:

```promql
kostal_own_consumed_watts
```

to:

```promql
sum without (source) (kostal_own_consumed_watts)
```

Apply the same outer aggregation to range expressions. For example:

```promql
sum without (source) (
  sum_over_time(avg_over_time(kostal_grid_consumed_watts[1h])[$__interval:1h])
)
```

The importer writes history only where the corresponding real-time 10-minute bucket is empty, so this aggregation does not double-count overlapping data.

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


