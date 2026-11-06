import { computed, type Ref } from 'vue'
import { decodeHTMLEntities } from '@airmedy/utils'

const LINE_PATTERN = /^\[(\d+):([0-5]\d(?:\.\d{1,3})?)\](.*)/
const METADATA_TAG = /^\[[a-zA-Z]+:.*\]$/
const INLINE_TAG = /<\d+:[^<>]*>/g
// A translated LRC and the original do not always share the exact same
// millisecond, so lines are matched with a small tolerance instead of by string.
const TIME_TOLERANCE_SECONDS = 0.5

function clean(text: string) {
  return text.replace(INLINE_TAG, '').trim()
}

interface TranslatedLine {
  time?: number
  text: string
}

/** parseTranslation splits a stored translation body into timed or plain lines. */
function parseTranslation(raw: string): TranslatedLine[] {
  let decoded: string
  try {
    decoded = decodeHTMLEntities(raw)
  } catch {
    decoded = raw
  }
  return decoded.split('\n').map((line) => {
    const trimmed = line.trim()
    if (METADATA_TAG.test(trimmed)) return { text: '' }
    const match = trimmed.match(LINE_PATTERN)
    if (!match) return { text: clean(trimmed) }
    const time = Number(match[1]) * 60 + Number(match[2])
    return Number.isFinite(time) ? { time, text: clean(match[3]) } : { text: '' }
  })
}

/**
 * useLyricTranslation aligns a stored translation to the lines the lyrics view
 * renders. A translation carrying timestamps is matched by time (a plugin's
 * `tlyric`); a translation without timestamps (a legacy split, or a plain-body
 * translation) is matched by line index. Empty slots stay undefined, so a
 * missing translation never renders an empty secondary line.
 */
export function useLyricTranslation(
  translation: Ref<string | undefined>,
  lineCount: Ref<number>,
  lineTimes: Ref<number[]>,
) {
  return computed<(string | undefined)[]>(() => {
    const raw = translation.value
    if (!raw || lineCount.value === 0) return []

    const parsed = parseTranslation(raw)
    const timed = parsed.filter((line): line is TranslatedLine & { time: number } =>
      line.time !== undefined && line.text !== '',
    )
    const result: (string | undefined)[] = []

    if (lineTimes.value.length > 0 && timed.length > 0) {
      for (const time of lineTimes.value) {
        let nearest: TranslatedLine | undefined
        let nearestDelta = TIME_TOLERANCE_SECONDS
        for (const line of timed) {
          const delta = Math.abs(line.time - time)
          if (delta <= nearestDelta) {
            nearest = line
            nearestDelta = delta
          }
        }
        result.push(nearest?.text || undefined)
      }
      return result
    }

    // No timeline to match against: keep the translation's line order, which is
    // what a legacy "original ^ translation" split produced.
    for (let i = 0; i < lineCount.value; i++) {
      result.push(parsed[i]?.text || undefined)
    }
    return result
  })
}
