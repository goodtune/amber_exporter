package collector

import (
	"bufio"
	"context"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/goodtune/amber_exporter/amber"
)

// priceRequestDays is the most days the API returns in one prices request.
const priceRequestDays = 7

// PriceAPI is the part of the Amber client that FetchPriceHistory uses.
type PriceAPI interface {
	Prices(ctx context.Context, siteID string, start, end time.Time) ([]amber.Interval, error)
}

// FetchPriceHistory gets the past prices of a site for the days from start
// to end, in requests of at most 7 days. The API works in whole days of NEM
// time, so the result can begin before start and end after end.
func FetchPriceHistory(ctx context.Context, api PriceAPI, siteID string, start, end time.Time) ([]amber.Interval, error) {
	first, last := nemDate(start), nemDate(end)
	var all []amber.Interval
	for from := first; !from.After(last); from = from.AddDate(0, 0, priceRequestDays) {
		to := from.AddDate(0, 0, priceRequestDays-1)
		if to.After(last) {
			to = last
		}
		intervals, err := api.Prices(ctx, siteID, from, to)
		if err != nil {
			return nil, err
		}
		all = append(all, intervals...)
	}
	return all, nil
}

// nemDate returns the start of the day of t in NEM time.
func nemDate(t time.Time) time.Time {
	t = t.In(nemTime)
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, nemTime)
}

// HistoryOptions says how PriceHistory writes the samples.
type HistoryOptions struct {
	// Job and Instance are the labels that Prometheus adds to the series of
	// the exporter when it scrapes it. With the same values, the history
	// continues the series of the scrapes.
	Job      string
	Instance string

	// Start and End limit the samples to this time range. Start is
	// included, End is not. A zero value sets no limit.
	Start time.Time
	End   time.Time

	// Step is the time between two samples of an interval, like the scrape
	// interval. The default is 1 minute.
	Step time.Duration
}

// PriceHistory writes past prices as the samples that the prices collector
// would have exported at the time, in the OpenMetrics text format that
// "promtool tsdb create-blocks-from openmetrics" reads.
type PriceHistory struct {
	opts      HistoryOptions
	collector *pricesCollector
	registry  *prometheus.Registry
	pending   pendingStep
	steps     []historyStep
}

// historyStep holds the metrics of one interval of one site.
type historyStep struct {
	start, end time.Time
	families   []*dto.MetricFamily
}

// pendingStep is the interval that the registry gathers. It has no Desc,
// so the registry does not check the metrics against a description.
type pendingStep struct {
	collector *pricesCollector
	siteID    string
	intervals []amber.Interval
}

func (p *pendingStep) Describe(chan<- *prometheus.Desc) {}

func (p *pendingStep) Collect(ch chan<- prometheus.Metric) {
	p.collector.send(ch, p.siteID, p.intervals)
}

// NewPriceHistory returns an empty PriceHistory.
func NewPriceHistory(opts HistoryOptions) *PriceHistory {
	if opts.Step <= 0 {
		opts.Step = time.Minute
	}
	h := &PriceHistory{
		opts:      opts,
		collector: newPricesCollector(deps{cfg: DefaultConfig(), now: time.Now}).(*pricesCollector),
		registry:  prometheus.NewRegistry(),
	}
	h.pending.collector = h.collector
	h.registry.MustRegister(&h.pending)
	return h
}

// Add adds the past intervals of a site. Intervals that are not past, and
// intervals that are fully outside the time range, are ignored.
func (h *PriceHistory) Add(siteID string, intervals []amber.Interval) {
	// The intervals of all channels that end at the same time are one step.
	byEnd := map[time.Time][]amber.Interval{}
	for _, i := range intervals {
		if i.Type != amber.TypeActual || i.Duration <= 0 {
			continue
		}
		if !h.opts.End.IsZero() && !i.Start().Before(h.opts.End) {
			continue
		}
		if !h.opts.Start.IsZero() && !i.EndTime.After(h.opts.Start) {
			continue
		}
		if i.Estimate == nil {
			// The API leaves the field out of a past interval. A scrape
			// exports it, so the history says that the price is final.
			final := false
			i.Estimate = &final
		}
		end := i.EndTime.UTC()
		if hasChannel(byEnd[end], i.ChannelType) {
			continue
		}
		byEnd[end] = append(byEnd[end], i)
	}
	for end, group := range byEnd {
		h.pending.siteID, h.pending.intervals = siteID, group
		families, err := h.registry.Gather()
		if err != nil {
			// The metrics are gauges with fixed label names, and each
			// channel is in the group once. Gather has nothing to refuse.
			panic(err)
		}
		h.steps = append(h.steps, historyStep{start: group[0].Start(), end: end, families: families})
	}
}

func hasChannel(intervals []amber.Interval, channelType string) bool {
	for _, i := range intervals {
		if i.ChannelType == channelType {
			return true
		}
	}
	return false
}

// Write writes all samples and the end marker of the format. Each metric
// family is written in one piece, with its samples in order of time.
func (h *PriceHistory) Write(w io.Writer) error {
	sort.SliceStable(h.steps, func(a, b int) bool { return h.steps[a].start.Before(h.steps[b].start) })

	help := map[string]string{}
	for _, s := range h.steps {
		for _, f := range s.families {
			help[f.GetName()] = f.GetHelp()
		}
	}
	names := make([]string, 0, len(help))
	for name := range help {
		names = append(names, name)
	}
	sort.Strings(names)

	bw := bufio.NewWriter(w)
	for _, name := range names {
		bw.WriteString("# TYPE " + name + " gauge\n")
		bw.WriteString("# HELP " + name + " " + escape(help[name]) + "\n")
		for _, s := range h.steps {
			for _, f := range s.families {
				if f.GetName() != name {
					continue
				}
				for _, m := range f.GetMetric() {
					h.writeSamples(bw, name, m, s)
				}
			}
		}
	}
	bw.WriteString("# EOF\n")
	return bw.Flush()
}

// writeSamples writes one sample of the metric for each step of time in the
// interval, from the start of the interval.
func (h *PriceHistory) writeSamples(bw *bufio.Writer, name string, m *dto.Metric, s historyStep) {
	var series strings.Builder
	series.WriteString(name)
	series.WriteString(`{instance="` + escape(h.opts.Instance) + `",job="` + escape(h.opts.Job) + `"`)
	for _, l := range m.GetLabel() {
		series.WriteString("," + l.GetName() + `="` + escape(l.GetValue()) + `"`)
	}
	series.WriteString("} ")
	series.WriteString(strconv.FormatFloat(m.GetGauge().GetValue(), 'g', -1, 64))
	series.WriteByte(' ')
	prefix := series.String()

	for t := s.start; t.Before(s.end); t = t.Add(h.opts.Step) {
		if !h.opts.Start.IsZero() && t.Before(h.opts.Start) {
			continue
		}
		if !h.opts.End.IsZero() && !t.Before(h.opts.End) {
			break
		}
		bw.WriteString(prefix)
		bw.WriteString(strconv.FormatFloat(float64(t.UnixMilli())/1000, 'f', -1, 64))
		bw.WriteByte('\n')
	}
}

// escape escapes a label value or a help text for the OpenMetrics format.
var escape = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace
