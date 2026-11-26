#!/usr/bin/env python3
"""Behavioral tests; temporary Git/Go repositories never publish anything."""
import copy
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

HERE = Path(__file__).resolve().parent


def load(name):
    path = HERE / (name + ".py")
    if not path.exists():
        return None
    spec = importlib.util.spec_from_file_location(name.replace("-", "_"), path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


class GateTests(unittest.TestCase):
    def setUp(self):
        self.gate = load("merge-gate")
        self.assertIsNotNone(self.gate, "缺少原生合并身份、依赖选择和终态证据实现")
        self.temporary = tempfile.TemporaryDirectory(prefix="merge gate ; $() ")
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.git("init", "-q")
        self.git("config", "user.name", "Fixture")
        self.git("config", "user.email", "fixture@example.invalid")
        self.put("go.mod", "module fixture\n\ngo 1.26.0\n")
        for name in ("a", "b", "c", "z", "q"):
            self.put(name + "/code.go", "package " + name + "\n")
        self.put("b/code.go", 'package b\nimport _ "fixture/a"\n')
        self.put("c/code.go", 'package c\nimport _ "fixture/b"\n')
        self.put("z/code_test.go", 'package z_test\nimport _ "fixture/a"\nimport "testing"\nfunc TestZ(t *testing.T) {}\n')
        self.put("q/race_test.go", '//go:build race\n\npackage q\nimport _ "fixture/a"\nimport "testing"\nfunc TestRaceEdge(t *testing.T) {}\n')
        self.base = self.commit()
        self.put("a/code.go", "package a\nconst Changed = true\n")
        self.head = self.commit()
        tree = self.git("rev-parse", "HEAD^{tree}")
        self.sha = self.git("commit-tree", tree, "-p", self.base, "-p", self.head, input="merge\n")
        self.git("reset", "--hard", self.sha)
        self.context = {"GITHUB_REPOSITORY": "PeterFile/ct0x", "GITHUB_SHA": self.sha,
                        "GITHUB_RUN_ID": "101", "GITHUB_RUN_ATTEMPT": "2", "GITHUB_EVENT_NAME": "pull_request"}
        self.event = {"number": 7, "pull_request": {"number": 7, "head": {"sha": self.head, "ref": "topic", "repo": {"id": 17, "full_name": "contributor/ct0x"}},
                      "base": {"sha": "f" * 40, "ref": "master", "repo": {"full_name": "PeterFile/ct0x"}}}}

    def git(self, *args, input=None):
        return subprocess.run(["git", "-C", str(self.root), *args], input=input, text=True,
                              check=True, capture_output=True).stdout.strip()

    def put(self, path, text):
        target = self.root / path
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_text(text)

    def commit(self):
        self.git("add", "--all")
        self.git("commit", "-qm", "fixture")
        return self.git("rev-parse", "HEAD")

    def selection(self, rules=None):
        return self.gate.select(self.root, self.base, self.sha, {"rules": rules or []})

    def test_native_identity_uses_true_parents_not_stale_payload_base(self):
        identity = self.gate.native_identity(self.root, self.event, self.context)
        self.assertEqual(identity["base"], self.base)
        self.assertEqual(identity["head"], self.head)
        self.assertEqual(identity["tree"], self.git("rev-parse", "HEAD^{tree}"))
        self.assertEqual(identity["run_attempt"], "2")

    def test_wrong_head_and_single_parent_never_validate(self):
        event = copy.deepcopy(self.event)
        event["pull_request"]["head"]["sha"] = self.base
        with self.assertRaises(ValueError):
            self.gate.native_identity(self.root, event, self.context)
        self.git("reset", "--hard", self.head)
        with self.assertRaises(ValueError):
            self.gate.native_identity(self.root, self.event, {**self.context, "GITHUB_SHA": self.head})

    def test_transitive_test_only_and_race_only_reverse_edges(self):
        selected = self.selection()
        self.assertEqual(selected["packages"], ["fixture/" + name for name in ("a", "b", "c", "q", "z")])
        self.assertFalse(selected["web"])
        self.assertFalse(selected["ci"])

    def test_race_only_edge_executes_real_selected_race_test(self):
        selected = self.selection()
        units = self.gate.tasks(selected)
        race = [unit for unit in units if unit.get("race") and unit.get("package") == "fixture/q"]
        self.assertTrue(race, "race-only 反向依赖不能只跑 ordinary")
        detail = self.gate.run_go(self.root, race[0])
        self.assertEqual(detail["passes"]["TestRaceEdge"], 1)

    def test_package_reached_in_both_modes_still_executes_extra_race_behavior(self):
        self.put("q/code.go", 'package q\nimport _ "fixture/a"\n')
        self.sha = self.commit()
        units = self.gate.tasks(self.selection())
        race = [unit for unit in units if unit.get("race") and unit.get("package") == "fixture/q"]
        self.assertTrue(race, "共同可达包仍须覆盖不同 race 编译文件")
        self.assertEqual(self.gate.run_go(self.root, race[0])["passes"]["TestRaceEdge"], 1)

    def test_deleted_embedded_markdown_cannot_be_docs_only(self):
        self.put("root.go", 'package fixture\nimport _ "embed"\n//go:embed docs/*.md\nvar docs embed.FS\n')
        # Use a named embed import for an actual compilable package.
        self.put("root.go", 'package fixture\nimport "embed"\n//go:embed docs/*.md\nvar docs embed.FS\n')
        self.put("docs/a.md", "a\n")
        self.put("docs/b.md", "b\n")
        self.put("z/code.go", 'package z\nimport _ "fixture"\n')
        self.base = self.commit()
        self.git("rm", "docs/a.md")
        self.sha = self.commit()
        selected = self.selection()
        self.assertIn("fixture", selected["packages"])
        self.assertIn("fixture/z", selected["packages"])

    def test_inspection_requires_actual_checkout_and_any_native_base_is_valid(self):
        with self.assertRaises(ValueError):
            self.gate.select(self.root, self.base, self.head, {"rules": []})
        event = copy.deepcopy(self.event)
        event["pull_request"]["base"]["ref"] = "stacked/topic"
        self.assertEqual(self.gate.native_identity(self.root, event, self.context)["base"], self.base)

    def test_ordinary_opt_in_skip_is_recorded_but_mandatory_skip_rejected(self):
        self.put("z/code_test.go", 'package z\nimport "testing"\nfunc TestOptIn(t *testing.T) { t.Skip("live provider is opt-in") }\nfunc TestRequired(t *testing.T) {}\n')
        task = {"package": "fixture/z", "race": False, "names": None, "required": ["TestRequired"], "worker": None, "profile": None}
        detail = self.gate.run_go(self.root, task)
        self.assertEqual(detail["skips"], {"TestOptIn": 1})
        self.assertEqual(detail["passes"], {"TestRequired": 1})
        task["required"] = ["TestOptIn"]
        with self.assertRaises(ValueError):
            self.gate.run_go(self.root, task)

    def test_copylocks_vet_is_a_real_required_task(self):
        self.put("a/code.go", 'package a\nimport "sync"\nfunc Copy(mu sync.Mutex) {}\n')
        self.sha = self.commit()
        tasks = self.gate.tasks(self.selection())
        self.assertTrue(any(task["kind"] == "vet" for task in tasks))
        with self.assertRaises(subprocess.CalledProcessError):
            self.gate.run_vet(self.root, ["fixture/a"])

    def test_deleted_package_and_unknown_root_broaden(self):
        self.git("rm", "a/code.go")
        self.sha = self.commit()
        # Broken imports must fail closed, not return a partial graph.
        with self.assertRaises((ValueError, subprocess.CalledProcessError)):
            self.selection()
        self.put("a/code.go", "package a\n")
        self.put("unknown ; $(touch nope)", "data\n")
        self.sha = self.commit()
        selected = self.selection()
        self.assertTrue(selected["conservative"])
        self.assertTrue(selected["web"] and selected["bridge"] and selected["ci"])
        self.assertFalse((self.root / "nope").exists())

    def test_deleted_unknown_fixture_and_nested_package_paths(self):
        self.put("a/testdata/white space;$x", "old\n")
        self.base = self.commit()
        self.git("rm", "a/testdata/white space;$x")
        self.sha = self.commit()
        self.assertIn("fixture/z", self.selection()["packages"])
        self.assertFalse(self.selection()["conservative"])

    def test_opaque_markdown_only_and_ci_have_no_go_test_charge(self):
        for path in ("docs/design.md", ".github/scripts/merge-gate-new.py"):
            with self.subTest(path=path):
                self.git("reset", "--hard", self.base)
                self.put(path, "text\n")
                self.sha = self.commit()
                selected = self.selection()
                self.assertEqual(selected["packages"], [])
                self.assertEqual(selected["ci"], path.startswith(".github/"))
        self.put("docs/data.bin", "opaque\n")
        self.sha = self.commit()
        self.assertTrue(self.selection()["conservative"])

    def test_module_root_and_embed_are_not_docs_only(self):
        self.put("root.go", 'package fixture\nimport _ "embed"\n//go:embed docs/*.md\nvar doc string\n')
        self.put("docs/embed.md", "before\n")
        self.base = self.commit()
        self.put("docs/embed.md", "after\n")
        self.sha = self.commit()
        self.assertIn("fixture", self.selection()["packages"])

    def test_missing_mandatory_name_fails_closed_and_remains_inspectable(self):
        rule = {"paths": ["a/"], "tests": {"fixture/z": ["TestMissingSafety"]}}
        selected = self.selection([rule])
        self.assertEqual(selected["unresolved"], ["fixture/z:TestMissingSafety"])
        with self.assertRaises(ValueError):
            self.gate.tasks(selected)

    def test_required_race_names_are_explicit_and_source_verified(self):
        rule = {"paths": ["a/"], "tests": {"fixture/q": ["TestRaceEdge"]}}
        selected = self.selection([rule])
        self.assertEqual(selected["race"]["fixture/q"], ["TestRaceEdge"])
        self.assertEqual(selected["unresolved"], [])

    def test_final_requires_exact_success_and_identity_for_every_unit(self):
        identity = self.gate.native_identity(self.root, self.event, self.context)
        plan = {**identity, "tasks": [{"unit": "ordinary-a", "kind": "identity"}]}
        evidence = [{**identity, "unit": "ordinary-a", "result": "success"}]
        results = {"plan": {"result": "success"}, "tests": {"result": "success"}}
        self.gate.finalize(plan, results, evidence)
        for state in ("skipped", "cancelled", "failure", None):
            with self.subTest(state=state), self.assertRaises(ValueError):
                self.gate.finalize(plan, {**results, "tests": {"result": state}}, evidence)
        for field in ("repo", "pr", "head", "base", "sha", "tree", "run_id", "run_attempt"):
            bad = copy.deepcopy(evidence)
            bad[0][field] = "wrong"
            with self.subTest(field=field), self.assertRaises(ValueError):
                self.gate.finalize(plan, results, bad)
        with self.assertRaises(ValueError):
            self.gate.finalize(plan, results, [])

    def test_empty_diff_has_substantive_identity_unit(self):
        self.git("reset", "--hard", self.base)
        tree = self.git("rev-parse", "HEAD^{tree}")
        sha = self.git("commit-tree", tree, "-p", self.base, "-p", self.head, input="empty merge\n")
        self.git("reset", "--hard", sha)
        selected = self.gate.select(self.root, self.base, sha, {"rules": []})
        self.assertEqual(self.gate.tasks(selected), [{"unit": "identity-diff", "kind": "identity"}])

    def test_repository_manifest_keeps_ordinary_liveness_and_upgrade_matrix(self):
        manifest = self.gate.read_json(HERE / "merge-gate-manifest.json")
        self.assertTrue(self.gate.matches("internal/store/delivery_artifact_read_grant.sql", "internal/store/*.sql"))
        self.assertTrue(any("TestCoordinationCompositionSchedulerRoleReceipts" in names
                            for rule in manifest["rules"] for names in rule.get("ordinary", {}).values()))
        source = HERE.parents[1] / "internal/store/merge_gate_migration_test.go"
        self.assertTrue(source.exists(), "缺少各历史迁移前缀的真实磁盘升级/reopen/replay 测试")

    def test_compile_only_package_and_real_test_execution(self):
        self.gate.run_go(self.root, {"package": "fixture/a", "race": False, "names": None,
                                     "worker": None, "profile": None})
        self.gate.run_go(self.root, {"package": "fixture/z", "race": False, "names": ["TestZ"],
                                     "worker": None, "profile": None})

    def test_base_controller_binds_dispatch_to_exact_candidate_and_policy_sha(self):
        event = {"inputs": {"pr": "7", "head": self.head, "base": self.base, "sha": self.sha,
                             "tree": self.git("rev-parse", "HEAD^{tree}"), "policy_sha": self.base,
                             "source_repo_id": "17", "source_repo": "contributor/ct0x", "source_ref": "topic", "base_ref": "master"}}
        context = {**self.context, "GITHUB_EVENT_NAME": "workflow_dispatch", "GITHUB_SHA": self.base,
                   "GITHUB_WORKFLOW_SHA": self.base}
        identity = self.gate.native_identity(self.root, event, context)
        self.assertEqual(identity["sha"], self.sha)
        event["inputs"]["sha"] = self.head
        with self.assertRaises(ValueError):
            self.gate.native_identity(self.root, event, context)

    def test_ci_discovery_keeps_future_suites_and_missing_mandatory_blocks(self):
        manifest = self.gate.load_manifest(HERE / "merge-gate-manifest.json")
        for name in manifest["ci_tests"] + ["test-ci-observer.py", "test-periodic-full.py", "test-release-proof.py"]:
            self.put(".github/scripts/" + name, "pass\n")
        paths = self.gate.ci_tests(self.root, manifest)
        self.assertIn("test-release-proof.py", [path.name for path in paths])
        (self.root / ".github/scripts/test-ci-scheduling.py").unlink()
        with self.assertRaises(ValueError):
            self.gate.ci_tests(self.root, manifest)

    def test_independent_buckets_have_exact_union_and_no_empty_match_claim(self):
        names = self.gate.Counter({"TestCase" + str(i): 1 for i in range(63)})
        groups = [self.gate.bucket_selection(names, i, 8) for i in range(8)]
        self.assertEqual(sum(groups, self.gate.Counter()), names)
        self.assertEqual(sum(len(group) for group in groups), len(names))

    def test_no_match_go_zero_exit_is_not_evidence(self):
        events = self.root / "events.json"
        package = "fixture/a"
        events.write_text("\n".join(json.dumps(row) for row in [
            {"Action": "start", "Package": package}, {"Action": "pass", "Package": package}]) + "\n")
        with self.assertRaises(ValueError):
            self.gate.verifier.verify(events, package, {"TestRequired": 1})


if __name__ == "__main__":
    unittest.main()
