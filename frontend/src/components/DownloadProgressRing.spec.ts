import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import DownloadProgressRing from './DownloadProgressRing.vue'
import type { DownloadState } from '@/stores/download'

vi.mock('vue-i18n', () => ({
  useI18n: () => ({
    t: (key: string, values?: Record<string, unknown>) =>
      values ? `${key}:${JSON.stringify(values)}` : key,
  }),
}))

const state = (overrides: Partial<DownloadState> = {}): DownloadState => ({
  jobId: 'job-1',
  trackId: 'track-1',
  playlistId: 'pl-1',
  state: 'active',
  bytesDone: 512,
  bytesTotal: 1024,
  percent: 50,
  error: '',
  ...overrides,
})

const mountRing = (s?: DownloadState) =>
  mount(DownloadProgressRing, { props: { state: s } })

describe('DownloadProgressRing', () => {
  it('renders nothing when the track has no download in flight', () => {
    const wrapper = mountRing(undefined)
    expect(wrapper.find('[data-test="download-ring"]').exists()).toBe(false)
  })

  it('shows a filled ring for a running download', () => {
    const wrapper = mountRing(state({ percent: 50 }))
    const ring = wrapper.find('[data-test="download-ring"]')
    expect(ring.exists()).toBe(true)
    expect(ring.attributes('aria-valuenow')).toBe('50')
    expect(ring.attributes('data-state')).toBe('active')
    // Two circles: the track and the progress arc.
    expect(wrapper.findAll('circle')).toHaveLength(2)
  })

  it('is indeterminate when the server sent no Content-Length', () => {
    // bytesTotal 0 must not read as "0% done" — it is unknown, not stuck.
    const wrapper = mountRing(state({ bytesTotal: 0, percent: 0 }))
    const ring = wrapper.find('[data-test="download-ring"]')
    expect(ring.classes()).toContain('animate-spin')
    expect(ring.attributes('aria-valuenow')).toBeUndefined()
  })

  it('labels a queued download and does not spin', () => {
    const wrapper = mountRing(state({ state: 'queued', percent: 0 }))
    const ring = wrapper.find('[data-test="download-ring"]')
    expect(ring.attributes('data-state')).toBe('queued')
    expect(ring.classes()).not.toContain('animate-spin')
    expect(ring.attributes('aria-label')).toBe('download.queued')
  })

  it('surfaces the backend error on a failed download', () => {
    const wrapper = mountRing(state({ state: 'failed', error: 'stream unavailable' }))
    expect(wrapper.find('[data-test="download-ring"]').attributes('aria-label')).toBe('stream unavailable')
  })

  it('clamps an over-reported percentage to a full ring', () => {
    const wrapper = mountRing(state({ percent: 140 }))
    expect(wrapper.find('[data-test="download-ring"]').attributes('aria-valuenow')).toBe('140')
    // The geometry never goes negative, so no arc is drawn backwards.
    const arc = wrapper.findAll('circle')[1]
    expect(Number(arc.attributes('stroke-dashoffset'))).toBeGreaterThanOrEqual(0)
  })

  it('honours the size prop', () => {
    const wrapper = mount(DownloadProgressRing, { props: { state: state(), size: 24 } })
    expect(wrapper.find('svg').attributes('width')).toBe('24')
  })
})
