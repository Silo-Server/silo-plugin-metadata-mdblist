package provider

import (
	"context"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/Silo-Server/silo-plugin-metadata-mdblist/metadata"
)

// GetMetadata enriches an item another provider already identified.
//
// It returns (nil, nil) when the request carries no imdb or tmdb ID, when the
// item type is one MDBList does not cover, and when MDBList has nothing to
// say. It returns an error, wrapping one of the sentinels in errors.go, when
// MDBList could not answer: no key, a rejected key, a spent quota, an outage,
// or an unusable answer; and when the caller's context ends.
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

// lookupID picks the ID to query MDBList with. TMDB comes first because
// MDBList's batch endpoint types its IDs as integers: a TMDB ID matches that
// contract, while sending IMDb's tt-prefixed strings relies on behavior the
// schema does not promise. Both routes answer the single-title lookup alike.
func lookupID(providerIDs map[string]string) (idProvider, mediaID string, ok bool) {
	if tmdbID := normalizeTMDbID(providerIDs["tmdb"]); tmdbID != "" {
		return "tmdb", tmdbID, true
	}
	if imdbID := normalizeIMDbID(providerIDs["imdb"]); imdbID != "" {
		return "imdb", imdbID, true
	}
	return "", "", false
}

func normalizeIMDbID(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	digits, ok := strings.CutPrefix(value, "tt")
	if !ok || digits == "" {
		return ""
	}
	if _, err := strconv.ParseUint(digits, 10, 64); err != nil {
		return ""
	}
	return value
}

// normalizeTMDbID returns a TMDB ID in canonical decimal form, so "0578" and
// "578" are one lookup and a batch answer's ids.tmdb matches the request.
func normalizeTMDbID(value string) string {
	id, err := strconv.ParseUint(strings.TrimSpace(value), 10, 64)
	if err != nil || id == 0 {
		return ""
	}
	return strconv.FormatUint(id, 10)
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
// Title, overview and artwork are left out for the same reason in a weaker
// form: they are the primary provider's, localised, and MDBList's are English
// only, so filling a blank with them would put English text on an item whose
// library asked for another language.
//
// Keywords and countries are left out too. The host adds list fields from
// every provider together rather than filling a blank, so MDBList's keywords,
// which are slugs such as "parent-child-relationship" mixed with MDBList's own
// tags such as "has-trailer" and "2k-blu-ray", would pile up on top of TMDB's
// keywords on every title, and its countries on top of TMDB's.
//
// mediaType is the route the lookup used ("movie" or "show"). A body that
// answers with the other type means the external ID resolved to a different
// kind of title, so nothing from it is safe to attach.
func resultFromResponse(response *mediaResponse, mediaType string) *metadata.MetadataResult {
	if responseType := strings.ToLower(strings.TrimSpace(response.Type)); responseType != "" && responseType != mediaType {
		return nil
	}

	result := &metadata.MetadataResult{
		ContentRating:    strings.TrimSpace(response.Certification),
		OriginalLanguage: strings.ToLower(strings.TrimSpace(response.Language)),
		Genres:           cleanLabels(response.Genres),
	}

	if response.Year > 0 {
		result.Year = response.Year
	}
	released := releaseDate(response.Released)
	if mediaType == "show" {
		result.FirstAirDate = released
		result.ShowStatus = strings.TrimSpace(response.Status)
	} else {
		result.ReleaseDate = released
		// A show's runtime is not a per-episode figure, so only a movie's
		// is safe to put in the runtime column.
		if response.Runtime > 0 {
			result.Runtime = response.Runtime
		}
	}

	applyRatings(&result.Ratings, response.Ratings)
	result.RatingSources = ratingSources(response)

	if response.AgeRating != nil && *response.AgeRating > 0 {
		result.AdvisoryAge = *response.AgeRating
		result.AdvisorySource = metadata.AdvisorySourceMDBList
		if response.Commonsense {
			result.AdvisorySource = metadata.AdvisorySourceCommonSense
		}
	}

	return result
}

// releaseDate keeps a date only in the host's YYYY-MM-DD form.
func releaseDate(value string) string {
	value = strings.TrimSpace(value)
	if _, err := time.Parse(time.DateOnly, value); err != nil {
		return ""
	}
	return value
}

// cleanLabels trims, drops empties and removes case-insensitive duplicates.
func cleanLabels(values []string) []string {
	var labels []string
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		key := strings.ToLower(value)
		if value == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		labels = append(labels, value)
	}
	return labels
}

// audienceSources are the names MDBList uses for the Rotten Tomatoes audience
// score. The legacy mdblist.com/api host says "tomatoesaudience" (Silo-Server
// #694), third-party clients of api.mdblist.com read "popcorn", and the API's
// own ratings endpoint calls the source "audience". No captured response pins
// which one this endpoint sends, so all three are accepted.
var audienceSources = map[string]bool{
	"audience":         true,
	"popcorn":          true,
	"tomatoesaudience": true,
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
// Every other source MDBList reports goes to ratingSources instead.
func applyRatings(ratings *metadata.Ratings, entries []ratingEntry) {
	for _, entry := range entries {
		value, hasValue := positive(entry.Value)
		score, hasScore := positive(entry.Score)

		source := strings.ToLower(strings.TrimSpace(entry.Source))
		switch {
		case source == "imdb":
			// Fixture: value 8.1, score 81 -> rating_imdb 8.1 (0-10). Value
			// leads here because it keeps the decimal score rounds away.
			switch {
			case hasValue:
				ratings.IMDB = value
			case hasScore:
				ratings.IMDB = score / 10
			}
		case source == "tmdb":
			// Fixture: value 76, score 76 -> rating_tmdb 7.6 (0-10).
			switch {
			case hasScore:
				ratings.TMDB = score / 10
			case hasValue:
				ratings.TMDB = value / 10
			}
		case source == "tomatoes":
			// Rotten Tomatoes critic score. Fixture: value 97, score 97 ->
			// rating_rt_critic 97 (integer 0-100).
			switch {
			case hasScore:
				ratings.RTCritic = math.Round(score)
			case hasValue:
				ratings.RTCritic = math.Round(value)
			}
		case audienceSources[source]:
			// Rotten Tomatoes audience score -> rating_rt_audience (0-100).
			// No captured response contains an audience entry, so unlike the
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

// valueScales converts a source's own-scale "value" to 0-100 when "score" is
// null. Only scales the Jaws fixture pins are listed; a source missing here is
// used only when MDBList supplied a score.
//
//	imdb        8.1 -> 81   (out of 10)
//	tmdb, trakt, metacritic, tomatoes: value equals score (out of 100)
//	letterboxd  8   -> 80   (MDBList doubles Letterboxd's 5 stars to 10)
//	rogerebert  4   -> 100  (out of 4 stars; the fixture has no score)
var valueScales = map[string]float64{
	metadata.RatingSourceIMDB:       10,
	metadata.RatingSourceTMDB:       1,
	metadata.RatingSourceRTCritic:   1,
	metadata.RatingSourceMetacritic: 1,
	metadata.RatingSourceTrakt:      1,
	metadata.RatingSourceLetterboxd: 10,
	metadata.RatingSourceRogerEbert: 25,
}

// ratingSourceNames maps MDBList's source names onto Silo's keys.
var ratingSourceNames = map[string]string{
	"imdb":             metadata.RatingSourceIMDB,
	"tmdb":             metadata.RatingSourceTMDB,
	"tomatoes":         metadata.RatingSourceRTCritic,
	"audience":         metadata.RatingSourceRTAudience,
	"popcorn":          metadata.RatingSourceRTAudience,
	"tomatoesaudience": metadata.RatingSourceRTAudience,
	"metacritic":       metadata.RatingSourceMetacritic,
	"metacriticuser":   metadata.RatingSourceMetacriticUser,
	"trakt":            metadata.RatingSourceTrakt,
	"letterboxd":       metadata.RatingSourceLetterboxd,
	"rogerebert":       metadata.RatingSourceRogerEbert,
	"myanimelist":      metadata.RatingSourceMyAnimeList,
	"mal":              metadata.RatingSourceMyAnimeList,
}

// ratingSources reports every rated source on the common 0-100 scale, plus
// MDBList's own aggregate score. An unknown or unrated source is skipped.
func ratingSources(response *mediaResponse) map[string]metadata.RatingSource {
	sources := make(map[string]metadata.RatingSource)
	for _, entry := range response.Ratings {
		name, ok := ratingSourceNames[strings.ToLower(strings.TrimSpace(entry.Source))]
		if !ok {
			continue
		}

		score, hasScore := positive(entry.Score)
		if !hasScore {
			value, hasValue := positive(entry.Value)
			scale, pinned := valueScales[name]
			if !hasValue || !pinned {
				continue
			}
			score = value * scale
		}
		if score > 100 {
			// A value that converts past 100 means the scale guess is wrong
			// for this title; storing it would be worse than storing nothing.
			continue
		}

		var votes int64
		if count, ok := positive(entry.Votes); ok {
			votes = int64(count)
		}
		sources[name] = metadata.RatingSource{Score: math.Round(score*10) / 10, Votes: votes}
	}

	if score, ok := positive(response.Score); ok && score <= 100 {
		sources[metadata.RatingSourceMDBList] = metadata.RatingSource{Score: math.Round(score*10) / 10}
	}

	if len(sources) == 0 {
		return nil
	}
	return sources
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
