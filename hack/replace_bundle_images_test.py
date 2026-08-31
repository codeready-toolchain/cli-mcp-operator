#!/usr/bin/env python3
"""Regression: catalog stamp must replace REPLACE_CREATED_AT, not abort on it."""

from __future__ import annotations

import importlib.util
import pathlib
import unittest

ROOT = pathlib.Path(__file__).resolve().parent
SPEC = importlib.util.spec_from_file_location(
    "replace_bundle_images", ROOT / "replace-bundle-images.py"
)
mod = importlib.util.module_from_spec(SPEC)
assert SPEC.loader is not None
SPEC.loader.exec_module(mod)


SAMPLE = """\
metadata:
  annotations:
    createdAt: "REPLACE_CREATED_AT"
                - name: RELATED_IMAGE_SERVER
                  value: REPLACE_SERVER_IMAGE
                image: REPLACE_OPERATOR_IMAGE
  relatedImages:
  - image: REPLACE_OPERATOR_IMAGE
    name: manager
  - image: REPLACE_SERVER_IMAGE
    name: server
  - image: REPLACE_SANDBOX_IMAGE
    name: sandbox
  - image: REPLACE_KUBE_RBAC_PROXY_IMAGE
    name: kube-rbac-proxy
"""


class ReplaceBundleImagesTest(unittest.TestCase):
    def test_replaces_images_and_created_at(self) -> None:
        got = mod.apply_replacements(
            SAMPLE,
            "quay.io/example/operator:abc",
            "quay.io/example/server:abc",
            "quay.io/example/sandbox:abc",
            "quay.io/brancz/kube-rbac-proxy:v0.19.1",
            "2026-08-26T12:00:00Z",
        )
        self.assertIn('createdAt: "2026-08-26T12:00:00Z"', got)
        self.assertIn("quay.io/example/operator:abc", got)
        self.assertNotIn("REPLACE_", got)

    def test_aborts_if_created_at_placeholder_missing(self) -> None:
        with self.assertRaises(SystemExit) as ctx:
            mod.apply_replacements(
                SAMPLE.replace("REPLACE_CREATED_AT", "already-set"),
                "op",
                "srv",
                "sbx",
                "proxy",
                "2026-08-26T12:00:00Z",
            )
        self.assertIn("REPLACE_CREATED_AT", str(ctx.exception))


if __name__ == "__main__":
    unittest.main()
