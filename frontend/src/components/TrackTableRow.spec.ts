import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import { createTestingPinia } from '@pinia/testing'
import TrackTableRow from './TrackTableRow.vue'
import { TrackDTO } from '../../bindings/airmedy/internal/domain/models'
import type { ColumnDef } from '@/composables/useTrackTableSettings'

vi.mock('vue-i18n', () => ({
  useI18n: () => ({ t: (key: string) => key }),
}))

vi.mock('@/components/LazyImg.vue', () => ({
  default: { props: ['src'], template: '<img v-if="src" :src="src" />' },
}))

vi.mock('@wailsio/runtime', () => ({
  Events: { On: vi.fn(() => () => {}) },
  Create: {
    Nullable: (fn: any) => (v: any) => (v == null ? null : fn(v)),
    Array: (fn: any) => (arr: any[]) => (arr ?? []).map(fn),
    Struct: (ctor: any) => (v: any) => (v == null ? null : new ctor(v)),
    Map: (k: any, v: any) => (val: any) => val,
    Any: (v: any) => v,
  },
  Call: { ByID: vi.fn().mockResolvedValue(null) },
}))

const ALBUM_COLUMN: ColumnDef = {
  key: 'album',
  labelKey: 'library.album',
  gridWidth: '1fr',
  minWidthPx: 80,
  alwaysVisible: false,
  sortable: true,
  draggable: true,
}

function mountRow(track: TrackDTO) {
  return mount(TrackTableRow, {
    props: {
      track,
      index: 0,
      currentIndex: -1,
      orderedVisibleColumns: [ALBUM_COLUMN],
      gridTemplateColumns: '1fr',
      rowBg: () => '',
    },
    global: {
      plugins: [createTestingPinia({ createSpy: vi.fn })],
      mocks: { $t: (key: string) => key },
      stubs: { PlayingBar: true, DownloadProgressRing: true },
    },
  })
}

describe('TrackTableRow album cell', () => {
  it('shows the raw album snapshot as plain text for a remote row', async () => {
    const wrapper = mountRow(new TrackDTO({
      id: 'remote-1',
      availability: 'remote',
      raw_album_name: 'Some Album',
      album: null,
    }))

    const cell = wrapper.find('span.truncate')
    expect(cell.text()).toBe('Some Album')
    expect(cell.classes()).not.toContain('cursor-pointer')

    await cell.trigger('click')
    expect(wrapper.emitted('navigate-album')).toBeUndefined()
    wrapper.unmount()
  })

  it('keeps a local album clickable', async () => {
    const wrapper = mountRow(new TrackDTO({
      id: 'local-1',
      availability: 'ready',
      album: { id: 'album-1', title: 'Local Album' } as never,
    }))

    const cell = wrapper.find('span.truncate')
    expect(cell.text()).toBe('Local Album')
    expect(cell.classes()).toContain('cursor-pointer')

    await cell.trigger('click')
    expect(wrapper.emitted('navigate-album')?.[0]).toEqual(['album-1'])
    wrapper.unmount()
  })
})
