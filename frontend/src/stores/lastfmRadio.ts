import { defineStore } from 'pinia'
import { ref } from 'vue'
import { Events } from '@wailsio/runtime'
import * as LastFmService from '../../bindings/airmedy/internal/infra/wails/lastfmservice'
import * as PlayerService from '../../bindings/airmedy/internal/infra/wails/playerservice'
import type { TrackDTO } from '../../bindings/airmedy/internal/domain/models'
import { useMoodRadioStore } from './moodRadio'
import { usePlayerStore } from './player'

/** SeedRadio names the row the user picked, with or without a library id. */
export interface RadioSeedRequest {
  trackId?: string
  provider?: string
  externalId?: string
  title: string
  artist: string
  album?: string
  duration?: number
}

const APPENDED_NOTICE_MS = 4000

export const useLastFmRadioStore = defineStore('lastfmRadio', () => {
  const active = ref(false)
  const starting = ref(false)
  const appendedCount = ref(0)

  let activeVersion = 0
  let pendingStart = false
  let noticeTimer: ReturnType<typeof setTimeout> | null = null
  let _offState: (() => void) | null = null
  let _offAppended: (() => void) | null = null
  let _initialized = false

  async function syncActive() {
    const version = activeVersion
    const next = await LastFmService.GetRadioActive()
    if (version === activeVersion) active.value = next
  }

  function setActive(next: boolean) {
    activeVersion += 1
    active.value = next
  }

  async function start(seed: RadioSeedRequest) {
    if (pendingStart) return
    pendingStart = true
    starting.value = true
    try {
      const result = await LastFmService.StartRadio({
        track_id: seed.trackId ?? '',
        provider: seed.provider ?? '',
        external_id: seed.externalId ?? '',
        title: seed.title,
        artist: seed.artist,
        album: seed.album ?? '',
        duration: seed.duration ?? 0,
      })
      const tracks = (result?.tracks ?? []).filter(Boolean) as TrackDTO[]
      const seedTrack = result?.seed
      if (!seedTrack) return
      // The two radio modes share one queue; starting this one ends the other.
      const moodRadio = useMoodRadioStore()
      if (moodRadio.active) moodRadio.stop()
      const playerStore = usePlayerStore()
      const queue = [seedTrack, ...tracks]
      if (playerStore.currentTrack?.id === seedTrack.id) {
        // The pick is already playing; replace only the upcoming queue,
        // keeping the current position instead of reloading the seed.
        playerStore.queue = queue
        await PlayerService.ReplaceQueueKeepingCurrentTrackIDs(queue.map(track => track.id))
      } else {
        // start Mood Radio off: an ordinary queue replace must not leave both
        // modes claiming the queue.
        await playerStore.playTracks(queue, 0)
      }
      setActive(true)
    } catch (error) {
      console.error('Last.fm radio start failed', error)
      setActive(false)
    } finally {
      pendingStart = false
      starting.value = false
    }
  }

  function stop() {
    activeVersion += 1
    active.value = false
    clearNotice()
    void LastFmService.StopRadio()
  }

  function clearNotice() {
    appendedCount.value = 0
    if (noticeTimer) {
      clearTimeout(noticeTimer)
      noticeTimer = null
    }
  }

  function showAppended(count: number) {
    if (count <= 0) return
    appendedCount.value = count
    if (noticeTimer) clearTimeout(noticeTimer)
    noticeTimer = setTimeout(() => {
      appendedCount.value = 0
      noticeTimer = null
    }, APPENDED_NOTICE_MS)
  }

  function init() {
    if (_initialized) return
    _initialized = true
    _offState = Events.On('radio:state', (event: Events.WailsEvent) => {
      activeVersion += 1
      active.value = Boolean(event.data)
      if (!active.value) clearNotice()
    })
    _offAppended = Events.On('radio:queue-appended', (event: Events.WailsEvent) => {
      const payload = event.data as { count?: number } | null
      showAppended(Number(payload?.count ?? 0))
    })
    void syncActive()
  }

  function dispose() {
    _offState?.()
    _offAppended?.()
    _offState = null
    _offAppended = null
    _initialized = false
    clearNotice()
  }

  return {
    active,
    starting,
    appendedCount,
    start,
    stop,
    init,
    dispose,
  }
})
