<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import { AlertCircle, Ban, LoaderCircle, RotateCcw, X } from '@lucide/vue'
import type { DownloadJobView } from '@/stores/download'

/**
 * One row of the download queue.
 *
 * The row never decides what a state means — the store already dropped `done`
 * jobs and hid stale failed/canceled rows behind a newer retry. It only picks
 * the copy, the icon and whether cancel/retry apply.
 *
 * `bytesTotal === 0` is "unknown size", not "0% done": the CDN often omits a
 * Content-Length, so the bar renders an indeterminate stripe instead.
 */
const props = defineProps<{
  job: DownloadJobView
  busy?: boolean
}>()

const emit = defineEmits<{
  cancel: [jobId: string]
  retry: [jobId: string]
}>()

const { t } = useI18n()

const isIndeterminate = computed(() => props.job.state === 'active' && props.job.bytesTotal <= 0)
const isFailed = computed(() => props.job.state === 'failed')
const isCanceled = computed(() => props.job.state === 'canceled')
const isActive = computed(() => props.job.state === 'active')
const isQueued = computed(() => props.job.state === 'queued')
const canCancel = computed(() => isActive.value || isQueued.value)
const canRetry = computed(() => isFailed.value || isCanceled.value)

/** Title, then the id, then a fallback: a deleted track must not blank the row. */
const title = computed(() => props.job.title || props.job.trackId || t('download.unknown_track'))
const artist = computed(() => props.job.artist)

const percent = computed(() => Math.min(100, Math.max(0, props.job.percent)))

const statusText = computed(() => {
  if (isFailed.value) return props.job.error || t('download.failed')
  if (isCanceled.value) return t('download.canceled')
  if (isQueued.value) return t('download.queued_hint')
  if (isIndeterminate.value) return t('download.downloading')
  return t('download.percent', { percent: percent.value })
})

const attemptText = computed(() => {
  const { attempts, maxAttempts } = props.job
  if (!isFailed.value || maxAttempts <= 0) return ''
  return t('download.attempt', { attempt: attempts, total: maxAttempts })
})
</script>

<template>
  <div
    class="group flex items-start gap-3 rounded-xl px-3 py-2.5 transition-colors hover:bg-foreground/[0.04]"
    :data-test="`download-job-${job.state}`"
    :data-job-id="job.jobId">
    <div class="mt-0.5 shrink-0">
      <AlertCircle v-if="isFailed" class="h-4 w-4 text-primary" />
      <Ban v-else-if="isCanceled" class="h-4 w-4 text-dimmer" />
      <LoaderCircle v-else-if="isActive" class="h-4 w-4 animate-spin text-primary" />
      <LoaderCircle v-else class="h-4 w-4 text-dimmer" />
    </div>

    <div class="min-w-0 flex-1">
      <p class="truncate text-sm font-medium" :class="{ 'text-dim': isCanceled || isQueued }">
        {{ title }}
      </p>
      <p v-if="artist" class="truncate text-xs text-[color:var(--text-muted)]">{{ artist }}</p>

      <!-- Progress: a real bar while active, nothing while queued/canceled/failed. -->
      <div v-if="isActive" class="mt-1.5 flex items-center gap-2">
        <div
          class="h-1 flex-1 overflow-hidden rounded-full bg-foreground/10 transition-all duration-300 group-hover:h-1.5"
          role="progressbar"
          :aria-label="statusText"
          :aria-valuenow="isIndeterminate ? undefined : percent"
          aria-valuemin="0"
          aria-valuemax="100"
          :data-indeterminate="isIndeterminate ? 'true' : undefined">
          <div
            v-if="isIndeterminate"
            class="h-full w-1/3 animate-pulse rounded-full bg-primary"
            data-test="download-bar-indeterminate" />
          <div
            v-else
            class="h-full rounded-full bg-primary transition-all duration-300"
            :style="{ width: `${percent}%` }"
            data-test="download-bar-fill" />
        </div>
        <span class="shrink-0 text-[10px] tabular-nums text-[color:var(--text-muted)]">
          {{ isIndeterminate ? t('download.waiting') : `${percent}%` }}
        </span>
      </div>

      <p
        v-else
        class="mt-1 truncate text-[11px]"
        :class="isFailed ? 'text-primary opacity-90' : 'text-dimmer'"
        :title="isFailed ? job.error : undefined"
        data-test="download-status">
        {{ statusText }}
      </p>

      <p v-if="attemptText" class="text-[10px] text-dimmer">{{ attemptText }}</p>
    </div>

    <button
      v-if="canCancel"
      type="button"
      class="shrink-0 rounded-full p-1.5 text-dim opacity-0 transition-all hover:bg-foreground/[0.06] hover:text-foreground focus-visible:opacity-100 group-hover:opacity-100"
      :disabled="busy"
      :title="t('download.cancel')"
      :aria-label="t('download.cancel')"
      data-test="download-cancel"
      @click="emit('cancel', job.jobId)">
      <X class="h-3.5 w-3.5" />
    </button>
    <button
      v-else-if="canRetry"
      type="button"
      class="flex shrink-0 items-center gap-1 rounded-full px-2 py-1 text-[11px] font-medium text-dim transition-colors hover:bg-foreground/[0.06] hover:text-foreground"
      :disabled="busy"
      :title="t('download.retry')"
      data-test="download-retry"
      @click="emit('retry', job.jobId)">
      <RotateCcw class="h-3 w-3" />
      {{ t('download.retry') }}
    </button>
  </div>
</template>
