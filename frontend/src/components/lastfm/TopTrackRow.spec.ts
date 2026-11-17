import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import TopTrackRow from './TopTrackRow.vue'

vi.mock('vue-i18n', () => ({
  useI18n: () => ({
    t: (key: string, params?: Record<string, unknown>) =>
      params && 'source' in params ? `${key}:${params.source}` : key,
  }),
}))

function item(overrides: Record<string, unknown> = {}) {
  return {
    title: 'Synthetic Fracture',
    artist: 'Sad Keyboard Guy',
    album: 'Album',
    duration: 200,
    artists: ['Sad Keyboard Guy'],
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

function mountRow(overrides: Record<string, unknown> = {}, props: Record<string, unknown> = {}) {
  return mount(TopTrackRow, {
    props: { item: item(overrides) as never, index: 0, ...props },
    global: {
      stubs: {
        LazyImg: {
          props: ['src', 'alt'],
          emits: ['error'],
          template: '<img :src="src" :alt="alt" @error="$emit(\'error\')" />',
        },
      },
    },
  })
}

describe('TopTrackRow table row', () => {
  it('renders the row number and switches to the playing icon', () => {
    expect(mountRow({}, { index: 2 }).find('[data-test="top-track-index"]').text()).toBe('3')
    expect(mountRow({}, { current: true }).find('[data-test="top-track-index"] svg').exists()).toBe(true)
  })

  it('shows the five match states in the status column', () => {
    expect(mountRow({ matching: true }).find('[data-test="top-track-status"]').attributes('data-state')).toBe('matching')
    expect(mountRow({ matching: false, no_source: true, provider: '' })
      .find('[data-test="top-track-status"]').attributes('data-state')).toBe('no-match')
    expect(mountRow({ local_track_id: 'local-1' }).find('[data-test="top-track-status"]').attributes('data-state')).toBe('in_library')
    expect(mountRow({ suspicious: true }).find('[data-test="top-track-status"]').attributes('data-state')).toBe('suspicious')
    const matched = mountRow().find('[data-test="top-track-status"]')
    expect(matched.attributes('data-state')).toBe('matched')
    expect(matched.text()).toBe('lastfm.matched_from:wy')
  })

  it('plays from the title only when the row is playable', async () => {
    const row = mountRow()
    await row.find('[data-test="top-track-title"]').trigger('click')
    expect(row.emitted('play')).toHaveLength(1)

    const pending = mountRow({ matching: true })
    expect(pending.find('button[data-test="top-track-title"]').exists()).toBe(false)
  })

  it('disables the play action for a matching or sourceless row', () => {
    expect(mountRow({ matching: true }).find('[data-test="top-track-play"]').attributes('disabled')).toBeDefined()
    expect(mountRow({ no_source: true }).find('[data-test="top-track-play"]').attributes('disabled')).toBeDefined()
    expect(mountRow().find('[data-test="top-track-play"]').attributes('disabled')).toBeUndefined()
  })

  it('offers save before the song is local and favorite after', async () => {
    const remote = mountRow()
    await remote.find('[data-test="top-track-save"]').trigger('click')
    expect(remote.emitted('save')).toHaveLength(1)
    expect(remote.find('[data-test="top-track-favorite"]').exists()).toBe(false)

    const local = mountRow({ local_track_id: 'local-1', provider: '', external_id: '' })
    expect(local.find('[data-test="top-track-save"]').exists()).toBe(false)
    await local.find('[data-test="top-track-favorite"]').trigger('click')
    expect(local.emitted('favorite')).toHaveLength(1)
  })

  it('opens the full menu from the more button', async () => {
    const row = mountRow()
    await row.find('[data-test="top-track-more"]').trigger('click')
    const emitted = row.emitted('menu')
    expect(emitted).toHaveLength(1)
    expect(emitted![0][1]).toMatchObject({ title: 'Synthetic Fracture' })
  })

  it('keeps the artist as a subtitle for narrow containers', () => {
    const row = mountRow({ artists: ['A', 'B'] })
    expect(row.find('[data-test="top-track-artist-mobile"]').text()).toBe('A、B')
    expect(row.find('[data-test="top-track-artist"]').text()).toBe('A、B')
  })

  it('puts the cover thumbnail inside the title column', async () => {
    const placeholder = mountRow()
    expect(placeholder.find('[data-test="top-track-cover-placeholder"]').exists()).toBe(true)
    expect(placeholder.find('[data-test="top-track-cover"]').exists()).toBe(false)

    const starred = mountRow({ cover_url: 'https://lastfm-img.freetls.fastly.net/i/u/64s/2a96cbd8b46e442fc41c2b86b821562f.png' })
    expect(starred.find('[data-test="top-track-cover"]').exists()).toBe(false)

    const wrapper = mountRow({ cover_url: 'https://cdn.example/cover.jpg' })
    const cover = wrapper.find('[data-test="top-track-cover"]')
    expect(cover.exists()).toBe(true)
    expect(cover.attributes('src')).toBe('https://cdn.example/cover.jpg')

    // A failed image falls back to the placeholder without changing the row.
    await cover.trigger('error')
    expect(wrapper.find('[data-test="top-track-cover-placeholder"]').exists()).toBe(true)
  })

  it('keeps the weekly play count beside the title', () => {
    const row = mountRow({ play_count: 7 })
    expect(row.find('[data-test="top-track-plays"]').text()).toBe('lastfm.plays_count')
  })
})
