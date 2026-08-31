#!/usr/bin/env python3
"""Stamp committed bundle CSV with REPLACE_* placeholders (stable for git)."""

from __future__ import annotations

import pathlib
import re
import sys

CSV = pathlib.Path("bundle/manifests/cli-mcp-operator.clusterserviceversion.yaml")

ENV_PLACEHOLDERS = {
    "RELATED_IMAGE_SERVER": "REPLACE_SERVER_IMAGE",
    "RELATED_IMAGE_SANDBOX": "REPLACE_SANDBOX_IMAGE",
    "RELATED_IMAGE_KUBE_RBAC_PROXY": "REPLACE_KUBE_RBAC_PROXY_IMAGE",
}

RELATED_IMAGES = """  relatedImages:
  - image: REPLACE_OPERATOR_IMAGE
    name: manager
  - image: REPLACE_SERVER_IMAGE
    name: server
  - image: REPLACE_SANDBOX_IMAGE
    name: sandbox
  - image: REPLACE_KUBE_RBAC_PROXY_IMAGE
    name: kube-rbac-proxy
"""


def stamp(text: str) -> str:
    text = re.sub(r"(?m)^    createdAt: .*$", '    createdAt: "REPLACE_CREATED_AT"', text)

    def replace_env_value(match: re.Match[str]) -> str:
        name = match.group(1)
        placeholder = ENV_PLACEHOLDERS[name]
        return f"                - name: {name}\n                  value: {placeholder}"

    text = re.sub(
        r"                - name: (RELATED_IMAGE_SERVER|RELATED_IMAGE_SANDBOX|RELATED_IMAGE_KUBE_RBAC_PROXY)\n                  value: .+",
        replace_env_value,
        text,
    )

    # Manager container image (command: /manager block).
    text = re.sub(
        r"(command:\n(?:                - /manager\n)(?:                [^\n]+\n)*?                image: )[^\n]+",
        r"\1REPLACE_OPERATOR_IMAGE",
        text,
        count=1,
    )
    # Fallback if command/image order differs.
    if "image: REPLACE_OPERATOR_IMAGE" not in text:
        text = re.sub(
            r"(?m)^                image: (?!REPLACE_OPERATOR_IMAGE).+$",
            "                image: REPLACE_OPERATOR_IMAGE",
            text,
            count=1,
        )

    text = re.sub(
        r"(?m)^  relatedImages:\n(?:  - image: .*\n    name: .*\n)+",
        RELATED_IMAGES,
        text,
    )
    if "relatedImages:" not in text:
        text = re.sub(r"(?m)^  version: ", RELATED_IMAGES + "  version: ", text, count=1)

    leftovers = []
    for needle in (
        "quay.io/codeready-toolchain/cli-mcp-operator:",
        "quay.io/codeready-toolchain/cli-mcp-server:",
        "quay.io/codeready-toolchain/cli-mcp-sandbox:",
        "quay.io/brancz/kube-rbac-proxy:",
        "cli-mcp-operator:latest",
    ):
        if needle in text:
            leftovers.append(needle)
    if leftovers:
        raise SystemExit(f"bundle CSV still contains concrete image refs: {leftovers}")
    for placeholder in (
        "REPLACE_OPERATOR_IMAGE",
        "REPLACE_SERVER_IMAGE",
        "REPLACE_SANDBOX_IMAGE",
        "REPLACE_KUBE_RBAC_PROXY_IMAGE",
        "REPLACE_CREATED_AT",
    ):
        if placeholder not in text:
            raise SystemExit(f"bundle CSV missing placeholder {placeholder}")
    return text


def main() -> None:
    if not CSV.exists():
        raise SystemExit(f"{CSV} not found")
    CSV.write_text(stamp(CSV.read_text()))


if __name__ == "__main__":
    main()
    sys.exit(0)
