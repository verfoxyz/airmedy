import {
  Copy,
  Disc,
  Heart,
  ListEnd,
  ListPlus,
  ListStart,
  Play,
  RadioTower,
  RefreshCw,
  Save,
  ThumbsDown,
  User,
  UserX,
} from '@lucide/vue'
import { useI18n } from 'vue-i18n'
import { useRouter } from 'vue-router'
import { usePlayerStore } from '@/stores/player'
import { useCatalogStore } from '@/stores/catalog'
import { useFavoritesStore } from '@/stores/favorites'
import { usePlaylistsStore } from '@/stores/playlists'
import { useLastFmRadioStore } from '@/stores/lastfmRadio'
import { useAddToPlaylistMenu } from './useAddToPlaylistMenu'
import { isPlayableLastFmRow, lastFmRowToCatalogRow } from '@/lib/lastfmTracks'
import type { ContextMenuItem } from './useContextMenu'
import type { TopTrackItemDTO } from '../../bindings/airmedy/internal/infra/wails/models'
import type { TrackDTO } from '../../bindings/airmedy/internal/domain/models'
import * as PlayerService from '../../bindings/airmedy/internal/infra/wails/playerservice'
import * as PlaylistService from '../../bindings/airmedy/internal/infra/wails/playlistservice'
import * as LibraryService from '../../bindings/airmedy/internal/infra/wails/libraryservice'

/** The view owns the list-specific actions; the menu only renders them. */
export interface LastFmRowMenuHooks {
  onRefresh: (item: TopTrackItemDTO) => void
  onDislikeTrack: (item: TopTrackItemDTO) => void
  onDislikeArtist: (item: TopTrackItemDTO) => void
}

/**
 * Common actions for one Last.fm recommendation row.
 *
 * The rows are not library rows yet: play/queue/playlist need a resolved
 * playable track, so those actions prepare it lazily and reuse the same
 * registration path as the play button. Unavailable entries stay in the menu
 * and are disabled, so the order never jumps.
 */
export function useLastFmRowMenu(hooks: LastFmRowMenuHooks) {
  const { t } = useI18n()
  const router = useRouter()
  const playerStore = usePlayerStore()
  const catalogStore = useCatalogStore()
  const favoritesStore = useFavoritesStore()
  const playlistsStore = usePlaylistsStore()
  const lastfmRadioStore = useLastFmRadioStore()
  const { buildCreatePlaylistItems } = useAddToPlaylistMenu()

  /** resolveTrack registers/returns the playable row behind a recommendation. */
  async function resolveTrack(item: TopTrackItemDTO): Promise<TrackDTO | null> {
    try {
      const id = await catalogStore.preparePlayback(lastFmRowToCatalogRow(item))
      if (!id) return null
      const rows = (await LibraryService.GetTracksByIDs([id])) as TrackDTO[]
      return rows?.[0] ?? null
    } catch (error) {
      console.error('Failed to resolve a Last.fm recommendation row', error)
      return null
    }
  }

  function buildMenuItems(item: TopTrackItemDTO): ContextMenuItem[] {
    const playable = isPlayableLastFmRow(item)
    const inLibrary = item.state === 'in_library' || !!item.local_track_id
    const artist = (item.artists?.[0] || item.artist || '').trim()
    const matchingHint = item.matching ? t('lastfm.matching') : undefined
    const items: ContextMenuItem[] = []

    const playItem: ContextMenuItem = {
      label: t('context_menu.play'),
      icon: Play,
      disabled: !playable,
      tooltip: !playable ? matchingHint : undefined,
      action: async () => {
        const track = await resolveTrack(item)
        if (track) await playerStore.playTrackIds([track.id])
      },
    }
    items.push(playItem)

    items.push({
      label: t('context_menu.play_next'),
      icon: ListStart,
      disabled: !playable,
      tooltip: !playable ? matchingHint : undefined,
      action: async () => {
        const track = await resolveTrack(item)
        if (track) await PlayerService.PlayNext(track)
      },
    })

    items.push({
      label: t('context_menu.add_to_queue'),
      icon: ListEnd,
      disabled: !playable,
      tooltip: !playable ? matchingHint : undefined,
      action: async () => {
        const track = await resolveTrack(item)
        if (track) await PlayerService.AppendTracks([track])
      },
    })

    items.push({
      label: t('context_menu.start_radio'),
      icon: RadioTower,
      disabled: !playable,
      tooltip: !playable ? matchingHint : undefined,
      action: () => {
        void lastfmRadioStore.start({
          trackId: item.local_track_id || '',
          provider: item.provider || '',
          externalId: item.external_id || '',
          title: item.title,
          artist,
          album: item.album ?? '',
          duration: item.duration,
        })
      },
    })

    items.push({ separator: true })

    const playlistChildren: ContextMenuItem[] = [
      ...buildCreatePlaylistItems(async () => {
        const track = await resolveTrack(item)
        return track ? [track.id] : []
      }),
      ...(playlistsStore.manualPlaylists.length
        ? playlistsStore.manualPlaylists.map((playlist) => ({
            label: playlist.name,
            action: async () => {
              const track = await resolveTrack(item)
              if (track) await PlaylistService.AddTrackToPlaylist(playlist.id, track.id, '')
            },
          }))
        : [{ label: t('context_menu.no_playlists'), disabled: true }]),
    ]
    items.push({
      label: t('context_menu.add_to_playlist'),
      icon: ListPlus,
      disabled: !playable,
      tooltip: !playable ? matchingHint : undefined,
      children: playlistChildren,
    })

    if (!inLibrary) {
      items.push({
        label: t('lastfm.save_to_library'),
        icon: Save,
        disabled: !playable,
        tooltip: !playable ? matchingHint : undefined,
        action: () => {
          void catalogStore.save(lastFmRowToCatalogRow(item))
        },
      })
    }

    if (inLibrary) {
      const favoriteItem: ContextMenuItem = {
        label: t('context_menu.add_to_favorites'),
        icon: Heart,
        disabled: !item.local_track_id,
        action: async () => {
          if (item.local_track_id) await favoritesStore.toggle(item.local_track_id)
        },
      }
      items.push(favoriteItem)
      if (item.local_track_id) {
        // The row DTO is not loaded yet; correct the label once its favorite
        // flag is known. The action is a toggle either way.
        LibraryService.GetTracksByIDs([item.local_track_id])
          .then((rows) => {
            const track = (rows as TrackDTO[])?.[0]
            if (track && favoritesStore.isFavorite(track)) {
              favoriteItem.label = t('context_menu.remove_from_favorites')
            }
          })
          .catch(() => {})
      }
    }

    items.push({ separator: true })

    const artistItem: ContextMenuItem = {
      label: t('context_menu.go_to_artist'),
      icon: User,
      disabled: !playable,
      action: async () => {
        const track = await resolveTrack(item)
        const target = track?.artists?.find((entry) => !!entry?.id)
        if (target?.id) router.push(`/artists/${target.id}`)
      },
    }
    const albumItem: ContextMenuItem = {
      label: t('context_menu.go_to_album'),
      icon: Disc,
      disabled: !playable,
      action: async () => {
        const track = await resolveTrack(item)
        if (track?.album?.id) router.push(`/albums/${track.album.id}`)
      },
    }
    items.push(artistItem, albumItem)

    items.push({
      label: t('lastfm.copy_row'),
      icon: Copy,
      action: async () => {
        try {
          await navigator.clipboard.writeText(artist ? `${artist} - ${item.title}` : item.title)
        } catch (error) {
          console.debug('Clipboard write failed', error)
        }
      },
    })

    items.push({ separator: true })

    items.push({
      label: t('lastfm.refresh_item'),
      icon: RefreshCw,
      disabled: item.matching === true,
      action: () => hooks.onRefresh(item),
    })

    items.push({
      label: t('lastfm.dislike_track'),
      icon: ThumbsDown,
      action: () => hooks.onDislikeTrack(item),
    })

    items.push({
      label: t('lastfm.dislike_artist'),
      icon: UserX,
      disabled: !artist,
      action: () => hooks.onDislikeArtist(item),
    })

    return items
  }

  return { buildMenuItems, resolveTrack }
}
