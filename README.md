# Silo MDBList plugin

Fills in ratings and age ratings that other metadata providers leave empty:
IMDb, TMDB, Rotten Tomatoes critic and audience, the release certification, and
the Common Sense Media minimum age.

## Why it has to sit below a primary provider

This plugin never identifies an item. It has no search: `Search` returns zero
results by design, even though MDBList has a `/search` endpoint. It only looks
up titles that another provider has already matched, using the IMDb or TMDB ID
that provider resolved.

Silo runs a library's metadata providers as a priority-ordered chain and merges
the results fill-empty, so the first provider to supply a field keeps it. Put a
primary provider — TMDB — above MDBList. The primary resolves identity and
fills the bulk of the record; MDBList then adds the ratings columns the primary
had nothing for.

Given no IMDb or TMDB ID in the request, the plugin returns an empty result. It
does not guess, and it does not fall back to searching.

## What it maps

| MDBList | Silo |
| --- | --- |
| `ratings[source=imdb]` | `rating_imdb` (0-10) |
| `ratings[source=tmdb]` | `rating_tmdb` (0-10) |
| `ratings[source=tomatoes]` | `rating_rt_critic` (0-100) |
| `ratings[source=audience]` | `rating_rt_audience` (0-100) |
| `certification` | content rating |
| `age_rating` + `commonsense` | `advisory_age` / `advisory_source` |

Nothing else. The plugin maps no titles, overviews, artwork or external IDs:
MDBList returns an `ids` object, but an enrichment-only provider must not hand
the host identity it did not verify, so that object is not even parsed.

MDBList reports each rating twice: `value` on the source's own scale and
`score` normalised to 0-100. The scales are not uniform — IMDb's `value` is out
of 10, TMDB's and Rotten Tomatoes' are out of 100, Roger Ebert's is out of 5 —
so `score` is the input wherever it is present, and the per-source conversion
is pinned to `provider/testdata/movie_jaws.json`. Sources Silo has no column
for (Metacritic, Trakt, Letterboxd, Roger Ebert, MyAnimeList) are skipped.

The Common Sense age has no typed field in the plugin API, so it rides in the
free-form metadata map under `advisory_age` and `advisory_source`, which the
host reads.

## Known limitations

**Requires a Silo server that reads `lookup_provider_ids`.** Silo used to call
a metadata provider only when the item carried an ID of the provider's own,
which an enrichment-only provider never has. The manifest now declares
`capabilities[0].metadata.lookup_provider_ids: ["imdb", "tmdb"]`, and servers
that understand the key call this plugin whenever the item carries either ID.
An older server ignores the key; the plugin then installs and configures but
contributes nothing.

A genuine 0% Rotten Tomatoes score is reported as "no score". Zero is the
absent sentinel in the host's rating merge and columns, so it cannot currently
be told apart from unrated; fixing it needs nullable rating fields host side.

## Setup

1. Create an API key in your [MDBList](https://mdblist.com) preferences.
2. Install the plugin and paste the key into its settings.
3. Add **MDBList** to a library's metadata provider chain at a *lower* priority
   than the primary provider.

Without a key the plugin stays idle and contributes nothing.

## Rate limits and failure behavior

MDBList meters requests per day: 1000 on the free tier, then 10k, 25k, 100k and
250k by paid tier. The plugin applies a conservative client-side limiter so a
large scan does not arrive as one burst.

A metadata refresh never fails because of MDBList. No API key, a rejected key,
an unknown title, an exhausted quota, or an outage all resolve to "no data",
leaving those fields for another provider. The only error the plugin propagates
is the host cancelling the request.

## Building

```sh
make build        # host platform
make build-all    # linux/amd64, linux/arm64, darwin/arm64
```
