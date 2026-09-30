#!/usr/bin/env python3
"""Per-commit CSV version, replaces, and skipRange for catalog publish."""

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
        nxt, prev = mod.release_versions(42, "abcdef1", "1234567")
        self.assertEqual(nxt, "0.0.42-commit-abcdef1")
        self.assertEqual(prev, "0.0.41-commit-1234567")

    def test_stamps_name_replaces_skiprange_and_is_idempotent(self) -> None:
        got = mod.apply_release(SAMPLE, "0.0.42-commit-abcdef1", "0.0.41-commit-1234567")
        self.assertIn("\n  name: cli-mcp-operator.v0.0.42-commit-abcdef1\n", got)
        self.assertIn("\n    olm.skipRange: '<0.0.42-commit-abcdef1'\n", got)
        self.assertIn("\n  replaces: cli-mcp-operator.v0.0.41-commit-1234567\n", got)
        self.assertIn("\n  version: 0.0.42-commit-abcdef1\n", got)
        self.assertEqual(
            mod.apply_release(got, "0.0.42-commit-abcdef1", "0.0.41-commit-1234567"),
            got,
        )

    def test_leaves_example_instance_name(self) -> None:
        sample = SAMPLE.replace(
            "    capabilities: Basic Install\n",
            '    capabilities: Basic Install\n            "name": "oc"\n',
        )
        got = mod.apply_release(sample, "0.0.2-commit-abcdef1", "0.0.1-commit-1234567")
        self.assertIn('"name": "oc"', got)
        self.assertEqual(got.count("cli-mcp-operator.v0.0.2-commit-abcdef1"), 1)

    def test_rejects_first_commit(self) -> None:
        with self.assertRaises(SystemExit):
            mod.release_versions(1, "abcdef1", "1234567")


if __name__ == "__main__":
    unittest.main()
