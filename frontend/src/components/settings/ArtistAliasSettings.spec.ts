import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import ArtistAliasSettings from './ArtistAliasSettings.vue'

const mocks = vi.hoisted(() => ({
  list: vi.fn(),
  add: vi.fn(),
  remove: vi.fn(),
  setEnabled: vi.fn(),
}))

vi.mock('vue-i18n', () => ({
  useI18n: () => ({ t: (key: string) => key }),
}))

vi.mock('../../../bindings/airmedy/internal/infra/wails/libraryservice', () => ({
  ListArtistAliases: mocks.list,
  AddArtistAlias: mocks.add,
  RemoveArtistAlias: mocks.remove,
  SetArtistAliasEnabled: mocks.setEnabled,
}))

function alias(overrides: Record<string, unknown> = {}) {
  return {
    alias: 'camellia',
    canonical: 'かめりあ',
    evidence: 'manual',
    enabled: true,
    created_at: '2026-01-01T00:00:00Z',
    ...overrides,
  }
}

async function mountSection() {
  const wrapper = mount(ArtistAliasSettings, {
    global: {
      mocks: { $t: (key: string) => key },
      stubs: {
        Input: {
          props: ['modelValue', 'placeholder'],
          emits: ['update:modelValue'],
          template: '<input :value="modelValue" @input="$emit(\'update:modelValue\', $event.target.value)" />',
        },
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

describe('ArtistAliasSettings', () => {
  beforeEach(() => {
    mocks.list.mockReset()
    mocks.add.mockReset()
    mocks.remove.mockReset()
    mocks.setEnabled.mockReset()
    mocks.list.mockResolvedValue([alias()])
    mocks.add.mockResolvedValue(undefined)
    mocks.remove.mockResolvedValue(undefined)
    mocks.setEnabled.mockResolvedValue(undefined)
  })

  it('renders every pair with its evidence', async () => {
    mocks.list.mockResolvedValue([
      alias({ evidence: 'merge' }),
      alias({ alias: 'a', canonical: 'b', evidence: 'manual' }),
    ])
    const wrapper = await mountSection()

    const rows = wrapper.findAll('[data-test="artist-alias-row"]')
    expect(rows).toHaveLength(2)
    expect(rows[0].text()).toContain('camellia')
    expect(rows[0].text()).toContain('settings.artist_aliases.evidence_merge')
    expect(rows[1].text()).toContain('settings.artist_aliases.evidence_manual')
  })

  it('shows the empty state when nothing is stored', async () => {
    mocks.list.mockResolvedValue([])
    const wrapper = await mountSection()
    expect(wrapper.find('[data-test="artist-alias-empty"]').exists()).toBe(true)
  })

  it('adds a normalized pair and reloads', async () => {
    const wrapper = await mountSection()
    const inputs = wrapper.findAll('input')
    await inputs[0].setValue(' Camellia ')
    await inputs[1].setValue('かめりあ')
    await wrapper.find('[data-test="artist-alias-add"]').trigger('click')
    await flushPromises()

    expect(mocks.add).toHaveBeenCalledWith('Camellia', 'かめりあ')
    expect(mocks.list).toHaveBeenCalledTimes(2)
  })

  it('rejects an empty pair before calling the backend', async () => {
    const wrapper = await mountSection()
    await wrapper.find('[data-test="artist-alias-add"]').trigger('click')
    await flushPromises()

    expect(mocks.add).not.toHaveBeenCalled()
    expect(wrapper.find('[data-test="artist-alias-error"]').text()).toBe(
      'settings.artist_aliases.invalid',
    )
  })

  it('rejects a self reference before calling the backend', async () => {
    const wrapper = await mountSection()
    const inputs = wrapper.findAll('input')
    await inputs[0].setValue('Camellia')
    await inputs[1].setValue('camellia')
    await wrapper.find('[data-test="artist-alias-add"]').trigger('click')
    await flushPromises()

    expect(mocks.add).not.toHaveBeenCalled()
    expect(wrapper.find('[data-test="artist-alias-error"]').text()).toBe(
      'settings.artist_aliases.self',
    )
  })

  it('maps a duplicate backend error to the exists message', async () => {
    mocks.add.mockRejectedValue(new Error('artist alias: this pair already exists'))
    const wrapper = await mountSection()
    const inputs = wrapper.findAll('input')
    await inputs[0].setValue('a')
    await inputs[1].setValue('b')
    await wrapper.find('[data-test="artist-alias-add"]').trigger('click')
    await flushPromises()

    expect(wrapper.find('[data-test="artist-alias-error"]').text()).toBe(
      'settings.artist_aliases.exists',
    )
  })

  it('toggles a pair and removes a manual one', async () => {
    const wrapper = await mountSection()
    await wrapper.find('[data-test="artist-alias-toggle"]').trigger('click')
    await flushPromises()
    expect(mocks.setEnabled).toHaveBeenCalledWith('camellia', 'かめりあ', false)

    await wrapper.find('[data-test="artist-alias-delete"]').trigger('click')
    await flushPromises()
    expect(mocks.remove).toHaveBeenCalledWith('camellia', 'かめりあ')
  })

  it('does not offer delete for a learned pair', async () => {
    mocks.list.mockResolvedValue([alias({ evidence: 'merge' })])
    const wrapper = await mountSection()
    expect(wrapper.find('[data-test="artist-alias-delete"]').exists()).toBe(false)
    expect(wrapper.find('[data-test="artist-alias-toggle"]').exists()).toBe(true)
  })
})
