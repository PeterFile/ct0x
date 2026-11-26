#!/usr/bin/env python3
"""Black-box Store/Platform/HTTPAPI shard gate regressions; never invokes Go."""
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
PLATFORM_TOTAL = 9
HTTPAPI_PACKAGE = "agentworkspace/internal/httpapi"
COORDINATION_PACKAGE = "agentworkspace/internal/coordination"
WORKSPACE_PACKAGE = "agentworkspace/cmd/workspace"
# Fixed SHA-256 first-eight-byte big-endian vectors: mod 8 except for legacy slot 4,
# where mod 16 = 4 stays and mod 16 = 12 moves to slot 8.
PLATFORM_SLOTS = {"TestCoordination": 7, "TestAlpha": 3, "TestBeta": 3, "TestGamma": 8,
                  "Test新入口": 2, "Example": 5, "FuzzBytes": 4,
                  "TestVector6": 0, "TestVector0": 1, "TestIota": 6}
# Fixed SHA-256 first-eight-byte big-endian mod-8 vectors.
HTTPAPI_SLOTS = {"TestVector6": 0, "TestVector0": 1, "Test新入口": 2, "TestAlpha": 3,
                 "FuzzBytes": 4, "Example": 5, "TestIota": 6, "TestCoordination": 7,
                 "TestMemberMCPCodexMetadataCoordinatorDecision": 6}


def event(action, name="", **fields):
    row = {"Action": action, "Package": PACKAGE, **fields}
    if name:
        row["Test"] = name
    return row


def platform_event(action, name="", **fields):
    return event(action, name, Package=PLATFORM_PACKAGE, **fields)


def httpapi_event(action, name="", **fields):
    return event(action, name, Package=HTTPAPI_PACKAGE, **fields)


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
        package = HTTPAPI_PACKAGE if profile == "httpapi" else (PLATFORM_PACKAGE if profile == "platform" else PACKAGE)
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
        slots, total = (HTTPAPI_SLOTS, 8) if profile == "httpapi" else ((PLATFORM_SLOTS, PLATFORM_TOTAL) if profile == "platform" else (SLOTS, 64))
        make_event = httpapi_event if profile == "httpapi" else (platform_event if profile == "platform" else event)
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
        if package is None:
            package = HTTPAPI_PACKAGE if profile == "httpapi" else (PLATFORM_PACKAGE if profile == "platform" else PACKAGE)
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
        for slot in range(PLATFORM_TOTAL):
            result = self.invoke("select", slot, PLATFORM_PACKAGE, listed, "--profile", "platform")
            self.assertEqual(result.returncode, 0, result.stderr)
            expected = sorted(name for name, assigned in PLATFORM_SLOTS.items() if assigned == slot)
            self.assertEqual(result.stdout.strip(), "^(" + "|".join(expected) + ")$")
            self.assertIsNone(re.fullmatch(result.stdout.strip(), expected[0] + "Extra"))
        directory = self.artifacts(PLATFORM_SLOTS, "platform")
        summary = self.success(self.aggregate(directory, profile="platform"))
        self.assertEqual((summary["schema"], summary["total"], summary["package"]), (1, PLATFORM_TOTAL, PLATFORM_PACKAGE))
        for field in ["compiled", "runs", "passes"]:
            self.assertEqual(summary[field], {name: 1 for name in PLATFORM_SLOTS})
        self.assertEqual(summary["skips"], {})
        directory.rename(self.directory / "full-slots")
        directory = self.artifacts([ROOT], "platform")
        self.success(self.aggregate(directory, profile="platform"))
        for slot in [0, 8]:
            with self.subTest(missing_empty_slot=slot):
                artifact = directory / f"platform-shard-v1-{slot}"
                empty = json.loads((artifact / "platform-summary.json").read_text())
                self.assertEqual(empty["compiled"], {ROOT: 1})
                for field in ["selected", "runs", "passes", "skips"]:
                    self.assertEqual(empty[field], {})
                moved = self.directory / artifact.name
                artifact.rename(moved)
                result = self.aggregate(directory, profile="platform")
                self.reject(result)
                self.assertIn("9 个 artifact 目录", result.stderr)
                moved.rename(artifact)

    def test_platform_split_rejects_misrouted_or_missing_execution(self):
        names = ["FuzzBytes", "TestGamma"]
        # Running both in slot 4 used to pass; TestGamma must now run in slot 8.
        rows = [platform_event(action, name) for name in names for action in ("run", "pass")]
        misrouted = self.verify(rows, names, slot=4, profile="platform")
        self.reject(misrouted)
        self.assertIn("未列出或不属于此分片的顶层项：TestGamma", misrouted.stderr)
        omitted = self.verify([], names, slot=8, profile="platform")
        self.reject(omitted)
        self.assertIn("顶层 run/pass 覆盖计数不符", omitted.stderr)

    def test_profiles_are_bounded_and_cross_package_select_verify_fail(self):
        listed = self.inventory([ROOT], PLATFORM_PACKAGE)
        run = self.json_lines("run.json", [platform_event("start"), platform_event("run", ROOT),
                                            platform_event("pass", ROOT), platform_event("pass")])
        for command, args in [("select", [listed]), ("verify", [listed, run, *IDENTITY])]:
            for slot in [-1, PLATFORM_TOTAL, 18]:
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
        summary = self.success(self.verify(rows, slot=7, profile="platform"))
        self.assertEqual(summary["selected"], {ROOT: 1})
        good = [platform_event("run", ROOT), platform_event("pass", ROOT)]
        cases = [[], good * 2, [platform_event("run", ROOT)], [platform_event("pass", ROOT)]]
        for action in ["skip", "fail"]:
            cases.extend([[platform_event("run", ROOT), platform_event(action, ROOT)],
                          [platform_event("run", ROOT), platform_event("run", child), platform_event(action, child),
                           platform_event("pass", ROOT)]])
        for rows in cases:
            with self.subTest(rows=rows):
                self.reject(self.verify(rows, slot=7, profile="platform"))
        listed = self.inventory([ROOT], PLATFORM_PACKAGE)
        for rows in [[platform_event("start"), *good], [platform_event("start"), platform_event("skip")],
                     [platform_event("start"), platform_event("fail")]]:
            run = self.json_lines("run.json", rows)
            self.reject(self.invoke("verify", 7, PLATFORM_PACKAGE, listed, run, *IDENTITY, "--profile", "platform"))
        run = self.json_lines("run.json", [platform_event("start"), *good, platform_event("pass")])
        run.write_bytes(run.read_bytes()[:-1])
        self.reject(self.invoke("verify", 7, PLATFORM_PACKAGE, listed, run, *IDENTITY, "--profile", "platform"))
        listed.write_bytes(listed.read_bytes()[:-1])
        self.reject(self.invoke("select", 7, PLATFORM_PACKAGE, listed, "--profile", "platform"))
        self.reject(self.invoke("select", 0, PLATFORM_PACKAGE, self.inventory([], PLATFORM_PACKAGE), "--profile", "platform"))

    def test_platform_aggregate_identity_profile_and_prefix_binding(self):
        directory = self.artifacts(PLATFORM_SLOTS, "platform")
        for index, value in [(1, "42566516009"), (3, "c" * 40), (5, "d" * 40)]:
            identity = list(IDENTITY)
            identity[index] = value
            self.reject(self.aggregate(directory, identity=identity, profile="platform"))
        for field, value, original in [("run_id", "42566516009", IDENTITY[1]), ("checkout_sha", "c" * 40, IDENTITY[3]),
                                       ("checkout_tree", "d" * 40, IDENTITY[5]), ("package", PACKAGE, PLATFORM_PACKAGE)]:
            for slot in range(PLATFORM_TOTAL):
                self.mutate(directory, slot, lambda summary: summary.update({field: value}), "platform")
            self.reject(self.aggregate(directory, profile="platform"))
            if field == "package":
                self.reject(self.aggregate(directory, package=PACKAGE, profile="platform"))
            for slot in range(PLATFORM_TOTAL):
                self.mutate(directory, slot, lambda summary: summary.update({field: original}), "platform")
        self.reject(self.aggregate(directory))
        path = directory / "platform-shard-v1-0/platform-summary.json"
        path.rename(path.with_name("store-summary.json"))
        self.reject(self.aggregate(directory, profile="platform"))
        path.with_name("store-summary.json").rename(path)
        for slot in range(PLATFORM_TOTAL):
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
        extra = directory / f"platform-shard-v1-{PLATFORM_TOTAL}"
        extra.mkdir()
        self.reject(self.aggregate(directory, profile="platform"))
        extra.rmdir()
        self.mutate(directory, 0, lambda summary: summary["compiled"].pop(ROOT), "platform")
        self.reject(self.aggregate(directory, profile="platform"))
        for slot in range(PLATFORM_TOTAL):
            self.mutate(directory, slot, lambda summary: summary.update(compiled={}, selected={}, runs={}, passes={}), "platform")
        self.reject(self.aggregate(directory, profile="platform"))

    def test_platform_aggregate_recomputes_hash_despite_complete_union(self):
        directory = self.artifacts(PLATFORM_SLOTS, "platform")
        # All wrong assignments still partition the complete inventory exactly once.
        assignments = [{"TestCoordination": 3, "TestAlpha": 1, "TestBeta": 1, "TestGamma": 2,
                        "Test新入口": 0, "Example": 1, "FuzzBytes": 0,
                        "TestVector6": 0, "TestVector0": 1, "TestIota": 0},  # Legacy Store18 mod 4, not the Platform split.
                       {name: (slot + 1) % PLATFORM_TOTAL for name, slot in PLATFORM_SLOTS.items()},
                       {**PLATFORM_SLOTS, "TestGamma": 4}]  # Pre-split routing.
        for assigned in assignments:
            for slot in range(PLATFORM_TOTAL):
                selected = {name: 1 for name in assigned if assigned[name] == slot}
                self.mutate(directory, slot, lambda summary: summary.update(selected=selected, runs=selected, passes=selected), "platform")
            result = self.aggregate(directory, profile="platform")
            self.reject(result)
            self.assertIn("summary 哈希选片或 run/pass/skip 覆盖不符", result.stderr)


    def test_httpapi_fixed_vectors_full_coverage_and_empty_slots(self):
        self.assertEqual(set(HTTPAPI_SLOTS.values()), set(range(8)))
        listed = self.inventory(HTTPAPI_SLOTS, HTTPAPI_PACKAGE)
        for slot in range(8):
            result = self.invoke("select", slot, HTTPAPI_PACKAGE, listed, "--profile", "httpapi")
            self.assertEqual(result.returncode, 0, result.stderr)
            pattern = result.stdout.strip()
            expected = sorted(name for name, assigned in HTTPAPI_SLOTS.items() if assigned == slot)
            self.assertEqual(pattern, "^(" + "|".join(expected) + ")$")
            self.assertNotIn("/", pattern)
            for name, assigned in HTTPAPI_SLOTS.items():
                self.assertEqual(re.fullmatch(pattern, name) is not None, assigned == slot)
                self.assertIsNone(re.fullmatch(pattern, name + "Extra"))
        directory = self.artifacts(HTTPAPI_SLOTS, "httpapi")
        summary = self.success(self.aggregate(directory, profile="httpapi"))
        self.assertEqual((summary["schema"], summary["total"], summary["package"]), (1, 8, HTTPAPI_PACKAGE))
        for field in ["compiled", "runs", "passes"]:
            self.assertEqual(summary[field], {name: 1 for name in HTTPAPI_SLOTS})
        self.assertEqual(summary["skips"], {})
        directory.rename(self.directory / "full-slots")
        directory = self.artifacts([ROOT], "httpapi")
        self.success(self.aggregate(directory, profile="httpapi"))
        listed = self.inventory([ROOT], HTTPAPI_PACKAGE)
        empty = self.invoke("select", 0, HTTPAPI_PACKAGE, listed, "--profile", "httpapi")
        self.assertEqual(empty.returncode, 0, empty.stderr)
        self.assertEqual(empty.stdout.strip(), "^$")
        summary = json.loads((directory / "httpapi-shard-v1-0/httpapi-summary.json").read_text())
        self.assertEqual(summary["compiled"], {ROOT: 1})
        for field in ["selected", "runs", "passes", "skips"]:
            self.assertEqual(summary[field], {})
        (directory / "httpapi-shard-v1-0").rename(self.directory / "empty-slot")
        self.reject(self.aggregate(directory, profile="httpapi"))

    def test_httpapi_bounds_and_all_profile_package_pairs(self):
        listed = self.inventory([ROOT], HTTPAPI_PACKAGE)
        run = self.json_lines("run.json", [httpapi_event("start"), httpapi_event("run", ROOT),
                                            httpapi_event("pass", ROOT), httpapi_event("pass")])
        for command, args in [("select", [listed]), ("verify", [listed, run, *IDENTITY])]:
            for slot in [-1, 8, 64]:
                with self.subTest(command=command, slot=slot):
                    result = self.invoke(command, slot, HTTPAPI_PACKAGE, *args, "--profile", "httpapi")
                    self.assertEqual(result.returncode, 2, result.stderr)
                    self.assertIn("invalid choice", result.stderr)
        for total in [4, 8, 64]:
            result = self.invoke("select", 0, HTTPAPI_PACKAGE, listed, "--profile", "httpapi", "--total", total)
            self.assertEqual(result.returncode, 2, result.stderr)
        packages = {"store": PACKAGE, "platform": PLATFORM_PACKAGE, "httpapi": HTTPAPI_PACKAGE}
        for profile, expected in packages.items():
            for package in packages.values():
                if package == expected:
                    continue
                listed = self.inventory([ROOT], package)
                run = self.json_lines("run.json", [event("start", Package=package), event("run", ROOT, Package=package),
                                                    event("pass", ROOT, Package=package), event("pass", Package=package)])
                for command, args in [("select", [listed]), ("verify", [listed, run, *IDENTITY])]:
                    with self.subTest(command=command, profile=profile, package=package):
                        self.reject(self.invoke(command, 3, package, *args, "--profile", profile))

    def test_httpapi_nested_lifecycle_and_bad_execution_evidence(self):
        child = ROOT + "/child/embedded/slash#00"
        rows = [httpapi_event("run", ROOT), httpapi_event("run", child), httpapi_event("pause", child),
                httpapi_event("pause", ROOT), httpapi_event("output", child, Output="background log\n"),
                httpapi_event("cont", ROOT), httpapi_event("cont", child), httpapi_event("pass", child), httpapi_event("pass", ROOT)]
        summary = self.success(self.verify(rows, slot=7, profile="httpapi"))
        for field in ["selected", "runs", "passes"]:
            self.assertEqual(summary[field], {ROOT: 1})
        for root, slot in [("Example", 5), ("FuzzBytes", 4)]:
            rows = [httpapi_event("run", root)]
            if root == "FuzzBytes":
                rows += [httpapi_event("run", root + "/seed#0"), httpapi_event("pass", root + "/seed#0")]
            self.success(self.verify([*rows, httpapi_event("pass", root)], names=[root], slot=slot, profile="httpapi"))
        good = [httpapi_event("run", ROOT), httpapi_event("pass", ROOT)]
        cases = [[], good * 2, [httpapi_event("run", ROOT)], [httpapi_event("pass", ROOT)],
                 [httpapi_event("run", ROOT), httpapi_event("pause", ROOT), httpapi_event("pass", ROOT)],
                 [httpapi_event("run", child), *good],
                 [httpapi_event("run", ROOT), httpapi_event("run", child), httpapi_event("pass", ROOT)],
                 [httpapi_event("run", ROOT), httpapi_event("run", "TestVector6"), httpapi_event("pass", "TestVector6"), httpapi_event("pass", ROOT)],
                 [event("run", ROOT), event("pass", ROOT)]]
        for action in ["skip", "fail"]:
            cases.extend([[httpapi_event("run", ROOT), httpapi_event(action, ROOT)],
                          [httpapi_event("run", ROOT), httpapi_event("run", child), httpapi_event(action, child), httpapi_event("pass", ROOT)]])
        for rows in cases:
            with self.subTest(rows=rows):
                self.reject(self.verify(rows, names=[ROOT, "TestVector6"], slot=7, profile="httpapi"))
        listed = self.inventory([ROOT], HTTPAPI_PACKAGE)
        for rows in [[httpapi_event("start"), *good], [httpapi_event("start"), httpapi_event("skip")],
                     [httpapi_event("start"), httpapi_event("fail")]]:
            run = self.json_lines("run.json", rows)
            self.reject(self.invoke("verify", 7, HTTPAPI_PACKAGE, listed, run, *IDENTITY, "--profile", "httpapi"))
        run = self.json_lines("run.json", [httpapi_event("start"), *good, httpapi_event("pass")])
        run.write_bytes(run.read_bytes()[:-1])
        self.reject(self.invoke("verify", 7, HTTPAPI_PACKAGE, listed, run, *IDENTITY, "--profile", "httpapi"))
        listed.write_bytes(listed.read_bytes()[:-1])
        self.reject(self.invoke("select", 7, HTTPAPI_PACKAGE, listed, "--profile", "httpapi"))
        for names in [[], [ROOT, ROOT]]:
            self.reject(self.invoke("select", 7, HTTPAPI_PACKAGE, self.inventory(names, HTTPAPI_PACKAGE), "--profile", "httpapi"))

    def test_httpapi_aggregate_external_identity_and_namespace_binding(self):
        directory = self.artifacts(HTTPAPI_SLOTS, "httpapi")
        for index, value in [(1, "42566516009"), (3, "c" * 40), (5, "d" * 40)]:
            identity = list(IDENTITY)
            identity[index] = value
            self.reject(self.aggregate(directory, identity=identity, profile="httpapi"))
        for field, value, original in [("run_id", "42566516009", IDENTITY[1]), ("checkout_sha", "c" * 40, IDENTITY[3]),
                                       ("checkout_tree", "d" * 40, IDENTITY[5]), ("package", PLATFORM_PACKAGE, HTTPAPI_PACKAGE)]:
            # Even unanimous stale/foreign summaries must fail against the current checkout.
            for slot in range(8):
                self.mutate(directory, slot, lambda summary: summary.update({field: value}), "httpapi")
            self.reject(self.aggregate(directory, profile="httpapi"))
            if field == "package":
                self.reject(self.aggregate(directory, package=PLATFORM_PACKAGE, profile="httpapi"))
            for slot in range(8):
                self.mutate(directory, slot, lambda summary: summary.update({field: original}), "httpapi")
        for profile in [None, "platform"]:
            self.reject(self.aggregate(directory, profile=profile))
        path = directory / "httpapi-shard-v1-0/httpapi-summary.json"
        path.rename(path.with_name("platform-summary.json"))
        self.reject(self.aggregate(directory, profile="httpapi"))
        path.with_name("platform-summary.json").rename(path)
        for slot in range(8):
            (directory / f"httpapi-shard-v1-{slot}").rename(directory / f"platform-shard-v1-{slot}")
        self.reject(self.aggregate(directory, profile="httpapi"))

    def test_httpapi_aggregate_all_slots_and_path_safety(self):
        directory = self.artifacts(HTTPAPI_SLOTS, "httpapi")
        artifact = directory / "httpapi-shard-v1-7"
        target = self.directory / "outside-artifact"
        artifact.rename(target)
        self.reject(self.aggregate(directory, profile="httpapi"))
        artifact.symlink_to(target, target_is_directory=True)
        self.reject(self.aggregate(directory, profile="httpapi"))
        artifact.unlink()
        target.rename(artifact)
        self.mutate(directory, 7, lambda summary: summary.update(slot=0), "httpapi")
        self.reject(self.aggregate(directory, profile="httpapi"))
        self.mutate(directory, 7, lambda summary: summary.update(slot=7), "httpapi")
        extra = directory / "httpapi-shard-v1-8"
        extra.mkdir()
        self.reject(self.aggregate(directory, profile="httpapi"))
        extra.rmdir()
        path = artifact / "httpapi-summary.json"
        unknown = artifact / "second-summary.json"
        unknown.write_bytes(path.read_bytes())
        self.reject(self.aggregate(directory, profile="httpapi"))
        unknown.unlink()
        outside = self.directory / "outside.json"
        path.rename(outside)
        self.reject(self.aggregate(directory, profile="httpapi"))
        path.symlink_to(outside)
        self.reject(self.aggregate(directory, profile="httpapi"))
        path.unlink()
        outside.rename(path)
        alias = self.directory / "summary-link"
        alias.symlink_to(directory, target_is_directory=True)
        self.reject(self.aggregate(alias, profile="httpapi"))
        self.success(self.aggregate(directory, profile="httpapi"))

    def test_httpapi_aggregate_counters_compiled_and_mixed_totals(self):
        directory = self.artifacts(HTTPAPI_SLOTS, "httpapi")
        for slot in range(8):
            path = directory / f"httpapi-shard-v1-{slot}/httpapi-summary.json"
            original = path.read_text()
            name = next(name for name, assigned in HTTPAPI_SLOTS.items() if assigned == slot)
            for field in ["compiled", "selected", "runs", "passes", "skips"]:
                with self.subTest(slot=slot, field=field):
                    self.mutate(directory, slot, lambda summary: summary[field].update({name: 2}), "httpapi")
                    self.reject(self.aggregate(directory, profile="httpapi"))
                    path.write_text(original)
        path = directory / "httpapi-shard-v1-7/httpapi-summary.json"
        original = path.read_text()
        for field, value in [("schema", True), ("slot", 7.0), ("total", 8.0), ("compiled", {}),
                             ("selected", {}), ("runs", {}), ("passes", {}), ("skips", {ROOT: 1}),
                             ("selected", {ROOT: True}), ("runs", {ROOT: 0}), ("passes", {ROOT: 1.0})]:
            with self.subTest(field=field, value=value):
                self.mutate(directory, 7, lambda summary: summary.update({field: value}), "httpapi")
                self.reject(self.aggregate(directory, profile="httpapi"))
                path.write_text(original)
        for total in [4, 18, 32, 64]:
            for slots in [[7], range(8)]:
                with self.subTest(total=total, slots=list(slots)):
                    for slot in slots:
                        self.mutate(directory, slot, lambda summary: summary.update(total=total), "httpapi")
                    self.reject(self.aggregate(directory, profile="httpapi"))
                    for slot in slots:
                        self.mutate(directory, slot, lambda summary: summary.update(total=8), "httpapi")
        self.success(self.aggregate(directory, profile="httpapi"))
        self.mutate(directory, 0, lambda summary: summary["compiled"].pop(ROOT), "httpapi")
        self.reject(self.aggregate(directory, profile="httpapi"))
        for slot in range(8):
            self.mutate(directory, slot, lambda summary: summary.update(compiled={}, selected={}, runs={}, passes={}), "httpapi")
        self.reject(self.aggregate(directory, profile="httpapi"))

    def test_httpapi_aggregate_recomputes_hash_despite_complete_union(self):
        directory = self.artifacts(HTTPAPI_SLOTS, "httpapi")
        # A complete, duplicate-free union is not sufficient without the fixed hash.
        combined = {}
        for slot in range(8):
            selected = {name: 1 for name, assigned in HTTPAPI_SLOTS.items() if (assigned + 1) % 8 == slot}
            self.assertFalse(combined.keys() & selected.keys())
            combined.update(selected)
            self.mutate(directory, slot, lambda summary: summary.update(selected=selected, runs=selected, passes=selected), "httpapi")
        self.assertEqual(combined, {name: 1 for name in HTTPAPI_SLOTS})
        self.reject(self.aggregate(directory, profile="httpapi"))


class WorkflowTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.workflow = SCRIPT.parent.parent.joinpath("workflows/ci-full.yml").read_text(encoding="utf-8")
        cls.parent_workflow = SCRIPT.parent.parent.joinpath("workflows/ci.yml").read_text(encoding="utf-8")
        cls.worker = SCRIPT.with_name("ci-shards.py").read_text(encoding="utf-8")

    def test_full_gate_checks_all_results_then_coverage_without_product_work(self):
        match = re.search(r"\n  gate:\n(.*)", self.workflow, re.S)
        self.assertIsNotNone(match)
        job = match[1]
        for text in ["name: Full suite evidence\n", "if: ${{ !cancelled() }}", "toJSON(needs)", "ci-gate.py",
                     "ci-shards.py aggregate", "merge-multiple: false",
                     '--run-id "$GITHUB_RUN_ID" --sha "$CHECKOUT_SHA" --tree "$CHECKOUT_TREE"']:
            self.assertIn(text, job)
        self.assertLess(job.index("ci-gate.py"), job.index("download-artifact"))
        self.assertLess(job.index("download-artifact"), job.index("ci-shards.py aggregate"))
        self.assertNotRegex(job, r"\b(?:go test|go vet|npm|setup-go)\b")
        # Ordinary cmd precedes admission; coordination/rest/workspace/vet/bench do not wait for shards.
        self.assertIn("go test -count=1 ./cmd/workspace", self.parent_workflow)
        self.assertNotIn("go test -count=1 ./cmd/workspace", self.workflow)
        for name in ["go-other", "go-workspace", "go-vet", "go-benchmarks"]:
            block = re.search(rf"\n  {name}:\n(.*?)(?=\n  [\w-]+:)", self.workflow, re.S)[1]
            self.assertNotIn("needs:", block)
        for command in ["go test -count=1 ./internal/coordination", "go test -count=1 -race ./internal/coordination"]:
            self.assertEqual(self.workflow.count("run: " + command + "\n"), 1)

    def test_batched_workers_keep_real_uncached_runs_and_isolated_evidence(self):
        block = re.search(r"\n  go-shards:\n(.*?)(?=\n  go-other:)", self.workflow, re.S)[1]
        for text in ["fail-fast: false", "timeout-minutes: 40", "max-parallel: 7", "actions/setup-go@v7",
                     "ci-shards.py run", "actions/upload-artifact@v7", "overwrite: true", "if-no-files-found: error",
                     "name: ${{ matrix.profile }}-worker-v1-${{ matrix.worker }}", "retention-days: 7"]:
            self.assertIn(text, block)
        for text in ['["go", "test", "-count=1", "-race", "-json", "-list", "^(Test|Example|Fuzz)", target]',
                     '["go", "test", "-count=1", "-race", "-json", "-run", pattern, target]',
                     'verifier.inventory(listed, package)', 'verifier.selection(compiled, slot, profile)',
                     'verifier.run_pattern(subset)', 'verifier.verify(events, package, subset)',
                     'verifier.aggregate(flattened, expected, profile)',
                     '"git", "rev-parse", "HEAD"', '"git", "rev-parse", "HEAD^{tree}"']:
            self.assertIn(text, self.worker)
        self.assertNotIn("-timeout", self.worker)
        self.assertNotIn("STORE_RACE_GROUP", self.workflow)
        self.assertIn("pattern: '*-worker-v1-*'", self.workflow)
        self.assertEqual(self.workflow.count("python3 .github/scripts/test-store-shard.py -v"), 1)
        self.assertIn("name: go-benchmarks", self.workflow)
        self.assertNotIn("fixture", self.workflow.lower())

    def test_profiles_keep_independent_package_bound_aggregates(self):
        # Physical workers changed; the three logical profiles and aggregate checks did not.
        for profile, package, total in [("store", PACKAGE, 64), ("platform", PLATFORM_PACKAGE, 9), ("httpapi", HTTPAPI_PACKAGE, 8)]:
            source = SCRIPT.read_text(encoding="utf-8")
            self.assertIn(package, source)
            self.assertIn(f'"{profile}"', self.worker)
            self.assertIn('verifier.PROFILES[profile]', self.worker)
        self.assertIn('package == expected_package', self.worker)
        self.assertIn('source.is_file() and not source.is_symlink()', self.worker)
        self.assertIn('directory_entries(artifact, [f"{prefix}{slot}" for slot in slots])', self.worker)

    def test_remaining_package_exclusion_executes_actual_shell_without_go(self):
        match = re.search(r"- name: Race test all other packages\n        run: \|\n(.*?)(?=\n      -|\n  [\w-]+:|\Z)", self.workflow, re.S)
        self.assertIsNotNone(match)
        script = textwrap.dedent(match.group(1))
        # PATH is empty, and every go invocation is handled here or rejected.
        stub = '''go() {
          if [[ "$#" -eq 2 && "$1" == list ]]; then
            case "$2" in
              ./internal/store) printf '%s\\n' "$STUB_STORE" ;;
              ./internal/platform) printf '%s\\n' "$STUB_PLATFORM" ;;
              ./internal/httpapi) printf '%s\\n' "$STUB_HTTPAPI" ;;
              ./internal/coordination) printf '%s\\n' "$STUB_COORDINATION" ;;
              ./cmd/workspace) printf '%s\\n' "$STUB_WORKSPACE" ;;
              ./...) printf '%s\\n' "$STUB_ALL" ;;
              *) return 90 ;;
            esac
          elif [[ "$#" -gt 2 && "$1" == test && "$2" == -race ]]; then
            [[ "$GOFLAGS" == '-mod=readonly -timeout=14m' ]] || return 91
            printf 'race:%s\\n' "${@:3}"
          else
            return 90
          fi
        }
        readonly -f go
        '''
        packages = {"STORE": PACKAGE, "PLATFORM": PLATFORM_PACKAGE, "HTTPAPI": HTTPAPI_PACKAGE,
                    "COORDINATION": COORDINATION_PACKAGE, "WORKSPACE": WORKSPACE_PACKAGE}
        remainder = ["agentworkspace/other", PACKAGE + "extra", PLATFORM_PACKAGE + "/child", HTTPAPI_PACKAGE + "extra",
                     HTTPAPI_PACKAGE + "/child", COORDINATION_PACKAGE + "/child", COORDINATION_PACKAGE + "extra",
                     WORKSPACE_PACKAGE + "extra", WORKSPACE_PACKAGE + "/child"]
        valid = [PACKAGE, *remainder, PLATFORM_PACKAGE, HTTPAPI_PACKAGE, COORDINATION_PACKAGE, WORKSPACE_PACKAGE]
        cases = [("valid", packages, valid, True), ("empty-remainder", packages, list(packages.values()), False),
                 ("blank-remainder", packages, [*valid, "", "other"], False), ("empty-all", packages, [], False)]
        for suite, package in packages.items():
            cases.extend([(f"empty-{suite}", {**packages, suite: ""}, valid, False),
                          (f"blank-{suite}", {**packages, suite: "\n"}, valid, False),
                          (f"multiline-{suite}", {**packages, suite: package + "\nother"}, valid, False),
                          (f"missing-{suite}", packages, [name for name in valid if name != package], False),
                          (f"duplicate-{suite}", packages, [*valid, package], False)])
            for other, identity in packages.items():
                if other != suite:
                    cases.append((f"{suite}-equals-{other}", {**packages, suite: identity}, valid, False))
        for label, identities, all_packages, accepted in cases:
            with self.subTest(case=label):
                result = subprocess.run(["/bin/bash", "-c", stub + script],
                                        env={**os.environ, "PATH": "", "GOFLAGS": "-mod=readonly",
                                             **{"STUB_" + suite: package for suite, package in identities.items()},
                                             "STUB_ALL": "\n".join(all_packages)}, text=True, capture_output=True, timeout=5)
                self.assertEqual(result.returncode == 0, accepted, result.stderr)
                self.assertEqual(result.stdout.splitlines(), ["race:" + package for package in remainder] if accepted else [])


if __name__ == "__main__":
    unittest.main()
