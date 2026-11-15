package collector

import (
	"bytes"
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/goodtune/amber_exporter/amber"
)

// historyInterval returns a past 5-minute interval that ends at end.
func historyInterval(channel string, end time.Time, perKwh float64) amber.Interval {
	return amber.Interval{
		Type:        amber.TypeActual,
		Duration:    5,
		PerKwh:      perKwh,
		SpotPerKwh:  11,
		Renewables:  25,
		EndTime:     end,
		StartTime:   end.Add(-5*time.Minute + time.Second),
		ChannelType: channel,
		Descriptor:  "low",
		SpikeStatus: "none",
	}
}

func writeHistory(t *testing.T, opts HistoryOptions, intervals []amber.Interval) string {
	t.Helper()
	var buf bytes.Buffer
	h := NewPriceHistory(opts)
	h.Add("SITE", intervals)
	if err := h.Write(&buf); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func TestPriceHistory(t *testing.T) {
	// The interval from 00:00 to 00:05 UTC, and the one after it.
	end := time.Date(2026, 11, 1, 0, 5, 0, 0, time.UTC)
	out := writeHistory(t, HistoryOptions{Job: "amber", Instance: "host:10062"}, []amber.Interval{
		historyInterval(amber.ChannelGeneral, end, 20),
		historyInterval(amber.ChannelFeedIn, end, -7),
		historyInterval(amber.ChannelGeneral, end.Add(5*time.Minute), 30),
	})

	if !strings.HasSuffix(out, "# EOF\n") {
		t.Errorf("the output does not end with # EOF")
	}
	t0 := end.Add(-5 * time.Minute).Unix()
	for _, want := range []string{
		"# TYPE amber_price_cents_per_kwh gauge\n",
		// One sample for each minute of the interval, from its start.
		`amber_price_cents_per_kwh{instance="host:10062",job="amber",channel_type="general",site_id="SITE"} 20 ` + itoa(t0) + "\n",
		`amber_price_cents_per_kwh{instance="host:10062",job="amber",channel_type="general",site_id="SITE"} 20 ` + itoa(t0+240) + "\n",
		// The next interval takes over at its own start.
		`amber_price_cents_per_kwh{instance="host:10062",job="amber",channel_type="general",site_id="SITE"} 30 ` + itoa(t0+300) + "\n",
		// The sign of the feedIn channel is turned around, as in a scrape.
		`amber_price_cents_per_kwh{instance="host:10062",job="amber",channel_type="feedIn",site_id="SITE"} 7 ` + itoa(t0) + "\n",
		// A past price is final.
		`amber_price_estimate{instance="host:10062",job="amber",channel_type="general",site_id="SITE"} 0 ` + itoa(t0) + "\n",
		`amber_price_descriptor{instance="host:10062",job="amber",channel_type="general",descriptor="low",site_id="SITE"} 1 ` + itoa(t0) + "\n",
		`amber_renewables_ratio{instance="host:10062",job="amber",site_id="SITE"} 0.25 ` + itoa(t0) + "\n",
		`amber_price_interval_end_timestamp_seconds{instance="host:10062",job="amber",site_id="SITE"} ` + strconv.FormatFloat(float64(end.Unix()), 'g', -1, 64) + " " + itoa(t0) + "\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the output does not contain %q", want)
		}
	}
	if n := strings.Count(out, "amber_price_cents_per_kwh{"); n != 15 {
		t.Errorf("got %d price samples, want 15", n)
	}
	if strings.Contains(out, itoa(t0+600)+"\n") {
		t.Errorf("a sample is at or after the end of the last interval")
	}
	// A metric family is written in one piece.
	if n := strings.Count(out, "# TYPE amber_price_cents_per_kwh "); n != 1 {
		t.Errorf("got %d TYPE lines of the price, want 1", n)
	}
}

func TestPriceHistoryRange(t *testing.T) {
	end := time.Date(2026, 11, 1, 0, 5, 0, 0, time.UTC)
	t0 := end.Add(-5 * time.Minute)
	out := writeHistory(t, HistoryOptions{
		Job: "amber", Instance: "i",
		Start: t0.Add(time.Minute),
		End:   t0.Add(3 * time.Minute),
	}, []amber.Interval{historyInterval(amber.ChannelGeneral, end, 20)})

	// Start is included, End is not.
	if n := strings.Count(out, "amber_price_cents_per_kwh{"); n != 2 {
		t.Errorf("got %d price samples, want 2", n)
	}
	for _, ts := range []int64{t0.Unix(), t0.Unix() + 180} {
		if strings.Contains(out, " "+itoa(ts)+"\n") {
			t.Errorf("the output has a sample at %d, outside the range", ts)
		}
	}
}

func TestPriceHistoryEscapesLabels(t *testing.T) {
	end := time.Date(2026, 11, 1, 0, 5, 0, 0, time.UTC)
	out := writeHistory(t, HistoryOptions{Job: `a"b\c`, Instance: "i"},
		[]amber.Interval{historyInterval(amber.ChannelGeneral, end, 20)})
	if !strings.Contains(out, `job="a\"b\\c"`) {
		t.Errorf("the job label is not escaped")
	}
}

type historyAPI struct {
	calls [][2]string
}

func (a *historyAPI) Prices(_ context.Context, _ string, start, end time.Time) ([]amber.Interval, error) {
	a.calls = append(a.calls, [2]string{start.Format(amber.DateFormat), end.Format(amber.DateFormat)})
	return []amber.Interval{{ChannelType: amber.ChannelGeneral}}, nil
}

func TestFetchPriceHistory(t *testing.T) {
	api := &historyAPI{}
	// 23:00 UTC on 31 October is 1 November in NEM time.
	start := time.Date(2026, 10, 31, 23, 0, 0, 0, time.UTC)
	end := time.Date(2026, 11, 10, 3, 0, 0, 0, time.UTC)
	intervals, err := FetchPriceHistory(context.Background(), api, "SITE", start, end)
	if err != nil {
		t.Fatal(err)
	}
	want := [][2]string{{"2026-11-01", "2026-11-07"}, {"2026-11-08", "2026-11-10"}}
	if len(api.calls) != len(want) {
		t.Fatalf("calls = %v, want %v", api.calls, want)
	}
	for i := range want {
		if api.calls[i] != want[i] {
			t.Errorf("call %d = %v, want %v", i, api.calls[i], want[i])
		}
	}
	if len(intervals) != 2 {
		t.Errorf("got %d intervals, want 2", len(intervals))
	}
}

func itoa(v int64) string {
	return strconv.FormatInt(v, 10)
}
