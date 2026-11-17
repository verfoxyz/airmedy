import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'

class FakeIntersectionObserver {
  static instances: FakeIntersectionObserver[] = []
  callback: IntersectionObserverCallback
  options?: IntersectionObserverInit
  observed: Element[] = []
  disconnected = false

  constructor(callback: IntersectionObserverCallback, options?: IntersectionObserverInit) {
    this.callback = callback
    this.options = options
    FakeIntersectionObserver.instances.push(this)
  }

  observe(target: Element) {
    this.observed.push(target)
  }

  disconnect() {
    this.disconnected = true
  }

  trigger(isIntersecting: boolean) {
    this.callback([{ isIntersecting } as IntersectionObserverEntry], this as unknown as IntersectionObserver)
  }
}

vi.mock('vue-i18n', () => ({
  useI18n: () => ({ t: (key: string) => key }),
}))

import StickyDetailBar from './StickyDetailBar.vue'
import ContextMenu from './ContextMenu.vue'

function mountBar(overrides: Record<string, unknown> = {}) {
  return mount(StickyDetailBar, {
    props: {
      title: 'My playlist',
      sentinel: document.createElement('span'),
      root: document.createElement('div'),
      visible: false,
      testPrefix: 'test',
      ...overrides,
    },
    global: {
      stubs: {
        DetailsButton: {
          props: ['icon', 'label', 'disabled'],
          template: '<button :disabled="disabled"><slot />{{ label }}</button>',
        },
        IconButton: {
          props: ['active'],
          template: '<button :data-active="active"><slot /></button>',
        },
        Input: {
          props: ['modelValue'],
          emits: ['update:modelValue'],
          setup(_: unknown, { expose }: { expose: (api: object) => void }) {
            expose({ focus: vi.fn() })
            return {}
          },
          template: '<input :value="modelValue" @input="$emit(\'update:modelValue\', $event.target.value)" />',
        },
      },
    },
  })
}

function lastObserver() {
  return FakeIntersectionObserver.instances.at(-1)!
}

describe('StickyDetailBar', () => {
  beforeEach(() => {
    vi.stubGlobal('IntersectionObserver', FakeIntersectionObserver)
    FakeIntersectionObserver.instances = []
  })

  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('takes over when the title leaves the page scroller', async () => {
    const wrapper = mountBar()
    await flushPromises()

    const observer = lastObserver()
    expect(observer.observed).toHaveLength(1)
    expect(observer.options?.root).toBeDefined()

    observer.trigger(false)
    expect(wrapper.emitted('update:visible')?.[0]).toEqual([true])
    observer.trigger(true)
    expect(wrapper.emitted('update:visible')?.[1]).toEqual([false])
  })

  it('renders a glass toolbar with the prefixed test hooks', () => {
    const wrapper = mountBar({ visible: true })
    expect(wrapper.find('[data-test="test-sticky-bar"]').attributes('data-visible')).toBe('true')
    expect(wrapper.find('[role="toolbar"]').attributes('aria-label')).toBe('My playlist')
    expect(wrapper.find('[data-test="test-sticky-play"]').exists()).toBe(true)
    expect(wrapper.find('[data-test="test-sticky-shuffle"]').exists()).toBe(true)
    expect(wrapper.find('[data-test="test-sticky-more"]').exists()).toBe(true)
  })

  it('forwards the toolbar actions', async () => {
    const wrapper = mountBar({ visible: true })
    await wrapper.find('[data-test="test-sticky-back"]').trigger('click')
    await wrapper.find('[data-test="test-sticky-play"]').trigger('click')
    await wrapper.find('[data-test="test-sticky-shuffle"]').trigger('click')

    expect(wrapper.emitted('back')).toHaveLength(1)
    expect(wrapper.emitted('play')).toHaveLength(1)
    expect(wrapper.emitted('shuffle')).toHaveLength(1)
  })

  it('shares the search state with the page', async () => {
    const wrapper = mountBar({ visible: true, modelValue: '' })
    await wrapper.find('[data-test="test-sticky-search"]').trigger('click')
    await flushPromises()

    const input = wrapper.find('[data-test="test-sticky-search-input"]')
    expect(input.exists()).toBe(true)
    await input.setValue('hello')
    expect(wrapper.emitted('update:modelValue')?.[0]).toEqual(['hello'])
  })

  it('opens the more menu with the supplied items', async () => {
    const items = [{ label: 'Refresh', action: vi.fn() }]
    const wrapper = mountBar({ visible: true, moreItems: items })
    await wrapper.find('[data-test="test-sticky-more"]').trigger('click')
    await flushPromises()

    expect(wrapper.findComponent(ContextMenu).props('items')).toEqual(items)
  })

  it('disconnects the observer on unmount', async () => {
    const wrapper = mountBar()
    await flushPromises()
    const observer = lastObserver()
    wrapper.unmount()
    expect(observer.disconnected).toBe(true)
  })
})
