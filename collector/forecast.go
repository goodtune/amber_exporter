package collector

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/model"

	"github.com/goodtune/amber_exporter/amber"
)

// forecastResolution is the interval length that forecasts are requested in.
// The API has 5 minute intervals only for the next hour or so. 30 minute
// intervals cover the full forecast and give a fixed set of horizons.
const forecastResolution = 30 * time.Minute

// forecastCollector exports forecast prices.
//
// A forecast is about a time in the future, and Prometheus only stores
// samples for the present. The forecast is therefore exported as what is
// known now, with the lead time in the horizon label: horizon="1h30m" is the
// interval that starts 1 hour 30 minutes after the start of the current
// 30 minute interval. The label values are a fixed set, so the number of
// series does not grow.
type forecastCollector struct {
	api     API
	sites   *siteSource
	cache   *siteCaches[[]amber.Interval]
	horizon time.Duration
	now     func() time.Time

	price         *prometheus.Desc
	advancedPrice *prometheus.Desc
	spotPrice     *prometheus.Desc
	renewables    *prometheus.Desc
	intervalStart *prometheus.Desc
}

func newForecastCollector(d deps) Collector {
	return &forecastCollector{
		api:     d.api,
		sites:   d.sites,
		cache:   newSiteCaches[[]amber.Interval](d.cfg.ForecastTTL, d.now),
		horizon: d.cfg.ForecastHorizon,
		now:     d.now,
		price: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "forecast", "price_cents_per_kwh"),
			"Forecast price in c/kWh, from the AEMO forecast, for the 30 minute interval that starts one horizon after the start of the current one."+signHelp,
			[]string{"site_id", "channel_type", "horizon"}, nil,
		),
		advancedPrice: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "forecast", "advanced_price_cents_per_kwh"),
			"Amber's own forecast price in c/kWh: the predicted price and the low and high ends of the band around it. Not available for all horizons."+signHelp,
			[]string{"site_id", "channel_type", "horizon", "estimate"}, nil,
		),
		spotPrice: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "forecast", "spot_price_cents_per_kwh"),
			"Forecast NEM spot price in c/kWh. Includes GST.",
			[]string{"site_id", "horizon"}, nil,
		),
		renewables: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "forecast", "renewables_ratio"),
			"Forecast part of the grid's energy that comes from renewables, from 0 to 1.",
			[]string{"site_id", "horizon"}, nil,
		),
		intervalStart: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "forecast", "interval_start_timestamp_seconds"),
			"Start of the forecast interval as a Unix timestamp. Use it to put forecast values on a time axis.",
			[]string{"site_id", "horizon"}, nil,
		),
	}
}

func (c *forecastCollector) Update(ctx context.Context, ch chan<- prometheus.Metric) error {
	sites, err := c.sites.priced(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, site := range sites {
		intervals, err := c.cache.get(ctx, site.ID, func(ctx context.Context) ([]amber.Interval, error) {
			return c.api.CurrentPrices(ctx, site.ID, amber.PriceOptions{
				Next:       int(c.horizon / forecastResolution),
				Resolution: int(forecastResolution / time.Minute),
			})
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("site %s: get forecast: %w", site.ID, err))
			continue
		}
		if err := c.collectSite(ch, site.ID, intervals); err != nil {
			errs = append(errs, fmt.Errorf("site %s: %w", site.ID, err))
		}
	}
	return errors.Join(errs...)
}

func (c *forecastCollector) collectSite(ch chan<- prometheus.Metric, siteID string, intervals []amber.Interval) error {
	now := c.now()
	byChannel := map[string][]amber.Interval{}
	var channels []string
	for _, i := range intervals {
		if _, ok := byChannel[i.ChannelType]; !ok {
			channels = append(channels, i.ChannelType)
		}
		byChannel[i.ChannelType] = append(byChannel[i.ChannelType], i)
	}
	if len(channels) == 0 {
		return errors.New("the API returned no intervals")
	}

	// Site-wide values are sent once for each horizon, from the general
	// channel when there is one.
	siteChannel := channels[0]
	if _, ok := byChannel[amber.ChannelGeneral]; ok {
		siteChannel = amber.ChannelGeneral
	}

	var errs []error
	for _, channel := range channels {
		upcoming, err := upcomingIntervals(byChannel[channel], now)
		if err != nil {
			errs = append(errs, fmt.Errorf("channel %s: %w", channel, err))
			continue
		}
		for _, f := range upcoming {
			if f.horizon > c.horizon {
				break
			}
			horizon := model.Duration(f.horizon).String()
			gauge(ch, c.price, signed(channel, f.PerKwh), siteID, channel, horizon)
			if a := f.AdvancedPrice; a != nil {
				low, high := signed(channel, a.Low), signed(channel, a.High)
				if low > high {
					// The change of sign turns the band around.
					low, high = high, low
				}
				gauge(ch, c.advancedPrice, low, siteID, channel, horizon, "low")
				gauge(ch, c.advancedPrice, signed(channel, a.Predicted), siteID, channel, horizon, "predicted")
				gauge(ch, c.advancedPrice, high, siteID, channel, horizon, "high")
			}
			if channel == siteChannel {
				gauge(ch, c.spotPrice, f.SpotPerKwh, siteID, horizon)
				gauge(ch, c.renewables, f.Renewables/100, siteID, horizon)
				gauge(ch, c.intervalStart, unixSeconds(f.Start()), siteID, horizon)
			}
		}
	}
	return errors.Join(errs...)
}

type forecastInterval struct {
	amber.Interval
	// horizon is the time from the start of the current interval to the
	// start of this interval.
	horizon time.Duration
}

// upcomingIntervals returns the intervals of one channel that start after
// the interval that contains now, each with its horizon, soonest first.
//
// The current interval is found from the clock, not from the type field,
// because the data can come from the cache: an interval that was a forecast
// when it was fetched can be the current one now.
func upcomingIntervals(intervals []amber.Interval, now time.Time) ([]forecastInterval, error) {
	sorted := append([]amber.Interval(nil), intervals...)
	sort.Slice(sorted, func(a, b int) bool { return sorted[a].EndTime.Before(sorted[b].EndTime) })

	current := sort.Search(len(sorted), func(n int) bool { return sorted[n].EndTime.After(now) })
	if current == len(sorted) {
		return nil, errors.New("all intervals are in the past")
	}
	var out []forecastInterval
	var previous time.Duration
	for _, i := range sorted[current+1:] {
		// Whole minutes: the API reports interval ends on the minute.
		horizon := i.EndTime.Sub(sorted[current].EndTime).Round(time.Minute)
		if horizon <= previous {
			continue
		}
		previous = horizon
		out = append(out, forecastInterval{Interval: i, horizon: horizon})
	}
	return out, nil
}
