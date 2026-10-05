#!/bin/sh
# Rebuild bench/base/alibuild: the tracked files of ~/src/alibuild at the
# revision in base/alibuild.rev, as a fresh git repo the tasks diff against.
#
#   ./make-base.sh [path/to/alibuild]
set -eu
cd "$(dirname "$0")"
src=${1:-$HOME/src/alibuild}
rev=$(cat base/alibuild.rev)
dest=base/alibuild

[ -e "$dest" ] && { echo "$dest exists; remove it first" >&2; exit 1; }
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# Tracked files at that revision, via a throwaway clone: the working copy may
# have moved on or carry local changes. A local Sapling repo clones through its
# git store, and only by full hash.
node=$(sl log -R "$src" -r "$rev" -T '{node}')
sl clone -q --git "$src/.sl/store/git" "$tmp/src"
sl -R "$tmp/src" pull -q -r "$node" "$src/.sl/store/git"
sl -R "$tmp/src" goto -q "$node"
mkdir -p "$dest"
(cd "$tmp/src" && sl files .) | rsync -a --files-from=- "$tmp/src/" "$dest/"
rm -rf "$dest/.claude"   # project skills would reach Claude Code but not pi

cd "$dest"
git init -q
git config core.fsmonitor false   # its socket cannot be copied into workspaces
git add -A
git -c user.name=bench -c user.email=bench@localhost commit -qm base
echo "rebuilt $dest at $rev"
