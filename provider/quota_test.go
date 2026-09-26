package provider

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"
)

// fakeClock is a settable time source for the quota pause.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// fetchOne looks up one title. A spent quota is an expected answer in these
// tests and comes back as a nil response; any other error fails the test.
func fetchOne(t *testing.T, client *Client) *mediaResponse {
	t.Helper()
	response, err := client.FetchMedia(context.Background(), "imdb", "movie", "tt0073195")
	if err != nil && !errors.Is(err, ErrQuotaExhausted) {
		t.Fatalf("FetchMedia() returned error: %v", err)
	}
	return response
}

// TestQuota429PausesUntilRetryAfter is the "downgraded key" case: once MDBList
// says the quota is spent, the rest of a scan costs no requests until the
// time it names.
func TestQuota429PausesUntilRetryAfter(t *testing.T) {
	t.Parallel()

	var limited bool
	var mu sync.Mutex
	api := newScriptedAPI(t, func(w http.ResponseWriter, req recordedRequest) {
		mu.Lock()
		defer mu.Unlock()
		if !limited {
			limited = true
			w.Header().Set("Retry-After", "600")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":"API rate limit exceeded!"}`)
			return
		}
		_, _ = io.WriteString(w, movieBody("tt0073195", 578, 8.1))
	})
	clock := &fakeClock{now: time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)}
	client := api.client(0)
	client.now = clock.Now

	for i := range 4 {
		response, err := client.FetchMedia(context.Background(), "imdb", "movie", "tt0073195")
		if response != nil || !errors.Is(err, ErrQuotaExhausted) {
			t.Fatalf("lookup %d = (%+v, %v), want ErrQuotaExhausted", i, response, err)
		}
	}
	if got := len(api.seen()); got != 1 {
		t.Fatalf("made %d requests while paused, want only the one that was refused", got)
	}

	clock.Advance(601 * time.Second)
	if response := fetchOne(t, client); imdbRating(response) != 8.1 {
		t.Fatalf("after Retry-After got %+v, want the title", response)
	}
}

func TestDaily429WithoutRetryAfterPausesUntilUTCMidnight(t *testing.T) {
	t.Parallel()

	api := newScriptedAPI(t, func(w http.ResponseWriter, req recordedRequest) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":"Daily API limit exceeded!"}`)
	})
	clock := &fakeClock{now: time.Date(2026, 9, 25, 18, 30, 0, 0, time.UTC)}
	client := api.client(0)
	client.now = clock.Now

	fetchOne(t, client)

	client.quotaMu.Lock()
	until := client.cooldownUntil
	client.quotaMu.Unlock()
	if want := time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC); !until.Equal(want) {
		t.Fatalf("paused until %s, want %s", until, want)
	}
}

// TestLastRequestOfTheDayPausesPreemptively uses the quota headers on a
// successful answer, so the request after the last one is never sent.
func TestLastRequestOfTheDayPausesPreemptively(t *testing.T) {
	t.Parallel()

	clock := &fakeClock{now: time.Date(2026, 9, 25, 23, 0, 0, 0, time.UTC)}
	reset := time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC)
	api := newScriptedAPI(t, func(w http.ResponseWriter, req recordedRequest) {
		w.Header().Set("X-RateLimit-Limit", "1000")
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
		_, _ = io.WriteString(w, movieBody("tt0073195", 578, 8.1))
	})
	client := api.client(0)
	client.now = clock.Now

	if response := fetchOne(t, client); imdbRating(response) != 8.1 {
		t.Fatalf("last request of the day got %+v, want its answer", response)
	}
	fetchOne(t, client)
	if got := len(api.seen()); got != 1 {
		t.Fatalf("made %d requests, want none after the quota ran out", got)
	}

	clock.Advance(time.Hour + time.Second)
	fetchOne(t, client)
	if got := len(api.seen()); got != 2 {
		t.Fatalf("made %d requests, want requests to resume after the reset", got)
	}
}

// TestNewAPIKeyLiftsThePause: the quota belongs to the account, so pasting a
// different key must not inherit the old key's pause.
func TestNewAPIKeyLiftsThePause(t *testing.T) {
	t.Parallel()

	api := newScriptedAPI(t, func(w http.ResponseWriter, req recordedRequest) {
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	client := api.client(0)

	fetchOne(t, client)
	if !client.coolingDown() {
		t.Fatal("client is not paused after a 429")
	}

	client.SetAPIKey("test-key")
	if !client.coolingDown() {
		t.Fatal("re-saving the same key lifted the pause")
	}
	client.SetAPIKey("another-key")
	if client.coolingDown() {
		t.Fatal("a new key kept the old key's pause")
	}
}

func TestPauseIsCappedAgainstBogusHeaders(t *testing.T) {
	t.Parallel()

	api := newScriptedAPI(t, func(w http.ResponseWriter, req recordedRequest) {
		w.Header().Set("Retry-After", "99999999")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	clock := &fakeClock{now: time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)}
	client := api.client(0)
	client.now = clock.Now

	fetchOne(t, client)

	client.quotaMu.Lock()
	until := client.cooldownUntil
	client.quotaMu.Unlock()
	if limit := clock.Now().Add(maxCooldown); until.After(limit) {
		t.Fatalf("paused until %s, want at most %s", until, limit)
	}
}

func TestRedactRemovesTheAPIKey(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		`Get "https://api.mdblist.com/imdb/movie/tt1?apikey=secret&append_to_response=keyword": timeout`: `Get "https://api.mdblist.com/imdb/movie/tt1?apikey=REDACTED&append_to_response=keyword": timeout`,
		`Post "https://api.mdblist.com/imdb/movie/?apikey=secret": EOF`:                                  `Post "https://api.mdblist.com/imdb/movie/?apikey=REDACTED": EOF`,
		`dial tcp: connection refused`: `dial tcp: connection refused`,
	}
	for input, want := range tests {
		if got := redact(input); got != want {
			t.Fatalf("redact(%q) = %q, want %q", input, got, want)
		}
	}
}

// TestInBandQuotaErrorOnASingleLookupPauses covers the 200 error body on the
// single-title route.
func TestInBandQuotaErrorOnASingleLookupPauses(t *testing.T) {
	t.Parallel()

	api := newScriptedAPI(t, func(w http.ResponseWriter, req recordedRequest) {
		_, _ = io.WriteString(w, `{"error":"API request limit reached"}`)
	})
	client := api.client(0)

	fetchOne(t, client)
	fetchOne(t, client)

	if got := len(api.seen()); got != 1 {
		t.Fatalf("made %d requests, want none after the in-band quota error", got)
	}
}

func TestRetryAfterAcceptsAnHTTPDate(t *testing.T) {
	t.Parallel()

	clock := &fakeClock{now: time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)}
	resume := clock.Now().Add(2 * time.Hour)
	api := newScriptedAPI(t, func(w http.ResponseWriter, req recordedRequest) {
		w.Header().Set("Retry-After", resume.Format(http.TimeFormat))
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":"API rate limit exceeded!"}`)
	})
	client := api.client(0)
	client.now = clock.Now

	fetchOne(t, client)

	client.quotaMu.Lock()
	until := client.cooldownUntil
	client.quotaMu.Unlock()
	if !until.Equal(resume) {
		t.Fatalf("paused until %s, want %s from the HTTP-date Retry-After", until, resume)
	}
}
