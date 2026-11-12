import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { nextTick } from 'vue'
import LastFmView from './LastFmView.vue'

const mocks = vi.hoisted(() => ({
  push: vi.fn(),
  list: vi.fn(),
  refreshList: vi.fn(),
  refreshAll: vi.fn(),
  handlers: new Map<string, (event: { data?: unknown }) => void>(),
  off: vi.fn(),
}))

vi.mock('vue-i18n', () => ({
  useI18n: () => ({ t: (key: string) => key }),
}))

vi.mock('vue-router', () => ({
  useRouter: () => ({ push: mocks.push }),
}))

vi.mock('@wailsio/runtime', async (importOriginal) => ({
  ...await importOriginal<typeof import('@wailsio/runtime')>(),
  Events: {
    On: (name: string, handler: (event: { data?: unknown }) => void) => {
      mocks.handlers.set(name, handler)
      return mocks.off
    },
  },
}))

vi.mock('../../bindings/airmedy/internal/infra/wails/lastfmservice', () => ({
  ListRecommendationLists: mocks.list,
  RefreshRecommendationList: mocks.refreshList,
  RefreshAllRecommendationLists: mocks.refreshAll,
}))

function summary(kind: string, count: number, covers: string[] = []) {
  return {
    kind,
    title: kind === 'similar' ? 'lastfm.similar_tracks' : 'lastfm.top',
    track_count: count,
    matching_count: 0,
    cover_urls: covers,
    updated_at: '2026-01-01T00:00:00Z',
  }
}

function collection(status = 'ok') {
  return {
    status,
    lists: status === 'not_connected'
      ? []
      : [summary('similar', 3, ['https://cover/a.jpg']), summary('top_tracks', 50)],
  }
}

async function mountView() {
  const wrapper = mount(LastFmView, {
    global: {
      mocks: { $t: (key: string) => key },
    },
  })
  await flushPromises()
  return wrapper
}

async function emitMatch(data: unknown) {
  const handler = mocks.handlers.get('lastfm:recommendations-matched')
  expect(handler).toBeDefined()
  handler!({ data })
  await nextTick()
}

describe('LastFmView (persisted recommendation cards)', () => {
  beforeEach(() => {
    mocks.push.mockReset()
    mocks.list.mockReset()
    mocks.refreshList.mockReset()
    mocks.refreshAll.mockReset()
    mocks.handlers.clear()
    mocks.off.mockReset()
    mocks.refreshList.mockResolvedValue(undefined)
    mocks.refreshAll.mockResolvedValue(undefined)
  })

  it('guides the user to settings when Last.fm is not connected', async () => {
    mocks.list.mockResolvedValue(collection('not_connected'))
    const wrapper = await mountView()

    expect(wrapper.find('[data-test="lastfm-not-connected"]').exists()).toBe(true)
    await wrapper.find('[data-test="lastfm-open-settings"]').trigger('click')
    expect(mocks.push).toHaveBeenCalledWith('/settings/integrations')
  })

  it('renders one card per list with its count and covers', async () => {
    mocks.list.mockResolvedValue(collection())
    const wrapper = await mountView()

    const cards = wrapper.findAll('[data-test="lastfm-card"]')
    expect(cards).toHaveLength(2)
    expect(cards[0].attributes('data-kind')).toBe('similar')
    expect(cards[0].text()).toContain('lastfm.similar_tracks')
    expect(cards[0].find('[data-test="lastfm-card-count"]').text()).toBe('3')
    expect(cards[1].attributes('data-kind')).toBe('top_tracks')
    expect(cards[1].find('[data-test="lastfm-card-count"]').text()).toBe('50')
    expect(cards[0].findAll('img')).toHaveLength(1)
  })

  it('opens the list detail when a card is clicked', async () => {
    mocks.list.mockResolvedValue(collection())
    const wrapper = await mountView()

    await wrapper.findAll('[data-test="lastfm-card"]')[1].trigger('click')
    expect(mocks.push).toHaveBeenCalledWith('/lastfm/lists/top_tracks')
  })

  it('refreshes only the clicked card', async () => {
    mocks.list.mockResolvedValue(collection())
    const wrapper = await mountView()

    const buttons = wrapper.findAll('[data-test="lastfm-card-refresh"]')
    await buttons[0].trigger('click')
    await flushPromises()

    expect(mocks.refreshList).toHaveBeenCalledTimes(1)
    expect(mocks.refreshList).toHaveBeenCalledWith('similar')
    expect(mocks.refreshAll).not.toHaveBeenCalled()
    expect(mocks.list).toHaveBeenCalledTimes(2)
  })

  it('refreshes every list from the header', async () => {
    mocks.list.mockResolvedValue(collection())
    const wrapper = await mountView()

    await wrapper.find('[data-test="lastfm-refresh-all"]').trigger('click')
    await flushPromises()

    expect(mocks.refreshAll).toHaveBeenCalledTimes(1)
    expect(mocks.list).toHaveBeenCalledTimes(2)
  })

  it('reloads the cards when a worker run finishes', async () => {
    mocks.list.mockResolvedValue(collection())
    await mountView()
    expect(mocks.list).toHaveBeenCalledTimes(1)

    await emitMatch({ kind: 'top_tracks', items: [], done: true })
    await flushPromises()

    expect(mocks.list).toHaveBeenCalledTimes(2)
  })

  it('shows the empty state and keeps a refresh action', async () => {
    mocks.list.mockResolvedValue({ status: 'ok', lists: [] })
    const wrapper = await mountView()

    const empty = wrapper.find('[data-test="lastfm-empty"]')
    expect(empty.exists()).toBe(true)
    await empty.find('button').trigger('click')
    await flushPromises()
    expect(mocks.refreshAll).toHaveBeenCalledTimes(1)
  })

  it('releases the match listener on unmount', async () => {
    mocks.list.mockResolvedValue(collection())
    const wrapper = await mountView()

    wrapper.unmount()

    expect(mocks.off).toHaveBeenCalledTimes(1)
  })
})
