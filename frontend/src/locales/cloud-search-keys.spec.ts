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

type Messages = Record<string, string>

const enMessages = (en as unknown as { cloud_search: Messages }).cloud_search
const requiredKeys = Object.keys(enMessages)

describe('cloud search locale coverage', () => {
  // Every key the search page and the row state chips render. A missing one shows
  // the raw dot path; an extra one is dead text nobody can translate.
  it('declares every key the cloud branch renders', () => {
    expect(requiredKeys.sort()).toEqual([
      'add_to_playlist', 'all_sources', 'all_sources_failed', 'blocked_no_resolver',
      'download_anyway', 'downloading', 'empty_description', 'empty_title', 'failed',
      'failure_source_disabled', 'failure_timeout', 'failure_upstream',
      'favorite', 'hint', 'in_library', 'load_more', 'loading_more',
      'mode_cloud', 'mode_local', 'no_playlists', 'no_sources_description',
      'no_sources_title', 'not_downloadable', 'results', 'save_failed', 'saved',
      'source_unavailable_description', 'source_unavailable_title',
    ].sort())
  })

  for (const [name, messages] of Object.entries(locales)) {
    it(`${name} translates the cloud branch`, () => {
      const section = (messages as unknown as { cloud_search: Messages }).cloud_search
      expect(section).toBeDefined()
      expect(Object.keys(section).sort()).toEqual(requiredKeys.sort())
      for (const key of requiredKeys) {
        expect(section[key], `${name}.cloud_search.${key}`).toBeTruthy()
      }
      // The failure chips are interpolated with the source label; losing the
      // placeholder would show which source failed only in the surrounding text.
      for (const key of ['failure_timeout', 'failure_upstream', 'failure_source_disabled']) {
        expect(section[key], `${name}.cloud_search.${key}`).toContain('{source}')
      }
    })
  }
})
