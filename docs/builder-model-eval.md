# Builder model comparison

Compares local ollama models on the chat builder's drafting (`flo flow draft
create`): the same fixed tasks, asked of each model through the engine's own
builder path (prompt, node catalog, validation, three retries, dry run).

Off by default. `go test ./...` and `make ci-remote` skip it; only
`TestBuilderEvalTasksAreSound` runs there, with no model, to keep the tasks
honest (each reference solution passes its check, the unchanged flow does not).

## Tasks

Eleven instructions over synthetic flows: `testdata/builder-eval/` (newsletter,
digest), `testdata/builder/greeter` and `testdata/flows/dependency-watch`. They
cover add a step, change a schedule, add a `when`, rename an output, repoint a
reference, tighten privacy, add vars, add a `flo/ai.classify@1` step with
labels, add a human approval, and split a step. Tasks and checks are in
`internal/engine/serve/builder_eval_tasks_test.go`.

A task passes when the proposal validates and its scripted check, an assertion
on the parsed document, holds. The three tasks on `dependency-watch` are also
replayed on a recording of the base flow and must match it.

## Metrics

Per model: valid on first try, mean attempts (a failed draft counts four),
drafts that failed after three retries, scripted-check pass rate, dry-run match,
latency per draft, and tokens as ollama reports them (prompt tokens a cache
serves are not counted).

Sampling is pinned for the comparison only: temperature 0, a fixed seed, a
12288 token context, at most 6144 generated tokens. Latency excludes loading
the model.

## Run

On a host with ollama and the models already pulled (nothing is pulled or
removed). Models run one at a time and each is unloaded after its tasks.

```sh
FLO_BUILDER_EVAL=qwen3:4b-instruct-2507-q4_K_M,gemma4:e4b \
FLO_BUILDER_EVAL_RUNS=2 \
FLO_BUILDER_EVAL_OUT=/tmp/builder-eval.json \
  go test ./internal/engine/serve -run TestBuilderModelEval -count=1 -timeout 0 -v
```

| variable | meaning |
|---|---|
| `FLO_BUILDER_EVAL` | comma-separated ollama models; unset skips the test |
| `FLO_BUILDER_EVAL_RUNS` | times each task is asked of each model (default 1) |
| `FLO_BUILDER_EVAL_TASKS` | comma-separated task ids (default all) |
| `FLO_BUILDER_EVAL_TIMEOUT` | per-draft limit (default 10m) |
| `FLO_BUILDER_EVAL_OUT` | write results as JSON, and the tables as `<file>.md` |

The tables print at the end. The JSON holds numbers and check verdicts only,
no prompts or answers.

To run on another machine, `scripts/builder-eval.sh` syncs the tree to a scratch
directory over ssh, runs the test there against its ollama, copies the results
back and removes the directory (`EVAL_REMOTE`, `EVAL_MODELS`, `EVAL_RUNS`,
`EVAL_TASKS`, `EVAL_OUT`; see the script header).

Rerun after a change to the builder's prompt, the node catalog or the
validation rules, and before changing the default model.
