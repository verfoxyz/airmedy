import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import { createTestingPinia } from '@pinia/testing'
import PlayerQueuePanel from './PlayerQueuePanel.vue'

vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))
vi.mock('vue-router', () => ({ useRouter: () => ({ push: vi.fn() }) }))

function mountPanel(active: boolean, lastfmActive = false) {
  return mount(PlayerQueuePanel, {
    props: { queue: [] },
    global: {
      plugins: [createTestingPinia({
        createSpy: vi.fn,
        initialState: { moodRadio: { active }, lastfmRadio: { active: lastfmActive } },
      })],
      stubs: { TrackTable: true },
    },
  })
}

describe('PlayerQueuePanel', () => {
  it('shows the radio indicator only while Mood Radio is active', () => {
    expect(mountPanel(true).find('[data-test="mood-radio-indicator"]').exists()).toBe(true)
    expect(mountPanel(false).find('[data-test="mood-radio-indicator"]').exists()).toBe(false)
  })

  it('shows a closable Last.fm radio marker and the refill notice', () => {
    const wrapper = mountPanel(false, true)
    expect(wrapper.find('[data-test="lastfm-radio-indicator"]').exists()).toBe(true)
    expect(mountPanel(false, false).find('[data-test="lastfm-radio-indicator"]').exists()).toBe(false)

    const withNotice = mount(PlayerQueuePanel, {
      props: { queue: [] },
      global: {
        plugins: [createTestingPinia({
          createSpy: vi.fn,
          initialState: { moodRadio: { active: false }, lastfmRadio: { active: true, appendedCount: 10 } },
        })],
        stubs: { TrackTable: true },
      },
    })
    expect(withNotice.find('[data-test="lastfm-radio-appended"]').text()).toBe('player.radio_appended')
  })
})
