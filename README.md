# amber_exporter

A Prometheus exporter for [Amber Electric](https://www.amber.com.au).
The exporter reads the Amber API and exports these data:

- The price of energy now, for each channel of the meter: general, controlled load, and feed-in.
- The forecast prices for approximately the next 38 hours.
- The energy and the cost of each past day, for usage and for solar export.

The structure of the code and the flags are the same as in `node_exporter` and `systemd_exporter`.

## Quick start

1. Create an API key in the Amber web app, on the [Developers page](https://app.amber.com.au/developers).
2. Build the exporter:

   ```sh
   make build
   ```

3. Put the API key in the environment and start the exporter:

   ```sh
   export AMBER_API_KEY=psk_...
   ./amber_exporter
   ```

4. Read the metrics at <http://localhost:10062/metrics>.

To read the API key from a file, use `--amber.api-key-file`.
The exporter does not accept the API key as a flag, because other users of the host can see flags in the process list.

The directory `examples` has a systemd unit, a Prometheus configuration, and Prometheus rules.

To build a container image, run `make docker`.
Then start the container with `docker run -e AMBER_API_KEY -p 10062:10062 amber_exporter:0.1.0`.

Note: Port 10062 was not in the Prometheus list of default port allocations on 2026-11-14. The port is not registered in that list.

## Flags

| Flag | Default | Function |
|---|---|---|
| `--web.listen-address` | `:10062` | Address for the web server. You can repeat the flag. |
| `--web.config.file` | none | File for TLS and basic authentication, in the `exporter-toolkit` format. |
| `--web.telemetry-path` | `/metrics` | Path of the metrics. |
| `--web.disable-exporter-metrics` | off | Remove the `go_*`, `process_*`, and `promhttp_*` metrics. |
| `--amber.api-key-file` | none | File that contains the API key. Without this flag, the exporter reads `AMBER_API_KEY`. |
| `--amber.api-url` | `https://api.amber.com.au/v1` | Base URL of the API. |
| `--amber.timeout` | `15s` | Timeout for one API request. |
| `--amber.site-id` | all active sites | Site to collect. You can repeat the flag. |
| `--[no-]collector.sites` | on | Sites and channels of the account. |
| `--[no-]collector.prices` | on | Price of the current interval. |
| `--[no-]collector.forecast` | on | Forecast prices. |
| `--[no-]collector.usage` | on | Energy and cost of each day. |
| `--collector.sites.cache-ttl` | `1h` | Minimum time between two API requests for the site list. |
| `--collector.prices.cache-ttl` | `1m` | Minimum time between two API requests for the current price. |
| `--collector.forecast.cache-ttl` | `5m` | Minimum time between two API requests for the forecast. |
| `--collector.usage.cache-ttl` | `30m` | Minimum time between two API requests for usage data. |
| `--collector.forecast.horizon` | `48h` | Maximum lead time of the forecast prices. The API has approximately 38 hours. |
| `--collector.usage.lookback-days` | `7` | Number of days of usage to export, from 1 to 90. |
| `--log.level`, `--log.format` | `info`, `logfmt` | Log settings. |

## API requests and the cache

The Amber API permits 50 requests in 5 minutes.
Prometheus can scrape more frequently than that limit permits.
Thus the exporter keeps the API data in a cache, and each collector has a cache TTL.
When the data in the cache is older than the TTL, the subsequent scrape gets new data from the API.

With the default flags, one site uses approximately 7 requests in 5 minutes:

| Collector | Requests |
|---|---|
| `prices` | 1 each minute |
| `forecast` | 1 each 5 minutes |
| `usage` | 1 each 30 minutes for each 7 days of `--collector.usage.lookback-days` |
| `sites` | 1 each hour |

If an API request fails, the collector exports no data for that scrape and sets `amber_scrape_collector_success` to 0.
The exporter does not export data that is older than the TTL.
After a failure, the collector waits 30 seconds before the subsequent API request.
If the API reports that no requests remain, the exporter sends no requests until the rate limit window ends.

## Sign of prices and costs

The sign of a value depends on the channel.

| Channel | Positive value | Negative value |
|---|---|---|
| `general`, `controlledLoad` | You pay for the energy. | Amber pays you to use energy. |
| `feedIn` | You earn money for your export. | You pay a penalty to export. |

A feed-in price of `11` means that you earn 11 c/kWh for your export.
A feed-in price of `-4` means that you pay 4 c/kWh to export.
The same rule applies to `amber_usage_daily_cost_cents`: for the `feedIn` channel, a positive value is income.

Note: The Amber API gives all values as money that you pay, thus its feed-in values have the opposite sign. The exporter changes the sign of the `feedIn` channel only.

The net cost of energy is the cost of the `general` and `controlledLoad` channels minus the value of the `feedIn` channel.
The API costs do not include the daily supply charge or the Amber subscription fee.

Prices are in c/kWh and costs are in cents, the units of the API. All values include GST.

## Metrics

All metrics have the label `site_id`. The tables below do not show this label.

### Sites

| Metric | Labels | Description |
|---|---|---|
| `amber_site_info` | `nmi`, `network`, `status` | A site of the account. The value is 1. |
| `amber_site_interval_length_seconds` | | Length of the billing interval. |
| `amber_site_channel_info` | `channel_id`, `channel_type`, `tariff` | A meter channel of the site. The value is 1. |

### Current price

| Metric | Labels | Description |
|---|---|---|
| `amber_price_cents_per_kwh` | `channel_type` | Price of the current interval. For `feedIn`, this is the price that you earn. |
| `amber_price_estimate` | `channel_type` | 1 if the price is an estimate, 0 if the price is final. |
| `amber_price_descriptor` | `channel_type`, `descriptor` | 1 for the current descriptor (`extremelyLow` to `spike`), 0 for the others. |
| `amber_price_spike_status` | `channel_type`, `status` | 1 for the current status (`none`, `potential`, `spike`), 0 for the others. |
| `amber_price_interval_start_timestamp_seconds` | | Start of the current interval. |
| `amber_price_interval_end_timestamp_seconds` | | End of the current interval. |
| `amber_spot_price_cents_per_kwh` | | NEM spot price of the current interval. |
| `amber_renewables_ratio` | | Part of the grid energy that comes from renewables, from 0 to 1. |
| `amber_tariff_info` | `channel_type`, `period`, `season` | Time of use period and season. The value is 1. |
| `amber_tariff_demand_window` | `channel_type` | 1 if the current interval is in the demand window. |

### Forecast

| Metric | Labels | Description |
|---|---|---|
| `amber_forecast_price_cents_per_kwh` | `channel_type`, `horizon` | Forecast price, from the AEMO forecast. |
| `amber_forecast_advanced_price_cents_per_kwh` | `channel_type`, `horizon`, `estimate` | Forecast of Amber: `predicted`, `low`, and `high`. Not available for all horizons. |
| `amber_forecast_spot_price_cents_per_kwh` | `horizon` | Forecast NEM spot price. |
| `amber_forecast_renewables_ratio` | `horizon` | Forecast part of renewables, from 0 to 1. |
| `amber_forecast_interval_start_timestamp_seconds` | `horizon` | Start of the forecast interval. |

### Usage

| Metric | Labels | Description |
|---|---|---|
| `amber_usage_daily_energy_kwh` | `channel_id`, `channel_type`, `date` | Energy that the channel metered on the date. For `feedIn`, this is the export. |
| `amber_usage_daily_cost_cents` | `channel_id`, `channel_type`, `date` | Cost of that energy. For `feedIn`, this is the income. |
| `amber_usage_daily_intervals` | `channel_id`, `channel_type`, `date`, `quality` | Number of metered intervals, `billable` or `estimated`. |
| `amber_usage_latest_interval_end_timestamp_seconds` | | End of the latest interval that the API has. |

### Exporter

| Metric | Labels | Description |
|---|---|---|
| `amber_scrape_collector_success` | `collector` | 1 if the collector succeeded in this scrape. |
| `amber_scrape_collector_duration_seconds` | `collector` | Duration of the collector in this scrape. |
| `amber_api_requests_total` | `endpoint`, `code` | Requests that the exporter sent to the API. |
| `amber_api_request_duration_seconds_total` | `endpoint` | Total time of the API requests. |
| `amber_api_ratelimit_limit` | | Requests that the API permits in one window. |
| `amber_api_ratelimit_remaining` | | Requests that remain in the window. |
| `amber_api_ratelimit_reset_timestamp_seconds` | | End of the window. |
| `amber_exporter_build_info` | `version`, `revision`, `branch`, `goversion`, `goos`, `goarch`, `tags` | The build of the exporter. The value is 1. |

## Version of the exporter

Each scrape exports `amber_exporter_build_info`, with the version and the Git revision of the build in its labels.
This series has the same `instance` and `job` labels as the other metrics of the scrape.
Thus it shows which version of the exporter made each data point.

The other metrics do not have a version label.
A version label on each series makes a new series at each upgrade, and then queries on the daily totals count a day two times.

To add the version to the result of a query, join the query with `amber_exporter_build_info`:

```promql
amber_price_cents_per_kwh
* on (instance, job) group_left (version, revision)
amber_exporter_build_info
```

To see when the version changed, make a graph of `amber_exporter_build_info`.

`make build`, `make docker`, and `promu build` set the version from the file `VERSION`.
A build with `go build` or `go install` uses the version of the Go module.

## How the exporter handles time

A Prometheus sample has the time of the scrape.
The current price agrees with this model, but forecasts and usage do not.
A forecast is about a future time, and usage data arrives after the day ends.
The exporter puts that time in a label.

### Forecast: the `horizon` label

The exporter exports each forecast as a fact that is known now.
The label `horizon` gives the lead time, from `30m` to approximately `38h` in steps of 30 minutes.

`horizon="1h30m"` is the 30-minute interval that starts 1 hour 30 minutes after the start of the current 30-minute interval.
The values of the label are a fixed set, thus the number of series does not increase.
A site with three channels has approximately 900 forecast series.
To decrease this number, set `--collector.forecast.horizon` to a smaller value.

The label has the format of a Prometheus duration.
Thus `amber_forecast_price_cents_per_kwh{horizon="2h"} offset 2h` shows the 2-hour forecast for the present time.
You can compare this result with `amber_price_cents_per_kwh` to see how accurate the forecast was.

`amber_forecast_interval_start_timestamp_seconds` gives the clock time of each horizon.
Use this metric to select forecasts by time, or to put the forecast on a time axis.

### Usage: the `date` label

The meter data arrives one day or more after the day ends, and it arrives one full day at a time.
Amber can change the data later: `estimated` intervals become `billable`.
Thus the exporter exports one total for each day, with the day in the label `date`.
The date is in NEM time (UTC+10, no daylight saving), the same as on the Amber bill.

Each day and channel is one series.
The series exists while the date is in the lookback window, and its last value is the correct value for that day.
To read the days, use `last_over_time` with a range that includes them.

To load the history that the API keeps, start the exporter one time with `--collector.usage.lookback-days=90`.
After Prometheus scrapes the exporter one time, start the exporter again with the default value.
The retention time of Prometheus sets how far back you can query the days.

## Queries

Price now, and the value of solar export now, in c/kWh:

```promql
amber_price_cents_per_kwh{channel_type="general"}
amber_price_cents_per_kwh{channel_type="feedIn"}
```

Spend and feed-in income of each day, in dollars.
Run these as instant queries. Each result has one row for each `date`.

```promql
sum by (date) (last_over_time(amber_usage_daily_cost_cents{channel_type!="feedIn"}[30d])) / 100
sum by (date) (last_over_time(amber_usage_daily_cost_cents{channel_type="feedIn"}[30d])) / 100
```

Net cost of each day, in dollars. A negative result is a day with more income than spend.

```promql
(
  sum by (date) (last_over_time(amber_usage_daily_cost_cents{channel_type!="feedIn"}[30d]))
  - sum by (date) (last_over_time(amber_usage_daily_cost_cents{channel_type="feedIn"}[30d]))
) / 100
```

Net cost of one month, in dollars:

```promql
(
  sum(last_over_time(amber_usage_daily_cost_cents{channel_type!="feedIn", date=~"2026-11-.*"}[35d]))
  - sum(last_over_time(amber_usage_daily_cost_cents{channel_type="feedIn", date=~"2026-11-.*"}[35d]))
) / 100
```

Lowest general price in the next 12 hours:

```promql
min by (site_id) (
  amber_forecast_price_cents_per_kwh{channel_type="general"}
  and on (site_id, horizon)
  (amber_forecast_interval_start_timestamp_seconds - time() < 12 * 3600)
)
```

Start time of the cheapest interval in the forecast:

```promql
amber_forecast_interval_start_timestamp_seconds
and on (site_id, horizon)
bottomk by (site_id) (1, amber_forecast_price_cents_per_kwh{channel_type="general"})
```

Best feed-in value in the forecast:

```promql
max by (site_id) (amber_forecast_price_cents_per_kwh{channel_type="feedIn"})
```

## Grafana

Note: There was no test of these steps in Grafana. The queries passed a test with the Prometheus query engine.

For a bar chart of the cost of each day:

1. Add one of the daily queries as an instant query with the format `Table`.
2. In the range of the query, use `$__range` as an alternative to `30d`.
3. Add the transformation "Convert field type" and convert the field `date` to a time.

For the cost of the month that the dashboard shows, use the selector `date=~"${__to:date:YYYY-MM}-.*"`.

For a chart of the forecast against clock time:

1. Add `amber_forecast_price_cents_per_kwh{channel_type="general"}` as an instant query with the format `Table`.
2. Add `amber_forecast_interval_start_timestamp_seconds * 1000` as a second instant query with the format `Table`.
3. Join the two queries on the field `horizon`.
4. Convert the value of the second query to a time, and use that field as the X axis.

## Development

```sh
make all     # format check, vet, tests, build
make lint    # golangci-lint
```

The tests do not use the network. They use the fixtures in `amber/testdata`, which contain no real data.
