import { describe, expect, it } from 'vitest'
import en from './en.json'
import de from './de.json'
import es from './es.json'
import fr from './fr.json'
import itLocale from './it.json'
import ja from './ja.json'
import ko from './ko.json'
import pt from './pt.json'
import ru from './ru.json'
import th from './th.json'
import vi from './vi.json'
import zh from './zh.json'

const locales = { en, de, es, fr, it: itLocale, ja, ko, pt, ru, th, vi, zh }

type Tree = { [key: string]: string | Tree }

function flatten(tree: Tree, prefix = ''): string[] {
  return Object.entries(tree).flatMap(([key, value]) =>
    typeof value === 'object' && value !== null ? flatten(value, `${prefix}${key}.`) : [`${prefix}${key}`],
  )
}

const downloadKeys = Object.keys(en.download)
const taskKeys = Object.keys(en.tasks)

describe('locale coverage', () => {
  for (const [name, messages] of Object.entries(locales)) {
    it(`${name} has every english key`, () => {
      const missing = flatten(en as Tree).filter(key => !flatten(messages as Tree).includes(key))
      expect(missing).toEqual([])
    })
  }

  it('keeps the download queue keys in all 12 locales', () => {
    expect(downloadKeys).toEqual(expect.arrayContaining([
      'queue_title', 'queue_empty', 'queue_empty_hint', 'cancel',
      'retry', 'queued_hint', 'canceled', 'unknown_track', 'attempt', 'waiting',
    ]))
    for (const [name, messages] of Object.entries(locales)) {
      for (const key of downloadKeys) {
        expect((messages.download as Record<string, string>)[key], `${name}.download.${key}`).toBeTruthy()
      }
    }
  })

  it('keeps the tasks page keys in all 12 locales', () => {
    expect(taskKeys).toEqual(expect.arrayContaining([
      'title', 'tab_all', 'tab_active', 'tab_done', 'tab_failed', 'kind_request', 'kind_cache',
      'phase_resolving', 'phase_downloading', 'phase_promoting', 'phase_verifying',
      'empty_all', 'empty_active', 'empty_done', 'empty_failed', 'clear', 'clear_title',
      'clear_message', 'clear_confirm', 'clear_cancel', 'clear_done',
    ]))
    for (const [name, messages] of Object.entries(locales)) {
      for (const key of taskKeys) {
        expect((messages.tasks as Record<string, string>)[key], `${name}.tasks.${key}`).toBeTruthy()
      }
    }
  })

  it('uses the same interpolation placeholders in every locale', () => {
    const placeholders = (value: string) => [...value.matchAll(/\{(\w+)\}/g)].map(match => match[1]).sort()
    for (const key of downloadKeys) {
      const expected = placeholders((en.download as Record<string, string>)[key])
      for (const [name, messages] of Object.entries(locales)) {
        expect(placeholders((messages.download as Record<string, string>)[key]), `${name}.download.${key}`)
          .toEqual(expected)
      }
    }
  })
})
