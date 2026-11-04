<script setup lang="ts">
import { Modal } from '@airmedy/ui'
import { useI18n } from 'vue-i18n'

defineProps<{
  open: boolean
  title: string
  message: string
  confirmLabel?: string
  cancelLabel?: string
  danger?: boolean
}>()

const emit = defineEmits<{
  'update:open': [value: boolean]
  'confirm': []
  'cancel': []
}>()

const { t } = useI18n()

function handleCancel() {
  emit('cancel')
  emit('update:open', false)
}

function handleConfirm() {
  // The payload event goes first. A parent that clears its pending target on
  // update:open would otherwise see an empty target by the time it handles
  // confirm — the deletion silently never happened.
  emit('confirm')
  emit('update:open', false)
}
</script>

<template>
  <Modal :open="open" :title="title" @close="handleCancel">
    <p class="text-sm text-foreground/70 leading-relaxed">
      {{ message }}
    </p>

    <template #footer>
      <div class="flex gap-2 justify-end">
        <button
          @click="handleCancel"
          class="px-3 py-1.5 text-sm font-medium rounded-lg hover:bg-foreground/5 transition-colors"
        >
          {{ cancelLabel ?? t('common.cancel') }}
        </button>
        <button
          @click="handleConfirm"
          :class="[
            'px-3 py-1.5 text-sm font-medium rounded-lg transition-colors',
            danger ? 'bg-red-500 text-white hover:bg-red-600' : 'bg-primary text-primary-foreground hover:bg-primary/90'
          ]"
        >
          {{ confirmLabel ?? t('common.confirm') }}
        </button>
      </div>
    </template>
  </Modal>
</template>
