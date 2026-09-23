package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// defaultBaseURL is the MDBList API root. Tests override it with SetBaseURL.
const defaultBaseURL = "https://api.mdblist.com"

const (
	defaultTimeout  = 15 * time.Second
	maxResponseBody = 1 << 20 // 1 MB
)

// Client fetches single titles from MDBList.
//
// Every failure MDBList can hand us — no key, a rejected key, an unknown
// title, an exhausted quota, an outage — resolves to (nil, nil) rather than an
// error. This plugin only ever fills fields another provider left empty, so a
// refresh must not fail on its account. The one error the client propagates is
// the caller's own cancelled or expired context, which the host needs to see.
type Client struct {
	httpClient *http.Client
	limiter    *rate.Limiter

	mu      sync.RWMutex
	apiKey  string
	baseURL string
}

// NewClient builds a client with no API key; Configure supplies one later.
func NewClient() *Client {
	return &Client{
		httpClient: &http.Client{Timeout: defaultTimeout},
		// MDBList meters by requests per day (1000 on the free tier), not per
		// second. This limiter exists to keep a large library scan from
		// arriving as one burst, so it is deliberately conservative.
		limiter: rate.NewLimiter(5, 10),
		baseURL: defaultBaseURL,
	}
}

// SetAPIKey replaces the key in place. Safe to call while requests are in
// flight; an empty key parks the client until one is configured.
func (c *Client) SetAPIKey(apiKey string) {
	c.mu.Lock()
	c.apiKey = strings.TrimSpace(apiKey)
	c.mu.Unlock()
}

// SetBaseURL overrides the API root. Used by tests.
func (c *Client) SetBaseURL(baseURL string) {
	c.mu.Lock()
	c.baseURL = baseURL
	c.mu.Unlock()
}

func (c *Client) snapshot() (apiKey, baseURL string) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.apiKey, c.baseURL
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

	apiKey, baseURL := c.snapshot()
	if apiKey == "" {
		// Not configured yet. Idle rather than noisy.
		return nil, nil
	}

	if err := c.limiter.Wait(ctx); err != nil {
		// Either the context is done — the one error the host needs back — or
		// Wait refused up front because the delay would outlast the deadline,
		// in which case ctx.Err() is still nil and this title is simply
		// skipped like any other MDBList unavailability.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		log.Printf("mdblist: rate limiter declined %s/%s/%s: %v", idProvider, mediaType, mediaID, err)
		return nil, nil
	}

	endpoint := fmt.Sprintf("%s/%s/%s/%s?apikey=%s",
		baseURL,
		url.PathEscape(idProvider),
		url.PathEscape(mediaType),
		url.PathEscape(mediaID),
		url.QueryEscape(apiKey),
	)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("mdblist: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		// Network trouble or a timeout: MDBList is unreachable, which is not
		// a reason to fail the refresh.
		log.Printf("mdblist: %s/%s/%s unreachable: %v", idProvider, mediaType, mediaID, err)
		return nil, nil
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, nil
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		log.Printf("mdblist: API key rejected (HTTP %d); provider is contributing nothing", resp.StatusCode)
		return nil, nil
	case resp.StatusCode == http.StatusTooManyRequests:
		// Daily quota or burst limit. There is no useful retry inside one
		// refresh, so give up quietly and let the next refresh try again.
		log.Printf("mdblist: rate limited (HTTP 429); skipping %s/%s/%s", idProvider, mediaType, mediaID)
		return nil, nil
	case resp.StatusCode >= 400:
		log.Printf("mdblist: HTTP %d for %s/%s/%s", resp.StatusCode, idProvider, mediaType, mediaID)
		return nil, nil
	}

	var decoded mediaResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBody)).Decode(&decoded); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		// A shape we do not recognise is a plugin problem to fix, not a
		// refresh to fail.
		log.Printf("mdblist: decode %s/%s/%s: %v", idProvider, mediaType, mediaID, err)
		return nil, nil
	}

	// MDBList answers 200 with an error body when the daily quota is gone.
	if decoded.Error != "" {
		log.Printf("mdblist: %s", decoded.Error)
		return nil, nil
	}
	if decoded.Response != nil && !*decoded.Response {
		return nil, nil
	}

	return &decoded, nil
}
