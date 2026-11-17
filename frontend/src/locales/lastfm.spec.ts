import { describe, expect, it } from 'vitest'
import de from './de.json'
import en from './en.json'
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

/**
 * The Last.fm page ships in every locale in the same change.
 *
 * vue-i18n falls back silently, so a missing key renders the raw path instead
 * of failing a build; this test is the guard that keeps the twelve files in
 * lockstep.
 */
const locales = { de, en, es, fr, it: itLocale, ja, ko, pt, ru, th, vi, zh }

const EXPECTED_KEYS = [
  'auto_refresh',
  'column_actions',
  'column_artist',
  'column_status',
  'column_title',
  'combined',
  'copy_row',
  'discover',
  'dislike_artist',
  'dislike_track',
  'empty_description',
  'empty_title',
  'in_library',
  'list_empty',
  'listeners',
  'load_failed',
  'load_more',
  'loved',
  'matched_from',
  'matching',
  'more_actions',
  'no_match',
  'no_more',
  'no_source',
  'not_connected_description',
  'not_connected_title',
  'open_settings',
  'partial',
  'play',
  'play_all',
  'play_matching',
  'play_skipped',
  'play_unavailable',
  'plays_count',
  'recent',
  'refresh',
  'refresh_all',
  'refresh_item',
  'refresh_list',
  'save',
  'save_to_library',
  'search_artist',
  'shuffle',
  'similar_artists',
  'similar_empty',
  'similar_tracks',
  'start_radio',
  'subtitle',
  'subtitle_account',
  'title',
  'top',
  'week_next',
  'week_prev',
  'weekly_echo',
].sort()

describe('Last.fm locale keys', () => {
  it('every locale has the complete key set', () => {
    for (const [name, messages] of Object.entries(locales)) {
      expect(Object.keys(messages.lastfm).sort(), name).toEqual(EXPECTED_KEYS)
      expect((messages as { sidebar: { lastfm?: string } }).sidebar.lastfm, name).toBe('Last.fm')
    }
  })
})
