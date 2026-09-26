// Package metadata holds the plugin's own request and result types. It has no
// protobuf dependency: main.go translates between these types and the wire
// format, and provider/ works only in these terms.
package metadata

// Advisory sources reported in MetadataResult.AdvisorySource.
const (
	// AdvisorySourceCommonSense marks an age that MDBList sourced from Common
	// Sense Media (the response's "commonsense" flag).
	AdvisorySourceCommonSense = "commonsense"
	// AdvisorySourceMDBList marks an age MDBList derived itself.
	AdvisorySourceMDBList = "mdblist"
)

// MetadataRequest is one enrichment lookup. ProviderIDs carries the external
// IDs a higher-priority provider already resolved; this plugin never searches,
// so without an imdb or tmdb entry there is nothing to look up.
type MetadataRequest struct {
	ProviderIDs map[string]string
	ContentType string // "movie" or "series"
}

// Ratings maps onto Silo's four rating columns, which the host reads from the
// plugin's ratings Struct under the keys "imdb" and "tmdb" (0-10) and
// "rt_critic" and "rt_audience" (0-100). Any other key is ignored host side.
// The per-source conversion that produces these lives in provider.applyRatings.
type Ratings struct {
	IMDB       float64
	TMDB       float64
	RTCritic   float64
	RTAudience float64
}

// Rating source keys used in MetadataResult.RatingSources. They are Silo's
// names, not MDBList's: the host stores them verbatim, so they stay stable even
// if MDBList renames a source (it already spells the Rotten Tomatoes audience
// score three different ways).
const (
	RatingSourceIMDB           = "imdb"
	RatingSourceTMDB           = "tmdb"
	RatingSourceRTCritic       = "rt_critic"
	RatingSourceRTAudience     = "rt_audience"
	RatingSourceMetacritic     = "metacritic"
	RatingSourceMetacriticUser = "metacritic_user"
	RatingSourceTrakt          = "trakt"
	RatingSourceLetterboxd     = "letterboxd"
	RatingSourceRogerEbert     = "rogerebert"
	RatingSourceMyAnimeList    = "myanimelist"
	// RatingSourceMDBList is MDBList's own aggregate score for the title.
	RatingSourceMDBList = "mdblist"
)

// RatingSource is one source's rating on a common 0-100 scale, plus how many
// votes produced it (0 when MDBList does not say). The common scale lets the
// host store every source in one shape; the four typed columns in Ratings keep
// their own scales for compatibility.
type RatingSource struct {
	Score float64
	Votes int64
}

// MetadataResult is what one MDBList lookup contributes; a nil result means
// MDBList had nothing to add. Every field is optional: the host merges
// fill-empty, so anything left at its zero value stays with whichever provider
// supplied it first.
//
// There is deliberately no identity here. This provider never establishes what
// an item is, so it returns no external IDs even though MDBList reports them.
type MetadataResult struct {
	// ContentRating is MDBList's "certification", e.g. "PG".
	ContentRating string

	Ratings Ratings

	// RatingSources carries every source MDBList rated, keyed by the
	// RatingSource* constants. Hosts that predate per-source storage ignore
	// it; it never replaces Ratings.
	RatingSources map[string]RatingSource

	// The fields below fill what the primary provider left empty. The host
	// merges them fill-empty, so none of them can displace TMDB's values.
	// Countries and keywords are deliberately absent: the host unions list
	// fields across providers, so MDBList's would be added to TMDB's rather
	// than fill a blank (see resultFromResponse).
	Year             int
	ReleaseDate      string // movies only
	FirstAirDate     string // shows only
	Runtime          int    // minutes, movies only
	OriginalLanguage string
	Genres           []string
	ShowStatus       string // shows only; the host normalises the spelling

	// AdvisoryAge is the minimum recommended age in years, 0 when unknown.
	AdvisoryAge int
	// AdvisorySource is AdvisorySourceCommonSense or AdvisorySourceMDBList,
	// empty when AdvisoryAge is 0.
	AdvisorySource string
}
