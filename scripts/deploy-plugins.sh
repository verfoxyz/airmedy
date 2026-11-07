#!/usr/bin/env bash
# Copy plugin sources from their own repositories into the app data directory.
#
# The app discovers plugins with filepath.WalkDir, which does not follow
# junctions or symlinks, so <data dir>/plugins/<id> has to be a real directory
# holding real files. This script keeps that copy in sync.
#
# Override the source root with AIRMEDY_PLUGIN_ROOT and the data directory with
# AIRMEDY_DATA_DIR when the layout differs.
set -euo pipefail

PLUGIN_ROOT="${AIRMEDY_PLUGIN_ROOT:-/d/code/music}"
DATA_DIR="${AIRMEDY_DATA_DIR:-${LOCALAPPDATA:-$HOME}/airmedy}"
DATA_DIR="$(cygpath -u "$DATA_DIR" 2>/dev/null || printf '%s' "$DATA_DIR")"

for id in wy lx; do
  src="$PLUGIN_ROOT/$id"
  dst="$DATA_DIR/plugins/$id"

  if [ ! -d "$src" ]; then
    echo "deploy-plugins: missing source $src" >&2
    exit 1
  fi

  mkdir -p "$dst"
  # tar keeps the copy free of .git and local backups without needing rsync.
  ( cd "$src" && tar --exclude=.git --exclude=node_modules --exclude='*.bak-*' -cf - . ) \
    | ( cd "$dst" && tar -xf - )
  echo "deploy-plugins: $id -> $dst"
done

echo "deploy-plugins: restart Airmedy so the plugin host reloads the manifests"
