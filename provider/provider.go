package provider

import (
	"context"
	"math"
	"strconv"
	"strings"

	"github.com/Silo-Server/silo-plugin-mdblist/metadata"
)

// GetMetadata enriches an item another provider already identified.
//
// It returns (nil, nil) — never an error — when the request carries no imdb or
// tmdb ID, when the item type is one MDBList does not cover, and when MDBList
// has nothing to say. Only a cancelled or expired context produces an error.
func (c *Client) GetMetadata(ctx context.Context, req metadata.MetadataRequest) (*metadata.MetadataResult, error) {
	mediaType, ok := mdblistMediaType(req.ContentType)
	if !ok {
		return nil, nil
	}

	idProvider, mediaID, ok := lookupID(req.ProviderIDs)
	if !ok {
		// Nothing upstream resolved. This plugin never searches, so there is
		// no fallback path from here.
		return nil, nil
	}

	response, err := c.FetchMedia(ctx, idProvider, mediaType, mediaID)
	if err != nil {
		return nil, err
	}
	if response == nil {
		return nil, nil
	}

	return resultFromResponse(response, mediaType), nil
}

// lookupID picks the ID to query MDBList with, preferring IMDb because it is
// the ID MDBList itself keys on.
func lookupID(providerIDs map[string]string) (idProvider, mediaID string, ok bool) {
	if imdbID := normalizeIMDbID(providerIDs["imdb"]); imdbID != "" {
		return "imdb", imdbID, true
	}
	if tmdbID := normalizeTMDbID(providerIDs["tmdb"]); tmdbID != "" {
		return "tmdb", tmdbID, true
	}
	return "", "", false
}

func normalizeIMDbID(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if !strings.HasPrefix(value, "tt") || len(value) <= 2 {
		return ""
	}
	return value
}

func normalizeTMDbID(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if _, err := strconv.ParseInt(value, 10, 64); err != nil {
		return ""
	}
	return value
}

// mdblistMediaType maps Silo's item type onto MDBList's path segment. Seasons
// and episodes are absent on purpose: MDBList works at title level only, and
// the manifest's default_priority omits them so the host does not ask.
func mdblistMediaType(itemType string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(itemType)) {
	case "movie":
		return "movie", true
	case "series", "show", "tv":
		return "show", true
	default:
		return "", false
	}
}

// resultFromResponse maps one MDBList body onto the fields Silo can store.
//
// It deliberately contributes no identity. MDBList's "ids" object is a
// Trakt-derived, community-edited mapping, and the host merges whatever a
// provider returns into the item's durable identity set; an enrichment-only
// provider inventing an IMDb or TVDB ID the request never carried would send
// lower-priority providers off to resolve a title nobody verified.
//
// mediaType is the route the lookup used ("movie" or "show"). A body that
// answers with the other type means the external ID resolved to a different
// kind of title, so nothing from it is safe to attach.
func resultFromResponse(response *mediaResponse, mediaType string) *metadata.MetadataResult {
	if responseType := strings.ToLower(strings.TrimSpace(response.Type)); responseType != "" && responseType != mediaType {
		return nil
	}

	result := &metadata.MetadataResult{
		ContentRating: strings.TrimSpace(response.Certification),
	}

	applyRatings(&result.Ratings, response.Ratings)

	if response.AgeRating != nil && *response.AgeRating > 0 {
		result.AdvisoryAge = *response.AgeRating
		result.AdvisorySource = metadata.AdvisorySourceMDBList
		if response.Commonsense {
			result.AdvisorySource = metadata.AdvisorySourceCommonSense
		}
	}

	return result
}

// applyRatings fills Silo's four rating columns. This is the one conversion in
// the plugin that corrupts data silently when it is wrong, so every case below
// is pinned to testdata/movie_jaws.json by TestRatingsPinnedToJawsFixture.
//
// Each entry carries both "value", on the source's own scale, and "score",
// MDBList's normalisation. Only "score" has a fixed scale: every non-null score
// in the fixture is out of 100 (imdb 81, metacritic 87, trakt 78, tomatoes 97,
// tmdb 76, letterboxd 80), while "value" varies by source (imdb 8.1 of 10,
// tmdb 76 of 100, tomatoes 97 of 100, rogerebert 4 stars). So "score" is the
// input, and "value" is only a fallback where the fixture pins its scale.
//
// MDBList reports more sources than these four (metacritic, trakt, letterboxd,
// rogerebert, myanimelist). Silo has no column for them and the host drops any
// ratings key it does not recognise, so they are skipped.
func applyRatings(ratings *metadata.Ratings, entries []ratingEntry) {
	for _, entry := range entries {
		value, hasValue := positive(entry.Value)
		score, hasScore := positive(entry.Score)

		switch strings.ToLower(strings.TrimSpace(entry.Source)) {
		case "imdb":
			// Fixture: value 8.1, score 81 -> rating_imdb 8.1 (0-10). Value
			// leads here because it keeps the decimal score rounds away.
			switch {
			case hasValue:
				ratings.IMDB = value
			case hasScore:
				ratings.IMDB = score / 10
			}
		case "tmdb":
			// Fixture: value 76, score 76 -> rating_tmdb 7.6 (0-10).
			switch {
			case hasScore:
				ratings.TMDB = score / 10
			case hasValue:
				ratings.TMDB = value / 10
			}
		case "tomatoes":
			// Rotten Tomatoes critic score. Fixture: value 97, score 97 ->
			// rating_rt_critic 97 (integer 0-100).
			switch {
			case hasScore:
				ratings.RTCritic = math.Round(score)
			case hasValue:
				ratings.RTCritic = math.Round(value)
			}
		case "audience":
			// Rotten Tomatoes audience score -> rating_rt_audience (0-100).
			// No captured response contains an "audience" entry, so unlike the
			// three above, its "value" scale is unobserved. Score is used on
			// its own: if MDBList ever reports audience the way it reports
			// rogerebert (a 0-5 star figure in "value"), reading "value" would
			// write 5 into a column whose real answer is 90.
			if hasScore {
				ratings.RTAudience = math.Round(score)
			}
		}
	}
}

// positive reports a rating figure MDBList actually supplied.
//
// Zero doubles as the absent sentinel, so a genuine 0% Tomatometer is dropped
// rather than stored. That is a host limitation, not a choice made here: the
// host's own merge skips a zero source value and only writes the column when
// the value is above zero, so emitting a bare 0 would be discarded anyway.
// Distinguishing the two needs nullable rating fields host side.
func positive(value *float64) (float64, bool) {
	if value == nil || *value <= 0 {
		return 0, false
	}
	return *value, true
}
