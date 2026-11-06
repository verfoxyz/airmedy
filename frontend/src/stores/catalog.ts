import { defineStore } from 'pinia'
import { computed, ref } from 'vue'
import { Events } from '@wailsio/runtime'
import * as CatalogService from '../../bindings/airmedy/internal/infra/wails/catalogservice'
import * as LibraryService from '../../bindings/airmedy/internal/infra/wails/libraryservice'
import type { TrackDTO } from '../../bindings/airmedy/internal/domain/models'
import type {
  CatalogFailureDTO,
  CatalogRowDTO,
  CatalogSourceDTO,
} from '../../bindings/airmedy/internal/infra/wails/models'

/**
 * Cloud search state.
 *
 * It is deliberately its own store rather than a branch of `search`: local
 * results are library rows (tracks/albums/artists) while cloud rows are
 * (provider, externalId) identities with their own states, sources and paging.
 * Sharing one store would mean every consumer branching on the mode.
 *
 * The page size mirrors the backend cap: the service clamps a request to 50 and
 * `hasMore` is derived from the merged slice, so "load more" is an offset bump.
 */
const PAGE_SIZE = 50
const SEARCH_DEBOUNCE_MS = 300

export type CatalogMode = 'local' | 'cloud'

export interface CatalogSaveOptions {
  favorite?: boolean
  playlistId?: string
  /** forceDownload ignores a matched local file and fetches the provider copy. */
  forceDownload?: boolean
}

/** identityKey is the row identity, the same one the backend registers. */
export function identityKey(provider: string, externalId: string): string {
  return `${provider}/${externalId}`
}

export const useCatalogStore = defineStore('catalog', () => {
  const mode = ref<CatalogMode>('local')
  const query = ref('')
  /** activeSource is the provider id being searched; '' means "全部". */
  const activeSource = ref('')
  const rows = ref<CatalogRowDTO[]>([])
  const sources = ref<CatalogSourceDTO[]>([])
  const failures = ref<CatalogFailureDTO[]>([])
  const hasMore = ref(false)
  const loading = ref(false)
  const loadingMore = ref(false)
  /** errorKind is a page-level failure kind the view words itself. */
  const errorKind = ref('')
  /** actionError is set when a save failed; it is a kind, not a message. */
  const actionError = ref('')

  /** identity -> track id, so a download event can update the row it belongs to. */
  const trackIds = ref<Record<string, string>>({})

  let requestVersion = 0
  let debounceTimer: ReturnType<typeof setTimeout> | null = null

  const hasQuery = computed(() => query.value.trim() !== '')

  function clearResults() {
    rows.value = []
    failures.value = []
    hasMore.value = false
    errorKind.value = ''
  }

  async function run(reset: boolean, version: number) {
    const text = query.value.trim()
    if (!text) {
      clearResults()
      loading.value = false
      loadingMore.value = false
      return
    }

    const offset = reset ? 0 : rows.value.length
    try {
      const page = await CatalogService.SearchCatalog({
        query: text,
        sources: activeSource.value ? [activeSource.value] : [],
        limit: PAGE_SIZE,
        offset,
      })
      if (version !== requestVersion) return

      sources.value = (page.sources ?? []).filter(Boolean) as CatalogSourceDTO[]
      failures.value = (page.failures ?? []).filter(Boolean) as CatalogFailureDTO[]
      errorKind.value = page.error_kind ?? ''
      hasMore.value = !!page.has_more

      const incoming = (page.rows ?? []).filter(Boolean) as CatalogRowDTO[]
      rows.value = reset ? incoming : [...rows.value, ...incoming]
    } catch (e) {
      if (version !== requestVersion) return
      console.error('Catalog search failed', e)
      // The binding only returns an error for an unexpected failure; the UI has
      // no wording for it, so it reads as "every source failed".
      errorKind.value = 'unavailable'
      if (reset) clearResults()
    } finally {
      if (version === requestVersion) {
        loading.value = false
        loadingMore.value = false
      }
    }
  }

  function cancelDebounce() {
    if (debounceTimer) {
      clearTimeout(debounceTimer)
      debounceTimer = null
    }
  }

  /** search starts a fresh page from the keyword, debounced while typing. */
  function search(text: string) {
    query.value = text
    cancelDebounce()
    const version = ++requestVersion

    if (!text.trim()) {
      clearResults()
      loading.value = false
      loadingMore.value = false
      return
    }

    loading.value = true
    debounceTimer = setTimeout(() => {
      debounceTimer = null
      void run(true, version)
    }, SEARCH_DEBOUNCE_MS)
  }

  /**
   * ensureSearch restores the stored page instead of re-querying it.
   *
   * Switching modes or returning to the page with the same keyword keeps the
   * rows, paging position, source and `hasMore` the user already loaded. A
   * changed keyword, an empty result set or an in-flight request still goes
   * through `search`.
   */
  function ensureSearch(text: string) {
    const next = text.trim()
    if (next === query.value.trim()) {
      if (!next || loading.value || loadingMore.value || rows.value.length > 0) return
    }
    search(text)
  }

  /** setSource switches the source filter and re-runs immediately (no debounce). */
  function setSource(sourceId: string) {
    activeSource.value = sourceId
    cancelDebounce()
    if (!hasQuery.value) return
    const version = ++requestVersion
    loading.value = true
    void run(true, version)
  }

  /**
   * setMode only flips the view.
   *
   * Searching is the view's job: it calls `search` when the keyword applies to
   * the mode being entered, so switching back and forth never loses either
   * result set and never fires a request the user did not ask for.
   */
  function setMode(next: CatalogMode) {
    mode.value = next
  }

  /** loadMore appends the next merged page. */
  function loadMore() {
    if (!hasMore.value || loading.value || loadingMore.value || !hasQuery.value) return
    loadingMore.value = true
    const version = requestVersion
    void run(false, version)
  }

  /** applyState updates the row for one identity after an action or an event. */
  function applyState(provider: string, externalId: string, state: string) {
    rows.value = rows.value.map((row) =>
      row.provider === provider && row.external_id === externalId ? { ...row, state } : row,
    )
  }

  /**
   * save registers the row and starts its download.
   *
   * Both buttons use it: favorite and "add to playlist" are the same download
   * intent with a different side effect (see the backend's Act).
   */
  async function save(row: CatalogRowDTO, options: CatalogSaveOptions = {}) {
    actionError.value = ''
    try {
      const result = await CatalogService.SaveCatalogResult({
        provider: row.provider,
        external_id: row.external_id,
        title: row.title,
        artists: row.artists,
        album_name: row.album_name,
        duration: row.duration,
        // The file's embedded artwork must be the large rendition; a source that
        // does not distinguish sizes falls back to the list thumbnail.
        cover_url: row.cover_url_large || row.cover_url || '',
        playlist_id: options.playlistId ?? '',
        favorite: options.favorite ?? false,
        local_track_id: options.forceDownload ? '' : (row.local_track_id ?? ''),
        force_download: options.forceDownload ?? false,
      })
      const id = identityKey(row.provider, row.external_id)
      if (result.track_id) {
        trackIds.value = { ...trackIds.value, [id]: result.track_id }
      }
      applyState(row.provider, row.external_id, result.state || 'downloading')
      return true
    } catch (e) {
      console.error('Catalog save failed', e)
      actionError.value = 'save_failed'
      return false
    }
  }

  function identityForTrack(trackId: string): string | null {
    for (const [identity, id] of Object.entries(trackIds.value)) {
      if (id === trackId) return identity
    }
    return null
  }

  /**
   * refreshRow re-reads the row state by identity.
   *
   * `download:done` names the remote track id, but the merge has already deleted
   * that row and moved its identity onto the downloaded file. Looking the state
   * up by id therefore returned nothing and the chip stayed "downloading" until
   * the page was searched again. The identity survives the merge, so it is the
   * right key.
   */
  async function refreshRow(trackId: string) {
    const identity = identityForTrack(trackId)
    if (!identity) return
    const [provider, ...rest] = identity.split('/')
    const externalId = rest.join('/')
    try {
      const states = ((await CatalogService.GetCatalogRowStates([
        { provider, external_id: externalId },
      ])) ?? []) as Array<{ provider: string; external_id: string; state?: string }>
      const match = states.find((entry) => entry?.provider === provider && entry.external_id === externalId)
      if (match?.state) {
        applyState(provider, externalId, match.state)
        return
      }
    } catch (e) {
      console.error('Catalog: cannot re-read the row state', e)
    }

    // Fallback for a build without the identity column: read the saved row.
    let tracks: TrackDTO[] = []
    try {
      tracks = ((await LibraryService.GetTracksByIDs([trackId])) ?? []) as TrackDTO[]
    } catch (e) {
      console.error('Catalog: cannot re-read the saved track', e)
      return
    }
    const track = tracks.filter(Boolean)[0]
    if (!track) return
    const onDisk = !track.path?.startsWith('remote://') && track.availability === 'ready'
    applyState(provider, externalId, onDisk ? 'in_library' : track.availability === 'failed' ? 'failed' : 'downloading')
  }

  /** onDownloadDone re-reads the row: in-library, and pointing at the real file. */
  function onDownloadDone(event: Events.WailsEvent) {
    const trackId = (event.data as { track_id?: string } | null)?.track_id
    if (!trackId) return
    if (!identityForTrack(trackId)) return
    void refreshRow(trackId)
  }

  /** onDownloadFailed marks the matching row as failed (the UI offers a retry). */
  function onDownloadFailed(event: Events.WailsEvent) {
    const trackId = (event.data as { track_id?: string } | null)?.track_id
    if (!trackId) return
    const identity = identityForTrack(trackId)
    if (!identity) return
    const [provider, ...rest] = identity.split('/')
    applyState(provider, rest.join('/'), 'failed')
  }

  const _offDone = Events.On('download:done', onDownloadDone)
  const _offFailed = Events.On('download:failed', onDownloadFailed)

  /** dispose releases the event listeners (call from onUnmounted or the app teardown). */
  function dispose() {
    _offDone()
    _offFailed()
  }

  function clear() {
    cancelDebounce()
    requestVersion++
    query.value = ''
    activeSource.value = ''
    clearResults()
    sources.value = []
    actionError.value = ''
    loading.value = false
    loadingMore.value = false
  }

  return {
    mode,
    query,
    activeSource,
    rows,
    sources,
    failures,
    hasMore,
    loading,
    loadingMore,
    errorKind,
    actionError,
    hasQuery,
    setMode,
    setSource,
    search,
    ensureSearch,
    loadMore,
    save,
    clear,
    dispose,
  }
})