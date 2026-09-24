package provider

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/Silo-Server/silo-plugin-metadata-mdblist/metadata"
)

// jawsFixturePath is a real captured MDBList response. Every rating-scale
// expectation in this file is pinned to it rather than to a hand-written
// sample, because a wrong scale silently corrupts every rating in a library.
const jawsFixturePath = "testdata/movie_jaws.json"

func jawsFixture(t *testing.T) string {
	t.Helper()

	data, err := os.ReadFile(jawsFixturePath)
	if err != nil {
		t.Fatalf("read %s: %v", jawsFixturePath, err)
	}
	return string(data)
}

func decodeResponse(t *testing.T, body string) *mediaResponse {
	t.Helper()

	var decoded mediaResponse
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		t.Fatalf("decode media response: %v", err)
	}
	return &decoded
}

// ratingsBody wraps a raw "ratings" array in an otherwise minimal response, so
// the table cases below go through the same JSON decode as a live answer and
// therefore exercise the nullable Value/Score pointers in types.go.
func ratingsBody(ratings string) string {
	return `{"title":"Test","type":"movie","ratings":` + ratings + `}`
}

// fakeAPI is a stand-in MDBList. It records every request so tests can assert
// both the route the plugin chose and, just as importantly, that it made no
// request at all when it had nothing to look up.
type fakeAPI struct {
	server *httptest.Server

	mu     sync.Mutex
	paths  []string
	keys   []string
	status int
	body   string
}

func newFakeAPI(t *testing.T, status int, body string) *fakeAPI {
	t.Helper()

	api := &fakeAPI{status: status, body: body}
	api.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		api.mu.Lock()
		api.paths = append(api.paths, r.URL.Path)
		api.keys = append(api.keys, r.URL.Query().Get("apikey"))
		status, body := api.status, api.body
		api.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(api.server.Close)
	return api
}

func (a *fakeAPI) requests() (paths, keys []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.paths...), append([]string(nil), a.keys...)
}

func (a *fakeAPI) client(apiKey string) *Client {
	client := NewClient()
	client.SetBaseURL(a.server.URL)
	client.SetAPIKey(apiKey)
	return client
}

// TestRatingsPinnedToJawsFixture is the most important test in the repo: it
// fixes the per-source scale conversion against a real response.
//
// Pinned from testdata/movie_jaws.json:
//
//	imdb      value 8.1  score 81  -> rating_imdb        8.1  (Silo scale 0-10)
//	tmdb      value 76   score 76  -> rating_tmdb        7.6  (Silo scale 0-10)
//	tomatoes  value 97   score 97  -> rating_rt_critic   97   (Silo scale 0-100)
//	audience  not present          -> rating_rt_audience unset
//
// IMDb publishes out of 10 so its "value" passes straight through, while
// TMDB's "value" is already a percentage and has to be divided by ten. The
// fixture also carries metacritic 87, trakt 78, letterboxd 8/80 and
// rogerebert 4, which Silo has no column for; asserting each of the four
// fields exactly is therefore also the check that none of those leaked into a
// column that is not theirs.
func TestRatingsPinnedToJawsFixture(t *testing.T) {
	t.Parallel()

	result := resultFromResponse(decodeResponse(t, jawsFixture(t)), "movie")

	if result == nil {
		t.Fatal("resultFromResponse() = nil, want a result for a decoded MDBList response")
	}

	want := metadata.Ratings{
		IMDB:       8.1,
		TMDB:       7.6,
		RTCritic:   97,
		RTAudience: 0,
	}
	if result.Ratings != want {
		t.Fatalf("Ratings = %+v, want %+v", result.Ratings, want)
	}
}

func TestRatingScaleConversionPerSource(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		ratings string
		want    metadata.Ratings
	}{
		{
			name:    "imdb value is native 0-10 and passes through",
			ratings: `[{"source":"imdb","value":8.1,"score":81}]`,
			want:    metadata.Ratings{IMDB: 8.1},
		},
		{
			name:    "imdb without a value falls back to score divided by ten",
			ratings: `[{"source":"imdb","value":null,"score":81}]`,
			want:    metadata.Ratings{IMDB: 8.1},
		},
		{
			name:    "tmdb value is a percentage and is divided by ten",
			ratings: `[{"source":"tmdb","value":76,"score":76}]`,
			want:    metadata.Ratings{TMDB: 7.6},
		},
		{
			name:    "tmdb without a value falls back to score divided by ten",
			ratings: `[{"source":"tmdb","value":null,"score":76}]`,
			want:    metadata.Ratings{TMDB: 7.6},
		},
		{
			name:    "tomatoes is the rotten tomatoes critic score out of 100",
			ratings: `[{"source":"tomatoes","value":97,"score":97}]`,
			want:    metadata.Ratings{RTCritic: 97},
		},
		{
			name:    "audience is the rotten tomatoes audience score out of 100",
			ratings: `[{"source":"audience","value":89,"score":89}]`,
			want:    metadata.Ratings{RTAudience: 89},
		},
		{
			// The score-only path for the 0-100 columns, matching the cases
			// above that cover it for imdb and tmdb.
			name:    "rotten tomatoes scores fall back to score when value is null",
			ratings: `[{"source":"tomatoes","value":null,"score":97},{"source":"audience","value":null,"score":89}]`,
			want:    metadata.Ratings{RTCritic: 97, RTAudience: 89},
		},
		{
			// No captured MDBList response contains an "audience" entry, so
			// the scale of its "value" is unobserved. Reading it would risk
			// writing a star figure into a percentage column, so an audience
			// entry with no score contributes nothing and the column is left
			// for another provider. Every other source here is pinned by the
			// fixture and may fall back to value.
			name:    "an audience entry with no score is skipped rather than guessed",
			ratings: `[{"source":"audience","value":4.5}]`,
			want:    metadata.Ratings{},
		},
		{
			// rating_rt_critic and rating_rt_audience are integer columns, so a
			// fractional percentage is rounded rather than truncated.
			name:    "fractional rotten tomatoes scores round to the nearest whole percent",
			ratings: `[{"source":"tomatoes","value":96.5,"score":96.5},{"source":"audience","value":88.4,"score":88.4}]`,
			want:    metadata.Ratings{RTCritic: 97, RTAudience: 88},
		},
		{
			name:    "source names are matched case and whitespace insensitively",
			ratings: `[{"source":" IMDb ","value":8.1,"score":81},{"source":"TMDB","value":76,"score":76}]`,
			want:    metadata.Ratings{IMDB: 8.1, TMDB: 7.6},
		},
		{
			// Silo has no column for these, and the host drops any ratings key
			// it does not recognise, so they must not be mapped anywhere.
			name: "sources silo cannot store are ignored entirely",
			ratings: `[{"source":"metacritic","value":87,"score":87},` +
				`{"source":"metacriticuser","value":null,"score":null},` +
				`{"source":"trakt","value":78,"score":78},` +
				`{"source":"letterboxd","value":8,"score":80},` +
				`{"source":"rogerebert","value":4,"score":null},` +
				`{"source":"myanimelist","value":null,"score":null}]`,
			want: metadata.Ratings{},
		},
		{
			name:    "all four columns fill from one response",
			ratings: `[{"source":"imdb","value":8.1,"score":81},{"source":"tmdb","value":76,"score":76},{"source":"tomatoes","value":97,"score":97},{"source":"audience","value":90,"score":90}]`,
			want:    metadata.Ratings{IMDB: 8.1, TMDB: 7.6, RTCritic: 97, RTAudience: 90},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result := resultFromResponse(decodeResponse(t, ratingsBody(tt.ratings)), "movie")
			if result.Ratings != tt.want {
				t.Fatalf("Ratings = %+v, want %+v", result.Ratings, tt.want)
			}
		})
	}
}

// TestUnratedEntriesAreSkippedNotZeroed covers the case that matters for
// fill-empty merging host side: an entry a source has not rated must leave the
// column untouched, never write a zero that looks like a real rating.
func TestUnratedEntriesAreSkippedNotZeroed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		ratings string
		want    metadata.Ratings
	}{
		{
			name:    "null value and null score",
			ratings: `[{"source":"imdb","value":null,"score":null},{"source":"tmdb","value":null,"score":null},{"source":"tomatoes","value":null,"score":null},{"source":"audience","value":null,"score":null}]`,
			want:    metadata.Ratings{},
		},
		{
			name:    "value and score fields absent altogether",
			ratings: `[{"source":"imdb"},{"source":"tmdb"},{"source":"tomatoes"},{"source":"audience"}]`,
			want:    metadata.Ratings{},
		},
		{
			// Zero is the absent sentinel at every layer, host side included,
			// so a genuine 0% Tomatometer is indistinguishable from unrated.
			// This pins the current host limitation, not a desired semantic:
			// emitting a bare 0 would be dropped by the host's merge anyway.
			name:    "explicit zeros are treated as unrated",
			ratings: `[{"source":"imdb","value":0,"score":0},{"source":"tomatoes","value":0,"score":0}]`,
			want:    metadata.Ratings{},
		},
		{
			name:    "an empty ratings array yields no ratings",
			ratings: `[]`,
			want:    metadata.Ratings{},
		},
		{
			// A duplicate, unrated entry for a source must not clobber the
			// rating an earlier entry already supplied.
			name:    "a later unrated entry does not overwrite an earlier rating",
			ratings: `[{"source":"imdb","value":8.1,"score":81},{"source":"imdb","value":null,"score":null}]`,
			want:    metadata.Ratings{IMDB: 8.1},
		},
		{
			name:    "an entry with no source is ignored",
			ratings: `[{"source":"","value":8.1,"score":81}]`,
			want:    metadata.Ratings{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result := resultFromResponse(decodeResponse(t, ratingsBody(tt.ratings)), "movie")
			if result.Ratings != tt.want {
				t.Fatalf("Ratings = %+v, want %+v", result.Ratings, tt.want)
			}
		})
	}
}

func TestCertificationAndAdvisoryAge(t *testing.T) {
	t.Parallel()

	fixture := resultFromResponse(decodeResponse(t, jawsFixture(t)), "movie")
	if got, want := fixture.ContentRating, "PG"; got != want {
		t.Fatalf("fixture ContentRating = %q, want %q", got, want)
	}
	if got, want := fixture.AdvisoryAge, 13; got != want {
		t.Fatalf("fixture AdvisoryAge = %d, want %d", got, want)
	}
	if got, want := fixture.AdvisorySource, metadata.AdvisorySourceCommonSense; got != want {
		t.Fatalf("fixture AdvisorySource = %q, want %q", got, want)
	}

	tests := []struct {
		name              string
		body              string
		wantContentRating string
		wantAge           int
		wantSource        string
	}{
		{
			name:              "commonsense false attributes the age to mdblist",
			body:              `{"certification":"PG-13","commonsense":false,"age_rating":13}`,
			wantContentRating: "PG-13",
			wantAge:           13,
			wantSource:        metadata.AdvisorySourceMDBList,
		},
		{
			name:              "certification is trimmed",
			body:              `{"certification":"  R  ","commonsense":true,"age_rating":17}`,
			wantContentRating: "R",
			wantAge:           17,
			wantSource:        metadata.AdvisorySourceCommonSense,
		},
		{
			name:              "a null age rating leaves no advisory at all",
			body:              `{"certification":"TV-MA","commonsense":true,"age_rating":null}`,
			wantContentRating: "TV-MA",
			wantAge:           0,
			wantSource:        "",
		},
		{
			name:              "a zero age rating leaves no advisory at all",
			body:              `{"certification":"G","commonsense":true,"age_rating":0}`,
			wantContentRating: "G",
			wantAge:           0,
			wantSource:        "",
		},
		{
			name:              "a missing certification stays empty for another provider to fill",
			body:              `{"age_rating":13,"commonsense":true}`,
			wantContentRating: "",
			wantAge:           13,
			wantSource:        metadata.AdvisorySourceCommonSense,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result := resultFromResponse(decodeResponse(t, tt.body), "movie")
			if got := result.ContentRating; got != tt.wantContentRating {
				t.Fatalf("ContentRating = %q, want %q", got, tt.wantContentRating)
			}
			if got := result.AdvisoryAge; got != tt.wantAge {
				t.Fatalf("AdvisoryAge = %d, want %d", got, tt.wantAge)
			}
			if got := result.AdvisorySource; got != tt.wantSource {
				t.Fatalf("AdvisorySource = %q, want %q", got, tt.wantSource)
			}
		})
	}
}

// TestResponseTypeMismatchYieldsNothing guards the one way a stale external ID
// can poison a catalog: the movie route answering with a show record (or the
// reverse). Nothing in that body — certification, ratings, advisory age —
// belongs to the item the host asked about.
func TestResponseTypeMismatchYieldsNothing(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		mediaType string
		body      string
		wantNil   bool
	}{
		{
			name:      "a show body on the movie route",
			mediaType: "movie",
			body:      `{"type":"show","certification":"TV-MA","ratings":[{"source":"tomatoes","value":96,"score":96}]}`,
			wantNil:   true,
		},
		{
			name:      "a movie body on the show route",
			mediaType: "show",
			body:      `{"type":"movie","certification":"PG","ratings":[{"source":"imdb","value":8.1,"score":81}]}`,
			wantNil:   true,
		},
		{
			name:      "matching types are mapped",
			mediaType: "show",
			body:      `{"type":"show","certification":"TV-MA"}`,
		},
		{
			name:      "type case and whitespace are normalised",
			mediaType: "movie",
			body:      `{"type":"  Movie ","certification":"PG"}`,
		},
		{
			// MDBList omits "type" on some records; that is not evidence of a
			// mismatch, so the body is still mapped.
			name:      "an absent type is not a mismatch",
			mediaType: "movie",
			body:      `{"certification":"PG"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result := resultFromResponse(decodeResponse(t, tt.body), tt.mediaType)
			if tt.wantNil {
				if result != nil {
					t.Fatalf("resultFromResponse() = %+v, want nil", result)
				}
				return
			}
			if result == nil {
				t.Fatal("resultFromResponse() = nil, want a result")
			}
		})
	}
}

// TestGetMetadataWithoutUsableIDMakesNoRequest pins the enrichment-only
// contract at the provider level: with nothing another provider resolved,
// there is no lookup to make and no error to report.
func TestGetMetadataWithoutUsableIDMakesNoRequest(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		providerIDs map[string]string
	}{
		{name: "no provider ids at all", providerIDs: nil},
		{name: "empty provider ids", providerIDs: map[string]string{}},
		{name: "only ids mdblist cannot be queried by", providerIDs: map[string]string{"tvdb": "81189", "plex": "abc"}},
		{name: "imdb id missing its tt prefix", providerIDs: map[string]string{"imdb": "0073195"}},
		{name: "imdb id that is only the prefix", providerIDs: map[string]string{"imdb": "tt"}},
		{name: "empty imdb id", providerIDs: map[string]string{"imdb": ""}},
		{name: "non-numeric tmdb id", providerIDs: map[string]string{"tmdb": "not-a-number"}},
		{name: "empty tmdb id", providerIDs: map[string]string{"tmdb": ""}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			api := newFakeAPI(t, http.StatusOK, jawsFixture(t))
			provider := api.client("test-key")

			result, err := provider.GetMetadata(context.Background(), metadata.MetadataRequest{
				ProviderIDs: tt.providerIDs,
				ContentType: "movie",
			})
			if err != nil {
				t.Fatalf("GetMetadata() returned error: %v", err)
			}
			if result != nil {
				t.Fatalf("GetMetadata() = %+v, want no result for %+v", result, tt.providerIDs)
			}
			if paths, _ := api.requests(); len(paths) != 0 {
				t.Fatalf("made %d MDBList request(s) %v, want none", len(paths), paths)
			}
		})
	}
}

func TestGetMetadataUnsupportedItemType(t *testing.T) {
	t.Parallel()

	// The manifest's default_priority deliberately omits season and episode so
	// the host never asks, but an unexpected type still has to be answered
	// empty rather than with an error.
	for _, itemType := range []string{"season", "episode", "audiobook", "ebook", "podcast", ""} {
		t.Run("item type "+itemType, func(t *testing.T) {
			t.Parallel()

			api := newFakeAPI(t, http.StatusOK, jawsFixture(t))
			provider := api.client("test-key")

			result, err := provider.GetMetadata(context.Background(), metadata.MetadataRequest{
				ProviderIDs: map[string]string{"imdb": "tt0073195"},
				ContentType: itemType,
			})
			if err != nil {
				t.Fatalf("GetMetadata() returned error: %v", err)
			}
			if result != nil {
				t.Fatalf("GetMetadata() = %+v, want no result", result)
			}
			if paths, _ := api.requests(); len(paths) != 0 {
				t.Fatalf("made %d MDBList request(s) %v, want none", len(paths), paths)
			}
		})
	}
}

func TestGetMetadataRouteSelection(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		providerIDs map[string]string
		itemType    string
		wantPath    string
	}{
		{
			name:        "movie by imdb id",
			providerIDs: map[string]string{"imdb": "tt0073195"},
			itemType:    "movie",
			wantPath:    "/imdb/movie/tt0073195",
		},
		{
			name:        "imdb is preferred when both ids are present",
			providerIDs: map[string]string{"imdb": "tt0073195", "tmdb": "578"},
			itemType:    "movie",
			wantPath:    "/imdb/movie/tt0073195",
		},
		{
			name:        "movie by tmdb id when imdb is missing",
			providerIDs: map[string]string{"tmdb": "578"},
			itemType:    "movie",
			wantPath:    "/tmdb/movie/578",
		},
		{
			name:        "silo series maps to the mdblist show path",
			providerIDs: map[string]string{"tmdb": "1396"},
			itemType:    "series",
			wantPath:    "/tmdb/show/1396",
		},
		{
			name:        "show is accepted as a series alias",
			providerIDs: map[string]string{"imdb": "tt0903747"},
			itemType:    "show",
			wantPath:    "/imdb/show/tt0903747",
		},
		{
			name:        "tv is accepted as a series alias",
			providerIDs: map[string]string{"imdb": "tt0903747"},
			itemType:    "tv",
			wantPath:    "/imdb/show/tt0903747",
		},
		{
			name:        "item type case and whitespace are normalised",
			providerIDs: map[string]string{"imdb": "tt0073195"},
			itemType:    "  Movie ",
			wantPath:    "/imdb/movie/tt0073195",
		},
		{
			name:        "imdb ids are lowercased and trimmed",
			providerIDs: map[string]string{"imdb": " TT0073195 "},
			itemType:    "movie",
			wantPath:    "/imdb/movie/tt0073195",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			body := jawsFixture(t)
			if strings.Contains(tt.wantPath, "/show/") {
				body = `{"type":"show","certification":"TV-MA"}`
			}
			api := newFakeAPI(t, http.StatusOK, body)
			provider := api.client("test-key")

			result, err := provider.GetMetadata(context.Background(), metadata.MetadataRequest{
				ProviderIDs: tt.providerIDs,
				ContentType: tt.itemType,
			})
			if err != nil {
				t.Fatalf("GetMetadata() returned error: %v", err)
			}
			if result == nil {
				t.Fatal("GetMetadata() = nil, want metadata")
			}

			paths, keys := api.requests()
			if want := []string{tt.wantPath}; !reflect.DeepEqual(paths, want) {
				t.Fatalf("request paths = %v, want %v", paths, want)
			}
			if want := []string{"test-key"}; !reflect.DeepEqual(keys, want) {
				t.Fatalf("apikey query values = %v, want %v", keys, want)
			}
		})
	}
}

// TestGetMetadataDegradesWithoutError is the provider-level half of the
// availability contract: a metadata refresh must never fail because MDBList is
// unreachable, unauthorised, or out of credits.
func TestGetMetadataDegradesWithoutError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		apiKey       string
		status       int
		body         string
		wantRequests int
	}{
		{name: "no api key configured", apiKey: "", status: http.StatusOK, body: `{"title":"Jaws"}`, wantRequests: 0},
		{name: "unauthorized", apiKey: "bad", status: http.StatusUnauthorized, body: `{"error":"Invalid API key"}`, wantRequests: 1},
		{name: "forbidden", apiKey: "bad", status: http.StatusForbidden, body: ``, wantRequests: 1},
		{name: "not found", apiKey: "k", status: http.StatusNotFound, body: `{"error":"not found"}`, wantRequests: 1},
		{name: "rate limited", apiKey: "k", status: http.StatusTooManyRequests, body: `{"error":"limit"}`, wantRequests: 1},
		{name: "server error", apiKey: "k", status: http.StatusInternalServerError, body: ``, wantRequests: 1},
		{name: "quota exhausted behind a 200", apiKey: "k", status: http.StatusOK, body: `{"error":"API request limit reached","response":false}`, wantRequests: 1},
		{name: "explicit response false", apiKey: "k", status: http.StatusOK, body: `{"response":false}`, wantRequests: 1},
		{name: "truncated json", apiKey: "k", status: http.StatusOK, body: `{"ratings":[`, wantRequests: 1},
		{name: "an html error page instead of json", apiKey: "k", status: http.StatusOK, body: `<html><body>nope</body></html>`, wantRequests: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			api := newFakeAPI(t, tt.status, tt.body)
			provider := api.client(tt.apiKey)

			result, err := provider.GetMetadata(context.Background(), metadata.MetadataRequest{
				ProviderIDs: map[string]string{"imdb": "tt0073195"},
				ContentType: "movie",
			})
			if err != nil {
				t.Fatalf("GetMetadata() returned error: %v", err)
			}
			if result != nil {
				t.Fatalf("GetMetadata() = %+v, want no result", result)
			}
			if paths, _ := api.requests(); len(paths) != tt.wantRequests {
				t.Fatalf("made %d MDBList request(s) %v, want %d", len(paths), paths, tt.wantRequests)
			}
		})
	}
}

// TestGetMetadataPropagatesContextError is the one exception to "never return
// an error": a cancelled or expired context is the host's own signal and has to
// reach it.
func TestGetMetadataPropagatesContextError(t *testing.T) {
	t.Parallel()

	api := newFakeAPI(t, http.StatusOK, jawsFixture(t))
	provider := api.client("test-key")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result, err := provider.GetMetadata(ctx, metadata.MetadataRequest{
		ProviderIDs: map[string]string{"imdb": "tt0073195"},
		ContentType: "movie",
	})
	if err == nil {
		t.Fatalf("GetMetadata() with a cancelled context returned %+v and no error, want an error", result)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("GetMetadata() error = %v, want context.Canceled", err)
	}
}
