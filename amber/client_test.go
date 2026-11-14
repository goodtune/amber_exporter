package amber

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func serve(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func TestSites(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sites" {
			t.Errorf("path = %q, want /sites", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			t.Errorf("Authorization = %q, want Bearer secret", got)
		}
		if got := r.Header.Get("User-Agent"); got != "amber_exporter/test" {
			t.Errorf("User-Agent = %q", got)
		}
		w.Header().Set("RateLimit-Limit", "50")
		w.Header().Set("RateLimit-Remaining", "49")
		w.Header().Set("RateLimit-Reset", "300")
		_, _ = w.Write(fixture(t, "sites.json"))
	})
	now := time.Date(2026, 11, 14, 3, 0, 0, 0, time.UTC)
	c := NewClient("secret", WithBaseURL(srv.URL+"/"), WithUserAgent("amber_exporter/test"),
		WithClock(func() time.Time { return now }))

	sites, err := c.Sites(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sites) != 2 {
		t.Fatalf("got %d sites, want 2", len(sites))
	}
	active := sites[1]
	if active.ID != "01F5A5CRKMZ5BCX9P1S4V990AM" || active.Status != StatusActive ||
		active.Network != "Ausgrid" || active.IntervalLength != 5 {
		t.Errorf("unexpected site: %+v", active)
	}
	if len(active.Channels) != 3 || active.Channels[2] != (Channel{Identifier: "B1", Type: ChannelFeedIn, Tariff: "EA029"}) {
		t.Errorf("unexpected channels: %+v", active.Channels)
	}
	if sites[0].ClosedOn != "2023-06-13" || active.ClosedOn != "" {
		t.Errorf("closedOn = %q and %q", sites[0].ClosedOn, active.ClosedOn)
	}

	rl := c.RateLimit()
	want := RateLimit{Limit: 50, Remaining: 49, Reset: now.Add(300 * time.Second), Known: true}
	if rl != want {
		t.Errorf("rate limit = %+v, want %+v", rl, want)
	}
}

func TestCurrentPrices(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sites/SITE/prices/current" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.URL.RawQuery; got != "next=4&resolution=30" {
			t.Errorf("query = %q, want next=4&resolution=30", got)
		}
		_, _ = w.Write(fixture(t, "forecast.json"))
	})
	c := NewClient("secret", WithBaseURL(srv.URL))

	intervals, err := c.CurrentPrices(context.Background(), "SITE", PriceOptions{Next: 4, Resolution: 30})
	if err != nil {
		t.Fatal(err)
	}
	if len(intervals) != 9 {
		t.Fatalf("got %d intervals, want 9", len(intervals))
	}

	cur := intervals[0]
	if cur.Type != TypeCurrent || cur.ChannelType != ChannelGeneral || cur.PerKwh != 20 {
		t.Errorf("unexpected current interval: %+v", cur)
	}
	if cur.Estimate == nil || !*cur.Estimate {
		t.Errorf("estimate = %v, want true", cur.Estimate)
	}
	if cur.TariffInformation == nil || cur.TariffInformation.Period != "offPeak" ||
		cur.TariffInformation.DemandWindow == nil || *cur.TariffInformation.DemandWindow {
		t.Errorf("unexpected tariff information: %+v", cur.TariffInformation)
	}
	wantStart := time.Date(2026, 11, 14, 3, 30, 0, 0, time.UTC)
	if !cur.Start().Equal(wantStart) {
		t.Errorf("Start() = %s, want %s", cur.Start(), wantStart)
	}

	fc := intervals[1]
	if fc.Type != TypeForecast || fc.AdvancedPrice == nil || *fc.AdvancedPrice != (AdvancedPrice{Low: 18, Predicted: 21, High: 25}) {
		t.Errorf("unexpected forecast interval: %+v", fc)
	}
	if last := intervals[2]; last.AdvancedPrice != nil {
		t.Errorf("advancedPrice = %+v, want nil", last.AdvancedPrice)
	}
}

func TestCurrentPricesWithoutOptions(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "" {
			t.Errorf("query = %q, want none", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte("[]"))
	})
	c := NewClient("secret", WithBaseURL(srv.URL))
	if _, err := c.CurrentPrices(context.Background(), "SITE", PriceOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestUsage(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sites/SITE/usage" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.URL.RawQuery; got != "endDate=2026-11-14&startDate=2026-11-08" {
			t.Errorf("query = %q", got)
		}
		_, _ = w.Write(fixture(t, "usage.json"))
	})
	c := NewClient("secret", WithBaseURL(srv.URL))

	start := time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)
	usage, err := c.Usage(context.Background(), "SITE", start, start.AddDate(0, 0, 6))
	if err != nil {
		t.Fatal(err)
	}
	if len(usage) != 7 {
		t.Fatalf("got %d usage intervals, want 7", len(usage))
	}
	feedIn := usage[5]
	if feedIn.ChannelType != ChannelFeedIn || feedIn.ChannelIdentifier != "B1" ||
		feedIn.Kwh != 2 || feedIn.Cost != -20 || feedIn.Quality != QualityEstimated || feedIn.Date != "2026-11-13" {
		t.Errorf("unexpected usage: %+v", feedIn)
	}
}

func TestAPIError(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"Unauthorized"}`, http.StatusUnauthorized)
	})
	c := NewClient("wrong", WithBaseURL(srv.URL))

	_, err := c.Sites(context.Background())
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *APIError", err)
	}
	if apiErr.StatusCode != http.StatusUnauthorized || apiErr.Body != `{"message":"Unauthorized"}` {
		t.Errorf("unexpected error: %+v", apiErr)
	}
}

func TestRequestObserver(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/sites" {
			_, _ = w.Write([]byte("[]"))
			return
		}
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	type call struct {
		endpoint string
		status   int
	}
	var calls []call
	c := NewClient("secret", WithBaseURL(srv.URL), WithRequestObserver(func(endpoint string, status int, _ time.Duration) {
		calls = append(calls, call{endpoint, status})
	}))

	_, _ = c.Sites(context.Background())
	_, _ = c.CurrentPrices(context.Background(), "SITE", PriceOptions{})
	srv.Close()
	_, _ = c.Usage(context.Background(), "SITE", time.Now(), time.Now())

	want := []call{{"sites", 200}, {"prices", 500}, {"usage", 0}}
	if len(calls) != len(want) {
		t.Fatalf("calls = %+v, want %+v", calls, want)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Errorf("call %d = %+v, want %+v", i, calls[i], want[i])
		}
	}
}

// When the API refuses a request with 429, the client must not send more
// requests until the window ends.
func TestRateLimitRefused(t *testing.T) {
	var requests atomic.Int32
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			w.Header().Set("RateLimit-Limit", "50")
			w.Header().Set("RateLimit-Remaining", "0")
			w.Header().Set("RateLimit-Reset", "120")
			http.Error(w, "slow down", http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte("[]"))
	})
	now := time.Date(2026, 11, 14, 3, 0, 0, 0, time.UTC)
	c := NewClient("secret", WithBaseURL(srv.URL), WithClock(func() time.Time { return now }))

	_, err := c.Sites(context.Background())
	var rl *RateLimitError
	if !errors.As(err, &rl) {
		t.Fatalf("err = %v, want *RateLimitError", err)
	}
	if want := now.Add(120 * time.Second); !rl.RetryAt.Equal(want) {
		t.Errorf("RetryAt = %s, want %s", rl.RetryAt, want)
	}

	now = now.Add(119 * time.Second)
	if _, err := c.Sites(context.Background()); !IsRateLimited(err) {
		t.Fatalf("err = %v, want rate limit error", err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("server saw %d requests while blocked, want 1", got)
	}

	now = now.Add(time.Second)
	if _, err := c.Sites(context.Background()); err != nil {
		t.Fatalf("after the window: %v", err)
	}
	if got := requests.Load(); got != 2 {
		t.Errorf("server saw %d requests, want 2", got)
	}
}

// A successful response that uses up the window also stops further requests.
func TestRateLimitExhausted(t *testing.T) {
	var requests atomic.Int32
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("RateLimit-Limit", "50")
		w.Header().Set("RateLimit-Remaining", "0")
		w.Header().Set("RateLimit-Reset", "30")
		_, _ = w.Write([]byte("[]"))
	})
	now := time.Date(2026, 11, 14, 3, 0, 0, 0, time.UTC)
	c := NewClient("secret", WithBaseURL(srv.URL), WithClock(func() time.Time { return now }))

	if _, err := c.Sites(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Sites(context.Background()); !IsRateLimited(err) {
		t.Fatalf("err = %v, want rate limit error", err)
	}
	if got := requests.Load(); got != 1 {
		t.Errorf("server saw %d requests, want 1", got)
	}
}

// A 429 with no headers still backs off.
func TestRateLimitRefusedWithoutHeaders(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "slow down", http.StatusTooManyRequests)
	})
	now := time.Date(2026, 11, 14, 3, 0, 0, 0, time.UTC)
	c := NewClient("secret", WithBaseURL(srv.URL), WithClock(func() time.Time { return now }))

	_, err := c.Sites(context.Background())
	var rl *RateLimitError
	if !errors.As(err, &rl) {
		t.Fatalf("err = %v, want *RateLimitError", err)
	}
	if want := now.Add(defaultRateLimitWait); !rl.RetryAt.Equal(want) {
		t.Errorf("RetryAt = %s, want %s", rl.RetryAt, want)
	}
}
