# kostal2influx

Extract metrics from a Kostal PV inverter and write them directly to VictoriaMetrics or InfluxDB v2.

Repositories: [GitHub](https://github.com/msf/kostal2influx) · [Codeberg mirror](https://codeberg.org/mfilipe/kostal2influx)

## VictoriaMetrics

Set `VM_HOST` to write directly to VictoriaMetrics. `VM_PORT` defaults to `8428`, and `VM_TOKEN` optionally sets a bearer token. InfluxDB writes are disabled by default; set `INFLUX_ENABLED=true` to use InfluxDB alone or alongside VictoriaMetrics.

### Startup history backfill

When VictoriaMetrics is enabled, startup fetches `/yields.json?total=0` once and
backfills whatever the inverter still remembers, best resolution first. Every tier
writes the same four metrics — `kostal_AC_Power_W`, `kostal_grid_consumed_watts`,
`kostal_grid_injected_watts`, `kostal_own_consumed_watts` — in watts, and is
identified by a `source` label:

| `source` | from | covers | resolution |
| --- | --- | --- | --- |
| *(absent)* | live `measurements.xml` polling | now | 5s |
| `history_10m_avg` | `DayCurves` | rolling 31 days | 10-minute average power |
| `history_daily_avg` | `MonthCurves` | rolling 13 months | one day's energy as flat average power |
| `history_monthly_avg` | `YearCurves` | up to 20 years | one month's energy as flat average power |

The two low-resolution tiers only fill calendar days VictoriaMetrics holds *no*
sample for, and `history_10m_avg` only fills 10-minute buckets no real-time sample
landed in, so energy is never counted twice. Days with zero yield are skipped, so a
genuine outage stays a visible gap.

A day's energy is written as 24 hourly samples of constant average power (`Wh / 24`).
That is deliberately lossy in shape but exact in total: the dashboards' hourly
integration `sum_over_time(avg_over_time(x[1h])[1d:1h])` reproduces the inverter's
own daily figure to the watt-hour.

#### Inverter clock correction

These inverters have no NTP: the clock sits at an arbitrary offset from UTC and
free-runs from there (mine is 65 minutes slow). History timestamps are in that
clock, so they are useless until corrected. `HISTORY_OFFSET` defaults to `auto`,
which measures `now - measurements.xml DateTime` at every import and rounds it to
the curve's 10-minute bucket. Pin a Go duration such as `1h5m` to override.

Repeated imports require VictoriaMetrics exact-timestamp deduplication
(`-dedup.minScrapeInterval=1ms`).

#### Grafana queries

The dashboards live in [`dashboards/`](dashboards). The `source` label makes each
backfill tier its own Prometheus series, so aggregate it away to render one line:

```promql
sum without (source) (kostal_own_consumed_watts)

sum without (source) (
  sum_over_time(avg_over_time(kostal_grid_consumed_watts[1h])[$__interval:1h])
)
```

The "Sample provenance" panel on each dashboard keeps the tiers visible:

```promql
clamp_max(count_over_time(kostal_AC_Power_W{source="history_daily_avg"}[$__interval]), 1)
```

## Container

The latest published container image is [`ghcr.io/msf/kostal2influx:v0.6`](https://github.com/users/msf/packages/container/package/kostal2influx); `ghcr.io/msf/kostal2influx:latest` currently points to the same image.

```sh
docker pull ghcr.io/msf/kostal2influx:v0.6
```

## How it gets data from Kostal Inverter PIKO 4.6-2 MP plus

My inverter has an old firmware, so it doesn't have the `http://hostname/api/dxs.json`
So I couldn't use work like: [kostal-dataexporter](https://github.com/svijee/kostal-dataexporter)

So, looking at the source code of the page: `http://hostname/pages/livechart.html`
there's a XML endpoint at `http://hostname/measurements.xml` which is used to read the data.

## InfluxDB v2 data push and dashboards.

I've created a ![dashboard](dashboard-influx2.png) to monitor my system.
It uses raw data, but it also uses  synthetic metrics to at a glance read total power generation and consumption (see `kostal_inverter_msf`). Here's the [json config of the dashboard](dashboard-influx2.json)


