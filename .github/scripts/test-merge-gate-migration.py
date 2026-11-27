#!/usr/bin/env python3
"""Exercise trusted registry authority with real Go processes, without compiling the product."""
import hashlib
import importlib.util
import json
from pathlib import Path
import subprocess
import tempfile
import unittest

HERE = Path(__file__).resolve().parent


def load():
    spec = importlib.util.spec_from_file_location("merge_gate_migration", HERE / "merge-gate-migration.py")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


class MigrationTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="merge registry ; $() ")
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.store = self.root / "internal/store"
        self.store.mkdir(parents=True)
        (self.root / "go.mod").write_text("module fixture\n\ngo 1.26.0\n")
        self.registry = self.root / ".github/scripts/merge-gate-migrations.json"
        self.registry.parent.mkdir(parents=True)
        self.authority = self.root / "trusted.json"
        self.sql = [f"migration v{version}" for version in range(1, 49)]
        self.write_registry(self.authority)
        self.write_registry(self.registry)
        self.driver = load()

    def write_registry(self, path):
        path.write_text(json.dumps({"base": "a" * 40, "version_count": len(self.sql),
                                    "sha256": [hashlib.sha256(sql.encode()).hexdigest() for sql in self.sql]}))

    def run_authority(self):
        (self.store / "migrations.go").write_text("package store\nvar migrations = []string{" +
                                                  ",".join(json.dumps(sql) for sql in self.sql) + "}\n")
        (self.store / "authority_test.go").write_text(self.driver.source(self.authority))
        result = subprocess.run(["go", "test", "-count=1", "-json", "-run",
                                 "^TestMergeGatePublishedMigrationAuthority$", "./internal/store"],
                                cwd=self.root, text=True, capture_output=True)
        if result.returncode == 0:
            passes = [json.loads(line).get("Test") for line in result.stdout.splitlines()
                      if json.loads(line).get("Action") == "pass"]
            self.assertIn("TestMergeGatePublishedMigrationAuthority", passes)
        return result

    def test_runtime_append_requires_complete_candidate_registry(self):
        self.sql.append("migration v49")
        result = self.run_authority()
        self.assertNotEqual(result.returncode, 0, "unregistered runtime v49 was accepted")
        self.write_registry(self.registry)
        result = self.run_authority()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_next_base_freezes_v49_even_with_matching_candidate_rewrite(self):
        self.sql.append("migration v49")
        self.write_registry(self.registry)
        result = self.run_authority()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.write_registry(self.authority)
        self.sql[-1] += " rewritten"
        self.write_registry(self.registry)
        result = self.run_authority()
        self.assertNotEqual(result.returncode, 0, "the next base's v49 was rewritten and self-certified")

    def test_candidate_registry_order_digest_and_count_are_exact(self):
        original = self.registry.read_text()
        for kind in ("order", "digest", "count", "extra"):
            with self.subTest(kind=kind):
                value = json.loads(original)
                if kind == "order":
                    value["sha256"][0], value["sha256"][1] = value["sha256"][1], value["sha256"][0]
                elif kind == "digest":
                    value["sha256"][-1] = "0" * 64
                elif kind == "count":
                    value["version_count"] -= 1
                else:
                    value["sha256"].append("0" * 64)
                    value["version_count"] += 1
                self.registry.write_text(json.dumps(value))
                self.assertNotEqual(self.run_authority().returncode, 0, kind)

    def test_trusted_templates_are_exclusive_and_cleaned_on_failure(self):
        target = self.store / "merge_gate_authority_generated_test.go"
        target.write_text("candidate collision")
        with self.assertRaises(FileExistsError):
            with self.driver.generated(self.root, self.authority):
                self.fail("candidate collision was overwritten")
        self.assertEqual(target.read_text(), "candidate collision")
        target.unlink()
        with self.assertRaisesRegex(RuntimeError, "stop"):
            with self.driver.generated(self.root, self.authority):
                self.assertIn("TestMergeGateAllMigrationPrefixesUpgradeReopenReplay",
                              (self.store / "merge_gate_migration_generated_test.go").read_text())
                raise RuntimeError("stop")
        self.assertFalse(target.exists())
        self.assertFalse((self.store / "merge_gate_migration_generated_test.go").exists())


if __name__ == "__main__":
    unittest.main()
