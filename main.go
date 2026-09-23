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
	"fmt"
	"os"
	"strings"

	"google.golang.org/protobuf/types/known/structpb"

	"github.com/Silo-Server/silo-plugin-mdblist/metadata"
	"github.com/Silo-Server/silo-plugin-mdblist/provider"
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
		return nil, err
	}
	if result == nil {
		// An empty item, not an error: MDBList having nothing for a title is
		// the normal case for anything it has not indexed.
		return &pluginv1.GetMetadataResponse{}, nil
	}

	return &pluginv1.GetMetadataResponse{Item: metadataItemFromResult(result, req.GetItemType())}, nil
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
		client:   provider.NewClient(),
	}

	runtime.Serve(runtime.ServeConfig{
		Servers: runtime.CapabilityServers{
			Runtime:          rs,
			MetadataProvider: &metadataServer{runtime: rs},
		},
	})
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
// into the item's durable identity host side.
func metadataItemFromResult(result *metadata.MetadataResult, itemType string) *pluginv1.MetadataItem {
	return &pluginv1.MetadataItem{
		ItemType:      itemType,
		ContentRating: result.ContentRating,
		Ratings:       ratingsStruct(result.Ratings),
		Metadata:      advisoryStruct(result),
	}
}

// advisoryStruct carries the Common Sense age advisory in the free-form
// metadata Struct.
//
// Forward-compatible seam: MetadataItem has no typed advisory fields yet (a
// planned additive proto change adds them). Until then these keys ride in the
// open metadata map, which today's host ignores, so emitting them costs
// nothing and the typed fields can take over without a wire break.
func advisoryStruct(result *metadata.MetadataResult) *structpb.Struct {
	if result.AdvisoryAge <= 0 {
		return nil
	}
	return structFromMap(map[string]any{
		"advisory_age":    result.AdvisoryAge,
		"advisory_source": result.AdvisorySource,
	})
}

// ratingsStruct emits the four keys the host reads. A zero is omitted: the
// host merges fill-empty, so an emitted zero would occupy the column without
// carrying a rating.
func ratingsStruct(ratings metadata.Ratings) *structpb.Struct {
	values := make(map[string]any, 4)
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
