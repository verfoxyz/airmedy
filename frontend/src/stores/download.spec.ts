import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { createPinia, setActivePinia } from 'pinia'
import { useDownloadStore } from './download'
import * as DownloadService from '../../bindings/airmedy/internal/infra/wails/downloadservice'
import * as LibraryService from '../../bindings/airmedy/internal/infra/wails/libraryservice'

// The store listens to backend events and hydrates from ListDownloads, so the
// test plays both sides: the event bus and the two services it calls.
const handlers = vi.hoisted(() => new Map<string, (event: unknown) => void>())
vi.mock('@wailsio/runtime', () => ({
  Events: {
    On: vi.fn((name: string, handler: (event: unknown) => void) => {
      handlers.set(name, handler)
      return () => handlers.delete(name)
    }),
  },
}))

vi.mock('../../bindings/airmedy/internal/infra/wails/downloadservice', () => ({
  ListDownloads: vi.fn(),
  CancelDownload: vi.fn(),
  RetryDownload: vi.fn(),
}))

vi.mock('../../bindings/airmedy/internal/infra/wails/libraryservice', () => ({
  GetTracksByIDs: vi.fn(),
}))

const emit = (name: string, data: unknown) => {
  const handler = handlers.get(name)
  if (!handler) throw new Error(`no handler registered for ${name}`)
  handler({ data })
}

const progress = (overrides: Record<string, unknown> = {}) => ({
  job_id: 'job-1',
  track_id: 'track-1',
  playlist_id: 'pl-1',
  state: 'active',
  bytes_done: 512,
  bytes_total: 1024,
  percent: 50,
  error: '',
  ...overrides,
})

/** A backend job row, i.e. what ListDownloads returns. */
const job = (overrides: Record<string, unknown> = {}) => ({
  id: 'job-1',
  track_id: 'track-1',
  playlist_id: 'pl-1',
  quality: 'lossless',
  state: 'queued',
  attempts: 0,
  max_attempts: 3,
  last_error: '',
  bytes_done: 0,
  bytes_total: 0,
  target_path: '',
  created_at: '2026-01-01T00:00:00.000Z',
  updated_at: '2026-01-01T00:00:00.000Z',
  ...overrides,
})

describe('download store', () => {
  beforeEach(() => {
    handlers.clear()
    setActivePinia(createPinia())
    vi.mocked(DownloadService.ListDownloads).mockResolvedValue([])
    vi.mocked(DownloadService.CancelDownload).mockResolvedValue(undefined)
    vi.mocked(DownloadService.RetryDownload).mockResolvedValue(undefined)
    vi.mocked(LibraryService.GetTracksByIDs).mockResolvedValue([])
  })

  afterEach(() => {
    vi.clearAllMocks()
  })

  it('records progress for a track', () => {
    const store = useDownloadStore()
    emit('download:progress', progress())

    const state = store.forTrack('track-1')
    expect(state?.percent).toBe(50)
    expect(state?.playlistId).toBe('pl-1')
    expect(store.isDownloading('track-1')).toBe(true)
    expect(store.activeCount).toBe(1)
  })

  it('keeps a track with no Content-Length as indeterminate, not 0%', () => {
    const store = useDownloadStore()
    // A server that sends no length leaves the total at 0; the ring must show
    // an indeterminate state instead of an empty one.
    emit('download:progress', progress({ bytes_total: 0, percent: 0 }))

    expect(store.forTrack('track-1')?.bytesTotal).toBe(0)
    expect(store.isDownloading('track-1')).toBe(true)
  })

  it('drops a finished download so the row renders normally again', () => {
    const store = useDownloadStore()
    emit('download:progress', progress())
    emit('download:done', progress({ state: 'done', bytes_done: 1024, percent: 100 }))

    expect(store.forTrack('track-1')).toBeUndefined()
    expect(store.isDownloading('track-1')).toBe(false)
    expect(store.activeCount).toBe(0)
  })

  it('keeps a failed download so the row can offer a retry', () => {
    const store = useDownloadStore()
    emit('download:progress', progress())
    emit('download:failed', progress({ state: 'failed', error: 'stream unavailable' }))

    expect(store.hasFailed('track-1')).toBe(true)
    expect(store.forTrack('track-1')?.error).toBe('stream unavailable')
    expect(store.isDownloading('track-1')).toBe(false)
    expect(store.activeCount).toBe(0)
  })

  it('tracks several downloads independently', () => {
    const store = useDownloadStore()
    emit('download:progress', progress({ track_id: 'a', job_id: 'j1' }))
    emit('download:progress', progress({ track_id: 'b', job_id: 'j2' }))

    expect(store.activeCount).toBe(2)
    emit('download:done', progress({ track_id: 'a', job_id: 'j1', state: 'done' }))
    expect(store.isDownloading('a')).toBe(false)
    expect(store.isDownloading('b')).toBe(true)
    expect(store.activeCount).toBe(1)
  })

  it('unsubscribes on dispose', () => {
    const store = useDownloadStore()
    store.dispose()
    expect(handlers.size).toBe(0)
  })

  it('hydrates the queue at startup and syncs the per-track map', async () => {
    // A job left queued by the previous run is invisible without this call.
    vi.mocked(DownloadService.ListDownloads).mockResolvedValue([
      job({ id: 'job-1', state: 'queued' }),
      job({ id: 'job-2', track_id: 'track-2', state: 'active', bytes_done: 256, bytes_total: 1024 }),
    ] as never)

    const store = useDownloadStore()
    await store.hydrate()

    expect(DownloadService.ListDownloads).toHaveBeenCalledTimes(1)
    expect(store.jobs).toHaveLength(2)
    expect(store.forTrack('track-1')?.state).toBe('queued')
    expect(store.forTrack('track-2')?.percent).toBe(25)
    expect(store.activeCount).toBe(2)
  })

  it('lets a live event override the snapshot it raced with', async () => {
    vi.mocked(DownloadService.ListDownloads).mockResolvedValue([job({ state: 'queued' })] as never)

    const store = useDownloadStore()
    const hydrating = store.hydrate()
    // The worker starts the job while ListDownloads is still in flight.
    emit('download:progress', progress({ state: 'active', bytes_done: 768, percent: 75 }))
    await hydrating

    expect(store.jobs).toHaveLength(1)
    expect(store.jobs[0].state).toBe('active')
    expect(store.jobs[0].percent).toBe(75)
    expect(store.forTrack('track-1')?.percent).toBe(75)
  })

  it('never lists a finished job', async () => {
    vi.mocked(DownloadService.ListDownloads).mockResolvedValue([
      job({ id: 'job-done', state: 'done' }),
      job({ id: 'job-queued', state: 'queued' }),
    ] as never)

    const store = useDownloadStore()
    await store.hydrate()

    expect(store.pendingJobs.map(j => j.jobId)).toEqual(['job-queued'])
    expect(store.forTrack('track-1')?.jobId).toBe('job-queued')
  })

  it('hides a failed row once a newer job for the same track exists', async () => {
    vi.mocked(DownloadService.ListDownloads).mockResolvedValue([
      job({ id: 'job-old', state: 'failed', last_error: 'stream unavailable' }),
      job({ id: 'job-new', state: 'queued' }),
    ] as never)

    const store = useDownloadStore()
    await store.hydrate()

    // A retry creates a fresh row; the old one is only the record of what
    // happened, so the panel keeps a single row per track.
    expect(store.pendingJobs.map(j => j.jobId)).toEqual(['job-new'])
  })

  it('resolves track titles in one batched request', async () => {
    vi.mocked(DownloadService.ListDownloads).mockResolvedValue([
      job({ id: 'job-1' }),
      job({ id: 'job-2', track_id: 'track-2' }),
    ] as never)
    vi.mocked(LibraryService.GetTracksByIDs).mockResolvedValue([
      { id: 'track-1', title: 'First', raw_artist_names: 'Artist A' },
      { id: 'track-2', title: 'Second', raw_artist_names: 'Artist B' },
    ] as never)

    const store = useDownloadStore()
    await store.hydrate()

    expect(LibraryService.GetTracksByIDs).toHaveBeenCalledTimes(1)
    expect(vi.mocked(LibraryService.GetTracksByIDs).mock.calls[0][0].sort()).toEqual(['track-1', 'track-2'])
    expect(store.pendingJobs[0].title).toBe('First')
    expect(store.pendingJobs[0].artist).toBe('Artist A')
  })

  it('keeps the row when its track is gone from the library', async () => {
    vi.mocked(DownloadService.ListDownloads).mockResolvedValue([job({ track_id: 'deleted-track' })] as never)
    vi.mocked(LibraryService.GetTracksByIDs).mockResolvedValue([] as never)

    const store = useDownloadStore()
    await store.hydrate()

    // No title to show, but the queue row survives so it can still be canceled.
    expect(store.pendingJobs).toHaveLength(1)
    expect(store.pendingJobs[0].trackId).toBe('deleted-track')
    expect(store.pendingJobs[0].title).toBe('')
  })

  it('survives a ListDownloads failure without clearing the queue', async () => {
    vi.mocked(DownloadService.ListDownloads).mockResolvedValue([job()] as never)
    const store = useDownloadStore()
    await store.hydrate()

    vi.mocked(DownloadService.ListDownloads).mockRejectedValue(new Error('ipc down'))
    await expect(store.hydrate()).resolves.toBeUndefined()
    expect(store.pendingJobs).toHaveLength(1)
  })

  it('cancels a job and stops showing it as running', async () => {
    vi.mocked(DownloadService.ListDownloads).mockResolvedValue([job({ state: 'active' })] as never)
    const store = useDownloadStore()
    await store.hydrate()

    await store.cancel('job-1')

    expect(DownloadService.CancelDownload).toHaveBeenCalledWith('job-1')
    expect(store.forTrack('track-1')).toBeUndefined()
    expect(store.pendingJobs[0]?.state).toBe('canceled')
  })

  it('keeps the hydrated quality and attempt count when an event arrives', async () => {
    vi.mocked(DownloadService.ListDownloads).mockResolvedValue([
      job({ id: 'job-1', state: 'active', quality: 'lossless', attempts: 2, max_attempts: 3 }),
    ] as never)
    const store = useDownloadStore()
    await store.hydrate()

    // An event payload carries no quality/attempts; it must not wipe them.
    emit('download:progress', progress({ job_id: 'job-1', state: 'active', bytes_done: 1024, percent: 100 }))

    expect(store.jobs).toHaveLength(1)
    expect(store.jobs[0].quality).toBe('lossless')
    expect(store.jobs[0].attempts).toBe(2)
    expect(store.jobs[0].percent).toBe(100)
  })

  it('retries a job and reloads the fresh row the backend created', async () => {
    vi.mocked(DownloadService.ListDownloads).mockResolvedValue([
      job({ id: 'job-1', state: 'failed', last_error: 'boom' }),
    ] as never)
    const store = useDownloadStore()
    await store.hydrate()

    vi.mocked(DownloadService.ListDownloads).mockResolvedValue([
      job({ id: 'job-1', state: 'failed', last_error: 'boom' }),
      job({ id: 'job-2', state: 'queued' }),
    ] as never)
    await store.retry('job-1')

    expect(DownloadService.RetryDownload).toHaveBeenCalledWith('job-1')
    expect(store.jobs.map(j => j.jobId)).toEqual(['job-1', 'job-2'])
    // Only the fresh row is offered: the failed one is superseded.
    expect(store.pendingJobs.map(j => j.jobId)).toEqual(['job-2'])
  })
})
