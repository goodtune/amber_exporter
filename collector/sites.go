package collector

import (
	"context"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/goodtune/amber_exporter/amber"
)

// siteSource gives the collectors the sites of the account. The site list
// changes rarely, so it is cached for a long time.
type siteSource struct {
	api    API
	filter []string
	cache  *cache[[]amber.Site]
}

func newSiteSource(api API, filter []string, ttl time.Duration, now func() time.Time) *siteSource {
	return &siteSource{api: api, filter: filter, cache: newCache[[]amber.Site](ttl, now)}
}

// listed returns the sites that metrics are exported for: the configured
// sites, or all sites of the account when none are configured.
func (s *siteSource) listed(ctx context.Context) ([]amber.Site, error) {
	sites, err := s.cache.get(ctx, s.api.Sites)
	if err != nil {
		return nil, fmt.Errorf("get sites: %w", err)
	}
	if len(s.filter) == 0 {
		return sites, nil
	}
	byID := map[string]amber.Site{}
	for _, site := range sites {
		byID[site.ID] = site
	}
	var out []amber.Site
	for _, id := range s.filter {
		site, ok := byID[id]
		if !ok {
			return nil, fmt.Errorf("site %q is not linked to this account", id)
		}
		out = append(out, site)
	}
	return out, nil
}

// priced returns the sites that prices and usage are collected for: the
// configured sites, or the active sites when none are configured.
func (s *siteSource) priced(ctx context.Context) ([]amber.Site, error) {
	sites, err := s.listed(ctx)
	if err != nil {
		return nil, err
	}
	if len(s.filter) > 0 {
		return sites, nil
	}
	var out []amber.Site
	for _, site := range sites {
		if site.Status == amber.StatusActive {
			out = append(out, site)
		}
	}
	return out, nil
}

type sitesCollector struct {
	sites *siteSource

	info           *prometheus.Desc
	intervalLength *prometheus.Desc
	channelInfo    *prometheus.Desc
}

func newSitesCollector(d deps) Collector {
	return &sitesCollector{
		sites: d.sites,
		info: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "site", "info"),
			"A site linked to the account. The value is always 1.",
			[]string{"site_id", "nmi", "network", "status"}, nil,
		),
		intervalLength: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "site", "interval_length_seconds"),
			"Length of the billing interval of the site.",
			[]string{"site_id"}, nil,
		),
		channelInfo: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "site", "channel_info"),
			"A meter channel of a site. The value is always 1.",
			[]string{"site_id", "channel_id", "channel_type", "tariff"}, nil,
		),
	}
}

func (c *sitesCollector) Update(ctx context.Context, ch chan<- prometheus.Metric) error {
	sites, err := c.sites.listed(ctx)
	if err != nil {
		return err
	}
	for _, site := range sites {
		gauge(ch, c.info, 1, site.ID, site.NMI, site.Network, site.Status)
		gauge(ch, c.intervalLength, site.IntervalLength*60, site.ID)
		for _, channel := range site.Channels {
			gauge(ch, c.channelInfo, 1, site.ID, channel.Identifier, channel.Type, channel.Tariff)
		}
	}
	return nil
}
