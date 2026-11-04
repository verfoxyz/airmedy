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

type PluginMessages = Record<string, string>
type LxMessages = Record<string, string>

const settings = en.settings as unknown as {
  plugins: PluginMessages & { lx: LxMessages }
  categories: Record<string, string>
}

const pluginKeys = Object.keys(settings.plugins).filter(key => key !== 'lx')

describe('plugin settings locale coverage', () => {
  // The settings page renders every one of these; a missing key would show the
  // raw dot path to the user.
  const required = [
    'title', 'description', 'refresh', 'loading',
    'empty_title', 'empty_desc',
    'status_running', 'status_idle', 'status_failed', 'status_invalid',
    'show_error', 'hide_error',
    'network_label', 'network_hosts', 'network_any', 'storage_label',
  ]

  it('declares every key the plugins section renders', () => {
    expect(pluginKeys.sort()).toEqual([...required].sort())
  })

  // The host/sub-plugin model is gone, so its wording must be gone too: a
  // leftover key would be dead text nobody can translate correctly, and a
  // leftover usage would show a raw dot path or a stale status.
  it('has no host_disabled key', () => {
    expect(pluginKeys).not.toContain('host_disabled')
  })

  // The Lx source list is a sub-page of the plugins section, so it has its own
  // key set. A missing key would render the raw dot path in the UI.
  const lxRequired = [
    'title', 'description', 'add', 'back',
    'empty_title', 'empty_desc',
    'enabled', 'disabled', 'remove', 'remove_title', 'remove_confirm',
    'providers', 'no_providers', 'plugin_off', 'failed',
    'move_up', 'move_down', 'order_hint',
  ]

  it('declares every key the lx source list renders', () => {
    expect(Object.keys(settings.plugins.lx).sort()).toEqual([...lxRequired].sort())
  })

  for (const [name, messages] of Object.entries(locales)) {
    it(`${name} translates the plugins section`, () => {
      const section = (messages as unknown as { settings: { plugins: PluginMessages } }).settings.plugins
      expect(Object.keys(section).sort()).toEqual([...required, 'lx'].sort())
      for (const key of required) {
        expect(section[key], `${name}.settings.plugins.${key}`).toBeTruthy()
      }
      // The network tooltip is interpolated with the declared hosts; losing the
      // placeholder would show the wording without ever naming a host.
      expect(section.network_hosts, `${name}.settings.plugins.network_hosts`).toContain('{hosts}')
    })

    it(`${name} translates the lx source list`, () => {
      const section = (messages as unknown as { settings: { plugins: { lx: LxMessages } } })
        .settings.plugins.lx
      expect(Object.keys(section).sort()).toEqual([...lxRequired].sort())
      for (const key of lxRequired) {
        expect(section[key], `${name}.settings.plugins.lx.${key}`).toBeTruthy()
      }
      // The remove confirmation is interpolated with the source name; losing the
      // placeholder would silently drop it from the dialog.
      expect(section.remove_confirm, `${name}.settings.plugins.lx.remove_confirm`).toContain('{name}')
    })

    it(`${name} names the plugins settings category`, () => {
      const categories = (messages as unknown as { settings: { categories: Record<string, string> } })
        .settings.categories
      expect(categories.plugins, `${name}.settings.categories.plugins`).toBeTruthy()
    })
  }
})
