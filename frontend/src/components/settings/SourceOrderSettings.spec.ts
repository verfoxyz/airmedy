import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'

const mocks = vi.hoisted(() => ({
  preferences: vi.fn(),
  saveOrder: vi.fn(),
  setDisabled: vi.fn(),
}))

vi.mock('vue-i18n', () => ({
  useI18n: () => ({ t: (key: string) => key }),
}))

vi.mock('../../../bindings/airmedy/internal/infra/wails/catalogservice', () => ({
  GetSourcePreferences: mocks.preferences,
  SaveSourceOrder: mocks.saveOrder,
  SetSourceDisabled: mocks.setDisabled,
}))

import SourceOrderSettings from './SourceOrderSettings.vue'

function preferences(overrides: Record<string, unknown> = {}) {
  return {
    order: ['wy', 'kg'],
    disabled: [],
    sources: [
      { id: 'wy', name: '网易云' },
      { id: 'kg', name: '酷狗' },
    ],
    ...overrides,
  }
}

async function mountSection() {
  const wrapper = mount(SourceOrderSettings, {
    global: {
      mocks: { $t: (key: string) => key },
      stubs: {
        Switch: {
          props: ['modelValue'],
          emits: ['update:modelValue'],
          template: '<button data-test="stub-switch" @click="$emit(\'update:modelValue\', !modelValue)"></button>',
        },
      },
    },
  })
  await flushPromises()
  return wrapper
}

function renderedOrder(wrapper: Awaited<ReturnType<typeof mountSection>>): string[] {
  return wrapper
    .findAll('[data-test="source-order-row"]')
    .map((row) => row.attributes('data-source') ?? '')
}

describe('SourceOrderSettings', () => {
  beforeEach(() => {
    mocks.preferences.mockReset()
    mocks.saveOrder.mockReset()
    mocks.setDisabled.mockReset()
    mocks.preferences.mockResolvedValue(preferences())
    mocks.saveOrder.mockResolvedValue(['kg', 'wy'])
    mocks.setDisabled.mockResolvedValue(['wy'])
  })

  it('renders the configured order and enable state', async () => {
    mocks.preferences.mockResolvedValue(preferences({ disabled: ['kg'] }))
    const wrapper = await mountSection()

    expect(renderedOrder(wrapper)).toEqual(['wy', 'kg'])
    const rows = wrapper.findAll('[data-test="source-order-row"]')
    expect(rows[0].attributes('data-enabled')).toBe('true')
    expect(rows[1].attributes('data-enabled')).toBe('false')
    // A disabled source stays visible so it can be switched back on.
    expect(rows[1].text()).toContain('酷狗')
  })

  it('moves with the arrow buttons and saves once', async () => {
    const wrapper = await mountSection()

    await wrapper.findAll('[data-test="source-order-down"]')[0].trigger('click')
    await flushPromises()

    expect(mocks.saveOrder).toHaveBeenCalledTimes(1)
    expect(mocks.saveOrder).toHaveBeenCalledWith(['kg', 'wy'])
    expect(renderedOrder(wrapper)).toEqual(['kg', 'wy'])
  })

  it('disables the boundary buttons', async () => {
    const wrapper = await mountSection()
    const ups = wrapper.findAll('[data-test="source-order-up"]')
    const downs = wrapper.findAll('[data-test="source-order-down"]')

    expect(ups[0].attributes('disabled')).toBeDefined()
    expect(downs[1].attributes('disabled')).toBeDefined()
    expect(ups[1].attributes('disabled')).toBeUndefined()
  })

  it('reorders by drag and saves once', async () => {
    const wrapper = await mountSection()
    const rows = wrapper.findAll('[data-test="source-order-row"]')

    await rows[0].find('[data-test="source-order-handle"]').trigger('dragstart')
    await rows[1].trigger('dragover')
    await rows[1].trigger('drop')
    await flushPromises()

    expect(mocks.saveOrder).toHaveBeenCalledTimes(1)
    expect(mocks.saveOrder).toHaveBeenCalledWith(['kg', 'wy'])
    expect(renderedOrder(wrapper)).toEqual(['kg', 'wy'])
  })

  it('toggles a source and updates its state', async () => {
    mocks.setDisabled.mockResolvedValue(['kg'])
    const wrapper = await mountSection()
    const rows = wrapper.findAll('[data-test="source-order-row"]')

    await rows[1].find('[data-test="source-order-toggle"]').trigger('click')
    await flushPromises()

    expect(mocks.setDisabled).toHaveBeenCalledWith('kg', true)
    expect(rows[1].attributes('data-enabled')).toBe('false')
  })

  it('rolls the toggle back when saving fails', async () => {
    mocks.setDisabled.mockRejectedValue(new Error('offline'))
    const wrapper = await mountSection()
    const rows = wrapper.findAll('[data-test="source-order-row"]')

    await rows[1].find('[data-test="source-order-toggle"]').trigger('click')
    await flushPromises()

    expect(wrapper.findAll('[data-test="source-order-row"]')[1].attributes('data-enabled')).toBe('true')
    expect(wrapper.find('[data-test="source-order-error"]').exists()).toBe(true)
  })

  it('warns when every source is disabled', async () => {
    mocks.preferences.mockResolvedValue(preferences({ disabled: ['wy', 'kg'] }))
    const wrapper = await mountSection()

    expect(wrapper.find('[data-test="source-order-all-disabled"]').exists()).toBe(true)
  })
})
