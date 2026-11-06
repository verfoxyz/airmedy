import { computed, type Ref } from 'vue'
import { decodeHTMLEntities } from '@airmedy/utils'

export interface LyricLine {
  text: string
  secondary?: string
  time: number
  words?: LyricWord[]
}

export interface LyricWord {
  text: string
  start: number
  end: number
}

export interface PlainLine {
  primary: string
  secondary?: string
}

const LINE_PATTERN = /^\[(\d+):([0-5]\d(?:\.\d{1,3})?)\](.*)/
const WORD_PATTERN = /^<(\d+):([0-5]\d(?:\.\d{1,3})?)>$/
// Also remove malformed timing tags when falling back to line-level lyrics.
const INLINE_TAG = /<\d+:[^<>]*>/g
const BILINGUAL_SEP = /\s*\^\s*|\s*\/\s*/

function cleanText(text: string) {
  return text.replace(INLINE_TAG, '').trim()
}

function parseWords(text: string, time: number, nextTime?: number): LyricWord[] | undefined {
  const markers = [...text.matchAll(INLINE_TAG)]
  if (!markers.length) return undefined
  const words: LyricWord[] = []
  let start = time
  let cursor = 0
  for (const marker of markers) {
    const match = marker[0].match(WORD_PATTERN)
    if (!match) return undefined
    const end = Number(match[1]) * 60 + Number(match[2])
    if (!Number.isFinite(end) || end < start) return undefined
    const segment = text.slice(cursor, marker.index)
    if (segment.trim()) words.push({ text: segment, start, end })
    else if (words.length) words[words.length - 1].text += segment
    start = end
    cursor = marker.index! + marker[0].length
  }
  const tail = text.slice(cursor)
  if (tail.trim()) {
    // ponytail: missing final-word ends use at most 1s; explicit tags avoid this heuristic.
    const end = Math.min(start + 1, nextTime ?? Infinity)
    if (end < start) return undefined
    words.push({ text: tail, start, end })
  }
  if (!words.length) return undefined
  words[0].text = words[0].text.trimStart()
  words[words.length - 1].text = words[words.length - 1].text.trimEnd()
  return words
}

function parseBilingual(text: string): { primary: string; secondary?: string } {
  const parts = text.split(BILINGUAL_SEP, 2)
  if (parts.length === 2 && parts[1].trim()) {
    return { primary: parts[0].trim(), secondary: parts[1].trim() }
  }
  return { primary: text.trim() }
}

interface TimedLine {
  primary: string
  secondary?: string
  time: number
}

/**
 * mergeSameTime merges records that share a timestamp. Local `.lrc` files often
 * write the original block first and the translation block second, repeating the
 * timestamps; rendering them as two lines makes both scroll independently. The
 * first non-empty record at a timestamp is the original, the second is its
 * translation, and any further record is dropped. Grouping is keyed by the exact
 * timestamp (no tolerance) and keeps the order of first appearance.
 */
function mergeSameTime(lines: TimedLine[]): TimedLine[] {
  const groups = new Map<number, TimedLine>()
  let dropped = 0
  for (const line of lines) {
    const group = groups.get(line.time)
    if (!group) {
      groups.set(line.time, { ...line })
      continue
    }
    if (!line.primary) continue
    if (!group.primary) {
      group.primary = line.primary
      if (group.secondary === undefined && line.secondary) group.secondary = line.secondary
      continue
    }
    if (group.secondary === undefined) {
      group.secondary = line.primary
      continue
    }
    dropped++
  }
  if (dropped > 0) {
    console.debug(`lyrics: dropped ${dropped} duplicate line(s) sharing a timestamp`)
  }
  return [...groups.values()]
}

export function useLyrics(lyrics: Ref<string | undefined>) {
  const decodedLyrics = computed(() => {
    if (!lyrics.value) return undefined
    return decodeHTMLEntities(lyrics.value)
  })

  const syncedLines = computed<LyricLine[]>(() => {
    if (!decodedLyrics.value) return []
    const raw = decodedLyrics.value.split('\n').flatMap(line => {
      const match = line.match(LINE_PATTERN)
      if (!match) return []
      const time = Number(match[1]) * 60 + Number(match[2])
      if (!Number.isFinite(time)) return []
      const { primary, secondary } = parseBilingual(match[3])
      return [{ primary, secondary, time }]
    })
    const lines = mergeSameTime(raw)
    return lines.map((line, index) => {
      const words = parseWords(line.primary, line.time, lines[index + 1]?.time)
      return {
        text: cleanText(line.primary), secondary: line.secondary ? cleanText(line.secondary) : undefined,
        time: line.time, ...(words ? { words } : {}),
      }
    })
  })

  const isSynced = computed(() => syncedLines.value.length > 0)

  const plainLines = computed<PlainLine[]>(() => {
    if (!decodedLyrics.value) return []
    return decodedLyrics.value
      .split('\n')
      .map(l => cleanText(l.replace(LINE_PATTERN, '$3')))
      .filter(l => l)
      .map(l => {
        const { primary, secondary } = parseBilingual(l)
        return { primary, secondary }
      })
  })

  return { isSynced, syncedLines, plainLines }
}
