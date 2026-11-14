package collector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alecthomas/kingpin/v2"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/goodtune/amber_exporter/amber"
)

const (
	activeSite = "01F5A5CRKMZ5BCX9P1S4V990AM"
	closedSite = "01E8RD8Q0GKBJ4DY1NT5Y0ZX1A"
)

// fakeAPI serves the fixtures of the amber package and records the requests.
type fakeAPI struct {
	sites    []amber.Site
	prices   []amber.Interval
	forecast []amber.Interval
	usage    []amber.Usage

	mu    sync.Mutex
	calls []string
	err   error
}

func newFakeAPI(t *testing.T) *fakeAPI {
	t.Helper()
	f := &fakeAPI{}
	load(t, "sites.json", &f.sites)
	load(t, "prices.json", &f.prices)
	load(t, "forecast.json", &f.forecast)
	load(t, "usage.json", &f.usage)
	return f
}

func load(t *testing.T, name string, out any) {
	t.Helper()
	b, err := os.ReadFile("../amber/testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, out); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
}

func (f *fakeAPI) record(call string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
	return f.err
}

func (f *fakeAPI) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

// seen returns the recorded requests that start with prefix.
func (f *fakeAPI) seen(prefix string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeAPI) Sites(context.Context) ([]amber.Site, error) {
	if err := f.record("sites"); err != nil {
		return nil, err
	}
	return f.sites, nil
}

func (f *fakeAPI) CurrentPrices(_ context.Context, siteID string, opts amber.PriceOptions) ([]amber.Interval, error) {
	if opts == (amber.PriceOptions{}) {
		if err := f.record("prices " + siteID); err != nil {
			return nil, err
		}
		return f.prices, nil
	}
	if err := f.record(fmt.Sprintf("forecast %s next=%d resolution=%d", siteID, opts.Next, opts.Resolution)); err != nil {
		return nil, err
	}
	return f.forecast, nil
}

func (f *fakeAPI) Usage(_ context.Context, siteID string, start, end time.Time) ([]amber.Usage, error) {
	call := fmt.Sprintf("usage %s %s..%s", siteID, start.Format(amber.DateFormat), end.Format(amber.DateFormat))
	if err := f.record(call); err != nil {
		return nil, err
	}
	// The fixture is returned for the request that ends today, so that a
	// lookback of more than one request does not count it twice.
	if end.Format(amber.DateFormat) != "2026-11-14" {
		return nil, nil
	}
	return f.usage, nil
}

// clock is a time source that tests move by hand.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

// 13:40 in NEM time on 2026-11-14.
func newClock() *clock {
	return &clock{t: time.Date(2026, 11, 14, 3, 40, 0, 0, time.UTC)}
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// only returns the default settings with one collector enabled.
func only(clk *clock, names ...string) Config {
	cfg := DefaultConfig()
	cfg.Now = clk.now
	for _, f := range factories {
		cfg.Enabled[f.name] = false
	}
	for _, name := range names {
		cfg.Enabled[name] = true
	}
	return cfg
}

func newRegistry(t *testing.T, api API, cfg Config) *prometheus.Registry {
	t.Helper()
	c, err := New(api, cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	reg := prometheus.NewRegistry()
	reg.MustRegister(c)
	return reg
}

func compare(t *testing.T, reg *prometheus.Registry, want string, names ...string) {
	t.Helper()
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), names...); err != nil {
		t.Error(err)
	}
}

func TestSitesCollector(t *testing.T) {
	reg := newRegistry(t, newFakeAPI(t), only(newClock(), "sites"))
	compare(t, reg, `
# HELP amber_site_channel_info A meter channel of a site. The value is always 1.
# TYPE amber_site_channel_info gauge
amber_site_channel_info{channel_id="B1",channel_type="feedIn",site_id="01F5A5CRKMZ5BCX9P1S4V990AM",tariff="EA029"} 1
amber_site_channel_info{channel_id="E1",channel_type="general",site_id="01E8RD8Q0GKBJ4DY1NT5Y0ZX1A",tariff="EA010"} 1
amber_site_channel_info{channel_id="E1",channel_type="general",site_id="01F5A5CRKMZ5BCX9P1S4V990AM",tariff="EA116"} 1
amber_site_channel_info{channel_id="E2",channel_type="controlledLoad",site_id="01F5A5CRKMZ5BCX9P1S4V990AM",tariff="EA030"} 1
# HELP amber_site_info A site linked to the account. The value is always 1.
# TYPE amber_site_info gauge
amber_site_info{network="Ausgrid",nmi="3052282871",site_id="01E8RD8Q0GKBJ4DY1NT5Y0ZX1A",status="closed"} 1
amber_site_info{network="Ausgrid",nmi="3052282872",site_id="01F5A5CRKMZ5BCX9P1S4V990AM",status="active"} 1
# HELP amber_site_interval_length_seconds Length of the billing interval of the site.
# TYPE amber_site_interval_length_seconds gauge
amber_site_interval_length_seconds{site_id="01E8RD8Q0GKBJ4DY1NT5Y0ZX1A"} 1800
amber_site_interval_length_seconds{site_id="01F5A5CRKMZ5BCX9P1S4V990AM"} 300
# HELP amber_scrape_collector_success Whether the collector succeeded in this scrape: 1 for yes, 0 for no.
# TYPE amber_scrape_collector_success gauge
amber_scrape_collector_success{collector="sites"} 1
`, "amber_site_info", "amber_site_interval_length_seconds", "amber_site_channel_info", "amber_scrape_collector_success")
}

func TestPricesCollector(t *testing.T) {
	api := newFakeAPI(t)
	reg := newRegistry(t, api, only(newClock(), "prices"))
	compare(t, reg, `
# HELP amber_price_cents_per_kwh Price of the current interval in c/kWh. Includes GST. For the general and controlledLoad channels, this is what you pay. For the feedIn channel, this is what you earn: a negative value means that you pay to export.
# TYPE amber_price_cents_per_kwh gauge
amber_price_cents_per_kwh{channel_type="controlledLoad",site_id="01F5A5CRKMZ5BCX9P1S4V990AM"} 20.08
amber_price_cents_per_kwh{channel_type="feedIn",site_id="01F5A5CRKMZ5BCX9P1S4V990AM"} 11.09
amber_price_cents_per_kwh{channel_type="general",site_id="01F5A5CRKMZ5BCX9P1S4V990AM"} 19.9
# HELP amber_price_estimate Whether the price of the current interval is an estimate (1) or final (0).
# TYPE amber_price_estimate gauge
amber_price_estimate{channel_type="controlledLoad",site_id="01F5A5CRKMZ5BCX9P1S4V990AM"} 0
amber_price_estimate{channel_type="feedIn",site_id="01F5A5CRKMZ5BCX9P1S4V990AM"} 0
amber_price_estimate{channel_type="general",site_id="01F5A5CRKMZ5BCX9P1S4V990AM"} 0
# HELP amber_price_interval_end_timestamp_seconds End of the current interval as a Unix timestamp.
# TYPE amber_price_interval_end_timestamp_seconds gauge
amber_price_interval_end_timestamp_seconds{site_id="01F5A5CRKMZ5BCX9P1S4V990AM"} 1.7946276e+09
# HELP amber_price_interval_start_timestamp_seconds Start of the current interval as a Unix timestamp.
# TYPE amber_price_interval_start_timestamp_seconds gauge
amber_price_interval_start_timestamp_seconds{site_id="01F5A5CRKMZ5BCX9P1S4V990AM"} 1.7946273e+09
# HELP amber_renewables_ratio Part of the grid's energy that comes from renewables in the current interval, from 0 to 1.
# TYPE amber_renewables_ratio gauge
amber_renewables_ratio{site_id="01F5A5CRKMZ5BCX9P1S4V990AM"} 0.52
# HELP amber_spot_price_cents_per_kwh NEM spot price of the current interval in c/kWh. Includes GST.
# TYPE amber_spot_price_cents_per_kwh gauge
amber_spot_price_cents_per_kwh{site_id="01F5A5CRKMZ5BCX9P1S4V990AM"} 12.98
# HELP amber_tariff_demand_window Whether the current interval is in the demand window: 1 for yes, 0 for no. Only for channels on a demand tariff.
# TYPE amber_tariff_demand_window gauge
amber_tariff_demand_window{channel_type="general",site_id="01F5A5CRKMZ5BCX9P1S4V990AM"} 0
# HELP amber_tariff_info Time of use period and season of the current interval. The value is always 1. Only for channels on a time of use tariff.
# TYPE amber_tariff_info gauge
amber_tariff_info{channel_type="general",period="offPeak",season="summer",site_id="01F5A5CRKMZ5BCX9P1S4V990AM"} 1
`, "amber_price_cents_per_kwh", "amber_price_estimate", "amber_price_interval_start_timestamp_seconds",
		"amber_price_interval_end_timestamp_seconds", "amber_spot_price_cents_per_kwh", "amber_renewables_ratio",
		"amber_tariff_info", "amber_tariff_demand_window")

	compare(t, reg, `
# HELP amber_price_descriptor Amber's description of the current price. The series of the current descriptor is 1, the others are 0.
# TYPE amber_price_descriptor gauge
amber_price_descriptor{channel_type="feedIn",descriptor="extremelyLow",site_id="01F5A5CRKMZ5BCX9P1S4V990AM"} 0
amber_price_descriptor{channel_type="feedIn",descriptor="high",site_id="01F5A5CRKMZ5BCX9P1S4V990AM"} 1
amber_price_descriptor{channel_type="feedIn",descriptor="low",site_id="01F5A5CRKMZ5BCX9P1S4V990AM"} 0
amber_price_descriptor{channel_type="feedIn",descriptor="neutral",site_id="01F5A5CRKMZ5BCX9P1S4V990AM"} 0
amber_price_descriptor{channel_type="feedIn",descriptor="spike",site_id="01F5A5CRKMZ5BCX9P1S4V990AM"} 0
amber_price_descriptor{channel_type="feedIn",descriptor="veryLow",site_id="01F5A5CRKMZ5BCX9P1S4V990AM"} 0
`+descriptorSeries("controlledLoad", "low")+descriptorSeries("general", "veryLow")+`
# HELP amber_price_spike_status Spike status of the current interval. The series of the current status is 1, the others are 0.
# TYPE amber_price_spike_status gauge
amber_price_spike_status{channel_type="controlledLoad",site_id="01F5A5CRKMZ5BCX9P1S4V990AM",status="none"} 1
amber_price_spike_status{channel_type="controlledLoad",site_id="01F5A5CRKMZ5BCX9P1S4V990AM",status="potential"} 0
amber_price_spike_status{channel_type="controlledLoad",site_id="01F5A5CRKMZ5BCX9P1S4V990AM",status="spike"} 0
amber_price_spike_status{channel_type="feedIn",site_id="01F5A5CRKMZ5BCX9P1S4V990AM",status="none"} 1
amber_price_spike_status{channel_type="feedIn",site_id="01F5A5CRKMZ5BCX9P1S4V990AM",status="potential"} 0
amber_price_spike_status{channel_type="feedIn",site_id="01F5A5CRKMZ5BCX9P1S4V990AM",status="spike"} 0
amber_price_spike_status{channel_type="general",site_id="01F5A5CRKMZ5BCX9P1S4V990AM",status="none"} 1
amber_price_spike_status{channel_type="general",site_id="01F5A5CRKMZ5BCX9P1S4V990AM",status="potential"} 0
amber_price_spike_status{channel_type="general",site_id="01F5A5CRKMZ5BCX9P1S4V990AM",status="spike"} 0
`, "amber_price_descriptor", "amber_price_spike_status")

	// The closed site has no prices to collect.
	if got := api.seen("prices"); len(got) != 1 || got[0] != "prices "+activeSite {
		t.Errorf("price requests = %v, want one for the active site", got)
	}
}

// descriptorSeries returns the exposition of amber_price_descriptor for one
// channel of the active site.
func descriptorSeries(channel, current string) string {
	var b strings.Builder
	for _, d := range priceDescriptors {
		v := 0
		if d == current {
			v = 1
		}
		fmt.Fprintf(&b, "amber_price_descriptor{channel_type=%q,descriptor=%q,site_id=%q} %d\n", channel, d, activeSite, v)
	}
	return b.String()
}

// A descriptor that the exporter does not know gets a series of its own.
func TestPricesCollectorUnknownDescriptor(t *testing.T) {
	api := newFakeAPI(t)
	api.prices = api.prices[:1]
	api.prices[0].Descriptor = "negative"
	reg := newRegistry(t, api, only(newClock(), "prices"))

	n, err := testutil.GatherAndCount(reg, "amber_price_descriptor")
	if err != nil {
		t.Fatal(err)
	}
	if want := len(priceDescriptors) + 1; n != want {
		t.Errorf("got %d descriptor series, want %d", n, want)
	}
}

// The API reports a feed-in price as what you pay. The exporter turns it
// around: you earn a positive value, and you pay to export at a negative one.
func TestFeedInSign(t *testing.T) {
	for _, tc := range []struct {
		api  float64
		want string
	}{
		{-11.09, "11.09"}, // Amber pays you.
		{4.5, "-4.5"},     // You pay to export.
		{0, "0"},          // Not "-0".
	} {
		api := newFakeAPI(t)
		api.prices[2].PerKwh = tc.api
		api.prices[0].PerKwh = tc.api // The general channel keeps the sign of the API.
		reg := newRegistry(t, api, only(newClock(), "prices"))

		general := strings.TrimPrefix(fmt.Sprint(tc.api), "+")
		compare(t, reg, fmt.Sprintf(`
# HELP amber_price_cents_per_kwh Price of the current interval in c/kWh.`+signHelp+`
# TYPE amber_price_cents_per_kwh gauge
amber_price_cents_per_kwh{channel_type="controlledLoad",site_id="01F5A5CRKMZ5BCX9P1S4V990AM"} 20.08
amber_price_cents_per_kwh{channel_type="feedIn",site_id="01F5A5CRKMZ5BCX9P1S4V990AM"} %s
amber_price_cents_per_kwh{channel_type="general",site_id="01F5A5CRKMZ5BCX9P1S4V990AM"} %s
`, tc.want, general), "amber_price_cents_per_kwh")
	}
}

// A feed-in cost from the API that is positive is a penalty: the exporter
// shows it as a negative value.
func TestUsageCollectorFeedInPenalty(t *testing.T) {
	api := newFakeAPI(t)
	api.usage = api.usage[2:3] // One feedIn interval on 2026-11-12.
	api.usage[0].Cost = 6
	reg := newRegistry(t, api, only(newClock(), "usage"))
	compare(t, reg, `
# HELP amber_usage_daily_cost_cents Cost of the energy metered on the channel on the date, in cents. The date is in NEM time (UTC+10).`+signHelp+`
# TYPE amber_usage_daily_cost_cents gauge
amber_usage_daily_cost_cents{channel_id="B1",channel_type="feedIn",date="2026-11-12",site_id="01F5A5CRKMZ5BCX9P1S4V990AM"} -6
`, "amber_usage_daily_cost_cents")
}

const forecastHelp = `
# HELP amber_forecast_advanced_price_cents_per_kwh Amber's own forecast price in c/kWh: the predicted price and the low and high ends of the band around it. Not available for all horizons. Includes GST. For the general and controlledLoad channels, this is what you pay. For the feedIn channel, this is what you earn: a negative value means that you pay to export.
# TYPE amber_forecast_advanced_price_cents_per_kwh gauge
# HELP amber_forecast_interval_start_timestamp_seconds Start of the forecast interval as a Unix timestamp. Use it to put forecast values on a time axis.
# TYPE amber_forecast_interval_start_timestamp_seconds gauge
# HELP amber_forecast_price_cents_per_kwh Forecast price in c/kWh, from the AEMO forecast, for the 30 minute interval that starts one horizon after the start of the current one. Includes GST. For the general and controlledLoad channels, this is what you pay. For the feedIn channel, this is what you earn: a negative value means that you pay to export.
# TYPE amber_forecast_price_cents_per_kwh gauge
# HELP amber_forecast_renewables_ratio Forecast part of the grid's energy that comes from renewables, from 0 to 1.
# TYPE amber_forecast_renewables_ratio gauge
# HELP amber_forecast_spot_price_cents_per_kwh Forecast NEM spot price in c/kWh. Includes GST.
# TYPE amber_forecast_spot_price_cents_per_kwh gauge
`

var forecastNames = []string{
	"amber_forecast_price_cents_per_kwh", "amber_forecast_advanced_price_cents_per_kwh",
	"amber_forecast_spot_price_cents_per_kwh", "amber_forecast_renewables_ratio",
	"amber_forecast_interval_start_timestamp_seconds",
}

func TestForecastCollector(t *testing.T) {
	api := newFakeAPI(t)
	reg := newRegistry(t, api, only(newClock(), "forecast"))
	compare(t, reg, forecastHelp+`
amber_forecast_advanced_price_cents_per_kwh{channel_type="controlledLoad",estimate="high",horizon="30m",site_id="01F5A5CRKMZ5BCX9P1S4V990AM"} 17
amber_forecast_advanced_price_cents_per_kwh{channel_type="controlledLoad",estimate="low",horizon="30m",site_id="01F5A5CRKMZ5BCX9P1S4V990AM"} 14
amber_forecast_advanced_price_cents_per_kwh{channel_type="controlledLoad",estimate="predicted",horizon="30m",site_id="01F5A5CRKMZ5BCX9P1S4V990AM"} 15.5
amber_forecast_advanced_price_cents_per_kwh{channel_type="feedIn",estimate="high",horizon="30m",site_id="01F5A5CRKMZ5BCX9P1S4V990AM"} 15
amber_forecast_advanced_price_cents_per_kwh{channel_type="feedIn",estimate="low",horizon="30m",site_id="01F5A5CRKMZ5BCX9P1S4V990AM"} 8
amber_forecast_advanced_price_cents_per_kwh{channel_type="feedIn",estimate="predicted",horizon="30m",site_id="01F5A5CRKMZ5BCX9P1S4V990AM"} 11
amber_forecast_advanced_price_cents_per_kwh{channel_type="general",estimate="high",horizon="30m",site_id="01F5A5CRKMZ5BCX9P1S4V990AM"} 25
amber_forecast_advanced_price_cents_per_kwh{channel_type="general",estimate="low",horizon="30m",site_id="01F5A5CRKMZ5BCX9P1S4V990AM"} 18
amber_forecast_advanced_price_cents_per_kwh{channel_type="general",estimate="predicted",horizon="30m",site_id="01F5A5CRKMZ5BCX9P1S4V990AM"} 21
amber_forecast_interval_start_timestamp_seconds{horizon="1h",site_id="01F5A5CRKMZ5BCX9P1S4V990AM"} 1.7946306e+09
amber_forecast_interval_start_timestamp_seconds{horizon="30m",site_id="01F5A5CRKMZ5BCX9P1S4V990AM"} 1.7946288e+09
amber_forecast_price_cents_per_kwh{channel_type="controlledLoad",horizon="1h",site_id="01F5A5CRKMZ5BCX9P1S4V990AM"} 30
amber_forecast_price_cents_per_kwh{channel_type="controlledLoad",horizon="30m",site_id="01F5A5CRKMZ5BCX9P1S4V990AM"} 16
amber_forecast_price_cents_per_kwh{channel_type="feedIn",horizon="1h",site_id="01F5A5CRKMZ5BCX9P1S4V990AM"} 28
amber_forecast_price_cents_per_kwh{channel_type="feedIn",horizon="30m",site_id="01F5A5CRKMZ5BCX9P1S4V990AM"} 12
amber_forecast_price_cents_per_kwh{channel_type="general",horizon="1h",site_id="01F5A5CRKMZ5BCX9P1S4V990AM"} 40
amber_forecast_price_cents_per_kwh{channel_type="general",horizon="30m",site_id="01F5A5CRKMZ5BCX9P1S4V990AM"} 22
amber_forecast_renewables_ratio{horizon="1h",site_id="01F5A5CRKMZ5BCX9P1S4V990AM"} 0.3
amber_forecast_renewables_ratio{horizon="30m",site_id="01F5A5CRKMZ5BCX9P1S4V990AM"} 0.45
amber_forecast_spot_price_cents_per_kwh{horizon="1h",site_id="01F5A5CRKMZ5BCX9P1S4V990AM"} 30
amber_forecast_spot_price_cents_per_kwh{horizon="30m",site_id="01F5A5CRKMZ5BCX9P1S4V990AM"} 14
`, forecastNames...)

	// 48 hours of 30 minute intervals.
	want := "forecast " + activeSite + " next=96 resolution=30"
	if got := api.seen("forecast"); len(got) != 1 || got[0] != want {
		t.Errorf("forecast requests = %v, want [%s]", got, want)
	}
}

// The horizon is counted from the interval that contains the present time.
// When cached data is served after the half hour has passed, the interval
// that was 30 minutes ahead is the current one and the next one moves up.
func TestForecastCollectorCountsHorizonFromTheClock(t *testing.T) {
	api := newFakeAPI(t)
	clk := newClock()
	cfg := only(clk, "forecast")
	cfg.ForecastTTL = time.Hour
	reg := newRegistry(t, api, cfg)

	if n, err := testutil.GatherAndCount(reg, "amber_forecast_price_cents_per_kwh"); err != nil || n != 6 {
		t.Fatalf("got %d price series (err %v), want 6", n, err)
	}

	clk.advance(25 * time.Minute) // 04:05: the interval from 04:00 is the current one.
	compare(t, reg, forecastHelp+`
amber_forecast_interval_start_timestamp_seconds{horizon="30m",site_id="01F5A5CRKMZ5BCX9P1S4V990AM"} 1.7946306e+09
amber_forecast_price_cents_per_kwh{channel_type="controlledLoad",horizon="30m",site_id="01F5A5CRKMZ5BCX9P1S4V990AM"} 30
amber_forecast_price_cents_per_kwh{channel_type="feedIn",horizon="30m",site_id="01F5A5CRKMZ5BCX9P1S4V990AM"} 28
amber_forecast_price_cents_per_kwh{channel_type="general",horizon="30m",site_id="01F5A5CRKMZ5BCX9P1S4V990AM"} 40
amber_forecast_renewables_ratio{horizon="30m",site_id="01F5A5CRKMZ5BCX9P1S4V990AM"} 0.3
amber_forecast_spot_price_cents_per_kwh{horizon="30m",site_id="01F5A5CRKMZ5BCX9P1S4V990AM"} 30
`, forecastNames...)
	if got := api.seen("forecast"); len(got) != 1 {
		t.Errorf("forecast requests = %v, want one: the second scrape is served from the cache", got)
	}

	// When all cached intervals are in the past, the collector fails.
	clk.advance(55 * time.Minute) // 05:00, still inside the TTL of one hour.
	compare(t, reg, successHelp+`amber_scrape_collector_success{collector="forecast"} 0
`, "amber_scrape_collector_success")
}

func TestForecastCollectorHorizonLimit(t *testing.T) {
	api := newFakeAPI(t)
	cfg := only(newClock(), "forecast")
	cfg.ForecastHorizon = 30 * time.Minute
	reg := newRegistry(t, api, cfg)

	if n, err := testutil.GatherAndCount(reg, "amber_forecast_price_cents_per_kwh"); err != nil || n != 3 {
		t.Errorf("got %d price series (err %v), want 3: one horizon on three channels", n, err)
	}
	want := "forecast " + activeSite + " next=1 resolution=30"
	if got := api.seen("forecast"); len(got) != 1 || got[0] != want {
		t.Errorf("forecast requests = %v, want [%s]", got, want)
	}
}

func TestUsageCollector(t *testing.T) {
	api := newFakeAPI(t)
	reg := newRegistry(t, api, only(newClock(), "usage"))
	compare(t, reg, `
# HELP amber_usage_daily_cost_cents Cost of the energy metered on the channel on the date, in cents. The date is in NEM time (UTC+10). Includes GST. For the general and controlledLoad channels, this is what you pay. For the feedIn channel, this is what you earn: a negative value means that you pay to export.
# TYPE amber_usage_daily_cost_cents gauge
amber_usage_daily_cost_cents{channel_id="B1",channel_type="feedIn",date="2026-11-12",site_id="01F5A5CRKMZ5BCX9P1S4V990AM"} 24
amber_usage_daily_cost_cents{channel_id="B1",channel_type="feedIn",date="2026-11-13",site_id="01F5A5CRKMZ5BCX9P1S4V990AM"} 20
amber_usage_daily_cost_cents{channel_id="E1",channel_type="general",date="2026-11-12",site_id="01F5A5CRKMZ5BCX9P1S4V990AM"} 42
amber_usage_daily_cost_cents{channel_id="E1",channel_type="general",date="2026-11-13",site_id="01F5A5CRKMZ5BCX9P1S4V990AM"} 70
amber_usage_daily_cost_cents{channel_id="E2",channel_type="controlledLoad",date="2026-11-13",site_id="01F5A5CRKMZ5BCX9P1S4V990AM"} 0
# HELP amber_usage_daily_energy_kwh Energy metered on the channel on the date, in kWh: energy used, or energy exported for the feedIn channel. The date is in NEM time (UTC+10).
# TYPE amber_usage_daily_energy_kwh gauge
amber_usage_daily_energy_kwh{channel_id="B1",channel_type="feedIn",date="2026-11-12",site_id="01F5A5CRKMZ5BCX9P1S4V990AM"} 3
amber_usage_daily_energy_kwh{channel_id="B1",channel_type="feedIn",date="2026-11-13",site_id="01F5A5CRKMZ5BCX9P1S4V990AM"} 2
amber_usage_daily_energy_kwh{channel_id="E1",channel_type="general",date="2026-11-12",site_id="01F5A5CRKMZ5BCX9P1S4V990AM"} 2
amber_usage_daily_energy_kwh{channel_id="E1",channel_type="general",date="2026-11-13",site_id="01F5A5CRKMZ5BCX9P1S4V990AM"} 3
amber_usage_daily_energy_kwh{channel_id="E2",channel_type="controlledLoad",date="2026-11-13",site_id="01F5A5CRKMZ5BCX9P1S4V990AM"} 0
# HELP amber_usage_daily_intervals Number of metered intervals on the channel on the date, by data quality. Estimated data can change until it is billable.
# TYPE amber_usage_daily_intervals gauge
amber_usage_daily_intervals{channel_id="B1",channel_type="feedIn",date="2026-11-12",quality="billable",site_id="01F5A5CRKMZ5BCX9P1S4V990AM"} 1
amber_usage_daily_intervals{channel_id="B1",channel_type="feedIn",date="2026-11-12",quality="estimated",site_id="01F5A5CRKMZ5BCX9P1S4V990AM"} 0
amber_usage_daily_intervals{channel_id="B1",channel_type="feedIn",date="2026-11-13",quality="billable",site_id="01F5A5CRKMZ5BCX9P1S4V990AM"} 0
amber_usage_daily_intervals{channel_id="B1",channel_type="feedIn",date="2026-11-13",quality="estimated",site_id="01F5A5CRKMZ5BCX9P1S4V990AM"} 1
amber_usage_daily_intervals{channel_id="E1",channel_type="general",date="2026-11-12",quality="billable",site_id="01F5A5CRKMZ5BCX9P1S4V990AM"} 2
amber_usage_daily_intervals{channel_id="E1",channel_type="general",date="2026-11-12",quality="estimated",site_id="01F5A5CRKMZ5BCX9P1S4V990AM"} 0
amber_usage_daily_intervals{channel_id="E1",channel_type="general",date="2026-11-13",quality="billable",site_id="01F5A5CRKMZ5BCX9P1S4V990AM"} 1
amber_usage_daily_intervals{channel_id="E1",channel_type="general",date="2026-11-13",quality="estimated",site_id="01F5A5CRKMZ5BCX9P1S4V990AM"} 1
amber_usage_daily_intervals{channel_id="E2",channel_type="controlledLoad",date="2026-11-13",quality="billable",site_id="01F5A5CRKMZ5BCX9P1S4V990AM"} 1
amber_usage_daily_intervals{channel_id="E2",channel_type="controlledLoad",date="2026-11-13",quality="estimated",site_id="01F5A5CRKMZ5BCX9P1S4V990AM"} 0
# HELP amber_usage_latest_interval_end_timestamp_seconds End of the latest metered interval that the API has, as a Unix timestamp.
# TYPE amber_usage_latest_interval_end_timestamp_seconds gauge
amber_usage_latest_interval_end_timestamp_seconds{site_id="01F5A5CRKMZ5BCX9P1S4V990AM"} 1.7944926e+09
`, "amber_usage_daily_energy_kwh", "amber_usage_daily_cost_cents", "amber_usage_daily_intervals",
		"amber_usage_latest_interval_end_timestamp_seconds")

	// 7 days that end today in NEM time.
	want := "usage " + activeSite + " 2026-11-08..2026-11-14"
	if got := api.seen("usage"); len(got) != 1 || got[0] != want {
		t.Errorf("usage requests = %v, want [%s]", got, want)
	}
}

// A lookback of more than 7 days is split into requests of at most 7 days.
func TestUsageCollectorSplitsRequests(t *testing.T) {
	api := newFakeAPI(t)
	cfg := only(newClock(), "usage")
	cfg.UsageLookbackDays = 16
	reg := newRegistry(t, api, cfg)

	if n, err := testutil.GatherAndCount(reg, "amber_usage_daily_cost_cents"); err != nil || n != 5 {
		t.Errorf("got %d cost series (err %v), want 5", n, err)
	}
	want := []string{
		"usage " + activeSite + " 2026-10-30..2026-11-05",
		"usage " + activeSite + " 2026-11-06..2026-11-12",
		"usage " + activeSite + " 2026-11-13..2026-11-14",
	}
	if got := api.seen("usage"); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("usage requests = %v, want %v", got, want)
	}
}

// The day changes at midnight NEM time, which is 14:00 UTC.
func TestUsageCollectorUsesNEMDate(t *testing.T) {
	api := newFakeAPI(t)
	clk := newClock()
	clk.advance(11 * time.Hour) // 14:40 UTC on the 14th is 00:40 on the 15th in NEM time.
	cfg := only(clk, "usage")
	cfg.UsageLookbackDays = 1
	reg := newRegistry(t, api, cfg)

	if _, err := reg.Gather(); err != nil {
		t.Fatal(err)
	}
	want := "usage " + activeSite + " 2026-11-15..2026-11-15"
	if got := api.seen("usage"); len(got) != 1 || got[0] != want {
		t.Errorf("usage requests = %v, want [%s]", got, want)
	}
}

const successHelp = `
# HELP amber_scrape_collector_success Whether the collector succeeded in this scrape: 1 for yes, 0 for no.
# TYPE amber_scrape_collector_success gauge
`

// The API is asked again only when the cached data is older than the TTL.
func TestCacheTTL(t *testing.T) {
	api := newFakeAPI(t)
	clk := newClock()
	reg := newRegistry(t, api, only(clk, "prices")) // TTL: 1 minute.

	gather := func() {
		t.Helper()
		if _, err := reg.Gather(); err != nil {
			t.Fatal(err)
		}
	}
	gather()
	clk.advance(59 * time.Second)
	gather()
	if got := len(api.seen("prices")); got != 1 {
		t.Fatalf("%d price requests inside the TTL, want 1", got)
	}
	clk.advance(time.Second)
	gather()
	if got := len(api.seen("prices")); got != 2 {
		t.Errorf("%d price requests after the TTL, want 2", got)
	}
	// The site list has a TTL of one hour.
	if got := len(api.seen("sites")); got != 1 {
		t.Errorf("%d site requests, want 1", got)
	}
}

// When the API fails, the collector reports the failure and sends no stale
// prices. The API is not asked again until the retry time has passed.
func TestCollectorFailure(t *testing.T) {
	api := newFakeAPI(t)
	clk := newClock()
	reg := newRegistry(t, api, only(clk, "prices"))

	if n, err := testutil.GatherAndCount(reg, "amber_price_cents_per_kwh"); err != nil || n != 3 {
		t.Fatalf("got %d price series (err %v), want 3", n, err)
	}

	api.setErr(errors.New("boom"))
	clk.advance(time.Minute)
	compare(t, reg, successHelp+`amber_scrape_collector_success{collector="prices"} 0
`, "amber_scrape_collector_success", "amber_price_cents_per_kwh")

	clk.advance(29 * time.Second)
	if _, err := reg.Gather(); err != nil {
		t.Fatal(err)
	}
	if got := len(api.seen("prices")); got != 2 {
		t.Fatalf("%d price requests, want 2: the failure is remembered for 30 seconds", got)
	}

	api.setErr(nil)
	clk.advance(time.Second)
	compare(t, reg, successHelp+`amber_scrape_collector_success{collector="prices"} 1
`, "amber_scrape_collector_success")
	if got := len(api.seen("prices")); got != 3 {
		t.Errorf("%d price requests, want 3", got)
	}
}

// With --amber.site-id, only the named sites are collected, whatever their
// status.
func TestSiteFilter(t *testing.T) {
	api := newFakeAPI(t)
	cfg := only(newClock(), "sites", "prices")
	cfg.SiteIDs = []string{closedSite}
	reg := newRegistry(t, api, cfg)

	if n, err := testutil.GatherAndCount(reg, "amber_site_info"); err != nil || n != 1 {
		t.Errorf("got %d site_info series (err %v), want 1", n, err)
	}
	if got := api.seen("prices"); len(got) != 1 || got[0] != "prices "+closedSite {
		t.Errorf("price requests = %v, want one for the configured site", got)
	}
}

func TestSiteFilterUnknownSite(t *testing.T) {
	cfg := only(newClock(), "sites")
	cfg.SiteIDs = []string{"NOSUCHSITE"}
	reg := newRegistry(t, newFakeAPI(t), cfg)

	compare(t, reg, successHelp+`amber_scrape_collector_success{collector="sites"} 0
`, "amber_scrape_collector_success", "amber_site_info")
}

func TestDisabledCollectors(t *testing.T) {
	api := newFakeAPI(t)
	reg := newRegistry(t, api, only(newClock(), "sites"))
	if _, err := reg.Gather(); err != nil {
		t.Fatal(err)
	}
	if got := append(api.seen("prices"), append(api.seen("forecast"), api.seen("usage")...)...); len(got) != 0 {
		t.Errorf("disabled collectors sent requests: %v", got)
	}

	if _, err := New(api, only(newClock()), slog.New(slog.DiscardHandler)); err == nil {
		t.Error("New with no collector enabled: got no error")
	}
}

func TestAllCollectorsByDefault(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Now = newClock().now
	c, err := New(newFakeAPI(t), cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(c.Names(), ","), "forecast,prices,sites,usage"; got != want {
		t.Errorf("collectors = %s, want %s", got, want)
	}

	// The metric names must pass the Prometheus linter, apart from the unit
	// rule: prices and costs are in the units of the API, c/kWh and cents.
	reg := prometheus.NewRegistry()
	reg.MustRegister(c)
	problems, err := testutil.GatherAndLint(reg)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range problems {
		t.Errorf("lint: %s: %s", p.Metric, p.Text)
	}
}

func TestValidate(t *testing.T) {
	for name, change := range map[string]func(*Config){
		"zero ttl":          func(c *Config) { c.PricesTTL = 0 },
		"horizon too short": func(c *Config) { c.ForecastHorizon = time.Minute },
		"horizon too long":  func(c *Config) { c.ForecastHorizon = 8 * 24 * time.Hour },
		"lookback too low":  func(c *Config) { c.UsageLookbackDays = 0 },
		"lookback too high": func(c *Config) { c.UsageLookbackDays = 91 },
	} {
		cfg := DefaultConfig()
		change(&cfg)
		if err := cfg.Validate(); err == nil {
			t.Errorf("%s: got no error", name)
		}
	}
	if err := DefaultConfig().Validate(); err != nil {
		t.Errorf("default config: %v", err)
	}
}

func TestRegisterFlags(t *testing.T) {
	parse := func(args ...string) Config {
		t.Helper()
		cfg := DefaultConfig()
		app := kingpin.New("test", "")
		cfg.RegisterFlags(app)
		if _, err := app.Parse(args); err != nil {
			t.Fatal(err)
		}
		return cfg
	}

	cfg := parse()
	want := DefaultConfig()
	if cfg.PricesTTL != want.PricesTTL || cfg.ForecastTTL != want.ForecastTTL || cfg.UsageTTL != want.UsageTTL ||
		cfg.SitesTTL != want.SitesTTL || cfg.ForecastHorizon != want.ForecastHorizon ||
		cfg.UsageLookbackDays != want.UsageLookbackDays || len(cfg.SiteIDs) != 0 {
		t.Errorf("no flags: got %+v, want the defaults", cfg)
	}
	for _, f := range factories {
		if !cfg.Enabled[f.name] {
			t.Errorf("no flags: collector %s is not enabled", f.name)
		}
	}

	cfg = parse("--no-collector.usage", "--collector.forecast.horizon=12h", "--collector.prices.cache-ttl=30s",
		"--collector.usage.lookback-days=90", "--amber.site-id=A", "--amber.site-id=B")
	if cfg.Enabled["usage"] || !cfg.Enabled["prices"] {
		t.Errorf("enabled = %v, want usage off and prices on", cfg.Enabled)
	}
	if cfg.ForecastHorizon != 12*time.Hour || cfg.PricesTTL != 30*time.Second || cfg.UsageLookbackDays != 90 {
		t.Errorf("unexpected settings: %+v", cfg)
	}
	if strings.Join(cfg.SiteIDs, ",") != "A,B" {
		t.Errorf("site ids = %v, want [A B]", cfg.SiteIDs)
	}
}

func TestAPIMetrics(t *testing.T) {
	m := NewAPIMetrics()
	reg := prometheus.NewRegistry()
	reg.MustRegister(m)

	m.Observe("prices", 200, 250*time.Millisecond)
	m.Observe("prices", 200, 250*time.Millisecond)
	m.Observe("usage", 0, time.Second)
	compare(t, reg, `
# HELP amber_api_requests_total Requests sent to the Amber API, by endpoint and HTTP status code. The code is "error" when there was no response.
# TYPE amber_api_requests_total counter
amber_api_requests_total{code="200",endpoint="prices"} 2
amber_api_requests_total{code="error",endpoint="usage"} 1
# HELP amber_api_request_duration_seconds_total Total time spent on requests to the Amber API, by endpoint.
# TYPE amber_api_request_duration_seconds_total counter
amber_api_request_duration_seconds_total{endpoint="prices"} 0.5
amber_api_request_duration_seconds_total{endpoint="usage"} 1
`)

	// The rate limit is exported after the API has reported it.
	rl := amber.RateLimit{}
	m.SetRateLimitSource(func() amber.RateLimit { return rl })
	if n, err := testutil.GatherAndCount(reg, "amber_api_ratelimit_remaining"); err != nil || n != 0 {
		t.Errorf("got %d rate limit series (err %v) before the API reported one, want 0", n, err)
	}
	rl = amber.RateLimit{Limit: 50, Remaining: 42, Reset: time.Unix(1794627900, 0), Known: true}
	compare(t, reg, `
# HELP amber_api_ratelimit_limit Number of requests the Amber API allows in one rate limit window.
# TYPE amber_api_ratelimit_limit gauge
amber_api_ratelimit_limit 50
# HELP amber_api_ratelimit_remaining Number of requests left in the rate limit window, as of the last API response.
# TYPE amber_api_ratelimit_remaining gauge
amber_api_ratelimit_remaining 42
# HELP amber_api_ratelimit_reset_timestamp_seconds End of the rate limit window as a Unix timestamp, as of the last API response.
# TYPE amber_api_ratelimit_reset_timestamp_seconds gauge
amber_api_ratelimit_reset_timestamp_seconds 1.7946279e+09
`, "amber_api_ratelimit_limit", "amber_api_ratelimit_remaining", "amber_api_ratelimit_reset_timestamp_seconds")
}
