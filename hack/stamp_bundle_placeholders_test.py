#!/usr/bin/env python3
"""Fail-closed stamp: one match per field, no regex fallbacks."""

from __future__ import annotations

import importlib.util
import pathlib
import unittest

ROOT = pathlib.Path(__file__).resolve().parent
SPEC = importlib.util.spec_from_file_location(
    "stamp_bundle_placeholders", ROOT / "stamp-bundle-placeholders.py"
)
mod = importlib.util.module_from_spec(SPEC)
assert SPEC.loader is not None
SPEC.loader.exec_module(mod)


GENERATED = """\
metadata:
  annotations:
    createdAt: "2026-08-31T12:00:00Z"
            spec:
              containers:
              - command:
                - /manager
                env:
                - name: RELATED_IMAGE_SERVER
                  value: quay.io/codeready-toolchain/cli-mcp-server:latest
                - name: RELATED_IMAGE_SANDBOX
                  value: quay.io/codeready-toolchain/cli-mcp-sandbox:latest
                - name: RELATED_IMAGE_KUBE_RBAC_PROXY
                  value: quay.io/brancz/kube-rbac-proxy:v0.19.1
                image: cli-mcp-operator:latest
                name: manager
  relatedImages:
  - image: REPLACE_OPERATOR_IMAGE
    name: manager
  - image: REPLACE_SERVER_IMAGE
    name: server
  - image: REPLACE_SANDBOX_IMAGE
    name: sandbox
  - image: REPLACE_KUBE_RBAC_PROXY_IMAGE
    name: kube-rbac-proxy
  version: 0.0.1
"""


class StampBundlePlaceholdersTest(unittest.TestCase):
    def test_committed_csv_is_idempotent(self) -> None:
        csv = ROOT.parent / "bundle/manifests/cli-mcp-operator.clusterserviceversion.yaml"
        text = csv.read_text()
        self.assertEqual(mod.stamp(text), text)

    def test_stamps_created_at_env_manager_image(self) -> None:
        got = mod.stamp(GENERATED)
        self.assertIn('createdAt: "REPLACE_CREATED_AT"', got)
        self.assertIn("value: REPLACE_SERVER_IMAGE", got)
        self.assertIn("value: REPLACE_SANDBOX_IMAGE", got)
        self.assertIn("value: REPLACE_KUBE_RBAC_PROXY_IMAGE", got)
        self.assertIn("image: REPLACE_OPERATOR_IMAGE", got)
        self.assertNotIn("cli-mcp-operator:latest", got)
        self.assertNotIn("quay.io/brancz/kube-rbac-proxy:", got)

    def test_idempotent_on_already_stamped_csv(self) -> None:
        stamped = mod.stamp(GENERATED)
        self.assertEqual(mod.stamp(stamped), stamped)

    def test_aborts_if_created_at_missing(self) -> None:
        with self.assertRaises(SystemExit) as ctx:
            mod.stamp(GENERATED.replace("    createdAt: ", "    other: "))
        self.assertIn("createdAt", str(ctx.exception))

    def test_aborts_if_related_image_env_missing(self) -> None:
        with self.assertRaises(SystemExit) as ctx:
            mod.stamp(GENERATED.replace("RELATED_IMAGE_SERVER", "RELATED_IMAGE_OTHER"))
        self.assertIn("RELATED_IMAGE_SERVER", str(ctx.exception))

    def test_aborts_if_two_manager_image_lines(self) -> None:
        extra = GENERATED.replace(
            "                image: cli-mcp-operator:latest\n",
            "                image: cli-mcp-operator:latest\n"
            "                image: extra:latest\n",
        )
        with self.assertRaises(SystemExit) as ctx:
            mod.stamp(extra)
        self.assertIn("manager image", str(ctx.exception))

    def test_aborts_if_related_images_block_missing(self) -> None:
        stripped = GENERATED
        stripped = stripped.replace(
            "  relatedImages:\n"
            "  - image: REPLACE_OPERATOR_IMAGE\n"
            "    name: manager\n"
            "  - image: REPLACE_SERVER_IMAGE\n"
            "    name: server\n"
            "  - image: REPLACE_SANDBOX_IMAGE\n"
            "    name: sandbox\n"
            "  - image: REPLACE_KUBE_RBAC_PROXY_IMAGE\n"
            "    name: kube-rbac-proxy\n",
            "",
        )
        with self.assertRaises(SystemExit) as ctx:
            mod.stamp(stripped)
        self.assertIn("relatedImages", str(ctx.exception))


if __name__ == "__main__":
    unittest.main()
