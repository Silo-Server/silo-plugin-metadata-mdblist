package provider

// mediaResponse is the body of GET /{media_provider}/{media_type}/{media_id}.
// It mirrors testdata/movie_jaws.json and declares only the fields the plugin
// maps: everything else in the response (title, year, overview, artwork,
// streams, watch_providers) belongs to the primary provider and is ignored on
// decode.
type mediaResponse struct {
	// Type is "movie" or "show". It is checked against the route the lookup
	// used so a mis-typed external ID cannot attach a show's ratings and
	// certification to a movie.
	Type string `json:"type"`

	Ratings       []ratingEntry `json:"ratings"`
	Certification string        `json:"certification"`

	// Commonsense is true when AgeRating came from Common Sense Media.
	Commonsense bool `json:"commonsense"`
	// AgeRating is a minimum age in years. Null for titles MDBList has no
	// advisory for, so it is a pointer rather than a plain int.
	AgeRating *int `json:"age_rating"`

	// Error and Response appear instead of a body when MDBList rejects the
	// request — most often an exhausted daily quota. Both are absent on
	// success, so a non-empty Error or an explicit false Response means
	// "no data", never "this title has no ratings".
	Error    string `json:"error"`
	Response *bool  `json:"response"`
}

// ratingEntry is one element of "ratings". Value is on the source's own scale
// and Score is MDBList's 0-100 normalisation; either or both can be null when
// the source has not rated the title. The entry's "url" field is deliberately
// absent here: MDBList types it as a string for some sources and a number for
// others, and the plugin has no use for it.
type ratingEntry struct {
	Source string   `json:"source"`
	Value  *float64 `json:"value"`
	Score  *float64 `json:"score"`
}
