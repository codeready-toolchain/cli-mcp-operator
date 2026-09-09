#!/usr/bin/env python3
"""Wrap an opm-rendered olm.bundle with package + channel so opm validate passes.

opm render of a bundle image only emits olm.bundle. A file-based catalog also
needs olm.package and an olm.channel entry that names that bundle.
"""

from __future__ import annotations

import argparse
import pathlib
import sys

BUNDLE_SCHEMA = "olm.bundle"


def _top_level_fields(doc: str) -> dict[str, str]:
    fields: dict[str, str] = {}
    for line in doc.splitlines():
        if not line or line.startswith(" ") or line.startswith("\t") or line.startswith("#"):
            continue
        if line.strip() == "---":
            continue
        if ":" not in line:
            continue
        key, _, value = line.partition(":")
        fields[key.strip()] = value.strip().strip("\"'")
    return fields


def split_docs(text: str) -> list[str]:
    docs: list[str] = []
    current: list[str] = []
    for line in text.splitlines():
        if line.strip() == "---" and current:
            docs.append("\n".join(current).strip())
            current = []
            continue
        if line.strip() == "---":
            continue
        current.append(line)
    if current:
        docs.append("\n".join(current).strip())
    return [d for d in docs if d]


def bundle_identity(rendered: str) -> tuple[str, str]:
    bundles: list[tuple[str, str]] = []
    for doc in split_docs(rendered):
        fields = _top_level_fields(doc)
        if fields.get("schema") != BUNDLE_SCHEMA:
            continue
        name = fields.get("name", "")
        package = fields.get("package", "")
        if not name or not package:
            raise SystemExit("olm.bundle is missing name or package")
        bundles.append((package, name))
    if len(bundles) != 1:
        raise SystemExit(f"expected exactly 1 olm.bundle, found {len(bundles)}")
    return bundles[0]


def compose_catalog(rendered: str, channel: str) -> str:
    if not rendered.strip():
        raise SystemExit("rendered bundle YAML is empty")
    if not channel.strip() or any(c.isspace() for c in channel):
        raise SystemExit("channel must be a non-empty token")
    package, bundle_name = bundle_identity(rendered)
    body = rendered.strip()
    if not body.startswith("---"):
        body = "---\n" + body
    return (
        f"---\n"
        f"defaultChannel: {channel}\n"
        f"name: {package}\n"
        f"schema: olm.package\n"
        f"---\n"
        f"schema: olm.channel\n"
        f"package: {package}\n"
        f"name: {channel}\n"
        f"entries:\n"
        f"  - name: {bundle_name}\n"
        f"{body}\n"
    )


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "rendered",
        help="opm render YAML file, or - for stdin",
    )
    parser.add_argument("output", help="catalog YAML path to write")
    parser.add_argument(
        "--channel",
        required=True,
        help="default (and only) catalog channel, e.g. alpha",
    )
    args = parser.parse_args()
    if args.rendered == "-":
        text = sys.stdin.read()
    else:
        path = pathlib.Path(args.rendered)
        if not path.exists():
            raise SystemExit(f"{path} not found")
        text = path.read_text()
    out = pathlib.Path(args.output)
    out.parent.mkdir(parents=True, exist_ok=True)
    out.write_text(compose_catalog(text, args.channel))


if __name__ == "__main__":
    main()
