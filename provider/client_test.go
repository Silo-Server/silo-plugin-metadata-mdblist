package provider

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

func TestFetchMediaDecodesTheFixture(t *testing.T) {
	t.Parallel()

	var gotAccept string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAccept = r.Header.Get("Accept")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(jawsFixture(t)))
	}))
	t.Cleanup(server.Close)

	client := NewClient()
	client.SetBaseURL(server.URL)
	client.SetAPIKey("test-key")

	response, err := client.FetchMedia(context.Background(), "imdb", "movie", "tt0073195")
	if err != nil {
		t.Fatalf("FetchMedia() returned error: %v", err)
	}
	if response == nil {
		t.Fatal("FetchMedia() returned no response, want the decoded fixture")
	}

	if got, want := gotAccept, "application/json"; got != want {
		t.Fatalf("Accept header = %q, want %q", got, want)
	}
	if got, want := response.Type, "movie"; got != want {
		t.Fatalf("Type = %q, want %q", got, want)
	}
	if got, want := response.Certification, "PG"; got != want {
		t.Fatalf("Certification = %q, want %q", got, want)
	}
	if !response.Commonsense {
		t.Fatal("Commonsense = false, want true")
	}
	if response.AgeRating == nil || *response.AgeRating != 13 {
		t.Fatalf("AgeRating = %v, want 13", response.AgeRating)
	}
	if got, want := len(response.Ratings), 9; got != want {
		t.Fatalf("len(Ratings) = %d, want %d", got, want)
	}
}

// TestFetchMediaWithoutAPIKeyMakesNoRequest pins that an unconfigured client
// parks instead of calling MDBList, and that the key is trimmed so an admin
// form value of only spaces counts as unconfigured.
func TestFetchMediaWithoutAPIKeyMakesNoRequest(t *testing.T) {
	t.Parallel()

	for _, apiKey := range []string{"", "   ", "\t\n"} {
		t.Run("api key "+strconv.Quote(apiKey), func(t *testing.T) {
			t.Parallel()

			api := newFakeAPI(t, http.StatusOK, jawsFixture(t))
			client := api.client(apiKey)

			response, err := client.FetchMedia(context.Background(), "imdb", "movie", "tt0073195")
			if err != nil {
				t.Fatalf("FetchMedia() returned error: %v", err)
			}
			if response != nil {
				t.Fatalf("FetchMedia() = %+v, want no response", response)
			}
			if paths, _ := api.requests(); len(paths) != 0 {
				t.Fatalf("made %d request(s) %v, want none", len(paths), paths)
			}
		})
	}
}

func TestFetchMediaRejectsIncompleteArguments(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		idProvider string
		mediaType  string
		mediaID    string
	}{
		{name: "no id provider", mediaType: "movie", mediaID: "tt0073195"},
		{name: "no media type", idProvider: "imdb", mediaID: "tt0073195"},
		{name: "no media id", idProvider: "imdb", mediaType: "movie"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			api := newFakeAPI(t, http.StatusOK, jawsFixture(t))
			client := api.client("test-key")

			response, err := client.FetchMedia(context.Background(), tt.idProvider, tt.mediaType, tt.mediaID)
			if err != nil {
				t.Fatalf("FetchMedia() returned error: %v", err)
			}
			if response != nil {
				t.Fatalf("FetchMedia() = %+v, want no response", response)
			}
			if paths, _ := api.requests(); len(paths) != 0 {
				t.Fatalf("made %d request(s) %v, want none", len(paths), paths)
			}
		})
	}
}

// TestFetchMediaTurnsEveryMDBListFailureIntoNoData is the availability
// contract at its source: whatever MDBList answers, the client reports "no
// data" rather than an error, so a metadata refresh never fails on its
// account.
func TestFetchMediaTurnsEveryMDBListFailureIntoNoData(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		status int
		body   string
	}{
		{name: "401 rejected key", status: http.StatusUnauthorized, body: `{"error":"Invalid API key"}`},
		{name: "403 forbidden", status: http.StatusForbidden, body: ``},
		{name: "404 unknown title", status: http.StatusNotFound, body: `{"error":"not found"}`},
		{name: "429 quota or burst limit", status: http.StatusTooManyRequests, body: `{"error":"limit"}`},
		{name: "500 upstream outage", status: http.StatusInternalServerError, body: `oops`},
		{name: "503 upstream outage", status: http.StatusServiceUnavailable, body: ``},
		{name: "200 with an error body", status: http.StatusOK, body: `{"error":"API request limit reached"}`},
		{name: "200 with response false", status: http.StatusOK, body: `{"response":false}`},
		{name: "200 with truncated json", status: http.StatusOK, body: `{"ratings":[`},
		{name: "200 with a non-json body", status: http.StatusOK, body: `<html>nope</html>`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			api := newFakeAPI(t, tt.status, tt.body)
			client := api.client("test-key")

			response, err := client.FetchMedia(context.Background(), "imdb", "movie", "tt0073195")
			if err != nil {
				t.Fatalf("FetchMedia() returned error: %v", err)
			}
			if response != nil {
				t.Fatalf("FetchMedia() = %+v, want no response", response)
			}
		})
	}
}

func TestFetchMediaUnreachableHostIsNotAnError(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	baseURL := server.URL
	server.Close()

	client := NewClient()
	client.SetBaseURL(baseURL)
	client.SetAPIKey("test-key")

	response, err := client.FetchMedia(context.Background(), "imdb", "movie", "tt0073195")
	if err != nil {
		t.Fatalf("FetchMedia() returned error: %v", err)
	}
	if response != nil {
		t.Fatalf("FetchMedia() = %+v, want no response", response)
	}
}

func TestFetchMediaPropagatesContextCancellation(t *testing.T) {
	t.Parallel()

	api := newFakeAPI(t, http.StatusOK, jawsFixture(t))
	client := api.client("test-key")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := client.FetchMedia(ctx, "imdb", "movie", "tt0073195"); !errors.Is(err, context.Canceled) {
		t.Fatalf("FetchMedia() error = %v, want context.Canceled", err)
	}
	if paths, _ := api.requests(); len(paths) != 0 {
		t.Fatalf("made %d request(s) %v, want none", len(paths), paths)
	}
}
