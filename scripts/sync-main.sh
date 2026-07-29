#!/usr/bin/env bash
set -euo pipefail

repository_root="$(git rev-parse --show-toplevel)"
cd "${repository_root}"

if [[ "$(git branch --show-current)" != "main" ]]; then
  echo "sync-main must run from a main worktree" >&2
  exit 1
fi

if [[ -n "$(git status --porcelain)" ]]; then
  echo "refusing to update a dirty main worktree; commit or move the changes first" >&2
  exit 1
fi

git fetch origin --prune

if ! git merge-base --is-ancestor HEAD origin/main; then
  echo "local main has commits that are not on origin/main; resolve them outside this mirror" >&2
  exit 1
fi

git merge --ff-only origin/main
git status --short --branch
