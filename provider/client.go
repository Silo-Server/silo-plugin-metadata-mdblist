package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// defaultBaseURL is the MDBList API root. Tests override it with SetBaseURL.
const defaultBaseURL = "https://api.mdblist.com"

// defaultUserAgent identifies the plugin to MDBList, which sits behind
// Cloudflare; main.go appends the build version.
const defaultUserAgent = "silo-plugin-metadata-mdblist"

const (
	defaultTimeout  = 15 * time.Second
	maxResponseBody = 1 << 20 // 1 MB for one title
	maxBatchBody    = 8 << 20 // 8 MB for up to maxBatchSize titles

	// requestBudget bounds one queued lookup from flush to answer, including
	// the limiter wait. It matches the host's 30s GetMetadata deadline.
	requestBudget = 30 * time.Second

	// defaultRetryAfter is the pause after a 429 that names no Retry-After.
	defaultRetryAfter = time.Minute
	// maxCooldown caps a pause taken from response headers, so a bogus reset
	// time cannot park the plugin for longer than a day.
	maxCooldown = 25 * time.Hour
)

// Client fetches titles from MDBList.
//
// Every failure MDBList can hand us — no key, a rejected key, an unknown
// title, an exhausted quota, an outage — resolves to (nil, nil) rather than an
// error. This plugin only ever fills fields another provider left empty, so a
// refresh must not fail on its account. The one error the client propagates is
// the caller's own cancelled or expired context, which the host needs to see.
//
// Lookups that arrive close together are answered with one batch request (see
// batch.go), which is what makes a large library fit a small daily quota.
type Client struct {
	httpClient *http.Client
	limiter    *rate.Limiter
	now        func() time.Time

	mu        sync.RWMutex
	apiKey    string
	baseURL   string
	userAgent string

	// cooldownUntil parks the client after MDBList says the quota is spent,
	// so the rest of a library scan costs no requests until it resets.
	quotaMu       sync.Mutex
	cooldownUntil time.Time

	batchMu     sync.Mutex
	batchWindow time.Duration
	queues      map[batchKey]*batchQueue
	batchLimits map[batchKey]int
}

// NewClient builds a client with no API key; Configure supplies one later.
func NewClient() *Client {
	return &Client{
		httpClient: &http.Client{Timeout: defaultTimeout},
		// MDBList allows 1000 reads per fixed five-minute window on every
		// tier, on top of the daily quota: 3.33 a second. Three a second with
		// a burst of three stays under it however the window boundaries fall.
		limiter:     rate.NewLimiter(3, 3),
		now:         time.Now,
		baseURL:     defaultBaseURL,
		userAgent:   defaultUserAgent,
		batchWindow: defaultBatchWindow,
		queues:      make(map[batchKey]*batchQueue),
		batchLimits: make(map[batchKey]int),
	}
}

// SetAPIKey replaces the key in place. Safe to call while requests are in
// flight; an empty key parks the client until one is configured. A new key
// also lifts any quota pause, since the quota belongs to the account.
func (c *Client) SetAPIKey(apiKey string) {
	apiKey = strings.TrimSpace(apiKey)

	c.mu.Lock()
	changed := apiKey != c.apiKey
	c.apiKey = apiKey
	c.mu.Unlock()

	if changed {
		c.quotaMu.Lock()
		c.cooldownUntil = time.Time{}
		c.quotaMu.Unlock()
	}
}

// SetBaseURL overrides the API root. Used by tests.
func (c *Client) SetBaseURL(baseURL string) {
	c.mu.Lock()
	c.baseURL = baseURL
	c.mu.Unlock()
}

// SetUserAgent sets the User-Agent sent with every request.
func (c *Client) SetUserAgent(userAgent string) {
	userAgent = strings.TrimSpace(userAgent)
	if userAgent == "" {
		userAgent = defaultUserAgent
	}
	c.mu.Lock()
	c.userAgent = userAgent
	c.mu.Unlock()
}

// SetBatchWindow sets how long the first lookup of a batch waits for others to
// join it. Zero sends every lookup on its own.
func (c *Client) SetBatchWindow(window time.Duration) {
	if window < 0 {
		window = 0
	}
	c.batchMu.Lock()
	c.batchWindow = window
	c.batchMu.Unlock()
}

func (c *Client) snapshot() (apiKey, baseURL, userAgent string) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.apiKey, c.baseURL, c.userAgent
}

// FetchMedia looks up one title.
//
// idProvider is "imdb" or "tmdb" and mediaType is "movie" or "show", matching
// MDBList's /{media_provider}/{media_type}/{media_id} route. A nil response
// with a nil error means "no data", which is a normal outcome.
func (c *Client) FetchMedia(ctx context.Context, idProvider, mediaType, mediaID string) (*mediaResponse, error) {
	if idProvider == "" || mediaType == "" || mediaID == "" {
		return nil, nil
	}

	if apiKey, _, _ := c.snapshot(); apiKey == "" {
		// Not configured yet. Idle rather than noisy.
		return nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c.coolingDown() {
		// The quota is spent; the pause was logged when it began.
		return nil, nil
	}

	answer := c.enqueue(batchKey{idProvider: idProvider, mediaType: mediaType}, mediaID)
	select {
	case response := <-answer:
		return response, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// fetchSingle is the one-title request, used when a lookup has no batch
// partners or batching is not available for its route. accepted reports
// whether MDBList took the request: it answered, or said it does not know the
// title, rather than refusing the request or failing.
func (c *Client) fetchSingle(ctx context.Context, key batchKey, mediaID string) (*mediaResponse, bool) {
	apiKey, baseURL, _ := c.snapshot()
	endpoint := fmt.Sprintf("%s/%s/%s/%s?apikey=%s&append_to_response=keyword",
		baseURL,
		url.PathEscape(key.idProvider),
		url.PathEscape(key.mediaType),
		url.PathEscape(mediaID),
		url.QueryEscape(apiKey),
	)
	label := key.idProvider + "/" + key.mediaType + "/" + mediaID

	status, body, ok := c.send(ctx, http.MethodGet, endpoint, nil, maxResponseBody, label)
	if !ok {
		return nil, false
	}

	switch {
	case status == http.StatusNotFound:
		return nil, true
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		logRejected(status)
		return nil, false
	case status >= 400:
		log.Printf("mdblist: HTTP %d for %s", status, label)
		return nil, false
	}

	var decoded mediaResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		// A shape we do not recognise is a plugin problem to fix, not a
		// refresh to fail.
		log.Printf("mdblist: decode %s: %v", label, err)
		return nil, false
	}
	if decoded.Error != "" {
		c.noteInBandError(decoded.Error)
		return nil, false
	}
	if decoded.Response != nil && !*decoded.Response {
		return nil, true
	}
	return &decoded, true
}

// noteInBandError handles an error MDBList sent in a 200 body. When it is a
// rate or quota limit, the client pauses exactly as it would for a 429.
func (c *Client) noteInBandError(message string) {
	if strings.Contains(strings.ToLower(message), "limit") {
		c.pauseForLimit(nil, message)
		return
	}
	log.Printf("mdblist: %s", message)
}

// logRejected reports a 401 or 403. A 403 can also be Cloudflare refusing the
// request rather than MDBList refusing the key, so the message names both.
func logRejected(status int) {
	if status == http.StatusUnauthorized {
		log.Printf("mdblist: API key rejected (HTTP 401); provider is contributing nothing")
		return
	}
	log.Printf("mdblist: request refused (HTTP 403): the API key was rejected or the request was blocked upstream; provider is contributing nothing")
}

// send runs one request through the quota pause and the rate limiter, and
// records what the response says about the remaining quota. ok is false when
// there is no response worth reading: paused, limiter refused, transport
// failure, or 429.
func (c *Client) send(ctx context.Context, method, endpoint string, payload []byte, limit int64, label string) (status int, body []byte, ok bool) {
	if c.coolingDown() {
		return 0, nil, false
	}
	if err := c.limiter.Wait(ctx); err != nil {
		log.Printf("mdblist: rate limiter declined %s: %v", label, err)
		return 0, nil, false
	}
	if c.coolingDown() {
		// The pause may have begun while this request waited its turn.
		return 0, nil, false
	}

	var reader io.Reader
	if payload != nil {
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		log.Printf("mdblist: build request for %s: %v", label, err)
		return 0, nil, false
	}
	_, _, userAgent := c.snapshot()
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// Network trouble or a timeout: MDBList is unreachable, which is not
		// a reason to fail the refresh.
		log.Printf("mdblist: %s unreachable: %s", label, redact(err.Error()))
		return 0, nil, false
	}
	defer resp.Body.Close()

	body, err = io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		log.Printf("mdblist: read %s: %s", label, redact(err.Error()))
		return 0, nil, false
	}

	c.observeQuota(resp.Header)
	if resp.StatusCode == http.StatusTooManyRequests {
		var envelope struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(body, &envelope)
		c.pauseForLimit(resp.Header, envelope.Error)
		return resp.StatusCode, nil, false
	}
	return resp.StatusCode, body, true
}

// redact strips the API key from a transport error, which embeds the URL.
func redact(message string) string {
	if index := strings.Index(message, "apikey="); index >= 0 {
		end := strings.IndexAny(message[index:], "&\" ")
		if end < 0 {
			return message[:index] + "apikey=REDACTED"
		}
		return message[:index] + "apikey=REDACTED" + message[index+end:]
	}
	return message
}

func (c *Client) coolingDown() bool {
	c.quotaMu.Lock()
	defer c.quotaMu.Unlock()
	return c.now().Before(c.cooldownUntil)
}

// pauseUntil parks the client until the given time, logging only when the
// pause begins or grows.
func (c *Client) pauseUntil(until time.Time, reason string) {
	now := c.now()
	if until.After(now.Add(maxCooldown)) {
		until = now.Add(maxCooldown)
	}

	c.quotaMu.Lock()
	extended := until.After(c.cooldownUntil)
	if extended {
		c.cooldownUntil = until
	}
	c.quotaMu.Unlock()

	if extended {
		log.Printf("mdblist: %s; pausing requests until %s", reason, until.UTC().Format(time.RFC3339))
	}
}

// observeQuota reads the daily-quota headers MDBList sends on every successful
// response and pauses once the last request has been spent, so the next one
// is not a guaranteed 429.
func (c *Client) observeQuota(header http.Header) {
	remaining, err := strconv.ParseInt(strings.TrimSpace(header.Get("X-RateLimit-Remaining")), 10, 64)
	if err != nil || remaining > 0 {
		return
	}
	reset, err := strconv.ParseInt(strings.TrimSpace(header.Get("X-RateLimit-Reset")), 10, 64)
	if err != nil || reset <= 0 {
		return
	}
	c.pauseUntil(time.Unix(reset, 0), "daily request quota used up")
}

// pauseForLimit pauses after MDBList reports a limit, by 429 or in a 200
// body. It honours Retry-After when there is one; MDBList sends it for both
// the daily quota and the five-minute burst limit and names which in the
// error message. header may be nil.
func (c *Client) pauseForLimit(header http.Header, message string) {
	reason := "rate limited"
	if message != "" {
		reason = "rate limited (" + message + ")"
	}

	now := c.now()
	if header != nil {
		if seconds, err := strconv.ParseInt(strings.TrimSpace(header.Get("Retry-After")), 10, 64); err == nil && seconds > 0 {
			c.pauseUntil(now.Add(time.Duration(seconds)*time.Second), reason)
			return
		}
	}
	if strings.Contains(strings.ToLower(message), "daily") {
		// The daily quota resets at 00:00 UTC.
		midnight := now.UTC().Truncate(24 * time.Hour).Add(24 * time.Hour)
		c.pauseUntil(midnight, reason)
		return
	}
	c.pauseUntil(now.Add(defaultRetryAfter), reason)
}
