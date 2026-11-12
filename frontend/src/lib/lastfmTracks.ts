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
