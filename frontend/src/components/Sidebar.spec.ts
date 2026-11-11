import { beforeEach, describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import Sidebar from './Sidebar.vue'

vi.mock('vue-i18n', () => ({
  useI18n: () => ({ t: (key: string) => key }),
}))

const push = vi.fn()
vi.mock('vue-router', () => ({
  useRouter: () => ({ push }),
  RouterLink: { props: ['to'], template: '<a :data-to="to"><slot /></a>' },
}))

vi.mock('@wailsio/runtime', async (importOriginal) => ({
  ...await importOriginal<typeof import('@wailsio/runtime')>(),
  Events: { On: vi.fn(() => () => {}) },
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({ updateInfo: null, updateApplied: false, isUpdateDialogOpen: false }),
}))
vi.mock('@/stores/download', () => ({
  useDownloadStore: () => ({ activeCount: 0 }),
}))
vi.mock('@/stores/device', () => ({
  useDeviceStore: () => ({ isMac: false }),
}))

// The playlist and main-nav sections are unrelated to the entry under test and
// pull their own bindings; stubbing them keeps this a navigation-order test.
vi.mock('./SidebarPlaylists.vue', () => ({ default: { template: '<div />' } }))
vi.mock('./SidebarNav.vue', () => ({ default: { template: '<div />' } }))

describe('Sidebar', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    push.mockReset()
  })

  it('puts the Last.fm entry directly after search', () => {
    const wrapper = mount(Sidebar)

    const labels = wrapper.findAll('a').map((anchor) => anchor.text())
    const searchIndex = labels.indexOf('sidebar.search')
    const lastfmIndex = labels.indexOf('sidebar.lastfm')

    expect(searchIndex).toBeGreaterThanOrEqual(0)
    expect(lastfmIndex).toBe(searchIndex + 1)
  })

  it('navigates to the Last.fm page from the entry', () => {
    const wrapper = mount(Sidebar)

    const lastfm = wrapper.findAll('a').find((anchor) => anchor.text() === 'sidebar.lastfm')
    expect(lastfm?.attributes('data-to')).toBe('/lastfm')
  })
})
