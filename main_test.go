package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sync"
	"testing"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/Silo-Server/silo-plugin-metadata-mdblist/metadata"
	"github.com/Silo-Server/silo-plugin-metadata-mdblist/provider"
)

// fixturePath is the same real captured MDBList response the provider tests
// pin the rating scales to.
const fixturePath = "provider/testdata/movie_jaws.json"

func fixtureBody(t *testing.T) string {
	t.Helper()

	data, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("read %s: %v", fixturePath, err)
	}
	return string(data)
}

func mustStruct(t *testing.T, value map[string]any) *structpb.Struct {
	t.Helper()

	result, err := structpb.NewStruct(value)
	if err != nil {
		t.Fatalf("structpb.NewStruct() returned error: %v", err)
	}
	return result
}

// newServers wires the same two gRPC server types main() serves, pointed at a
// fake MDBList. This is as close as a unit test gets to the way the host calls
// the plugin.
func newServers(t *testing.T, status int, body string) (*runtimeServer, *metadataServer, *fakeAPI) {
	t.Helper()

	api := newFakeAPI(t, status, body)

	client := provider.NewClient()
	client.SetBaseURL(api.server.URL)

	rs := &runtimeServer{
		manifest: &pluginv1.PluginManifest{PluginId: "silo.mdblist"},
		client:   client,
	}
	return rs, &metadataServer{runtime: rs}, api
}

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

// TestGetMetadataEndToEnd drives the metadata server the way the host does —
// Configure with the admin form's saved account, then GetMetadata with the IDs
// the primary provider resolved — and pins the resulting MetadataItem.
func TestGetMetadataEndToEnd(t *testing.T) {
	t.Parallel()

	rs, ms, api := newServers(t, http.StatusOK, fixtureBody(t))

	if _, err := rs.Configure(context.Background(), &pluginv1.ConfigureRequest{
		Config: []*pluginv1.ConfigEntry{{
			Key:   "account",
			Value: mustStruct(t, map[string]any{"api_key": "secret-key"}),
		}},
	}); err != nil {
		t.Fatalf("Configure() returned error: %v", err)
	}

	response, err := ms.GetMetadata(context.Background(), &pluginv1.GetMetadataRequest{
		ItemType:    "movie",
		ProviderIds: mustStruct(t, map[string]any{"imdb": "tt0073195", "tmdb": "578"}),
	})
	if err != nil {
		t.Fatalf("GetMetadata() returned error: %v", err)
	}

	item := response.GetItem()
	if item == nil {
		t.Fatal("GetMetadata() returned no item, want the enriched fixture")
	}

	paths, keys := api.requests()
	if want := []string{"/tmdb/movie/578"}; !reflect.DeepEqual(paths, want) {
		t.Fatalf("request paths = %v, want %v", paths, want)
	}
	if want := []string{"secret-key"}; !reflect.DeepEqual(keys, want) {
		t.Fatalf("apikey query values = %v, want %v (Configure must reach the client)", keys, want)
	}

	if got, want := item.GetItemType(), "movie"; got != want {
		t.Fatalf("ItemType = %q, want %q", got, want)
	}
	if got, want := item.GetContentRating(), "PG"; got != want {
		t.Fatalf("ContentRating = %q, want %q", got, want)
	}

	// The host stores these four keys in typed columns (imdb and tmdb out of
	// 10, rt_critic and rt_audience out of 100). The fixture has no Rotten
	// Tomatoes audience score, so that key must be absent rather than zero.
	// "sources" carries every rated source on a 0-100 scale for hosts that
	// store them; the fixture's metacriticuser and myanimelist entries are
	// null and must not appear.
	wantRatings := map[string]any{
		"imdb":      8.1,
		"tmdb":      7.6,
		"rt_critic": float64(97),
		"sources": map[string]any{
			"imdb":       map[string]any{"score": float64(81), "votes": float64(673852)},
			"tmdb":       map[string]any{"score": float64(76), "votes": float64(10114)},
			"rt_critic":  map[string]any{"score": float64(97), "votes": float64(102)},
			"metacritic": map[string]any{"score": float64(87), "votes": float64(21)},
			"trakt":      map[string]any{"score": float64(78), "votes": float64(14033)},
			"letterboxd": map[string]any{"score": float64(80), "votes": float64(876082)},
			"rogerebert": map[string]any{"score": float64(100)},
			"mdblist":    map[string]any{"score": float64(86)},
		},
	}
	if got := item.GetRatings().AsMap(); !reflect.DeepEqual(got, wantRatings) {
		t.Fatalf("Ratings = %v, want %v", got, wantRatings)
	}

	// Fill-empty fields: the host keeps TMDB's value wherever it has one, so
	// these only land in blanks.
	if got, want := item.GetYear(), int32(1975); got != want {
		t.Fatalf("Year = %d, want %d", got, want)
	}
	if got, want := item.GetReleaseDate(), "1975-06-20"; got != want {
		t.Fatalf("ReleaseDate = %q, want %q", got, want)
	}
	if got := item.GetFirstAirDate(); got != "" {
		t.Fatalf("FirstAirDate = %q, want empty for a movie", got)
	}
	if got, want := item.GetRuntime(), int32(124); got != want {
		t.Fatalf("Runtime = %d, want %d", got, want)
	}
	if got, want := item.GetOriginalLanguage(), "en"; got != want {
		t.Fatalf("OriginalLanguage = %q, want %q", got, want)
	}
	// The fixture carries "country": "us", but the host unions countries
	// across providers, so MDBList's would be added to TMDB's.
	if got := item.GetCountries(); len(got) != 0 {
		t.Fatalf("Countries = %v, want none", got)
	}
	// Keywords would pile up on TMDB's the same way.
	if _, ok := item.GetMetadata().AsMap()["keywords"]; ok {
		t.Fatalf("metadata carries keywords: %v", item.GetMetadata().AsMap())
	}
	// "released" is a movie's status; the host's status field is a show's.
	if got := item.GetStatus(); got != "" {
		t.Fatalf("Status = %q, want empty for a movie", got)
	}

	// Forward-compatible seam: the advisory age rides in the free-form
	// metadata Struct until the typed proto fields land.
	wantMetadata := map[string]any{
		"advisory_age":    float64(13),
		"advisory_source": "commonsense",
	}
	if got := item.GetMetadata().AsMap(); !reflect.DeepEqual(got, wantMetadata) {
		t.Fatalf("Metadata = %v, want %v", got, wantMetadata)
	}

	// Enrichment only: the plugin contributes no identity or descriptive
	// fields even though the response carries them, so it can never win a
	// title or overview from the primary provider. Identity matters most: the
	// host merges a returned ID into the item's durable identity set and hands
	// it to every later provider, so MDBList's ids object stays unread.
	if got := item.GetProviderIds(); got != nil {
		t.Fatalf("ProviderIds = %v, want none from an enrichment-only provider", got.AsMap())
	}
	if got := item.GetProviderId(); got != "" {
		t.Fatalf("ProviderId = %q, want empty from an enrichment-only provider", got)
	}
	if got := item.GetTitle(); got != "" {
		t.Fatalf("Title = %q, want empty for an enrichment-only provider", got)
	}
	if got := item.GetOverview(); got != "" {
		t.Fatalf("Overview = %q, want empty for an enrichment-only provider", got)
	}
	if got := item.GetTagline(); got != "" {
		t.Fatalf("Tagline = %q, want empty for an enrichment-only provider", got)
	}
	if got := item.GetPosterPath(); got != "" {
		t.Fatalf("PosterPath = %q, want empty for an enrichment-only provider", got)
	}
	if got := item.GetVideos(); len(got) != 0 {
		t.Fatalf("Videos = %v, want none: the host accumulates videos, so MDBList's trailer would duplicate TMDB's", got)
	}
}

func TestGetMetadataForSeriesEndToEnd(t *testing.T) {
	t.Parallel()

	body := `{"title":"Breaking Bad","type":"show","certification":"TV-MA","commonsense":false,` +
		`"age_rating":16,"ids":{"imdb":"tt0903747","tmdb":1396,"tvdb":81189},` +
		`"year":2008,"released":"2008-01-20","runtime":2700,"status":"Ended","language":"en","country":"us",` +
		`"ratings":[{"source":"imdb","value":9.5,"score":95},{"source":"tmdb","value":89,"score":89},` +
		`{"source":"tomatoes","value":96,"score":96},{"source":"popcorn","value":97,"score":97}]}`

	rs, ms, api := newServers(t, http.StatusOK, body)
	rs.client.SetAPIKey("k")

	response, err := ms.GetMetadata(context.Background(), &pluginv1.GetMetadataRequest{
		ItemType:    "series",
		ProviderIds: mustStruct(t, map[string]any{"tmdb": "1396"}),
	})
	if err != nil {
		t.Fatalf("GetMetadata() returned error: %v", err)
	}

	item := response.GetItem()
	if item == nil {
		t.Fatal("GetMetadata() returned no item")
	}

	paths, _ := api.requests()
	if want := []string{"/tmdb/show/1396"}; !reflect.DeepEqual(paths, want) {
		t.Fatalf("request paths = %v, want %v", paths, want)
	}
	if got, want := item.GetItemType(), "series"; got != want {
		t.Fatalf("ItemType = %q, want %q", got, want)
	}
	if got, want := item.GetContentRating(), "TV-MA"; got != want {
		t.Fatalf("ContentRating = %q, want %q", got, want)
	}

	wantRatings := map[string]any{
		"imdb":        9.5,
		"tmdb":        8.9,
		"rt_critic":   float64(96),
		"rt_audience": float64(97),
		"sources": map[string]any{
			"imdb":        map[string]any{"score": float64(95)},
			"tmdb":        map[string]any{"score": float64(89)},
			"rt_critic":   map[string]any{"score": float64(96)},
			"rt_audience": map[string]any{"score": float64(97)},
		},
	}
	if got := item.GetRatings().AsMap(); !reflect.DeepEqual(got, wantRatings) {
		t.Fatalf("Ratings = %v, want %v", got, wantRatings)
	}

	// A show's first air date goes to first_air_date and its status through;
	// its runtime is a whole-series figure, not per episode, so it stays out.
	if got, want := item.GetFirstAirDate(), "2008-01-20"; got != want {
		t.Fatalf("FirstAirDate = %q, want %q", got, want)
	}
	if got := item.GetReleaseDate(); got != "" {
		t.Fatalf("ReleaseDate = %q, want empty for a show", got)
	}
	if got := item.GetRuntime(); got != 0 {
		t.Fatalf("Runtime = %d, want 0 for a show", got)
	}
	if got, want := item.GetStatus(), "Ended"; got != want {
		t.Fatalf("Status = %q, want %q", got, want)
	}

	wantMetadata := map[string]any{"advisory_age": float64(16), "advisory_source": "mdblist"}
	if got := item.GetMetadata().AsMap(); !reflect.DeepEqual(got, wantMetadata) {
		t.Fatalf("Metadata = %v, want %v", got, wantMetadata)
	}

	// The response maps this series to imdb tt0903747 and tvdb 81189, neither
	// of which the request carried. Inventing them would give the host an
	// unverified identity to persist and to hand to lower-priority providers.
	if got := item.GetProviderIds(); got != nil {
		t.Fatalf("ProviderIds = %v, want none from an enrichment-only provider", got.AsMap())
	}
}

// TestGetMetadataWithoutExternalIDReturnsEmpty is the enrichment-only contract
// at the RPC boundary: the plugin never identifies anything, so without an ID
// the primary provider resolved it answers empty and successfully.
func TestGetMetadataWithoutExternalIDReturnsEmpty(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		providerIDs *structpb.Struct
	}{
		{name: "nil provider ids struct", providerIDs: nil},
		{name: "empty provider ids struct", providerIDs: &structpb.Struct{}},
		{
			name:        "only ids mdblist cannot be queried by",
			providerIDs: mustStruct(t, map[string]any{"tvdb": "81189", "plex": "abc"}),
		},
		{
			name:        "an imdb id missing its tt prefix",
			providerIDs: mustStruct(t, map[string]any{"imdb": "0073195"}),
		},
		{
			name:        "a non-numeric tmdb id",
			providerIDs: mustStruct(t, map[string]any{"tmdb": "not-a-number"}),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rs, ms, api := newServers(t, http.StatusOK, fixtureBody(t))
			rs.client.SetAPIKey("k")

			response, err := ms.GetMetadata(context.Background(), &pluginv1.GetMetadataRequest{
				ItemType:    "movie",
				ProviderIds: tt.providerIDs,
			})
			if err != nil {
				t.Fatalf("GetMetadata() returned error: %v", err)
			}
			if response.GetItem() != nil {
				t.Fatalf("GetMetadata() item = %v, want none", response.GetItem())
			}
			if paths, _ := api.requests(); len(paths) != 0 {
				t.Fatalf("made %d MDBList request(s) %v, want none", len(paths), paths)
			}
		})
	}
}

// TestGetMetadataMapsFailuresToStatuses covers the failure contract at the RPC
// boundary: an unknown title is an empty item, and every reason MDBList cannot
// answer is the gRPC status Silo's bulk enrichment pass reads it by.
func TestGetMetadataMapsFailuresToStatuses(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		apiKey   string
		status   int
		body     string
		wantCode codes.Code
	}{
		{name: "http 404 unknown title", apiKey: "k", status: http.StatusNotFound, body: `{"error":"not found"}`, wantCode: codes.OK},
		{name: "no api key configured", apiKey: "", status: http.StatusOK, body: `{"title":"Jaws"}`, wantCode: codes.FailedPrecondition},
		{name: "http 401 rejected key", apiKey: "bad", status: http.StatusUnauthorized, body: `{"error":"Invalid API key"}`, wantCode: codes.Unauthenticated},
		{name: "http 429 quota exhausted", apiKey: "k", status: http.StatusTooManyRequests, body: `{"error":"limit"}`, wantCode: codes.ResourceExhausted},
		{name: "http 500 outage", apiKey: "k", status: http.StatusInternalServerError, body: ``, wantCode: codes.Unavailable},
		{name: "quota exhausted behind a 200", apiKey: "k", status: http.StatusOK, body: `{"error":"API request limit reached","response":false}`, wantCode: codes.ResourceExhausted},
		{name: "unusable answer", apiKey: "k", status: http.StatusOK, body: `<html>nope</html>`, wantCode: codes.Internal},
		{name: "title error behind a 200", apiKey: "k", status: http.StatusOK, body: `{"error":"Something went wrong","response":false}`, wantCode: codes.Internal},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rs, ms, _ := newServers(t, tt.status, tt.body)
			rs.client.SetAPIKey(tt.apiKey)

			response, err := ms.GetMetadata(context.Background(), &pluginv1.GetMetadataRequest{
				ItemType:    "movie",
				ProviderIds: mustStruct(t, map[string]any{"imdb": "tt0073195"}),
			})
			if got := status.Code(err); got != tt.wantCode {
				t.Fatalf("GetMetadata() status = %v (%v), want %v", got, err, tt.wantCode)
			}
			if response.GetItem() != nil {
				t.Fatalf("GetMetadata() item = %v, want none", response.GetItem())
			}
		})
	}
}

// TestSearchReturnsNoResults pins the rule that keeps this plugin out of
// identity: MDBList has a /search endpoint, and the plugin must not use it.
func TestSearchReturnsNoResults(t *testing.T) {
	t.Parallel()

	_, ms, api := newServers(t, http.StatusOK, fixtureBody(t))

	for _, req := range []*pluginv1.SearchMetadataRequest{
		{Query: "Jaws", ItemType: "movie", Year: 1975},
		{Query: "Breaking Bad", ItemType: "series"},
		{Query: "Jaws", ItemType: "movie", ProviderIds: mustStruct(t, map[string]any{"imdb": "tt0073195"})},
	} {
		response, err := ms.Search(context.Background(), req)
		if err != nil {
			t.Fatalf("Search(%q) returned error: %v", req.GetQuery(), err)
		}
		if got := len(response.GetResults()); got != 0 {
			t.Fatalf("Search(%q) returned %d results, want 0", req.GetQuery(), got)
		}
	}

	if paths, _ := api.requests(); len(paths) != 0 {
		t.Fatalf("Search made %d MDBList request(s) %v, want none", len(paths), paths)
	}
}

// TestUnsupportedRPCsAnswerEmptySuccessfully covers the rest of the interface
// the SDK's PluginProvider satisfies unconditionally. The host will call each
// of these on us; an error here would add failure noise to every match.
func TestUnsupportedRPCsAnswerEmptySuccessfully(t *testing.T) {
	t.Parallel()

	_, ms, api := newServers(t, http.StatusOK, fixtureBody(t))
	ctx := context.Background()
	providerIDs := mustStruct(t, map[string]any{"imdb": "tt0903747", "tmdb": "1396"})

	images, err := ms.GetImages(ctx, &pluginv1.GetImagesRequest{ItemType: "movie", ProviderIds: providerIDs})
	if err != nil {
		t.Fatalf("GetImages() returned error: %v", err)
	}
	if got := len(images.GetImages()); got != 0 {
		t.Fatalf("GetImages() returned %d images, want 0", got)
	}

	seasons, err := ms.GetSeasons(ctx, &pluginv1.GetSeasonsRequest{ProviderIds: providerIDs})
	if err != nil {
		t.Fatalf("GetSeasons() returned error: %v", err)
	}
	if got := len(seasons.GetSeasons()); got != 0 {
		t.Fatalf("GetSeasons() returned %d seasons, want 0", got)
	}

	episodes, err := ms.GetEpisodes(ctx, &pluginv1.GetEpisodesRequest{ProviderIds: providerIDs, SeasonNumber: 1})
	if err != nil {
		t.Fatalf("GetEpisodes() returned error: %v", err)
	}
	if got := len(episodes.GetEpisodes()); got != 0 {
		t.Fatalf("GetEpisodes() returned %d episodes, want 0", got)
	}

	person, err := ms.GetPersonDetail(ctx, &pluginv1.GetPersonDetailRequest{ProviderIds: providerIDs})
	if err != nil {
		t.Fatalf("GetPersonDetail() returned error: %v", err)
	}
	if person.GetPerson() != nil {
		t.Fatalf("GetPersonDetail() returned %v, want no person", person.GetPerson())
	}

	single, err := ms.ResolveImageURL(ctx, &pluginv1.ResolveImageURLRequest{Path: "mdblist://poster/x.jpg"})
	if err != nil {
		t.Fatalf("ResolveImageURL() returned error: %v", err)
	}
	if got := single.GetUrl(); got != "" {
		t.Fatalf("ResolveImageURL() = %q, want empty", got)
	}

	// Empty, not a map of paths to empty strings: the host caches what a
	// resolver answers, and a fabricated "" reads as a resolved URL.
	batch, err := ms.ResolveImageURLs(ctx, &pluginv1.ResolveImageURLsRequest{Paths: []string{"a", "b"}})
	if err != nil {
		t.Fatalf("ResolveImageURLs() returned error: %v", err)
	}
	if got := len(batch.GetUrls()); got != 0 {
		t.Fatalf("ResolveImageURLs() = %v, want no urls", batch.GetUrls())
	}

	if paths, _ := api.requests(); len(paths) != 0 {
		t.Fatalf("the unsupported RPCs made %d MDBList request(s) %v, want none", len(paths), paths)
	}
}

func TestRatingsStruct(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		ratings metadata.Ratings
		sources map[string]metadata.RatingSource
		want    map[string]any
	}{
		{
			name:    "all four host keys",
			ratings: metadata.Ratings{IMDB: 8.1, TMDB: 7.6, RTCritic: 97, RTAudience: 89},
			want:    map[string]any{"imdb": 8.1, "tmdb": 7.6, "rt_critic": float64(97), "rt_audience": float64(89)},
		},
		{
			// A zero must never be emitted: the host merges fill-empty, so an
			// emitted zero would look like a real rating and could displace
			// nothing while still writing a wrong value.
			name:    "unset ratings are omitted rather than sent as zero",
			ratings: metadata.Ratings{RTCritic: 97},
			want:    map[string]any{"rt_critic": float64(97)},
		},
		{
			// Per-source ratings ride in a nested object. A host that
			// predates it reads only number-valued keys, so the flat four
			// must stay exactly as they were beside it.
			name:    "per-source ratings nest under sources",
			ratings: metadata.Ratings{IMDB: 8.1},
			sources: map[string]metadata.RatingSource{
				metadata.RatingSourceIMDB:       {Score: 81, Votes: 673852},
				metadata.RatingSourceRogerEbert: {Score: 100},
			},
			want: map[string]any{
				"imdb": 8.1,
				"sources": map[string]any{
					"imdb":       map[string]any{"score": float64(81), "votes": float64(673852)},
					"rogerebert": map[string]any{"score": float64(100)},
				},
			},
		},
		{
			name:    "no ratings at all",
			ratings: metadata.Ratings{},
			want:    nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result := ratingsStruct(tt.ratings, tt.sources)
			if tt.want == nil {
				if result != nil {
					t.Fatalf("ratingsStruct() = %v, want nil", result.AsMap())
				}
				return
			}
			if result == nil {
				t.Fatalf("ratingsStruct() = nil, want %v", tt.want)
			}
			if got := result.AsMap(); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("ratingsStruct() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMetadataStruct(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		result metadata.MetadataResult
		want   map[string]any
	}{
		{
			name:   "common sense age",
			result: metadata.MetadataResult{AdvisoryAge: 13, AdvisorySource: metadata.AdvisorySourceCommonSense},
			want:   map[string]any{"advisory_age": float64(13), "advisory_source": "commonsense"},
		},
		{
			name:   "mdblist derived age",
			result: metadata.MetadataResult{AdvisoryAge: 16, AdvisorySource: metadata.AdvisorySourceMDBList},
			want:   map[string]any{"advisory_age": float64(16), "advisory_source": "mdblist"},
		},
		{
			name:   "no advisory age means no metadata struct",
			result: metadata.MetadataResult{AdvisorySource: metadata.AdvisorySourceCommonSense},
			want:   nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result := metadataStruct(&tt.result)
			if tt.want == nil {
				if result != nil {
					t.Fatalf("metadataStruct() = %v, want nil", result.AsMap())
				}
				return
			}
			if result == nil {
				t.Fatalf("metadataStruct() = nil, want %v", tt.want)
			}
			if got := result.AsMap(); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("metadataStruct() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAPIKeyFromConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		entries []*pluginv1.ConfigEntry
		want    string
	}{
		{name: "no entries", entries: nil, want: ""},
		{
			name:    "the account entry declared in the manifest",
			entries: []*pluginv1.ConfigEntry{{Key: "account", Value: mustStruct(t, map[string]any{"api_key": "secret"})}},
			want:    "secret",
		},
		{
			name:    "surrounding whitespace is trimmed",
			entries: []*pluginv1.ConfigEntry{{Key: "account", Value: mustStruct(t, map[string]any{"api_key": "  secret  "})}},
			want:    "secret",
		},
		{
			name:    "a different config entry is ignored",
			entries: []*pluginv1.ConfigEntry{{Key: "settings", Value: mustStruct(t, map[string]any{"api_key": "secret"})}},
			want:    "",
		},
		{
			name:    "an account entry with no value",
			entries: []*pluginv1.ConfigEntry{{Key: "account"}},
			want:    "",
		},
		{
			name:    "an account entry with no api_key field",
			entries: []*pluginv1.ConfigEntry{{Key: "account", Value: mustStruct(t, map[string]any{"other": "x"})}},
			want:    "",
		},
		{
			name:    "a non-string api_key",
			entries: []*pluginv1.ConfigEntry{{Key: "account", Value: mustStruct(t, map[string]any{"api_key": float64(42)})}},
			want:    "",
		},
		{
			name:    "a nil entry is skipped",
			entries: []*pluginv1.ConfigEntry{nil, {Key: "account", Value: mustStruct(t, map[string]any{"api_key": "secret"})}},
			want:    "secret",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := apiKeyFromConfig(tt.entries); got != tt.want {
				t.Fatalf("apiKeyFromConfig() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestConfigureClearsTheKey pins that the host can revoke the account: an
// empty Configure parks the provider instead of leaving the old key in place.
// It asserts through the observable behaviour — whether a lookup reaches
// MDBList — rather than through a getter that exists only for the test.
func TestConfigureClearsTheKey(t *testing.T) {
	t.Parallel()

	rs, ms, api := newServers(t, http.StatusOK, fixtureBody(t))
	lookup := func(want codes.Code) {
		t.Helper()

		_, err := ms.GetMetadata(context.Background(), &pluginv1.GetMetadataRequest{
			ItemType:    "movie",
			ProviderIds: mustStruct(t, map[string]any{"imdb": "tt0073195"}),
		})
		if got := status.Code(err); got != want {
			t.Fatalf("GetMetadata() status = %v (%v), want %v", got, err, want)
		}
	}

	if _, err := rs.Configure(context.Background(), &pluginv1.ConfigureRequest{
		Config: []*pluginv1.ConfigEntry{{Key: "account", Value: mustStruct(t, map[string]any{"api_key": "secret"})}},
	}); err != nil {
		t.Fatalf("Configure() returned error: %v", err)
	}
	lookup(codes.OK)
	if _, keys := api.requests(); !reflect.DeepEqual(keys, []string{"secret"}) {
		t.Fatalf("apikey query values = %v, want [secret] after Configure with a key", keys)
	}

	if _, err := rs.Configure(context.Background(), &pluginv1.ConfigureRequest{}); err != nil {
		t.Fatalf("Configure() returned error: %v", err)
	}
	lookup(codes.FailedPrecondition)
	if _, keys := api.requests(); len(keys) != 1 {
		t.Fatalf("made %d request(s) in total %v, want 1: an empty Configure must park the provider", len(keys), keys)
	}
}

func TestStringMapFromStruct(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		value *structpb.Struct
		want  map[string]string
	}{
		{name: "nil struct", value: nil, want: map[string]string{}},
		{
			name:  "keys are lowercased and trimmed",
			value: mustStruct(t, map[string]any{" IMDB ": "tt0073195", "TMDB": "578"}),
			want:  map[string]string{"imdb": "tt0073195", "tmdb": "578"},
		},
		{
			name:  "empty and non-string values are dropped",
			value: mustStruct(t, map[string]any{"imdb": "", "tmdb": float64(578), "tvdb": nil, "plex": "abc"}),
			want:  map[string]string{"plex": "abc"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := stringMapFromStruct(tt.value); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("stringMapFromStruct() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestManifestContract guards the declarations the enrichment-only design
// depends on: the capability the host looks for, a priority low enough to sit
// under a primary provider, no season or episode priority at all, and the
// account config entry the API key arrives in.
func TestManifestContract(t *testing.T) {
	t.Parallel()

	manifest, err := loadManifest()
	if err != nil {
		t.Fatalf("loadManifest() returned error: %v", err)
	}

	if got, want := manifest.GetPluginId(), "silo.mdblist"; got != want {
		t.Fatalf("plugin_id = %q, want %q", got, want)
	}
	if got := manifest.GetChecksum(); got == "" || got == "__CHECKSUM__" {
		t.Fatalf("checksum = %q, want the running binary's sha256", got)
	}

	capabilities := manifest.GetCapabilities()
	if len(capabilities) != 1 {
		t.Fatalf("got %d capabilities, want 1", len(capabilities))
	}
	capability := capabilities[0]
	if got, want := capability.GetType(), "metadata_provider.v1"; got != want {
		t.Fatalf("capability type = %q, want %q", got, want)
	}
	if got, want := capability.GetId(), "mdblist"; got != want {
		t.Fatalf("capability id = %q, want %q", got, want)
	}

	capabilityMetadata := capability.GetMetadata().AsMap()
	priority, ok := capabilityMetadata["default_priority"].(map[string]any)
	if !ok {
		t.Fatalf("capability metadata has no default_priority object: %v", capabilityMetadata)
	}
	wantPriority := map[string]any{"movie": float64(8), "series": float64(8)}
	if !reflect.DeepEqual(priority, wantPriority) {
		t.Fatalf("default_priority = %v, want %v (season and episode must stay absent)", priority, wantPriority)
	}

	// Silo calls an enrichment-only provider only on the provider-ID keys it
	// declares here. Without them the plugin never runs.
	lookupIDs, ok := capabilityMetadata["lookup_provider_ids"].([]any)
	if !ok {
		t.Fatalf("capability metadata has no lookup_provider_ids list: %v", capabilityMetadata)
	}
	if want := []any{"imdb", "tmdb"}; !reflect.DeepEqual(lookupIDs, want) {
		t.Fatalf("lookup_provider_ids = %v, want %v", lookupIDs, want)
	}
	// Silo's bulk enrichment pass keeps this many lookups in flight, so the
	// client can fill its largest batch (provider.maxBatchSize).
	if got, want := capabilityMetadata["bulk_lookup_limit"], float64(100); got != want {
		t.Fatalf("bulk_lookup_limit = %v, want %v", got, want)
	}
	// required_external_ids means "all of these" to the markers capability;
	// declaring it here would suggest a constraint Silo does not apply.
	if _, declared := capabilityMetadata["required_external_ids"]; declared {
		t.Fatal("capability metadata still declares required_external_ids")
	}

	schemas := manifest.GetGlobalConfigSchema()
	if len(schemas) != 1 {
		t.Fatalf("got %d global config schemas, want 1", len(schemas))
	}
	if got, want := schemas[0].GetKey(), "account"; got != want {
		t.Fatalf("global config schema key = %q, want %q (apiKeyFromConfig reads this key)", got, want)
	}
	if !schemas[0].GetRequired() {
		t.Fatal("the account config schema is not required, want required")
	}
}
