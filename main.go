// Command plugin is the MDBList metadata provider for Silo.
//
// It is enrichment only. It never identifies an item: Search returns nothing,
// and GetMetadata works purely from the IMDb or TMDB ID a higher-priority
// provider already resolved. Run it below a primary provider such as TMDB.
package main

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/Silo-Server/silo-plugin-metadata-mdblist/metadata"
	"github.com/Silo-Server/silo-plugin-metadata-mdblist/provider"
	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	publicmanifest "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/manifest"
	"github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtime"
)

// version is set at build time via -ldflags "-X main.version=...".
var version string

//go:embed manifest.json
var manifestJSON []byte

type runtimeServer struct {
	pluginv1.UnimplementedRuntimeServer

	manifest *pluginv1.PluginManifest
	client   *provider.Client
}

type metadataServer struct {
	pluginv1.UnimplementedMetadataProviderServer
	runtime *runtimeServer
}

func (s *runtimeServer) GetManifest(context.Context, *pluginv1.GetManifestRequest) (*pluginv1.GetManifestResponse, error) {
	return &pluginv1.GetManifestResponse{Manifest: s.manifest}, nil
}

func (s *runtimeServer) Configure(_ context.Context, req *pluginv1.ConfigureRequest) (*pluginv1.ConfigureResponse, error) {
	s.client.SetAPIKey(apiKeyFromConfig(req.GetConfig()))
	return &pluginv1.ConfigureResponse{}, nil
}

// Search always returns nothing. MDBList has a /search endpoint, but using it
// would put this plugin in competition with the primary provider for identity,
// which is exactly what an enrichment provider must not do. Returning an empty
// result rather than an error keeps the host's match logs clean.
func (s *metadataServer) Search(context.Context, *pluginv1.SearchMetadataRequest) (*pluginv1.SearchMetadataResponse, error) {
	return &pluginv1.SearchMetadataResponse{}, nil
}

func (s *metadataServer) GetMetadata(ctx context.Context, req *pluginv1.GetMetadataRequest) (*pluginv1.GetMetadataResponse, error) {
	result, err := s.runtime.client.GetMetadata(ctx, metadata.MetadataRequest{
		ProviderIDs: stringMapFromStruct(req.GetProviderIds()),
		ContentType: req.GetItemType(),
	})
	if err != nil {
		return nil, lookupStatus(err)
	}
	if result == nil {
		// An empty item, not an error: MDBList having nothing for a title is
		// the normal case for anything it has not indexed.
		return &pluginv1.GetMetadataResponse{}, nil
	}

	return &pluginv1.GetMetadataResponse{Item: metadataItemFromResult(result, req.GetItemType())}, nil
}

// lookupStatus maps a lookup failure to the gRPC status Silo reads it by. The
// statuses follow the host's contract for bulk enrichment providers: every
// status but INTERNAL means "MDBList cannot answer right now, ask again later",
// and INTERNAL means this one title's answer was unusable. A cancelled or
// expired caller context passes through for gRPC to map.
func lookupStatus(err error) error {
	var code codes.Code
	switch {
	case errors.Is(err, provider.ErrQuotaExhausted):
		code = codes.ResourceExhausted
	case errors.Is(err, provider.ErrNotConfigured):
		code = codes.FailedPrecondition
	case errors.Is(err, provider.ErrKeyRejected):
		code = codes.Unauthenticated
	case errors.Is(err, provider.ErrUnavailable):
		code = codes.Unavailable
	case errors.Is(err, provider.ErrUnusableAnswer):
		code = codes.Internal
	default:
		return err
	}
	return status.Error(code, err.Error())
}

// The remaining MetadataProvider RPCs exist because the host calls every
// method on every provider in a library's chain. MDBList has no data for any
// of them, so each answers empty and successfully; returning an error here
// would add a failure to every match and refresh in the host.

func (s *metadataServer) GetPersonDetail(context.Context, *pluginv1.GetPersonDetailRequest) (*pluginv1.GetPersonDetailResponse, error) {
	return &pluginv1.GetPersonDetailResponse{}, nil
}

func (s *metadataServer) GetSeasons(context.Context, *pluginv1.GetSeasonsRequest) (*pluginv1.GetSeasonsResponse, error) {
	return &pluginv1.GetSeasonsResponse{}, nil
}

func (s *metadataServer) GetEpisodes(context.Context, *pluginv1.GetEpisodesRequest) (*pluginv1.GetEpisodesResponse, error) {
	return &pluginv1.GetEpisodesResponse{}, nil
}

func (s *metadataServer) GetImages(context.Context, *pluginv1.GetImagesRequest) (*pluginv1.GetImagesResponse, error) {
	return &pluginv1.GetImagesResponse{}, nil
}

func (s *metadataServer) ResolveImageURL(context.Context, *pluginv1.ResolveImageURLRequest) (*pluginv1.ResolveImageURLResponse, error) {
	return &pluginv1.ResolveImageURLResponse{}, nil
}

func (s *metadataServer) ResolveImageURLs(context.Context, *pluginv1.ResolveImageURLsRequest) (*pluginv1.ResolveImageURLsResponse, error) {
	return &pluginv1.ResolveImageURLsResponse{}, nil
}

func main() {
	manifest, err := loadManifest()
	if err != nil {
		panic(err)
	}

	rs := &runtimeServer{
		manifest: manifest,
		client:   newClient(),
	}

	runtime.Serve(runtime.ServeConfig{
		Servers: runtime.CapabilityServers{
			Runtime:          rs,
			MetadataProvider: &metadataServer{runtime: rs},
		},
	})
}

// newClient builds the MDBList client with a User-Agent naming this build.
func newClient() *provider.Client {
	client := provider.NewClient()
	userAgent := "silo-plugin-metadata-mdblist"
	if version != "" {
		userAgent += "/" + version
	}
	client.SetUserAgent(userAgent)
	return client
}

func loadManifest() (*pluginv1.PluginManifest, error) {
	manifest, err := publicmanifest.Load(manifestJSON)
	if err != nil {
		return nil, fmt.Errorf("load embedded manifest: %w", err)
	}

	if version != "" {
		manifest.Version = version
	}

	executablePath, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve executable path: %w", err)
	}
	binaryData, err := os.ReadFile(executablePath)
	if err != nil {
		return nil, fmt.Errorf("read executable %q: %w", executablePath, err)
	}
	checksum := sha256.Sum256(binaryData)
	manifest.Checksum = hex.EncodeToString(checksum[:])

	return manifest, nil
}

// apiKeyFromConfig reads the "account" config entry declared in
// manifest.json's global_config_schema.
func apiKeyFromConfig(entries []*pluginv1.ConfigEntry) string {
	for _, entry := range entries {
		if entry == nil || entry.GetKey() != "account" || entry.GetValue() == nil {
			continue
		}
		if raw, ok := entry.GetValue().AsMap()["api_key"].(string); ok {
			return strings.TrimSpace(raw)
		}
	}
	return ""
}

// metadataItemFromResult builds the wire item. It sets no ProviderIds: this
// provider never identifies an item, and anything it put there would be merged
// into the item's durable identity set host side. Every other field is merged
// fill-empty, so it only lands where the primary provider left a blank.
func metadataItemFromResult(result *metadata.MetadataResult, itemType string) *pluginv1.MetadataItem {
	return &pluginv1.MetadataItem{
		ItemType:         itemType,
		ContentRating:    result.ContentRating,
		Ratings:          ratingsStruct(result.Ratings, result.RatingSources),
		Metadata:         metadataStruct(result),
		Year:             int32(result.Year),
		ReleaseDate:      result.ReleaseDate,
		FirstAirDate:     result.FirstAirDate,
		Runtime:          int32(result.Runtime),
		OriginalLanguage: result.OriginalLanguage,
		Genres:           result.Genres,
		Status:           result.ShowStatus,
	}
}

// metadataStruct carries what MetadataItem has no typed field for, in the
// free-form metadata Struct under keys the host reads and allow-lists.
//
// The Common Sense age advisory rides under "advisory_age" and
// "advisory_source"; typed proto fields remain a later additive option. The
// host also reads "keywords" from here, which this plugin never sends (see
// resultFromResponse).
func metadataStruct(result *metadata.MetadataResult) *structpb.Struct {
	values := make(map[string]any, 2)
	if result.AdvisoryAge > 0 {
		values["advisory_age"] = result.AdvisoryAge
		values["advisory_source"] = result.AdvisorySource
	}
	return structFromMap(values)
}

// ratingsStruct emits the four keys the host stores in typed columns, plus a
// "sources" object with every source MDBList rated.
//
// The four flat keys keep their scales (imdb and tmdb 0-10, rt_critic and
// rt_audience 0-100). A zero is omitted: the host merges fill-empty, so an
// emitted zero would occupy the column without carrying a rating.
//
// "sources" maps a source name to {"score": 0-100, "votes": n}. Hosts that
// predate per-source storage read only number-valued keys and skip it, so it
// is safe to send to every host version.
func ratingsStruct(ratings metadata.Ratings, sources map[string]metadata.RatingSource) *structpb.Struct {
	values := make(map[string]any, 5)
	if ratings.IMDB > 0 {
		values["imdb"] = ratings.IMDB
	}
	if ratings.TMDB > 0 {
		values["tmdb"] = ratings.TMDB
	}
	if ratings.RTCritic > 0 {
		values["rt_critic"] = ratings.RTCritic
	}
	if ratings.RTAudience > 0 {
		values["rt_audience"] = ratings.RTAudience
	}
	if len(sources) > 0 {
		perSource := make(map[string]any, len(sources))
		for name, source := range sources {
			entry := map[string]any{"score": source.Score}
			if source.Votes > 0 {
				entry["votes"] = float64(source.Votes)
			}
			perSource[name] = entry
		}
		values["sources"] = perSource
	}
	return structFromMap(values)
}

// stringMapFromStruct reads the external IDs the primary provider resolved.
// Keys are normalised because the plugin only ever looks two of them up by
// name, and a host or third-party client that sent "IMDB" should not silently
// park the provider.
func stringMapFromStruct(value *structpb.Struct) map[string]string {
	result := make(map[string]string)
	if value == nil {
		return result
	}
	for key, raw := range value.AsMap() {
		text, ok := raw.(string)
		if !ok || text == "" {
			continue
		}
		result[strings.ToLower(strings.TrimSpace(key))] = text
	}
	return result
}

func structFromMap(value map[string]any) *structpb.Struct {
	if len(value) == 0 {
		return nil
	}
	result, err := structpb.NewStruct(value)
	if err != nil {
		return nil
	}
	return result
}
