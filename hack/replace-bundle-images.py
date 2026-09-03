#!/usr/bin/env python3
"""Replace REPLACE_* placeholders in the bundle CSV with concrete image refs."""

from __future__ import annotations

import datetime
import pathlib
import sys

CSV = pathlib.Path("bundle/manifests/cli-mcp-operator.clusterserviceversion.yaml")

PLACEHOLDERS = (
    "REPLACE_OPERATOR_IMAGE",
    "REPLACE_SERVER_IMAGE",
    "REPLACE_SANDBOX_IMAGE",
    "REPLACE_KUBE_RBAC_PROXY_IMAGE",
    "REPLACE_CREATED_AT",
)


def utc_created_at(now: datetime.datetime | None = None) -> str:
    stamp = now or datetime.datetime.now(datetime.timezone.utc)
    return stamp.strftime("%Y-%m-%dT%H:%M:%SZ")


def apply_replacements(
    text: str,
    operator: str,
    server: str,
    sandbox: str,
    proxy: str,
    created_at: str,
) -> str:
    repl = {
        "REPLACE_OPERATOR_IMAGE": operator,
        "REPLACE_SERVER_IMAGE": server,
        "REPLACE_SANDBOX_IMAGE": sandbox,
        "REPLACE_KUBE_RBAC_PROXY_IMAGE": proxy,
        "REPLACE_CREATED_AT": created_at,
    }
    for placeholder, value in repl.items():
        if placeholder not in text:
            raise SystemExit(f"bundle CSV missing placeholder {placeholder}")
        text = text.replace(placeholder, value)
    if "REPLACE_" in text:
        raise SystemExit("bundle CSV still contains REPLACE_ placeholders")
    return text


def main() -> None:
    if len(sys.argv) not in (5, 6):
        raise SystemExit(
            "usage: replace-bundle-images.py OPERATOR_IMG SERVER_IMG SANDBOX_IMG "
            "KUBE_RBAC_PROXY_IMG [CREATED_AT]"
        )
    operator, server, sandbox, proxy = sys.argv[1:5]
    created_at = sys.argv[5] if len(sys.argv) == 6 else utc_created_at()
    if not CSV.exists():
        raise SystemExit(f"{CSV} not found")
    CSV.write_text(
        apply_replacements(CSV.read_text(), operator, server, sandbox, proxy, created_at)
    )


if __name__ == "__main__":
    main()
