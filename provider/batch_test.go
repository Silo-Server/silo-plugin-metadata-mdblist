package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// recordedRequest is one request the scripted API saw.
type recordedRequest struct {
	method    string
	path      string
	query     string
	userAgent string
	ids       []any
	appended  []string
}

// scriptedAPI is a fake MDBList whose answer is computed per request, for
// tests that need the single and batch routes to answer differently.
type scriptedAPI struct {
	server *httptest.Server

	mu       sync.Mutex
	requests []recordedRequest
	handle   func(w http.ResponseWriter, req recordedRequest)
}

func newScriptedAPI(t *testing.T, handle func(w http.ResponseWriter, req recordedRequest)) *scriptedAPI {
	t.Helper()

	api := &scriptedAPI{handle: handle}
	api.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorded := recordedRequest{
			method:    r.Method,
			path:      r.URL.Path,
			query:     r.URL.RawQuery,
			userAgent: r.Header.Get("User-Agent"),
		}
		if r.Method == http.MethodPost {
			var body struct {
				IDs              []any    `json:"ids"`
				AppendToResponse []string `json:"append_to_response"`
			}
			data, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(data, &body)
			recorded.ids = body.IDs
			recorded.appended = body.AppendToResponse
		}

		api.mu.Lock()
		api.requests = append(api.requests, recorded)
		api.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		api.handle(w, recorded)
	}))
	t.Cleanup(api.server.Close)
	return api
}

func (a *scriptedAPI) seen() []recordedRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]recordedRequest(nil), a.requests...)
}

func (a *scriptedAPI) client(window time.Duration) *Client {
	client := NewClient()
	client.SetBaseURL(a.server.URL)
	client.SetAPIKey("test-key")
	client.SetBatchWindow(window)
	return client
}

// movieBody is a minimal media answer carrying its own IDs, the way both the
// single and the batch endpoint answer.
func movieBody(imdbID string, tmdbID int, imdbRating float64) string {
	return fmt.Sprintf(`{"type":"movie","ids":{"imdb":%q,"tmdb":%d},"ratings":[{"source":"imdb","value":%v}]}`,
		imdbID, tmdbID, imdbRating)
}

// fetchAll runs lookups concurrently, as the host's match workers do.
func fetchAll(t *testing.T, client *Client, idProvider string, ids ...string) map[string]*mediaResponse {
	t.Helper()

	var mu sync.Mutex
	answers := make(map[string]*mediaResponse, len(ids))
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			response, err := client.FetchMedia(context.Background(), idProvider, "movie", id)
			if err != nil {
				t.Errorf("FetchMedia(%s) returned error: %v", id, err)
				return
			}
			mu.Lock()
			answers[id] = response
			mu.Unlock()
		}()
	}
	wg.Wait()
	return answers
}

func imdbRating(response *mediaResponse) float64 {
	if response == nil {
		return -1
	}
	for _, entry := range response.Ratings {
		if entry.Source == "imdb" && entry.Value != nil {
			return *entry.Value
		}
	}
	return 0
}

// TestConcurrentLookupsShareOneBatchRequest is the quota win: lookups that
// arrive together cost one request, and each gets its own title back.
func TestConcurrentLookupsShareOneBatchRequest(t *testing.T) {
	t.Parallel()

	api := newScriptedAPI(t, func(w http.ResponseWriter, req recordedRequest) {
		if req.method != http.MethodPost {
			w.WriteHeader(http.StatusTeapot)
			return
		}
		// Answer out of order and include a title nobody asked for, to prove
		// correlation is by ID rather than by position.
		_, _ = io.WriteString(w, "["+
			movieBody("tt0000003", 3, 3.3)+","+
			movieBody("tt9999999", 9, 9.9)+","+
			movieBody("tt0000001", 1, 1.1)+"]")
	})
	client := api.client(time.Second)

	answers := fetchAll(t, client, "imdb", "tt0000001", "tt0000002", "tt0000003")

	requests := api.seen()
	if len(requests) != 1 {
		t.Fatalf("made %d requests %+v, want one batch", len(requests), requests)
	}
	batch := requests[0]
	if batch.path != "/imdb/movie/" {
		t.Fatalf("batch path = %q, want /imdb/movie/", batch.path)
	}
	if !strings.Contains(batch.query, "apikey=test-key") {
		t.Fatalf("batch query = %q, want the api key", batch.query)
	}
	if len(batch.ids) != 3 {
		t.Fatalf("batch ids = %v, want the three requested", batch.ids)
	}
	// Keywords are not used, so none are requested.
	if len(batch.appended) != 0 {
		t.Fatalf("append_to_response = %v, want none", batch.appended)
	}

	if got := imdbRating(answers["tt0000001"]); got != 1.1 {
		t.Fatalf("tt0000001 got rating %v, want its own 1.1", got)
	}
	if got := imdbRating(answers["tt0000003"]); got != 3.3 {
		t.Fatalf("tt0000003 got rating %v, want its own 3.3", got)
	}
	if answers["tt0000002"] != nil {
		t.Fatalf("tt0000002 = %+v, want no data: MDBList did not answer it", answers["tt0000002"])
	}
}

func TestBatchSendsTMDBIDsAsNumbers(t *testing.T) {
	t.Parallel()

	api := newScriptedAPI(t, func(w http.ResponseWriter, req recordedRequest) {
		_, _ = io.WriteString(w, "["+movieBody("tt0000001", 578, 8.1)+","+movieBody("tt0000002", 1396, 9.5)+"]")
	})
	client := api.client(time.Second)

	answers := fetchAll(t, client, "tmdb", "578", "1396")

	requests := api.seen()
	if len(requests) != 1 {
		t.Fatalf("made %d requests, want one batch", len(requests))
	}
	for _, id := range requests[0].ids {
		if _, ok := id.(float64); !ok {
			t.Fatalf("batch id %v is %T, want a JSON number", id, id)
		}
	}
	if got := imdbRating(answers["578"]); got != 8.1 {
		t.Fatalf("578 got rating %v, want 8.1", got)
	}
	if got := imdbRating(answers["1396"]); got != 9.5 {
		t.Fatalf("1396 got rating %v, want 9.5", got)
	}
}

// TestLoneLookupUsesTheSingleRoute keeps the common case — one refresh at a
// time — on the endpoint the fixture was captured from.
func TestLoneLookupUsesTheSingleRoute(t *testing.T) {
	t.Parallel()

	api := newScriptedAPI(t, func(w http.ResponseWriter, req recordedRequest) {
		_, _ = io.WriteString(w, movieBody("tt0073195", 578, 8.1))
	})
	client := api.client(10 * time.Millisecond)
	client.SetUserAgent("silo-plugin-metadata-mdblist/1.2.3")

	answers := fetchAll(t, client, "imdb", "tt0073195")

	requests := api.seen()
	if len(requests) != 1 || requests[0].method != http.MethodGet {
		t.Fatalf("requests = %+v, want one GET", requests)
	}
	if got, want := requests[0].path, "/imdb/movie/tt0073195"; got != want {
		t.Fatalf("path = %q, want %q", got, want)
	}
	if strings.Contains(requests[0].query, "append_to_response") {
		t.Fatalf("query = %q, want no append_to_response", requests[0].query)
	}
	if got, want := requests[0].userAgent, "silo-plugin-metadata-mdblist/1.2.3"; got != want {
		t.Fatalf("User-Agent = %q, want %q", got, want)
	}
	if got := imdbRating(answers["tt0073195"]); got != 8.1 {
		t.Fatalf("rating = %v, want 8.1", got)
	}
}

func TestDuplicateLookupsShareOneSlot(t *testing.T) {
	t.Parallel()

	api := newScriptedAPI(t, func(w http.ResponseWriter, req recordedRequest) {
		_, _ = io.WriteString(w, movieBody("tt0073195", 578, 8.1))
	})
	client := api.client(100 * time.Millisecond)

	var wg sync.WaitGroup
	results := make([]*mediaResponse, 3)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], _ = client.FetchMedia(context.Background(), "imdb", "movie", "tt0073195")
		}()
	}
	wg.Wait()

	if requests := api.seen(); len(requests) != 1 || requests[0].method != http.MethodGet {
		t.Fatalf("requests = %+v, want one GET for one distinct ID", requests)
	}
	for i, response := range results {
		if imdbRating(response) != 8.1 {
			t.Fatalf("caller %d got %+v, want the shared answer", i, response)
		}
	}
}

// TestRejectedBatchFallsBackAndRemembers covers an account or ID form the
// batch endpoint refuses: every lookup is still answered, and the lower limit
// sticks so the next scan does not pay for the refusal again.
func TestRejectedBatchFallsBackAndRemembers(t *testing.T) {
	t.Parallel()

	api := newScriptedAPI(t, func(w http.ResponseWriter, req recordedRequest) {
		if req.method == http.MethodPost {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"too many ids"}`)
			return
		}
		id := req.path[strings.LastIndex(req.path, "/")+1:]
		number, _ := strconv.Atoi(strings.TrimPrefix(id, "tt"))
		_, _ = io.WriteString(w, movieBody(id, number, float64(number)))
	})
	client := api.client(time.Second)

	answers := fetchAll(t, client, "imdb", "tt0000001", "tt0000002")

	for _, id := range []string{"tt0000001", "tt0000002"} {
		if answers[id] == nil {
			t.Fatalf("%s got no answer after the batch was refused", id)
		}
	}
	var posts, gets int
	for _, req := range api.seen() {
		if req.method == http.MethodPost {
			posts++
		} else {
			gets++
		}
	}
	if posts != 1 || gets != 2 {
		t.Fatalf("made %d POST and %d GET, want one refused batch then two singles", posts, gets)
	}

	client.batchMu.Lock()
	limit := client.batchLimitLocked(batchKey{idProvider: "imdb", mediaType: "movie"})
	client.batchMu.Unlock()
	if limit != 1 {
		t.Fatalf("batch limit = %d, want 1 after a two-ID batch was refused", limit)
	}
}

func TestFailedBatchAnswersNoDataWithoutRetrying(t *testing.T) {
	t.Parallel()

	api := newScriptedAPI(t, func(w http.ResponseWriter, req recordedRequest) {
		w.WriteHeader(http.StatusBadGateway)
	})
	client := api.client(time.Second)

	answers := fetchAll(t, client, "imdb", "tt0000001", "tt0000002", "tt0000003")

	if requests := api.seen(); len(requests) != 1 {
		t.Fatalf("made %d requests, want the one failed batch and no retries during an outage", len(requests))
	}
	for id, response := range answers {
		if response != nil {
			t.Fatalf("%s = %+v, want no data", id, response)
		}
	}
}

// TestFullQueueSendsBeforeTheWindowEnds keeps a large burst from waiting out
// the window once a batch is as big as it can be.
func TestFullQueueSendsBeforeTheWindowEnds(t *testing.T) {
	t.Parallel()

	api := newScriptedAPI(t, func(w http.ResponseWriter, req recordedRequest) {
		_, _ = io.WriteString(w, "[]")
	})
	client := api.client(time.Hour)
	client.lowerBatchLimit(batchKey{idProvider: "imdb", mediaType: "movie"}, 2)

	done := make(chan struct{})
	go func() {
		fetchAll(t, client, "imdb", "tt0000001", "tt0000002")
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a full batch waited for the window")
	}
}

// TestCallerCancellationDoesNotStrandOthers checks that one host call giving
// up returns promptly and leaves its batch partners with their answers.
func TestCallerCancellationDoesNotStrandOthers(t *testing.T) {
	t.Parallel()

	api := newScriptedAPI(t, func(w http.ResponseWriter, req recordedRequest) {
		_, _ = io.WriteString(w, "["+movieBody("tt0000001", 1, 1.1)+","+movieBody("tt0000002", 2, 2.2)+"]")
	})
	client := api.client(200 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	cancelled := make(chan error, 1)
	go func() {
		_, err := client.FetchMedia(ctx, "imdb", "movie", "tt0000001")
		cancelled <- err
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()

	if err := <-cancelled; err != context.Canceled {
		t.Fatalf("cancelled caller got %v, want context.Canceled", err)
	}

	response, err := client.FetchMedia(context.Background(), "imdb", "movie", "tt0000002")
	if err != nil || imdbRating(response) != 2.2 {
		t.Fatalf("partner got (%+v, %v), want its answer", response, err)
	}
}

// TestBatchSizeRefusalLowersTheLimit: an account that allows at most two IDs
// per batch refuses a batch of four, and both halves then go through. That is
// the evidence the refusal was about size, so the route remembers two.
func TestBatchSizeRefusalLowersTheLimit(t *testing.T) {
	t.Parallel()

	api := newScriptedAPI(t, func(w http.ResponseWriter, req recordedRequest) {
		if req.method == http.MethodPost && len(req.ids) > 2 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"too many ids"}`)
			return
		}
		var elements []string
		for _, id := range req.ids {
			text := id.(string)
			number, _ := strconv.Atoi(strings.TrimPrefix(text, "tt"))
			elements = append(elements, movieBody(text, number, float64(number)))
		}
		_, _ = io.WriteString(w, "["+strings.Join(elements, ",")+"]")
	})
	client := api.client(time.Second)

	answers := fetchAll(t, client, "imdb", "tt0000001", "tt0000002", "tt0000003", "tt0000004")

	for id, response := range answers {
		if response == nil {
			t.Fatalf("%s got no answer", id)
		}
	}
	if got := len(api.seen()); got != 3 {
		t.Fatalf("made %d requests, want the refused batch of four and two batches of two", got)
	}
	client.batchMu.Lock()
	limit := client.batchLimitLocked(batchKey{idProvider: "imdb", mediaType: "movie"})
	client.batchMu.Unlock()
	if limit != 2 {
		t.Fatalf("batch limit = %d, want 2", limit)
	}
}

// TestOneBadIDDoesNotTurnBatchingOff: MDBList refuses any batch containing
// one malformed ID. Every good ID must still be answered, and the route must
// keep batching, because the refusals were never about size.
func TestOneBadIDDoesNotTurnBatchingOff(t *testing.T) {
	t.Parallel()

	const bad = "tt0000004"
	api := newScriptedAPI(t, func(w http.ResponseWriter, req recordedRequest) {
		if req.method == http.MethodGet {
			if strings.HasSuffix(req.path, bad) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			id := req.path[strings.LastIndex(req.path, "/")+1:]
			_, _ = io.WriteString(w, movieBody(id, 1, 1))
			return
		}
		var elements []string
		for _, id := range req.ids {
			if id == bad {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			elements = append(elements, movieBody(id.(string), 1, 1))
		}
		_, _ = io.WriteString(w, "["+strings.Join(elements, ",")+"]")
	})
	client := api.client(time.Second)

	answers := fetchAll(t, client, "imdb", "tt0000001", "tt0000002", "tt0000003", bad)

	for _, id := range []string{"tt0000001", "tt0000002", "tt0000003"} {
		if answers[id] == nil {
			t.Fatalf("%s got no answer", id)
		}
	}
	if answers[bad] != nil {
		t.Fatalf("%s = %+v, want no data", bad, answers[bad])
	}
	client.batchMu.Lock()
	limit := client.batchLimitLocked(batchKey{idProvider: "imdb", mediaType: "movie"})
	client.batchMu.Unlock()
	if limit != maxBatchSize {
		t.Fatalf("batch limit = %d, want %d: one bad ID must not shrink the route", limit, maxBatchSize)
	}
}

// TestInBandQuotaErrorOnABatchPausesWithoutSplitting: MDBList can report the
// daily quota as a 200 error object. Splitting that batch would spend a
// request per half for nothing.
func TestInBandQuotaErrorOnABatchPausesWithoutSplitting(t *testing.T) {
	t.Parallel()

	api := newScriptedAPI(t, func(w http.ResponseWriter, req recordedRequest) {
		_, _ = io.WriteString(w, `{"error":"Daily API limit exceeded!"}`)
	})
	client := api.client(time.Second)

	fetchAll(t, client, "imdb", "tt0000001", "tt0000002", "tt0000003")

	if got := len(api.seen()); got != 1 {
		t.Fatalf("made %d requests, want only the one batch", got)
	}
	if !client.coolingDown() {
		t.Fatal("client is not paused after an in-band quota error")
	}
	client.batchMu.Lock()
	limit := client.batchLimitLocked(batchKey{idProvider: "imdb", mediaType: "movie"})
	client.batchMu.Unlock()
	if limit != maxBatchSize {
		t.Fatalf("batch limit = %d, want %d", limit, maxBatchSize)
	}
}

func TestBatch429PausesTheClient(t *testing.T) {
	t.Parallel()

	api := newScriptedAPI(t, func(w http.ResponseWriter, req recordedRequest) {
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":"API rate limit exceeded!"}`)
	})
	client := api.client(time.Second)

	fetchAll(t, client, "imdb", "tt0000001", "tt0000002")
	fetchAll(t, client, "imdb", "tt0000003")

	if got := len(api.seen()); got != 1 {
		t.Fatalf("made %d requests, want the one refused batch and nothing while paused", got)
	}
}
