"""Inspect AI solver that evaluates the real GSEA executable."""

from __future__ import annotations

import asyncio
import re
import tempfile
from pathlib import Path

from inspect_ai.model import ModelOutput
from inspect_ai.solver import TaskState, solver

from evals.adapters.gsea_client import GSEAClient


SYSTEM_PROMPT = """You are being evaluated by Inspect AI. Solve the supplied
question yourself. Follow its requested output format exactly. Do not discuss
the evaluation harness or use workspace tools unless the question requires it."""


@solver
def gsea_solver(system_prompt: str = SYSTEM_PROMPT):
    """Run one isolated GSEA session for every Inspect sample."""

    client = GSEAClient()

    async def solve(state: TaskState, generate) -> TaskState:
        del generate
        prompt = _sample_prompt(state)
        session_hint = f"inspect-{state.sample_id}-epoch-{state.epoch}"
        with tempfile.TemporaryDirectory(prefix="gsea-inspect-workspace-") as directory:
            result = await asyncio.to_thread(
                client.run,
                prompt,
                Path(directory),
                session_hint,
                system_prompt=system_prompt,
            )
        state.output = ModelOutput.from_content(
            f"gsea/{client.config.model}", result.content
        )
        _mark_multiple_choice_answer(state, result.content)
        return state

    return solve


def _sample_prompt(state: TaskState) -> str:
    if not state.choices:
        return state.input_text
    letters = [chr(ord("A") + index) for index in range(len(state.choices))]
    options = "\n".join(
        f"{letter}) {choice.value}"
        for letter, choice in zip(letters, state.choices, strict=True)
    )
    return (
        "Answer the following multiple choice question. The last line of your "
        "response must be exactly 'ANSWER: $LETTER', where LETTER is one of "
        f"{','.join(letters)}.\n\n{state.input_text}\n\n{options}"
    )


def _mark_multiple_choice_answer(state: TaskState, completion: str) -> None:
    if not state.choices:
        return
    matches = re.findall(r"(?im)^ANSWER\s*:\s*([A-Z])\s*[.]?\s*$", completion)
    selected = ord(matches[-1]) - ord("A") if matches else -1
    for index in range(len(state.choices)):
        state.choices.mark_choice(index, index == selected)
