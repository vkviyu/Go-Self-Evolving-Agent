"""Harbor 0.22 adapter for running the GSEA executable in benchmark tasks.

Harbor owns task materialization, time/resource limits, and verification. This
adapter only installs the prebuilt Linux GSEA binary, runs one isolated agent
session in the task environment, and reports usage back to Harbor.
"""

from __future__ import annotations

import asyncio
import hashlib
import json
import shlex
import sqlite3
from pathlib import Path
from typing import Any

from harbor.agents.base import BaseAgent
from harbor.environments.base import BaseEnvironment, ExecResult
from harbor.models.agent.context import AgentContext
from harbor.models.trajectories.agent import Agent
from harbor.models.trajectories.final_metrics import FinalMetrics
from harbor.models.trajectories.metrics import Metrics
from harbor.models.trajectories.observation import Observation
from harbor.models.trajectories.observation_result import ObservationResult
from harbor.models.trajectories.step import Step
from harbor.models.trajectories.tool_call import ToolCall
from harbor.models.trajectories.trajectory import Trajectory
from harbor.utils.trajectory_utils import format_trajectory_json


SYSTEM_PROMPT = """You are GSEA running inside a Terminal-Bench task container.
Complete the user's task in /app using the workspace tools. Work as a single
Agent: do not spawn child Agents or create orchestration tasks. Finish once the
requested terminal state is achieved. Do not inspect /tests or verifier files;
they are evaluator-owned and unavailable during the agent phase."""

REMOTE_ROOT = "/opt/gsea"
REMOTE_BINARY = f"{REMOTE_ROOT}/gsea"
REMOTE_PROMPT = f"{REMOTE_ROOT}/task.txt"
REMOTE_DATA = "/tmp/gsea-data"


class GSEAHarborAgent(BaseAgent):
    """Current Harbor external agent used for Terminal-Bench 2.1."""

    SUPPORTS_ATIF = True

    def __init__(
        self,
        binary_path: str,
        model_name: str | None = None,
        gsea_model: str | None = None,
        provider: str = "openai",
        wire_api: str = "responses",
        base_url: str = "https://2xa.cc.cd/v1",
        api_key_env: str = "XAPI_API_KEY",
        reasoning_effort: str = "high",
        request_timeout: str = "10m",
        process_timeout_seconds: int = 3600,
        max_process_attempts: int = 2,
        max_iterations: int = 30,
        workspace: str = "/app",
        **kwargs: Any,
    ) -> None:
        super().__init__(model_name=model_name, **kwargs)
        self.binary_path = Path(binary_path).expanduser().resolve()
        if not self.binary_path.is_file():
            raise ValueError(f"GSEA Linux binary does not exist: {self.binary_path}")
        self._version = "sha256-" + _sha256(self.binary_path)[:12]
        self.model = gsea_model or _model_component(model_name) or "gpt-5.6-sol"
        self.provider = provider
        self.wire_api = wire_api
        self.base_url = base_url
        self.api_key_env = api_key_env
        self.reasoning_effort = reasoning_effort
        self.request_timeout = request_timeout
        self.process_timeout_seconds = int(process_timeout_seconds)
        self.max_process_attempts = max(1, int(max_process_attempts))
        self.max_iterations = int(max_iterations)
        self.workspace = workspace

    @staticmethod
    def name() -> str:
        return "gsea"

    def version(self) -> str:
        return self._version

    async def setup(self, environment: BaseEnvironment) -> None:
        prepared = await environment.exec(
            command=f"mkdir -p {shlex.quote(REMOTE_ROOT)}",
            user="root",
            timeout_sec=30,
        )
        _require_success(prepared, "create GSEA install directory")
        await environment.upload_file(self.binary_path, REMOTE_BINARY)
        installed = await environment.exec(
            command=f"chmod 0755 {shlex.quote(REMOTE_BINARY)}",
            user="root",
            timeout_sec=30,
        )
        _require_success(installed, "install GSEA executable")

    async def run(
        self,
        instruction: str,
        environment: BaseEnvironment,
        context: AgentContext,
    ) -> None:
        api_key = self._get_env(self.api_key_env)
        if not api_key:
            raise RuntimeError(
                f"required API key environment is unset: {self.api_key_env}"
            )

        self.logs_dir.mkdir(parents=True, exist_ok=True)
        prompt_path = self.logs_dir / "prompt.txt"
        prompt_path.write_text(instruction, encoding="utf-8")
        await environment.upload_file(prompt_path, REMOTE_PROMPT)

        session_id = "harbor-" + str(self.context_id or self.session_id or "trial")
        command = [
            REMOTE_BINARY,
            "chat",
            "--prompt-file",
            REMOTE_PROMPT,
            "--headless-format",
            "json",
            "--evaluation-mode",
            "--shell-sandbox",
            "container",
            "--workspace",
            self.workspace,
            "--data-dir",
            REMOTE_DATA,
            "--session",
            session_id,
            "--provider",
            self.provider,
            "--wire-api",
            self.wire_api,
            "--model",
            self.model,
            "--base-url",
            self.base_url,
            "--api-key-env",
            self.api_key_env,
            "--reasoning-effort",
            self.reasoning_effort,
            "--timeout",
            self.request_timeout,
            "--max-iterations",
            str(self.max_iterations),
            "--system",
            SYSTEM_PROMPT,
        ]
        execution: ExecResult | None = None
        for attempt in range(1, self.max_process_attempts + 1):
            execution = await environment.exec(
                command=shlex.join(command),
                cwd=self.workspace,
                env={
                    self.api_key_env: api_key,
                    "GSEA_LSP_ENABLED": "false",
                    "GSEA_HOME": "/tmp/gsea-home",
                },
                timeout_sec=self.process_timeout_seconds,
            )
            attempt_stdout = execution.stdout or ""
            attempt_stderr = execution.stderr or ""
            (self.logs_dir / f"gsea.attempt-{attempt}.stdout.txt").write_text(
                attempt_stdout, encoding="utf-8"
            )
            (self.logs_dir / f"gsea.attempt-{attempt}.stderr.txt").write_text(
                attempt_stderr, encoding="utf-8"
            )
            if execution.return_code == 0 or not _is_transient_model_failure(
                attempt_stdout + "\n" + attempt_stderr
            ):
                break
            if attempt < self.max_process_attempts:
                await asyncio.sleep(float(2 ** (attempt - 1)))
        assert execution is not None
        stdout = execution.stdout or ""
        stderr = execution.stderr or ""
        (self.logs_dir / "gsea.stdout.jsonl").write_text(stdout, encoding="utf-8")
        (self.logs_dir / "gsea.stderr.txt").write_text(stderr, encoding="utf-8")

        usage = _empty_usage()
        parse_error: Exception | None = None
        try:
            usage = _parse_headless_result(stdout)
        except Exception as error:
            parse_error = error
        database_path = self.logs_dir / "sessions.db"
        try:
            await environment.download_file(
                f"{REMOTE_DATA}/sessions.db", database_path
            )
            usage = _read_durable_usage(database_path)
            _write_atif_trajectory(
                database_path,
                self.logs_dir / "trajectory.json",
                session_id=session_id,
                agent_version=self.version(),
                model_name=self.model_name or self.model,
                reasoning_effort=self.reasoning_effort,
            )
        except Exception as error:
            # Successful Terminal-Bench submissions require a complete ATIF
            # trace for the reward-hacking audit. Do not advertise ATIF support
            # while silently degrading to a trace-less successful trial.
            if execution.return_code == 0:
                raise RuntimeError(
                    f"durable usage/ATIF export failed: {error}"
                ) from error
            context.metadata = {
                "artifact_warning": f"durable usage/ATIF export failed: {error}"
            }

        context.n_input_tokens = usage["input_tokens"]
        context.n_cache_tokens = usage["cached_tokens"]
        context.n_output_tokens = usage["output_tokens"]
        if usage["cost_micros"] > 0:
            context.cost_usd = usage["cost_micros"] / 1_000_000
        context.metadata = {
            **(context.metadata or {}),
            "gsea_session_id": session_id,
            "reasoning_tokens": usage["reasoning_tokens"],
            "total_tokens": usage["total_tokens"],
        }

        if execution.return_code != 0:
            detail = (stderr or stdout or "no output").strip()[-2000:]
            raise RuntimeError(
                f"GSEA exited with code {execution.return_code}: {detail}"
            )
        if parse_error is not None:
            raise RuntimeError(f"GSEA result record is unavailable: {parse_error}")


def _require_success(result: ExecResult, action: str) -> None:
    if result.return_code == 0:
        return
    detail = (result.stderr or result.stdout or "no output").strip()[-2000:]
    raise RuntimeError(f"failed to {action} (exit {result.return_code}): {detail}")


def _parse_headless_result(stdout: str) -> dict[str, int]:
    records: list[dict[str, Any]] = []
    for line in stdout.splitlines():
        if not line.strip():
            continue
        try:
            value = json.loads(line)
        except json.JSONDecodeError:
            continue
        if isinstance(value, dict) and value.get("schema") == "gsea.chat.result.v1":
            records.append(value)
    if len(records) != 1:
        raise RuntimeError("GSEA returned an invalid gsea.chat.result.v1 record")
    usage = records[0].get("usage") or {}
    return {
        "input_tokens": int(usage.get("input_tokens", 0)),
        "cached_tokens": int(usage.get("cached_tokens", 0)),
        "output_tokens": int(usage.get("output_tokens", 0)),
        "reasoning_tokens": int(usage.get("reasoning_tokens", 0)),
        "total_tokens": int(usage.get("total_tokens", 0)),
        "cost_micros": 0,
    }


def _empty_usage() -> dict[str, int]:
    return {
        "input_tokens": 0,
        "cached_tokens": 0,
        "output_tokens": 0,
        "reasoning_tokens": 0,
        "total_tokens": 0,
        "cost_micros": 0,
    }


def _is_transient_model_failure(output: str) -> bool:
    normalized = output.lower()
    markers = (
        "read responses stream",
        "internal_error",
        "connection reset by peer",
        "unexpected eof",
        "server disconnected",
        "http2: stream closed",
    )
    return any(marker in normalized for marker in markers)


def _read_durable_usage(database: Path) -> dict[str, int]:
    with sqlite3.connect(f"file:{database}?mode=ro", uri=True) as connection:
        row = connection.execute(
            """SELECT COALESCE(SUM(input_tokens), 0),
                      COALESCE(SUM(cached_tokens), 0),
                      COALESCE(SUM(output_tokens), 0),
                      COALESCE(SUM(reasoning_tokens), 0),
                      COALESCE(SUM(total_tokens), 0),
                      COALESCE(SUM(cost_micros), 0)
                 FROM usage_records"""
        ).fetchone()
    if row is None:
        raise RuntimeError("usage_records query returned no row")
    keys = (
        "input_tokens",
        "cached_tokens",
        "output_tokens",
        "reasoning_tokens",
        "total_tokens",
        "cost_micros",
    )
    return dict(zip(keys, (int(value) for value in row), strict=True))


def _write_atif_trajectory(
    database: Path,
    destination: Path,
    *,
    session_id: str,
    agent_version: str,
    model_name: str,
    reasoning_effort: str,
) -> None:
    """Project GSEA's append-only event stream into Harbor ATIF v1.7.

    The durable events are the source of truth: assistant tool calls and their
    observations stay linked by provider call ID, while operational metadata
    (policy and approval events) remains in sessions.db for deeper auditing.
    GSEA does not persist hidden chain-of-thought, so reasoning_content is
    intentionally absent.
    """

    with sqlite3.connect(f"file:{database}?mode=ro", uri=True) as connection:
        events = connection.execute(
            """SELECT event_type, occurred_at, payload
                 FROM session_events
                WHERE session_id = ?
                ORDER BY sequence""",
            (session_id,),
        ).fetchall()
        usage_rows = connection.execute(
            """SELECT request_id, input_tokens, cached_tokens, output_tokens,
                      reasoning_tokens, cost_micros
                 FROM usage_records"""
        ).fetchall()

    usage_by_request = {
        str(row[0]): {
            "input_tokens": int(row[1]),
            "cached_tokens": int(row[2]),
            "output_tokens": int(row[3]),
            "reasoning_tokens": int(row[4]),
            "cost_micros": int(row[5]),
        }
        for row in usage_rows
    }
    steps: list[Step] = [
        Step(step_id=1, source="system", message=SYSTEM_PROMPT)
    ]
    step_by_call_id: dict[str, Step] = {}

    for event_type, occurred_at, raw_payload in events:
        payload = json.loads(raw_payload)
        if event_type == "user.message":
            content = str(payload.get("content") or "")
            steps.append(
                Step(
                    step_id=len(steps) + 1,
                    timestamp=occurred_at,
                    source="user",
                    message=content,
                )
            )
            continue
        if event_type == "assistant.message":
            tool_calls = [_atif_tool_call(value) for value in payload.get("tool_calls") or []]
            request_usage = usage_by_request.get(str(payload.get("request_id") or ""))
            metrics = _atif_metrics(payload, request_usage)
            step = Step(
                step_id=len(steps) + 1,
                timestamp=occurred_at,
                source="agent",
                model_name=model_name,
                reasoning_effort=reasoning_effort,
                message=str(payload.get("content") or ""),
                tool_calls=tool_calls or None,
                metrics=metrics,
                llm_call_count=1,
            )
            steps.append(step)
            for call in tool_calls:
                step_by_call_id[call.tool_call_id] = step
            continue
        if event_type not in {"tool.succeeded", "tool.failed"}:
            continue
        call_id = str(payload.get("call_id") or "")
        step = step_by_call_id.get(call_id)
        if step is None:
            continue
        if event_type == "tool.succeeded":
            result = payload.get("result")
            content = _stringify_tool_result(result)
        else:
            content = json.dumps(
                {"error": str(payload.get("error") or "tool failed")},
                ensure_ascii=False,
                separators=(",", ":"),
            )
        observation_result = ObservationResult(
            source_call_id=call_id,
            content=content,
        )
        if step.observation is None:
            step.observation = Observation(results=[observation_result])
        else:
            step.observation.results.append(observation_result)

    if len(steps) == 1:
        raise RuntimeError(f"no GSEA conversation events found for {session_id}")

    usage = _read_durable_usage(database)
    trajectory = Trajectory(
        schema_version="ATIF-v1.7",
        session_id=session_id,
        agent=Agent(
            name=GSEAHarborAgent.name(),
            version=agent_version,
            model_name=model_name,
            extra={"runtime": "GSEA", "reasoning_effort": reasoning_effort},
        ),
        steps=steps,
        final_metrics=FinalMetrics(
            total_prompt_tokens=usage["input_tokens"],
            total_completion_tokens=usage["output_tokens"],
            total_cached_tokens=usage["cached_tokens"] or None,
            total_cost_usd=(
                usage["cost_micros"] / 1_000_000
                if usage["cost_micros"] > 0
                else None
            ),
            total_steps=len(steps),
            extra={"total_reasoning_tokens": usage["reasoning_tokens"]},
        ),
        notes=(
            "Projected from GSEA's durable append-only session events; hidden "
            "chain-of-thought is neither requested nor persisted."
        ),
    )
    destination.write_text(
        format_trajectory_json(trajectory.to_json_dict()), encoding="utf-8"
    )


def _atif_tool_call(value: dict[str, Any]) -> ToolCall:
    function = value.get("function") or {}
    raw_arguments = function.get("arguments") or "{}"
    if isinstance(raw_arguments, dict):
        arguments = raw_arguments
    else:
        try:
            decoded = json.loads(str(raw_arguments))
            arguments = decoded if isinstance(decoded, dict) else {"value": decoded}
        except json.JSONDecodeError:
            arguments = {"_raw": str(raw_arguments)}
    return ToolCall(
        tool_call_id=str(value.get("id") or ""),
        function_name=str(function.get("name") or "unknown"),
        arguments=arguments,
    )


def _atif_metrics(
    payload: dict[str, Any], request_usage: dict[str, int] | None
) -> Metrics | None:
    input_tokens = int(payload.get("prompt_tokens") or 0)
    cached_tokens = int(payload.get("cached_prompt_tokens") or 0)
    output_tokens = int(payload.get("completion_tokens") or 0)
    reasoning_tokens = int(payload.get("reasoning_tokens") or 0)
    cost_micros = 0
    if request_usage is not None:
        input_tokens = request_usage["input_tokens"]
        cached_tokens = request_usage["cached_tokens"]
        output_tokens = request_usage["output_tokens"]
        reasoning_tokens = request_usage["reasoning_tokens"]
        cost_micros = request_usage["cost_micros"]
    if not any((input_tokens, cached_tokens, output_tokens, reasoning_tokens, cost_micros)):
        return None
    return Metrics(
        prompt_tokens=input_tokens,
        completion_tokens=output_tokens,
        cached_tokens=cached_tokens or None,
        cost_usd=cost_micros / 1_000_000 if cost_micros > 0 else None,
        extra={"reasoning_tokens": reasoning_tokens} if reasoning_tokens else None,
    )


def _stringify_tool_result(value: Any) -> str:
    if isinstance(value, str):
        return value
    return json.dumps(value, ensure_ascii=False, separators=(",", ":"))


def _model_component(model_name: str | None) -> str | None:
    if not model_name:
        return None
    return model_name.rsplit("/", 1)[-1]


def _sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as source:
        for chunk in iter(lambda: source.read(1 << 20), b""):
            digest.update(chunk)
    return digest.hexdigest()
