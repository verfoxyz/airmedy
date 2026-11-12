import { describe, expect, it } from 'vitest'
import {
  formatLastFmDuration,
  isPlayableLastFmRow,
  lastFmCoverClass,
  lastFmCovers,
  lastFmRowToCatalogRow,
  lastFmTotalDuration,
} from './lastfmTracks'

function row(patch: Record<string, unknown> = {}) {
  return {
    title: 'Song',
    artist: 'Artist',
    album: 'Album',
    duration: 200,
    artists: [] as string[],
    cover_url: 'https://cover/a.jpg',
    cover_url_large: 'https://cover/a-large.jpg',
    matching: false,
    matched: true,
    no_source: false,
    provider: 'wy',
    external_id: '42',
    state: '',
    local_track_id: '',
    blocked_reason: '',
    sources: ['wy'],
    downloadable: true,
    kind: 'top_tracks',
    match_key: 'artist\u0000song',
    ...patch,
  } as Parameters<typeof lastFmRowToCatalogRow>[0]
}

describe('Last.fm row adapter', () => {
  it('maps provider fields and falls back to the single artist', () => {
    const converted = lastFmRowToCatalogRow(row())
    expect(converted).toMatchObject({
      provider: 'wy',
      external_id: '42',
      title: 'Song',
      artists: ['Artist'],
      album_name: 'Album',
      duration: 200,
      cover_url: 'https://cover/a.jpg',
      downloadable: true,
      sources: ['wy'],
    })
  })

  it('uses the full credit list when present', () => {
    const converted = lastFmRowToCatalogRow(row({ artists: ['A', 'B'] }))
    expect(converted.artists).toEqual(['A', 'B'])
  })

  it('treats a source match or a local copy as playable', () => {
    expect(isPlayableLastFmRow(row())).toBe(true)
    expect(isPlayableLastFmRow(row({ provider: '', external_id: '', local_track_id: 'local-1' }))).toBe(true)
    expect(isPlayableLastFmRow(row({ matching: true }))).toBe(false)
    expect(isPlayableLastFmRow(row({ no_source: true }))).toBe(false)
    expect(isPlayableLastFmRow(row({ provider: '', external_id: '' }))).toBe(false)
  })

  it('dedupes and pads a cover mosaic to four cells', () => {
    expect(lastFmCovers([])).toEqual([])
    expect(lastFmCovers(['a.jpg', '', 'a.jpg', 'b.jpg'])).toEqual([
      'a.jpg',
      'b.jpg',
      '',
      '',
    ])
    expect(lastFmCovers(['1', '2', '3', '4', '5'])).toEqual(['1', '2', '3', '4'])
    expect(lastFmCoverClass(['1'], 0)).toBe('col-span-2 row-span-2')
    expect(lastFmCoverClass(['1', '2'], 1)).toBe('row-span-2')
    expect(lastFmCoverClass(['1', '2', '3', '4'], 1)).toBe('')
  })

  it('sums and words a list duration with the shared units', () => {
    expect(lastFmTotalDuration([{ duration: 200 }, {}, { duration: 180 }])).toBe(380)
    const t = (key: string) => key
    expect(formatLastFmDuration(380, t)).toBe('6 common.min')
  })
})
