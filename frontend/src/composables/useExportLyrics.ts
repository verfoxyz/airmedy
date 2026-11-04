import { ref } from 'vue'
import * as LyricsService from '../../bindings/airmedy/internal/infra/wails/lyricsservice'

export type LyricsExportPhase = 'idle' | 'conflict' | 'done' | 'error'

// Module-level state, like the other small dialogs: the lyrics drawer and the
// track context menu start an export, the single dialog in App.vue reports it.
const isVisible = ref(false)
const phase = ref<LyricsExportPhase>('idle')
const trackID = ref<string | null>(null)
const targetPath = ref('')
const message = ref('')
const isWorking = ref(false)
const synced = ref(true)

function errorText(e: unknown): string {
  return e instanceof Error ? e.message : String(e)
}

async function run(overwrite: boolean) {
  const id = trackID.value
  if (!id || isWorking.value) return
  isWorking.value = true
  message.value = ''
  try {
    const result = await LyricsService.ExportLyrics(id, overwrite)
    targetPath.value = result.path
    if (result.exists) {
      // Existing .lrc: never overwrite silently, the dialog asks.
      phase.value = 'conflict'
      isVisible.value = true
      return
    }
    phase.value = 'done'
    isVisible.value = true
  } catch (e) {
    // Covers "no lyrics", "no local file" (remote track) and write failures.
    phase.value = 'error'
    message.value = errorText(e)
    isVisible.value = true
  } finally {
    isWorking.value = false
  }
}

export function useExportLyrics() {
  /** Exports the audio-sibling .lrc. Shows the dialog only for a conflict,
   *  an error, or the "saved to" confirmation. */
  async function open(track: { id: string }, isSynced = true) {
    trackID.value = track.id
    synced.value = isSynced
    phase.value = 'idle'
    targetPath.value = ''
    message.value = ''
    isVisible.value = false
    await run(false)
  }

  async function overwrite() {
    await run(true)
  }

  async function saveAs() {
    const id = trackID.value
    if (!id || isWorking.value) return
    isWorking.value = true
    message.value = ''
    try {
      const path = await LyricsService.ExportLyricsAs(id)
      if (!path) {
        // The user cancelled the OS file picker: keep the conflict dialog up.
        phase.value = 'conflict'
        return
      }
      targetPath.value = path
      phase.value = 'done'
    } catch (e) {
      phase.value = 'error'
      message.value = errorText(e)
    } finally {
      isWorking.value = false
    }
  }

  function close() {
    isVisible.value = false
    phase.value = 'idle'
    trackID.value = null
    targetPath.value = ''
    message.value = ''
    synced.value = true
  }

  return {
    isVisible,
    phase,
    targetPath,
    message,
    isWorking,
    synced,
    open,
    overwrite,
    saveAs,
    close,
  }
}
