<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import type { DownloadState } from '@/stores/download'

/**
 * A small progress ring for a track that is being downloaded.
 *
 * `state` is undefined for tracks with nothing in flight, in which case the
 * component renders nothing at all — callers can always mount it.
 *
 * The ring is indeterminate while `bytesTotal` is 0 (the CDN often omits a
 * Content-Length): showing a 0% ring there would read as "stuck".
 */
const props = defineProps<{
  state?: DownloadState
  size?: number
}>()

const { t } = useI18n()

const diameter = computed(() => props.size ?? 16)
const strokeWidth = 2
const radius = computed(() => (diameter.value - strokeWidth) / 2)
const circumference = computed(() => 2 * Math.PI * radius.value)

const isIndeterminate = computed(() => {
  const s = props.state
  return s !== undefined && s.bytesTotal === 0 && (s.state === 'active' || s.state === 'queued')
})

const isFailed = computed(() => props.state?.state === 'failed')

const dashOffset = computed(() => {
  const percent = Math.min(100, Math.max(0, props.state?.percent ?? 0))
  return circumference.value * (1 - percent / 100)
})

const label = computed(() => {
  const s = props.state
  if (!s) return ''
  if (s.state === 'failed') return s.error || t('download.failed')
  if (s.state === 'queued') return t('download.queued')
  if (isIndeterminate.value) return t('download.downloading')
  return t('download.percent', { percent: s.percent })
})
</script>

<template>
  <span
    v-if="state"
    class="inline-flex items-center justify-center shrink-0"
    :class="{ 'animate-spin': isIndeterminate }"
    role="progressbar"
    :aria-label="label"
    :aria-valuenow="isIndeterminate ? undefined : state.percent"
    aria-valuemin="0"
    aria-valuemax="100"
    data-test="download-ring"
    :data-state="state.state"
  >
    <svg :width="diameter" :height="diameter" :viewBox="`0 0 ${diameter} ${diameter}`" class="-rotate-90">
      <circle
        :cx="diameter / 2" :cy="diameter / 2" :r="radius"
        fill="none" :stroke-width="strokeWidth"
        class="text-foreground opacity-15" stroke="currentColor"
      />
      <circle
        v-if="!isIndeterminate"
        :cx="diameter / 2" :cy="diameter / 2" :r="radius"
        fill="none" :stroke-width="strokeWidth"
        :stroke-dasharray="circumference"
        :stroke-dashoffset="dashOffset"
        :class="isFailed ? 'text-primary opacity-60' : 'text-primary'"
        stroke="currentColor"
        stroke-linecap="round"
        class="transition-[stroke-dashoffset] duration-300"
      />
      <!-- Indeterminate: a quarter arc the parent spin animates. -->
      <circle
        v-else
        :cx="diameter / 2" :cy="diameter / 2" :r="radius"
        fill="none" :stroke-width="strokeWidth"
        :stroke-dasharray="`${circumference / 4} ${circumference}`"
        class="text-primary" stroke="currentColor" stroke-linecap="round"
      />
    </svg>
  </span>
</template>
