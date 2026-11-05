"""Materialize official SWE-bench instances as GSEA task manifests."""

from __future__ import annotations

import argparse
import json
import os
import shutil
import tarfile
import tempfile
import urllib.request
from pathlib import Path


DEFAULT_DATASET = "SWE-bench/SWE-bench_Verified"
DEFAULT_HF_ENDPOINT = "https://hf-mirror.com"


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--instance-id", required=True)
    parser.add_argument("--output-root", type=Path, required=True)
    parser.add_argument("--tasks", type=Path, required=True)
    parser.add_argument("--dataset", default=DEFAULT_DATASET)
    parser.add_argument("--split", default="test")
    parser.add_argument("--hf-endpoint", default=DEFAULT_HF_ENDPOINT)
    args = parser.parse_args()

    os.environ.setdefault("HF_ENDPOINT", args.hf_endpoint)
    from datasets import load_dataset

    dataset = load_dataset(args.dataset, split=args.split)
    matches = [row for row in dataset if row["instance_id"] == args.instance_id]
    if len(matches) != 1:
        raise SystemExit(f"expected one {args.instance_id!r} row, found {len(matches)}")
    row = matches[0]

    output_root = args.output_root.resolve()
    source = output_root / "sources" / safe_component(args.instance_id)
    source.parent.mkdir(parents=True, exist_ok=True)
    if source.exists():
        raise SystemExit(f"source already exists: {source}")

    archive_url = (
        f"https://api.github.com/repos/{row['repo']}/tarball/{row['base_commit']}"
    )
    with tempfile.TemporaryDirectory(prefix="gsea-swebench-download-") as temporary:
        archive = Path(temporary) / "source.tar.gz"
        request = urllib.request.Request(
            archive_url,
            headers={"Accept": "application/vnd.github+json", "User-Agent": "gsea-eval"},
        )
        with urllib.request.urlopen(request, timeout=120) as response:
            with archive.open("wb") as output:
                shutil.copyfileobj(response, output)
        extract_archive(archive, source)

    instance_path = output_root / "instances" / f"{safe_component(args.instance_id)}.json"
    instance_path.parent.mkdir(parents=True, exist_ok=True)
    instance_path.write_text(json.dumps(row, ensure_ascii=False, indent=2) + "\n")

    task = {
        "schema": "gsea.eval.task.v1",
        "id": row["instance_id"],
        "instance_id": row["instance_id"],
        "suite": "swe-bench-verified",
        "kind": "patch",
        "language": "python",
        "workspace_mode": "directory_copy",
        "repository": str(source),
        "problem_statement": row["problem_statement"],
        "agent_timeout_seconds": 1800,
        "checks": [
            {
                "name": "patch-produced",
                "phase": "general",
                "backend": "sandbox",
                "command": 'test -n "$(git diff --name-only HEAD)"',
                "timeout_seconds": 30,
            }
        ],
        "tags": ["swe-bench", "verified"],
        "metadata": {
            "dataset": args.dataset,
            "dataset_split": args.split,
            "repository": row["repo"],
            "base_commit": row["base_commit"],
            "version": row.get("version", ""),
            "instance_path": str(instance_path),
        },
    }
    args.tasks.parent.mkdir(parents=True, exist_ok=True)
    args.tasks.write_text(json.dumps(task, ensure_ascii=False) + "\n")
    print(json.dumps({"task": task, "tasks_path": str(args.tasks)}, ensure_ascii=False))


def extract_archive(archive: Path, destination: Path) -> None:
    temporary_root = destination.parent / f".{destination.name}.extracting"
    if temporary_root.exists():
        raise RuntimeError(f"temporary extraction directory already exists: {temporary_root}")
    temporary_root.mkdir(parents=True)
    try:
        with tarfile.open(archive, "r:gz") as bundle:
            members = bundle.getmembers()
            roots = {Path(member.name).parts[0] for member in members if member.name}
            if len(roots) != 1:
                raise RuntimeError("GitHub source archive must contain one root directory")
            root = next(iter(roots))
            for member in members:
                relative = Path(member.name).relative_to(root)
                if not relative.parts:
                    continue
                target = (temporary_root / relative).resolve()
                if temporary_root.resolve() not in target.parents:
                    raise RuntimeError(f"archive entry escapes destination: {member.name}")
                if member.issym():
                    link_target = (target.parent / member.linkname).resolve()
                    if temporary_root.resolve() not in link_target.parents:
                        raise RuntimeError(f"archive link escapes destination: {member.name}")
                    target.parent.mkdir(parents=True, exist_ok=True)
                    target.symlink_to(member.linkname)
                elif member.islnk():
                    linked_name = Path(member.linkname)
                    if linked_name.parts and linked_name.parts[0] == root:
                        linked_name = Path(*linked_name.parts[1:])
                    linked_target = (temporary_root / linked_name).resolve()
                    if temporary_root.resolve() not in linked_target.parents:
                        raise RuntimeError(f"archive hard link escapes destination: {member.name}")
                    target.parent.mkdir(parents=True, exist_ok=True)
                    os.link(linked_target, target)
                elif member.isdir():
                    target.mkdir(parents=True, exist_ok=True)
                elif member.isfile():
                    target.parent.mkdir(parents=True, exist_ok=True)
                    source = bundle.extractfile(member)
                    if source is None:
                        raise RuntimeError(f"cannot read archive entry: {member.name}")
                    with source, target.open("wb") as output:
                        shutil.copyfileobj(source, output)
                    target.chmod(member.mode & 0o777)
        temporary_root.rename(destination)
    except BaseException:
        shutil.rmtree(temporary_root, ignore_errors=True)
        raise


def safe_component(value: str) -> str:
    return "".join(character if character.isalnum() or character in "._-" else "_" for character in value)


if __name__ == "__main__":
    main()
