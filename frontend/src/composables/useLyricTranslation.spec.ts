import { describe, expect, it } from 'vitest'
import { ref } from 'vue'
import { useLyricTranslation } from './useLyricTranslation'

describe('useLyricTranslation', () => {
  it('aligns a timed translation to the original timestamps', () => {
    const aligned = useLyricTranslation(
      ref('[00:00.10]Hello\n[00:05.05]Bye'),
      ref(2),
      ref([0, 5]),
    )
    expect(aligned.value).toEqual(['Hello', 'Bye'])
  })

  it('aligns a translation without timestamps by line index', () => {
    const aligned = useLyricTranslation(
      ref('Hello\n\nBye'),
      ref(3),
      ref([0, 5, 10]),
    )
    expect(aligned.value).toEqual(['Hello', undefined, 'Bye'])
  })

  it('leaves unmatched timestamps empty instead of shifting lines', () => {
    const aligned = useLyricTranslation(
      ref('[00:00]Hello'),
      ref(2),
      ref([0, 5]),
    )
    expect(aligned.value).toEqual(['Hello', undefined])
  })

  it('returns no lines without a translation', () => {
    expect(useLyricTranslation(ref(undefined), ref(3), ref([0, 1, 2])).value).toEqual([])
    expect(useLyricTranslation(ref(''), ref(3), ref([0, 1, 2])).value).toEqual([])
  })

  it('decodes entities and strips inline timing tags', () => {
    const aligned = useLyricTranslation(
      ref('[00:00]<00:01>Tom &amp; Jerry<00:02>'),
      ref(1),
      ref([0]),
    )
    expect(aligned.value).toEqual(['Tom & Jerry'])
  })
})
