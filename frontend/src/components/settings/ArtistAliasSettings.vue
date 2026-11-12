<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { Languages, Loader2, Plus, Trash2 } from '@lucide/vue'
import { Input, Switch } from '@airmedy/ui'
import * as LibraryService from '../../../bindings/airmedy/internal/infra/wails/libraryservice'
import SettingSection from './SettingSection.vue'

/**
 * Artist alias editor.
 *
 * An alias lets one song match under another spelling of the same artist
 * ("Camellia" / "かめりあ"). Learned pairs can only be disabled; manual pairs can
 * also be deleted.
 */
const { t } = useI18n()

interface ArtistAlias {
  alias: string
  canonical: string
  evidence: string
  enabled: boolean
}

const aliases = ref<ArtistAlias[]>([])
const loading = ref(false)
const saving = ref(false)
const aliasInput = ref('')
const canonicalInput = ref('')
const errorKey = ref('')

async function load() {
  loading.value = true
  try {
    aliases.value = (await LibraryService.ListArtistAliases()) as ArtistAlias[]
  } catch (error) {
    console.error('Failed to load artist aliases', error)
    errorKey.value = 'save_failed'
  } finally {
    loading.value = false
  }
}

/** errorKeyFor maps a backend error onto the message the form shows. */
function errorKeyFor(error: unknown): string {
  const message = error instanceof Error ? error.message : String(error)
  if (message.includes('cannot alias itself')) return 'self'
  if (message.includes('already exists')) return 'exists'
  if (message.includes('both names are required')) return 'invalid'
  return 'save_failed'
}

async function add() {
  const alias = aliasInput.value.trim()
  const canonical = canonicalInput.value.trim()
  errorKey.value = ''
  if (!alias || !canonical) {
    errorKey.value = 'invalid'
    return
  }
  if (alias.toLowerCase() === canonical.toLowerCase()) {
    errorKey.value = 'self'
    return
  }
  saving.value = true
  try {
    await LibraryService.AddArtistAlias(alias, canonical)
    aliasInput.value = ''
    canonicalInput.value = ''
    await load()
  } catch (error) {
    errorKey.value = errorKeyFor(error)
  } finally {
    saving.value = false
  }
}

async function toggle(item: ArtistAlias, enabled: boolean) {
  try {
    await LibraryService.SetArtistAliasEnabled(item.alias, item.canonical, enabled)
    item.enabled = enabled
  } catch (error) {
    console.error('Failed to toggle artist alias', error)
    errorKey.value = errorKeyFor(error)
  }
}

async function remove(item: ArtistAlias) {
  try {
    await LibraryService.RemoveArtistAlias(item.alias, item.canonical)
    await load()
  } catch (error) {
    console.error('Failed to remove artist alias', error)
    errorKey.value = errorKeyFor(error)
  }
}

onMounted(load)
</script>

<template>
  <SettingSection :icon="Languages" :label="t('settings.artist_aliases.title')" variant="panel">
    <p class="mb-6 text-sm text-dim">{{ t('settings.artist_aliases.description') }}</p>

    <div class="mb-6 flex flex-col gap-2 sm:flex-row sm:items-center">
      <Input
        v-model="aliasInput"
        data-test="artist-alias-alias"
        :placeholder="t('settings.artist_aliases.alias_label')"
        class="h-9 flex-1 bg-foreground/[0.04]"
        @keyup.enter="add"
      />
      <span class="hidden text-dimmer sm:block">→</span>
      <Input
        v-model="canonicalInput"
        data-test="artist-alias-canonical"
        :placeholder="t('settings.artist_aliases.canonical_label')"
        class="h-9 flex-1 bg-foreground/[0.04]"
        @keyup.enter="add"
      />
      <button
        type="button"
        class="flex h-9 shrink-0 items-center justify-center gap-2 rounded-lg bg-primary px-4 text-sm font-medium text-primary-foreground transition-opacity hover:opacity-90 disabled:opacity-50"
        data-test="artist-alias-add"
        :disabled="saving"
        @click="add"
      >
        <Loader2 v-if="saving" class="h-4 w-4 animate-spin" aria-hidden="true" />
        <Plus v-else class="h-4 w-4" aria-hidden="true" />
        {{ t('settings.artist_aliases.add') }}
      </button>
    </div>

    <p v-if="errorKey" class="mb-4 text-sm text-dim" data-test="artist-alias-error">
      {{ t(`settings.artist_aliases.${errorKey}`) }}
    </p>

    <div v-if="loading && aliases.length === 0" class="py-6 text-center text-sm text-dimmer">
      {{ t('settings.artist_aliases.loading') }}
    </div>
    <p v-else-if="aliases.length === 0" class="py-6 text-sm text-dimmer" data-test="artist-alias-empty">
      {{ t('settings.artist_aliases.empty') }}
    </p>
    <ul v-else class="divide-y divide-foreground/[0.06]">
      <li
        v-for="item in aliases"
        :key="`${item.alias}|${item.canonical}`"
        class="flex items-center gap-3 py-3"
        data-test="artist-alias-row"
      >
        <div class="min-w-0 flex-1">
          <p class="truncate text-sm text-foreground">
            <span class="font-medium">{{ item.alias }}</span>
            <span class="mx-2 text-dimmer">→</span>
            <span>{{ item.canonical }}</span>
          </p>
          <p class="mt-0.5 text-xs text-dimmer">
            {{ t(`settings.artist_aliases.evidence_${item.evidence}`) }}
          </p>
        </div>

        <Switch
          :model-value="item.enabled"
          :aria-label="t('settings.artist_aliases.enable')"
          data-test="artist-alias-toggle"
          @update:model-value="value => toggle(item, value === true)"
        />
        <button
          v-if="item.evidence === 'manual'"
          type="button"
          class="flex h-8 w-8 items-center justify-center rounded-full text-foreground/60 transition-colors hover:bg-foreground/[0.06] hover:text-foreground/80"
          data-test="artist-alias-delete"
          :aria-label="t('settings.artist_aliases.delete')"
          :title="t('settings.artist_aliases.delete')"
          @click="remove(item)"
        >
          <Trash2 class="h-4 w-4" aria-hidden="true" />
        </button>
      </li>
    </ul>
  </SettingSection>
</template>
