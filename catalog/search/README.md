# Search

## Summary

Full-text search across tracks, albums, artists, playlists, and composers using **Bleve v2**. A single global index stores all entity types. Queries use prefix + exact matching with AND semantics across terms.

## Files

| File                                     | Purpose                         |
| ---------------------------------------- | ------------------------------- |
| `internal/infra/bleve/bleve.go`          | Index creation, indexing, query |
| `internal/domain/search.go`              | SearchService interface         |
| `internal/app/library/search.go`         | Alias expansion, variant merge  |
| `internal/app/library/artist_alias.go`   | Alias table reads and the expansion rules |
| `internal/infra/wails/search_service.go` | Wails binding (entity hydration only) |

## SearchService Interface

```go
type SearchService interface {
    IndexTrack(ctx context.Context, track *TrackDTO) error
    IndexAlbum(ctx context.Context, album *AlbumDTO) error
    IndexArtist(ctx context.Context, artist *Artist) error
    IndexPlaylist(ctx context.Context, playlist *Playlist) error
    IndexComposer(ctx context.Context, composer *Composer) error
    BatchReindex(ctx context.Context, data *ReindexData, onProgress func(path string)) error
    Search(ctx context.Context, query string) ([]SearchResult, error)
    DeleteFromIndex(ctx context.Context, id string) error
    DeleteAlbumFromIndex(ctx context.Context, albumID string) error
    DeleteArtistFromIndex(ctx context.Context, artistID string) error
    DeleteComposerFromIndex(ctx context.Context, composerID string) error
    DocCount(ctx context.Context) (uint64, error)
    Reset(ctx context.Context) error
    Close() error
}
```

## Index Structure

**Storage:** Bleve v2 on-disk index at `$XDG_DATA_HOME/airmedy/airmedy.bleve`.

**Document ID convention:** `{type}:{entity_id}` — e.g., `track:abc123`, `album:def456`.

**Field mappings per document type:**

| Type     | Fields indexed                                                                                 |
| -------- | ---------------------------------------------------------------------------------------------- |
| Track    | `id`, `type`, `title`, `artist_name` (primary), `artist_names` (array), `album_name`, `genres` |
| Album    | `id`, `type`, `title`, `artist_name`, `artist_names`                                           |
| Artist   | `id`, `type`, `title` (= name), `name`                                                         |
| Playlist | `id`, `type`, `title` (= name), `description`                                                  |
| Composer | `id`, `type`, `title` (= name)                                                                 |

**Field types:**

- `id`, `type` — keyword (exact match, stored, not analyzed)
- All other fields — text (analyzed with custom "no_stop_words" analyzer using unicode tokenizer and lowercase filter, preserving English and Vietnamese stop words like "more", "than", "you", "the", "a", etc.; not stored)

All text values are normalized via `domain.FoldUnicode()` before indexing.

## Query Algorithm

```go
func Search(ctx, query string) ([]SearchResult, error)
```

1. **Normalize:** `domain.FoldUnicode(query)` — removes diacritics for accent-insensitive matching.
2. **Tokenize:** Split into terms, strip `*` wildcard characters.
3. **Per-term query:** For each term, build a disjunction (OR) of:
   - Exact term match (MatchQuery)
   - Prefix match (PrefixQuery)
4. **AND all terms:** All per-term disjunctions are combined with a ConjunctionQuery.
5. **Phrase boost:** If query has multiple terms, add an exact phrase MatchPhraseQuery with boost `2.0` — exact phrase matches rank higher.

**Result:** Returns `[]SearchResult{ID, Type, Score}` sorted by score descending. The app-layer `library.SearchService` expands the query through artist aliases and merges the variant results (see below); the Wails binding then resolves full entity objects from the DB for each result ID.

## Alias Expansion

Local search expands a query through the **enabled** artist aliases (`artist_aliases`, the same table matching uses), so `camellia` also finds tracks filed under `かめりあ` and vice versa. Expansion is query-time only: the Bleve index is never rebuilt for an alias.

- **Whole words only.** The whole query is checked first (an artist name may contain spaces), then each whitespace-separated word. A word is replaced only when its `NormalizationKey` equals an alias key, so `camellias` is not rewritten. Case/accent/full-width differences are handled by that normalization.
- **Bidirectional.** One stored direction (alias → canonical) yields both replacements.
- **Bounded.** At most four variants besides the original. Pairs whose key contains the multi-artist separator (`\x1f`) are skipped: they cannot be substituted into a search box as one word.
- **Cached.** The enabled alias list is read once and cached for 30 s; `Add`, `Remove` and `SetEnabled` invalidate it immediately.
- **Merged.** The original query runs first and keeps its rank; variant hits are appended by `type + id` deduplication and the merged list is capped at the per-query limit (200). A variant failure is logged and skipped; only the original query's failure fails the search. A query without a matching alias runs exactly one index query.

No transliteration is inferred: only names explicitly present in the alias table expand.

## SearchResult

```go
type SearchResult struct {
    ID    string   // entity ID (without type prefix)
    Type  string   // "track", "album", "artist", "playlist", "composer"
    Score float64
}
```

## SearchResultSet (Wails binding output)

```typescript
interface SearchResultSet {
  tracks: TrackDTO[];
  albums: AlbumDTO[];
  artists: Artist[];
  playlists: Playlist[];
  playlist_tracks: Record<string, TrackDTO[]>; // first 4 tracks per playlist
  composers: Composer[];
}
```

## Index Lifecycle

| Operation           | When                                                                                                                    |
| ------------------- | ----------------------------------------------------------------------------------------------------------------------- |
| `IndexTrack()`      | After every file import                                                                                                 |
| `IndexAlbum()`      | After album upsert                                                                                                      |
| `IndexPlaylist()`   | On create/update                                                                                                        |
| `DeleteFromIndex()` | On track/playlist delete                                                                                                |
| `Reset()`           | Called at the start of a full reindex to delete the old index files on disk and recreate it with the correct mapping    |
| `ReindexAll()`      | Re-indexes all data (called on settings resync, user-triggered optimize search, or auto-triggered on startup if index is empty) |

## Frontend Integration

**`stores/search.ts`:** Holds `query`, `results`, `loading`. `search(q)` is debounced 300ms.

**`SearchView`** renders `SearchResultSet` in sections: tracks (table), albums (grid), artists (grid), playlists (grid), composers (list).

**`ViewHeader`** in list views contains a search input that routes to `/search` with the query.
