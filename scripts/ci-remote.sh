#!/usr/bin/env bash
# ci-remote.sh: run the CI gate on a remote build host over ssh.
#
# Local:  actionlint, gh actions-lock --verify (both need .git).
# Remote: make lint, build + test (root and spec), kit conformance
#         verify-no-leak + verify-stories.
#
# Env:
#   CI_REMOTE        ssh target (required), e.g. user@host
#   CI_REMOTE_DIR    remote work dir (default: ci/<repo>-<branch>)
#   CI_REMOTE_PATH   PATH prefix on the remote (default: /opt/homebrew/bin)
#   CI_KEEP=1        keep the remote dir after the run
#   KIT_BIN          local kit binary copied over when the remote has none
#                    (default: kit on PATH; must match the remote OS/arch)
# Remote commands are built client-side on purpose (dir, PATH and kit
# path are resolved locally), hence SC2029/SC2087; cleanup runs via trap.
# shellcheck disable=SC2029,SC2087,SC2329
set -euo pipefail

: "${CI_REMOTE:?set CI_REMOTE=user@host}"
root=$(git rev-parse --show-toplevel)
cd "$root"
repo=$(basename "$(git rev-parse --path-format=absolute --git-common-dir | sed 's#/\.git$##; s#/$##')")
branch=$(git rev-parse --abbrev-ref HEAD | tr '/' '-')
dir=${CI_REMOTE_DIR:-ci/${repo}-${branch}}
# Remote dir must be a relative path with at least one component under
# the remote home, so cleanup can never target home or root.
case "$dir" in
  ''|/*|.|..|*/..|../*|*/../*) echo "unsafe CI_REMOTE_DIR: '$dir'" >&2; exit 2 ;;
esac
rpath=${CI_REMOTE_PATH:-/opt/homebrew/bin}
fail=0

step() { printf '\n== %s\n' "$*"; }

step "local: actionlint"
if command -v actionlint >/dev/null; then actionlint || fail=1
else echo "actionlint not installed; skipped"; fi

step "local: actions lockfile"
if gh actions-lock --help >/dev/null 2>&1; then gh actions-lock --verify || fail=1
else echo "gh actions-lock not installed; skipped"; fi

step "sync to ${CI_REMOTE}:${dir}"
ssh "$CI_REMOTE" "mkdir -p '$dir'"
rsync -a --delete --exclude .git --exclude bin --exclude dist ./ "${CI_REMOTE}:${dir}/"

cleanup() { [ "${CI_KEEP:-0}" = 1 ] || ssh "$CI_REMOTE" "rm -rf '$dir'"; }
trap cleanup EXIT

kit_remote=kit
if ! ssh "$CI_REMOTE" "PATH='$rpath':\$PATH command -v kit" >/dev/null 2>&1; then
  kit_local=${KIT_BIN:-$(command -v kit || true)}
  if [ -n "$kit_local" ]; then
    scp -q "$kit_local" "${CI_REMOTE}:${dir}/.ci-kit"
    kit_remote=./.ci-kit
  else
    kit_remote=
  fi
fi

step "remote: lint, build, test, conformance"
ssh "$CI_REMOTE" "bash -s" <<EOF || fail=1
set -uo pipefail
export PATH='$rpath':\$PATH GOFLAGS=-buildvcs=false
export GOPRIVATE='hop.top/flo*' GONOSUMDB='hop.top/flo*'
umask 022
cd '$dir'
rc=0
mise trust -q mise.toml >/dev/null 2>&1; mise install -q >/dev/null 2>&1
echo "-- lint";        mise exec -- make lint || rc=1
echo "-- build+test";  { go build ./... && go test ./... -count=1; } || rc=1
echo "-- spec";        { cd spec && go build ./... && go test ./... -count=1; cd ..; } || rc=1
if [ -n '$kit_remote' ]; then
  echo "-- 12fc"
  '$kit_remote' conformance verify-no-leak --paths . || rc=1
  '$kit_remote' conformance verify-stories || rc=1
else
  echo "-- 12fc: no kit binary; skipped"; rc=1
fi
exit \$rc
EOF

step "result"
if [ "$fail" = 0 ]; then echo "ci-remote: PASS"; else echo "ci-remote: FAIL"; fi
exit "$fail"
