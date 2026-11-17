import { beforeEach, describe, expect, it, vi } from 'vitest'
import { createPinia, setActivePinia } from 'pinia'

const mockStartRadio = vi.fn()
const mockStopRadio = vi.fn()
const mockGetRadioActive = vi.fn().mockResolvedValue(false)
const mockPlayTrackIDs = vi.fn()
const mockReplaceQueueKeepingCurrentTrackIDs = vi.fn()
const listeners = new Map<string, (event: { data: unknown }) => void>()

vi.mock('@wailsio/runtime', () => ({
  Events: {
    On: vi.fn((name: string, listener: (event: { data: unknown }) => void) => {
      listeners.set(name, listener)
      return () => { listeners.delete(name) }
    }),
  },
  Create: {
    Nullable: (fn: (value: unknown) => unknown) => (value: unknown) => value == null ? null : fn(value),
    Array: (fn: (value: unknown) => unknown) => (values: unknown[]) => (values ?? []).map(fn),
    Struct: (ctor: new (value: unknown) => unknown) => (value: unknown) => value == null ? null : new ctor(value),
    Map: () => (value: unknown) => value,
  },
  Call: { ByID: vi.fn() },
}))

vi.mock('../../bindings/airmedy/internal/infra/wails/lastfmservice', () => ({
  StartRadio: (...args: unknown[]) => mockStartRadio(...args),
  StopRadio: (...args: unknown[]) => mockStopRadio(...args),
  GetRadioActive: () => mockGetRadioActive(),
}))

vi.mock('../../bindings/airmedy/internal/infra/wails/playerservice', () => ({
  PlayTrackIDs: (...args: unknown[]) => mockPlayTrackIDs(...args),
  ReplaceQueueKeepingCurrentTrackIDs: (...args: unknown[]) =>
    mockReplaceQueueKeepingCurrentTrackIDs(...args),
}))

import { useLastFmRadioStore } from './lastfmRadio'
import { usePlayerStore } from './player'

function seedTrack() {
  return { id: 'seed', title: 'Seed Song' }
}

function generated() {
  return [{ id: 'gen-1', title: 'Generated One' }, { id: 'gen-2', title: 'Generated Two' }]
}

describe('useLastFmRadioStore', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    vi.clearAllMocks()
    listeners.clear()
    mockStartRadio.mockResolvedValue({ seed: seedTrack(), tracks: generated() })
    mockStopRadio.mockResolvedValue(undefined)
    mockGetRadioActive.mockResolvedValue(false)
    mockPlayTrackIDs.mockResolvedValue(undefined)
    mockReplaceQueueKeepingCurrentTrackIDs.mockResolvedValue(undefined)
  })

  it('plays the returned seed and queue, then marks the radio active', async () => {
    const radio = useLastFmRadioStore()
    await radio.start({ trackId: 'seed', title: 'Seed Song', artist: 'Seed Artist' })

    expect(mockStartRadio).toHaveBeenCalledWith(expect.objectContaining({
      track_id: 'seed',
      title: 'Seed Song',
      artist: 'Seed Artist',
    }))
    expect(mockPlayTrackIDs).toHaveBeenCalledWith(['seed', 'gen-1', 'gen-2'], 0)
    expect(radio.active).toBe(true)
  })

  it('keeps the playing seed and replaces the upcoming queue', async () => {
    const player = usePlayerStore()
    player.currentTrack = seedTrack() as never
    const radio = useLastFmRadioStore()

    await radio.start({ trackId: 'seed', title: 'Seed Song', artist: 'Seed Artist' })

    expect(mockPlayTrackIDs).not.toHaveBeenCalled()
    expect(mockReplaceQueueKeepingCurrentTrackIDs).toHaveBeenCalledWith(['seed', 'gen-1', 'gen-2'])
    expect(player.queue).toEqual([seedTrack(), ...generated()])
    expect(radio.active).toBe(true)
  })

  it('tracks the queue-appended notice and clears it when the radio stops', async () => {
    const radio = useLastFmRadioStore()
    radio.init()
    listeners.get('radio:state')?.({ data: true })

    listeners.get('radio:queue-appended')?.({ data: { count: 10 } })
    expect(radio.appendedCount).toBe(10)

    listeners.get('radio:state')?.({ data: false })
    expect(radio.active).toBe(false)
    expect(radio.appendedCount).toBe(0)
  })

  it('stops the backend session and stops watching the state', () => {
    const radio = useLastFmRadioStore()
    radio.init()
    listeners.get('radio:state')?.({ data: true })

    radio.stop()

    expect(mockStopRadio).toHaveBeenCalled()
    expect(radio.active).toBe(false)
    expect(radio.appendedCount).toBe(0)

    radio.dispose()
    expect(listeners.has('radio:state')).toBe(false)
    expect(listeners.has('radio:queue-appended')).toBe(false)
  })
})
