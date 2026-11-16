import { formatTotalDuration } from '@airmedy/utils'
import type {
  CatalogRowDTO,
  TopTrackItemDTO,
} from '../../bindings/airmedy/internal/infra/wails/models'

/**
 * lastFmCovers returns up to four unique covers and pads the mosaic to four
 * cells, so the card and the detail header never reflow as images arrive.
 */
export function lastFmCovers(urls: (string | null | undefined)[]): string[] {
  const covers: string[] = []
  for (const raw of urls) {
    const url = (raw ?? '').trim()
    if (!url || covers.includes(url) || covers.length >= 4) continue
    covers.push(url)
  }
  while (covers.length > 0 && covers.length < 4) covers.push('')
  return covers
}

/** lastFmCoverClass keeps a 1–4 cover mosaic filling the square. */
export function lastFmCoverClass(covers: string[], index: number): string {
  if (covers.length === 1) return 'col-span-2 row-span-2'
  if (covers.length === 2) return 'row-span-2'
  if (covers.length === 3 && index === 0) return 'row-span-2'
  return ''
}

/** lastFmTotalDuration sums a list's row durations in seconds. */
export function lastFmTotalDuration(items: { duration?: number | null }[]): number {
  return items.reduce((total, item) => total + (item.duration ?? 0), 0)
}

/** formatLastFmDuration words a list's total duration with the shared units. */
export function formatLastFmDuration(
  seconds: number,
  t: (key: string) => string,
): string {
  return formatTotalDuration(seconds, t)
}

/**
 * formatLastFmWeekRange words a weekly chart window. Both bounds are Unix
 * seconds from Last.fm; the end is the next week's start, which is the label
 * the page shows (11/09 – 11/16).
 */
export function formatLastFmWeekRange(from: number | null | undefined, to: number | null | undefined): string {
  if (!from || !to) return ''
  const label = (seconds: number) => {
    const date = new Date(seconds * 1000)
    return `${String(date.getMonth() + 1).padStart(2, '0')}/${String(date.getDate()).padStart(2, '0')}`
  }
  return `${label(from)} – ${label(to)}`
}

/**
 * lastFmRowToCatalogRow adapts a persisted Last.fm row to the shared catalogue
 * action shape, so play/save reuse the search pipeline unchanged.
 */
export function lastFmRowToCatalogRow(item: TopTrackItemDTO): CatalogRowDTO {
  return {
    provider: item.provider,
    external_id: item.external_id,
    title: item.title,
    artists: item.artists?.length ? item.artists : [item.artist],
    album_name: item.album,
    duration: item.duration,
    sources: item.sources ?? [],
    cover_url: item.cover_url,
    cover_url_large: item.cover_url_large,
    downloadable: item.downloadable,
    blocked_reason: item.blocked_reason,
    state: item.state,
    local_track_id: item.local_track_id,
  }
}

/**
 * isPlayableLastFmRow reports whether a row can start playback now: a matched
 * source identity or an on-disk copy, and no pending or "no source" state.
 */
export function isPlayableLastFmRow(item: TopTrackItemDTO): boolean {
  return (
    item.matching !== true &&
    item.no_source !== true &&
    (!!item.provider || !!item.local_track_id)
  )
}

export interface LastFmPlaybackPlan {
  /** Resolved library track ids, in row order. */
  ids: string[]
  /** Rows that could not be played, including the ones still matching. */
  skipped: number
  /** At least one row is still waiting for its source to answer. */
  matching: boolean
}

/**
 * planLastFmPlayback resolves a list's rows one at a time.
 *
 * A row that cannot be resolved only counts as skipped: one dead source must
 * not abort the whole batch, which used to leave "Play all" doing nothing.
 * `matching` lets the caller word an empty result as "still matching" instead
 * of "nothing playable".
 */
export async function planLastFmPlayback(
  rows: TopTrackItemDTO[],
  prepare: (row: CatalogRowDTO) => Promise<string>,
): Promise<LastFmPlaybackPlan> {
  const ids: string[] = []
  let skipped = 0
  let matching = false
  for (const row of rows) {
    if (!isPlayableLastFmRow(row)) {
      if (row.matching === true) matching = true
      skipped++
      continue
    }
    try {
      const id = await prepare(lastFmRowToCatalogRow(row))
      if (id) ids.push(id)
      else skipped++
    } catch (error) {
      skipped++
      console.warn('lastfm: skip a row that could not be prepared', row.match_key, error)
    }
  }
  return { ids, skipped, matching }
}
