package amber

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultBaseURL is the production API.
const DefaultBaseURL = "https://api.amber.com.au/v1"

// DateFormat is the layout of the date parameters of the API.
const DateFormat = "2006-01-02"

// defaultRateLimitWait is used when the API refuses a request for rate
// limiting but does not say when to try again.
const defaultRateLimitWait = time.Minute

// APIError is a non-2xx response from the API.
type APIError struct {
	StatusCode int
	Body       string
}

func (e *APIError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("amber api: status %d", e.StatusCode)
	}
	return fmt.Sprintf("amber api: status %d: %s", e.StatusCode, e.Body)
}

// RateLimitError means the request was not sent, or was refused, because the
// rate limit is used up.
type RateLimitError struct {
	// RetryAt is when the API accepts requests again.
	RetryAt time.Time
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("amber api: rate limited until %s", e.RetryAt.UTC().Format(time.RFC3339))
}

// RequestObserver is told about every request sent to the API. Endpoint is
// a short fixed name ("sites", "prices", "usage"). Status is the HTTP status
// code, or 0 when no response was received.
type RequestObserver func(endpoint string, status int, duration time.Duration)

// Client talks to the Amber API.
type Client struct {
	baseURL    string
	apiKey     string
	userAgent  string
	httpClient *http.Client
	observer   RequestObserver
	now        func() time.Time

	mu           sync.Mutex
	rateLimit    RateLimit
	blockedUntil time.Time
}

// Option configures a Client.
type Option func(*Client)

// WithBaseURL sets the API base URL.
func WithBaseURL(u string) Option {
	return func(c *Client) { c.baseURL = strings.TrimRight(u, "/") }
}

// WithHTTPClient sets the HTTP client.
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) { c.httpClient = h }
}

// WithUserAgent sets the User-Agent header.
func WithUserAgent(ua string) Option {
	return func(c *Client) { c.userAgent = ua }
}

// WithRequestObserver sets a callback that is told about every request.
func WithRequestObserver(o RequestObserver) Option {
	return func(c *Client) { c.observer = o }
}

// WithClock sets the time source. It is for tests.
func WithClock(now func() time.Time) Option {
	return func(c *Client) { c.now = now }
}

// NewClient returns a Client that authenticates with apiKey.
func NewClient(apiKey string, opts ...Option) *Client {
	c := &Client{
		baseURL:    DefaultBaseURL,
		apiKey:     apiKey,
		httpClient: &http.Client{Timeout: 30 * time.Second},
		now:        time.Now,
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// RateLimit returns the most recent rate limit state reported by the API.
func (c *Client) RateLimit() RateLimit {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rateLimit
}

// Sites returns all sites linked to the account.
func (c *Client) Sites(ctx context.Context) ([]Site, error) {
	var sites []Site
	if err := c.get(ctx, "sites", "/sites", nil, &sites); err != nil {
		return nil, err
	}
	return sites, nil
}

// CurrentPrices returns the current interval of each channel of a site,
// plus the past and forecast intervals that opts asks for.
func (c *Client) CurrentPrices(ctx context.Context, siteID string, opts PriceOptions) ([]Interval, error) {
	q := url.Values{}
	if opts.Next > 0 {
		q.Set("next", strconv.Itoa(opts.Next))
	}
	if opts.Previous > 0 {
		q.Set("previous", strconv.Itoa(opts.Previous))
	}
	if opts.Resolution > 0 {
		q.Set("resolution", strconv.Itoa(opts.Resolution))
	}
	var intervals []Interval
	path := "/sites/" + url.PathEscape(siteID) + "/prices/current"
	if err := c.get(ctx, "prices", path, q, &intervals); err != nil {
		return nil, err
	}
	return intervals, nil
}

// Prices returns the past prices of a site for each interval from the start
// day to the end day, both included, at the interval length of the site.
// The API accepts at most 7 days in one request.
func (c *Client) Prices(ctx context.Context, siteID string, start, end time.Time) ([]Interval, error) {
	q := url.Values{}
	q.Set("startDate", start.Format(DateFormat))
	q.Set("endDate", end.Format(DateFormat))
	var intervals []Interval
	path := "/sites/" + url.PathEscape(siteID) + "/prices"
	if err := c.get(ctx, "prices", path, q, &intervals); err != nil {
		return nil, err
	}
	return intervals, nil
}

// Usage returns the metered usage of a site for each interval from the
// start day to the end day, both included. The API accepts at most 7 days
// in one request and keeps 90 days of history.
func (c *Client) Usage(ctx context.Context, siteID string, start, end time.Time) ([]Usage, error) {
	q := url.Values{}
	q.Set("startDate", start.Format(DateFormat))
	q.Set("endDate", end.Format(DateFormat))
	var usage []Usage
	path := "/sites/" + url.PathEscape(siteID) + "/usage"
	if err := c.get(ctx, "usage", path, q, &usage); err != nil {
		return nil, err
	}
	return usage, nil
}

func (c *Client) get(ctx context.Context, endpoint, path string, query url.Values, out any) error {
	if retryAt, blocked := c.blocked(); blocked {
		return &RateLimitError{RetryAt: retryAt}
	}

	u := c.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "application/json")
	if c.userAgent != "" {
		req.Header.Set("User-Agent", c.userAgent)
	}

	begin := c.now()
	resp, err := c.httpClient.Do(req)
	if err != nil {
		c.observe(endpoint, 0, c.now().Sub(begin))
		// The URL in the error has no secret in it: the key travels in a header.
		return fmt.Errorf("amber api: %s: %w", endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()
	c.observe(endpoint, resp.StatusCode, c.now().Sub(begin))
	c.recordRateLimit(resp)

	if resp.StatusCode == http.StatusTooManyRequests {
		retryAt, _ := c.blocked()
		return &RateLimitError{RetryAt: retryAt}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return &APIError{StatusCode: resp.StatusCode, Body: strings.TrimSpace(string(body))}
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("amber api: %s: decode response: %w", endpoint, err)
	}
	return nil
}

func (c *Client) observe(endpoint string, status int, d time.Duration) {
	if c.observer != nil {
		c.observer(endpoint, status, d)
	}
}

// blocked reports whether requests must wait for the rate limit window to end.
func (c *Client) blocked() (time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.blockedUntil, c.now().Before(c.blockedUntil)
}

// recordRateLimit reads the RateLimit-* headers of a response. When the
// window has no requests left, or the API answered 429, later requests fail
// fast until the window ends.
func (c *Client) recordRateLimit(resp *http.Response) {
	now := c.now()
	limit, limitOK := headerInt(resp.Header, "RateLimit-Limit")
	remaining, remainingOK := headerInt(resp.Header, "RateLimit-Remaining")
	reset, resetOK := headerInt(resp.Header, "RateLimit-Reset")
	if !resetOK {
		reset, resetOK = headerInt(resp.Header, "Retry-After")
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if limitOK && remainingOK {
		c.rateLimit = RateLimit{Limit: limit, Remaining: remaining, Known: true}
		if resetOK {
			c.rateLimit.Reset = now.Add(time.Duration(reset) * time.Second)
		}
	}

	refused := resp.StatusCode == http.StatusTooManyRequests
	if refused && c.rateLimit.Known {
		c.rateLimit.Remaining = 0
	}
	if refused || (remainingOK && remaining <= 0) {
		wait := defaultRateLimitWait
		if resetOK && reset > 0 {
			wait = time.Duration(reset) * time.Second
		}
		c.blockedUntil = now.Add(wait)
	}
}

func headerInt(h http.Header, name string) (int, bool) {
	v := h.Get(name)
	if v == "" {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return 0, false
	}
	return n, true
}

// IsRateLimited reports whether err is a rate limit error.
func IsRateLimited(err error) bool {
	var rl *RateLimitError
	return errors.As(err, &rl)
}
