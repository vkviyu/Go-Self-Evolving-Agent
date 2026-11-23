"""Terminal-Bench BaseAgent adapter for the real GSEA Linux executable."""

from __future__ import annotations

import json
import os
import tempfile
from pathlib import Path

from terminal_bench.agents.base_agent import AgentResult, BaseAgent
from terminal_bench.agents.failure_mode import FailureMode
from terminal_bench.terminal.tmux_session import TmuxSession


SYSTEM_PROMPT = """You are GSEA running inside a Terminal-Bench task container.
Complete the user's task in /app using the workspace tools. Work as a single
Agent: do not spawn child Agents or create orchestration tasks. Finish once the
requested terminal state is achieved."""


class GSEATerminalBenchAgent(BaseAgent):
    def __init__(
        self,
        binary_path: str,
        model_name: str | None = None,
        gsea_model: str | None = None,
        provider: str = "openai",
        wire_api: str = "responses",
        base_url: str = "https://2xa.cc.cd/v1",
        api_key_env: str = "XAPI_API_KEY",
        reasoning_effort: str = "xhigh",
        request_timeout: str = "10m",
        max_iterations: int = 30,
        **kwargs,
    ) -> None:
        super().__init__(**kwargs)
        self.binary_path = Path(binary_path).expanduser().resolve()
        if not self.binary_path.is_file():
            raise ValueError(f"GSEA Linux binary does not exist: {self.binary_path}")
        self.model = gsea_model or _model_component(model_name) or "gpt-5.6-sol"
        self.provider = provider
        self.wire_api = wire_api
        self.base_url = base_url
        self.api_key_env = api_key_env
        self.reasoning_effort = reasoning_effort
        self.request_timeout = request_timeout
        self.max_iterations = int(max_iterations)

    @staticmethod
    def name() -> str:
        return "gsea"

    def perform_task(
        self,
        instruction: str,
        session: TmuxSession,
        logging_dir: Path | None = None,
    ) -> AgentResult:
        api_key = os.environ.get(self.api_key_env)
        if not api_key:
            raise RuntimeError(f"required API key environment is unset: {self.api_key_env}")

        container_binary = "/opt/gsea/gsea"
        container_prompt = "/opt/gsea/task.txt"
        session.copy_to_container(
            self.binary_path,
            container_dir="/opt/gsea",
            container_filename="gsea",
        )
        chmod = session.container.exec_run(["chmod", "0755", container_binary])
        if chmod.exit_code != 0:
            return AgentResult(failure_mode=FailureMode.AGENT_INSTALLATION_FAILED)

        with tempfile.NamedTemporaryFile("w", suffix=".txt", delete=False) as prompt_file:
            prompt_file.write(self._render_instruction(instruction))
            prompt_path = Path(prompt_file.name)
        try:
            session.copy_to_container(
                prompt_path,
                container_dir="/opt/gsea",
                container_filename="task.txt",
            )
        finally:
            prompt_path.unlink(missing_ok=True)

        command = [
            container_binary,
            "chat",
            "--prompt-file",
            container_prompt,
            "--headless-format",
            "json",
            "--evaluation-mode",
            "--shell-sandbox",
            "container",
            "--workspace",
            "/app",
            "--data-dir",
            "/tmp/gsea-data",
            "--session",
            "terminal-bench",
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
        execution = session.container.exec_run(
            command,
            workdir="/app",
            environment={
                self.api_key_env: api_key,
                "GSEA_LSP_ENABLED": "false",
                "GSEA_HOME": "/tmp/gsea-home",
            },
            demux=True,
        )
        stdout_bytes, stderr_bytes = execution.output or (b"", b"")
        stdout = (stdout_bytes or b"").decode(errors="replace")
        stderr = (stderr_bytes or b"").decode(errors="replace")
        if logging_dir is not None:
            logging_dir.mkdir(parents=True, exist_ok=True)
            (logging_dir / "gsea.stdout.jsonl").write_text(stdout)
            (logging_dir / "gsea.stderr.txt").write_text(stderr)
            (logging_dir / "prompt.txt").write_text(instruction)
            # Preserve the durable event log before Terminal-Bench removes the
            # disposable task container. Older task definitions do not mount
            # /agent-logs, so copy the closed SQLite file through Docker exec.
            database = session.container.exec_run(
                ["cat", "/tmp/gsea-data/sessions.db"]
            )
            if database.exit_code == 0 and isinstance(database.output, bytes):
                (logging_dir / "sessions.db").write_bytes(database.output)

        if execution.exit_code != 0:
            return AgentResult(failure_mode=FailureMode.UNKNOWN_AGENT_ERROR)
        try:
            records = [json.loads(line) for line in stdout.splitlines() if line.strip()]
            if len(records) != 1 or records[0].get("schema") != "gsea.chat.result.v1":
                raise ValueError("unexpected GSEA output")
            usage = records[0].get("usage", {})
            input_tokens = int(usage.get("input_tokens", 0))
            output_tokens = int(usage.get("output_tokens", 0))
        except (TypeError, ValueError, json.JSONDecodeError):
            return AgentResult(failure_mode=FailureMode.PARSE_ERROR)
        return AgentResult(
            total_input_tokens=input_tokens,
            total_output_tokens=output_tokens,
            failure_mode=FailureMode.NONE,
        )


def _model_component(model_name: str | None) -> str | None:
    if not model_name:
        return None
    return model_name.rsplit("/", 1)[-1]
