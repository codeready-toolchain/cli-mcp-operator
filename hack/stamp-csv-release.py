#!/usr/bin/env python3
"""Stamp the bundle CSV with a per-commit OLM version at catalog publish time.

Same version scheme as toolchain-cicd scripts/cd/olm-setup.sh for a repo with
no embedded operator:

  0.0.<commit-count>-commit-<sha>

The catalog is one file-based bundle, so the previous bundle is not kept.
Upgrades come from olm.channel skipRange (hack/compose-catalog.py), using
>=0.0.0 <version. The CSV annotation records the same range. The committed
bundle stays VERSION 0.0.1.
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


def release_version(commit_count: int, commit: str) -> str:
    if re.fullmatch(r"[0-9a-f]{7,}", commit) is None:
        raise SystemExit(f"commit SHA must be at least 7 hex chars, got {commit!r}")
    return f"0.0.{commit_count}-commit-{commit}"


def version_from_git() -> str:
    count = int(_git("rev-list", "--count", "HEAD"))
    commit = _git("rev-parse", "--short=7", "HEAD")
    return release_version(count, commit)


def skip_range(version: str) -> str:
    return f">=0.0.0 <{version}"


def _replace_exactly_one(pattern: str, repl: str, text: str, what: str) -> str:
    new, n = re.subn(pattern, repl, text)
    if n != 1:
        raise SystemExit(f"expected exactly 1 {what}, found {n}")
    return new


def apply_release(text: str, version: str) -> str:
    text = _replace_exactly_one(
        rf"(?m)^  name: {PACKAGE}\.v\S+$",
        f"  name: {PACKAGE}.v{version}",
        text,
        "CSV name",
    )
    skip = f"    olm.skipRange: '{skip_range(version)}'"
    if re.search(r"(?m)^    olm\.skipRange: ", text):
        text = _replace_exactly_one(r"(?m)^    olm\.skipRange: .*$", skip, text, "skipRange")
    else:
        text = _replace_exactly_one(
            r"(?m)^  annotations:\n",
            f"  annotations:\n{skip}\n",
            text,
            "metadata annotations",
        )
    return _replace_exactly_one(
        r"(?m)^  version: \S+$",
        f"  version: {version}",
        text,
        "version",
    )


def main() -> None:
    if len(sys.argv) != 2 or sys.argv[1] not in ("print", "apply"):
        raise SystemExit("usage: stamp-csv-release.py print|apply")
    version = version_from_git()
    if sys.argv[1] == "print":
        print(version)
        return
    if not CSV.exists():
        raise SystemExit(f"{CSV} not found")
    CSV.write_text(apply_release(CSV.read_text(), version))


if __name__ == "__main__":
    main()
