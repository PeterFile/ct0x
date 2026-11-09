#!/usr/bin/env python3
"""Black-box Store shard gate regressions; never invokes Go."""
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile
import unittest


SCRIPT = Path(__file__).with_name("verify-store-shard.py")
PACKAGE = "agentworkspace/internal/store"
ROOT = "TestCoordination"
IDENTITY = ["--run-id", "42566516008", "--sha", "a" * 40, "--tree", "b" * 40]
# Fixed SHA-256 first-eight-byte big-endian mod-10 vectors, not a second selector.
SLOTS = {"TestCoordination": 1, "TestAlpha": 5, "TestBeta": 5, "TestGamma": 0,
         "TestDelta": 3, "TestNewPrefix": 8, "Test新入口": 0, "TestÉclair": 3,
         "Example": 3, "ExampleWidget_suffix": 7, "FuzzBytes": 8, "TestZero": 4,
         "TestTheta": 6, "TestIota": 2, "TestOne": 9}


def event(action, name="", **fields):
    row = {"Action": action, "Package": PACKAGE, **fields}
    if name:
        row["Test"] = name
    return row


class VerifierTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="store-shard-")
        self.addCleanup(self.temp.cleanup)
        self.directory = Path(self.temp.name)

    def json_lines(self, filename, rows):
        path = self.directory / filename
        path.write_text("".join(json.dumps(row, ensure_ascii=True) + "\n" for row in rows), encoding="utf-8")
        return path

    def inventory(self, names):
        return self.json_lines("list.json", [event("start"), event("output", Output="".join(name + "\n" for name in names)),
                                             event("output", Output=f"ok  \t{PACKAGE}\t0.001s\n"), event("pass")])

    def invoke(self, *args):
        return subprocess.run([sys.executable, str(SCRIPT), *map(str, args)], text=True,
                              capture_output=True, timeout=10)

    def verify(self, rows, names=(ROOT,), slot=1):
        listed = self.inventory(names)
        run = self.json_lines("run.json", [event("start"), *rows, event("pass")])
        return self.invoke("verify", slot, PACKAGE, listed, run, *IDENTITY)

    def success(self, result):
        self.assertEqual(result.returncode, 0, result.stderr)
        return json.loads(result.stdout)

    def artifacts(self, names=tuple(SLOTS)):
        directory = self.directory / "summaries"
        for slot in range(10):
            selected = [name for name in names if SLOTS[name] == slot]
            rows = [row for name in selected for row in (event("run", name), event("pass", name))]
            summary = self.success(self.verify(rows, names, slot))
            path = directory / f"store-shard-v1-{slot}" / "store-summary.json"
            path.parent.mkdir(parents=True)
            path.write_text(json.dumps(summary), encoding="utf-8")
        return directory

    def mutate(self, directory, slot, change):
        path = directory / f"store-shard-v1-{slot}" / "store-summary.json"
        summary = json.loads(path.read_text(encoding="utf-8"))
        change(summary)
        path.write_text(json.dumps(summary), encoding="utf-8")

    def aggregate(self, directory, identity=IDENTITY, package=PACKAGE):
        return self.invoke("aggregate", package, directory, *identity)

    def reject(self, result):
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertIn("Store 分片校验失败", result.stderr)

    def test_valid_nested_lifecycle(self):
        child = ROOT + "/child"
        result = self.verify([event("run", ROOT), event("run", child), event("pause", child),
                              event("cont", child), event("pass", child), event("pass", ROOT)])
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_nested_parallel_lifecycle(self):
        child, nested = ROOT + "/child", ROOT + "/child/nested"
        rows = [event("run", name) for name in [ROOT, child, nested]]
        rows += [event("pause", name) for name in [nested, child, ROOT]]
        rows += [event("cont", name) for name in [ROOT, child, nested]]
        rows += [event("pass", name) for name in [nested, child, ROOT]]
        self.success(self.verify(rows))

    def test_child_output_while_root_and_child_paused(self):
        child = ROOT + "/child"
        # Parallel blocks the test function, not goroutines it already spawned.
        self.success(self.verify([event("run", ROOT), event("run", child), event("pause", child),
                                  event("pause", ROOT), event("output", child, Output="background log\n"),
                                  event("cont", ROOT), event("cont", child), event("pass", child), event("pass", ROOT)]))

    def test_child_output_does_not_resume_or_outlive_entries(self):
        child = ROOT + "/child"
        output = event("output", child, Output="background log\n")
        paused = [event("run", ROOT), event("run", child), event("pause", child), event("pause", ROOT), output]
        completed = [event("run", ROOT), event("run", child), event("pass", child)]
        for rows in [[*paused, event("cont", child), event("pass", child), event("pass", ROOT)],
                     [*paused, event("cont", ROOT), event("pass", child), event("pass", ROOT)],
                     [output, *completed, event("pass", ROOT)],
                     [event("run", ROOT), output, event("run", child), event("pass", child), event("pass", ROOT)],
                     [*completed, output, event("pass", ROOT)], [*completed, event("pass", ROOT), output]]:
            with self.subTest(rows=rows):
                self.reject(self.verify(rows))

    def test_fuzz_seed_and_example_top_counters(self):
        root, seed = "FuzzBytes", "FuzzBytes/seed#0"
        summary = self.success(self.verify([event("run", root), event("run", seed), event("pass", seed), event("pass", root)],
                                           names=[root, "Example"], slot=8))
        self.assertEqual(summary["runs"], {root: 1})
        self.assertEqual(summary["passes"], {root: 1})
        self.success(self.verify([event("run", "Example"), event("pass", "Example")], names=[root, "Example"], slot=3))

    def test_top_skip_rejected(self):
        self.reject(self.verify([event("run", ROOT), event("skip", ROOT)]))

    def test_child_skip_rejected(self):
        child = ROOT + "/child"
        self.reject(self.verify([event("run", ROOT), event("run", child), event("skip", child), event("pass", ROOT)]))

    def test_pause_without_cont_rejected(self):
        self.reject(self.verify([event("run", ROOT), event("pause", ROOT), event("pass", ROOT)]))

    def test_duplicate_pause_rejected(self):
        self.reject(self.verify([event("run", ROOT), event("pause", ROOT), event("pause", ROOT),
                                 event("cont", ROOT), event("pass", ROOT)]))

    def test_cont_without_pause_rejected(self):
        self.reject(self.verify([event("run", ROOT), event("cont", ROOT), event("pass", ROOT)]))

    def test_sequential_slash_siblings(self):
        first, second = ROOT + "/a", ROOT + "/a/b"
        self.success(self.verify([event("run", ROOT), event("run", first), event("pass", first),
                                  event("run", second), event("pass", second), event("pass", ROOT)]))

    def test_parallel_slash_siblings(self):
        first, second = ROOT + "/a", ROOT + "/a/b"
        self.success(self.verify([event("run", ROOT), event("run", first), event("pause", first),
                                  event("run", second), event("pass", second), event("cont", first),
                                  event("pass", first), event("pass", ROOT)]))

    def test_slash_sibling_pass_while_prefixed_sibling_paused(self):
        first, second = ROOT + "/a/b", ROOT + "/a"
        self.success(self.verify([event("run", ROOT), event("run", first), event("pause", first),
                                  event("run", second), event("pass", second), event("cont", first),
                                  event("pass", first), event("pass", ROOT)]))

    def test_inactive_root_rejected(self):
        child = ROOT + "/child"
        # JSON proves top-level ownership, not direct parents of slash labels.
        for rows in [[event("run", child), event("pass", child), event("run", ROOT), event("pass", ROOT)],
                     [event("run", ROOT), event("pause", ROOT), event("run", child), event("pass", child),
                      event("cont", ROOT), event("pass", ROOT)],
                     [event("run", ROOT), event("pass", ROOT), event("run", child), event("pass", child)]]:
            with self.subTest(rows=rows):
                self.reject(self.verify(rows))

    def test_duplicate_subtest_run_rejected(self):
        child = ROOT + "/child"
        self.reject(self.verify([event("run", ROOT), event("run", child), event("pass", child),
                                 event("run", child), event("pass", child), event("pass", ROOT)]))

    def test_fixed_hash_vectors_and_safe_union(self):
        self.assertEqual(set(SLOTS.values()), set(range(10)))
        listed = self.inventory(SLOTS)
        for slot in range(10):
            with self.subTest(slot=slot):
                result = self.invoke("select", slot, PACKAGE, listed)
                self.assertEqual(result.returncode, 0, result.stderr)
                pattern = result.stdout.strip()
                self.assertEqual(pattern, "^(" + "|".join(sorted(name for name in SLOTS if SLOTS[name] == slot)) + ")$")
                self.assertNotIn("/", pattern)
                for name, expected in SLOTS.items():
                    self.assertEqual(re.fullmatch(pattern, name) is not None, slot == expected)
                    self.assertIsNone(re.fullmatch(pattern, name + "Extra"))

    def test_slot_bounds_rejected(self):
        listed = self.inventory([ROOT])
        run = self.json_lines("run.json", [event("start"), event("run", ROOT), event("pass", ROOT), event("pass")])
        for command, args in [("select", [listed]), ("verify", [listed, run, *IDENTITY])]:
            for slot in [-1, 10]:
                with self.subTest(command=command, slot=slot):
                    result = self.invoke(command, slot, PACKAGE, *args)
                    self.assertEqual(result.returncode, 2, result.stderr)
                    self.assertIn("invalid choice", result.stderr)

    def test_empty_slot_not_empty_inventory(self):
        listed = self.inventory([ROOT])
        result = self.invoke("select", 0, PACKAGE, listed)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout.strip(), "^$")
        summary = self.success(self.verify([], slot=0))
        self.assertEqual(summary["selected"], {})
        self.assertEqual(summary["runs"], {})
        self.assertEqual(summary["passes"], {})
        self.reject(self.invoke("select", 0, PACKAGE, self.inventory([])))

    def test_compiled_duplicates_are_not_hidden_by_a_set(self):
        self.reject(self.invoke("select", 1, PACKAGE, self.inventory([ROOT, ROOT])))
        rows = [event("run", ROOT), event("pass", ROOT)] * 2
        self.reject(self.verify(rows, names=[ROOT, ROOT]))

    def test_inventory_identifier_and_format_boundaries(self):
        for name in ["Testlower", "Fuzzlower", "Examplelower", "Test/Child", "Test.Bad", "Test A",
                     " TestHidden", "BenchmarkA", "Test\u0301", "Test\ud800", "Test" + "A" * 4093, "Test" + "界" * 1365]:
            with self.subTest(name=repr(name)):
                self.reject(self.invoke("select", 0, PACKAGE, self.inventory([name])))
        for boundary in ["Test" + "A" * 4092, "Test" + "界" * 1364]:
            for slot in range(10):
                result = self.invoke("select", slot, PACKAGE, self.inventory([boundary]))
                self.assertEqual(result.returncode, 0, result.stderr)
        for text in [ROOT, ROOT + "\ntrailing junk\n", ROOT + "\r\n", ROOT + "\nok  \twrong/package\t0.001s\n"]:
            with self.subTest(text=repr(text)):
                listed = self.json_lines("list.json", [event("start"), event("output", Output=text), event("pass")])
                self.reject(self.invoke("select", 0, PACKAGE, listed))
        listed.write_bytes(b"\xff\n")
        self.reject(self.invoke("select", 0, PACKAGE, listed))

    def test_large_inventory_fails_closed_instead_of_truncating(self):
        for label in ["A" * 296, "界" * 98]:
            listed = self.inventory(["Test" + label + str(index) for index in range(2500)])
            results = [self.invoke("select", slot, PACKAGE, listed) for slot in range(10)]
            self.assertTrue(any(result.returncode != 0 for result in results))
            for result in results:
                if result.returncode != 0:
                    self.assertIn("表达式过长", result.stderr)

    def test_json_and_event_shape_rejected(self):
        listed = self.inventory([ROOT])
        valid = [event("start"), event("run", ROOT), event("pass", ROOT), event("pass")]
        bad_rows = ["{", "[]", '{"Action":"run","Action":"run","Package":"' + PACKAGE + '","Test":"' + ROOT + '"}']
        for value in ["NaN", "Infinity", "-Infinity", "1e999"]:
            bad_rows.append('{"Action":"run","Package":"' + PACKAGE + '","Test":"' + ROOT + '","Elapsed":' + value + '}')
        for field, value in [("Action", "unknown"), ("Action", []), ("Package", "wrong/package"),
                             ("Test", False), ("Test", "Test\ud800"), ("Elapsed", True), ("Elapsed", -1),
                             ("Elapsed", "1"), ("Time", 7)]:
            bad_rows.append(json.dumps({**valid[1], field: value}))
        bad_rows.append(json.dumps(event("output", ROOT, Output=7)))
        for bad in bad_rows:
            with self.subTest(row=bad):
                run = self.directory / "run.json"
                run.write_text(json.dumps(valid[0]) + "\n" + bad + "\n" + "\n".join(json.dumps(row) for row in valid[2:]) + "\n", encoding="utf-8")
                self.reject(self.invoke("verify", 1, PACKAGE, listed, run, *IDENTITY))
        listed.write_text('{"Action":"start","Package":"x","Package":"' + PACKAGE + '"}\n', encoding="utf-8")
        self.reject(self.invoke("select", 1, PACKAGE, listed))

    def test_package_lifecycle_and_inventory_events(self):
        listed = self.inventory([ROOT])
        for rows in [[event("start")], [event("pass")], [event("start"), event("skip")],
                     [event("start"), event("fail")], [event("start"), event("start"), event("pass")],
                     [event("start"), event("pass"), event("output", Output="late\n")],
                     [event("start"), event("run"), event("pass")]]:
            with self.subTest(rows=rows):
                run = self.json_lines("run.json", rows)
                self.reject(self.invoke("verify", 1, PACKAGE, listed, run, *IDENTITY))
        listed = self.json_lines("list.json", [event("start"), event("run", ROOT), event("pass", ROOT), event("pass")])
        self.reject(self.invoke("select", 1, PACKAGE, listed))

    def test_top_and_child_event_integrity(self):
        child = ROOT + "/child"
        good = [event("run", ROOT), event("pass", ROOT)]
        cases = [[], good * 2, [*good, event("pass", ROOT)], [event("pass", ROOT)],
                 [event("run", ROOT)], [event("run", child), *good],
                 [event("run", ROOT), event("run", "TestNewPrefix"), event("pass", "TestNewPrefix"), event("pass", ROOT)],
                 [event("run", ROOT), event("fail", ROOT)],
                 [event("run", ROOT), event("run", child), event("fail", child), event("pass", ROOT)],
                 [*good, event("output", ROOT, Output="late\n")],
                 [event("run", ROOT), event("pass", child), event("pass", ROOT)],
                 [event("run", ROOT), event("run", child), event("pass", ROOT)],
                 [event("run", ROOT), event("run", child), event("pass", child), event("pass", child), event("pass", ROOT)],
                 [event("run", ROOT), event("pause", ROOT), event("run", child), event("pass", child), event("cont", ROOT), event("pass", ROOT)],
                 [event("run", ROOT), event("pause", ROOT), event("cont", ROOT), event("cont", ROOT), event("pass", ROOT)]]
        for rows in cases:
            with self.subTest(rows=rows):
                self.reject(self.verify(rows, names=[ROOT, "TestNewPrefix"]))

    def test_summary_identity_and_top_counters(self):
        child = ROOT + "/embedded/slash#00"
        summary = self.success(self.verify([event("run", ROOT), event("run", child),
                                            event("output", child, Output="hello\n"),
                                            event("attr", child, Key="key", Value="value"),
                                            event("artifacts", child), event("pass", child), event("pass", ROOT)]))
        self.assertEqual(summary, {"schema": 1, "slot": 1, "total": 10, "run_id": IDENTITY[1],
                                   "checkout_sha": IDENTITY[3], "checkout_tree": IDENTITY[5], "package": PACKAGE,
                                   "compiled": {ROOT: 1}, "selected": {ROOT: 1}, "runs": {ROOT: 1},
                                   "passes": {ROOT: 1}, "skips": {}})
        for index, value in [(1, "0"), (1, "other-run"), (3, "short"), (5, "C" * 40)]:
            identity = list(IDENTITY)
            identity[index] = value
            listed = self.inventory([ROOT])
            run = self.json_lines("run.json", [event("start"), event("run", ROOT), event("pass", ROOT), event("pass")])
            self.reject(self.invoke("verify", 1, PACKAGE, listed, run, *identity))

    def test_aggregate_good_all_kinds_and_empty_slots(self):
        directory = self.artifacts()
        summary = self.success(self.aggregate(directory))
        self.assertEqual(summary["total"], 10)
        for field in ["compiled", "runs", "passes"]:
            self.assertEqual(summary[field], {name: 1 for name in SLOTS})
        self.assertEqual(summary["skips"], {})
        # Same-run partial retry replaces only this artifact, without attempt coupling.
        path = directory / "store-shard-v1-0" / "store-summary.json"
        path.write_text(json.dumps(json.loads(path.read_text()), sort_keys=True), encoding="utf-8")
        self.success(self.aggregate(directory))
        other = self.directory / "empty-slots"
        directory.rename(other)
        directory = self.artifacts(names=[ROOT])
        self.success(self.aggregate(directory))
        # An empty selection still needs its own current-run artifact.
        (directory / "store-shard-v1-4").rename(self.directory / "empty-slot")
        self.reject(self.aggregate(directory))

    def test_aggregate_missing_duplicate_extra_and_unsafe_files(self):
        directory = self.artifacts()
        for slot in [5, 4]:
            artifact = directory / f"store-shard-v1-{slot}"
            artifact.rename(self.directory / artifact.name)
            self.reject(self.aggregate(directory))
        for slot in [4, 5]:
            artifact = self.directory / f"store-shard-v1-{slot}"
            artifact.rename(directory / artifact.name)
        path = directory / "store-shard-v1-0" / "store-summary.json"
        data = path.read_bytes()
        path.unlink()
        self.reject(self.aggregate(directory))
        path.write_bytes(data)
        extra = directory / "store-shard-v1-extra"
        extra.mkdir()
        (extra / "store-summary.json").write_bytes(data)
        self.reject(self.aggregate(directory))
        (extra / "store-summary.json").unlink()
        extra.rmdir()
        unknown = path.parent / "second-summary.txt"
        unknown.write_bytes(data)
        self.reject(self.aggregate(directory))
        unknown.unlink()
        self.mutate(directory, 1, lambda summary: summary.update(slot=0))
        self.reject(self.aggregate(directory))
        self.mutate(directory, 1, lambda summary: summary.update(slot=1))
        target = self.directory / "outside.json"
        path.rename(target)
        path.symlink_to(target)
        self.reject(self.aggregate(directory))

    def test_aggregate_compiled_must_agree(self):
        directory = self.artifacts()
        self.mutate(directory, 0, lambda summary: summary["compiled"].pop(ROOT))
        self.reject(self.aggregate(directory))

    def test_aggregate_per_slot_multiset_rejected(self):
        directory = self.artifacts()
        for slot in range(10):
            name = next(name for name in SLOTS if SLOTS[name] == slot)
            for field in ["selected", "runs", "passes"]:
                with self.subTest(slot=slot, field=field):
                    self.mutate(directory, slot, lambda summary: summary[field].update({name: 2}))
                    self.reject(self.aggregate(directory))
                    self.mutate(directory, slot, lambda summary: summary[field].update({name: 1}))

    def test_aggregate_counter_and_schema_validation(self):
        directory = self.artifacts()
        path = directory / "store-shard-v1-1" / "store-summary.json"
        original = path.read_text(encoding="utf-8")
        changes = [("schema", False), ("schema", True), ("schema", 0), ("schema", 1.0),
                   ("total", True), ("total", 3), ("total", 4), ("total", 4.0), ("total", 10.0),
                   ("slot", False), ("slot", -1), ("slot", 4), ("slot", 10), ("slot", 1.0), ("run_id", 42566516008),
                   ("compiled", {}), ("selected", {}), ("runs", {}), ("passes", {}), ("skips", {ROOT: 1})]
        for field in ["compiled", "selected", "runs", "passes", "skips"]:
            for value in [True, False, -1, 0, 1.0, "1", 2]:
                changes.append((field, {**json.loads(original)[field], ROOT: value}))
        self.mutate(directory, 0, lambda summary: summary.update(slot=False))
        self.reject(self.aggregate(directory))
        self.mutate(directory, 0, lambda summary: summary.update(slot=0))
        for field, value in changes:
            with self.subTest(field=field, value=value):
                path.write_text(original, encoding="utf-8")
                self.mutate(directory, 1, lambda summary: summary.update({field: value}))
                self.reject(self.aggregate(directory))
        for change in [lambda summary: summary.update(extra=True), lambda summary: summary.pop("schema")]:
            path.write_text(original, encoding="utf-8")
            self.mutate(directory, 1, change)
            self.reject(self.aggregate(directory))
        for raw in [original.replace('"schema": 1', '"schema": 1, "schema": 1'),
                    original.replace('"' + ROOT + '": 1', '"' + ROOT + '": 1, "' + ROOT + '": 1', 1),
                    original.replace('"schema": 1', '"schema": NaN')]:
            path.write_text(raw, encoding="utf-8")
            self.reject(self.aggregate(directory))

    def test_aggregate_hash_recomputed_not_just_coverage(self):
        directory = self.artifacts()
        for slot in [0, 1]:
            def move(summary, slot=slot):
                for field in ["selected", "runs", "passes"]:
                    if slot == 0:
                        summary[field][ROOT] = 1
                    else:
                        summary[field].pop(ROOT)
            self.mutate(directory, slot, move)
        self.reject(self.aggregate(directory))

    def test_aggregate_external_identity_not_only_mutual_agreement(self):
        directory = self.artifacts()
        for index, value in [(1, "42566516009"), (3, "c" * 40), (5, "d" * 40)]:
            with self.subTest(index=index):
                identity = list(IDENTITY)
                identity[index] = value
                self.reject(self.aggregate(directory, identity=identity))
        self.reject(self.aggregate(directory, package="wrong/package"))
        for field, value in [("run_id", "42566516009"), ("checkout_sha", "c" * 40),
                             ("checkout_tree", "d" * 40), ("package", "wrong/package")]:
            self.mutate(directory, 0, lambda summary: summary.update({field: value}))
            self.reject(self.aggregate(directory))
            self.mutate(directory, 0, lambda summary: summary.update({field: {"run_id": IDENTITY[1], "checkout_sha": IDENTITY[3],
                                                                            "checkout_tree": IDENTITY[5], "package": PACKAGE}[field]}))


class WorkflowTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.workflow = SCRIPT.parent.parent.joinpath("workflows/ci.yml").read_text(encoding="utf-8")

    def test_matrix_success_is_first_gate_even_with_always(self):
        match = re.search(r"\n  go:\n(?P<job>.*?)(?=\n  web:)", self.workflow, re.S)
        self.assertIsNotNone(match)
        job = match.group("job")
        self.assertIn("name: Go\n", job)
        self.assertIn("needs: [go-store]", job)
        self.assertIn("if: ${{ always() }}", job)
        gate = re.search(r"steps:\n      - name: Check Store shards\n        env:\n          STORE_RESULT: \$\{\{ needs.go-store.result \}\}\n        run: ([^\n]+)", job)
        self.assertIsNotNone(gate, "matrix result check must be the first step")
        for state in ["success", "failure", "cancelled", "skipped", "", "unknown"]:
            with self.subTest(state=state):
                result = subprocess.run(["bash", "-c", gate.group(1)], env={**os.environ, "STORE_RESULT": state}, capture_output=True, timeout=5)
                self.assertEqual(result.returncode == 0, state == "success")
        positions = [job.index(text) for text in ["Check Store shards", "actions/checkout@v7", "actions/download-artifact@v7",
                                                 "verify-store-shard.py aggregate", "Race test all other packages", "go vet ./...", "Record benchmark metrics"]]
        self.assertEqual(positions, sorted(positions))
        self.assertIn('--run-id "$GITHUB_RUN_ID" --sha "$(git rev-parse HEAD)" --tree "$(git rev-parse HEAD^{tree})"', job)
        self.assertIn('package=$(go list ./internal/store)', job)

    def test_workflow_ten_slots_real_uncached_runs_and_artifact_isolation(self):
        store = self.workflow.split("\n  go-store:\n", 1)
        self.assertEqual(len(store), 2)
        store = store[1].split("\n  go:\n", 1)[0]
        for text in ["fail-fast: false", "slot: [0, 1, 2, 3, 4, 5, 6, 7, 8, 9]", "timeout-minutes: 15",
                     "go test -count=1 -race -json -list '^(Test|Example|Fuzz)' ./internal/store",
                     'go test -count=1 -race -json -run "$pattern" ./internal/store',
                     'package=$(go list ./internal/store)', 'git rev-parse HEAD', 'git rev-parse HEAD^{tree}',
                     '"$GITHUB_RUN_ID"', "verify-store-shard.py select", "verify-store-shard.py verify",
                     "actions/upload-artifact@v7", "name: store-shard-v1-${{ matrix.slot }}", "overwrite: true",
                     "if-no-files-found: error"]:
            self.assertIn(text, store)
        self.assertNotIn("-timeout", store)
        self.assertNotIn("STORE_RACE_GROUP", self.workflow)
        self.assertIn("pattern: store-shard-v1-*", self.workflow)
        self.assertIn("merge-multiple: false", self.workflow)
        self.assertEqual(self.workflow.count("python3 .github/scripts/test-store-shard.py"), 1)
        self.assertIn("name: go-benchmarks", self.workflow)
        self.assertNotIn("fixture", self.workflow.lower())


if __name__ == "__main__":
    unittest.main()
