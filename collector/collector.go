// Package collector turns Amber API data into Prometheus metrics.
package collector

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/alecthomas/kingpin/v2"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/goodtune/amber_exporter/amber"
)

const namespace = "amber"

// API is the part of the Amber client that the collectors use.
type API interface {
	Sites(ctx context.Context) ([]amber.Site, error)
	CurrentPrices(ctx context.Context, siteID string, opts amber.PriceOptions) ([]amber.Interval, error)
	Usage(ctx context.Context, siteID string, start, end time.Time) ([]amber.Usage, error)
}

// Collector gets metrics from one area of the API.
type Collector interface {
	// Update sends the metrics on ch. Metrics sent before an error is
	// returned are kept.
	Update(ctx context.Context, ch chan<- prometheus.Metric) error
}

// Config holds the settings of all collectors.
type Config struct {
	// SiteIDs limits collection to these sites. When it is empty, all active
	// sites of the account are collected.
	SiteIDs []string

	// Enabled says which collectors run. A collector that is not in the map
	// is enabled.
	Enabled map[string]bool

	// SitesTTL, PricesTTL, ForecastTTL and UsageTTL are the shortest times
	// between two API requests for the same data.
	SitesTTL    time.Duration
	PricesTTL   time.Duration
	ForecastTTL time.Duration
	UsageTTL    time.Duration

	// ForecastHorizon is how far ahead forecast prices are exported.
	ForecastHorizon time.Duration

	// UsageLookbackDays is the number of days of usage that are exported.
	UsageLookbackDays int

	// Now is the time source. It is for tests.
	Now func() time.Time
}

// DefaultConfig returns the default settings.
func DefaultConfig() Config {
	return Config{
		Enabled:           map[string]bool{},
		SitesTTL:          time.Hour,
		PricesTTL:         time.Minute,
		ForecastTTL:       5 * time.Minute,
		UsageTTL:          30 * time.Minute,
		ForecastHorizon:   48 * time.Hour,
		UsageLookbackDays: 7,
	}
}

// Limits of the API.
const (
	maxForecastHorizon   = 7 * 24 * time.Hour
	maxUsageLookbackDays = 90
)

// Validate reports settings that the API cannot serve.
func (c Config) Validate() error {
	for name, ttl := range map[string]time.Duration{
		"sites": c.SitesTTL, "prices": c.PricesTTL, "forecast": c.ForecastTTL, "usage": c.UsageTTL,
	} {
		if ttl <= 0 {
			return fmt.Errorf("collector.%s.cache-ttl must be more than 0", name)
		}
	}
	if c.ForecastHorizon < forecastResolution || c.ForecastHorizon > maxForecastHorizon {
		return fmt.Errorf("collector.forecast.horizon must be from %s to %s", forecastResolution, maxForecastHorizon)
	}
	if c.UsageLookbackDays < 1 || c.UsageLookbackDays > maxUsageLookbackDays {
		return fmt.Errorf("collector.usage.lookback-days must be from 1 to %d", maxUsageLookbackDays)
	}
	return nil
}

type factory struct {
	name string
	help string
	new  func(deps) Collector
}

// deps is what a collector gets when it is made.
type deps struct {
	api   API
	sites *siteSource
	cfg   Config
	now   func() time.Time
}

// factories lists all collectors. All are enabled by default.
var factories = []factory{
	{"sites", "Sites and meter channels of the account.", newSitesCollector},
	{"prices", "Price of the current interval.", newPricesCollector},
	{"forecast", "Forecast prices, labeled by how far ahead they are.", newForecastCollector},
	{"usage", "Metered energy and cost for each day.", newUsageCollector},
}

// RegisterFlags adds the collector flags to app. The flags write to c.
func (c *Config) RegisterFlags(app *kingpin.Application) {
	if c.Enabled == nil {
		c.Enabled = map[string]bool{}
	}
	app.Flag("amber.site-id", "Site to collect. Repeat the flag for more sites. Default: all active sites of the account.").
		StringsVar(&c.SiteIDs)

	for _, f := range factories {
		name := f.name
		c.Enabled[name] = true
		app.Flag("collector."+name, fmt.Sprintf("Enable the %s collector: %s", name, f.help)).
			Default("true").
			SetValue(&enabledFlag{enabled: c.Enabled, name: name})
	}

	ttlHelp := "Shortest time between two API requests for %s."
	app.Flag("collector.sites.cache-ttl", fmt.Sprintf(ttlHelp, "the site list")).
		Default(c.SitesTTL.String()).DurationVar(&c.SitesTTL)
	app.Flag("collector.prices.cache-ttl", fmt.Sprintf(ttlHelp, "the current price")).
		Default(c.PricesTTL.String()).DurationVar(&c.PricesTTL)
	app.Flag("collector.forecast.cache-ttl", fmt.Sprintf(ttlHelp, "the forecast")).
		Default(c.ForecastTTL.String()).DurationVar(&c.ForecastTTL)
	app.Flag("collector.usage.cache-ttl", fmt.Sprintf(ttlHelp, "usage data")).
		Default(c.UsageTTL.String()).DurationVar(&c.UsageTTL)
	app.Flag("collector.forecast.horizon", "How far ahead forecast prices are exported. The API has about 38 hours.").
		Default(c.ForecastHorizon.String()).DurationVar(&c.ForecastHorizon)
	app.Flag("collector.usage.lookback-days", "Number of days of usage to export, 1 to 90. Each 7 days is one API request.").
		Default(fmt.Sprint(c.UsageLookbackDays)).IntVar(&c.UsageLookbackDays)
}

// enabledFlag is a boolean flag that writes to one entry of a map. Kingpin
// also accepts the --no- form of the flag because of IsBoolFlag.
type enabledFlag struct {
	enabled map[string]bool
	name    string
}

func (e *enabledFlag) Set(s string) error {
	b, err := strconv.ParseBool(s)
	if err != nil {
		return err
	}
	e.enabled[e.name] = b
	return nil
}

func (e *enabledFlag) String() string   { return strconv.FormatBool(e.enabled[e.name]) }
func (e *enabledFlag) IsBoolFlag() bool { return true }

// AmberCollector runs the enabled collectors on each scrape.
type AmberCollector struct {
	collectors map[string]Collector
	logger     *slog.Logger
}

var (
	scrapeDurationDesc = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "scrape", "collector_duration_seconds"),
		"Time the collector took in this scrape. It is short when cached data is served.",
		[]string{"collector"}, nil,
	)
	scrapeSuccessDesc = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "scrape", "collector_success"),
		"Whether the collector succeeded in this scrape: 1 for yes, 0 for no.",
		[]string{"collector"}, nil,
	)
)

// New makes the collector for the given API and settings.
func New(api API, cfg Config, logger *slog.Logger) (*AmberCollector, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	d := deps{
		api:   api,
		sites: newSiteSource(api, cfg.SiteIDs, cfg.SitesTTL, now),
		cfg:   cfg,
		now:   now,
	}
	collectors := map[string]Collector{}
	for _, f := range factories {
		if enabled, set := cfg.Enabled[f.name]; set && !enabled {
			continue
		}
		collectors[f.name] = f.new(d)
	}
	if len(collectors) == 0 {
		return nil, errors.New("no collector is enabled")
	}
	return &AmberCollector{collectors: collectors, logger: logger}, nil
}

// Names returns the names of the enabled collectors, sorted.
func (a *AmberCollector) Names() []string {
	names := make([]string, 0, len(a.collectors))
	for name := range a.collectors {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Describe implements prometheus.Collector.
func (a *AmberCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- scrapeDurationDesc
	ch <- scrapeSuccessDesc
}

// Collect implements prometheus.Collector.
func (a *AmberCollector) Collect(ch chan<- prometheus.Metric) {
	// The scrape is not cancelled when Prometheus gives up: the fetch then
	// finishes in the background and the next scrape is served from the cache.
	ctx := context.Background()
	var wg sync.WaitGroup
	for name, c := range a.collectors {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a.run(ctx, name, c, ch)
		}()
	}
	wg.Wait()
}

func (a *AmberCollector) run(ctx context.Context, name string, c Collector, ch chan<- prometheus.Metric) {
	begin := time.Now()
	err := c.Update(ctx, ch)
	duration := time.Since(begin)

	success := 1.0
	if err != nil {
		success = 0
		level := slog.LevelError
		if amber.IsRateLimited(err) {
			level = slog.LevelWarn
		}
		a.logger.Log(ctx, level, "collector failed", "collector", name, "duration", duration, "err", err)
	} else {
		a.logger.Debug("collector succeeded", "collector", name, "duration", duration)
	}
	ch <- prometheus.MustNewConstMetric(scrapeDurationDesc, prometheus.GaugeValue, duration.Seconds(), name)
	ch <- prometheus.MustNewConstMetric(scrapeSuccessDesc, prometheus.GaugeValue, success, name)
}

func gauge(ch chan<- prometheus.Metric, desc *prometheus.Desc, value float64, labels ...string) {
	ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, value, labels...)
}

func boolValue(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func unixSeconds(t time.Time) float64 {
	return float64(t.UnixNano()) / 1e9
}
