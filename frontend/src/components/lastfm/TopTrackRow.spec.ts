import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import TopTrackRow from './TopTrackRow.vue'

vi.mock('vue-i18n', () => ({
  useI18n: () => ({
    t: (key: string, params?: Record<string, unknown>) =>
      params?.source ? `${key}:${params.source}` : key,
  }),
}))

const LASTFM_PLACEHOLDER = 'https://lastfm-img.freetls.fastly.net/i/u/64s/2a96cbd8b46e442fc41c2b86b821562f.png'

function item(overrides: Record<string, unknown> = {}) {
  return {
    title: 'Synthetic Fracture',
    artist: 'Sad Keyboard Guy',
    album: 'Album',
    duration: 200,
    artists: ['Sad Keyboard Guy'],
    cover_url: '',
    cover_url_large: '',
    provider: 'wy',
    external_id: '42',
    matching: false,
    matched: true,
    no_source: false,
    local_track_id: '',
    downloadable: true,
    ...overrides,
  }
}

function mountRow(overrides: Record<string, unknown> = {}) {
  return mount(TopTrackRow, {
    props: { item: item(overrides) as never },
    global: {
      stubs: {
        LazyImg: {
          props: ['src'],
          template: '<img data-test="cover" :src="src" />',
        },
      },
    },
  })
}

describe('TopTrackRow covers', () => {
  it('renders the placeholder when there is no cover', () => {
    const wrapper = mountRow()
    expect(wrapper.find('[data-test="cover"]').exists()).toBe(false)
  })

  it("treats Last.fm's default star image as no cover", () => {
    const wrapper = mountRow({ cover_url: LASTFM_PLACEHOLDER })
    expect(wrapper.find('[data-test="cover"]').exists()).toBe(false)
  })

  it('renders the cover when one is available', () => {
    const wrapper = mountRow({ cover_url: 'https://cdn.example/cover.jpg' })
    const cover = wrapper.find('[data-test="cover"]')
    expect(cover.exists()).toBe(true)
    expect(cover.attributes('src')).toBe('https://cdn.example/cover.jpg')
  })

  it('replaces the placeholder when a later event brings a cover', async () => {
    const wrapper = mountRow()
    expect(wrapper.find('[data-test="cover"]').exists()).toBe(false)

    await wrapper.setProps({ item: item({ cover_url: 'https://cdn.example/cover.jpg' }) as never })
    await wrapper.vm.$nextTick()

    expect(wrapper.find('[data-test="cover"]').attributes('src')).toBe('https://cdn.example/cover.jpg')
  })

  it('flags a suspicious duration match with a tooltip', () => {
    const wrapper = mountRow({ suspicious: true })
    const chip = wrapper.find('[data-test="top-track-suspicious"]')
    expect(chip.exists()).toBe(true)
    expect(chip.attributes('title')).toBe('common.suspicious_match_hint')
    expect(chip.text()).toBe('common.suspicious_match')
  })

  it('does not flag a normal match', () => {
    const wrapper = mountRow()
    expect(wrapper.find('[data-test="top-track-suspicious"]').exists()).toBe(false)
  })

  it('labels a local-only match as in the library', () => {
    const wrapper = mountRow({ provider: '', external_id: '', local_track_id: 'local-1' })
    expect(wrapper.find('[data-test="top-track-status"]').text()).toBe('lastfm.in_library')
  })

  it('prefers the local copy over a stale provider chip', () => {
    const wrapper = mountRow({ state: 'in_library', local_track_id: 'local-1' })
    expect(wrapper.find('[data-test="top-track-status"]').text()).toBe('lastfm.in_library')
    expect(wrapper.find('[data-test="top-track-save"]').exists()).toBe(false)
    expect(wrapper.find('[data-test="top-track-play"]').exists()).toBe(true)
  })
})
