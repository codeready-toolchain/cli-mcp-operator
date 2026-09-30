#!/usr/bin/env python3
"""Stamp the bundle CSV with a per-commit OLM version at catalog publish time.

Same scheme as toolchain-cicd scripts/cd/olm-setup.sh for a repo with no
embedded operator:

  next:     0.0.<commit-count>-commit-<sha>
  replaces: 0.0.<count-1>-commit-<parent-sha>

olm.skipRange '<next' is always set. This catalog is one file-based bundle,
so the previous bundle is not kept in the index, and a merge commit's parent
count is not always count-1. skipRange is what lets an installed v0.0.1 or a
skipped publish upgrade. The committed bundle stays VERSION 0.0.1.
"""

from __future__ import annotations

import pathlib
import re
import subprocess
import sys

CSV = pathlib.Path("bundle/manifests/cli-mcp-operator.clusterserviceversion.yaml")
PACKAGE = "cli-mcp-operator"


def _git(*args: str) -> str:
    out = subprocess.check_output(["git", *args], text=True).strip()
    if not out:
        raise SystemExit(f"git {' '.join(args)} returned nothing")
    return out


def release_versions(commit_count: int, commit: str, previous: str) -> tuple[str, str]:
    if commit_count < 2:
        raise SystemExit("CSV release version needs a parent commit")
    for sha in (commit, previous):
        if re.fullmatch(r"[0-9a-f]{7,}", sha) is None:
            raise SystemExit(f"commit SHA must be at least 7 hex chars, got {sha!r}")
    return (
        f"0.0.{commit_count}-commit-{commit}",
        f"0.0.{commit_count - 1}-commit-{previous}",
    )


def versions_from_git() -> tuple[str, str]:
    count = int(_git("rev-list", "--count", "HEAD"))
    commit = _git("rev-parse", "--short=7", "HEAD")
    previous = _git("rev-parse", "--short=7", "HEAD^")
    return release_versions(count, commit, previous)


def _replace_exactly_one(pattern: str, repl: str, text: str, what: str) -> str:
    new, n = re.subn(pattern, repl, text)
    if n != 1:
        raise SystemExit(f"expected exactly 1 {what}, found {n}")
    return new


def apply_release(text: str, version: str, replaces_version: str) -> str:
    replaces = f"{PACKAGE}.v{replaces_version}"
    text = _replace_exactly_one(
        rf"(?m)^  name: {PACKAGE}\.v\S+$",
        f"  name: {PACKAGE}.v{version}",
        text,
        "CSV name",
    )
    skip = f"    olm.skipRange: '<{version}'"
    if re.search(r"(?m)^    olm\.skipRange: ", text):
        text = _replace_exactly_one(r"(?m)^    olm\.skipRange: .*$", skip, text, "skipRange")
    else:
        text = _replace_exactly_one(
            r"(?m)^  annotations:\n",
            f"  annotations:\n{skip}\n",
            text,
            "metadata annotations",
        )
    version_line = f"  version: {version}"
    if re.search(r"(?m)^  replaces: ", text):
        text = _replace_exactly_one(
            r"(?m)^  replaces: .*$",
            f"  replaces: {replaces}",
            text,
            "replaces",
        )
        return _replace_exactly_one(r"(?m)^  version: \S+$", version_line, text, "version")
    return _replace_exactly_one(
        r"(?m)^  version: \S+$",
        f"  replaces: {replaces}\n{version_line}",
        text,
        "version",
    )


def main() -> None:
    if len(sys.argv) != 2 or sys.argv[1] not in ("print", "apply"):
        raise SystemExit("usage: stamp-csv-release.py print|apply")
    version, replaces_version = versions_from_git()
    if sys.argv[1] == "print":
        print(version)
        return
    if not CSV.exists():
        raise SystemExit(f"{CSV} not found")
    CSV.write_text(apply_release(CSV.read_text(), version, replaces_version))


if __name__ == "__main__":
    main()
