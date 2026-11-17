import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'

const mocks = vi.hoisted(() => ({
  list: vi.fn(),
  undislike: vi.fn(),
  dislike: vi.fn(),
  on: vi.fn(),
}))

vi.mock('vue-i18n', () => ({
  useI18n: () => ({ t: (key: string) => key }),
}))

vi.mock('../../../bindings/airmedy/internal/infra/wails/lastfmservice', () => ({
  ListDislikes: mocks.list,
  Undislike: mocks.undislike,
  Dislike: mocks.dislike,
}))

vi.mock('@wailsio/runtime', () => ({
  Events: { On: mocks.on },
}))

import DislikeSettings from './DislikeSettings.vue'
import { useDislikeStore } from '@/stores/dislike'

const rows = [
  {
    id: 'd1', kind: 'track', match_key: 'drop\x00song',
    title: 'Song', artist: 'Drop', created_at: '2026-11-17T00:00:00Z',
  },
  {
    id: 'd2', kind: 'artist', match_key: 'blocked',
    title: '', artist: 'Blocked Artist', created_at: '2026-11-16T00:00:00Z',
  },
]

async function mountSection() {
  const pinia = createPinia()
  setActivePinia(pinia)
  const wrapper = mount(DislikeSettings, {
    global: {
      plugins: [pinia],
      mocks: { $t: (key: string) => key },
    },
  })
  await flushPromises()
  return wrapper
}

describe('DislikeSettings', () => {
  beforeEach(() => {
    mocks.list.mockReset()
    mocks.undislike.mockReset()
    mocks.dislike.mockReset()
    mocks.on.mockReset()
    mocks.on.mockReturnValue(() => {})
    mocks.list.mockResolvedValue(rows)
    mocks.undislike.mockResolvedValue(undefined)
  })

  it('lists tracks and artists together', async () => {
    const wrapper = await mountSection()
    const rendered = wrapper.findAll('[data-test="dislike-row"]')

    expect(rendered).toHaveLength(2)
    expect(rendered[0].text()).toContain('Song')
    expect(rendered[1].text()).toContain('Blocked Artist')
    expect(wrapper.find('[data-test="dislike-empty"]').exists()).toBe(false)
  })

  it('shows the empty state when nothing is excluded', async () => {
    mocks.list.mockResolvedValue([])
    const wrapper = await mountSection()

    expect(wrapper.findAll('[data-test="dislike-row"]')).toHaveLength(0)
    expect(wrapper.find('[data-test="dislike-empty"]').exists()).toBe(true)
  })

  it('removes one row and updates the list', async () => {
    const wrapper = await mountSection()
    await wrapper.findAll('[data-test="dislike-remove"]')[0].trigger('click')
    await flushPromises()

    expect(mocks.undislike).toHaveBeenCalledWith('d1')
    expect(wrapper.findAll('[data-test="dislike-row"]')).toHaveLength(1)
    expect(useDislikeStore().items).toHaveLength(1)
  })
})
