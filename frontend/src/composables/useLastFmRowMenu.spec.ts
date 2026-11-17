import { beforeEach, describe, expect, it, vi } from 'vitest'
import { createPinia, setActivePinia } from 'pinia'

const mocks = vi.hoisted(() => ({
  prepare: vi.fn(),
  save: vi.fn(),
  tracks: vi.fn(),
  playTrackIds: vi.fn(),
  playNext: vi.fn(),
  append: vi.fn(),
  addToPlaylist: vi.fn(),
  startRadio: vi.fn(),
  push: vi.fn(),
}))

vi.mock('vue-i18n', () => ({
  useI18n: () => ({ t: (key: string) => key }),
}))

vi.mock('vue-router', () => ({
  useRouter: () => ({ push: mocks.push }),
}))

vi.mock('@wailsio/runtime', () => ({
  Events: { On: vi.fn(() => () => {}) },
  Create: {
    Nullable: (fn: any) => (v: any) => (v == null ? null : fn(v)),
    Array: (fn: any) => (arr: any[]) => (arr ?? []).map(fn),
    Struct: (ctor: any) => (v: any) => (v == null ? null : new ctor(v)),
    Map: () => (val: any) => val,
    Any: (val: any) => val,
  },
}))

vi.mock('../../bindings/airmedy/internal/infra/wails/catalogservice', () => ({
  PrepareCatalogPlayback: mocks.prepare,
  SaveCatalogResult: mocks.save,
}))

vi.mock('../../bindings/airmedy/internal/infra/wails/libraryservice', () => ({
  GetTracksByIDs: mocks.tracks,
  ToggleFavorite: vi.fn(),
}))

vi.mock('../../bindings/airmedy/internal/infra/wails/playerservice', () => ({
  PlayTrackIDs: mocks.playTrackIds,
  PlayNext: mocks.playNext,
  AppendTracks: mocks.append,
}))

vi.mock('../../bindings/airmedy/internal/infra/wails/playlistservice', () => ({
  AddTrackToPlaylist: mocks.addToPlaylist,
}))

vi.mock('../../bindings/airmedy/internal/infra/wails/lastfmservice', () => ({
  StartRadio: mocks.startRadio,
  GetRadioActive: vi.fn().mockResolvedValue(false),
}))

import { useLastFmRowMenu } from './useLastFmRowMenu'
import { usePlayerStore } from '@/stores/player'
import type { TopTrackItemDTO } from '../../bindings/airmedy/internal/infra/wails/models'

function item(overrides: Partial<TopTrackItemDTO> = {}): TopTrackItemDTO {
  return {
    title: 'Song',
    artist: 'Artist',
    artists: ['Artist'],
    album: 'Album',
    duration: 200,
    match_key: 'artist\x00song',
    provider: 'wy',
    external_id: '1',
    state: '',
    local_track_id: '',
    matching: false,
    no_source: false,
    ...overrides,
  } as TopTrackItemDTO
}

function build(target: TopTrackItemDTO, hooks = {
  onRefresh: vi.fn(),
  onDislikeTrack: vi.fn(),
  onDislikeArtist: vi.fn(),
}) {
  setActivePinia(createPinia())
  const menu = useLastFmRowMenu(hooks)
  return { items: menu.buildMenuItems(target), hooks }
}

function labels(items: { label?: string }[]): string[] {
  return items.filter(entry => !!entry.label).map(entry => entry.label as string)
}

function entry(items: any[], label: string) {
  return items.find((candidate) => candidate.label === label)
}

describe('useLastFmRowMenu', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    mocks.prepare.mockReset()
    mocks.save.mockReset()
    mocks.tracks.mockReset()
    mocks.playTrackIds.mockReset()
    mocks.playNext.mockReset()
    mocks.append.mockReset()
    mocks.addToPlaylist.mockReset()
    mocks.startRadio.mockReset()
    mocks.push.mockReset()
    mocks.prepare.mockResolvedValue('track-1')
    mocks.tracks.mockResolvedValue([{ id: 'track-1', is_favorite: false, artists: [{ id: 'artist-1' }], album: { id: 'album-1' } }])
    mocks.save.mockResolvedValue({})
  })

  it('orders the menu and disables play actions while a row is matching', () => {
    const { items } = build(item({ matching: true }))

    expect(labels(items)).toEqual([
      'context_menu.play',
      'context_menu.play_next',
      'context_menu.add_to_queue',
      'context_menu.start_radio',
      'context_menu.add_to_playlist',
      'lastfm.save_to_library',
      'context_menu.go_to_artist',
      'context_menu.go_to_album',
      'lastfm.copy_row',
      'lastfm.refresh_item',
      'lastfm.dislike_track',
      'lastfm.dislike_artist',
    ])
    for (const label of [
      'context_menu.play',
      'context_menu.play_next',
      'context_menu.add_to_queue',
      'context_menu.start_radio',
      'context_menu.add_to_playlist',
      'lastfm.save_to_library',
    ]) {
      expect(entry(items, label)?.disabled).toBe(true)
    }
    expect(entry(items, 'lastfm.copy_row')?.disabled).toBeUndefined()
    expect(entry(items, 'lastfm.dislike_track')?.disabled).toBeUndefined()
  })

  it('offers favorite instead of save for a row already in the library', () => {
    const { items } = build(item({ state: 'in_library', local_track_id: 'local-1' }))

    expect(entry(items, 'lastfm.save_to_library')).toBeUndefined()
    expect(entry(items, 'context_menu.add_to_favorites')).toBeDefined()
  })

  it('resolves the row for playback and queue actions', async () => {
    const { items } = build(item())
    await entry(items, 'context_menu.play').action()
    expect(mocks.prepare).toHaveBeenCalledOnce()
    const player = usePlayerStore()
    expect(player.queueIds).toBeDefined()
    expect(mocks.playTrackIds).toHaveBeenCalledWith(['track-1'], 0)

    await entry(items, 'context_menu.play_next').action()
    expect(mocks.playNext).toHaveBeenCalledWith(expect.objectContaining({ id: 'track-1' }))

    await entry(items, 'context_menu.add_to_queue').action()
    expect(mocks.append).toHaveBeenCalledWith([expect.objectContaining({ id: 'track-1' })])
  })

  it('saves the row through the catalog save path', async () => {
    const { items } = build(item())
    await entry(items, 'lastfm.save_to_library').action()
    expect(mocks.save).toHaveBeenCalledWith(expect.objectContaining({
      provider: 'wy', external_id: '1', title: 'Song',
    }))
  })

  it('opens the resolved artist and album pages', async () => {
    const { items } = build(item())
    await entry(items, 'context_menu.go_to_artist').action()
    expect(mocks.push).toHaveBeenCalledWith('/artists/artist-1')
    await entry(items, 'context_menu.go_to_album').action()
    expect(mocks.push).toHaveBeenCalledWith('/albums/album-1')
  })

  it('forwards refresh and dislike to the view', () => {
    const { items, hooks } = build(item())
    const target = item()
    entry(items, 'lastfm.refresh_item').action()
    expect(hooks.onRefresh).toHaveBeenCalled()
    entry(items, 'lastfm.dislike_track').action()
    expect(hooks.onDislikeTrack).toHaveBeenCalled()
    entry(items, 'lastfm.dislike_artist').action()
    expect(hooks.onDislikeArtist).toHaveBeenCalled()
  })

  it('copies the artist and title from the row', async () => {
    const writeText = vi.fn().mockResolvedValue(undefined)
    Object.assign(navigator, { clipboard: { writeText } })
    const { items } = build(item())
    await entry(items, 'lastfm.copy_row').action()
    expect(writeText).toHaveBeenCalledWith('Artist - Song')
  })
})
