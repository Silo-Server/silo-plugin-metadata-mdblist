package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Batching.
//
// The host calls GetMetadata once per item, but runs several match workers at
// once (eight by default), and a host running a bulk enrichment pass can run
// many more. MDBList's POST /{media_provider}/{media_type}/ answers a list of
// IDs in one request, so lookups for the same route that arrive within
// batchWindow of each other are sent together. A lookup that finds no partner
// goes out as the ordinary single-title GET, so an idle library pays only the
// window's latency.
//
// Nothing in MDBList's published schema states the batch endpoint's ID limit.
// Its ratings batch allows 10 IDs per request for regular accounts and 100 for
// supporters, so the client starts at maxBatchSize. When MDBList refuses a
// batch, the client retries it in halves; if both halves go through, the
// refusal was about size and the route keeps the smaller limit.

const (
	// defaultBatchWindow is how long the first lookup waits for company.
	defaultBatchWindow = 250 * time.Millisecond
	// maxBatchSize is the most IDs sent in one batch request.
	maxBatchSize = 100
)

// batchKey is one MDBList route: IDs are only batchable with IDs of the same
// provider and media type.
type batchKey struct {
	idProvider string
	mediaType  string
}

// lookupResult is one lookup's answer: a title, no title (both nil), or the
// reason MDBList could not answer.
type lookupResult struct {
	response *mediaResponse
	err      error
}

// batchQueue collects the lookups for one route until it is flushed.
type batchQueue struct {
	ids     []string
	waiters map[string][]chan lookupResult
	timer   *time.Timer
}

// batchOutcome classifies a batch request.
type batchOutcome int

const (
	// batchAnswered means the returned map is MDBList's answer; an ID absent
	// from it is a title MDBList does not know.
	batchAnswered batchOutcome = iota
	// batchRejected means MDBList refused the request itself (too many IDs,
	// an ID form the endpoint does not take), so smaller requests may work.
	batchRejected
	// batchFailed means nothing more can be learned now: outage, quota,
	// rejected key. The accompanying error says which.
	batchFailed
)

// enqueue adds a lookup to its route's queue and returns the channel its
// answer arrives on. Every channel receives exactly one value.
func (c *Client) enqueue(key batchKey, mediaID string) <-chan lookupResult {
	answer := make(chan lookupResult, 1)

	c.batchMu.Lock()
	defer c.batchMu.Unlock()

	if c.batchWindow <= 0 {
		go c.run(key, []string{mediaID}, map[string][]chan lookupResult{mediaID: {answer}})
		return answer
	}

	queue := c.queues[key]
	if queue == nil {
		queue = &batchQueue{waiters: make(map[string][]chan lookupResult)}
		c.queues[key] = queue
		queue.timer = time.AfterFunc(c.batchWindow, func() { c.flush(key, queue) })
	}
	if _, queued := queue.waiters[mediaID]; !queued {
		queue.ids = append(queue.ids, mediaID)
	}
	queue.waiters[mediaID] = append(queue.waiters[mediaID], answer)

	if len(queue.ids) >= c.batchLimitLocked(key) {
		// Full: send now rather than waiting out the window.
		queue.timer.Stop()
		delete(c.queues, key)
		go c.run(key, queue.ids, queue.waiters)
	}
	return answer
}

// flush sends a queue whose window has ended, unless it was already sent
// because it filled up.
func (c *Client) flush(key batchKey, queue *batchQueue) {
	c.batchMu.Lock()
	if c.queues[key] != queue {
		c.batchMu.Unlock()
		return
	}
	delete(c.queues, key)
	c.batchMu.Unlock()

	c.run(key, queue.ids, queue.waiters)
}

// run resolves one flushed queue and answers every waiter. It runs on its own
// context: the lookups in a batch belong to different host calls, and one of
// them giving up must not cancel the others.
func (c *Client) run(key batchKey, ids []string, waiters map[string][]chan lookupResult) {
	ctx, cancel := context.WithTimeout(context.Background(), requestBudget)
	defer cancel()

	results, _ := c.resolve(ctx, key, ids)
	for id, channels := range waiters {
		for _, channel := range channels {
			channel <- results[id]
		}
	}
}

// resolve answers a set of IDs for one route, as one batch where it can. An ID
// absent from results is a title MDBList does not know. accepted reports
// whether MDBList took the request as sent (answered it, or for a single
// title, said it does not know it), as opposed to refusing it or failing.
func (c *Client) resolve(ctx context.Context, key batchKey, ids []string) (results map[string]lookupResult, accepted bool) {
	if len(ids) == 1 {
		response, accepted, err := c.fetchSingle(ctx, key, ids[0])
		return map[string]lookupResult{ids[0]: {response: response, err: err}}, accepted
	}

	// The limit may have dropped after this queue filled.
	c.batchMu.Lock()
	limit := c.batchLimitLocked(key)
	c.batchMu.Unlock()
	if len(ids) > limit {
		results = make(map[string]lookupResult, len(ids))
		accepted = true
		for start := 0; start < len(ids); start += limit {
			end := min(start+limit, len(ids))
			chunk, ok := c.resolve(ctx, key, ids[start:end])
			for id, result := range chunk {
				results[id] = result
			}
			accepted = accepted && ok
		}
		return results, accepted
	}

	answers, outcome, err := c.fetchBatch(ctx, key, ids)
	switch outcome {
	case batchAnswered:
		results = make(map[string]lookupResult, len(answers))
		for id, response := range answers {
			results[id] = lookupResult{response: response}
		}
		return results, true
	case batchFailed:
		results = make(map[string]lookupResult, len(ids))
		for _, id := range ids {
			results[id] = lookupResult{err: err}
		}
		return results, false
	}

	// MDBList refused the batch itself: try the two halves, which at one ID
	// each is the single GET. Only when both halves then go through is the
	// refusal about size, so only then is the smaller limit remembered. A
	// refusal caused by one bad ID keeps failing in whichever half holds it,
	// and must not turn batching off for every later lookup on the route.
	half := len(ids) / 2
	left, leftAccepted := c.resolve(ctx, key, ids[:half])
	right, rightAccepted := c.resolve(ctx, key, ids[half:])
	for id, result := range right {
		left[id] = result
	}
	if leftAccepted && rightAccepted {
		c.lowerBatchLimit(key, len(ids)-half)
	}
	return left, false
}

// fetchBatch sends one batch request. err is set exactly when the outcome is
// batchFailed.
func (c *Client) fetchBatch(ctx context.Context, key batchKey, ids []string) (map[string]*mediaResponse, batchOutcome, error) {
	apiKey, baseURL, _ := c.snapshot()
	endpoint := fmt.Sprintf("%s/%s/%s/?apikey=%s",
		baseURL,
		url.PathEscape(key.idProvider),
		url.PathEscape(key.mediaType),
		url.QueryEscape(apiKey),
	)
	label := fmt.Sprintf("batch of %d %s/%s", len(ids), key.idProvider, key.mediaType)

	payload, err := json.Marshal(struct {
		IDs []any `json:"ids"`
	}{IDs: batchIDs(key, ids)})
	if err != nil {
		log.Printf("mdblist: encode %s: %v", label, err)
		return nil, batchFailed, fmt.Errorf("%w: encode batch: %v", ErrUnavailable, err)
	}

	status, body, err := c.send(ctx, http.MethodPost, endpoint, payload, maxBatchBody, label)
	if err != nil {
		return nil, batchFailed, err
	}

	switch status {
	case http.StatusBadRequest, http.StatusNotFound, http.StatusMethodNotAllowed,
		http.StatusRequestEntityTooLarge, http.StatusRequestURITooLong, http.StatusUnprocessableEntity:
		// The request itself was refused: too many IDs, an ID form the
		// endpoint does not take, or no batch route for this account.
		log.Printf("mdblist: HTTP %d for %s; retrying in smaller requests", status, label)
		return nil, batchRejected, nil
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, batchFailed, rejected(status)
	}
	if status >= 400 {
		log.Printf("mdblist: HTTP %d for %s", status, label)
		return nil, batchFailed, fmt.Errorf("%w: HTTP %d", ErrUnavailable, status)
	}

	var elements []json.RawMessage
	if err := json.Unmarshal(body, &elements); err != nil {
		// Not the documented array. MDBList reports some failures, the
		// daily quota among them, as a 200 error object; that is not a size
		// problem, and splitting it would spend requests for nothing.
		var envelope mediaResponse
		if json.Unmarshal(body, &envelope) == nil && envelope.Error != "" {
			return nil, batchFailed, c.noteInBandError(envelope.Error, ErrUnavailable)
		}
		log.Printf("mdblist: unexpected answer to %s: %v; retrying in smaller requests", label, err)
		return nil, batchRejected, nil
	}

	requested := make(map[string]bool, len(ids))
	for _, id := range ids {
		requested[id] = true
	}

	answers := make(map[string]*mediaResponse, len(ids))
	for _, element := range elements {
		var decoded mediaResponse
		if err := json.Unmarshal(element, &decoded); err != nil {
			continue
		}
		if decoded.Error != "" || (decoded.Response != nil && !*decoded.Response) {
			continue
		}
		id := answeredID(key, &decoded)
		if !requested[id] {
			// An answer that cannot be tied to a requested ID is dropped:
			// attaching it by position would risk another title's ratings.
			continue
		}
		answers[id] = &decoded
	}
	return answers, batchAnswered, nil
}

// batchIDs encodes IDs the way the endpoint's schema types them: TMDB IDs as
// numbers, everything else as strings.
func batchIDs(key batchKey, ids []string) []any {
	encoded := make([]any, 0, len(ids))
	for _, id := range ids {
		if key.idProvider == "tmdb" {
			if number, err := strconv.ParseInt(id, 10, 64); err == nil {
				encoded = append(encoded, number)
				continue
			}
		}
		encoded = append(encoded, id)
	}
	return encoded
}

// answeredID is the requested-ID form of a batch element's own ID for the
// batch's provider. lookupID lowercases IMDb IDs, so this does too.
func answeredID(key batchKey, response *mediaResponse) string {
	switch key.idProvider {
	case "imdb":
		return strings.ToLower(strings.TrimSpace(response.IDs.IMDB))
	case "tmdb":
		return string(response.IDs.TMDB)
	default:
		return ""
	}
}

func (c *Client) batchLimitLocked(key batchKey) int {
	if limit, ok := c.batchLimits[key]; ok {
		return limit
	}
	return maxBatchSize
}

func (c *Client) lowerBatchLimit(key batchKey, limit int) {
	if limit < 1 {
		limit = 1
	}
	c.batchMu.Lock()
	defer c.batchMu.Unlock()
	if limit < c.batchLimitLocked(key) {
		c.batchLimits[key] = limit
		log.Printf("mdblist: batch limit for %s/%s lowered to %d", key.idProvider, key.mediaType, limit)
	}
}
