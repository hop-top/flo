# Shared by gh-deps, gh-advisories and gh-releases. Sourced, not run.
#
# gh api exits non-zero on 304 and on every 4xx, and aps may redact its
# stdout, so each call writes its body to a file and the HTTP status is read
# back from the error line. The wrappers turn what they find into the nodes'
# documented exit codes: 0 ok, 75 provider rate limit or transient server
# error (retry later), 1 anything else.

EX_RETRY=75

# gh_get FILE ARGS...: gh api ARGS into FILE. Sets GH_STATUS (200 on success)
# and GH_ERR (the error text file; empty on success).
gh_get() {
  _out=$1
  shift
  GH_ERR="$_out.err"
  if gh api "$@" < /dev/null > "$_out" 2> "$GH_ERR"; then
    GH_STATUS=200
    return 0
  fi
  GH_STATUS=$(sed -n 's/.*HTTP \([0-9][0-9][0-9]\).*/\1/p' "$GH_ERR" | head -n 1)
  GH_STATUS=${GH_STATUS:-0}
  return 1
}

# gh_retryable: true when the last failure is a rate limit or a server error.
gh_retryable() {
  case $GH_STATUS in
    429 | 5??) return 0 ;;
    403) grep -qi 'rate limit' "$GH_ERR" "${1:-/dev/null}" ;;
    *) return 1 ;;
  esac
}

# gh_die FILE: report the last failure on stderr and exit as documented.
gh_die() {
  cat "$GH_ERR" >&2
  if gh_retryable "${1:-}"; then exit "$EX_RETRY"; fi
  exit 1
}

need_jq() {
  command -v jq > /dev/null 2>&1 || { echo "$0: jq is required" >&2; exit 1; }
}

# now_utc: the clock; FLO_WATCH_NOW overrides it for tests.
now_utc() {
  if [ -n "${FLO_WATCH_NOW:-}" ]; then printf '%s\n' "$FLO_WATCH_NOW"; else date -u +%Y-%m-%dT%H:%M:%SZ; fi
}
