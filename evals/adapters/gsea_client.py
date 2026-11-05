"""Small subprocess boundary shared by Python benchmark harnesses.

The Go executable remains the Agent implementation. Python adapters only map
benchmark inputs and outputs; they do not reproduce the Agent loop.
"""

from __future__ import annotations

import hashlib
import json
import os
import re
import subprocess
import tempfile
import time
from collections.abc import Sequence
from dataclasses import dataclass
from pathlib import Path


@dataclass(frozen=True)
class GSEAResult:
    content: str
    usage: dict[str, int]
    stdout: str
    stderr: str


@dataclass(frozen=True)
class GSEAConfig:
    binary: Path
    data_root: Path
    provider: str = "openai"
    wire_api: str = "responses"
    model: str = "gpt-5.6-sol"
    base_url: str = "https://2xa.cc.cd/v1"
    api_key_env: str = "XAPI_API_KEY"
    reasoning_effort: str = "xhigh"
    request_timeout: str = "10m"
    process_timeout_seconds: int = 900
    max_iterations: int = 30
    attempts: int = 3

    @classmethod
    def from_environment(cls) -> "GSEAConfig":
        binary = Path(os.environ.get("GSEA_BINARY", "local-agent")).expanduser()
        data_root_value = os.environ.get("GSEA_BENCH_DATA_ROOT")
        if data_root_value:
            data_root = Path(data_root_value).expanduser()
            data_root.mkdir(parents=True, exist_ok=True)
        else:
            data_root = Path(tempfile.mkdtemp(prefix="gsea-bench-data-"))
        return cls(
            binary=binary,
            data_root=data_root,
            provider=os.environ.get("GSEA_PROVIDER", "openai"),
            wire_api=os.environ.get("GSEA_WIRE_API", "responses"),
            model=os.environ.get("GSEA_MODEL", "gpt-5.6-sol"),
            base_url=os.environ.get("GSEA_BASE_URL", "https://2xa.cc.cd/v1"),
            api_key_env=os.environ.get("GSEA_API_KEY_ENV", "XAPI_API_KEY"),
            reasoning_effort=os.environ.get("GSEA_REASONING_EFFORT", "xhigh"),
            request_timeout=os.environ.get("GSEA_REQUEST_TIMEOUT", "10m"),
            process_timeout_seconds=int(os.environ.get("GSEA_PROCESS_TIMEOUT_SECONDS", "900")),
            max_iterations=int(os.environ.get("GSEA_MAX_ITERATIONS", "30")),
            attempts=int(os.environ.get("GSEA_ATTEMPTS", "3")),
        )


class GSEAClient:
    def __init__(self, config: GSEAConfig | None = None) -> None:
        self.config = config or GSEAConfig.from_environment()

    def run(
        self,
        prompt: str,
        workspace: Path,
        session_hint: str,
        *,
        system_prompt: str = "",
        evaluation_mode: bool = False,
        extra_arguments: Sequence[str] = (),
    ) -> GSEAResult:
        config = self.config
        workspace = workspace.resolve()
        workspace.mkdir(parents=True, exist_ok=True)
        if config.attempts < 1:
            raise ValueError("GSEA attempts must be positive")
        completed: subprocess.CompletedProcess[str] | None = None
        errors: list[str] = []
        for attempt in range(1, config.attempts + 1):
            session_id = _session_id(f"{session_hint}-attempt-{attempt}")
            data_directory = config.data_root / session_id
            data_directory.mkdir(parents=True, exist_ok=True)
            command = _command(
                config,
                workspace,
                data_directory,
                session_id,
                system_prompt,
                evaluation_mode,
                extra_arguments,
            )
            try:
                completed = subprocess.run(
                    command,
                    input=prompt,
                    text=True,
                    capture_output=True,
                    timeout=config.process_timeout_seconds,
                    check=False,
                )
            except subprocess.TimeoutExpired as error:
                errors.append(f"attempt {attempt}: process timed out after {error.timeout}s")
                completed = None
            else:
                if completed.returncode == 0:
                    break
                errors.append(
                    f"attempt {attempt}: exit {completed.returncode}: {completed.stderr.strip()}"
                )
            if attempt < config.attempts:
                time.sleep(min(2 ** attempt, 8))
        if completed is None or completed.returncode != 0:
            raise RuntimeError("GSEA failed after retries: " + " | ".join(errors))
        lines = [line for line in completed.stdout.splitlines() if line.strip()]
        if len(lines) != 1:
            raise RuntimeError(f"GSEA returned {len(lines)} stdout records, expected one")
        try:
            payload = json.loads(lines[0])
        except json.JSONDecodeError as error:
            raise RuntimeError(f"GSEA returned invalid JSON: {error}") from error
        if payload.get("schema") != "gsea.chat.result.v1":
            raise RuntimeError(f"unexpected GSEA response schema: {payload.get('schema')!r}")
        content = payload.get("content")
        if not isinstance(content, str):
            raise RuntimeError("GSEA response content must be a string")
        usage = payload.get("usage", {})
        if not isinstance(usage, dict):
            raise RuntimeError("GSEA response usage must be an object")
        return GSEAResult(content=content, usage=usage, stdout=completed.stdout, stderr=completed.stderr)


def _command(
    config: GSEAConfig,
    workspace: Path,
    data_directory: Path,
    session_id: str,
    system_prompt: str,
    evaluation_mode: bool,
    extra_arguments: Sequence[str],
) -> list[str]:
    command = [
        str(config.binary),
        "chat",
        "--prompt-file",
        "-",
        "--headless-format",
        "json",
        "--workspace",
        str(workspace),
        "--data-dir",
        str(data_directory),
        "--session",
        session_id,
        "--provider",
        config.provider,
        "--wire-api",
        config.wire_api,
        "--model",
        config.model,
        "--base-url",
        config.base_url,
        "--api-key-env",
        config.api_key_env,
        "--reasoning-effort",
        config.reasoning_effort,
        "--timeout",
        config.request_timeout,
        "--max-iterations",
        str(config.max_iterations),
        "--goal-mode=false",
    ]
    if system_prompt:
        command.extend(["--system", system_prompt])
    if evaluation_mode:
        command.append("--evaluation-mode")
    command.extend(extra_arguments)
    return command


def _session_id(value: str) -> str:
    safe = re.sub(r"[^A-Za-z0-9._-]+", "-", value).strip(".-") or "benchmark"
    digest = hashlib.sha256(value.encode("utf-8")).hexdigest()[:8]
    return f"bench-{safe[:104]}-{digest}"
