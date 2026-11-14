package collector

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/goodtune/amber_exporter/amber"
)

// nemTime is the time zone of the National Electricity Market: UTC+10 with
// no daylight saving. The API groups intervals into days in this zone.
var nemTime = time.FixedZone("NEM", 10*60*60)

// usageRequestDays is the most days the API returns in one usage request.
const usageRequestDays = 7

// Qualities that a series is always exported for.
var usageQualities = []string{amber.QualityBillable, amber.QualityEstimated}

// usageCollector exports the metered energy and its cost as totals for each
// day.
//
// Usage data comes from the meter a day or more after the fact, a whole day
// at a time, and it can be revised later. A sample with the time of the
// scrape cannot say when the energy was used. The totals are therefore
// labeled with the date that they are for. A date is a fixed fact, so a
// series is made once for each day and channel and the latest value of the
// series is the truth for that day.
type usageCollector struct {
	api      API
	sites    *siteSource
	cache    *siteCaches[[]amber.Usage]
	lookback int
	now      func() time.Time

	energy    *prometheus.Desc
	cost      *prometheus.Desc
	intervals *prometheus.Desc
	latestEnd *prometheus.Desc
}

func newUsageCollector(d deps) Collector {
	day := []string{"site_id", "channel_id", "channel_type", "date"}
	return &usageCollector{
		api:      d.api,
		sites:    d.sites,
		cache:    newSiteCaches[[]amber.Usage](d.cfg.UsageTTL, d.now),
		lookback: d.cfg.UsageLookbackDays,
		now:      d.now,
		energy: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "usage", "daily_energy_kwh"),
			"Energy metered on the channel on the date, in kWh: energy used, or energy exported for the feedIn channel. The date is in NEM time (UTC+10).",
			day, nil,
		),
		cost: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "usage", "daily_cost_cents"),
			"Cost of the energy metered on the channel on the date, in cents. The date is in NEM time (UTC+10)."+signHelp,
			day, nil,
		),
		intervals: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "usage", "daily_intervals"),
			"Number of metered intervals on the channel on the date, by data quality. Estimated data can change until it is billable.",
			append(day, "quality"), nil,
		),
		latestEnd: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "usage", "latest_interval_end_timestamp_seconds"),
			"End of the latest metered interval that the API has, as a Unix timestamp.",
			[]string{"site_id"}, nil,
		),
	}
}

func (c *usageCollector) Update(ctx context.Context, ch chan<- prometheus.Metric) error {
	sites, err := c.sites.priced(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, site := range sites {
		usage, err := c.cache.get(ctx, site.ID, func(ctx context.Context) ([]amber.Usage, error) {
			return c.fetch(ctx, site.ID)
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("site %s: get usage: %w", site.ID, err))
			continue
		}
		c.collectSite(ch, site.ID, usage)
	}
	return errors.Join(errs...)
}

// fetch gets the usage of the last lookback days, today included, in
// requests of at most 7 days.
func (c *usageCollector) fetch(ctx context.Context, siteID string) ([]amber.Usage, error) {
	now := c.now().In(nemTime)
	end := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, nemTime)
	start := end.AddDate(0, 0, -(c.lookback - 1))

	var all []amber.Usage
	for from := start; !from.After(end); from = from.AddDate(0, 0, usageRequestDays) {
		to := from.AddDate(0, 0, usageRequestDays-1)
		if to.After(end) {
			to = end
		}
		usage, err := c.api.Usage(ctx, siteID, from, to)
		if err != nil {
			return nil, err
		}
		all = append(all, usage...)
	}
	return all, nil
}

// round6 rounds to 6 decimal places. It removes the noise that adding
// hundreds of floating point values leaves, such as 413.5260000000003.
func round6(v float64) float64 {
	return math.Round(v*1e6) / 1e6
}

type usageDay struct {
	channelID   string
	channelType string
	date        string
}

type usageTotal struct {
	kwh       float64
	cost      float64
	intervals map[string]int
}

func (c *usageCollector) collectSite(ch chan<- prometheus.Metric, siteID string, usage []amber.Usage) {
	totals := map[usageDay]*usageTotal{}
	var latest time.Time
	for _, u := range usage {
		key := usageDay{channelID: u.ChannelIdentifier, channelType: u.ChannelType, date: u.Date}
		t, ok := totals[key]
		if !ok {
			t = &usageTotal{intervals: map[string]int{}}
			for _, q := range usageQualities {
				t.intervals[q] = 0
			}
			totals[key] = t
		}
		t.kwh += u.Kwh
		t.cost += u.Cost
		t.intervals[u.Quality]++
		if u.EndTime.After(latest) {
			latest = u.EndTime
		}
	}

	keys := make([]usageDay, 0, len(totals))
	for key := range totals {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(a, b int) bool {
		if keys[a].date != keys[b].date {
			return keys[a].date < keys[b].date
		}
		return keys[a].channelID < keys[b].channelID
	})
	for _, key := range keys {
		t := totals[key]
		labels := []string{siteID, key.channelID, key.channelType, key.date}
		gauge(ch, c.energy, round6(t.kwh), labels...)
		gauge(ch, c.cost, signed(key.channelType, round6(t.cost)), labels...)
		for quality, n := range t.intervals {
			gauge(ch, c.intervals, float64(n), append(labels, quality)...)
		}
	}
	if !latest.IsZero() {
		gauge(ch, c.latestEnd, unixSeconds(latest), siteID)
	}
}
