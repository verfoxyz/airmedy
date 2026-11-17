<script setup lang="ts">
import { nextTick, onActivated, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { ArrowLeft, MoreVertical, Play, Search, Shuffle } from '@lucide/vue'
import { useI18n } from 'vue-i18n'
import { DetailsButton, IconButton, Input } from '@airmedy/ui'
import ContextMenu from './ContextMenu.vue'
import { useContextMenu } from '@/composables/useContextMenu'
import type { ContextMenuItem } from '@/composables/useContextMenu'

/**
 * Toolbar that takes over once a detail page's hero title scrolls away.
 *
 * The parent owns the sentinel (just after the title) and the page scroller;
 * this component owns the IntersectionObserver, the slide transition and the
 * expandable search. Both are shared by Last.fm and playlist detail pages so
 * they behave identically.
 */
const props = withDefaults(defineProps<{
  title: string
  sentinel: HTMLElement | null
  root: HTMLElement | null
  visible?: boolean
  canPlay?: boolean
  canShuffle?: boolean
  moreItems?: ContextMenuItem[]
  modelValue?: string
  placeholder?: string
  testPrefix?: string
}>(), {
  visible: false,
  canPlay: true,
  canShuffle: true,
  moreItems: () => [],
  modelValue: '',
  placeholder: '',
  testPrefix: 'detail',
})

const emit = defineEmits<{
  'update:visible': [value: boolean]
  'update:modelValue': [value: string]
  back: []
  play: []
  shuffle: []
}>()

const { t } = useI18n()
const searchOpen = ref(false)
const searchInput = ref<{ focus: () => void } | null>(null)
const menu = useContextMenu()
let observer: IntersectionObserver | null = null

function observe() {
  observer?.disconnect()
  observer = null
  const sentinel = props.sentinel
  const root = props.root
  // A test DOM (or an old WebView) may not provide IntersectionObserver; the
  // page still works, it just never shows the takeover toolbar.
  if (typeof IntersectionObserver === 'undefined' || !sentinel || !root) {
    emit('update:visible', false)
    return
  }
  observer = new IntersectionObserver(
    ([entry]) => {
      emit('update:visible', !entry.isIntersecting)
    },
    { root, rootMargin: '-8px 0px 0px 0px', threshold: 0 },
  )
  observer.observe(sentinel)
}

onMounted(observe)

watch(() => [props.sentinel, props.root], observe, { flush: 'post' })

// A hidden bar must not reopen with the search already expanded.
watch(() => props.visible, (visible) => {
  if (!visible) searchOpen.value = false
})

// KeepAlive detaches the scroller; rebuild once the subtree is attached again.
onActivated(() => {
  emit('update:visible', false)
  searchOpen.value = false
  observe()
})

onBeforeUnmount(() => {
  observer?.disconnect()
  observer = null
})

async function openSearch() {
  searchOpen.value = true
  await nextTick()
  searchInput.value?.focus()
}

function openMore(event: MouseEvent) {
  menu.open(event, props.moreItems)
}
</script>

<template>
  <div
    class="absolute inset-x-0 top-0 z-30 h-14 transition-all duration-200 ease-out"
    :class="visible ? 'translate-y-0 opacity-100' : 'pointer-events-none -translate-y-full opacity-0'"
    :data-test="`${testPrefix}-sticky-bar`"
    :data-visible="visible"
  >
    <div
      role="toolbar"
      :aria-label="title"
      class="flex h-full items-center gap-2 bg-glass px-4 ring-1 ring-border-glass backdrop-blur-xl"
    >
      <IconButton
        :data-test="`${testPrefix}-sticky-back`"
        :aria-label="t('common.back')"
        :title="t('common.back')"
        @click="emit('back')"
      >
        <ArrowLeft class="h-4 w-4" aria-hidden="true" />
      </IconButton>

      <h2 class="min-w-0 flex-1 truncate text-sm font-medium text-foreground" :title="title">
        {{ title }}
      </h2>

      <DetailsButton
        :icon="Play"
        :label="t('common.play')"
        :filled-icon="true"
        :data-test="`${testPrefix}-sticky-play`"
        :disabled="!canPlay"
        @click="emit('play')"
      />
      <DetailsButton
        :icon="Shuffle"
        variant="outline"
        :data-test="`${testPrefix}-sticky-shuffle`"
        :disabled="!canShuffle"
        :title="t('common.shuffle')"
        :aria-label="t('common.shuffle')"
        @click="emit('shuffle')"
      />
      <DetailsButton
        :icon="MoreVertical"
        variant="outline"
        :data-test="`${testPrefix}-sticky-more`"
        :aria-label="t('lastfm.more_actions')"
        :title="t('lastfm.more_actions')"
        @click="openMore"
      />

      <Input
        v-if="searchOpen"
        ref="searchInput"
        :model-value="modelValue"
        type="text"
        :placeholder="placeholder || t('sidebar.search')"
        class="w-[220px]"
        clearable
        :data-test="`${testPrefix}-sticky-search-input`"
        @update:model-value="(value) => emit('update:modelValue', String(value))"
        @keydown.esc="searchOpen = false"
        @blur="searchOpen = false"
      />
      <IconButton
        v-else
        :data-test="`${testPrefix}-sticky-search`"
        :active="!!modelValue"
        :aria-label="t('sidebar.search')"
        :title="t('sidebar.search')"
        @click="openSearch"
      >
        <Search class="h-4 w-4" aria-hidden="true" />
      </IconButton>
    </div>
  </div>

  <ContextMenu
    :visible="menu.visible.value"
    :x="menu.x.value"
    :y="menu.y.value"
    :items="menu.items.value"
    @close="menu.close()"
  />
</template>
