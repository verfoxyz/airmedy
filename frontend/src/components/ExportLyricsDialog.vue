<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import { AlertTriangle, CheckCircle2, FileDown, Loader2 } from '@lucide/vue'
import { Modal } from '@airmedy/ui'
import { useExportLyrics } from '@/composables/useExportLyrics'

const { t } = useI18n()
const { isVisible, phase, targetPath, message, isWorking, synced, overwrite, saveAs, close } = useExportLyrics()

const title = computed(() => {
  if (phase.value === 'done') return t('export_lyrics.saved_title')
  if (phase.value === 'error') return t('export_lyrics.failed_title')
  return t('export_lyrics.title')
})
</script>

<template>
  <Modal :open="isVisible" :title="title" width-class="w-[26rem]" @close="close">
    <div v-if="phase === 'conflict'" class="space-y-4">
      <p class="text-sm text-dim">{{ t('export_lyrics.exists', { path: targetPath }) }}</p>
      <div class="space-y-2">
        <button
          data-test="export-overwrite"
          class="flex w-full items-center gap-3 rounded-xl border border-foreground/[0.06] px-4 py-3 text-left transition-colors hover:bg-foreground/[0.04] disabled:opacity-50"
          :disabled="isWorking"
          @click="overwrite"
        >
          <FileDown class="h-4 w-4 shrink-0 text-dim" />
          <span class="text-sm font-medium text-foreground">{{ t('export_lyrics.overwrite') }}</span>
        </button>
        <button
          data-test="export-save-as"
          class="flex w-full items-center gap-3 rounded-xl border border-foreground/[0.06] px-4 py-3 text-left transition-colors hover:bg-foreground/[0.04] disabled:opacity-50"
          :disabled="isWorking"
          @click="saveAs"
        >
          <FileDown class="h-4 w-4 shrink-0 text-dim" />
          <span class="text-sm font-medium text-foreground">{{ t('export_lyrics.save_as') }}</span>
        </button>
      </div>
      <p class="text-xs text-dimmer">{{ t('export_lyrics.file_hint') }}</p>
    </div>

    <div v-else-if="phase === 'done'" class="space-y-2" data-test="export-done">
      <div class="flex items-start gap-2 text-sm text-dim">
        <CheckCircle2 class="mt-0.5 h-4 w-4 shrink-0 text-primary" />
        <span class="min-w-0 break-words">{{ t('export_lyrics.saved', { path: targetPath }) }}</span>
      </div>
      <p v-if="!synced" class="text-xs text-dimmer" data-test="export-unsynced">
        {{ t('player.lyrics_unsynced') }}
      </p>
    </div>

    <div v-else-if="phase === 'error'" class="flex items-start gap-2 text-sm text-destructive" data-test="export-error">
      <AlertTriangle class="mt-0.5 h-4 w-4 shrink-0" />
      <span class="min-w-0 break-words">{{ t('export_lyrics.failed', { reason: message }) }}</span>
    </div>

    <div class="mt-5 flex justify-end">
      <button
        data-test="export-close"
        class="inline-flex items-center gap-2 rounded-lg px-4 py-2 text-sm text-dim transition-colors hover:bg-foreground/[0.04]"
        :disabled="isWorking"
        @click="close"
      >
        <Loader2 v-if="isWorking" class="h-3.5 w-3.5 animate-spin" />
        {{ t('common.close') }}
      </button>
    </div>
  </Modal>
</template>
