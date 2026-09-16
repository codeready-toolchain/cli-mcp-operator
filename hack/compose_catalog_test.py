#!/usr/bin/env python3
"""Regression: catalog FBC must include package + channel, not only the bundle."""

from __future__ import annotations

import importlib.util
import pathlib
import unittest

ROOT = pathlib.Path(__file__).resolve().parent
SPEC = importlib.util.spec_from_file_location("compose_catalog", ROOT / "compose-catalog.py")
mod = importlib.util.module_from_spec(SPEC)
assert SPEC.loader is not None
SPEC.loader.exec_module(mod)

RENDERED = """\
---
image: quay.io/codeready-toolchain/cli-mcp-operator-bundle:b788df3
name: cli-mcp-operator.v0.0.1
package: cli-mcp-operator
properties:
  - type: olm.package
    value:
      packageName: cli-mcp-operator
      version: 0.0.1
schema: olm.bundle
"""


class ComposeCatalogTest(unittest.TestCase):
    def test_adds_package_and_channel_around_bundle(self) -> None:
        got = mod.compose_catalog(RENDERED, "alpha")
        self.assertIn("schema: olm.package", got)
        self.assertIn("defaultChannel: alpha", got)
        self.assertIn("name: cli-mcp-operator", got)
        self.assertIn("schema: olm.channel", got)
        self.assertIn("package: cli-mcp-operator", got)
        self.assertIn("name: alpha", got)
        self.assertIn("  - name: cli-mcp-operator.v0.0.1", got)
        self.assertIn("schema: olm.bundle", got)
        self.assertIn(
            "image: quay.io/codeready-toolchain/cli-mcp-operator-bundle:b788df3",
            got,
        )

    def test_reads_bundle_name_from_render_not_hardcoded_version(self) -> None:
        rendered = RENDERED.replace("cli-mcp-operator.v0.0.1", "cli-mcp-operator.v1.2.3")
        got = mod.compose_catalog(rendered, "alpha")
        self.assertIn("  - name: cli-mcp-operator.v1.2.3", got)
        self.assertNotIn("cli-mcp-operator.v0.0.1", got)

    def test_aborts_on_bundle_only_missing_package_fields(self) -> None:
        with self.assertRaises(SystemExit) as ctx:
            mod.compose_catalog("schema: olm.bundle\nname: x\n", "alpha")
        self.assertIn("name or package", str(ctx.exception))

    def test_aborts_when_render_has_no_bundle(self) -> None:
        with self.assertRaises(SystemExit) as ctx:
            mod.compose_catalog("schema: olm.package\nname: cli-mcp-operator\n", "alpha")
        self.assertIn("exactly 1 olm.bundle", str(ctx.exception))

    def test_aborts_on_empty_render(self) -> None:
        with self.assertRaises(SystemExit) as ctx:
            mod.compose_catalog(" \n", "alpha")
        self.assertIn("empty", str(ctx.exception))

    def test_aborts_on_blank_channel(self) -> None:
        with self.assertRaises(SystemExit) as ctx:
            mod.compose_catalog(RENDERED, " ")
        self.assertIn("channel", str(ctx.exception))


if __name__ == "__main__":
    unittest.main()
