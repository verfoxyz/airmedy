/**
 * Column template shared by the Last.fm table header and every row.
 *
 * Title and artist are both flexible (1.2fr / 1fr) so the pair uses the space
 * between the fixed #/status/actions columns. Below the container's @md size
 * the artist column disappears and its name moves under the title, so the row
 * keeps 4 columns.
 */
export const LASTFM_ROW_GRID =
  'grid-cols-[48px_minmax(0,1fr)_112px_80px] @md:grid-cols-[48px_minmax(180px,1.2fr)_minmax(140px,1fr)_120px_96px]'
