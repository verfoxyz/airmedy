import { describe, it, expect, vi, beforeEach } from 'vitest'
import { createTestingPinia } from '@pinia/testing'
import { setActivePinia } from 'pinia'
import { useTrackContextMenu } from './useTrackContextMenu'
import { usePlayerStore } from '@/stores/player'
import { useAppStore } from '@/stores/app'
import type { TrackDTO } from '../../bindings/airmedy/internal/domain/models'

// Mock i18n
vi.mock('vue-i18n', () => ({
  useI18n: () => ({
    t: (key: string) => key,
  }),
}))

// Mock router
vi.mock('vue-router', () => ({
  useRouter: () => ({
    push: vi.fn(),
  }),
}))

// Mock bindings
vi.mock('../../bindings/airmedy/internal/infra/wails/playerservice', () => ({
  PlayNext: vi.fn(),
  GetStatus: vi.fn().mockResolvedValue({}),
  GetQueue: vi.fn().mockResolvedValue([]),
}))
vi.mock('../../bindings/airmedy/internal/infra/wails/playlistservice', () => ({
  GetPlaylistsForTrack: vi.fn().mockResolvedValue([]),
}))
vi.mock('../../bindings/airmedy/internal/infra/wails/libraryservice', () => ({}))
vi.mock('../../bindings/airmedy/internal/infra/wails/lyricsservice', () => ({
  GetLyrics: vi.fn().mockResolvedValue({ content: '[00:01.00]one' }),
}))
vi.mock('../../bindings/airmedy/internal/infra/wails/downloadservice', () => ({
  RedownloadTrack: vi.fn().mockResolvedValue(undefined),
}))

// Mock @wailsio/runtime for store init and model creation
vi.mock('@wailsio/runtime', () => ({
  Events: { On: vi.fn(), Off: vi.fn() },
  Create: {
    Nullable: (fn: any) => (v: any) => (v == null ? null : fn(v)),
    Array: (fn: any) => (arr: any[]) => (arr ?? []).map(fn),
    Struct: (ctor: any) => (v: any) => (v == null ? null : new ctor(v)),
    Map: (k: any, v: any) => (val: any) => val,
  },
}))

describe('useTrackContextMenu', () => {
  beforeEach(() => {
    setActivePinia(createTestingPinia({
      createSpy: vi.fn,
      stubActions: false,
    }))
  })

  it('excludes "Play Next" if track is currently playing', () => {
    const track: TrackDTO = { id: 'track-1', title: 'Track 1' } as any
    const playerStore = usePlayerStore()
    playerStore.currentTrack = track

    const { buildMenuItems } = useTrackContextMenu(vi.fn())
    
    const items = buildMenuItems(track)
    const playNextItem = items.find(item => item.label === 'context_menu.play_next')
    
    expect(playNextItem).toBeUndefined()
  })

  it('includes "Play Next" if track is not currently playing', () => {
    const track: TrackDTO = { id: 'track-1', title: 'Track 1' } as any
    const otherTrack: TrackDTO = { id: 'track-2', title: 'Track 2' } as any
    const playerStore = usePlayerStore()
    playerStore.currentTrack = otherTrack

    const { buildMenuItems } = useTrackContextMenu(vi.fn())
    
    const items = buildMenuItems(track)
    const playNextItem = items.find(item => item.label === 'context_menu.play_next')
    
    expect(playNextItem).toBeDefined()
    expect(playNextItem?.label).toBe('context_menu.play_next')
  })

  it('puts Create Playlist before the existing playlists', () => {
    const track: TrackDTO = { id: 'track-1', title: 'Track 1' } as any
    const { buildMenuItems } = useTrackContextMenu(vi.fn())

    const addToPlaylist = buildMenuItems(track).find(item => item.label === 'context_menu.add_to_playlist')

    expect(addToPlaylist?.children?.[0].label).toBe('sidebar.new_playlist')
    expect(addToPlaylist?.children?.[1].separator).toBe(true)
  })

  it('excludes "Play Next" if excludePlayNext option is true', () => {
    const track: TrackDTO = { id: 'track-1', title: 'Track 1' } as any
    const otherTrack: TrackDTO = { id: 'track-2', title: 'Track 2' } as any
    const playerStore = usePlayerStore()
    playerStore.currentTrack = otherTrack

    const { buildMenuItems } = useTrackContextMenu(vi.fn())
    
    const items = buildMenuItems(track, { excludePlayNext: true })
    const playNextItem = items.find(item => item.label === 'context_menu.play_next')
    
    expect(playNextItem).toBeUndefined()
  })

  it('excludes "Remove from Queue" if track is currently playing', () => {
    const track: TrackDTO = { id: 'track-1', title: 'Track 1' } as any
    const playerStore = usePlayerStore()
    playerStore.currentTrack = track

    const { buildMenuItems } = useTrackContextMenu(vi.fn())
    
    const items = buildMenuItems(track, { showRemoveFromQueue: true })
    const removeItem = items.find(item => item.label === 'context_menu.remove_from_queue')
    
    expect(removeItem).toBeUndefined()
  })

  it('includes "Remove from Queue" if track is not currently playing', () => {
    const track: TrackDTO = { id: 'track-1', title: 'Track 1' } as any
    const otherTrack: TrackDTO = { id: 'track-2', title: 'Track 2' } as any
    const playerStore = usePlayerStore()
    playerStore.currentTrack = otherTrack

    const { buildMenuItems } = useTrackContextMenu(vi.fn())
    
    const items = buildMenuItems(track, { showRemoveFromQueue: true })
    const removeItem = items.find(item => item.label === 'context_menu.remove_from_queue')
    
    expect(removeItem).toBeDefined()
    expect(removeItem?.label).toBe('context_menu.remove_from_queue')
  })

  it('limits the mini-player menu to queue, radio, and favorite actions', () => {
    const track: TrackDTO = { id: 'track-1', title: 'Track 1' } as any
    const playerStore = usePlayerStore()
    playerStore.currentTrack = { id: 'track-2' } as TrackDTO
    useAppStore().libraryAnalysisEnabled = true

    const { buildMenuItems } = useTrackContextMenu(vi.fn())
    const labels = buildMenuItems(track, { miniPlayer: true, showRemoveFromQueue: true })
      .filter(item => !item.separator)
      .map(item => item.label)

    expect(labels).toEqual([
      'context_menu.remove_from_queue',
      'context_menu.play_next',
      'context_menu.start_mood_radio',
      'context_menu.add_to_favorites',
    ])
  })

  it('offers redownload only for an online track that owns a local file', () => {
    const online = { id: 'online-1', title: 'Online', path: 'C:/Music/song.flac', provider: 'wy' } as TrackDTO
    const localOnly = { id: 'local-1', title: 'Local', path: 'C:/Music/local.flac' } as TrackDTO
    const remote = { id: 'remote-1', title: 'Remote', path: 'remote://wy/1', provider: 'wy' } as TrackDTO
    const { buildMenuItems } = useTrackContextMenu(vi.fn())

    const onlineItem = buildMenuItems(online).find(item => item.label === 'context_menu.redownload')
    const localItem = buildMenuItems(localOnly).find(item => item.label === 'context_menu.redownload')
    const remoteItem = buildMenuItems(remote).find(item => item.label === 'context_menu.redownload')

    expect(onlineItem?.disabled).toBeFalsy()
    expect(localItem?.disabled).toBe(true)
    expect(remoteItem?.disabled).toBe(true)
  })

  it('offers lyric export for a local track but not a remote one', () => {
    const localTrack = { id: 'local-1', title: 'Local', path: 'C:/Music/song.flac' } as TrackDTO
    const remoteTrack = { id: 'remote-1', title: 'Remote', path: 'remote://wy/1' } as TrackDTO
    const { buildMenuItems } = useTrackContextMenu(vi.fn())

    const localItem = buildMenuItems(localTrack).find(item => item.label === 'context_menu.export_lyrics')
    const remoteItem = buildMenuItems(remoteTrack).find(item => item.label === 'context_menu.export_lyrics')

    expect(localItem?.disabled).toBeFalsy()
    expect(remoteItem?.disabled).toBe(true)
  })
})
