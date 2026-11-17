import { computed, ref } from 'vue'
import { defineStore } from 'pinia'
import { Events } from '@wailsio/runtime'
import { foldUnicode } from '@airmedy/utils'
import * as LastFmService from '../../bindings/airmedy/internal/infra/wails/lastfmservice'

/**
 * One local recommendation exclusion.
 *
 * The row lives only in the local database: it hides a song or an artist from
 * Discover and Last.fm radio, never from the library.
 */
export interface DislikeEntry {
  id: string
  kind: string
  match_key: string
  title: string
  artist: string
  created_at: string
}

export const DISLIKE_KIND_TRACK = 'track'
export const DISLIKE_KIND_ARTIST = 'artist'

/** normalize matches the backend's NormalizationKey closely enough to filter
 *  the rendered rows without a round trip. */
function normalize(value: string): string {
  return foldUnicode((value ?? '').trim().toLowerCase()).replace(/\s+/g, ' ')
}

/** artistParts mirrors ArtistKeyParts: a credit list contributes each artist. */
function artistParts(value: string): string[] {
  const parts: string[] = []
  for (const chunk of normalize(value).split(/[,;&/]|\bfeat\.?\b|\bft\.?\b/g)) {
    const part = chunk.trim()
    if (part && !parts.includes(part)) parts.push(part)
  }
  return parts
}

export const useDislikeStore = defineStore('dislike', () => {
  const items = ref<DislikeEntry[]>([])
  const loading = ref(false)
  const saving = ref(false)

  const trackKeys = computed(() => {
    const keys = new Set<string>()
    for (const item of items.value) {
      if (item.kind === DISLIKE_KIND_TRACK && item.match_key) keys.add(item.match_key)
    }
    return keys
  })
  const artistKeys = computed(() => {
    const keys = new Set<string>()
    for (const item of items.value) {
      if (item.kind !== DISLIKE_KIND_ARTIST) continue
      const parts = artistParts(item.artist)
      if (parts.length === 0 && item.match_key) keys.add(normalize(item.match_key))
      for (const part of parts) keys.add(part)
    }
    return keys
  })

  function isArtistDisliked(artist: string): boolean {
    return artistParts(artist).some((part) => artistKeys.value.has(part))
  }

  /** isTrackDisliked reports whether a row is excluded by key or by artist. */
  function isTrackDisliked(matchKey: string | null | undefined, artist: string): boolean {
    if (matchKey && trackKeys.value.has(matchKey)) return true
    return isArtistDisliked(artist)
  }

  function upsert(entry: DislikeEntry | null | undefined) {
    if (!entry?.id) return
    const index = items.value.findIndex((item) => item.id === entry.id)
    if (index === -1) items.value = [entry, ...items.value]
    else items.value[index] = entry
  }

  function replace(next: DislikeEntry[] | null | undefined) {
    items.value = Array.isArray(next) ? next.filter((item) => !!item?.id) : []
  }

  async function load() {
    loading.value = true
    try {
      replace((await LastFmService.ListDislikes()) as DislikeEntry[])
    } catch (error) {
      console.error('Failed to load dislikes', error)
    } finally {
      loading.value = false
    }
  }

  async function dislikeTrack(input: { matchKey?: string; title: string; artist: string }) {
    saving.value = true
    try {
      const entry = (await LastFmService.Dislike(
        DISLIKE_KIND_TRACK,
        input.matchKey ?? '',
        input.title,
        input.artist,
      )) as DislikeEntry
      upsert(entry)
      return entry
    } finally {
      saving.value = false
    }
  }

  async function dislikeArtist(artist: string) {
    saving.value = true
    try {
      const entry = (await LastFmService.Dislike(
        DISLIKE_KIND_ARTIST,
        '',
        '',
        artist,
      )) as DislikeEntry
      upsert(entry)
      return entry
    } finally {
      saving.value = false
    }
  }

  async function remove(id: string) {
    if (!id) return
    saving.value = true
    try {
      await LastFmService.Undislike(id)
      items.value = items.value.filter((item) => item.id !== id)
    } finally {
      saving.value = false
    }
  }

  let _offUpdated: (() => void) | null = null
  let _initialized = false

  function init() {
    if (_initialized) return
    _initialized = true
    _offUpdated = Events.On('dislike:updated', (event: Events.WailsEvent) => {
      const payload = event.data as { items?: DislikeEntry[] } | null
      replace(payload?.items)
    })
    void load()
  }

  function dispose() {
    _offUpdated?.()
    _offUpdated = null
    _initialized = false
  }

  return {
    items,
    loading,
    saving,
    init,
    load,
    dispose,
    isTrackDisliked,
    isArtistDisliked,
    dislikeTrack,
    dislikeArtist,
    remove,
  }
})
