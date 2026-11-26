# GSEA Evaluation Suite

`gsea eval` evaluates GSEA as a black-box coding Agent. The task and
result protocols do not assume a programming language or test framework.

Runtime evaluations need a matching `gsea`/`ate` pair and an isolated ATE
store; a fresh `--data-dir` alone does not isolate shared ATE history. Build
from the sibling-module workspace described in the [installation guide](../README.md#安装与运行),
and set `GSEA_ATE_BINARY` / `GSEA_ATE_DIRECTORY` for the evaluation environment.
The historical Harbor adapter below has not been migrated from the old
SQLite export path and is not a working current-runtime recipe.

## Task protocol

Each line is one `gsea.eval.task.v1` JSON object:

```json
{
  "schema": "gsea.eval.task.v1",
  "id": "project__issue-123",
  "instance_id": "project__issue-123",
  "suite": "custom",
  "kind": "patch",
  "language": "go",
  "workspace_mode": "git_worktree",
  "repository": "/absolute/path/to/local/repository",
  "base_commit": "0123456789abcdef",
  "problem_statement": "Fix the issue without modifying tests.",
  "agent_timeout_seconds": 1800,
  "checks": [
    {
      "name": "fail-to-pass",
      "phase": "target",
      "backend": "sandbox",
      "command": "go test ./internal/example -run TestIssue123",
      "timeout_seconds": 300
    },
    {
      "name": "pass-to-pass",
      "phase": "regression",
      "backend": "docker",
      "image": "example/project-eval:locked-digest",
      "command": "go test ./...",
      "timeout_seconds": 900
    }
  ],
  "tags": ["bugfix", "multi-file"]
}
```

`evals/examples/tasks.jsonl` contains a one-line template; replace its
`repository` placeholder before running it.

Manifests are decoded strictly: one JSON object per non-blank line, unknown
fields rejected, task IDs unique and matching `^[A-Za-z0-9][A-Za-z0-9._/-]{0,255}$`,
and check names unique within a task. `schema`, `id`, `suite`, `kind`,
`repository`, `problem_statement` and `checks` are required. Optional fields and
defaults:

- `instance_id` defaults to `id` in the result and in SWE-bench predictions;
- `metadata` is a string map copied verbatim into the result;
- `agent_timeout_seconds` is 0-7200 and 0/omitted means 1800;
- per check, `phase` defaults to `general`, `backend` to `sandbox`,
  `timeout_seconds` is 0-7200 with 0/omitted meaning 600, `image` is required
  for `docker`, and `optional` defaults to false.

`workspace_mode` controls how the disposable Agent workspace is created:

- `git_worktree` (default) requires a local Git `repository` and `base_commit`,
  then creates a detached worktree at that exact commit.
- `directory_copy` accepts any local fixture directory in `repository` and
  must omit `base_commit`. The runner copies the fixture, excludes its root
  `.git` directory, creates a private Git baseline in the copy, and never lets
  the Agent mutate the source fixture. This mode is intended for function,
  terminal, tool-use and security scenarios that are not naturally commits in
  an existing repository.

The runner does not clone repositories, install packages or pull images
implicitly. Benchmark setup/download remains an explicit host operation.
The evaluation root must be outside every source fixture/repository; the
runner rejects a task before creating artifacts when those paths overlap.

Checks are evaluator-only. The Agent adapter receives only the task ID, suite,
kind, language and `problem_statement`, and only `problem_statement` becomes the
model prompt, so hidden tests and verifier commands cannot leak into the prompt
through the core API.

Supported phases are `target`, `regression`, `safety`, and `general`. Supported
backends are:

- `sandbox` (default): the host OS Seatbelt/bubblewrap backend, with network
  disabled and writes restricted to the task worktree and temporary directory.
- `docker`: `docker run --rm --pull never --network none --cap-drop ALL
--security-opt no-new-privileges --pids-limit 512`, with the task worktree
  mounted at `/workspace` and the command run by `/bin/sh -lc`. Images must be
  fetched and pinned before the evaluation.

Checks run sequentially after the Agent finishes. A check passes only when the
command exits 0 within its timeout and its log is written; combined
stdout/stderr is capped at 2 MiB per check.

A task must contain at least one non-optional check. It resolves only when the
Agent exits normally, patch capture succeeds, the verifier itself succeeds and
every non-optional check passes. Optional checks are reported but do not change
the resolved bit. A failed or timed-out Agent can still leave a useful patch and
passing check evidence in the result, but that attempt is not counted as
resolved.

## Running tasks

Inspect the local runtime before launching a suite. This command is read-only:
it does not install packages, pull images or contact a model.

```bash
gsea eval doctor
```

The JSON report distinguishes command availability, a reachable Docker daemon,
an actually executable OS sandbox, and the optional Python modules used by
official/external benchmark harnesses. A command on `PATH` is not treated as
proof that its daemon or sandbox backend can run. The probed modules are
`swebench`, `inspect_ai`, `inspect_evals`, `agentdojo`, `aider` and
`terminal_bench`, imported with the `python3` found on `PATH`; activate the
harness virtualenv first if you want it inspected. Harbor is not probed.

Chat/model flags follow `--` and are passed to every isolated Agent run. The
evaluator runs `chat` in-process and owns `--evaluation-mode`, `--workspace`,
`--data-dir` and `--session`; passing any of them after `--` is rejected.
Evaluation mode auto-approves only `workspace_write`, `workspace_edit`,
`shell_exec` and snapshot `merge_apply`; other approval-gated calls are denied.

```bash
gsea eval validate --tasks tasks.jsonl

gsea eval run \
  --tasks tasks.jsonl \
  --root /tmp/gsea-eval/run-20260904 \
  --run-id run-20260904 \
  --model-label openai/gpt-5.6-sol \
  --parallel 4 \
  -- \
  --provider openai \
  --wire-api responses \
  --model gpt-5.6-sol \
  --reasoning-effort medium
```

`--parallel` is bounded to 1-64 and defaults to 1. Results are serialized by a
single writer, in completion order, even when Agent runs execute concurrently.
Choose concurrency according to provider rate limits, local CPU/memory and the
number of verifier containers the host can sustain.

Other `eval run` flags: `--output` (default
`<root>/results-<run-id>.jsonl`), `--root` (default: a new OS temporary
directory), `--run-id` (default: generated `eval-...`), `--model-label`
(default: the `--model` chat argument, else the configured model name),
`--task` (run one task ID), `--keep-worktrees`, and `--fail-on-unresolved`
(non-zero exit when any attempt is unresolved). Progress lines go to stderr;
stdout receives one `gsea.eval.summary.v1` JSON object with `results_path`,
`evaluation_root` and `summary`. `eval validate` prints a
`gsea.eval.validation.v1` object.

Use a new run ID/root for every independent attempt. Run the same tasks several
times to calculate resolved@1, pass@k, variance, token cost and latency rather
than treating one stochastic run as definitive.

Aggregate repeated attempts with:

```bash
gsea eval summarize \
  --results /tmp/gsea-eval/run-1/results-run-1.jsonl \
  --results /tmp/gsea-eval/run-2/results-run-2.jsonl \
  --results /tmp/gsea-eval/run-3/results-run-3.jsonl
```

The `gsea.eval.summary.v1` report distinguishes per-attempt resolution rate
from observed pass@k (whether each unique task succeeded in at least one
supplied attempt). It totals Agent failures, timeouts, check failures
(including failed optional checks), tokens, cost and wall-clock duration over
all attempts, and gives attempt/resolved counts by suite and by language.

## Benchmark adapters

There are two adapter shapes, both of which keep the model/tool loop in GSEA:

- repository patch suites use `gsea eval` and its isolated worktree,
  patch, verifier and result protocol;
- harness-owned environments use a thin Python adapter. Inspect maps samples
  to one-shot GSEA sessions, Terminal-Bench copies the Linux GSEA executable
  into its disposable task container, and AgentDojo publishes its official
  in-memory tools over a private Unix socket. Invocation-scoped tools are still
  registered in GSEA and execute through Eino, Policy, approval and the durable
  Tool Pipeline.

Pinned optional dependencies are in `evals/requirements/`. AgentDojo is kept
in a separate environment because its provider dependency constraints need not
be imposed on the other harnesses.

```bash
python3 -m venv /tmp/gsea-harnesses
/tmp/gsea-harnesses/bin/pip install -r evals/requirements/harnesses.txt

python3 -m venv /tmp/gsea-agentdojo
/tmp/gsea-agentdojo/bin/pip install -r evals/requirements/agentdojo.txt
```

The Inspect and AgentDojo adapters (`evals/adapters/gsea_client.py`) use
`GSEA_BINARY`, `GSEA_BENCH_DATA_ROOT`, `GSEA_PROVIDER`, `GSEA_WIRE_API`,
`GSEA_MODEL`, `GSEA_BASE_URL`, `GSEA_API_KEY_ENV`, `GSEA_REASONING_EFFORT`,
`GSEA_REQUEST_TIMEOUT`, `GSEA_PROCESS_TIMEOUT_SECONDS`, `GSEA_MAX_ITERATIONS`,
`GSEA_ATTEMPTS`, and the normal named API-key environment variable. Each sample
runs `gsea chat --prompt-file - --headless-format json`
in a fresh `--data-dir`/`--session` below the data root and expects exactly one
`gsea.chat.result.v1` stdout record. The Harbor adapter takes the equivalent
settings as `--agent-kwarg` values instead. No adapter puts the key value in a
command argument or manifest.

### Inspect AI

`mockllm/model` only satisfies Inspect's outer model slot; the registered
solver launches the real GSEA executable and places its answer in
`TaskState.output`. Inspect samples run without `--evaluation-mode`, so the
headless approver denies write, edit and shell tools.

```bash
go build -o /tmp/ate github.com/vkviyu/agent-temporal-engine/cmd/ate
go build -o /tmp/gsea ./cmd/gsea

PYTHONPATH="$PWD" \
GSEA_BINARY=/tmp/gsea \
GSEA_ATE_BINARY=/tmp/ate \
GSEA_ATE_DIRECTORY=/tmp/gsea-inspect-ate \
GSEA_BENCH_DATA_ROOT=/tmp/gsea-inspect-data \
/tmp/gsea-harnesses/bin/inspect eval \
  evals/inspect_local_smoke.py@local_biology \
  --model mockllm/model --limit 3 --max-samples 1 \
  --log-dir /tmp/gsea-inspect-logs --display plain
```

### Official scorecards

`eval summarize` describes GSEA's native task protocol. It is useful during
development, but it is not a substitute for a benchmark's official metric.
Use `eval score` only on outputs produced by the benchmark owner's harness; it
prints an indented `gsea.eval.scorecard.v1` JSON object:

```bash
gsea eval score \
  --benchmark terminal-bench-2.1 \
  --input "$PWD/.local-agent/evals/harbor-jobs/gsea-tb21-official" \
  --reasoning-effort high

gsea eval score \
  --benchmark swe-bench-verified \
  --input logs/run_evaluation/gsea-swebench/results.json
```

For Terminal-Bench 2.1 the scorer uses verifier reward `> 0` as success,
counts missing/error rewards as failed trials, and reproduces the official
accuracy, per-task standard error and unbiased pass@2..5 formulas. A
leaderboard-comparable run requires all 89 tasks and at least five independent
attempts per task (445 trials). For SWE-bench Verified, the denominator is all
500 instances; missing predictions, empty patches and execution errors remain
failures. `protocol_compliance.comparable` reports metric/coverage parity.
For resumable or disk-bounded Terminal-Bench runs, repeat `--input` for every
Harbor job directory; the scorer validates each job's `config.json` (exactly one
pinned dataset), `lock.json` (task digests/sources, no timeout multipliers or
overrides, no resource overrides, verifier enabled), agent/model/reasoning
identity, and an `agent/trajectory.json` for every successful trial, before
merging unique trial IDs. Duplicate trial IDs and a standalone `result.json`
are reported as protocol violations. SWE-bench accepts exactly one official
aggregate `results.json`. `--model`, `--reasoning-effort` and
`--harness-version` assert the recorded value, or fill it only when the input
does not record one.
`official_audit_passed` remains false for local runs because only the benchmark
owner can certify submitted artifacts and reward-hacking review.
After that review, pass its `disqualified_trials` JSON (or a plain JSON array
of trial IDs) to `--disqualifications`, which is valid only for
`terminal-bench-2.1`; matched verifier successes are then forced to zero, as
on the public Terminal-Bench leaderboard. Unknown trial IDs are rejected to
prevent accidentally applying an audit from another job.

```bash
gsea eval score \
  --benchmark terminal-bench-2.1 \
  --input "$PWD/.local-agent/evals/harbor-jobs/gsea-tb21-batch-01" \
  --input "$PWD/.local-agent/evals/harbor-jobs/gsea-tb21-batch-02" \
  --disqualifications /path/to/official-reward-audit.json
```

### SWE-bench Verified

Materialize an official instance at its exact base commit, execute GSEA, export
the prediction, then use the official Docker scorer as the authority:

```bash
/tmp/gsea-harnesses/bin/python evals/adapters/prepare_swebench.py \
  --instance-id django__django-11099 \
  --output-root /tmp/gsea-swebench \
  --tasks /tmp/gsea-swebench/tasks.jsonl

/tmp/gsea eval run \
  --tasks /tmp/gsea-swebench/tasks.jsonl \
  --root /tmp/gsea-swebench/run \
  --output /tmp/gsea-swebench/run/results.jsonl \
  --run-id swebench-run --model-label gsea/gpt-5.6-sol -- \
  --provider openai --wire-api responses --model gpt-5.6-sol

/tmp/gsea eval export-swebench \
  --results /tmp/gsea-swebench/run/results.jsonl \
  --output /tmp/gsea-swebench/predictions.jsonl \
  --model gsea/gpt-5.6-sol

/tmp/gsea-harnesses/bin/python -m swebench.harness.run_evaluation \
  --dataset_name SWE-bench/SWE-bench_Verified --split test \
  --instance_ids django__django-11099 \
  --predictions_path /tmp/gsea-swebench/predictions.jsonl \
  --run_id gsea-django-11099

/tmp/gsea eval score \
  --benchmark swe-bench-verified \
  --input logs/run_evaluation/gsea-django-11099/results.json
```

An official full score uses the same flow without `--instance_ids`, submits one
prediction for every Verified instance, and scores the generated aggregate
`results.json`. The upstream Docker harness is authoritative; GSEA's native
per-task `resolved` bit is not used as the published SWE-bench score.

`prepare_swebench.py` (flags `--instance-id`, `--output-root`, `--tasks`, and
optional `--dataset`, `--split`, `--hf-endpoint`) downloads the dataset row and
the GitHub source tarball at `base_commit` (network access; `HF_ENDPOINT`
defaults to `https://hf-mirror.com` when unset), writes the row under
`<output-root>/instances/`, extracts the source under `<output-root>/sources/`,
and overwrites `--tasks` with one `directory_copy` task. Its only native check
is a `patch-produced` general check, so the native `resolved` bit only means
that a non-empty diff exists.

### Terminal-Bench 2.1 (Harbor 0.22, historical adapter)

**Blocked for the current ATE-backed runtime.** The adapter still uploads
only `gsea` and exports usage/trajectory from the retired `sessions.db`
business tables. It does not provision the matching Linux `ate` binary or
export native ATE history. Building an extra binary alone does not repair
this integration. The description and commands below document the earlier
SQLite-based experiment, not a current installation or acceptance procedure.
Adapter migration and end-to-end validation remain separate work.

The historical run used a static Linux executable with the Harbor 0.22 `BaseAgent` adapter.
The dataset reference below is intentionally immutable. The adapter selects
GSEA's explicit `container` shell boundary only inside the disposable task
container, exports the full durable token totals, and projects the append-only
session events to an ATIF v1.7 trajectory for audit. It reads the API key by
environment-variable name and never writes the secret value into job config or
command arguments.

The retained `GSEAHarborAgent` implementation uploads the binary to `/opt/gsea/gsea`, runs
`chat --evaluation-mode --shell-sandbox container --workspace /app
--data-dir /tmp/gsea-data`,
retries the process only on transient model-stream failures
(`max_process_attempts`, default 2), then downloads `sessions.db` and reads
`usage_records` and `session_events` from it. A trial that exits 0 but cannot
export durable usage/ATIF fails instead of silently becoming a trace-less
success. Other agent kwargs and defaults: `gsea_model` (default: the last
component of `--model`), `provider=openai`, `wire_api=responses`,
`api_key_env=XAPI_API_KEY`, `reasoning_effort=high`, `request_timeout=10m`,
`process_timeout_seconds=3600`, `max_iterations=30`, `workspace=/app`.
`evals/tests/test_harbor_gsea.py` covers result parsing, durable usage and the
ATIF projection (`python -m unittest evals.tests.test_harbor_gsea` in the
harness environment).

```bash
# Historical pre-ATE recipe; not runnable against the current runtime as-is.
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
  go build -o /tmp/gsea-linux ./cmd/gsea

export XAPI_API_KEY='...'

PYTHONPATH="$PWD" /tmp/gsea-harnesses/bin/harbor run --yes \
  --dataset 'terminal-bench/terminal-bench-2-1@sha256:7d7bdc1cbedad549fc1140404bd4dc45e5fd0ea7c4186773687d177ad3a0699a' \
  --agent evals.adapters.harbor_gsea:GSEAHarborAgent \
  --model openai/gpt-5.6-sol \
  --n-attempts 5 \
  --n-concurrent 1 \
  --jobs-dir "$PWD/.local-agent/evals/harbor-jobs" \
  --job-name gsea-tb21-official \
  --agent-kwarg binary_path=/tmp/gsea-linux \
  --agent-kwarg base_url=https://2xa.cc.cd/v1 \
  --agent-kwarg reasoning_effort=high \
  --agent-env 'XAPI_API_KEY=${XAPI_API_KEY}'

go run ./cmd/gsea eval score \
  --benchmark terminal-bench-2.1 \
  --input "$PWD/.local-agent/evals/harbor-jobs/gsea-tb21-official" \
  --reasoning-effort high
```

On macOS Docker Desktop, keep `--jobs-dir` below a shared `/Users/...` path.
Using `/tmp` can leave Harbor's bind-mounted `/logs/verifier` invisible to the
host even though the verifier ran inside the container. If an enterprise PAC
requires selected download domains to use a proxy, pass the proxy by an
environment template (never its literal credential) and keep localhost plus
PAC-direct domains in `NO_PROXY`; this is an operational network route, not a
test, timeout, or resource change.

Do not add task filters, timeout multipliers, verifier timeout overrides, or resource
overrides to a comparable run. A one-task or one-attempt run is an integration
smoke test and its scorecard will explicitly set `comparable: false`. The
legacy `terminal_bench_gsea.py` adapter remains only for reproducing older
Terminal-Bench 0.2.x experiments. New Harbor results require the ATE migration
and validation noted above.

### AgentDojo

The Unix-socket bridge transports only individual tool calls. Tool definitions
are invocation scoped, marked execute-risk, approved only in explicit
evaluation mode (`--tool-bridge-manifest` and `--tool-bridge-socket` must be
given together with `--evaluation-mode`), and cannot replace a built-in tool:
a name clash fails tool registration at startup. When a bridge is active its
mutation allowlist is exclusive: only the bridged tools are auto-approved, and
workspace write/edit, shell and `merge_apply` are denied, preventing harness
content from inspecting the model process environment. The
official environment mutation and utility/security checks remain in
AgentDojo. `security_results` in AgentDojo actually indicate that the injected
goal succeeded, so the runner also emits explicit `attack_success_rate` and
`secure_rate` fields in its `gsea.agentdojo.result.v1` output. Each AgentDojo
task runs GSEA once (no process retry), because a retry after partial tool
calls would reuse a mutated environment. `--user-task` and `--injection-task`
are repeatable; `--benchmark-version` defaults to `v1.2.2` and `--suite` to
`workspace`. Per-task prompts, stdout/stderr and bridged tool calls are written
below `--artifacts`, with AgentDojo's own logs in `<artifacts>/official`.

```bash
PYTHONPATH="$PWD" \
GSEA_BINARY=/tmp/gsea \
GSEA_BENCH_DATA_ROOT=/tmp/gsea-agentdojo-data \
/tmp/gsea-agentdojo/bin/python evals/run_agentdojo.py \
  --benchmark-version v1.2.2 --suite workspace \
  --user-task user_task_0 \
  --artifacts /tmp/gsea-agentdojo/utility \
  --output /tmp/gsea-agentdojo/utility.json

PYTHONPATH="$PWD" \
GSEA_BINARY=/tmp/gsea \
GSEA_BENCH_DATA_ROOT=/tmp/gsea-agentdojo-data \
/tmp/gsea-agentdojo/bin/python evals/run_agentdojo.py \
  --benchmark-version v1.2.2 --suite workspace \
  --user-task user_task_0 --attack injecagent \
  --injection-task injection_task_0 \
  --artifacts /tmp/gsea-agentdojo/injection \
  --output /tmp/gsea-agentdojo/injection.json
```

The checked integration evidence from 2026-09-05 is recorded in
`evals/results/2026-09-05-integration.json`, alongside one-trial Terminal-Bench
2.1 smoke and pilot scorecards (`comparable: false`). They prove the adapters
execute end to end, but a one-shot sample is not a leaderboard or a statistical
model quality claim.

The common task protocol also covers other suites:

- SWE-bench: use the instance ID as `id`/`instance_id`, materialize the stated
  repository/base commit, run GSEA, then export prediction JSONL. The official
  Python `swebench` Docker harness remains the authoritative scorer.
- Aider Polyglot: no adapter is shipped yet. Materialize each exercise
  repository, put its instruction in `problem_statement`, and express language
  tests as target/regression checks.
- Terminal-style tasks outside Terminal-Bench can still use `directory_copy`
  and a sandbox/Docker verifier in the native evaluator.
- Tool/safety tasks: use a fixture repository containing adversarial content;
  safety checks assert that outside state was not changed and expected files
  were not disclosed.
- Runtime/recovery tasks: keep deterministic Go integration tests for exact
  checkpoint, context, Flow and Tool Pipeline invariants; model-level scenarios
  can additionally be represented in this manifest.

Export SWE-bench predictions:

```bash
gsea eval export-swebench \
  --results results.jsonl \
  --output predictions.jsonl \
  --model gsea/openai-gpt-5.6-sol

python -m swebench.harness.run_evaluation \
  --dataset_name <installed-swebench-dataset> \
  --split test \
  --predictions_path predictions.jsonl \
  --run_id gsea-openai-gpt-5.6-sol
```

The local `resolved` field represents the checks declared in the GSEA task.
For SWE-bench, the official harness report supersedes that field.

## Result artifacts

Each `gsea.eval.result.v1` record includes task identity, model label, Agent
status (`completed`, `failed` or `timed_out`), durable aggregate token/cost
usage, check outcomes, embedded patch, patch SHA-256, timing and paths to
verbose artifacts. The current `gsea eval` runner reads durable usage from the
session's ATE records through `app.ReadSessionUsage`; if that read fails, the
tokens parsed from the headless transcript are kept and `agent.error` records
the failure. The `sessions.db` export described in the historical Harbor
adapter section is retired and is not the current result source.

Per task, `<root>/runs/<run-id>/<task-id>/` (components are sanitized and
suffixed with a short hash) holds `transcript.txt`, `agent.stderr.txt`,
`model.patch`, `check-NNN-<name>.log` and the Agent `data/` directory. The
workspace is created below `<root>/worktree-runs/<run-id>/workspaces/` and
removed after verification unless `--keep-worktrees` is set, in which case
`workspace_path` is recorded. The patch is `git diff --binary` against the
base commit including untracked files, limited to 32 MiB; runtime-owned
`.gsea/artifacts/**` and `.gsea/worktrees/**` are excluded from the model
patch.
