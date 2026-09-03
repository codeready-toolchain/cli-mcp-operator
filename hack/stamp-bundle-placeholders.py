#!/usr/bin/env python3
"""Stamp committed bundle CSV with REPLACE_* placeholders (stable for git)."""

from __future__ import annotations

import pathlib
import re

CSV = pathlib.Path("bundle/manifests/cli-mcp-operator.clusterserviceversion.yaml")

ENV_PLACEHOLDERS = {
    "RELATED_IMAGE_SERVER": "REPLACE_SERVER_IMAGE",
    "RELATED_IMAGE_SANDBOX": "REPLACE_SANDBOX_IMAGE",
    "RELATED_IMAGE_KUBE_RBAC_PROXY": "REPLACE_KUBE_RBAC_PROXY_IMAGE",
}

PLACEHOLDERS = (
    "REPLACE_OPERATOR_IMAGE",
    "REPLACE_SERVER_IMAGE",
    "REPLACE_SANDBOX_IMAGE",
    "REPLACE_KUBE_RBAC_PROXY_IMAGE",
    "REPLACE_CREATED_AT",
)

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


def _replace_exactly_one(pattern: str, repl: str, text: str, what: str) -> str:
    new, n = re.subn(pattern, repl, text)
    if n != 1:
        raise SystemExit(f"expected exactly 1 {what}, found {n}")
    return new


def stamp(text: str) -> str:
    text = _replace_exactly_one(
        r"(?m)^    createdAt: .*$",
        '    createdAt: "REPLACE_CREATED_AT"',
        text,
        "createdAt",
    )

    for name, placeholder in ENV_PLACEHOLDERS.items():
        text = _replace_exactly_one(
            rf"(                - name: {name}\n                  value: ).+",
            rf"\1{placeholder}",
            text,
            name,
        )

    # Operator CSV has one manager container; 16-space indent is that image.
    # relatedImages uses "  - image:" (2 spaces) and is not matched here.
    text = _replace_exactly_one(
        r"(?m)^                image: .+$",
        "                image: REPLACE_OPERATOR_IMAGE",
        text,
        "manager image",
    )

    text = _replace_exactly_one(
        r"(?m)^  relatedImages:\n(?:  - image: .*\n    name: .*\n)+",
        RELATED_IMAGES,
        text,
        "relatedImages",
    )

    for placeholder in PLACEHOLDERS:
        if placeholder not in text:
            raise SystemExit(f"bundle CSV missing placeholder {placeholder}")
    return text


def main() -> None:
    if not CSV.exists():
        raise SystemExit(f"{CSV} not found")
    CSV.write_text(stamp(CSV.read_text()))


if __name__ == "__main__":
    main()
