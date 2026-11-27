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
from unittest import mock

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
        self.assertEqual(selected["packages"], ["fixture/" + name for name in ("a", "b", "c", "z")])
        self.assertIn("fixture/q", selected["race_full"])
        self.assertFalse(selected["web"])
        self.assertFalse(selected["ci"])

    def test_external_test_variant_owns_its_dir_not_other_binary_and_does_not_promote_consumers(self):
        self.put("a/internal_test.go", 'package a\nimport "testing"\nfunc TestInternal(t *testing.T) {}\n')
        self.put("a/external_test.go", 'package a_test\nimport _ "fixture/c"\nimport "testing"\nfunc TestExternal(t *testing.T) {}\n')
        self.put("c/safety_test.go", 'package c\nimport "testing"\nfunc TestSafety(t *testing.T) {}\n')
        self.put("c/liveness_test.go", '//go:build !race\n\npackage c\nimport "testing"\nfunc TestLiveness(t *testing.T) {}\n')
        self.base = self.commit()
        self.put("a/new_test.go", 'package a\nimport "testing"\nfunc TestAdded(t *testing.T) {}\n')
        self.sha = self.commit()
        rule = {"paths": ["a/"], "ordinary": {"fixture/c": ["TestSafety"]}}
        selected = self.selection([rule])
        self.assertEqual(selected["packages"], ["fixture/a"])
        self.assertNotIn("fixture/a", selected["race_full"], "另一个包的 !race 测试不是 a.test 的编译输入")
        units = self.gate.tasks(selected)
        ordinary = {unit["package"]: unit for unit in units if unit.get("kind") == "go" and not unit["race"]}
        self.assertIsNone(ordinary["fixture/a"]["names"])
        self.assertEqual(ordinary["fixture/c"]["names"], ["TestSafety"], "跨包必需普通测试不扩大为完整包")
        self.assertFalse(any(unit.get("race") for unit in units))

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
        self.assertTrue(self.selection()["conservative"])
        self.assertTrue(self.selection()["web"] and self.selection()["bridge"] and self.selection()["ci"])

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

    def test_modified_and_deleted_web_javascript_preserve_web_and_go_embed_owners(self):
        self.put("root.go", 'package fixture\nimport "embed"\n//go:embed web/*.js\nvar assets embed.FS\n')
        self.put("web/app.js", "export const value = 1;\n")
        self.put("web/keep.js", "export const keep = true;\n")
        self.put("z/code.go", 'package z\nimport _ "fixture"\n')
        self.base = self.commit()
        self.put("web/app.js", "export const value = 2;\n")
        self.sha = self.commit()
        selected = self.selection()
        self.assertTrue(selected["web"], "同一个 JS 文件同时需要 web 和 Go embed 检查")
        self.assertIn("fixture", selected["packages"])
        self.assertIn("fixture/z", selected["packages"])
        self.base = self.sha
        self.git("rm", "web/app.js")
        self.sha = self.commit()
        selected = self.selection()
        self.assertTrue(selected["web"])
        self.assertIn("fixture", selected["packages"])
        self.assertIn("fixture/z", selected["packages"])
        self.assertTrue(selected["conservative"])

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
        self.assertFalse((HERE.parents[1] / "internal/store/merge_gate_migration_test.go").exists(),
                         "Merge-only prefix matrix must not enter pre-existing Full race slots")
        self.assertTrue((HERE / "merge-gate-migration_test.go.template").is_file())

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

    def test_named_groups_execute_real_disjoint_race_tests_with_complete_final_union(self):
        names = ["TestNamed" + str(i) for i in range(6)]
        self.put("z/code_test.go", 'package z_test\nimport "testing"\n' +
                 "\n".join("func " + name + "(t *testing.T) {}" for name in names))
        self.head = self.commit()
        self.sha = self.git("commit-tree", self.git("rev-parse", "HEAD^{tree}"), "-p", self.base, "-p", self.head, input="merge\n")
        self.git("reset", "--hard", self.sha)
        self.event["pull_request"]["head"]["sha"] = self.head
        self.context["GITHUB_SHA"] = self.sha
        selected = {"packages": [], "race": {"fixture/z": names}, "ordinary_required": {}, "race_full": [],
                    "all_race": False, "unresolved": [], "web": False, "bridge": False, "ci": False}
        budget = {"target_seconds": 300, "max_groups": 7, "weights": {"race:fixture/z": {name: 100 for name in names}}}
        units = self.gate.tasks(selected, budget)
        self.assertEqual(len([unit for unit in units if unit["kind"] == "go"]), 2)
        identity = self.gate.native_identity(self.root, self.event, self.context)
        plan = {**identity, **selected, "tasks": units}
        evidence = [{**identity, "unit": unit["unit"], "result": "success", "detail":
                     self.gate.run_go(self.root, unit) if unit["kind"] == "go" else {}} for unit in units]
        needs = {"plan": {"result": "success"}, "tests": {"result": "success"}}
        self.gate.finalize(plan, needs, evidence)
        self.assertEqual(sum((self.gate.Counter(row["detail"]["passes"]) for row in evidence if row["detail"]), self.gate.Counter()),
                         self.gate.Counter(names))
        with self.assertRaises(ValueError):
            self.gate.finalize({**plan, "tasks": units[:-1]}, needs, evidence[:-1])

    def test_no_match_go_zero_exit_is_not_evidence(self):
        events = self.root / "events.json"
        package = "fixture/a"
        events.write_text("\n".join(json.dumps(row) for row in [
            {"Action": "start", "Package": package}, {"Action": "pass", "Package": package}]) + "\n")
        with self.assertRaises(ValueError):
            self.gate.verifier.verify(events, package, {"TestRequired": 1})


class NamedJobTests(unittest.TestCase):
    def setUp(self):
        self.gate = load("merge-gate")
        self.manifest = self.gate.load_manifest(HERE / "merge-gate-manifest.json")

    def selected(self):
        names = sorted({name for rule in self.manifest["rules"]
                        for name in rule.get("tests", {}).get("agentworkspace/internal/platform", [])})
        return {"packages": [], "race": {"agentworkspace/internal/platform": names}, "ordinary_required": {},
                "race_full": [], "all_race": False, "unresolved": [], "web": False, "bridge": False, "ci": False}

    def test_weighted_named_jobs_have_exact_mode_union_and_largest_first(self):
        selected = self.selected()
        units = [unit for unit in self.gate.tasks(selected) if unit["kind"] == "go"]
        self.assertGreater(len(units), 1, "measured 1650.86s of named race work is packed into one job")
        self.assertLessEqual(len(units), 7)
        self.assertEqual(sum((self.gate.Counter(unit["names"]) for unit in units), self.gate.Counter()),
                         self.gate.Counter(selected["race"]["agentworkspace/internal/platform"]))
        self.assertTrue(all(unit["race"] and unit["required"] == unit["names"] for unit in units))
        weights = [unit["estimated_seconds"] for unit in units]
        self.assertEqual(weights, sorted(weights, reverse=True))
        self.assertLessEqual(max(weights), 300)

    def test_concurrent_guard_weights_keep_exact_public_pass_observations(self):
        budget = self.gate.load_weights()
        key = "race:agentworkspace/internal/store"
        provenance = budget["provenance"]
        artifacts = {row["id"]: row for row in provenance["artifacts"]}
        cases = (
            ("TestTaskSubmissionConcurrentReplayAndCompetition", 11.96, 15146795956, 140095991478,
             "store-12-run-0", "2026-11-26T18:36:22.352097001Z",
             "sha256:1a4e431c46e860b0456dbc3034005e81c35ccb48548a3d2d20246b3a42589c60"),
            ("TestThreadDecisionAnswerHTTPAliasesAndHistoricalRead", 11.94, 15146851832, 140095991591,
             "store-22-run-0", "2026-11-26T18:46:09.031847155Z",
             "sha256:21d3c1ddbf60e8f24f3d88e76090098d96e7cceb8026cf2835c8a0aa22db2a11"),
            ("TestThreadDecisionAcknowledgementIsExplicitAndDurable", 6.55, 15146645277, 140095991734,
             "store-11-run-0", "2026-11-26T18:37:50.434344995Z",
             "sha256:f03bf524e7cec8838c928261acc522f1c7b642ce023ce2748706f038f2dc6175"),
        )
        for name, seconds, artifact, job, invocation, time, digest in cases:
            with self.subTest(name=name):
                self.assertEqual(budget["weights"][key].get(name), seconds)
                self.assertEqual(provenance["observations"][key][name],
                                 {"artifact": artifact, "invocation": invocation, "time": time})
                self.assertEqual(artifacts[artifact]["digest"], digest)
                self.assertEqual(artifacts[artifact]["job_id"], job)
                self.assertEqual(artifacts[artifact]["job_url"], provenance["run_url"].split("/attempts/")[0] + f"/job/{job}")
        self.assertEqual({key: set(names) for key, names in budget["weights"].items()},
                         {key: set(names) for key, names in provenance["observations"].items()})
        self.assertEqual(provenance["run_conclusion"], "failure")

    def test_full_race_keeps_batches_and_has_no_duplicate_named_jobs(self):
        selected = self.selected()
        selected["race_full"] = ["agentworkspace/internal/platform"]
        units = [unit for unit in self.gate.tasks(selected) if unit["kind"] == "go"]
        self.assertEqual([unit["worker"] for unit in units], list(range(6)))
        self.assertTrue(all(unit["names"] is None for unit in units))
        self.assertTrue(all(unit["required"] == selected["race"][unit["package"]] for unit in units))

    def test_migration_matrix_is_separate_ordinary_and_preserves_all_full_profile_tasks(self):
        selected = self.selected()
        package = "agentworkspace/internal/store"
        selected.update(packages=[package], race_full=[package], race={package: ["TestMissingAndCrossGroupReferences"]})
        original = self.gate.tasks(selected)
        current = self.gate.tasks({**selected, "migrations": True})
        migration = [task for task in current if task.get("migration")]
        self.assertEqual(len(migration), 1)
        self.assertIs(migration[0]["race"], False)
        self.assertIn("TestMergeGateAllMigrationPrefixesUpgradeReopenReplay", migration[0]["required"])
        self.assertEqual([task for task in current if not task.get("migration")], original)
        self.assertTrue(all("TestMergeGateAllMigrationPrefixesUpgradeReopenReplay" not in task.get("required", [])
                            for task in current if task.get("race")))

    def test_final_rejects_missing_and_duplicate_named_group_even_when_rows_match_tasks(self):
        gate = self.gate
        identity = dict(zip(gate.IDENTITY, ("PeterFile/ct0x", 7, "a" * 40, "b" * 40, "c" * 40, "d" * 40, "101", "1")))
        package = "fixture/named"
        tasks, evidence = [], []
        for i, name in enumerate(("TestA", "TestB")):
            unit = {"unit": f"named-{i}", "kind": "go", "package": package, "race": True,
                    "names": [name], "required": [name], "worker": None, "profile": None}
            tasks.append(unit)
            evidence.append({**identity, "unit": unit["unit"], "result": "success", "detail": {
                "compiled": {"TestA": 1, "TestB": 1}, "selected": {name: 1}, "runs": {name: 1}, "passes": {name: 1}, "skips": {}}})
        plan = {**identity, "tasks": tasks, "race": {package: ["TestA", "TestB"]}, "ordinary_required": {}}
        needs = {"plan": {"result": "success"}, "tests": {"result": "success"}}
        gate.finalize(plan, needs, evidence)
        with self.assertRaises(ValueError):
            gate.finalize({**plan, "tasks": tasks[:1]}, needs, evidence[:1])
        bad = copy.deepcopy(evidence)
        bad[1]["detail"].update(selected={"TestA": 1}, runs={"TestA": 1}, passes={"TestA": 1})
        duplicate = copy.deepcopy(tasks)
        duplicate[1].update(names=["TestA"], required=["TestA"])
        with self.assertRaises(ValueError):
            gate.finalize({**plan, "tasks": duplicate}, needs, bad)


class ConcurrentGuardTests(unittest.TestCase):
    package = "agentworkspace/internal/store"
    submission = "TestTaskSubmissionConcurrentReplayAndCompetition"
    answer = "TestThreadDecisionAnswerHTTPAliasesAndHistoricalRead"
    acknowledgement = "TestThreadDecisionAcknowledgementIsExplicitAndDurable"

    def setUp(self):
        self.gate = load("merge-gate")
        self.manifest = self.gate.load_manifest(HERE / "merge-gate-manifest.json")

    def routed(self, path):
        return {name for rule in self.manifest["rules"]
                if any(self.gate.matches(path, pattern) for pattern in rule["paths"])
                for name in rule.get("tests", {}).get(self.package, [])}

    def selection(self, paths):
        # Isolate policy routing with equal ordinary/race compiled inputs, not product execution.
        directories = ("internal/store", "internal/platform", "internal/httpapi", "internal/coordination", "cmd/workspace")
        packages = {"agentworkspace/" + directory: directory for directory in directories}
        owners = {path: "agentworkspace/" + str(Path(path).parent) for path in paths if path.endswith(".go")}
        files = {path: {package} for path, package in owners.items()}
        mode = {"files": files, "reverse": {package: {package + ".test"} for package in packages},
                "roots": {package: package + ".test" for package in packages},
                "nodes": {package: {package} for package in packages}, "inputs": {package: set() for package in packages}}
        graph = (packages, {}, files, set(), {False: mode, True: mode})
        sha = "b" * 40
        with mock.patch.object(self.gate, "git", return_value=sha), \
                mock.patch.object(self.gate.routing, "changed_paths", return_value=paths), \
                mock.patch.object(self.gate.routing, "git", return_value=b""), \
                mock.patch.object(self.gate, "graph", return_value=graph):
            selected = self.gate.select(HERE.parents[1], "a" * 40, sha, self.manifest)
        return selected, self.gate.tasks(selected)

    def test_submission_business_schema_and_lock_paths_route_named_race(self):
        for path in ("internal/store/task_submission.go", "internal/store/task_submission_test.go",
                     "internal/store/task_submission_scope_test.go", "internal/store/idempotency.go",
                     "internal/store/migrations.go", "internal/store/migrations_task_successor_registration_test.go",
                     "internal/store/task_schema_test.go", "internal/store/store.go"):
            with self.subTest(path=path):
                self.assertIn(self.submission, self.routed(path))

    def test_thread_answer_ack_wake_and_entry_paths_route_named_race(self):
        common = ("internal/store/decision_thread_answer.go", "internal/store/decision_thread_wake.go",
                  "internal/store/decision_thread_ack.go", "internal/store/decision_thread_answer_http_test.go",
                  "internal/store/decision_thread_ack_test.go", "internal/store/decision_thread_lifecycle_test.go",
                  "internal/store/decision_answer.go", "internal/store/store.go",
                  "internal/coordination/decision_answer.go", "internal/coordination/decision_answer_test.go")
        paths = {
            self.answer: common + ("internal/httpapi/decision_answer.go", "internal/httpapi/decision_answer_test.go",
                                   "internal/platform/decision_answer.go"),
            self.acknowledgement: common + ("internal/httpapi/member_thread_decision_ack.go",
                                            "internal/platform/member_thread_decision_ack.go"),
        }
        for name, routes in paths.items():
            for path in routes:
                with self.subTest(name=name, path=path):
                    self.assertIn(name, self.routed(path))

    def test_six_historical_rows_select_top_level_race_without_full_store(self):
        submission_paths = ["internal/store/migrations.go", "internal/store/migrations_task_successor_registration_test.go"]
        answer_paths = ["internal/store/decision_thread_answer.go", "internal/store/decision_thread_wake.go",
                        "internal/httpapi/decision_answer.go"]
        ack_paths = ["internal/store/decision_thread_ack.go", "internal/store/decision_thread_lifecycle_test.go",
                     "internal/store/decision_thread_answer.go", "internal/store/store.go"]
        # Source-record obligations only; this does not replay the historical SQLite failures.
        cases = [(45810094503, 139907489295, self.submission + suffix, submission_paths)
                 for suffix in ("/same-key", "/different-keys", "/no-key", "")]
        cases += [(44572131767, 135756059976, self.answer, answer_paths),
                  (44572131767, 135756060028, self.acknowledgement, ack_paths)]
        for run, job, observed, paths in cases:
            with self.subTest(run=run, job=job, observed=observed):
                name = observed.split("/", 1)[0]
                evidence = f"https://github.com/PeterFile/ct0x/actions/runs/{run}/job/{job}"
                selected, tasks = self.selection(paths)
                self.assertIn(name, selected["race"].get(self.package, []))
                self.assertTrue(any(evidence in rule.get("evidence", []) and name in rule.get("tests", {}).get(self.package, [])
                                    for rule in self.manifest["rules"]))
                race = [task for task in tasks if task.get("race") and task.get("package") == self.package]
                self.assertTrue(race)
                self.assertTrue(all(task["names"] is not None for task in race))
                self.assertEqual(sum((self.gate.Counter(task["names"]) for task in race), self.gate.Counter()),
                                 self.gate.Counter(selected["race"][self.package]))
                self.assertTrue(all(task["required"] == task["names"] and task["worker"] is None for task in race))
                self.assertEqual(selected["unresolved"], [])

    def test_manifest_runs_three_guards_but_unrelated_docs_stay_light(self):
        selected, tasks = self.selection([".github/scripts/merge-gate-manifest.json"])
        names = {self.submission, self.answer, self.acknowledgement}
        self.assertEqual(set(selected["race"].get(self.package, [])), names)
        self.assertEqual({name for task in tasks if task.get("race") and task.get("package") == self.package
                          for name in task["names"]}, names)
        self.assertEqual(selected["race_full"], [])
        for path in ("docs/design.md", "README.md"):
            with self.subTest(path=path):
                selected, tasks = self.selection([path])
                self.assertEqual(selected["packages"], [])
                self.assertEqual(selected["race"], {})
                self.assertEqual(selected["ordinary_required"], {})
                self.assertEqual(tasks, [{"unit": "identity-diff", "kind": "identity"}])
                self.assertEqual(self.routed(path), set())


class ManifestTests(unittest.TestCase):
    def test_nested_observer_and_release_suites_are_conditional_and_complete(self):
        gate = load("merge-gate")
        manifest = gate.load_manifest(HERE / "merge-gate-manifest.json")
        with tempfile.TemporaryDirectory(prefix="merge discovery ") as temporary:
            root = Path(temporary)
            scripts = root / ".github/scripts"
            scripts.mkdir(parents=True)
            for name in manifest["ci_tests"]:
                (scripts / name).write_text("pass\n")
            self.assertEqual({p.name for p in gate.ci_tests(root, manifest)}, set(manifest["ci_tests"]))
            (scripts / "observer").mkdir()
            (scripts / "observer/observer.py").write_text("pass\n")
            with self.assertRaises(ValueError):
                gate.ci_tests(root, manifest)
            (scripts / "test-ci-observer.py").write_text("pass\n")
            self.assertIn("test-ci-observer.py", {p.name for p in gate.ci_tests(root, manifest)})
            (scripts / "release-core.py").write_text("pass\n")
            (scripts / "test-release-policy.py").write_text("pass\n")
            suites = {"release-test-evidence.py", "release-test-machine.py", "release-test-workflow.py"}
            for name in suites:
                (scripts / name).write_text("pass\n")
            self.assertTrue(suites <= {p.name for p in gate.ci_tests(root, manifest)})
            for name in suites:
                with self.subTest(missing=name):
                    (scripts / name).unlink()
                    with self.assertRaises(ValueError):
                        gate.ci_tests(root, manifest)
                    (scripts / name).write_text("pass\n")

    def test_periodic_standalones_suppress_only_scheduling_nested_loader_and_ignore_inherited_flag(self):
        gate = load("merge-gate")
        manifest = gate.load_manifest(HERE / "merge-gate-manifest.json")
        periodic = {"test-periodic-clients.py", "test-periodic-diagnostics.py", "test-periodic-full.py", "test-periodic-history.py"}
        with tempfile.TemporaryDirectory(prefix="merge CI env ") as temporary:
            root = Path(temporary)
            scripts = root / ".github/scripts"
            scripts.mkdir(parents=True)
            trace = root / "trace.jsonl"
            body = ('import json,os,subprocess,sys\nfrom pathlib import Path\n'
                    'p=Path(__file__)\n'
                    'if p.name=="test-ci-scheduling.py" and (p.parent/"periodic-full.py").exists() and os.environ.get("CI_TESTS_DISCOVERED")!="1":\n'
                    ' for n in ("test-periodic-clients.py","test-periodic-diagnostics.py","test-periodic-full.py","test-periodic-history.py"):\n'
                    '  subprocess.run([sys.executable,str(p.parent/n)],check=True)\n'
                    'with open(os.environ["MERGE_TEST_TRACE"],"a") as f: f.write(json.dumps({"name":p.name,"flag":os.environ.get("CI_TESTS_DISCOVERED")})+"\\n")\n')
            for name in manifest["ci_tests"]:
                (scripts / name).write_text(body)
            identity = {key: str(index) for index, key in enumerate(gate.IDENTITY)}
            identity["source"] = {}
            plan = {**identity, "tasks": [{"unit": "ci", "kind": "ci"}]}
            for installed in (False, True):
                with self.subTest(periodic=installed):
                    if installed:
                        (scripts / "periodic-full.py").write_text("pass\n")
                        for name in periodic:
                            (scripts / name).write_text(body)
                    trace.write_text("")
                    with mock.patch.object(gate, "native_identity", return_value=identity), mock.patch.dict(os.environ, {
                            "CI_TESTS_DISCOVERED": "1", "MERGE_TEST_TRACE": str(trace)}):
                        gate.run_task(root, plan, 0, {}, {})
                    rows = [json.loads(line) for line in trace.read_text().splitlines()]
                    self.assertEqual(gate.Counter(row["name"] for row in rows),
                                     gate.Counter(set(manifest["ci_tests"]) | (periodic if installed else set())))
                    for row in rows:
                        self.assertEqual(row["flag"], "1" if installed and row["name"] == "test-ci-scheduling.py" else None)
            (scripts / "test-periodic-history.py").unlink()
            trace.write_text("")
            with self.assertRaises(ValueError):
                gate.ci_tests(root, manifest)
            self.assertEqual(trace.read_text(), "")

    def test_audited_safety_paths_keep_original_modes_without_doc_or_web_fanout(self):
        gate = load("merge-gate")
        manifest = gate.load_manifest(HERE / "merge-gate-manifest.json")
        cases = (
            ("internal/workspacefs/snapshot.go", "tests", "agentworkspace/cmd/workspace", "TestCoordinationCompositionUnknownCancelAndShutdownHoldClaims"),
            ("cmd/workspace/background_dispatch.go", "tests", "agentworkspace/internal/coordination", "TestIntentPassCancellationAndQueryFailure"),
            ("internal/scheduler/dispatch.go", "tests", "agentworkspace/internal/coordination", "TestIntentPassCancellationAndQueryFailure"),
            ("internal/platform/prepare_repair.go", "tests", "agentworkspace/internal/platform", "TestChatRepairPreparationWaitsUnclaimedAndSchedulerContinues"),
            ("internal/platform/member_capability.go", "tests", "agentworkspace/internal/platform", "TestPrepareAdaptersShareOrdinaryRoleSubmissionReviewChain"),
            ("internal/provider/pi.go", "tests", "agentworkspace/internal/platform", "TestMemberCapabilityRoleCommandRevokeDrainsCallback"),
            ("internal/platform/task_work.go", "tests", "agentworkspace/internal/platform", "TestFreshTaskWorkB29NativeWireConfirmedInput"),
            ("internal/platform/investigation.go", "tests", "agentworkspace/internal/platform", "TestPiInvestigationOwnerNativeSQLiteRoundTrip"),
            ("internal/store/migrations.go", "tests", "agentworkspace/internal/store", "TestCoordinationAssignmentV13MigrationFrozenHistory"),
            (".github/scripts/ci-shards.py", "tests", "agentworkspace/internal/httpapi", "TestMemberMCPCodexMetadataCoordinatorDecision"),
            (".github/scripts/merge-gate.py", "ordinary", "agentworkspace/cmd/workspace", "TestRunExplanationParentCancelAfterSendKeepsExactJournal"),
            ("internal/scheduler/dispatch.go", "ordinary", "agentworkspace/internal/coordination", "TestIntentPumpSchedulerBeginBusyRecovery"),
        )
        def names(path, mode):
            result = set()
            for rule in manifest["rules"]:
                if any(gate.matches(path, pattern) for pattern in rule["paths"]):
                    result.update((package, name) for package, tests in rule.get(mode, {}).items() for name in tests)
            return result
        for path, mode, package, name in cases:
            with self.subTest(path=path, mode=mode, name=name):
                self.assertIn((package, name), names(path, mode))
        for path in ("docs/design.md", "web/src/app.ts", ".github/scripts/test-merge-gate-publish.py"):
            with self.subTest(path=path):
                self.assertFalse(names(path, "tests") or names(path, "ordinary"))


if __name__ == "__main__":
    unittest.main()
