#!/usr/bin/env bash
# builder-eval.sh: compare local ollama models on the chat builder's drafting
# tasks, on a remote host that runs ollama (docs/builder-model-eval.md).
#
# Env:
#   EVAL_REMOTE      ssh target (required), e.g. user@host
#   EVAL_MODELS      comma-separated ollama models (required), run serially
#   EVAL_RUNS        times each task is asked of each model (default 1)
#   EVAL_TIMEOUT     per-draft limit, e.g. 3m (default 10m)
#   EVAL_TASKS       comma-separated task ids (default: all)
#   EVAL_REMOTE_DIR  scratch dir under the remote home (default: work/flo-builder-eval)
#   EVAL_REMOTE_PATH PATH prefix on the remote (default: /opt/homebrew/bin)
#   EVAL_OUT         local file for the results (default: builder-eval.json); .md beside it
#   EVAL_KEEP=1      keep the scratch dir
# The remote dir is removed afterwards. The models must already be pulled.
# shellcheck disable=SC2029
set -euo pipefail

: "${EVAL_REMOTE:?set EVAL_REMOTE=user@host}"
: "${EVAL_MODELS:?set EVAL_MODELS=model1,model2}"
root=$(git rev-parse --show-toplevel)
cd "$root"
dir=${EVAL_REMOTE_DIR:-work/flo-builder-eval}
case "$dir" in
  ''|/*|.|..|*/..|../*|*/../*) echo "unsafe EVAL_REMOTE_DIR: '$dir'" >&2; exit 2 ;;
esac
rpath=${EVAL_REMOTE_PATH:-/opt/homebrew/bin}
out=${EVAL_OUT:-builder-eval.json}

ssh "$EVAL_REMOTE" "mkdir -p '$dir'"
cleanup() { [ "${EVAL_KEEP:-0}" = 1 ] || ssh "$EVAL_REMOTE" "rm -rf '$dir'"; }
trap cleanup EXIT
rsync -a --delete --exclude .git --exclude bin --exclude dist ./ "${EVAL_REMOTE}:${dir}/"

ssh "$EVAL_REMOTE" "cd '$dir' && PATH='$rpath':\$PATH \
  FLO_BUILDER_EVAL='$EVAL_MODELS' FLO_BUILDER_EVAL_RUNS='${EVAL_RUNS:-1}' \
  FLO_BUILDER_EVAL_TASKS='${EVAL_TASKS:-}' FLO_BUILDER_EVAL_TIMEOUT='${EVAL_TIMEOUT:-}' FLO_BUILDER_EVAL_OUT=\$PWD/.eval-out.json \
  go test -buildvcs=false ./internal/engine/serve -run TestBuilderModelEval -count=1 -timeout 0 -v"
scp -q "${EVAL_REMOTE}:${dir}/.eval-out.json" "$out"
scp -q "${EVAL_REMOTE}:${dir}/.eval-out.json.md" "${out%.json}.md"
echo "results: $out ${out%.json}.md"
