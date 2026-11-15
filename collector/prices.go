package collector

import (
	"context"
	"errors"
	"fmt"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/goodtune/amber_exporter/amber"
)

// Values of the descriptor and spikeStatus fields. A series is exported for
// each one so that the series of a state does not come and go.
var (
	priceDescriptors = []string{"extremelyLow", "veryLow", "low", "neutral", "high", "spike"}
	spikeStatuses    = []string{"none", "potential", "spike"}
)

const signHelp = " Includes GST. For the general and controlledLoad channels, this is what you pay. For the feedIn channel, this is what you earn: a negative value means that you pay to export."

// signed returns a price or a cost with the sign that the exporter uses.
//
// The API reports all values as what the customer pays, so the feedIn
// channel is negative when the customer earns. The exporter turns the feedIn
// channel around: positive is money earned, negative is money paid to export.
func signed(channelType string, v float64) float64 {
	if channelType != amber.ChannelFeedIn || v == 0 {
		// Zero is returned as it is: its negation is shown as "-0".
		return v
	}
	return -v
}

// pricesCollector exports the current interval of each channel.
type pricesCollector struct {
	api   API
	sites *siteSource
	cache *siteCaches[[]amber.Interval]

	price         *prometheus.Desc
	estimate      *prometheus.Desc
	descriptor    *prometheus.Desc
	spikeStatus   *prometheus.Desc
	intervalStart *prometheus.Desc
	intervalEnd   *prometheus.Desc
	spotPrice     *prometheus.Desc
	renewables    *prometheus.Desc
	tariffInfo    *prometheus.Desc
	demandWindow  *prometheus.Desc
}

func newPricesCollector(d deps) Collector {
	site := []string{"site_id"}
	channel := []string{"site_id", "channel_type"}
	return &pricesCollector{
		api:   d.api,
		sites: d.sites,
		cache: newSiteCaches[[]amber.Interval](d.cfg.PricesTTL, d.now),
		price: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "price", "cents_per_kwh"),
			"Price of the current interval in c/kWh."+signHelp,
			channel, nil,
		),
		estimate: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "price", "estimate"),
			"Whether the price of the current interval is an estimate (1) or final (0).",
			channel, nil,
		),
		descriptor: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "price", "descriptor"),
			"Amber's description of the current price. The series of the current descriptor is 1, the others are 0.",
			[]string{"site_id", "channel_type", "descriptor"}, nil,
		),
		spikeStatus: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "price", "spike_status"),
			"Spike status of the current interval. The series of the current status is 1, the others are 0.",
			[]string{"site_id", "channel_type", "status"}, nil,
		),
		intervalStart: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "price", "interval_start_timestamp_seconds"),
			"Start of the current interval as a Unix timestamp.",
			site, nil,
		),
		intervalEnd: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "price", "interval_end_timestamp_seconds"),
			"End of the current interval as a Unix timestamp.",
			site, nil,
		),
		spotPrice: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "spot_price", "cents_per_kwh"),
			"NEM spot price of the current interval in c/kWh. Includes GST.",
			site, nil,
		),
		renewables: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "renewables", "ratio"),
			"Part of the grid's energy that comes from renewables in the current interval, from 0 to 1.",
			site, nil,
		),
		tariffInfo: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "tariff", "info"),
			"Time of use period and season of the current interval. The value is always 1. Only for channels on a time of use tariff.",
			[]string{"site_id", "channel_type", "period", "season"}, nil,
		),
		demandWindow: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "tariff", "demand_window"),
			"Whether the current interval is in the demand window: 1 for yes, 0 for no. Only for channels on a demand tariff.",
			channel, nil,
		),
	}
}

func (c *pricesCollector) Update(ctx context.Context, ch chan<- prometheus.Metric) error {
	sites, err := c.sites.priced(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, site := range sites {
		intervals, err := c.cache.get(ctx, site.ID, func(ctx context.Context) ([]amber.Interval, error) {
			return c.api.CurrentPrices(ctx, site.ID, amber.PriceOptions{})
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("site %s: get current prices: %w", site.ID, err))
			continue
		}
		if err := c.collectSite(ch, site.ID, intervals); err != nil {
			errs = append(errs, fmt.Errorf("site %s: %w", site.ID, err))
		}
	}
	return errors.Join(errs...)
}

func (c *pricesCollector) collectSite(ch chan<- prometheus.Metric, siteID string, intervals []amber.Interval) error {
	var current []amber.Interval
	seen := map[string]bool{}
	for _, i := range intervals {
		if i.Type == amber.TypeCurrent && !seen[i.ChannelType] {
			seen[i.ChannelType] = true
			current = append(current, i)
		}
	}
	if len(current) == 0 {
		return errors.New("the API returned no current interval")
	}
	c.send(ch, siteID, current)
	return nil
}

// send sends the metrics of one interval of a site. The slice has that
// interval once for each channel and must not be empty.
func (c *pricesCollector) send(ch chan<- prometheus.Metric, siteID string, current []amber.Interval) {
	for _, i := range current {
		gauge(ch, c.price, signed(i.ChannelType, i.PerKwh), siteID, i.ChannelType)
		if i.Estimate != nil {
			gauge(ch, c.estimate, boolValue(*i.Estimate), siteID, i.ChannelType)
		}
		stateSet(ch, c.descriptor, priceDescriptors, i.Descriptor, siteID, i.ChannelType)
		stateSet(ch, c.spikeStatus, spikeStatuses, i.SpikeStatus, siteID, i.ChannelType)
		if t := i.TariffInformation; t != nil {
			if t.Period != "" || t.Season != "" {
				gauge(ch, c.tariffInfo, 1, siteID, i.ChannelType, t.Period, t.Season)
			}
			if t.DemandWindow != nil {
				gauge(ch, c.demandWindow, boolValue(*t.DemandWindow), siteID, i.ChannelType)
			}
		}
	}

	// The spot price, the renewables and the interval times are the same on
	// all channels of a site.
	i := siteInterval(current)
	gauge(ch, c.intervalStart, unixSeconds(i.Start()), siteID)
	gauge(ch, c.intervalEnd, unixSeconds(i.EndTime), siteID)
	gauge(ch, c.spotPrice, i.SpotPerKwh, siteID)
	gauge(ch, c.renewables, i.Renewables/100, siteID)
}

// siteInterval returns the interval that site-wide values are read from:
// the general channel when there is one. The slice must not be empty.
func siteInterval(intervals []amber.Interval) amber.Interval {
	for _, i := range intervals {
		if i.ChannelType == amber.ChannelGeneral {
			return i
		}
	}
	return intervals[0]
}

// stateSet sends one series for each known state: 1 for the current state
// and 0 for the others. A state that is not known gets a series of its own.
func stateSet(ch chan<- prometheus.Metric, desc *prometheus.Desc, states []string, current string, labels ...string) {
	known := false
	for _, s := range states {
		gauge(ch, desc, boolValue(s == current), append(labels, s)...)
		known = known || s == current
	}
	if !known && current != "" {
		gauge(ch, desc, 1, append(labels, current)...)
	}
}
