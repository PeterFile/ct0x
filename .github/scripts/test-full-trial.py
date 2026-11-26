#!/usr/bin/env python3
"""Focused Full trial scheduling, execution evidence and workflow regressions."""
from collections import Counter
from contextlib import redirect_stdout
import copy
import importlib.util
import io
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tempfile
import unittest
from unittest import mock

SCRIPTS = Path(__file__).resolve().parent


def load(name):
    spec = importlib.util.spec_from_file_location(name.replace("-", "_"), SCRIPTS / (name + ".py"))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


class SchedulingTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.plan = load("full-trial-plan")
        cls.legacy = load("ci-plan")
        cls.shards = load("ci-shards")

    def inventories(self):
        return {profile: Counter({f"TestVector{i}": 1 for i in range(200)})
                for profile in ("platform", "store", "httpapi", "workspace")}

    def test_a_is_the_unchanged_baseline_not_lpt(self):
        schedule = self.plan.schedule("a", 7, self.inventories())
        self.assertEqual(schedule["shards"], [[{"profile": row["profile"], "worker": row["worker"], "part": None}]
                                               for row in self.legacy.shard_matrix()["include"]])
        self.assertEqual(schedule["workspace"], [0])
        self.assertEqual(len(schedule["shards"]), 24)
        self.assertEqual(self.shards.SLOW_STORE_SLOTS, {6, 8, 38, 50, 51, 57, 62})
        self.assertEqual(self.shards.BATCHES["platform"], ((4,), (8, 0), (1,), (3, 5), (6, 7), (2,)))

    def test_arm_parallel_and_repeat_are_strictly_bounded(self):
        for scheme, parallel in [("a", 8), ("b", 7), ("c", 7), ("b", 13), ("c", 17), ("d", 12), ("B", 12), ("b", True), ("b", 8.5)]:
            with self.subTest(scheme=scheme, parallel=parallel), self.assertRaises(ValueError):
                self.plan.parameters(scheme, 1, parallel)
        for repeat in (0, 4, True, 1.0, "1"):
            with self.subTest(repeat=repeat), self.assertRaises(ValueError):
                self.plan.parameters("b", repeat, 12)
        for scheme, parallel in [("a", 7), ("b", 8), ("b", 12), ("c", 12)]:
            self.plan.parameters(scheme, 3, parallel)

    def test_lpt_uses_frozen_weights_and_deterministic_least_load_ties(self):
        tasks = [("a", 8), ("b", 7), ("c", 6), ("d", 5), ("e", 4)]
        self.assertEqual(self.plan.lpt(tasks, 2), [["a", "d", "e"], ["b", "c"]])
        self.assertEqual(self.plan.lpt(list(reversed(tasks)), 2), self.plan.lpt(tasks, 2))
        weights = self.plan.weights()
        self.assertEqual({p: len(values) for p, values in weights.items()}, {"store": 64, "platform": 9, "httpapi": 8})
        self.assertAlmostEqual(weights["store"][52], 155.79)
        self.assertAlmostEqual(sum(weights["store"]), 14143.816)
        schedule = self.plan.schedule("b", 12, self.inventories())
        units = [unit for lane in schedule["shards"] for unit in lane]
        self.assertCountEqual(units, [dict(row, part=None) for row in self.legacy.shard_matrix()["include"]])
        self.assertEqual(len(schedule["shards"]), 12)
        self.assertEqual(schedule, self.plan.schedule("b", 12, {p: dict(reversed(list(v.items()))) for p, v in self.inventories().items()}))

    def test_c_slow_parts_are_on_distinct_job_runners_and_rebuild_all_slots(self):
        inventories = self.inventories()
        schedule = self.plan.schedule("c", 12, inventories)
        seen, slow_jobs = Counter(), {}
        for job, lane in enumerate(schedule["shards"]):
            for unit in lane:
                profile = unit["profile"]
                slots = self.plan.unit_slots("c", unit)
                for slot in slots:
                    if unit["part"] is None:
                        seen[profile, slot] += 1
                    else:
                        slow_jobs.setdefault(slot, []).append(job)
        for slot in self.shards.SLOW_STORE_SLOTS:
            selected, _ = self.shards.verifier.selection(inventories["store"], slot, "store")
            parts = self.shards.execution_subsets(selected, "store", slot)
            self.assertEqual(len(slow_jobs[slot]), len(parts))
            self.assertEqual(len(set(slow_jobs[slot])), len(parts), "a canonical slow slot must span independent jobs")
            seen["store", slot] += 1
        self.assertEqual(seen, Counter({(p, s): 1 for p, (n, *_) in self.shards.verifier.PROFILES.items() for s in range(n)}))
        self.assertEqual(schedule["workspace"], list(range(4)))
        parts = [self.plan.workspace_selection(inventories["workspace"], index, 4) for index in range(4)]
        self.assertEqual(sum(parts, Counter()), inventories["workspace"])
        self.assertTrue(all(not (a.keys() & b.keys()) for i, a in enumerate(parts) for b in parts[i + 1:]))


class EvidenceTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.events = load("full-trial-events")

    def event_file(self, directory, names, skips=(), mutate=None):
        package = "agentworkspace/cmd/workspace"
        rows = [{"Action": "start", "Package": package}]
        for name in names:
            rows += [{"Action": "run", "Package": package, "Test": name},
                     {"Action": "skip" if name in skips else "pass", "Package": package, "Test": name, "Elapsed": 0.01}]
        rows.append({"Action": "pass", "Package": package, "Elapsed": 0.2})
        if mutate:
            mutate(rows)
        path = Path(directory) / "events.json"
        path.write_text("".join(json.dumps(row) + "\n" for row in rows))
        return path

    def test_workspace_only_proven_skip_matches_real_monolith_outcomes(self):
        helper = "TestArtifactReadMarkerWriteFailureHelper"
        selected = Counter({helper: 1, "TestNormal": 1})
        with tempfile.TemporaryDirectory() as directory:
            path = self.event_file(directory, selected, [helper])
            result = self.events.workspace(path, selected)
            self.assertEqual(result["runs"], selected)
            self.assertEqual(result["skips"], {helper: 1})
            self.events.compare_workspace([result], result)
            for skips in ([], [helper, "TestNormal"]):
                path = self.event_file(directory, selected, skips)
                with self.assertRaises(ValueError):
                    self.events.workspace(path, selected)
            for problem in ("missing", "duplicate", "foreign", "fail", "unfinished"):
                def mutate(rows):
                    if problem == "missing":
                        del rows[1:3]
                    elif problem == "duplicate":
                        rows[1:1] = rows[1:3]
                    elif problem == "foreign":
                        rows[1]["Package"] = "foreign/package"
                    elif problem == "fail":
                        rows[2]["Action"] = "fail"
                    else:
                        rows.pop()
                path = self.event_file(directory, selected, [helper], mutate)
                with self.subTest(problem=problem), self.assertRaises(ValueError):
                    self.events.workspace(path, selected)
            for parts in ([], [result, result], [{**result, "passes": {}}]):
                with self.assertRaises(ValueError):
                    self.events.compare_workspace(parts, result)

    def test_timing_export_never_copies_output_or_credentials_and_keeps_failures(self):
        with tempfile.TemporaryDirectory() as directory:
            path = self.event_file(directory, ["TestNormal"], mutate=lambda rows: rows.insert(2, {
                "Action": "output", "Package": "agentworkspace/cmd/workspace", "Test": "TestNormal", "Output": "token=PRIVATE\n"}))
            records = self.events.compact(path, {"run_id": "42", "attempt": "2", "checkout_sha": "a" * 40,
                                                  "checkout_tree": "b" * 40}, "workspace", 0, "race-0")
            self.assertEqual([row["action"] for row in records], ["run", "pass", "pass"])
            self.assertNotIn("PRIVATE", json.dumps(records))
            self.assertEqual(records[-1]["name"], "")
            self.assertEqual(records[-1]["elapsed"], 0.2)


class ReceiptTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.driver = load("full-trial")
        cls.plan = load("full-trial-plan")

    def fixture(self, root, scheme="c", parallel=12):
        context = {"run_id": "42", "attempt": "1", "controller_sha": "c" * 40, "controller_tree": "d" * 40,
                   "checkout_sha": self.plan.PRODUCT_SHA, "checkout_tree": self.plan.PRODUCT_TREE,
                   "scheme": scheme, "repeat": 1, "shard_parallel": parallel}
        inventories = SchedulingTests.inventories(self)
        inventories["workspace"]["TestArtifactReadMarkerWriteFailureHelper"] = 1
        monolith = {"selected": dict(inventories["workspace"]), "runs": dict(inventories["workspace"]),
                    "passes": {n: 1 for n in inventories["workspace"] if n != "TestArtifactReadMarkerWriteFailureHelper"},
                    "skips": {"TestArtifactReadMarkerWriteFailureHelper": 1}}
        plan = {"schema": 1, "identity": context, "base": "e" * 40, "master_oid": "f" * 40,
                "inventories": inventories, "monolith": monolith,
                "schedule": self.plan.schedule(scheme, parallel, inventories)}
        for job_number, units in enumerate(plan["schedule"]["shards"]):
            results = []
            for unit in units:
                profile = unit["profile"]
                total, package, _, _ = self.plan.verifier.PROFILES[profile]
                summaries = []
                for slot in self.plan.unit_slots(scheme, unit):
                    selected, _ = self.plan.verifier.selection(inventories[profile], slot, profile)
                    if unit["part"] is not None:
                        selected = self.plan.shards.execution_subsets(selected, profile, slot)[unit["part"]]
                    summaries.append({"schema": 1, "slot": slot, "total": total,
                                      "run_id": context["run_id"], "checkout_sha": context["checkout_sha"],
                                      "checkout_tree": context["checkout_tree"], "package": package,
                                      "compiled": dict(inventories[profile]), "selected": dict(selected),
                                      "runs": dict(selected), "passes": dict(selected), "skips": {}})
                results.append({"unit": unit, "summaries": summaries})
            receipt = {"schema": 1, "identity": context, "job": "go-shards", "job_number": job_number, "units": results}
            path = root / "shards" / (self.driver.prefix(context) + f"-shard-{job_number}") / "receipt.json"
            path.parent.mkdir(parents=True)
            path.write_text(json.dumps(receipt))
        for job_number in plan["schedule"]["workspace"]:
            selected = self.plan.workspace_selection(inventories["workspace"], job_number, len(plan["schedule"]["workspace"]))
            proof = {field: {n: v for n, v in values.items() if n in selected} for field, values in monolith.items()}
            receipt = {"schema": 1, "identity": context, "job": "go-workspace", "job_number": job_number,
                       "workspace": {"compiled": dict(inventories["workspace"]), "proof": proof}}
            path = root / "workspace" / (self.driver.prefix(context) + f"-workspace-{job_number}") / "receipt.json"
            path.parent.mkdir(parents=True)
            path.write_text(json.dumps(receipt))
        return plan

    def test_real_original_aggregate_accepts_reconstructed_a_b_c_only(self):
        for scheme, parallel in [("a", 7), ("b", 12), ("c", 12)]:
            with self.subTest(scheme=scheme), tempfile.TemporaryDirectory() as directory, redirect_stdout(io.StringIO()):
                root = Path(directory)
                plan = self.fixture(root, scheme, parallel)
                canonical, workspace = self.driver.collect(plan, root / "shards", root / "workspace", plan["identity"])
                result = self.driver.aggregate_canonical(plan, canonical)
                self.assertEqual(sum(row["total"] for row in result.values()), 81)
                self.assertEqual(workspace, plan["monolith"])

    def test_omitted_duplicate_mixed_corrupt_and_wrong_arm_receipts_fail_closed(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            plan = self.fixture(root)
            path = next((root / "shards").glob("*/receipt.json"))
            original = path.read_text()
            valid = json.loads(original)
            for field, value in [("attempt", "2"), ("run_id", "43"), ("scheme", "b"), ("shard_parallel", 8),
                                 ("controller_sha", "9" * 40), ("controller_tree", "8" * 40),
                                 ("checkout_sha", "7" * 40), ("checkout_tree", "6" * 40)]:
                receipt = copy.deepcopy(valid)
                receipt["identity"][field] = value
                path.write_text(json.dumps(receipt))
                with self.subTest(field=field), self.assertRaises(ValueError):
                    self.driver.collect(plan, root / "shards", root / "workspace", plan["identity"])
                path.write_text(original)
            for mutation in (lambda r: r["units"].pop(), lambda r: r["units"].append(r["units"][0]),
                             lambda r: r["units"][0]["summaries"][0].update(runs={}),
                             lambda r: r["units"][0]["summaries"][0].update(compiled={"TestOnlyControlledPlan": 1})):
                receipt = copy.deepcopy(valid)
                mutation(receipt)
                path.write_text(json.dumps(receipt))
                with self.assertRaises(ValueError):
                    self.driver.collect(plan, root / "shards", root / "workspace", plan["identity"])
                path.write_text(original)
            moved = root / "omitted"
            path.parent.rename(moved)
            with self.assertRaises(ValueError):
                self.driver.collect(plan, root / "shards", root / "workspace", plan["identity"])
            moved.rename(path.parent)
            shutil.copytree(path.parent, root / "shards" / "duplicate")
            with self.assertRaises(ValueError):
                self.driver.collect(plan, root / "shards", root / "workspace", plan["identity"])

    def test_skipped_or_partial_job_set_is_not_success(self):
        required = {"preflight", "go-shards", "go-workspace", "go-other", "go-vet", "go-benchmarks"}
        good = {name: {"result": "success"} for name in required}
        self.driver.check_results(good)
        for name in required:
            for status in ("skipped", "cancelled", "failure", None):
                with self.assertRaises(ValueError):
                    self.driver.check_results({**good, name: {"result": status}})
            with self.assertRaises(ValueError):
                self.driver.check_results({k: v for k, v in good.items() if k != name})


class ExecutionTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.driver = load("full-trial")
        cls.plan = cls.driver.planner

    def go(self, inventories, fail=False):
        def command(args, **kwargs):
            profile = "workspace" if args[-1] == "./cmd/workspace" else args[-1].removeprefix("./internal/")
            package = "agentworkspace/" + args[-1].removeprefix("./")
            if args[:2] == ["go", "list"]:
                return subprocess.CompletedProcess(args, 0, stdout=package + "\n")
            self.assertEqual(args[:2], ["go", "test"])
            self.assertIn("-count=1", args)
            rows = [{"Action": "start", "Package": package}]
            if "-list" in args:
                rows += [{"Action": "output", "Package": package,
                          "Output": "\n".join(inventories[profile]) + f"\nok  \t{package}\t0.001s\n"}]
            else:
                pattern = args[args.index("-run") + 1] if "-run" in args else ".*"
                for name in inventories[profile]:
                    if re.fullmatch(pattern, name):
                        action = "skip" if name == "TestArtifactReadMarkerWriteFailureHelper" else "pass"
                        rows += [{"Action": "run", "Package": package, "Test": name},
                                 {"Action": action, "Package": package, "Test": name, "Elapsed": 0.01}]
                if fail:
                    del rows[1:3]
            rows += [{"Action": "pass", "Package": package, "Elapsed": 0.2}]
            kwargs["stdout"].write("".join(json.dumps(row) + "\n" for row in rows).encode())
            return subprocess.CompletedProcess(args, 0)
        return command

    def test_all_arms_execute_and_publish_verified_actual_counts(self):
        for scheme, parallel in [("a", 7), ("b", 12), ("c", 12)]:
            with self.subTest(scheme=scheme), tempfile.TemporaryDirectory() as directory, redirect_stdout(io.StringIO()):
                root = Path(directory)
                plan = ReceiptTests.fixture(self, root / "expected", scheme, parallel)
                context = plan["identity"]
                outputs = root / "actual"
                for kind, job in [("shard", "go-shards"), ("workspace", "go-workspace")]:
                    jobs = plan["schedule"]["shards" if kind == "shard" else "workspace"]
                    for number in range(len(jobs)):
                        temporary = root / f"temp-{kind}-{number}"
                        temporary.mkdir()
                        with mock.patch.dict(os.environ, {"RUNNER_TEMP": str(temporary), "TRIAL_JOB_NUMBER": str(number), "GITHUB_JOB": job}), \
                             mock.patch.object(self.driver.shards, "command", side_effect=self.go(plan["inventories"])), \
                             mock.patch.object(self.driver.shards, "check_identity"):
                            self.driver.worker(plan, context, kind)
                        target = outputs / ("shards" if kind == "shard" else "workspace") / (self.driver.prefix(context) + f"-{kind}-{number}")
                        target.parent.mkdir(parents=True, exist_ok=True)
                        (temporary / "trial-receipt").rename(target)
                        timing = [json.loads(line) for line in (temporary / "trial-timing.jsonl").read_text().splitlines()]
                        self.assertTrue(timing)
                        self.assertTrue(all(row["job_number"] == number and row["attempt"] == "1" for row in timing))
                canonical, workspace = self.driver.collect(plan, outputs / "shards", outputs / "workspace", context)
                result = self.driver.aggregate_canonical(plan, canonical)
                self.assertEqual(sum(row["total"] for row in result.values()), 81)
                self.assertEqual(workspace, plan["monolith"])

    def test_missing_real_execution_never_publishes_success_receipt(self):
        with tempfile.TemporaryDirectory() as directory, redirect_stdout(io.StringIO()):
            root = Path(directory)
            plan = ReceiptTests.fixture(self, root / "expected", "a", 7)
            with mock.patch.dict(os.environ, {"RUNNER_TEMP": directory, "TRIAL_JOB_NUMBER": "0", "GITHUB_JOB": "go-shards"}), \
                 mock.patch.object(self.driver.shards, "command", side_effect=self.go(plan["inventories"], fail=True)), \
                 mock.patch.object(self.driver.shards, "check_identity"):
                with self.assertRaises(ValueError):
                    self.driver.worker(plan, plan["identity"], "shard")
            self.assertFalse((root / "trial-receipt").exists())
            self.assertTrue((root / "trial-timing.jsonl").is_file())

    def test_real_go_compiled_inventory_and_race_subset_without_dependencies(self):
        with tempfile.TemporaryDirectory() as directory, redirect_stdout(io.StringIO()):
            root = Path(directory)
            store = root / "internal/store"
            store.mkdir(parents=True)
            names = []
            for index in range(10000):
                name = f"TestVector{index}"
                if self.plan.verifier.selection({name: 1}, 6, "store")[0]:
                    names.append(name)
                    if len(names) == 11:
                        break
            (root / "go.mod").write_text("module agentworkspace\n\ngo 1.24\n")
            (store / "store_test.go").write_text('package store\nimport "testing"\n' +
                "\n".join(f"func {name}(t *testing.T) {{ t.Log(\"private raw output\") }}" for name in names))
            context = {"run_id": "42", "attempt": "1", "checkout_sha": self.plan.PRODUCT_SHA,
                       "checkout_tree": self.plan.PRODUCT_TREE, "scheme": "c", "repeat": 1,
                       "shard_parallel": 12, "job": "go-shards", "job_number": 0}
            previous = Path.cwd()
            try:
                os.chdir(root)
                with mock.patch.dict(os.environ, {"RUNNER_TEMP": directory, "GOPROXY": "off", "GOSUMDB": "off",
                                                  "GOWORK": "off", "GOTOOLCHAIN": "local"}):
                    result = self.driver.run_unit({"profile": "store", "worker": 6, "part": 0}, context)
                self.assertEqual(result[0]["compiled"], Counter(names))
                self.assertEqual(result[0]["selected"], result[0]["runs"])
                self.assertEqual(len(result[0]["runs"]), 4)
                self.assertNotIn("private raw output", (root / "trial-timing.jsonl").read_text())
            finally:
                os.chdir(previous)


class WorkflowTests(unittest.TestCase):
    def test_generated_projection_is_reproducible_and_legacy_stays_immutable(self):
        render = load("full-trial-workflow")
        target = SCRIPTS.parent / "workflows/full-trial.yml"
        self.assertEqual(target.read_text(), render.render())
        render.check_legacy(Path.cwd())
        text = target.read_text()
        for key in ("workflow_call:", "sha:", "scheme:", "repeat:", "shard_parallel:"):
            self.assertIn(key, text)
        self.assertIn("fail-fast: false", text)
        self.assertIn("!cancelled()", text)
        self.assertNotIn("secrets:", text)
        self.assertNotIn(": write", text)
        self.assertNotIn("continue-on-error", text)


if __name__ == "__main__":
    unittest.main()
