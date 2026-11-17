#!/usr/bin/env python3
"""Black-box Store/Platform shard gate regressions; never invokes Go."""
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile
import textwrap
import unittest


SCRIPT = Path(__file__).with_name("verify-store-shard.py")
PACKAGE = "agentworkspace/internal/store"
ROOT = "TestCoordination"
IDENTITY = ["--run-id", "42566516008", "--sha", "a" * 40, "--tree", "b" * 40]
# Fixed SHA-256 first-eight-byte big-endian mod-64 vectors, not a second selector.
SLOTS = {"TestCoordination": 23, "TestAlpha": 27, "TestBeta": 51, "TestGamma": 12,
         "TestDelta": 15, "TestNewPrefix": 50, "Test新入口": 50, "TestÉclair": 37,
         "Example": 61, "ExampleWidget_suffix": 21, "FuzzBytes": 20, "TestZero": 24,
         "TestTheta": 56, "TestIota": 14, "TestOne": 27, "TestThree": 19,
         "TestVector77": 8, "TestVector5": 12, "TestSeven": 25, "TestVector2": 44,
         "TestFifteen": 19, "TestFive": 2, "TestVector6": 0, "TestVector0": 1,
         "TestVector11": 35, "TestVector116": 36, "TestVector18": 6, "TestVector10": 7,
         "TestVector136": 41, "TestVector25": 10, "TestVector36": 11, "TestVector23": 13,
         "TestVector12": 16, "TestVector15": 49, "TestVector16": 22, "TestVector7": 58,
         "TestVector31": 60, "TestVector51": 30, "TestVector3": 31, "TestVector4": 53,
         "TestVector8": 18, "TestVector14": 46, "TestVector19": 5, "TestVector21": 55,
         "TestVector22": 33, "TestVector28": 47, "TestVector30": 52, "TestVector32": 3,
         "TestVector33": 45, "TestVector43": 40, "TestVector46": 39, "TestVector48": 17,
         "TestVector55": 48, "TestVector60": 28, "TestVector65": 57, "TestVector86": 43,
         "TestVector92": 34, "TestVector93": 38, "TestVector95": 26, "TestVector113": 42,
         "TestVector123": 62, "TestVector151": 54, "TestVector152": 9, "TestVector194": 29,
         "TestVector201": 32, "TestVector202": 63, "TestVector282": 59, "TestVector283": 4}
PLATFORM_PACKAGE = "agentworkspace/internal/platform"
COORDINATION_PACKAGE = "agentworkspace/internal/coordination"
# Fixed SHA-256 first-eight-byte big-endian mod-4 vectors.
PLATFORM_SLOTS = {"TestCoordination": 3, "TestAlpha": 3, "TestBeta": 3, "TestGamma": 0,
                  "Test新入口": 2, "Example": 1, "FuzzBytes": 0}


def event(action, name="", **fields):
    row = {"Action": action, "Package": PACKAGE, **fields}
    if name:
        row["Test"] = name
    return row


def platform_event(action, name="", **fields):
    return event(action, name, Package=PLATFORM_PACKAGE, **fields)


class VerifierTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="store-shard-")
        self.addCleanup(self.temp.cleanup)
        self.directory = Path(self.temp.name)

    def json_lines(self, filename, rows):
        path = self.directory / filename
        path.write_text("".join(json.dumps(row, ensure_ascii=True) + "\n" for row in rows), encoding="utf-8")
        return path

    def inventory(self, names, package=PACKAGE):
        return self.json_lines("list.json", [event("start", Package=package),
                                             event("output", Package=package, Output="".join(name + "\n" for name in names)),
                                             event("output", Package=package, Output=f"ok  \t{package}\t0.001s\n"),
                                             event("pass", Package=package)])

    def invoke(self, *args):
        return subprocess.run([sys.executable, str(SCRIPT), *map(str, args)], text=True,
                              capture_output=True, timeout=10)

    def verify(self, rows, names=(ROOT,), slot=23, profile=None):
        package = PLATFORM_PACKAGE if profile == "platform" else PACKAGE
        listed = self.inventory(names, package)
        run = self.json_lines("run.json", [event("start", Package=package), *rows, event("pass", Package=package)])
        options = ["--profile", profile] if profile else []
        return self.invoke("verify", slot, package, listed, run, *IDENTITY, *options)

    def success(self, result):
        self.assertEqual(result.returncode, 0, result.stderr)
        return json.loads(result.stdout)

    def artifacts(self, names=tuple(SLOTS), profile=None):
        directory = self.directory / "summaries"
        suite = profile or "store"
        slots, total = (PLATFORM_SLOTS, 4) if profile == "platform" else (SLOTS, 64)
        make_event = platform_event if profile == "platform" else event
        for slot in range(total):
            selected = [name for name in names if slots[name] == slot]
            rows = [row for name in selected for row in (make_event("run", name), make_event("pass", name))]
            summary = self.success(self.verify(rows, names, slot, profile))
            path = directory / f"{suite}-shard-v1-{slot}" / f"{suite}-summary.json"
            path.parent.mkdir(parents=True)
            path.write_text(json.dumps(summary), encoding="utf-8")
        return directory

    def mutate(self, directory, slot, change, profile=None):
        suite = profile or "store"
        path = directory / f"{suite}-shard-v1-{slot}" / f"{suite}-summary.json"
        summary = json.loads(path.read_text(encoding="utf-8"))
        change(summary)
        path.write_text(json.dumps(summary), encoding="utf-8")

    def aggregate(self, directory, identity=IDENTITY, package=None, profile=None):
        package = package if package is not None else (PLATFORM_PACKAGE if profile == "platform" else PACKAGE)
        options = ["--profile", profile] if profile else []
        return self.invoke("aggregate", package, directory, *identity, *options)

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
                                           names=[root, "Example"], slot=20))
        self.assertEqual(summary["runs"], {root: 1})
        self.assertEqual(summary["passes"], {root: 1})
        self.success(self.verify([event("run", "Example"), event("pass", "Example")], names=[root, "Example"], slot=61))

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
        self.assertEqual(set(SLOTS.values()), set(range(64)))
        listed = self.inventory(SLOTS)
        for slot in range(64):
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
            for slot in [-1, 64]:
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
        self.reject(self.invoke("select", 23, PACKAGE, self.inventory([ROOT, ROOT])))
        rows = [event("run", ROOT), event("pass", ROOT)] * 2
        self.reject(self.verify(rows, names=[ROOT, ROOT]))

    def test_inventory_identifier_and_format_boundaries(self):
        for name in ["Testlower", "Fuzzlower", "Examplelower", "Test/Child", "Test.Bad", "Test A",
                     " TestHidden", "BenchmarkA", "Test\u0301", "Test\ud800", "Test" + "A" * 4093, "Test" + "界" * 1365]:
            with self.subTest(name=repr(name)):
                self.reject(self.invoke("select", 0, PACKAGE, self.inventory([name])))
        for boundary in ["Test" + "A" * 4092, "Test" + "界" * 1364]:
            for slot in range(64):
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
            listed = self.inventory(["Test" + label + str(index) for index in range(16000)])
            results = [self.invoke("select", slot, PACKAGE, listed) for slot in range(64)]
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
                self.reject(self.invoke("verify", 23, PACKAGE, listed, run, *IDENTITY))
        listed.write_text('{"Action":"start","Package":"x","Package":"' + PACKAGE + '"}\n', encoding="utf-8")
        self.reject(self.invoke("select", 23, PACKAGE, listed))

    def test_package_lifecycle_and_inventory_events(self):
        listed = self.inventory([ROOT])
        for rows in [[event("start")], [event("pass")], [event("start"), event("skip")],
                     [event("start"), event("fail")], [event("start"), event("start"), event("pass")],
                     [event("start"), event("pass"), event("output", Output="late\n")],
                     [event("start"), event("run"), event("pass")]]:
            with self.subTest(rows=rows):
                run = self.json_lines("run.json", rows)
                self.reject(self.invoke("verify", 23, PACKAGE, listed, run, *IDENTITY))
        listed = self.json_lines("list.json", [event("start"), event("run", ROOT), event("pass", ROOT), event("pass")])
        self.reject(self.invoke("select", 23, PACKAGE, listed))

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
        self.assertEqual(summary, {"schema": 1, "slot": 23, "total": 64, "run_id": IDENTITY[1],
                                   "checkout_sha": IDENTITY[3], "checkout_tree": IDENTITY[5], "package": PACKAGE,
                                   "compiled": {ROOT: 1}, "selected": {ROOT: 1}, "runs": {ROOT: 1},
                                   "passes": {ROOT: 1}, "skips": {}})
        for index, value in [(1, "0"), (1, "other-run"), (3, "short"), (5, "C" * 40)]:
            identity = list(IDENTITY)
            identity[index] = value
            listed = self.inventory([ROOT])
            run = self.json_lines("run.json", [event("start"), event("run", ROOT), event("pass", ROOT), event("pass")])
            self.reject(self.invoke("verify", 23, PACKAGE, listed, run, *identity))

    def test_aggregate_good_all_kinds_and_empty_slots(self):
        directory = self.artifacts()
        summary = self.success(self.aggregate(directory))
        self.assertEqual(summary["total"], 64)
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
        for slot in [63, 5, 4]:
            artifact = directory / f"store-shard-v1-{slot}"
            artifact.rename(self.directory / artifact.name)
            self.reject(self.aggregate(directory))
        for slot in [4, 5, 63]:
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
        self.mutate(directory, 63, lambda summary: summary.update(slot=0))
        self.reject(self.aggregate(directory))
        self.mutate(directory, 63, lambda summary: summary.update(slot=63))
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
        for slot in range(64):
            name = next(name for name in SLOTS if SLOTS[name] == slot)
            for field in ["selected", "runs", "passes"]:
                with self.subTest(slot=slot, field=field):
                    self.mutate(directory, slot, lambda summary: summary[field].update({name: 2}))
                    self.reject(self.aggregate(directory))
                    self.mutate(directory, slot, lambda summary: summary[field].update({name: 1}))

    def test_aggregate_counter_and_schema_validation(self):
        directory = self.artifacts()
        path = directory / "store-shard-v1-23" / "store-summary.json"
        original = path.read_text(encoding="utf-8")
        changes = [("schema", False), ("schema", True), ("schema", 0), ("schema", 1.0),
                   ("total", True), ("total", 3), ("total", 4), ("total", 4.0), ("total", 18.0), ("total", 32.0), ("total", 64.0),
                   ("slot", False), ("slot", -1), ("slot", 4), ("slot", 64), ("slot", 23.0), ("run_id", 42566516008),
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
                self.mutate(directory, 23, lambda summary: summary.update({field: value}))
                self.reject(self.aggregate(directory))
        for change in [lambda summary: summary.update(extra=True), lambda summary: summary.pop("schema")]:
            path.write_text(original, encoding="utf-8")
            self.mutate(directory, 23, change)
            self.reject(self.aggregate(directory))
        for raw in [original.replace('"schema": 1', '"schema": 1, "schema": 1'),
                    original.replace('"' + ROOT + '": 1', '"' + ROOT + '": 1, "' + ROOT + '": 1', 1),
                    original.replace('"schema": 1', '"schema": NaN')]:
            path.write_text(raw, encoding="utf-8")
            self.reject(self.aggregate(directory))

    def test_aggregate_legacy_and_mixed_totals_rejected(self):
        directory = self.artifacts()
        for total in [18, 24, 32, 48]:
            for slots in [[63], range(64)]:
                with self.subTest(total=total, slots=list(slots)):
                    for slot in slots:
                        self.mutate(directory, slot, lambda summary: summary.update(total=total))
                    self.reject(self.aggregate(directory))
                    for slot in slots:
                        self.mutate(directory, slot, lambda summary: summary.update(total=64))
        self.success(self.aggregate(directory))

    def test_aggregate_hash_recomputed_not_just_coverage(self):
        directory = self.artifacts()
        for slot in [63, 23]:
            def move(summary, slot=slot):
                for field in ["selected", "runs", "passes"]:
                    if slot == 63:
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

    def test_profile_package_binding_even_when_all_evidence_agrees(self):
        # The default Store CLI must not accept another package's consistent evidence.
        for package in [PLATFORM_PACKAGE, "other/internal/store"]:
            listed = self.inventory([ROOT], package)
            run = self.json_lines("run.json", [event("start", Package=package), event("run", ROOT, Package=package),
                                                event("pass", ROOT, Package=package), event("pass", Package=package)])
            for command, args in [("select", [listed]), ("verify", [listed, run, *IDENTITY])]:
                with self.subTest(package=package, command=command):
                    self.reject(self.invoke(command, 23, package, *args))
        directory = self.artifacts()
        for slot in range(64):
            self.mutate(directory, slot, lambda summary: summary.update(package=PLATFORM_PACKAGE))
        self.reject(self.aggregate(directory, package=PLATFORM_PACKAGE))

    def test_platform_fixed_vectors_full_coverage_and_empty_slots(self):
        listed = self.inventory(PLATFORM_SLOTS, PLATFORM_PACKAGE)
        for slot in range(4):
            result = self.invoke("select", slot, PLATFORM_PACKAGE, listed, "--profile", "platform")
            self.assertEqual(result.returncode, 0, result.stderr)
            expected = sorted(name for name, assigned in PLATFORM_SLOTS.items() if assigned == slot)
            self.assertEqual(result.stdout.strip(), "^(" + "|".join(expected) + ")$")
            self.assertIsNone(re.fullmatch(result.stdout.strip(), expected[0] + "Extra"))
        directory = self.artifacts(PLATFORM_SLOTS, "platform")
        summary = self.success(self.aggregate(directory, profile="platform"))
        self.assertEqual((summary["schema"], summary["total"], summary["package"]), (1, 4, PLATFORM_PACKAGE))
        for field in ["compiled", "runs", "passes"]:
            self.assertEqual(summary[field], {name: 1 for name in PLATFORM_SLOTS})
        self.assertEqual(summary["skips"], {})
        directory.rename(self.directory / "full-slots")
        directory = self.artifacts([ROOT], "platform")
        self.success(self.aggregate(directory, profile="platform"))
        empty = json.loads((directory / "platform-shard-v1-0/platform-summary.json").read_text())
        self.assertEqual(empty["compiled"], {ROOT: 1})
        for field in ["selected", "runs", "passes", "skips"]:
            self.assertEqual(empty[field], {})
        (directory / "platform-shard-v1-0").rename(self.directory / "empty-slot")
        self.reject(self.aggregate(directory, profile="platform"))

    def test_profiles_are_bounded_and_cross_package_select_verify_fail(self):
        listed = self.inventory([ROOT], PLATFORM_PACKAGE)
        run = self.json_lines("run.json", [platform_event("start"), platform_event("run", ROOT),
                                            platform_event("pass", ROOT), platform_event("pass")])
        for command, args in [("select", [listed]), ("verify", [listed, run, *IDENTITY])]:
            for slot in [-1, 4, 18]:
                with self.subTest(command=command, slot=slot):
                    result = self.invoke(command, slot, PLATFORM_PACKAGE, *args, "--profile", "platform")
                    self.assertEqual(result.returncode, 2, result.stderr)
                    self.assertIn("invalid choice", result.stderr)
            for profile, package in [("store", PLATFORM_PACKAGE), ("platform", PACKAGE)]:
                with self.subTest(command=command, profile=profile):
                    listed = self.inventory([ROOT], package)
                    run = self.json_lines("run.json", [event("start", Package=package), event("run", ROOT, Package=package),
                                                        event("pass", ROOT, Package=package), event("pass", Package=package)])
                    evidence = [listed] if command == "select" else [listed, run, *IDENTITY]
                    self.reject(self.invoke(command, 3, package, *evidence, "--profile", profile))
        listed = self.inventory([ROOT])
        for options in [("--profile", "other"), ("--total", "4"), ("--total", "64"), ("--profile", "platform", "--total", "18")]:
            result = self.invoke("select", 0, PACKAGE, listed, *options)
            self.assertEqual(result.returncode, 2, result.stderr)
        explicit = self.invoke("select", 23, PACKAGE, listed, "--profile", "store")
        legacy = self.invoke("select", 23, PACKAGE, listed)
        self.assertEqual(explicit.returncode, 0, explicit.stderr)
        self.assertEqual(explicit.stdout, legacy.stdout)

    def test_platform_nested_lifecycle_and_bad_execution_evidence(self):
        child = ROOT + "/child"
        rows = [platform_event("run", ROOT), platform_event("run", child), platform_event("pause", child),
                platform_event("cont", child), platform_event("pass", child), platform_event("pass", ROOT)]
        summary = self.success(self.verify(rows, slot=3, profile="platform"))
        self.assertEqual(summary["selected"], {ROOT: 1})
        good = [platform_event("run", ROOT), platform_event("pass", ROOT)]
        cases = [[], good * 2, [platform_event("run", ROOT)], [platform_event("pass", ROOT)]]
        for action in ["skip", "fail"]:
            cases.extend([[platform_event("run", ROOT), platform_event(action, ROOT)],
                          [platform_event("run", ROOT), platform_event("run", child), platform_event(action, child),
                           platform_event("pass", ROOT)]])
        for rows in cases:
            with self.subTest(rows=rows):
                self.reject(self.verify(rows, slot=3, profile="platform"))
        listed = self.inventory([ROOT], PLATFORM_PACKAGE)
        for rows in [[platform_event("start"), *good], [platform_event("start"), platform_event("skip")],
                     [platform_event("start"), platform_event("fail")]]:
            run = self.json_lines("run.json", rows)
            self.reject(self.invoke("verify", 3, PLATFORM_PACKAGE, listed, run, *IDENTITY, "--profile", "platform"))
        run = self.json_lines("run.json", [platform_event("start"), *good, platform_event("pass")])
        run.write_bytes(run.read_bytes()[:-1])
        self.reject(self.invoke("verify", 3, PLATFORM_PACKAGE, listed, run, *IDENTITY, "--profile", "platform"))
        listed.write_bytes(listed.read_bytes()[:-1])
        self.reject(self.invoke("select", 3, PLATFORM_PACKAGE, listed, "--profile", "platform"))
        self.reject(self.invoke("select", 0, PLATFORM_PACKAGE, self.inventory([], PLATFORM_PACKAGE), "--profile", "platform"))

    def test_platform_aggregate_identity_profile_and_prefix_binding(self):
        directory = self.artifacts(PLATFORM_SLOTS, "platform")
        for index, value in [(1, "42566516009"), (3, "c" * 40), (5, "d" * 40)]:
            identity = list(IDENTITY)
            identity[index] = value
            self.reject(self.aggregate(directory, identity=identity, profile="platform"))
        for field, value, original in [("run_id", "42566516009", IDENTITY[1]), ("checkout_sha", "c" * 40, IDENTITY[3]),
                                       ("checkout_tree", "d" * 40, IDENTITY[5]), ("package", PACKAGE, PLATFORM_PACKAGE)]:
            for slot in range(4):
                self.mutate(directory, slot, lambda summary: summary.update({field: value}), "platform")
            self.reject(self.aggregate(directory, profile="platform"))
            if field == "package":
                self.reject(self.aggregate(directory, package=PACKAGE, profile="platform"))
            for slot in range(4):
                self.mutate(directory, slot, lambda summary: summary.update({field: original}), "platform")
        self.reject(self.aggregate(directory))
        path = directory / "platform-shard-v1-0/platform-summary.json"
        path.rename(path.with_name("store-summary.json"))
        self.reject(self.aggregate(directory, profile="platform"))
        path.with_name("store-summary.json").rename(path)
        for slot in range(4):
            (directory / f"platform-shard-v1-{slot}").rename(directory / f"store-shard-v1-{slot}")
        self.reject(self.aggregate(directory, profile="platform"))

    def test_platform_aggregate_missing_duplicate_extra_and_compiled_inventory(self):
        directory = self.artifacts(PLATFORM_SLOTS, "platform")
        artifact = directory / "platform-shard-v1-1"
        artifact.rename(self.directory / "missing")
        self.reject(self.aggregate(directory, profile="platform"))
        (self.directory / "missing").rename(artifact)
        self.mutate(directory, 1, lambda summary: summary.update(slot=0), "platform")
        self.reject(self.aggregate(directory, profile="platform"))
        self.mutate(directory, 1, lambda summary: summary.update(slot=1), "platform")
        extra = directory / "platform-shard-v1-4"
        extra.mkdir()
        self.reject(self.aggregate(directory, profile="platform"))
        extra.rmdir()
        self.mutate(directory, 0, lambda summary: summary["compiled"].pop(ROOT), "platform")
        self.reject(self.aggregate(directory, profile="platform"))
        for slot in range(4):
            self.mutate(directory, slot, lambda summary: summary.update(compiled={}, selected={}, runs={}, passes={}), "platform")
        self.reject(self.aggregate(directory, profile="platform"))

    def test_platform_aggregate_recomputes_hash_despite_complete_union(self):
        directory = self.artifacts(PLATFORM_SLOTS, "platform")
        # Both wrong assignments still partition the complete inventory exactly once.
        assignments = [{"TestCoordination": 3, "TestAlpha": 1, "TestBeta": 1, "TestGamma": 2,
                        "Test新入口": 0, "Example": 1, "FuzzBytes": 0},  # Legacy Store18 mod 4, not Platform4.
                       {name: (slot + 1) % 4 for name, slot in PLATFORM_SLOTS.items()}]
        for assigned in assignments:
            for slot in range(4):
                selected = {name: 1 for name in assigned if assigned[name] == slot}
                self.mutate(directory, slot, lambda summary: summary.update(selected=selected, runs=selected, passes=selected), "platform")
            self.reject(self.aggregate(directory, profile="platform"))


class WorkflowTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.workflow = SCRIPT.parent.parent.joinpath("workflows/ci.yml").read_text(encoding="utf-8")

    def test_matrix_success_is_first_gate_even_with_always(self):
        match = re.search(r"\n  go:\n(?P<job>.*?)(?=\n  web:)", self.workflow, re.S)
        self.assertIsNotNone(match)
        job = match.group("job")
        self.assertIn("name: Go\n", job)
        self.assertIn("needs: [go-store, go-platform]", job)
        self.assertIn("if: ${{ always() }}", job)
        gate = re.search(r"steps:\n      - name: Check Store and Platform shards\n        env:\n          STORE_RESULT: \$\{\{ needs.go-store.result \}\}\n          PLATFORM_RESULT: \$\{\{ needs.go-platform.result \}\}\n        run: ([^\n]+)", job)
        self.assertIsNotNone(gate, "both matrix results must be checked in the first step")
        states = ["success", "failure", "cancelled", "skipped", "", "unknown"]
        for store_state in states:
            for platform_state in states:
                with self.subTest(store=store_state, platform=platform_state):
                    result = subprocess.run(["bash", "-c", gate.group(1)],
                                            env={**os.environ, "STORE_RESULT": store_state, "PLATFORM_RESULT": platform_state},
                                            capture_output=True, timeout=5)
                    self.assertEqual(result.returncode == 0, store_state == platform_state == "success")
        positions = [job.index(text) for text in ["Check Store and Platform shards", "actions/checkout@v7", "actions/download-artifact@v7",
                                                 "Verify Store shard coverage", "Verify Platform shard coverage",
                                                 "Ordinary test coordination package", "Race test coordination package",
                                                 "Ordinary test cmd package", "Race test all other packages", "go vet ./...", "Record benchmark metrics"]]
        self.assertEqual(positions, sorted(positions))
        coordination_steps = [("Ordinary test coordination package", "go test -count=1 ./internal/coordination"),
                              ("Race test coordination package", "go test -count=1 -race ./internal/coordination")]
        for name, command in coordination_steps:
            steps = re.findall(rf"^      - name: {re.escape(name)}\n        run: ([^\n]+)$", job, re.M)
            self.assertEqual(steps, [command])
        commands = re.findall(r"^[ \t]+(?:run: )?(go test[^\n]*)$", job, re.M)
        self.assertEqual([command for command in commands if "./internal/coordination" in command.split()],
                         [command for _, command in coordination_steps])
        cmd_command = "go test -count=1 ./cmd/workspace"
        cmd_steps = re.findall(r"^      - name: Ordinary test cmd package\n        run: ([^\n]+)$", job, re.M)
        self.assertEqual(cmd_steps, [cmd_command])
        self.assertEqual([command for command in commands if "./cmd/workspace" in command.split()], [cmd_command])
        self.assertIn('--run-id "$GITHUB_RUN_ID" --sha "$(git rev-parse HEAD)" --tree "$(git rev-parse HEAD^{tree})"', job)
        self.assertIn('package=$(go list ./internal/store)', job)

    def test_workflow_sixty_four_slots_real_uncached_runs_and_artifact_isolation(self):
        store = self.workflow.split("\n  go-store:\n", 1)
        self.assertEqual(len(store), 2)
        store = store[1].split("\n  go-platform:\n", 1)[0]
        matrices = re.findall(r"^        slot: (\[[^\n]+\])$", store, re.M)
        self.assertEqual(len(matrices), 1)
        self.assertEqual(json.loads(matrices[0]), list(range(64)))
        for text in ["fail-fast: false", "timeout-minutes: 15",
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

    def test_platform_matrix_and_independent_aggregate(self):
        platform = re.search(r"\n  go-platform:\n(.*?)(?=\n  go:)", self.workflow, re.S)
        self.assertIsNotNone(platform)
        for text in ["fail-fast: false", "slot: [0, 1, 2, 3]", "timeout-minutes: 15",
                     "go test -count=1 -race -json -list '^(Test|Example|Fuzz)' ./internal/platform",
                     'go test -count=1 -race -json -run "$pattern" ./internal/platform',
                     'package=$(go list ./internal/platform)', 'git rev-parse HEAD', 'git rev-parse HEAD^{tree}',
                     '"$GITHUB_RUN_ID"', 'verify-store-shard.py select --profile platform "$PLATFORM_SLOT"',
                     'verify-store-shard.py verify --profile platform "$PLATFORM_SLOT"',
                     "name: platform-shard-v1-${{ matrix.slot }}", "overwrite: true", "if-no-files-found: error",
                     "path: ${{ runner.temp }}/platform-summary.json"]:
            self.assertIn(text, platform.group(1))
        self.assertNotIn("-timeout", platform.group(1))
        for suite in ["store", "platform"]:
            download = re.search(rf"pattern: {suite}-shard-v1-\*\n          path: \$\{{\{{ runner.temp \}}\}}/{suite}-shards\n          merge-multiple: false", self.workflow)
            self.assertIsNotNone(download)
            aggregate = re.search(rf"- name: Verify {suite.title()} shard coverage\n        run: \|\n(.*?)(?=\n      -)", self.workflow, re.S)
            self.assertIsNotNone(aggregate)
            for text in [f'package=$(go list ./internal/{suite})', f'"$RUNNER_TEMP/{suite}-shards"',
                         '--run-id "$GITHUB_RUN_ID" --sha "$(git rev-parse HEAD)" --tree "$(git rev-parse HEAD^{tree})"']:
                self.assertIn(text, aggregate.group(1))
            self.assertIn("verify-store-shard.py aggregate" + (" --profile platform" if suite == "platform" else ""), aggregate.group(1))

    def test_remaining_package_exclusion_executes_actual_shell_without_go(self):
        match = re.search(r"- name: Race test all other packages\n        run: \|\n(.*?)(?=\n      -)", self.workflow, re.S)
        self.assertIsNotNone(match)
        script = textwrap.dedent(match.group(1))
        # PATH is empty, and every go invocation is handled here or rejected.
        stub = '''go() {
          if [[ "$#" -eq 2 && "$1" == list ]]; then
            case "$2" in
              ./internal/store) printf '%s\\n' "$STUB_STORE" ;;
              ./internal/platform) printf '%s\\n' "$STUB_PLATFORM" ;;
              ./internal/coordination) printf '%s\\n' "$STUB_COORDINATION" ;;
              ./...) printf '%s\\n' "$STUB_ALL" ;;
              *) return 90 ;;
            esac
          elif [[ "$#" -gt 2 && "$1" == test && "$2" == -race ]]; then
            printf 'race:%s\\n' "${@:3}"
          else
            return 90
          fi
        }
        readonly -f go
        '''
        remainder = ["agentworkspace/other", PACKAGE + "extra", PLATFORM_PACKAGE + "/child",
                     COORDINATION_PACKAGE + "/child", COORDINATION_PACKAGE + "extra"]
        valid = "\n".join([PACKAGE, *remainder, PLATFORM_PACKAGE, COORDINATION_PACKAGE])
        cases = [("valid", PACKAGE, PLATFORM_PACKAGE, COORDINATION_PACKAGE, valid, True),
                 ("empty-store", "", PLATFORM_PACKAGE, COORDINATION_PACKAGE, valid, False),
                 ("empty-platform", PACKAGE, "", COORDINATION_PACKAGE, valid, False),
                 ("multiline-store", PACKAGE + "\nother", PLATFORM_PACKAGE, COORDINATION_PACKAGE, valid, False),
                 ("multiline-platform", PACKAGE, PLATFORM_PACKAGE + "\nother", COORDINATION_PACKAGE, valid, False),
                 ("equal", PACKAGE, PACKAGE, COORDINATION_PACKAGE, valid, False),
                 ("missing-store", PACKAGE, PLATFORM_PACKAGE, COORDINATION_PACKAGE,
                  "\n".join([PLATFORM_PACKAGE, COORDINATION_PACKAGE, "other"]), False),
                 ("missing-platform", PACKAGE, PLATFORM_PACKAGE, COORDINATION_PACKAGE,
                  "\n".join([PACKAGE, COORDINATION_PACKAGE, "other"]), False),
                 ("duplicate-store", PACKAGE, PLATFORM_PACKAGE, COORDINATION_PACKAGE, valid + "\n" + PACKAGE, False),
                 ("duplicate-platform", PACKAGE, PLATFORM_PACKAGE, COORDINATION_PACKAGE, valid + "\n" + PLATFORM_PACKAGE, False),
                 ("empty-remainder", PACKAGE, PLATFORM_PACKAGE, COORDINATION_PACKAGE, PACKAGE + "\n" + PLATFORM_PACKAGE, False),
                 ("blank-remainder", PACKAGE, PLATFORM_PACKAGE, COORDINATION_PACKAGE,
                  PACKAGE + "\n\n" + PLATFORM_PACKAGE + "\n" + COORDINATION_PACKAGE, False),
                 ("empty-coordination", PACKAGE, PLATFORM_PACKAGE, "", valid, False),
                 ("multiline-coordination", PACKAGE, PLATFORM_PACKAGE, COORDINATION_PACKAGE + "\nother", valid, False),
                 ("coordination-equals-store", PACKAGE, PLATFORM_PACKAGE, PACKAGE, valid, False),
                 ("coordination-equals-platform", PACKAGE, PLATFORM_PACKAGE, PLATFORM_PACKAGE, valid, False),
                 ("missing-coordination", PACKAGE, PLATFORM_PACKAGE, COORDINATION_PACKAGE,
                  "\n".join([PACKAGE, *remainder, PLATFORM_PACKAGE]), False),
                 ("duplicate-coordination", PACKAGE, PLATFORM_PACKAGE, COORDINATION_PACKAGE,
                  valid + "\n" + COORDINATION_PACKAGE, False),
                 ("coordination-empty-remainder", PACKAGE, PLATFORM_PACKAGE, COORDINATION_PACKAGE,
                  "\n".join([PACKAGE, PLATFORM_PACKAGE, COORDINATION_PACKAGE]), False)]
        for label, store, platform, coordination, all_packages, accepted in cases:
            with self.subTest(case=label):
                result = subprocess.run(["/bin/bash", "-c", stub + script],
                                        env={**os.environ, "PATH": "", "STUB_STORE": store, "STUB_PLATFORM": platform,
                                             "STUB_COORDINATION": coordination, "STUB_ALL": all_packages},
                                        text=True, capture_output=True, timeout=5)
                self.assertEqual(result.returncode == 0, accepted, result.stderr)
                self.assertEqual(result.stdout.splitlines(), ["race:" + package for package in remainder] if accepted else [])


if __name__ == "__main__":
    unittest.main()
