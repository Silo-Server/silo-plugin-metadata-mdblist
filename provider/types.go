package provider

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
)

// mediaResponse is the body of GET /{media_provider}/{media_type}/{media_id}
// and one element of the batch endpoint's array. It mirrors
// testdata/movie_jaws.json and declares only the fields the plugin maps or
// needs: title, overview, artwork, trailer, streams and watch providers belong
// to the primary provider and are ignored on decode.
type mediaResponse struct {
	// Type is "movie" or "show". It is checked against the route the lookup
	// used so a mis-typed external ID cannot attach a show's ratings and
	// certification to a movie.
	Type string `json:"type"`

	// IDs correlates a batch answer with the ID it was requested by. It is
	// read for nothing else: the plugin never returns identity to the host.
	IDs mediaIDs `json:"ids"`

	Ratings       []ratingEntry `json:"ratings"`
	Certification string        `json:"certification"`
	// Score is MDBList's own 0-100 aggregate of the ratings above.
	Score *float64 `json:"score"`

	// Commonsense is true when AgeRating came from Common Sense Media.
	Commonsense bool `json:"commonsense"`
	// AgeRating is a minimum age in years. Null for titles MDBList has no
	// advisory for, so it is a pointer rather than a plain int.
	AgeRating *int `json:"age_rating"`

	// Descriptive fields the host merges fill-empty, so they only land where
	// the primary provider left a blank.
	Year     int       `json:"year"`
	Released string    `json:"released"`
	Runtime  int       `json:"runtime"`
	Language string    `json:"language"`
	Country  string    `json:"country"`
	Status   string    `json:"status"`
	Genres   labelList `json:"genres"`
	// Keywords is present only when the request asked for
	// append_to_response=keyword.
	Keywords labelList `json:"keywords"`

	// Error and Response appear instead of a body when MDBList rejects the
	// request — most often an exhausted daily quota. Both are absent on
	// success, so a non-empty Error or an explicit false Response means
	// "no data", never "this title has no ratings".
	Error    string `json:"error"`
	Response *bool  `json:"response"`
}

// mediaIDs is the response's "ids" object. MDBList types imdb as a string and
// tmdb as a number (null when unknown), so tmdb is decoded leniently.
type mediaIDs struct {
	IMDB string     `json:"imdb"`
	TMDB flexibleID `json:"tmdb"`
}

// flexibleID accepts a JSON number or string and keeps its decimal text.
type flexibleID string

func (id *flexibleID) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if bytes.Equal(data, []byte("null")) {
		*id = ""
		return nil
	}
	var text string
	if err := json.Unmarshal(data, &text); err == nil {
		*id = flexibleID(strings.TrimSpace(text))
		return nil
	}
	var number json.Number
	if err := json.Unmarshal(data, &number); err != nil {
		// An unexpected shape here must not discard an otherwise good body:
		// the ID is only used to correlate batch answers.
		*id = ""
		return nil
	}
	if value, err := strconv.ParseInt(number.String(), 10, 64); err == nil {
		*id = flexibleID(strconv.FormatInt(value, 10))
		return nil
	}
	*id = ""
	return nil
}

// labelList decodes a list of names that MDBList may send either as plain
// strings or as objects carrying the name under "title" or "name". The live
// shape of "genres" and "keywords" is not pinned by a captured response, so
// both forms are accepted and anything else is dropped rather than failing the
// whole body.
type labelList []string

func (l *labelList) UnmarshalJSON(data []byte) error {
	var raw []json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		*l = nil
		return nil
	}

	labels := make([]string, 0, len(raw))
	for _, entry := range raw {
		var text string
		if err := json.Unmarshal(entry, &text); err == nil {
			labels = append(labels, text)
			continue
		}
		var object struct {
			Title string `json:"title"`
			Name  string `json:"name"`
		}
		if err := json.Unmarshal(entry, &object); err == nil {
			if object.Title != "" {
				labels = append(labels, object.Title)
			} else {
				labels = append(labels, object.Name)
			}
		}
	}
	*l = labels
	return nil
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
	Votes  *float64 `json:"votes"`
}
