#!/usr/bin/env python3
"""Per-commit CSV version and skipRange for catalog publish."""

from __future__ import annotations

import importlib.util
import pathlib
import unittest

ROOT = pathlib.Path(__file__).resolve().parent
SPEC = importlib.util.spec_from_file_location("stamp_csv_release", ROOT / "stamp-csv-release.py")
mod = importlib.util.module_from_spec(SPEC)
assert SPEC.loader is not None
SPEC.loader.exec_module(mod)

SAMPLE = """\
metadata:
  annotations:
    capabilities: Basic Install
  name: cli-mcp-operator.v0.0.1
spec:
  version: 0.0.1
"""


class StampCsvReleaseTest(unittest.TestCase):
    def test_version_matches_host_operator_scheme(self) -> None:
        self.assertEqual(mod.release_version(42, "abcdef1"), "0.0.42-commit-abcdef1")

    def test_stamps_name_skiprange_and_is_idempotent(self) -> None:
        version = "0.0.42-commit-abcdef1"
        got = mod.apply_release(SAMPLE, version)
        self.assertIn(f"\n  name: cli-mcp-operator.v{version}\n", got)
        self.assertIn(f"\n    olm.skipRange: '>=0.0.0 <{version}'\n", got)
        self.assertNotIn("replaces:", got)
        self.assertIn(f"\n  version: {version}\n", got)
        self.assertEqual(mod.apply_release(got, version), got)

    def test_leaves_example_instance_name(self) -> None:
        sample = SAMPLE.replace(
            "    capabilities: Basic Install\n",
            '    capabilities: Basic Install\n            "name": "oc"\n',
        )
        got = mod.apply_release(sample, "0.0.2-commit-abcdef1")
        self.assertIn('"name": "oc"', got)
        self.assertEqual(got.count("cli-mcp-operator.v0.0.2-commit-abcdef1"), 1)


if __name__ == "__main__":
    unittest.main()
