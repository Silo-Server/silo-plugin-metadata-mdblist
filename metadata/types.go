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

	// AdvisoryAge is the minimum recommended age in years, 0 when unknown.
	AdvisoryAge int
	// AdvisorySource is AdvisorySourceCommonSense or AdvisorySourceMDBList,
	// empty when AdvisoryAge is 0.
	AdvisorySource string
}
