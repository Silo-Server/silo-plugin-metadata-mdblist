package provider

import (
	"reflect"
	"testing"

	"github.com/Silo-Server/silo-plugin-metadata-mdblist/metadata"
)

// TestAudienceSourceSpellings pins every name MDBList has been seen to use for
// the Rotten Tomatoes audience score. Matching only one of them is how the
// audience column stayed empty before.
func TestAudienceSourceSpellings(t *testing.T) {
	t.Parallel()

	for _, source := range []string{"audience", "popcorn", "tomatoesaudience", "Popcorn"} {
		t.Run(source, func(t *testing.T) {
			t.Parallel()

			body := ratingsBody(`[{"source":"` + source + `","value":85,"score":85}]`)
			result := resultFromResponse(decodeResponse(t, body), "movie")
			if got, want := result.Ratings.RTAudience, float64(85); got != want {
				t.Fatalf("RTAudience = %v, want %v", got, want)
			}
			if got, want := result.RatingSources[metadata.RatingSourceRTAudience], (metadata.RatingSource{Score: 85}); got != want {
				t.Fatalf("RatingSources[rt_audience] = %+v, want %+v", got, want)
			}
		})
	}
}

// TestRatingSourcesPinnedToJawsFixture fixes the common 0-100 scale per source
// against the captured response, including the one source (rogerebert) that
// has a value but no score.
func TestRatingSourcesPinnedToJawsFixture(t *testing.T) {
	t.Parallel()

	result := resultFromResponse(decodeResponse(t, jawsFixture(t)), "movie")

	want := map[string]metadata.RatingSource{
		metadata.RatingSourceIMDB:       {Score: 81, Votes: 673852},
		metadata.RatingSourceMetacritic: {Score: 87, Votes: 21},
		metadata.RatingSourceTrakt:      {Score: 78, Votes: 14033},
		metadata.RatingSourceRTCritic:   {Score: 97, Votes: 102},
		metadata.RatingSourceTMDB:       {Score: 76, Votes: 10114},
		metadata.RatingSourceLetterboxd: {Score: 80, Votes: 876082},
		// Roger Ebert's 4 of 4 stars, with no MDBList score.
		metadata.RatingSourceRogerEbert: {Score: 100},
		// The response's top-level aggregate.
		metadata.RatingSourceMDBList: {Score: 86},
	}
	if !reflect.DeepEqual(result.RatingSources, want) {
		t.Fatalf("RatingSources = %+v, want %+v", result.RatingSources, want)
	}
}

func TestRatingSourcesConversionRules(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		ratings string
		want    map[string]metadata.RatingSource
	}{
		{
			name:    "an unpinned scale is used only with a score",
			ratings: `[{"source":"metacriticuser","value":7.9,"score":null},{"source":"myanimelist","value":8.4,"score":84}]`,
			want:    map[string]metadata.RatingSource{metadata.RatingSourceMyAnimeList: {Score: 84}},
		},
		{
			name:    "a pinned scale converts a value without a score",
			ratings: `[{"source":"letterboxd","value":7.5,"score":null},{"source":"imdb","value":6.4}]`,
			want: map[string]metadata.RatingSource{
				metadata.RatingSourceLetterboxd: {Score: 75},
				metadata.RatingSourceIMDB:       {Score: 64},
			},
		},
		{
			name:    "a conversion past 100 is dropped rather than stored",
			ratings: `[{"source":"rogerebert","value":5}]`,
			want:    nil,
		},
		{
			name:    "unknown sources are skipped",
			ratings: `[{"source":"someday","value":50,"score":50}]`,
			want:    nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result := resultFromResponse(decodeResponse(t, ratingsBody(tt.ratings)), "movie")
			if !reflect.DeepEqual(result.RatingSources, tt.want) {
				t.Fatalf("RatingSources = %+v, want %+v", result.RatingSources, tt.want)
			}
		})
	}
}

func TestFillFieldsForAMovie(t *testing.T) {
	t.Parallel()

	result := resultFromResponse(decodeResponse(t, jawsFixture(t)), "movie")

	if got, want := result.Year, 1975; got != want {
		t.Fatalf("Year = %d, want %d", got, want)
	}
	if got, want := result.ReleaseDate, "1975-06-20"; got != want {
		t.Fatalf("ReleaseDate = %q, want %q", got, want)
	}
	if result.FirstAirDate != "" || result.ShowStatus != "" {
		t.Fatalf("FirstAirDate %q / ShowStatus %q set on a movie", result.FirstAirDate, result.ShowStatus)
	}
	if got, want := result.Runtime, 124; got != want {
		t.Fatalf("Runtime = %d, want %d", got, want)
	}
	if got, want := result.OriginalLanguage, "en"; got != want {
		t.Fatalf("OriginalLanguage = %q, want %q", got, want)
	}
}

func TestFillFieldsForAShow(t *testing.T) {
	t.Parallel()

	body := `{"type":"show","year":2008,"released":"2008-01-20","runtime":2700,"status":"Ended","language":"EN","country":"us, gb"}`
	result := resultFromResponse(decodeResponse(t, body), "show")

	if got, want := result.FirstAirDate, "2008-01-20"; got != want {
		t.Fatalf("FirstAirDate = %q, want %q", got, want)
	}
	if result.ReleaseDate != "" {
		t.Fatalf("ReleaseDate = %q, want empty for a show", result.ReleaseDate)
	}
	if result.Runtime != 0 {
		t.Fatalf("Runtime = %d, want 0: a show's runtime is not per episode", result.Runtime)
	}
	if got, want := result.ShowStatus, "Ended"; got != want {
		t.Fatalf("ShowStatus = %q, want %q", got, want)
	}
	if got, want := result.OriginalLanguage, "en"; got != want {
		t.Fatalf("OriginalLanguage = %q, want %q", got, want)
	}
}

func TestFillFieldsRejectMalformedValues(t *testing.T) {
	t.Parallel()

	body := `{"type":"movie","year":0,"released":"June 1975","runtime":-5,"language":"  "}`
	result := resultFromResponse(decodeResponse(t, body), "movie")

	if result.Year != 0 || result.ReleaseDate != "" || result.Runtime != 0 || result.OriginalLanguage != "" {
		t.Fatalf("malformed fields leaked through: %+v", result)
	}
}

// TestGenresDecodeEitherShape covers the {"id", "title"} objects the live API
// sends and plain strings.
func TestGenresDecodeEitherShape(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		body       string
		wantGenres []string
	}{
		{
			name:       "objects with a title",
			body:       `{"type":"movie","genres":[{"id":1,"title":"Thriller"},{"id":2,"title":"Horror"},{"id":3,"title":"thriller"}]}`,
			wantGenres: []string{"Thriller", "Horror"},
		},
		{
			name:       "plain strings",
			body:       `{"type":"movie","genres":["Thriller"," "]}`,
			wantGenres: []string{"Thriller"},
		},
		{
			name: "an unexpected shape is dropped without losing the body",
			body: `{"type":"movie","certification":"PG","genres":{"1":"Thriller"}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result := resultFromResponse(decodeResponse(t, tt.body), "movie")
			if !reflect.DeepEqual(result.Genres, tt.wantGenres) {
				t.Fatalf("Genres = %v, want %v", result.Genres, tt.wantGenres)
			}
		})
	}
}
