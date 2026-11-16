<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import { AlertCircle, Ban, Check, LoaderCircle, RotateCcw, X } from '@lucide/vue'
import type { DownloadJobView } from '@/stores/download'

/**
 * One row of the download queue or the tasks page.
 *
 * The row never decides what a state means — the store already grouped it into
 * a tab. It only picks the copy, the icon and whether cancel/retry apply. A
 * media request has no bytes of its own, so its row reports the request phase
 * as text; a stream-cache session has no user actions at all.
 *
 * `bytesTotal === 0` is "unknown size", not "0% done": the CDN often omits a
 * Content-Length, so the bar renders an indeterminate stripe instead.
 */
const props = defineProps<{
  job: DownloadJobView
  kind?: 'request' | 'cache'
  phase?: string
  busy?: boolean
}>()

const emit = defineEmits<{
  cancel: [jobId: string]
  retry: [jobId: string]
}>()

const { t } = useI18n()

const isRequest = computed(() => props.kind !== 'cache')
const isCache = computed(() => props.kind === 'cache')
const isFailed = computed(() => props.job.state === 'failed')
const isCanceled = computed(() => props.job.state === 'canceled')
const isDone = computed(() => props.job.state === 'done')
const isActive = computed(() => props.job.state === 'active')
const isQueued = computed(() => props.job.state === 'queued')
// A cache session has no user-facing cancel or retry: it belongs to playback,
// and finishing (or failing) it is the stream cache's decision. A request can
// be canceled while it runs and retried after it failed.
const canCancel = computed(() => isRequest.value && (isActive.value || isQueued.value))
const canRetry = computed(() => isRequest.value && (isFailed.value || isCanceled.value))

/** Title, then the id, then a fallback: a deleted track must not blank the row. */
const title = computed(() => props.job.title || props.job.trackId || t('download.unknown_track'))
const artist = computed(() => props.job.artist)
/** The id is a secondary label; showing it twice when it is the title is noise. */
const showTrackId = computed(() => !!props.job.trackId && title.value !== props.job.trackId)

const phaseText = computed(() => {
  switch (props.phase) {
    case 'resolving':
      return t('tasks.phase_resolving')
    case 'downloading':
      return t('tasks.phase_downloading')
    case 'promoting':
      return t('tasks.phase_promoting')
    case 'verifying':
      return t('tasks.phase_verifying')
    default:
      return ''
  }
})

const statusText = computed(() => {
  if (isFailed.value) return props.job.error || t('download.failed')
  if (isCanceled.value) return t('download.canceled')
  if (isDone.value) return props.job.quality || t('tasks.tab_done')
  if (phaseText.value) return phaseText.value
  if (isQueued.value) return t('download.queued_hint')
  return t('download.downloading')
})

const attemptText = computed(() => {
  const { attempts, maxAttempts } = props.job
  if (!isFailed.value || maxAttempts <= 0) return ''
  return t('download.attempt', { attempt: attempts, total: maxAttempts })
})

const kindLabel = computed(() => {
  if (isCache.value) return t('tasks.kind_cache')
  return t('tasks.kind_request')
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
      <Check v-else-if="isDone" class="h-4 w-4 text-dim" />
      <LoaderCircle v-else-if="isActive" class="h-4 w-4 animate-spin text-primary" />
      <LoaderCircle v-else class="h-4 w-4 text-dimmer" />
    </div>

    <div class="min-w-0 flex-1">
      <div class="flex min-w-0 items-center gap-1.5">
        <span
          class="shrink-0 rounded-full bg-foreground/[0.06] px-1.5 py-0.5 text-[10px] font-medium text-dimmer"
          data-test="task-kind">{{ kindLabel }}</span>
        <p class="min-w-0 truncate text-sm font-medium" :class="{ 'text-dim': isCanceled || isQueued }" data-test="task-title">
          {{ title }}
        </p>
        <span
          v-if="showTrackId"
          class="max-w-[45%] shrink-0 truncate font-mono text-[10px] text-dimmer"
          :title="job.trackId"
          data-test="task-id">{{ job.trackId }}</span>
      </div>
      <p v-if="artist" class="truncate text-xs text-[color:var(--text-muted)]">{{ artist }}</p>

      <!-- A request reports its phase as text: bytes belong to the job it
           delegates to, not to the request row the page lists. -->
      <p
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
