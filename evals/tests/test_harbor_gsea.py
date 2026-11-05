from __future__ import annotations

import sqlite3
import tempfile
import unittest
import json
from pathlib import Path

from evals.adapters.harbor_gsea import (
    _parse_headless_result,
    _read_durable_usage,
    _is_transient_model_failure,
    _write_atif_trajectory,
)


class HarborGSEAAdapterTest(unittest.TestCase):
    def test_transient_model_failure_detection(self) -> None:
        self.assertTrue(
            _is_transient_model_failure(
                "read Responses stream: stream ID 27; INTERNAL_ERROR"
            )
        )
        self.assertFalse(_is_transient_model_failure("credential is invalid"))

    def test_parse_headless_result(self) -> None:
        usage = _parse_headless_result(
            "WARNING: optional accelerator unavailable\n"
            "session: benchmark-session\n"
            '{"schema":"gsea.chat.result.v1","content":"done","usage":'
            '{"input_tokens":10,"cached_tokens":3,"output_tokens":4,'
            '"reasoning_tokens":2,"total_tokens":14}}\n'
        )
        self.assertEqual(usage["input_tokens"], 10)
        self.assertEqual(usage["cached_tokens"], 3)
        self.assertEqual(usage["output_tokens"], 4)
        self.assertEqual(usage["reasoning_tokens"], 2)

    def test_read_durable_usage_aggregates_all_model_calls(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            database = Path(directory) / "sessions.db"
            with sqlite3.connect(database) as connection:
                connection.execute(
                    """CREATE TABLE usage_records (
                        input_tokens INTEGER, cached_tokens INTEGER,
                        output_tokens INTEGER, reasoning_tokens INTEGER,
                        total_tokens INTEGER, cost_micros INTEGER
                    )"""
                )
                connection.executemany(
                    "INSERT INTO usage_records VALUES (?, ?, ?, ?, ?, ?)",
                    [(10, 3, 4, 2, 14, 7), (20, 8, 5, 1, 25, 11)],
                )
            usage = _read_durable_usage(database)
        self.assertEqual(
            usage,
            {
                "input_tokens": 30,
                "cached_tokens": 11,
                "output_tokens": 9,
                "reasoning_tokens": 3,
                "total_tokens": 39,
                "cost_micros": 18,
            },
        )

    def test_write_atif_trajectory_links_tool_observation(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            database = Path(directory) / "sessions.db"
            destination = Path(directory) / "trajectory.json"
            with sqlite3.connect(database) as connection:
                connection.execute(
                    """CREATE TABLE session_events (
                        session_id TEXT, sequence INTEGER, event_type TEXT,
                        occurred_at TEXT, payload BLOB
                    )"""
                )
                connection.execute(
                    """CREATE TABLE usage_records (
                        request_id TEXT, input_tokens INTEGER,
                        cached_tokens INTEGER, output_tokens INTEGER,
                        reasoning_tokens INTEGER, total_tokens INTEGER,
                        cost_micros INTEGER
                    )"""
                )
                events = [
                    ("session", 1, "user.message", '{"content":"fix it"}'),
                    (
                        "session",
                        2,
                        "assistant.message",
                        json.dumps(
                            {
                                "content": "",
                                "request_id": "request-1",
                                "tool_calls": [
                                    {
                                        "id": "call-1",
                                        "function": {
                                            "name": "shell_exec",
                                            "arguments": '{"command":"pwd"}',
                                        },
                                    }
                                ],
                            }
                        ),
                    ),
                    (
                        "session",
                        3,
                        "tool.succeeded",
                        '{"call_id":"call-1","result":{"stdout":"/app"}}',
                    ),
                    (
                        "session",
                        4,
                        "assistant.message",
                        '{"content":"done","request_id":"request-2"}',
                    ),
                ]
                connection.executemany(
                    "INSERT INTO session_events VALUES (?, ?, ?, '2026-09-05T00:00:00Z', ?)",
                    events,
                )
                connection.executemany(
                    "INSERT INTO usage_records VALUES (?, ?, ?, ?, ?, ?, ?)",
                    [
                        ("request-1", 10, 2, 3, 1, 13, 7),
                        ("request-2", 20, 5, 4, 0, 24, 11),
                    ],
                )
            _write_atif_trajectory(
                database,
                destination,
                session_id="session",
                agent_version="test",
                model_name="openai/model",
                reasoning_effort="high",
            )
            trajectory = json.loads(destination.read_text(encoding="utf-8"))
        self.assertEqual(trajectory["schema_version"], "ATIF-v1.7")
        self.assertEqual(trajectory["final_metrics"]["total_prompt_tokens"], 30)
        tool_step = trajectory["steps"][2]
        self.assertEqual(tool_step["tool_calls"][0]["tool_call_id"], "call-1")
        self.assertEqual(
            tool_step["observation"]["results"][0]["source_call_id"], "call-1"
        )


if __name__ == "__main__":
    unittest.main()
