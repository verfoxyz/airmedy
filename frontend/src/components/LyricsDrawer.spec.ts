import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { reactive } from 'vue'
import LyricsDrawer from './LyricsDrawer.vue'
import { usePlayerStore } from '../stores/player'
import { useAppStore } from '../stores/app'

const api = vi.hoisted(() => ({ InspectRomanization: vi.fn(), RomanizeLyrics: vi.fn() }))
const romanization = vi.hoisted(() => ({ enabled: true }))
const player = vi.hoisted(() => ({
  lyrics: { content: '[00:00.00]你好 ^ Translation', translation_content: '' } as
    { content: string; translation_content?: string },
  lyricsLoading: false,
  position: 0,
  isLyricsOpen: true,
  seek: vi.fn(),
  toggleLyrics: vi.fn(),
}))

vi.mock('../../bindings/airmedy/internal/infra/wails', () => ({ LyricsService: api }))
// The drawer saves the secondary-mode setting through the real app store; only
// the persistence call is stubbed so the choice stays reactive in the test.
vi.mock('../../bindings/airmedy/internal/infra/wails/settingsservice', () => ({
  GetSettings: vi.fn().mockResolvedValue(null),
  SaveSettings: vi.fn().mockResolvedValue(undefined),
}))
vi.mock('../stores/player', () => ({ usePlayerStore: () => reactive(player) }))
vi.mock('../stores/romanization', () => ({
  useRomanizationStore: () => Object.assign(reactive(romanization), { setEnabled: vi.fn(), getCached: () => undefined, cache: vi.fn() }),
}))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))
vi.mock('@wailsio/runtime', () => ({
  Events: { On: vi.fn(() => vi.fn()) },
  Create: {
    Nullable: (fn: (value: unknown) => unknown) => (value: unknown) => value == null ? null : fn(value),
    Array: (fn: (value: unknown) => unknown) => (value: unknown[]) => (value ?? []).map(fn),
    Struct: (ctor: new (value: unknown) => unknown) => (value: unknown) => value == null ? null : new ctor(value),
    Map: () => (value: unknown) => value,
  },
}))

describe('LyricsDrawer secondary line', () => {
  let store: typeof player

  beforeEach(() => {
    setActivePinia(createPinia())
    store = usePlayerStore() as unknown as typeof player
    store.lyrics = { content: '[00:00.00]你好 ^ Translation', translation_content: '' }
    store.lyricsLoading = false
    store.position = 0
    store.isLyricsOpen = true
    // Every test starts from the stored default, not from the previous test's choice.
    useAppStore().lyricsSecondaryMode = 'translation'
    reactive(romanization).enabled = true
    api.InspectRomanization.mockImplementation(() => Object.assign(Promise.resolve({ supported: true, mandarinDefault: true }), { cancel: vi.fn().mockResolvedValue(undefined) }))
    api.RomanizeLyrics.mockReturnValue(Object.assign(Promise.resolve([{ text: 'nǐ hǎo', status: 'converted' }]), { cancel: vi.fn().mockResolvedValue(undefined) }))
  })

  afterEach(() => { vi.clearAllMocks(); vi.restoreAllMocks(); vi.useRealTimers(); vi.unstubAllGlobals() })

  it('sweeps enhanced lyrics only while open and following playback', async () => {
    store.lyrics = { content: '[00:01]Người <00:02>hỏi<00:03>\n[00:04]Next' }
    store.position = 1.5
    const wrapper = mount(LyricsDrawer, { global: { mocks: { $t: (key: string) => key } } })
    await flushPromises()
    expect(wrapper.get('.karaoke-word').attributes('style')).toContain('50%')
    expect(wrapper.get('.cursor-pointer').attributes('style')).toBeUndefined()
    store.isLyricsOpen = false
    await wrapper.vm.$nextTick()
    expect(wrapper.find('.karaoke-word').exists()).toBe(false)
    expect(wrapper.get('.cursor-pointer').attributes('style')).toBeUndefined()
    store.isLyricsOpen = true
    await wrapper.vm.$nextTick()
    expect(wrapper.get('.karaoke-word').attributes('style')).toContain('50%')
    await wrapper.get('.scrollbar-hide').trigger('wheel')
    expect(wrapper.find('.karaoke-word').exists()).toBe(false)
    wrapper.unmount()
  })

  it('renders a stored translation as the secondary line, never as the primary line', async () => {
    store.lyrics = {
      content: '[00:00.00]你好\n[00:05.00]再见',
      translation_content: '[00:00.00]Hello\n[00:05.00]Bye',
    }
    const wrapper = mount(LyricsDrawer, { global: { mocks: { $t: (key: string) => key } } })
    await flushPromises()

    expect(wrapper.findAll('[data-test="lyric-primary"]').map(line => line.text())).toEqual(['你好', '再见'])
    expect(wrapper.findAll('[data-test="lyric-secondary"]').map(line => line.text())).toEqual(['Hello', 'Bye'])
    wrapper.unmount()
  })

  it('keeps the legacy inline translation as the secondary line', async () => {
    const wrapper = mount(LyricsDrawer, { global: { mocks: { $t: (key: string) => key } } })
    await flushPromises()

    expect(wrapper.get('[data-test="lyric-primary"]').text()).toBe('你好')
    expect(wrapper.get('[data-test="lyric-secondary"]').text()).toBe('Translation')
    wrapper.unmount()
  })

  it('hides the secondary line when the none option is chosen', async () => {
    store.lyrics = {
      content: '[00:00.00]你好\n[00:05.00]再见',
      translation_content: '[00:00.00]Hello\n[00:05.00]Bye',
    }
    const wrapper = mount(LyricsDrawer, { global: { mocks: { $t: (key: string) => key } } })
    await flushPromises()
    expect(wrapper.findAll('[data-test="lyric-secondary"]')).toHaveLength(2)

    await wrapper.get('[data-test="drawer-secondary-none"]').trigger('click')
    await flushPromises()

    expect(wrapper.find('[data-test="lyric-secondary"]').exists()).toBe(false)
    expect(useAppStore().lyricsSecondaryMode).toBe('none')
    wrapper.unmount()
  })

  it('swaps the translation for romanization when that option is chosen', async () => {
    store.lyrics = {
      content: '[00:00.00]你好\n[00:05.00]再见',
      translation_content: '[00:00.00]Hello\n[00:05.00]Bye',
    }
    const wrapper = mount(LyricsDrawer, { global: { mocks: { $t: (key: string) => key } } })
    await flushPromises()

    const romanization = wrapper.get('[data-test="drawer-secondary-romanization"]')
    expect(romanization.attributes('aria-checked')).toBe('false')
    await romanization.trigger('click')
    await flushPromises()

    expect(useAppStore().lyricsSecondaryMode).toBe('romanization')
    expect(wrapper.text()).toContain('nǐ hǎo')
    expect(wrapper.text()).not.toContain('Hello')
    wrapper.unmount()
  })

  it('disables translation without one and never renders an empty line', async () => {
    store.lyrics = { content: '[00:00.00]你好', translation_content: '' }
    const wrapper = mount(LyricsDrawer, { global: { mocks: { $t: (key: string) => key } } })
    await flushPromises()

    const translation = wrapper.get('[data-test="drawer-secondary-translation"]')
    expect(translation.attributes('disabled')).toBeDefined()
    expect(translation.attributes('aria-checked')).toBe('false')
    expect(wrapper.find('[data-test="lyric-secondary"]').exists()).toBe(false)

    await translation.trigger('click')
    // The stored choice is not overwritten just because this track cannot satisfy it.
    expect(useAppStore().lyricsSecondaryMode).toBe('translation')
    wrapper.unmount()
  })

  it('cycles the radio options with the arrow keys', async () => {
    const wrapper = mount(LyricsDrawer, { global: { mocks: { $t: (key: string) => key } } })
    await flushPromises()

    const group = wrapper.get('[data-test="drawer-secondary-modes"]')
    await group.trigger('keydown', { key: 'ArrowRight' })
    await flushPromises()
    expect(useAppStore().lyricsSecondaryMode).toBe('romanization')

    await group.trigger('keydown', { key: 'ArrowLeft' })
    await flushPromises()
    expect(useAppStore().lyricsSecondaryMode).toBe('translation')
    wrapper.unmount()
  })

  it('labels a plain lyric as unable to follow playback', async () => {
    store.lyrics = { content: '第一行\n第二行' }
    const wrapper = mount(LyricsDrawer, { global: { mocks: { $t: (key: string) => key } } })
    await flushPromises()

    expect(wrapper.get('[data-test="lyrics-unsynced"]').text()).toBe('player.lyrics_unsynced')
    expect(wrapper.text()).toContain('第一行')
    expect(wrapper.find('.karaoke-word').exists()).toBe(false)
    wrapper.unmount()
  })

  it('pins the active line before painting secondary changes and preserves browsing', async () => {
    vi.useFakeTimers()
    vi.stubGlobal('requestAnimationFrame', (callback: FrameRequestCallback) => window.setTimeout(() => callback(Date.now()), 16))
    vi.stubGlobal('cancelAnimationFrame', (id: number) => window.clearTimeout(id))
    vi.spyOn(HTMLElement.prototype, 'clientHeight', 'get').mockReturnValue(200)
    vi.spyOn(HTMLElement.prototype, 'clientWidth', 'get').mockReturnValue(300)
    store.lyrics = { content: '[00:00]你好\n[00:10]你好' }
    store.position = 10
    api.RomanizeLyrics.mockReturnValue(Object.assign(Promise.resolve([
      { text: 'nǐ hǎo', status: 'converted' }, { text: 'nǐ hǎo', status: 'converted' },
    ]), { cancel: vi.fn().mockResolvedValue(undefined) }))
    const wrapper = mount(LyricsDrawer, { global: { mocks: { $t: (key: string) => key } } })
    const container = wrapper.get('.scrollbar-hide').element
    const active = wrapper.findAll<HTMLElement>('.cursor-pointer')[1].element
    vi.spyOn(active, 'offsetTop', 'get').mockImplementation(() => active.textContent?.includes('nǐ hǎo') ? 600 : 300)
    vi.spyOn(active, 'clientHeight', 'get').mockImplementation(() => active.textContent?.includes('nǐ hǎo') ? 80 : 40)
    vi.spyOn(active, 'getBoundingClientRect').mockImplementation(() => ({ top: active.offsetTop - container.scrollTop }) as DOMRect)
    await flushPromises()
    await vi.advanceTimersByTimeAsync(1000)
    expect(container.scrollTop).toBe(540)

    reactive(romanization).enabled = false
    await flushPromises()
    expect(container.scrollTop).toBe(240)
    await vi.advanceTimersByTimeAsync(1000)
    expect(container.scrollTop).toBe(240)

    reactive(romanization).enabled = true
    await flushPromises()
    expect(container.scrollTop).toBe(540)
    await vi.advanceTimersByTimeAsync(1000)
    expect(container.scrollTop).toBe(540)

    await wrapper.get('.scrollbar-hide').trigger('wheel')
    container.scrollTop = 100
    reactive(romanization).enabled = false
    await flushPromises()
    await vi.advanceTimersByTimeAsync(1000)
    expect(container.scrollTop).toBe(100)
    wrapper.unmount()
  })

  it('restores auto-scroll when the drawer reopens', async () => {
    store.lyrics = { content: '[00:00.00]First\n[00:05.00]Second' }
    const wrapper = mount(LyricsDrawer, { global: { mocks: { $t: (key: string) => key } } })
    await flushPromises()

    await wrapper.get('.scrollbar-hide').trigger('wheel')
    expect(wrapper.html()).not.toContain('text-foreground/40')

    store.isLyricsOpen = false
    await wrapper.vm.$nextTick()
    store.isLyricsOpen = true
    await wrapper.vm.$nextTick()

    expect(wrapper.html()).toContain('text-foreground/40')
    wrapper.unmount()
  })
})
