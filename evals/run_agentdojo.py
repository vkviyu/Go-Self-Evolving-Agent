"""Run official AgentDojo tasks through the GSEA tool bridge."""

from __future__ import annotations

import argparse
import json
from pathlib import Path

from agentdojo.attacks import load_attack
from agentdojo.benchmark import (
    SuiteResults,
    benchmark_suite_with_injections,
    benchmark_suite_without_injections,
)
from agentdojo.logging import OutputLogger
from agentdojo.task_suite.load_suites import get_suite

from evals.adapters.agentdojo_gsea import GSEAAgentDojoPipeline


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--benchmark-version", default="v1.2.2")
    parser.add_argument("--suite", default="workspace")
    parser.add_argument("--user-task", action="append", required=True)
    parser.add_argument("--attack")
    parser.add_argument("--injection-task", action="append")
    parser.add_argument("--artifacts", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    arguments = parser.parse_args()

    arguments.artifacts.mkdir(parents=True, exist_ok=True)
    suite = get_suite(arguments.benchmark_version, arguments.suite)
    pipeline = GSEAAgentDojoPipeline(artifact_directory=arguments.artifacts)
    official_logs = arguments.artifacts / "official"
    official_logs.mkdir(parents=True, exist_ok=True)
    with OutputLogger(str(official_logs)):
        if arguments.attack is None:
            results = benchmark_suite_without_injections(
                pipeline,
                suite,
                logdir=official_logs,
                force_rerun=True,
                user_tasks=arguments.user_task,
                benchmark_version=arguments.benchmark_version,
            )
        else:
            attack = load_attack(arguments.attack, suite, pipeline)
            results = benchmark_suite_with_injections(
                pipeline,
                suite,
                attack,
                logdir=official_logs,
                force_rerun=True,
                user_tasks=arguments.user_task,
                injection_tasks=arguments.injection_task,
                verbose=False,
                benchmark_version=arguments.benchmark_version,
            )
    payload = _json_results(arguments, pipeline, results)
    arguments.output.parent.mkdir(parents=True, exist_ok=True)
    arguments.output.write_text(json.dumps(payload, indent=2, ensure_ascii=False))
    print(json.dumps(payload, indent=2, ensure_ascii=False))


def _json_results(
    arguments: argparse.Namespace,
    pipeline: GSEAAgentDojoPipeline,
    results: SuiteResults,
) -> dict:
    utility = {
        f"{user_task}|{injection_task}": passed
        for (user_task, injection_task), passed in results["utility_results"].items()
    }
    security = {
        f"{user_task}|{injection_task}": passed
        for (user_task, injection_task), passed in results["security_results"].items()
    }
    attack_success_rate = (
        sum(security.values()) / len(security)
        if arguments.attack is not None and security
        else None
    )
    return {
        "schema": "gsea.agentdojo.result.v1",
        "benchmark_version": arguments.benchmark_version,
        "suite": arguments.suite,
        "pipeline": pipeline.name,
        "attack": arguments.attack,
        "utility_results": utility,
        "security_results": security,
        "injection_tasks_utility_results": results[
            "injection_tasks_utility_results"
        ],
        "utility": sum(utility.values()) / len(utility) if utility else None,
        # AgentDojo's security_results booleans mean that the injected goal
        # succeeded. Publish both the official raw values and unambiguous rates.
        "attack_success_rate": attack_success_rate,
        "secure_rate": 1.0 - attack_success_rate if attack_success_rate is not None else None,
    }


if __name__ == "__main__":
    main()
