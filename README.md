# Silo MDBList plugin

Fills in what other metadata providers leave empty: ratings from every source
MDBList aggregates (IMDb, TMDB, Rotten Tomatoes critic and audience,
Metacritic, Letterboxd, Trakt, Roger Ebert, MyAnimeList, and MDBList's own
score), the release certification, the Common Sense Media minimum age, and
basic facts such as year, release date, runtime, language, country, genres,
keywords, and show status.

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
| `ratings[source=popcorn\|tomatoesaudience\|audience]` | `rating_rt_audience` (0-100) |
| every rated source, plus the top-level `score` | `ratings.sources` (0-100 with vote counts; see below) |
| `certification` | content rating |
| `age_rating` + `commonsense` | `advisory_age` / `advisory_source` |
| `year` | year |
| `released` | release date (movies) or first air date (shows) |
| `runtime` | runtime (movies only; a show's figure is not per episode) |
| `language` | original language |
| `country` | countries (uppercased to match TMDB's ISO codes) |
| `genres` | genres |
| `keywords` (requested with `append_to_response=keyword`) | keywords |
| `status` | show status (shows only; the host normalises the spelling) |

Silo merges a library's providers fill-empty, so every one of these only lands
where the primary provider left a blank. Countries and keywords are unioned
host side. Genres go to whichever provider supplies them first.

Nothing else. The plugin maps no titles, overviews, taglines, artwork, trailers
or external IDs. MDBList's text is English only and would override the
library's language wherever the primary provider left a blank. The host keeps
every provider's trailers, so MDBList's would duplicate TMDB's. And an
enrichment-only provider must not hand the host identity it did not verify, so
MDBList's `ids` object is read only to match batch answers to requests.

MDBList reports each rating twice: `value` on the source's own scale and
`score` normalised to 0-100. The scales are not uniform — IMDb's `value` is out
of 10, TMDB's and Rotten Tomatoes' are out of 100, Letterboxd's is doubled to
10, Roger Ebert's is out of 4 — so `score` is the input wherever it is present,
and the per-source conversion is pinned to `provider/testdata/movie_jaws.json`.

### Per-source ratings

Alongside the four flat keys, the ratings Struct carries a `sources` object:

```json
{
  "imdb": 8.1, "tmdb": 7.6, "rt_critic": 97,
  "sources": {
    "imdb":       {"score": 81, "votes": 673852},
    "metacritic": {"score": 87, "votes": 21},
    "letterboxd": {"score": 80, "votes": 876082},
    "rogerebert": {"score": 100},
    "mdblist":    {"score": 86}
  }
}
```

Keys are `imdb`, `tmdb`, `rt_critic`, `rt_audience`, `metacritic`,
`metacritic_user`, `trakt`, `letterboxd`, `rogerebert`, `myanimelist`, and
`mdblist`. Every `score` is 0-100; `votes` is omitted when MDBList has no
count. Silo servers that predate per-source storage read only number-valued
keys and skip `sources`, so the plugin sends it to every server version.

The Common Sense age has no typed field in the plugin API, so it rides in the
free-form metadata map under `advisory_age` and `advisory_source`, which the
host reads. Keywords ride there too, under `keywords`.

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

## Rate limits, batching and failure behavior

MDBList meters requests per day: 1000 on the free tier, then 10k, 25k, 100k and
250k by paid tier, resetting at 00:00 UTC. Every tier is also capped at 1000
reads per fixed five-minute window.

- **Batching.** Silo asks for one item at a time but runs several match workers
  at once. Lookups for the same route (IMDb or TMDB, movie or show) that arrive
  within 250 ms of each other go out as one request to MDBList's batch
  endpoint, up to 100 IDs. A lookup with no partner uses the ordinary
  single-title request. If MDBList refuses a batch, the plugin retries it in
  halves; when both halves go through, it remembers the smaller limit for that
  route. TMDB IDs are preferred over IMDb IDs for lookups because the batch
  endpoint's schema types IDs as integers.
- **Quota pause.** When MDBList answers 429, the plugin stops sending requests
  until `Retry-After` (or, for the daily quota, until 00:00 UTC). When a
  successful answer reports `X-RateLimit-Remaining: 0`, it pauses until
  `X-RateLimit-Reset` without spending another request. Saving a different API
  key lifts the pause.
- **Pacing.** A client-side limiter keeps requests at three a second, under the
  five-minute cap.

A metadata refresh never fails because of MDBList. No API key, a rejected key,
an unknown title, an exhausted quota, or an outage all resolve to "no data",
leaving those fields for another provider. The only error the plugin propagates
is the host cancelling the request.

## Building

```sh
make build        # host platform
make build-all    # linux/amd64, linux/arm64, darwin/arm64
```
