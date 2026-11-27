#!/usr/bin/env python3
"""Offline parsing/coverage regressions; no network, Go execution, or Git mutations."""
import copy
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch


SCRIPT = Path(__file__).with_name("ci-historical-audit.py")
PACKAGE = "agentworkspace/internal/store"
NAME = "TestNativeThreadHandoffChild"
CASE = {"sha": "a" * 40, "tree": "b" * 40, "base": "c" * 40,
        "parents": ["c" * 40, "d" * 40], "runs": [[44719422158, 1]]}


class AuditTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.audit = None
        if SCRIPT.is_file():
            spec = importlib.util.spec_from_file_location("historical_audit", SCRIPT)
            cls.audit = importlib.util.module_from_spec(spec)
            spec.loader.exec_module(cls.audit)

    def setUp(self):
        self.assertIsNotNone(self.audit, "Historical audit validation/parsing is not implemented")
        self.a = self.audit

    def runner(self, root):
        self.assertTrue(hasattr(self.a, "Runner"), "Isolated evidence runner is not implemented")
        return self.a.Runner(root)

    def events(self, outcome="skip", run=True):
        rows = [{"Action": "start", "Package": PACKAGE}]
        if run:
            rows.append({"Action": "run", "Package": PACKAGE, "Test": NAME})
        rows += [{"Action": outcome, "Package": PACKAGE, "Test": NAME},
                 {"Action": "fail" if outcome == "fail" else "pass", "Package": PACKAGE}]
        return "".join(json.dumps(row) + "\n" for row in rows)

    def test_strict_dispatch_inputs_and_bounds(self):
        self.assertEqual(self.a.request({"policy_sha": "a" * 40, "phase": "both"}, 113),
                         {"policy_sha": "a" * 40, "phase": "both", "offset": 0, "count": 150})
        for change in ({"policy_sha": "A" * 40}, {"policy_sha": "a" * 39},
                       {"policy_sha": "a" * 40 + "\n"}, {"phase": "shell"}, {"offset": "113"},
                       {"offset": "-1"}, {"count": "151"}, {"count": "0"}, {"count": True},
                       {"count": "01"}, {"count": False}, {"repository": "other/repo"}):
            with self.subTest(change=change), self.assertRaises(ValueError):
                self.a.request({"policy_sha": "a" * 40, "phase": "both", **change}, 113)

    def test_focus_sha_validation_and_default_source_selection(self):
        self.assertTrue(hasattr(self.a, "select_sources"), "Focused source selection is not implemented")
        cases = self.a.manifest(SCRIPT.with_name("ci-historical-cases.json").read_text())
        diagnostic = "7d70dc49ff1f3c3ae3876b2a519059c60d88ec3d"
        selector_only = next(row["sha"] for row in cases if row["sha"] not in self.a.DIAGNOSTICS)
        for phase in ("selector", "both", "diagnostics"):
            inputs = {"policy_sha": "a" * 40, "phase": phase}
            original = self.a.request(inputs, len(cases))
            self.assertEqual(self.a.request({**inputs, "focus_sha": ""}, len(cases)), original)
            selected, diagnostics = self.a.select_sources(cases, original)
            self.assertEqual(selected, [] if phase == "diagnostics" else cases)
            self.assertEqual(diagnostics, set() if phase == "selector" else set(self.a.DIAGNOSTICS))
            options = self.a.request({**inputs, "focus_sha": diagnostic, "offset": "112", "count": "1"}, len(cases))
            selected, diagnostics = self.a.select_sources(cases, options)
            self.assertEqual([row["sha"] for row in selected], [] if phase == "diagnostics" else [diagnostic])
            self.assertEqual(diagnostics, set() if phase == "selector" else {diagnostic})
        self.a.request({"policy_sha": "a" * 40, "phase": "selector", "focus_sha": selector_only}, len(cases))
        for phase in ("both", "diagnostics"):
            with self.subTest(phase=phase), self.assertRaises(ValueError):
                self.a.request({"policy_sha": "a" * 40, "phase": phase, "focus_sha": selector_only}, len(cases))
        for invalid in ("0" * 40, diagnostic.upper(), diagnostic + "\n", " " + diagnostic, False, None):
            with self.subTest(focus=invalid), self.assertRaises(ValueError):
                self.a.request({"policy_sha": "a" * 40, "phase": "selector", "focus_sha": invalid}, len(cases))
        with self.assertRaises(ValueError):
            self.a.select_sources([row for row in cases if row["sha"] != diagnostic], options)
        paged = self.a.request({"policy_sha": "a" * 40, "phase": "both", "offset": "7", "count": "2"}, len(cases))
        self.assertEqual(self.a.select_sources(cases, paged), (cases[7:9], set(self.a.DIAGNOSTICS)))

    def test_focused_audit_never_fetches_or_checks_out_unrelated_sources(self):
        sha = "7d70dc49ff1f3c3ae3876b2a519059c60d88ec3d"
        for phase in ("selector", "both", "diagnostics"):
            with self.subTest(phase=phase), tempfile.TemporaryDirectory() as directory:
                runner = self.runner(Path(directory))
                scripts = runner.root / "controller/.github/scripts"
                scripts.mkdir(parents=True)
                (scripts / "ci-historical-cases.json").write_bytes(SCRIPT.with_name("ci-historical-cases.json").read_bytes())
                event = runner.root / "event.json"
                event.write_text(json.dumps({"inputs": {"policy_sha": "a" * 40, "phase": phase, "focus_sha": sha,
                                                         "offset": "112", "count": "1"}}))
                def git(root, *args):
                    return {("rev-parse", "HEAD"): ("b" * 40 + "\n").encode(), ("--version",): b"git fixture"}.get(args, b"")
                with patch.object(self.a, "HERE", scripts), patch.object(self.a, "Runner", return_value=runner), \
                        patch.object(self.a, "context", return_value={"sha": "b" * 40}), \
                        patch.object(self.a, "public_run", return_value={}), \
                        patch.object(self.a.platform, "platform", return_value="fixture"), \
                        patch.object(self.a.subprocess, "run") as version, \
                        patch.dict(os.environ, {"GITHUB_EVENT_PATH": str(event)}), \
                        patch.object(runner, "identity", return_value={"sha": "b" * 40}), \
                        patch.object(runner, "git", side_effect=git), \
                        patch.object(runner, "prepare_policy", return_value={"sha": "a" * 40}), \
                        patch.object(runner, "prefetch", return_value=[]) as fetch, \
                        patch.object(runner, "checkout_source", return_value={"sha": sha}) as checkout, \
                        patch.object(runner, "inspect", return_value={"status": "policy-refusal"}) as inspect, \
                        patch.object(self.a, "diagnose", return_value={"status": "unknown"}) as diagnose:
                    version.return_value.stdout = b"go version go1.26.0 linux/amd64"
                    result = self.a.run_audit(runner.root)
                self.assertEqual(fetch.call_args.args[0], [sha], result["errors"])
                self.assertEqual(checkout.call_count, 1)
                self.assertEqual(inspect.call_count, 1)
                self.assertEqual(diagnose.call_count, 0 if phase == "selector" else 1)
                self.assertEqual([row["sha"] for row in result["rows"]], [sha])
                self.assertEqual(result["coverage"]["requested"], 1)
                self.assertEqual(result["status"], "complete" if phase == "selector" else "infrastructure-error")

    def test_manifest_is_only_frozen_public_identity(self):
        self.assertEqual(self.a.manifest(json.dumps([CASE])), [CASE])
        for field, value in (("tree", "bad"), ("base", "e" * 40), ("parents", []),
                             ("runs", [[True, 1]]), ("runs", [[1, 0]]), ("local_path", "forbidden")):
            case = {**CASE, field: value}
            with self.subTest(field=field, value=value), self.assertRaises(ValueError):
                self.a.manifest(json.dumps([case]))
        for text in (json.dumps([CASE, CASE]), '[{"sha":"a","sha":"b"}]', '[]'):
            with self.assertRaises(ValueError):
                self.a.manifest(text)

    def test_raw_commit_keeps_complete_ordered_parents(self):
        raw = (f'tree {CASE["tree"]}\n' + ''.join(f'parent {p}\n' for p in CASE["parents"]) +
               'author Fixture <fixture@example.invalid> 1 +0000\n\nfixture\n').encode()
        sha = hashlib.sha1(b"commit " + str(len(raw)).encode() + b"\0" + raw).hexdigest()
        actual = self.a.commit(raw, sha)
        case = {**CASE, "sha": sha}
        self.a.same_source(actual, case, sha)
        for changed in ({**actual, "parents": actual["parents"][::-1]},
                        {**actual, "parents": actual["parents"][:1]}, {**actual, "tree": "e" * 40}):
            with self.assertRaises(ValueError):
                self.a.same_source(changed, case, sha)
        with self.assertRaises(ValueError):
            self.a.commit(raw, "0" * 40)
        with self.assertRaises(ValueError):
            self.a.same_source(actual, case, "f" * 40)

    def test_policy_refusal_is_data_but_missing_json_is_failure(self):
        refused = {"unresolved": ["package:TestMissing"], "tasks": []}
        self.assertEqual(self.a.selector(json.dumps(refused), 1)["status"], "policy-refusal")
        self.assertEqual(self.a.selector('{"unresolved":[],"tasks":[]}', 0)["status"], "policy-refusal")
        selected = {"unresolved": [], "tasks": [{"unit": "identity-diff"}]}
        self.assertEqual(self.a.selector(json.dumps(selected), 0)["status"], "selected")
        for text, rc in (("", 1), ("{}", 0), ("{}\n{}", 0), (json.dumps(selected), 1),
                         ('{"unresolved":false,"tasks":[]}', 1)):
            with self.subTest(text=text, rc=rc), self.assertRaises(ValueError):
                self.a.selector(text, rc)

    def test_top_level_run_and_terminal_coverage_are_required(self):
        summary = self.a.go_events(self.events(), PACKAGE, [NAME])
        self.assertEqual(summary["tests"][NAME], {"run": 1, "pass": 0, "fail": 0, "skip": 1})
        for text in (self.events(run=False), self.events()[:-1],
                     self.events().replace(PACKAGE, "agentworkspace/internal/other"),
                     self.events().replace('"Action": "skip"', '"Action": "run"'),
                     self.events() + self.events()):
            with self.subTest(text=text), self.assertRaises(ValueError):
                self.a.go_events(text, PACKAGE, [NAME])
        with self.assertRaises(ValueError):
            self.a.go_events(self.events("pass"), PACKAGE, [NAME, "TestAbsent"])
        self.assertEqual(self.a.race_outcome(self.a.go_events(self.events("pass"), PACKAGE, [NAME]), 0),
                         "complete-no-red")
        self.assertEqual(self.a.race_outcome(self.a.go_events(self.events("fail"), PACKAGE, [NAME]), 1),
                         "red-reproduced")
        self.assertEqual(self.a.race_outcome(summary, 0), "unknown")

    def test_api_run_is_bound_to_exact_controller_attempt(self):
        expected = {"sha": "a" * 40, "run_id": "123", "run_attempt": "2"}
        api = {"id": 123, "run_attempt": 2, "event": "workflow_dispatch", "head_sha": "a" * 40,
               "head_branch": "codex/ci-historical-audit-20261126", "path": ".github/workflows/ci.yml",
               "repository": {"full_name": "PeterFile/ct0x"}}
        self.a.bind_run(api, expected)
        for key, value in (("id", 124), ("run_attempt", 1), ("head_sha", "b" * 40),
                           ("head_branch", "master"), ("path", ".github/workflows/other.yml"),
                           ("event", "push"), ("repository", {"full_name": "other/repo"})):
            with self.subTest(key=key), self.assertRaises(ValueError):
                self.a.bind_run({**api, key: value}, expected)

    def test_real_verifier_rejects_selected_but_skipped_json(self):
        with tempfile.TemporaryDirectory() as directory:
            runner = self.runner(Path(directory))
            raw = runner.output / "original-go.json"
            raw.write_text(self.events(), encoding="utf-8")
            verifier = SCRIPT.parent / "verify-store-shard.py"
            result = runner.verify(verifier, raw)
            self.assertFalse(result["result"]["accepted"])
            self.assertEqual(result["result"]["exception"], "ValueError")
            self.assertIn("跳过", result["result"]["error"])
            raw.write_text(self.events("pass"), encoding="utf-8")
            self.assertTrue(runner.verify(verifier, raw)["result"]["accepted"])

    def test_skip_diagnostic_does_not_mislabel_an_unrelated_verifier_error(self):
        with tempfile.TemporaryDirectory() as directory:
            runner = self.runner(Path(directory))
            source = runner.history / "internal/store/fixture_test.go"
            source.parent.mkdir(parents=True)
            source.write_text('func TestNativeThreadHandoffChild(t *testing.T) {\n'
                              'if os.Getenv("AW_F_HANDOFF_CHILD") != "1" { t.Skip("subprocess fixture") }\n}\n')
            raw = runner.output / "go.json"
            raw.write_text(self.events())
            (runner.output / "stderr").write_text("")
            command = {"rc": 0, "timeout": False, "stdout": "go.json", "stderr": "stderr"}
            frozen = {"sha": "eb02c8c2ad80571d0d46336b85a8eb851ca8ad4b"}
            for error, expected in (("unrelated malformed evidence", "unknown"),
                                    ("事件非法或包含失败/跳过", "red-reproduced")):
                verdict = {"result": {"accepted": False, "exception": "ValueError", "error": error}}
                with self.subTest(error=error), patch.object(runner, "record", return_value=command), \
                        patch.object(runner, "verify", return_value=verdict):
                    result = self.a.diagnose(runner, frozen, {})
                    self.assertEqual(result["status"], expected)
                    self.assertIsNone(result["candidate"]["enforcement_point"])
            raw.write_text(self.events().replace('"Action": "pass"', '"Action": "fail"'))
            with patch.object(runner, "record", return_value=command), patch.object(runner, "verify", return_value=verdict):
                self.assertEqual(self.a.diagnose(runner, frozen, {})["status"], "unknown")

    def test_subprocesses_have_no_tokens_provider_config_or_child_override(self):
        with tempfile.TemporaryDirectory() as directory, patch.dict(os.environ, {
                "AUDIT_READ_TOKEN": "never-in-child", "GITHUB_TOKEN": "never-in-child", "AW_F_HANDOFF_CHILD": "1"}):
            runner = self.runner(Path(directory))
            for key in ("AUDIT_READ_TOKEN", "GITHUB_TOKEN", "AW_F_HANDOFF_CHILD"):
                self.assertNotIn(key, runner.env)
            result = runner.record([sys.executable, "-B", "-c", "print('raw evidence')"], runner.root, "command", 5)
            self.assertEqual(result["rc"], 0)
            self.assertEqual((runner.output / result["stdout"]).read_text(), "raw evidence\n")
            timed = runner.record([sys.executable, "-B", "-c", "import time; time.sleep(30)"], runner.root, "timeout", 0.05)
            self.assertTrue(timed["timeout"])
            self.assertLess(timed["rc"], 0)

    def test_group_fetch_falls_back_without_losing_independent_sources(self):
        with tempfile.TemporaryDirectory() as directory:
            runner = self.runner(Path(directory))
            good, missing, independent = (letter * 40 for letter in "abc")
            available, fetched = set(), []
            def fetch(argv, cwd, prefix, timeout):
                shas = argv[argv.index("origin") + 1:]
                fetched.append(shas)
                if shas == [good, missing, independent]:
                    available.add(good)
                elif shas == [independent]:
                    available.add(independent)
                return {"rc": 1 if missing in shas else 0}
            with patch.object(runner, "exists_commit", side_effect=lambda sha: sha in available), \
                    patch.object(runner, "record", side_effect=fetch):
                runner.prefetch([good, missing, independent])
            self.assertEqual(fetched, [[good, missing, independent], [missing], [independent]])
            self.assertEqual(available, {good, independent})

    def selection(self, names=None, unresolved=()):
        names = list(self.a.RACE_TESTS if names is None else names)
        package = "agentworkspace/internal/coordination"
        task = {"unit": "race-agentworkspace-internal-coordination", "kind": "go", "package": package,
                "race": True, "profile": None, "worker": None, "names": names, "required": names}
        selected = {"race": {package: names}, "ordinary_required": {}, "packages": [package],
                    "all_race": False, "race_full": [], "unresolved": list(unresolved),
                    "tasks": [] if unresolved else [task]}
        return {"status": "policy-refusal" if unresolved else "selected", "selection": selected}, task

    def trace(self, names, failed=(), package="agentworkspace/internal/coordination", output=""):
        rows = [{"Action": "start", "Package": package}]
        for name in names:
            rows.append({"Action": "run", "Package": package, "Test": name})
            if output:
                rows.append({"Action": "output", "Package": package, "Test": name, "Output": output})
            rows.append({"Action": "fail" if name in failed else "pass", "Package": package, "Test": name})
        rows.append({"Action": "fail" if failed else "pass", "Package": package})
        return "".join(json.dumps(row) + "\n" for row in rows)

    def test_candidate_target_preserves_emitted_scope_and_unrelated_refusal(self):
        self.assertTrue(hasattr(self.a, "candidate_task"), "Candidate scope validation is not implemented")
        inspection, task = self.selection(unresolved=["other/package:TestModern"])
        target = self.a.candidate_task(inspection, "race", [task])
        self.assertEqual(target["task"], task)
        self.assertFalse(target["full_plan_executable"])
        self.assertEqual(target["selector_status"], "policy-refusal")
        for changed in ({**task, "names": task["names"][:-1]}, {**task, "required": []},
                        {**task, "race": False}, {**task, "worker": 0}):
            with self.subTest(task=changed), self.assertRaises(ValueError):
                self.a.candidate_task(inspection, "race", [changed])
        modern, extended = self.selection([*self.a.RACE_TESTS, "TestModernAbsent"], ["agentworkspace/internal/coordination:TestModernAbsent"])
        with self.assertRaises(ValueError):
            self.a.candidate_task(modern, "race", [task])
        target = self.a.candidate_task(modern, "race", [extended])
        self.assertEqual(target["task"]["names"], extended["names"])
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            source = root / "internal/coordination/fixture_test.go"
            source.parent.mkdir(parents=True)
            source.write_text("\n".join(f"func {n}(t *testing.T) {{}}" for n in self.a.RACE_TESTS))
            with self.assertRaises(ValueError):
                self.a.test_sources(root, "internal/coordination", target["task"]["names"])
        absent, task = self.selection(self.a.RACE_TESTS[:-1])
        with self.assertRaises(ValueError):
            self.a.candidate_task(absent, "race", [task])
        vet = {"unit": "vet", "kind": "vet", "packages": ["agentworkspace/internal/platform"]}
        with self.assertRaises(ValueError):
            self.a.candidate_task(inspection, "vet", [vet])

    def test_candidate_rejection_requires_actual_original_failure_not_new_names(self):
        self.assertTrue(hasattr(self.a, "named_outcome"), "Candidate execution validation is not implemented")
        names = list(self.a.RACE_TESTS)
        original = self.a.go_events(self.trace(names, failed=[names[0]]), "agentworkspace/internal/coordination", names)
        self.assertEqual(self.a.named_outcome(original, 1, 1, names), "red-reproduced")
        self.assertEqual(self.a.named_outcome(original, 0, 1, names), "unknown")
        self.assertEqual(self.a.named_outcome(original, 1, 0, names), "unknown")
        self.assertEqual(self.a.named_outcome(original, 1, -15, names), "unknown")
        extended = [*names, "TestNewUnrelatedFailure"]
        unrelated = self.a.go_events(self.trace(extended, failed=extended[-1:]), "agentworkspace/internal/coordination", extended)
        self.assertEqual(self.a.named_outcome(unrelated, 1, 1, names), "original-assertion-unconfirmed")
        with self.assertRaises(ValueError):
            self.a.go_events(self.trace(names, failed=names[:1]), "agentworkspace/internal/coordination", extended)
        green = self.a.go_events(self.trace(names), "agentworkspace/internal/coordination", names)
        self.assertEqual(self.a.named_outcome(green, 0, 0, names), "complete-no-red")
        self.assertEqual(self.a.named_outcome(green, 0, 1, names), "unknown")

    def test_fifth_case_runs_real_worker_before_one_isolated_probe(self):
        sha = "7d70dc49ff1f3c3ae3876b2a519059c60d88ec3d"
        self.assertIn(sha, self.a.DIAGNOSTICS, "Fifth original is missing from frozen diagnostics")
        rows = self.a.manifest(SCRIPT.with_name("ci-historical-cases.json").read_text())
        case = next(row for row in rows if row["sha"] == sha)
        self.assertEqual(case["tree"], "03436f274479296e6dd93ae2af3acc3ef7d70611")
        self.assertEqual(self.a.DIAGNOSTICS[sha]["job_id"], 139945232158)
        inspection, task = self.selection(unresolved=[PACKAGE + ":TestModernAbsent"])
        task.update(unit="race-agentworkspace-internal-store", package=PACKAGE, profile="store",
                    names=["TestTaskWorkInputEffectKeys", "TestModernAbsent"], required=["TestTaskWorkInputEffectKeys", "TestModernAbsent"])
        inspection["selection"]["race"] = {PACKAGE: task["names"]}
        inspection["selection"]["packages"] = [PACKAGE]
        with tempfile.TemporaryDirectory() as directory:
            runner = self.runner(Path(directory))
            source = runner.history / "internal/store/task_work_input_effect_test.go"
            source.parent.mkdir(parents=True)
            source.write_text('func TestTaskWorkInputEffectKeys(t *testing.T) {\n' + "\n" * 73 +
                              'conflict(f.s.HandBackWorkInputWithKey(ctx, scope, returnID, reason, key))\n' +
                              't.Fatal("expected conflict without a receipt")\n}\n')
            raw = runner.output / "probe.json"
            raw.write_text(self.trace(["TestTaskWorkInputEffectKeys"], ["TestTaskWorkInputEffectKeys"], PACKAGE,
                                     "task_work_input_effect_test.go:75: expected conflict without a receipt, got unexpected\n"))
            (runner.output / "stderr").write_text("")
            command = {"rc": 1, "timeout": False, "stdout": "probe.json", "stderr": "stderr"}
            worker = {"directory": ".", "command": command, "tracked_unchanged": True,
                      "receipt": {"api_invoked": True, "commands": [], "returned": False,
                                  "exception": "ValueError", "error": "必需名称不在实际编译测试清单中"}}
            order = []
            def refused(*args):
                order.append("worker")
                return worker
            def probe(*args):
                order.append("probe")
                return command
            with patch.object(runner, "project", return_value=[task]), patch.object(runner, "worker", side_effect=refused) as workers, \
                    patch.object(runner, "record", side_effect=probe) as calls:
                result = self.a.diagnose(runner, case, {}, inspection)
            self.assertEqual(order, ["worker", "probe"])
            self.assertEqual(workers.call_args.args[0], task)
            self.assertEqual(result["candidate"]["status"], "structural-refusal")
            self.assertEqual(result["candidate"]["missing_source_names"], ["TestModernAbsent"])
            self.assertEqual(result["status"], "unknown")
            self.assertFalse(result["candidate"]["caught_original_defect"])
            self.assertEqual(result["isolated_original"]["origin"], "isolated-original-probe")
            self.assertEqual(result["isolated_original"]["status"], "red-reproduced")
            self.assertEqual(calls.call_count, 1)
            worker["receipt"].update(exception="CalledProcessError", error="old API incompatible with trusted harness")
            with patch.object(runner, "project", return_value=[task]), patch.object(runner, "worker", return_value=worker), \
                    patch.object(runner, "record", return_value=command) as setup_probe:
                setup = self.a.diagnose(runner, case, {}, inspection)
            self.assertEqual(setup_probe.call_count, 1, setup)
            self.assertEqual(setup["candidate"]["status"], "structural-refusal")
            source.write_text(source.read_text().replace("expected conflict without a receipt", "unrelated assertion"))
            with patch.object(runner, "project", return_value=[task]), patch.object(runner, "worker", return_value=worker), \
                    patch.object(runner, "record", return_value=command):
                mismatch = self.a.diagnose(runner, case, {}, inspection)
            self.assertEqual(mismatch["isolated_original"]["status"], "unknown")
            source.write_text(source.read_text().replace("unrelated assertion", "expected conflict without a receipt"))
            inspection["selection"]["all_race"] = False
            task.update(names=None, worker=0)
            with patch.object(runner, "project", return_value=[task]), patch.object(runner, "record", return_value=command) as calls:
                broad = self.a.diagnose(runner, case, {}, inspection)
            self.assertNotIn("isolated_original", broad, "Missing real selection must not become an execution refusal")
            self.assertEqual(broad["status"], "unknown")
            self.assertFalse(broad["candidate"]["unit_exercised"])
            self.assertEqual(calls.call_count, 0)

    def test_candidate_unit_uses_one_worker_and_keeps_partial_original_test_evidence(self):
        with tempfile.TemporaryDirectory() as directory:
            runner = self.runner(Path(directory))
            inspection, task = self.selection(unresolved=["other/package:TestModern"])
            names = list(self.a.RACE_TESTS)
            source = runner.history / "internal/coordination/fixture_test.go"
            source.parent.mkdir(parents=True)
            source.write_text("\n".join(f"func {name}(t *testing.T) {{}}" for name in names))
            raw = runner.output / "go.json"
            signature = "intent_pump_test.go:145: 物化工作分配 stable detail\n"
            raw.write_text(self.trace(names, names[:1], output=signature))
            (runner.output / "stdout").write_text(raw.read_text())
            (runner.output / "stderr").write_text("")
            invocation = {"argv": ["go", "test", "-count=1", "-race", "-json", "-run", "^(" + "|".join(names) + ")$", task["package"]],
                          "rc": 1, "stdout": "go.json"}
            worker = {"directory": ".", "tracked_unchanged": True,
                      "command": {"rc": 1, "timeout": False, "stdout": "stdout", "stderr": "stderr"},
                      "receipt": {"api_invoked": True, "commands": [invocation], "returned": False}}
            frozen = {"sha": "1dc588d7986397f773c6ddfd493105243490e7a5"}
            with patch.object(runner, "project", return_value=[task]), patch.object(runner, "worker", return_value=worker) as calls, \
                    patch.object(runner, "record", side_effect=AssertionError("Raw duplicate Go invocation is forbidden")):
                result = self.a.diagnose(runner, frozen, {}, inspection)
                self.assertEqual(result["status"], "red-reproduced")
                self.assertTrue(result["candidate"]["caught_original_defect"])
                self.assertEqual(calls.call_count, 1)
                raw.write_text(self.trace(names, names[:1], output="intent_pump_test.go:999: unrelated assertion\n"))
                unrelated = self.a.diagnose(runner, frozen, {}, inspection)
                self.assertEqual(unrelated["status"], "original-assertion-unconfirmed")
                self.assertFalse(unrelated["candidate"]["caught_original_defect"])
                self.assertEqual(unrelated["go_events"]["tests"][names[0]]["fail"], 1)
                raw.write_text(self.trace(names))
                invocation["rc"] = 0
                worker["command"]["rc"] = 0
                self.assertEqual(self.a.diagnose(runner, frozen, {}, inspection)["status"], "complete-no-red")
                worker["command"]["rc"] = 1
                self.assertEqual(self.a.diagnose(runner, frozen, {}, inspection)["status"], "unknown")
                invocation["rc"] = 1
                raw.write_text(self.trace(names[:-1], names[:1], output=signature))
                partial = self.a.diagnose(runner, frozen, {}, inspection)
                self.assertEqual(partial["status"], "unknown")
                self.assertEqual(partial["go_events"]["tests"][names[0]]["fail"], 1)
                self.assertFalse(partial["go_events"]["complete"])
                worker["receipt"]["commands"] = []
                missing = self.a.diagnose(runner, frozen, {}, inspection)
                self.assertEqual(missing["status"], "unknown")
                self.assertFalse(missing["candidate"]["unit_exercised"])

    def test_worker_adapter_preserves_original_output_rc_and_missing_api(self):
        with tempfile.TemporaryDirectory() as directory:
            runner = self.runner(Path(directory))
            policy = runner.root / "policy.py"
            policy.write_text('import subprocess, sys\nfrom pathlib import Path\n'
                              'def command(argv, root, **kwargs):\n'
                              '    return subprocess.run(argv, cwd=root, check=True, **kwargs)\n'
                              'def run_go(root, task):\n'
                              '    raw = Path(root) / "temporary-original.json"\n'
                              '    try:\n'
                              '        with raw.open("wb") as out:\n'
                              '            command([sys.executable, "-B", "-c", "print(123); raise SystemExit(7)"], root, stdout=out)\n'
                              '    finally:\n'
                              '        print(raw.read_text(), end="", flush=True)\n'
                              '        raw.unlink()\n')
            _, task = self.selection()
            request = runner.output / "task.json"
            request.write_text(json.dumps(task))
            receipt = runner.output / "receipt.json"
            argv = [sys.executable, "-B", "-c", self.a.WORKER, str(policy), str(runner.root), str(request), str(receipt)]
            result = runner.record(argv, runner.root, "adapter", 10)
            data = json.loads(receipt.read_text())
            self.assertTrue(data.get("api_invoked"), "Missing API and executed worker must be distinguishable")
            self.assertEqual(data["task"], task)
            self.assertEqual(result["rc"], 1)
            self.assertEqual(data["commands"][0]["rc"], 7)
            self.assertEqual((receipt.parent / data["commands"][0]["stdout"]).read_text(), "123\n")
            self.assertEqual((runner.output / result["stdout"]).read_text(), "123\n")
            self.assertFalse((runner.root / "temporary-original.json").exists())
            original_policy = policy.read_text()
            policy.write_text('import subprocess, sys\n'
                              'def command(argv, root, **kwargs):\n'
                              '    return subprocess.run(argv, check=True, **kwargs)\n'
                              'def run_go(root, task):\n'
                              '    command([sys.executable, "-B", "-c", "import sys; sys.stderr.write(\\\"captured original setup error\\\" + chr(10)); sys.exit(7)"], root, capture_output=True, text=True)\n')
            result = runner.record(argv, runner.root, "captured-setup", 10)
            data = json.loads(receipt.read_text())
            self.assertIn("stderr", data["commands"][0], "Captured Go setup stderr must survive the policy exception")
            self.assertEqual((receipt.parent / data["commands"][0]["stderr"]).read_text(), "captured original setup error\n")
            policy.write_text(original_policy.replace("def run_go(", "def missing_run_go("))
            result = runner.record(argv, runner.root, "absent-api", 10)
            data = json.loads(receipt.read_text())
            self.assertFalse(data.get("api_invoked", False))
            self.assertEqual(data["commands"], [])
            self.assertEqual(result["rc"], 1)

    def producing_policy(self, runner, task, text, compiled=None, rc=1):
        """A subprocess policy producer: no Go process, auth, or historical checkout changes."""
        scripts = runner.policy / ".github/scripts"
        scripts.mkdir(parents=True)
        source = scripts / "merge-gate.py"
        source.write_text(
            'from collections import Counter\nimport json, subprocess, sys, tempfile\nfrom pathlib import Path\n'
            f'COMPILED = {repr(compiled or task["names"])}\nTEXT = {text!r}\nRC = {rc}\n'
            'def command(argv, root, **kwargs):\n'
            '    return subprocess.run([sys.executable, "-B", "-c", "import sys; sys.stdout.write(" + repr(TEXT) + "); sys.exit(" + str(RC) + ")"], check=True, **kwargs)\n'
            'def inventory(root, package, race, directory, flags):\n'
            '    assert json.loads(Path(flags[0].split("=", 1)[1]).read_text())["Replace"]\n'
            '    return Counter(COMPILED)\n'
            'def run_go(root, task):\n'
            '    generated = root / "internal/store/merge_gate_authority_generated_test.go"\n'
            '    generated.parent.mkdir(parents=True, exist_ok=True)\n'
            '    generated.write_text("trusted generated test")\n'
            '    try:\n'
            '        with tempfile.TemporaryDirectory() as directory:\n'
            '            overlay = Path(directory) / "overlay.json"\n'
            '            overlay.write_text(json.dumps({"Replace": {str(root / "internal/store/merge_gate_migration_test.go"): str(generated)}}))\n'
            '            flags = ["-overlay=" + str(overlay)]\n'
            '            inventory(root, task["package"], task["race"], directory, flags)\n'
            '            with (Path(directory) / "go.json").open("wb") as output:\n'
            '                command(["go", "test", *flags, "-count=1", "-race", "-json", "-run", "^(" + "|".join(task["names"]) + ")$", task["package"]], root, stdout=output)\n'
            '    finally:\n'
            '        generated.unlink()\n')
        return source

    def partition_policy(self, runner, names, text, rc=1):
        policy = self.producing_policy(runner, {"names": names}, text, names, rc)
        # Deliberately reverse BATCHES: choose through the policy helper, not audit-side hash arithmetic.
        policy.write_text(policy.read_text() +
            '\nfrom contextlib import contextmanager\nfrom types import SimpleNamespace\nimport importlib.util\n'
            f'_spec = importlib.util.spec_from_file_location("verifier", {str(SCRIPT.parent / "verify-store-shard.py")!r})\n'
            'verifier = importlib.util.module_from_spec(_spec)\n_spec.loader.exec_module(verifier)\n'
            'shards = SimpleNamespace(BATCHES={"store": tuple(reversed(tuple(tuple(range(i, i+4)) for i in range(0,64,4))))})\n'
            '@contextmanager\ndef generated(root, authority):\n'
            '    with tempfile.TemporaryDirectory() as directory:\n'
            '        overlay = Path(directory) / "overlay.json"\n'
            '        overlay.write_text(json.dumps({"Replace": {"legacy": "trusted"}}))\n'
            '        yield ["-overlay=" + str(overlay)]\n'
            'HERE = Path(__file__).parent\n'
            'def module(name):\n    return SimpleNamespace(generated=generated)\n'
            'def run_go(root, task):\n'
            '    with generated(root, HERE / "merge-gate-migrations.json") as flags:\n'
            '        compiled = inventory(root, task["package"], task["race"], root, flags)\n'
            '        selected = sum((verifier.selection(compiled, slot, "store")[0] for slot in shards.BATCHES["store"][task["worker"]]), Counter())\n'
            '        pattern = verifier.run_pattern(selected)\n'
            '        with tempfile.TemporaryDirectory() as directory:\n'
            '            with (Path(directory) / "go.json").open("wb") as output:\n'
            '                command(["go", "test", *flags, "-count=1", *(["-race"] if task["race"] else []), "-json", "-run", pattern, task["package"]], root, stdout=output)\n')
        return policy

    def full_store_selection(self):
        # Store projection captured in hosted run 45870675051/1; the full inspect remained refused.
        names = ["TestMergeGateAllMigrationPrefixesUpgradeReopenReplay", "TestMergeGatePublishedMigrationAuthority",
                 "TestMergeGatePublishedMigrationAuthorityRejectsMutation", "TestMissingAndCrossGroupReferences",
                 "TestQueueClaimRacesAcrossHandles", self.a.EFFECT, "TestTaskWorkInputMutationUncertainty",
                 "TestWorkerArtifactCrossGroupVersionAndCallerIsolation", "TestWorkspaceOperationRevocationClaimAndAttemptFences",
                 "TestWorkspaceOperationUnresolvedOutcomesAndRecovery"]
        tasks = [{"unit": f"race-agentworkspace-internal-store-{worker}", "kind": "go", "package": PACKAGE,
                  "race": True, "profile": "store", "worker": worker, "bucket": None, "buckets": None,
                  "names": None, "required": names} for worker in [1, 2, 9, 12, 14, 15, 0, 3, 4, 5, 6, 7, 8, 10, 11, 13]]
        inspection = {"status": "policy-refusal", "selection": {"all_race": False, "packages": [PACKAGE],
                      "race_full": ["agentworkspace/cmd/workspace", "agentworkspace/internal/coordination", PACKAGE],
                      "race": {PACKAGE: names}, "tasks": [],
                      "unresolved": [PACKAGE + ":TestMergeGateAllMigrationPrefixesUpgradeReopenReplay"]}}
        return inspection, tasks

    def test_full_store_race_uses_complete_pinned_partition_for_original_red_pass_and_other_red(self):
        sha = "7d70dc49ff1f3c3ae3876b2a519059c60d88ec3d"
        inspection, tasks = self.full_store_selection()
        verifier_spec = importlib.util.spec_from_file_location("fixture_verifier", SCRIPT.parent / "verify-store-shard.py")
        verifier = importlib.util.module_from_spec(verifier_spec)
        verifier_spec.loader.exec_module(verifier)
        slot = next(slot for slot in range(64) if verifier.selection({self.a.EFFECT: 1}, slot, "store")[0])
        mate = next("TestPartitionMate" + str(i) for i in range(10000)
                    if verifier.selection({"TestPartitionMate" + str(i): 1}, slot, "store")[0])
        compiled = [*tasks[0]["required"], mate]
        slots = next(slots for slots in (tuple(range(i, i + 4)) for i in range(0, 64, 4)) if slot in slots)
        selected = sorted({name for slot in slots for name in verifier.selection(dict.fromkeys(compiled, 1), slot, "store")[0]})
        for failed, output, rc, expected in (([self.a.EFFECT], "task_work_input_effect_test.go:75: expected conflict without a receipt", 1, "red-reproduced"),
                                            ([], "", 0, "complete-no-red"),
                                            ([self.a.EFFECT], "task_work_input_effect_test.go:76: unrelated assertion", 1, "original-assertion-unconfirmed"),
                                            ([mate], "unrelated setup assertion", 1, "original-assertion-unconfirmed")):
            with self.subTest(expected=expected, failed=failed), tempfile.TemporaryDirectory() as directory:
                runner = self.runner(Path(directory))
                source = runner.history / "internal/store/task_work_input_effect_test.go"
                source.parent.mkdir(parents=True)
                source.write_text(f'func {self.a.EFFECT}(t *testing.T) {{\n' + "\n" * 73 +
                                  'conflict(f.s.HandBackWorkInputWithKey(ctx))\nt.Fatal("expected conflict without a receipt")\n}\n')
                self.partition_policy(runner, compiled, self.trace(selected, failed, PACKAGE, output + "\n"), rc)
                def git(root, *args):
                    return {("rev-parse", "HEAD"): (sha + "\n").encode(),
                            ("rev-parse", "HEAD^{tree}"): ("b" * 40 + "\n").encode()}.get(args, b"")
                real_record = runner.record
                def python_only(argv, *args):
                    self.assertEqual(argv[0], sys.executable, "No redundant isolated Go probe")
                    return real_record(argv, *args)
                with patch.object(runner, "trusted_policy"), patch.object(runner, "project", return_value=tasks), \
                        patch.object(runner, "git", side_effect=git), patch.object(runner, "record", side_effect=python_only):
                    result = self.a.diagnose(runner, {"sha": sha}, {}, inspection)
                self.assertEqual(result["status"], expected, result)
                candidate = result["candidate"]
                self.assertTrue(candidate["unit_exercised"])
                self.assertEqual(candidate["caught_original_defect"], expected == "red-reproduced")
                self.assertFalse(candidate["full_plan_executable"])
                self.assertEqual(candidate["task"], next(task for task in tasks if task["worker"] == candidate["task"]["worker"]))
                self.assertIsNone(candidate["task"]["names"])
                receipt = candidate["worker"]["receipt"]
                self.assertTrue(all(row["race"] for row in receipt["inventories"]), "Preflight must inventory the same race build")
                self.assertEqual(receipt["partition"]["selected"], dict.fromkeys(selected, 1))
                self.assertIn(mate, result["go_events"]["tests"])
                self.assertNotIn("isolated_original", result)

    def test_full_store_race_requires_actual_full_selection_and_unchanged_emitted_tasks(self):
        inspection, tasks = self.full_store_selection()
        # A named registry is not required when the real selector requests full-race coverage.
        inspection["selection"]["race"] = {}
        for task in tasks:
            task["required"] = []
        target = self.a.candidate_task(inspection, "effect", tasks)
        self.assertEqual(target["target_tasks"], tasks)
        for field, value in (("names", [self.a.EFFECT]), ("required", [self.a.EFFECT]), ("profile", "platform"),
                             ("worker", None), ("worker", -1), ("bucket", 0)):
            changed = copy.deepcopy(tasks)
            changed[0][field] = value
            with self.subTest(field=field, value=value), self.assertRaises(ValueError):
                self.a.candidate_task(inspection, "effect", changed)
        inspection["selection"]["race_full"] = []
        with self.assertRaises(ValueError):
            self.a.candidate_task(inspection, "effect", tasks)
        inspection["selection"]["all_race"] = True
        self.assertEqual(self.a.candidate_task(inspection, "effect", tasks)["target_tasks"], tasks)

    def test_missing_static_name_is_supplied_by_policy_harness_and_worker_cleans_up(self):
        sha = "7d70dc49ff1f3c3ae3876b2a519059c60d88ec3d"
        names = [self.a.EFFECT, "TestModernGenerated"]
        inspection, task = self.selection(names, [PACKAGE + ":TestModernGenerated"])
        task.update(package=PACKAGE, profile="store")
        inspection["selection"].update(race={PACKAGE: names}, packages=[PACKAGE])
        with tempfile.TemporaryDirectory() as directory:
            runner = self.runner(Path(directory))
            source = runner.history / "internal/store/task_work_input_effect_test.go"
            source.parent.mkdir(parents=True)
            source.write_text(f'func {self.a.EFFECT}(t *testing.T) {{\n' + "\n" * 73 +
                              'conflict(f.s.HandBackWorkInputWithKey(ctx))\nt.Fatal("expected conflict without a receipt")\n}\n')
            text = self.trace(names, [self.a.EFFECT], PACKAGE,
                              "task_work_input_effect_test.go:75: expected conflict without a receipt\n")
            self.producing_policy(runner, task, text)
            def git(root, *args):
                return {("rev-parse", "HEAD"): (sha + "\n").encode(),
                        ("rev-parse", "HEAD^{tree}"): ("b" * 40 + "\n").encode()}.get(args, b"")
            record = runner.record
            def python_only(argv, *args):
                self.assertEqual(argv[0], sys.executable, "Fixture must not invoke Go")
                return record(argv, *args)
            with patch.object(runner, "trusted_policy"), patch.object(runner, "project", return_value=[task]), \
                    patch.object(runner, "git", side_effect=git), patch.object(runner, "record", side_effect=python_only):
                result = self.a.diagnose(runner, {"sha": sha}, {}, inspection)
            self.assertEqual(result["status"], "red-reproduced", result)
            candidate = result["candidate"]
            self.assertEqual(candidate["missing_source_names"], [names[1]])
            self.assertTrue(candidate["worker"]["tracked_unchanged"])
            self.assertEqual(candidate["worker"]["source_after"]["sha"], sha)
            self.assertEqual(candidate["worker"]["source_after"]["tracked_status"], "")
            self.assertTrue(candidate["worker"]["receipt"]["inventories"][0]["build_flags"][0].startswith("-overlay="))
            self.assertNotIn("isolated_original", result)
            self.assertFalse((source.parent / "merge_gate_authority_generated_test.go").exists())

    def test_original_race_signatures_require_fail_in_the_exact_attributed_test(self):
        self.assertTrue(hasattr(self.a, "original_signatures"), "Original assertion matching is missing")
        name = "TestIntentPassHeldAndGenerationConflict"
        subtest = name + "/held-both-kinds"
        output = "intent_pump_test.go:283: held is a persisted disposition, not a pass error: detail\n"
        for attributed, action, matches in ((subtest, "fail", True), (name, "fail", False),
                                            (name + "/other", "fail", False), (subtest, "pass", False)):
            rows = [{"Action": "start", "Package": PACKAGE}, {"Action": "run", "Package": PACKAGE, "Test": name},
                    {"Action": "run", "Package": PACKAGE, "Test": attributed}] if attributed != name else [
                        {"Action": "start", "Package": PACKAGE}, {"Action": "run", "Package": PACKAGE, "Test": name}]
            rows += [{"Action": "output", "Package": PACKAGE, "Test": attributed, "Output": output},
                     {"Action": action, "Package": PACKAGE, "Test": attributed}]
            if attributed != name:
                rows.append({"Action": "fail", "Package": PACKAGE, "Test": name})
            rows.append({"Action": "fail", "Package": PACKAGE})
            summary = self.a.observations("".join(json.dumps(row) + "\n" for row in rows), PACKAGE, [name])
            with self.subTest(attributed=attributed, action=action):
                signatures = self.a.original_signatures(summary, self.a.RACE_SIGNATURES)
                self.assertEqual(any(row["matches"] for row in signatures), matches)

    def test_original_signature_cannot_infer_run_from_output_and_fail_alone(self):
        name = self.a.RACE_TESTS[0]
        rows = [{"Action": "start", "Package": PACKAGE},
                {"Action": "output", "Package": PACKAGE, "Test": name, "Output": "intent_pump_test.go:145: 物化工作分配 detail\n"},
                {"Action": "fail", "Package": PACKAGE, "Test": name}, {"Action": "fail", "Package": PACKAGE}]
        summary = self.a.observations("".join(json.dumps(row) + "\n" for row in rows), PACKAGE, [name])
        self.assertFalse(any(row["matches"] for row in self.a.original_signatures(summary, self.a.RACE_SIGNATURES)))

    def test_added_classes_bind_original_jobs_and_frozen_trees(self):
        cases = {row["sha"]: row for row in self.a.manifest(SCRIPT.with_name("ci-historical-cases.json").read_text())}
        expected = [("3e56ab2fa42856c05bc31e8364144ffa43f0399d", "180528d0c481fb3b68fd290faafbfc976181599c", 44037303018, 133883154642),
                    ("41e24737b4b1cfe545219378694dcb1608d8a629", "8de84f4b6b7f310461c45c0f7b8e26625a2d6d06", 44282887126, 134727936958),
                    ("85b488fa772dc1197241e730131aee5f2c8d365a", "c998d4bbb3b3db38338ac03a2873b56f57bc7cc3", 43816639142, 133133787471)]
        self.assertEqual(len(self.a.DIAGNOSTICS), 8)
        for sha, tree, run, job in expected:
            with self.subTest(sha=sha):
                self.assertIn(sha, self.a.DIAGNOSTICS)
                spec = self.a.DIAGNOSTICS[sha]
                self.assertEqual(cases[sha]["tree"], tree)
                self.assertIn([run, 1], cases[sha]["runs"])
                self.assertEqual((spec["run_id"], spec["job_id"]), (run, job))

    def migration_sources(self, runner, spec):
        for original in spec["originals"]:
            path = runner.history / original["file"]
            path.parent.mkdir(parents=True, exist_ok=True)
            # Related assertions can share a file; keep their original source coordinates.
            lines = path.read_text().splitlines() if path.exists() else []
            lines += [""] * max(0, original["line"] + 2 - len(lines))
            lines[original["line"] - 3] = f'func {original["test"]}(t *testing.T) {{'
            lines[original["line"] - 2] = original.get("source_call", "")
            lines[original["line"] - 1] = original["assertion"]
            lines[original["line"]] = "}"
            path.write_text("\n".join(lines) + "\n")
            if original.get("helper_file"):
                helper = runner.history / original["helper_file"]
                helper.write_text(helper.read_text() + '\n' + original["helper_literal"] + '\n')

    def test_migration_uses_pinned_compiled_partition_and_entire_emitted_task(self):
        sha = "3e56ab2fa42856c05bc31e8364144ffa43f0399d"
        self.assertIn(sha, self.a.DIAGNOSTICS, "Migration class is missing")
        spec = self.a.DIAGNOSTICS[sha]
        with tempfile.TemporaryDirectory() as directory:
            runner = self.runner(Path(directory))
            self.migration_sources(runner, spec)
            names = [spec["representative"], "TestOtherInventoryItem"]
            tasks = [{"unit": f"ordinary-store-{worker}", "kind": "go", "package": PACKAGE, "race": False,
                      "profile": "store", "worker": worker, "bucket": None, "buckets": None,
                      "names": None, "required": []} for worker in range(16)]
            inspection = {"status": "policy-refusal", "selection": {"packages": [PACKAGE], "ordinary_required": {},
                          "unresolved": [PACKAGE + ":TestLater"], "tasks": []}}
            policy = self.partition_policy(runner, names, self.trace(names[:1], names[:1], PACKAGE,
                "queue_schema_test.go:126: upgrade changed message receipt: stable\n"))
            def git(root, *args):
                return {("rev-parse", "HEAD"): (sha + "\n").encode(),
                        ("rev-parse", "HEAD^{tree}"): ("b" * 40 + "\n").encode()}.get(args, b"")
            real_record = runner.record
            def python_only(argv, *args):
                self.assertEqual(argv[0], sys.executable, "Unexpected isolated Go probe")
                return real_record(argv, *args)
            with patch.object(runner, "trusted_policy"), patch.object(runner, "project", return_value=tasks), \
                    patch.object(runner, "git", side_effect=git), patch.object(runner, "record", side_effect=python_only):
                result = self.a.diagnose(runner, {"sha": sha}, {}, inspection)
            self.assertEqual(result["status"], "red-reproduced", result)
            candidate = result["candidate"]
            self.assertFalse(candidate["full_plan_executable"])
            self.assertEqual(candidate["task"], tasks[candidate["task"]["worker"]])
            self.assertIsNone(candidate["task"]["names"])
            self.assertEqual(candidate["worker"]["receipt"]["partition"]["compiled"], dict.fromkeys(names, 1))
            self.assertIn(spec["representative"], candidate["worker"]["receipt"]["partition"]["selected"])
            self.assertNotIn("isolated_original", result)
            policy.write_text(policy.read_text() + '\ndef inventory(*args):\n    raise ValueError("legacy API incompatible with trusted harness")\n')
            probes = []
            def original_probe(argv, cwd, prefix, timeout):
                if argv[0] != "go":
                    return real_record(argv, cwd, prefix, timeout)
                probes.append(argv)
                raw = runner.output / "legacy-probe.json"
                legacy = [row["test"] for row in spec["originals"]]
                raw.write_text(self.trace(legacy, legacy[:1], PACKAGE, "queue_schema_test.go:126: upgrade changed message receipt: detail\n"))
                (runner.output / "legacy-stderr").write_text("")
                return {"argv": argv, "rc": 1, "timeout": False, "stdout": raw.name, "stderr": "legacy-stderr"}
            with patch.object(runner, "trusted_policy"), patch.object(runner, "project", return_value=tasks), \
                    patch.object(runner, "git", side_effect=git), patch.object(runner, "record", side_effect=original_probe):
                refused = self.a.diagnose(runner, {"sha": sha}, {}, inspection)
            self.assertEqual(refused["candidate"]["status"], "structural-refusal", refused)
            self.assertTrue(refused["candidate"]["worker"]["receipt"]["api_invoked"])
            self.assertIn("inventory_refusal", refused["candidate"]["worker"]["receipt"]["partition"])
            self.assertEqual(len(probes), 1)
            self.assertEqual(probes[0][:7], ["go", "test", "-count=1", "-race", "-json", "-run", "^(" + "|".join(row["test"] for row in spec["originals"]) + ")$"])
            self.assertEqual(refused["isolated_original"]["status"], "red-reproduced", refused)
            self.assertFalse(refused["candidate"]["caught_original_defect"])

    def test_migration_keeps_specific_failure_across_multiple_commands_and_other_timeout(self):
        self.assertTrue(hasattr(self.a, "worker_observations"), "Multi-command worker consumer is missing")
        with tempfile.TemporaryDirectory() as directory:
            runner = self.runner(Path(directory))
            names = ["TestQueueV3UpgradePreservesMessagesAndReceipts", "TestUnrelated"]
            first = self.trace(names[:1], names[:1], PACKAGE, "queue_schema_test.go:126: upgrade changed message receipt: detail\n")
            second = self.trace(names[1:], package=PACKAGE)[:-30]
            commands = []
            for i, (name, text, rc) in enumerate(zip(names, [first, second], [1, -15])):
                path = runner.output / f"part-{i}.json"
                path.write_text(text)
                commands.append({"argv": ["go", "test", "-count=1", "-json", "-run", "^" + name + "$", PACKAGE],
                                 "rc": rc, "stdout": path.name})
            task = {"kind": "go", "package": PACKAGE, "race": False, "names": None}
            worker = {"directory": ".", "receipt": {"commands": commands, "partition": {"selected": dict.fromkeys(names, 1)}}}
            summary = self.a.worker_observations(runner, worker, task)
            self.assertFalse(summary["complete"])
            self.assertEqual(len(summary["commands"]), 2)
            matcher = {"test": names[0], "file": "queue_schema_test.go", "line": 126, "literal": "upgrade changed message receipt"}
            self.assertTrue(self.a.original_signatures(summary["commands"][0]["go_events"], [matcher])[0]["matches"])
            wrong = first.replace("queue_schema_test.go:126", "queue_schema_test.go:127")
            self.assertFalse(self.a.original_signatures(self.a.observations(wrong, PACKAGE, names[:1]), [matcher])[0]["matches"])

    def test_frozen_migration_sources_and_signatures_reject_moved_assertions(self):
        spec = self.a.DIAGNOSTICS["41e24737b4b1cfe545219378694dcb1608d8a629"]
        with tempfile.TemporaryDirectory() as directory:
            runner = self.runner(Path(directory))
            self.migration_sources(runner, spec)
            sources = self.a.migration_sources(runner, spec)
            self.assertTrue(all(row["source_matches"] for row in sources), sources)
            for original in spec["originals"]:
                name = original["test"]
                text = self.trace([name], [name], PACKAGE, f'{Path(original["file"]).name}:{original["line"]}: {original["literal"]}\n')
                summary = self.a.observations(text, PACKAGE, [name])
                self.assertTrue(any(row["matches"] for row in self.a.migration_signatures(summary, sources)))
                path = runner.history / original["file"]
                saved = path.read_text()
                path.write_text("\n" + saved)
                self.assertFalse(any(row["matches"] for row in self.a.migration_signatures(summary, self.a.migration_sources(runner, spec))))
                path.write_text(saved)

    def python_original(self, runner, spec):
        path = runner.history / spec["file"]
        path.parent.mkdir(parents=True, exist_ok=True)
        lines = ['import unittest\nfrom types import SimpleNamespace\nclass WorkflowTests(unittest.TestCase):',
                 '    def test_remaining_package_exclusion_executes_actual_shell_without_go(self):',
                 '        for label, accepted in [("valid", True)]:',
                 '            with self.subTest(case=label):',
                 '                result = SimpleNamespace(returncode=1, stderr="")']
        text = '\n'.join(lines) + '\n'
        text += '\n' * (spec["line"] - 1 - len(text.splitlines()))
        text += '                self.assertEqual(result.returncode == 0, accepted, result.stderr)\n'
        path.write_text(text + '\nif __name__ == "__main__":\n    unittest.main()\n')
        workflow = runner.history / ".github/workflows/ci.yml"
        workflow.parent.mkdir(parents=True)
        workflow.write_text('      - name: Test Store shard gates\n        run: python3 .github/scripts/test-store-shard.py -v\n')
        return path

    def test_ci_registration_refusal_and_original_unittest_are_independent(self):
        sha = "85b488fa772dc1197241e730131aee5f2c8d365a"
        self.assertIn(sha, self.a.DIAGNOSTICS, "CI rules class is missing")
        spec = self.a.DIAGNOSTICS[sha]
        with tempfile.TemporaryDirectory() as directory:
            runner = self.runner(Path(directory))
            path = self.python_original(runner, spec)
            scripts = runner.policy / ".github/scripts"
            scripts.mkdir(parents=True)
            (scripts / "merge-gate-manifest.json").write_text(json.dumps({"ci_tests": [path.name, "test-later.py"]}))
            (scripts / "merge-gate.py").write_text(
                'import json, subprocess, sys\nfrom pathlib import Path\nHERE = Path(__file__).parent\n'
                'def read_json(path):\n    return json.loads(Path(path).read_text())\n'
                'def command(argv, root, **kwargs):\n    return subprocess.run(argv, cwd=root, check=True, **kwargs)\n'
                'def ci_tests(root, manifest):\n'
                '    paths = sorted((root / ".github/scripts").glob("test-*.py"))\n'
                '    missing = set(manifest["ci_tests"]) - {p.name for p in paths}\n'
                '    if missing: raise ValueError("必需 CI 自测缺失：" + ", ".join(sorted(missing)))\n'
                '    return paths\n')
            task = {"unit": "ci", "kind": "ci"}
            inspection = {"status": "selected", "selection": {"ci": True, "unresolved": [], "tasks": [task]}}
            def git(root, *args):
                return {("rev-parse", "HEAD"): (sha + "\n").encode(),
                        ("rev-parse", "HEAD^{tree}"): ("b" * 40 + "\n").encode()}.get(args, b"")
            with patch.object(runner, "trusted_policy"), patch.object(runner, "project", return_value=[task]), \
                    patch.object(runner, "git", side_effect=git):
                result = self.a.diagnose(runner, {"sha": sha}, {}, inspection)
                self.assertEqual(result["candidate"]["status"], "structural-refusal", result)
                self.assertFalse(result["candidate"]["caught_original_defect"])
                self.assertEqual(result["candidate"]["worker"]["receipt"]["commands"], [])
                self.assertEqual(result["isolated_original"]["status"], "red-reproduced", result)
                self.assertFalse(result["isolated_original"]["candidate_unit_caught"])
                (scripts / "merge-gate-manifest.json").write_text(json.dumps({"ci_tests": [path.name]}))
                caught = self.a.diagnose(runner, {"sha": sha}, {}, inspection)
                self.assertEqual(caught["status"], "red-reproduced", caught)
                self.assertNotIn("isolated_original", caught)
                self.assertEqual(caught["candidate"]["worker"]["receipt"]["api"], "ci_tests+native-script-loop")
                with patch.object(runner, "project", side_effect=ValueError("actual selector unavailable")), \
                        patch.object(runner, "worker") as no_worker:
                    unknown = self.a.diagnose(runner, {"sha": sha}, {}, inspection)
                self.assertEqual(unknown["status"], "unknown")
                self.assertEqual(unknown["source"]["test"], spec["test"], "Source provenance must survive an earlier selector failure")
                no_worker.assert_not_called()
                original = path.read_text()
                path.write_text(original.replace("returncode=1", "returncode=0"))
                green = self.a.diagnose(runner, {"sha": sha}, {}, inspection)
                self.assertEqual(green["status"], "complete-no-red", green)
                path.write_text(original.replace("    unittest.main()", "    pass"))
                no_run = self.a.diagnose(runner, {"sha": sha}, {}, inspection)
                self.assertEqual(no_run["status"], "unknown", no_run)
                self.assertEqual(no_run["isolated_original"]["status"], "unknown", no_run)

    def test_ci_signature_requires_original_guard_trace_test_subcase_and_assertion(self):
        sha = "85b488fa772dc1197241e730131aee5f2c8d365a"
        self.assertIn(sha, self.a.DIAGNOSTICS, "CI source guard is missing")
        spec = self.a.DIAGNOSTICS[sha]
        with tempfile.TemporaryDirectory() as directory:
            runner = self.runner(Path(directory))
            path = self.python_original(runner, spec)
            text = ("FAIL: test_remaining_package_exclusion_executes_actual_shell_without_go (__main__.WorkflowTests.test_remaining_package_exclusion_executes_actual_shell_without_go) (case='valid')\n"
                    f'  File "{path}", line 706, in test_remaining_package_exclusion_executes_actual_shell_without_go\n'
                    "AssertionError: False != True : \n")
            self.assertTrue(self.a.python_signature(runner, spec, text)["matches"])
            for wrong in (text.replace("False != True", "True != False"), text.replace("case='valid'", "case='empty-store'"),
                          text.replace("line 706", "line 707"), text.replace("__main__.WorkflowTests", "__main__.OtherTests")):
                self.assertFalse(self.a.python_signature(runner, spec, wrong)["matches"])
            original = path.read_text()
            path.write_text("\n" + original)
            self.assertFalse(self.a.python_signature(runner, spec, text)["source_matches"])
            path.unlink()
            self.assertFalse(self.a.python_signature(runner, spec, text)["source_matches"])

    def test_actions_read_token_is_only_used_by_initial_fixed_api_read(self):
        expected = {"sha": "a" * 40, "run_id": "123", "run_attempt": "2"}
        api = {"id": 123, "run_attempt": 2, "event": "workflow_dispatch", "head_sha": "a" * 40,
               "head_branch": "codex/ci-historical-audit-20261126", "path": ".github/workflows/ci.yml",
               "repository": {"full_name": "PeterFile/ct0x"}}
        with patch.dict(os.environ, {"AUDIT_READ_TOKEN": "read-only-fixture-token"}), \
                patch.object(self.a.urllib.request, "urlopen", return_value=io.BytesIO(json.dumps(api).encode())) as call:
            self.a.public_run(expected)
            request = call.call_args.args[0]
            self.assertEqual(request.get_method(), "GET")
            self.assertEqual(request.full_url, "https://api.github.com/repos/PeterFile/ct0x/actions/runs/123/attempts/2")
            self.assertEqual(request.get_header("Authorization"), "Bearer read-only-fixture-token")
            self.assertNotIn("AUDIT_READ_TOKEN", os.environ)

    def test_terminal_index_never_turns_missing_work_green(self):
        index = {"errors": [], "rows": [{**CASE, "source_state": "exact", "selector": {"status": "pending"},
                                         "diagnostic": {"status": "not-requested"}}]}
        self.a.finish(index)
        self.assertEqual(index["status"], "infrastructure-error")
        self.assertEqual(index["rows"][0]["selector"]["status"], "unknown")
        self.assertEqual(index["coverage"], {"requested": 1, "exact": 1, "missing": 0})
        complete = copy.deepcopy(index)
        complete["rows"][0]["selector"] = {"status": "policy-refusal"}
        self.a.finish(complete)
        self.assertEqual(complete["status"], "complete")
        unattempted = copy.deepcopy(complete)
        unattempted["rows"][0]["source_state"] = "pending"
        self.a.finish(unattempted)
        self.assertEqual(unattempted["rows"][0]["source_state"], "unknown")
        self.assertEqual(unattempted["status"], "infrastructure-error")

    def miss_task(self, spec, **overrides):
        task = {"unit": "race-" + spec["package"].replace("/", "-"), "kind": "go",
                "package": spec["package"], "race": True, "profile": None, "worker": None,
                "bucket": None, "buckets": None, "names": [spec["test"], "TestPeer"],
                "required": [spec["test"], "TestPeer"]}
        task.update(overrides)
        return task

    def miss_events(self, spec, output=None, attribute=None, case_fail=True, peer_fail=False, green=False,
                    skip_case=False):
        package, root, case = spec["package"], spec["test"], spec["case"]
        rows = [{"Action": "start", "Package": package},
                {"Action": "run", "Package": package, "Test": root}]
        if "/" in case:
            rows.append({"Action": "run", "Package": package, "Test": case})
        if output is not None:
            rows.append({"Action": "output", "Package": package, "Test": attribute or case, "Output": output})
        if skip_case:
            if "/" in case:
                rows.append({"Action": "skip", "Package": package, "Test": case})
            rows.append({"Action": "skip", "Package": package, "Test": root})
        elif green or not case_fail:
            if "/" in case:
                rows.append({"Action": "pass", "Package": package, "Test": case})
            rows.append({"Action": "pass", "Package": package, "Test": root})
        else:
            if "/" in case:
                rows.append({"Action": "fail", "Package": package, "Test": case})
            rows.append({"Action": "fail", "Package": package, "Test": root})
        rows.append({"Action": "run", "Package": package, "Test": "TestPeer"})
        rows.append({"Action": "fail" if peer_fail and not green else "pass", "Package": package, "Test": "TestPeer"})
        rows.append({"Action": "fail" if not green and (case_fail or peer_fail) else "pass", "Package": package})
        return "".join(json.dumps(row) + "\n" for row in rows)

    def miss_worker(self, runner, task, text, rc=1, names=None, returned=None, exception=None):
        names = list(names if names is not None else task.get("names") or [])
        raw = runner.output / ("miss-" + task["unit"].replace("/", "-") + ".json")
        raw.write_text(text)
        (runner.output / "miss.stderr").write_text("")
        invocation = {"argv": ["go", "test", "-count=1", "-race", "-json", "-run",
                               "^(" + "|".join(names) + ")$", task["package"]],
                      "rc": rc, "stdout": raw.name}
        inventory = {"argv": ["go", "test", "-count=1", "-race", "-json", "-list",
                              "^(Test|Example|Fuzz)", task["package"]],
                     "rc": 0, "stdout": "inventory.json"}
        if returned is None:
            returned = rc == 0
        if exception is None and rc:
            exception = "CalledProcessError"
        receipt = {"task": task, "api_invoked": True, "returned": returned, "commands": [inventory, invocation]}
        if returned:
            receipt["detail"] = {"compiled": dict.fromkeys(names, 1),
                                 "selected": dict.fromkeys(names, 1),
                                 "runs": dict.fromkeys(names, 1),
                                 "passes": dict.fromkeys(names, 0 if rc else 1),
                                 "skips": {}}
        if exception:
            receipt.update(exception=exception, error="go test failed")
        return {"directory": ".", "tracked_unchanged": True,
                "command": {"rc": 1 if exception else 0, "timeout": False,
                            "stdout": raw.name, "stderr": "miss.stderr"},
                "receipt": receipt}

    def test_misses_phase_is_three_frozen_sources_and_defaults_unchanged(self):
        self.assertTrue(hasattr(self.a, "MISSES"), "Three historical selection misses are missing")
        cases = self.a.manifest(SCRIPT.with_name("ci-historical-cases.json").read_text())
        by_sha = {row["sha"]: row for row in cases}
        expected = {
            "5aa8203bda038d3ce08ef51bfef1e20ba8c5dca1": (
                162, 44088192148, 134063669160, "05537fee25f147e6aa137558b68e0cd28e1c0eac",
                "agentworkspace/cmd/workspace",
                "TestCoordinationCompositionUnknownCancelAndShutdownHoldClaims",
                "TestCoordinationCompositionUnknownCancelAndShutdownHoldClaims/shutdown",
                "cmd/workspace/coordination_composition_test.go", [528, 628]),
            "56d236dda96e1a5140031b6d7069917394a23d2f": (
                176, 44235577412, 134570782516, "59439a61f63bd2892e722c02ff7c9303c357a1e4",
                "agentworkspace/internal/platform",
                "TestChatRepairPreparationWaitsUnclaimedAndSchedulerContinues",
                "TestChatRepairPreparationWaitsUnclaimedAndSchedulerContinues",
                "internal/platform/prepare_repair_wiring_test.go", [254]),
            "4b87e75cfcaeaac19d7e1e8e1db076bf673651ba": (
                239, 45512885246, 138866224228, "0e17ec379a262401889aa9bbe343b5e2b3d6c688",
                "agentworkspace/internal/coordination",
                "TestIntentPassCancellationAndQueryFailure",
                "TestIntentPassCancellationAndQueryFailure/cancel-between-items-and-resume",
                "internal/coordination/intent_pump_test.go", [789]),
        }
        self.assertEqual(set(self.a.MISSES), set(expected))
        self.assertEqual(len(self.a.DIAGNOSTICS), 8)
        self.assertFalse(set(self.a.MISSES) & set(self.a.DIAGNOSTICS))
        for sha, (pr, run, job, tree, package, test, case, path, lines) in expected.items():
            with self.subTest(sha=sha):
                spec = self.a.MISSES[sha]
                self.assertEqual((spec["pr"], spec["run_id"], spec["run_attempt"], spec["job_id"]),
                                 (pr, run, 1, job))
                self.assertEqual((spec["package"], spec["test"], spec["case"], spec["file"]),
                                 (package, test, case, path))
                self.assertEqual([row["line"] for row in spec["signatures"]], lines)
                self.assertEqual(by_sha[sha]["tree"], tree)
                self.assertIn([run, 1], by_sha[sha]["runs"])
                self.assertIn(test, spec["original_tests"])
                if case != test:
                    self.assertIn(case, spec["original_tests"])
        options = self.a.request({"policy_sha": "a" * 40, "phase": "misses"}, len(cases))
        selected, diagnostics = self.a.select_sources(cases, options)
        self.assertEqual([row["sha"] for row in selected], list(self.a.MISSES))
        self.assertEqual(diagnostics, set())
        for sha in self.a.MISSES:
            focused = self.a.request({"policy_sha": "a" * 40, "phase": "misses", "focus_sha": sha,
                                      "offset": "112", "count": "1"}, len(cases))
            rows, diagnostics = self.a.select_sources(cases, focused)
            self.assertEqual([row["sha"] for row in rows], [sha])
            self.assertEqual(diagnostics, set())
        with self.assertRaises(ValueError):
            self.a.request({"policy_sha": "a" * 40, "phase": "misses",
                            "focus_sha": "7d70dc49ff1f3c3ae3876b2a519059c60d88ec3d"}, len(cases))
        with self.assertRaises(ValueError):
            self.a.select_sources(cases, {"phase": "misses", "focus_sha": "7d70dc49ff1f3c3ae3876b2a519059c60d88ec3d"})
        default = self.a.request({"policy_sha": "a" * 40, "phase": "both"}, len(cases))
        rows, diagnostics = self.a.select_sources(cases, default)
        self.assertEqual(len(rows), 113)
        self.assertEqual(diagnostics, set(self.a.DIAGNOSTICS))

    def test_miss_source_mapping_and_case_signatures_are_frozen(self):
        with tempfile.TemporaryDirectory() as directory:
            runner = self.runner(Path(directory))
            for sha, spec in self.a.MISSES.items():
                with self.subTest(sha=sha):
                    path = runner.history / spec["file"]
                    path.parent.mkdir(parents=True, exist_ok=True)
                    path.write_text(f'func {spec["test"]}(t *testing.T) {{\n}}\n')
                    sources, matches = self.a.miss_sources(runner, spec)
                    self.assertTrue(matches, sources)
                    self.assertEqual(sources[0]["file"], spec["file"])
                    matchers = self.a.miss_matchers(spec)
                    self.assertEqual(len(matchers), len(spec["signatures"]) * (2 if "/" in spec["case"] else 1))
                    path.unlink()
                    other = runner.history / spec["file"].replace(Path(spec["file"]).name, "other_test.go")
                    other.write_text(f'func {spec["test"]}(t *testing.T) {{\n}}\n')
                    self.assertFalse(self.a.miss_sources(runner, spec)[1])
                    other.unlink()

    def test_miss_select_unit_uses_pinned_bucket_and_partition_helpers(self):
        spec = self.a.MISSES["5aa8203bda038d3ce08ef51bfef1e20ba8c5dca1"]
        with tempfile.TemporaryDirectory() as directory:
            runner = self.runner(Path(directory))
            scripts = runner.policy / ".github/scripts"
            scripts.mkdir(parents=True)
            (scripts / "merge-gate.py").write_text(
                'import hashlib\nfrom collections import Counter\nfrom types import SimpleNamespace\n'
                'def bucket_selection(compiled, bucket, total):\n'
                '    return Counter({name: count for name, count in compiled.items()\n'
                '                    if int.from_bytes(hashlib.sha256(name.encode()).digest()[:8], "big") % total == bucket})\n'
                'class Verifier:\n'
                '    @staticmethod\n'
                '    def selection(compiled, slot, profile):\n'
                '        return Counter({name: count for name, count in compiled.items() if slot == 0}), None\n'
                'verifier = Verifier()\n'
                'shards = SimpleNamespace(BATCHES={"store": ((0,), (1,))})\n')
            tasks = [{"unit": "race-cmd-workspace-" + str(i), "kind": "go", "package": spec["package"],
                      "race": True, "profile": None, "worker": None, "bucket": i, "buckets": 8,
                      "names": None, "required": []} for i in range(8)]
            expected = int.from_bytes(hashlib.sha256(spec["test"].encode()).digest()[:8], "big") % 8
            with patch.object(runner, "trusted_policy"):
                choice = runner.select_unit(tasks, spec["package"], spec["test"], Path("sources") / "x" / "misses")
            self.assertEqual(choice["status"], "selected", choice)
            self.assertEqual(choice["matches"], [tasks[expected]])
            wrong = [{**task, "race": False} for task in tasks]
            with patch.object(runner, "trusted_policy"):
                choice = runner.select_unit(wrong, spec["package"], spec["test"], Path("sources") / "x" / "wrong")
            self.assertEqual(choice["status"], "wrong-mode")
            named = [{**tasks[0], "bucket": None, "buckets": None, "names": ["TestOther"],
                      "required": ["TestOther"]}]
            with patch.object(runner, "trusted_policy"):
                choice = runner.select_unit(named, spec["package"], spec["test"], Path("sources") / "x" / "named")
            self.assertEqual(choice["status"], "not-selected")
            partition = [{"unit": "race-store-" + str(i), "kind": "go", "package": "agentworkspace/internal/store",
                          "race": True, "profile": "store", "worker": i, "bucket": None, "buckets": None,
                          "names": None, "required": []} for i in (0, 1)]
            with patch.object(runner, "trusted_policy"):
                choice = runner.select_unit(partition, "agentworkspace/internal/store", "TestPartition",
                                            Path("sources") / "x" / "partition")
            self.assertEqual(choice["status"], "selected", choice)
            self.assertEqual(choice["matches"][0]["worker"], 0)
            unsupported = [{**partition[0], "profile": "missing"}]
            with patch.object(runner, "trusted_policy"):
                choice = runner.select_unit(unsupported, "agentworkspace/internal/store", "TestPartition",
                                            Path("sources") / "x" / "unsupported")
            self.assertEqual(choice["status"], "unsupported-worker-shape")

    def test_miss_diagnose_requires_named_case_line_and_peer_failure_is_not_catch(self):
        for sha, line in (("5aa8203bda038d3ce08ef51bfef1e20ba8c5dca1", 528),
                          ("56d236dda96e1a5140031b6d7069917394a23d2f", 254)):
            spec = self.a.MISSES[sha]
            task = self.miss_task(spec)
            with self.subTest(sha=sha), tempfile.TemporaryDirectory() as directory:
                runner = self.runner(Path(directory))
                source = runner.history / spec["file"]
                source.parent.mkdir(parents=True)
                source.write_text(f'func {spec["test"]}(t *testing.T) {{\n}}\n')
                inspection = {"status": "selected", "selection": {"unresolved": [], "tasks": [task]}}
                output = f'{Path(spec["file"]).name}:{line}: {spec["signatures"][0]["literal"]}\n'
                def diagnose(worker):
                    with patch.object(runner, "select_unit", return_value={"status": "selected", "matches": [task]}), \
                            patch.object(runner, "worker", return_value=worker) as calls:
                        result = self.a.diagnose_miss(runner, {"sha": sha}, {}, inspection)
                    self.assertEqual(calls.call_count, 1)
                    self.assertEqual(calls.call_args.args[0], task)
                    return result
                red = diagnose(self.miss_worker(runner, task, self.miss_events(spec, output=output), rc=1))
                self.assertEqual(red["status"], "red-reproduced", red)
                self.assertTrue(red["candidate"]["caught_original_defect"])
                self.assertTrue(red["candidate"]["unit_exercised"])
                self.assertEqual(red["task_origin"], "emitted-task")
                self.assertFalse(red["candidate"]["projected_unit"])
                self.assertTrue(all("-race" in row["argv"] for row in red["candidate"]["actual_go_commands"]))
                self.assertEqual(red["candidate"]["task"], task)
                if "/" in spec["case"]:
                    parent = diagnose(self.miss_worker(runner, task, self.miss_events(
                        spec, output=output, attribute=spec["test"]), rc=1))
                    self.assertEqual(parent["status"], "red-reproduced", parent)
                if len(spec["signatures"]) > 1:
                    second = (f'{Path(spec["file"]).name}:{spec["signatures"][1]["line"]}: '
                              f'{spec["signatures"][1]["literal"]}\n')
                    second_red = diagnose(self.miss_worker(runner, task, self.miss_events(spec, output=second), rc=1))
                    self.assertEqual(second_red["status"], "red-reproduced", second_red)
                wrong = diagnose(self.miss_worker(runner, task, self.miss_events(
                    spec, output=output.replace(f":{line}:", f":{line + 1}:")), rc=1))
                self.assertEqual(wrong["status"], "original-assertion-unconfirmed")
                peer = diagnose(self.miss_worker(runner, task, self.miss_events(
                    spec, case_fail=False, peer_fail=True), rc=1))
                self.assertEqual(peer["status"], "original-assertion-unconfirmed")
                self.assertFalse(peer["candidate"]["caught_original_defect"])
                green = diagnose(self.miss_worker(runner, task, self.miss_events(spec, output=output, green=True), rc=0))
                self.assertEqual(green["status"], "complete-no-red")
                self.assertFalse(green["candidate"]["caught_original_defect"])
                skipped = diagnose(self.miss_worker(runner, task, self.miss_events(spec, skip_case=True), rc=0))
                self.assertEqual(skipped["status"], "selected-but-unreached")
                self.assertTrue(skipped["candidate"]["selected_but_unreached"])

    def test_miss_diagnose_labels_projection_and_selected_but_unreached(self):
        sha = "4b87e75cfcaeaac19d7e1e8e1db076bf673651ba"
        spec = self.a.MISSES[sha]
        task = self.miss_task(spec)
        with tempfile.TemporaryDirectory() as directory:
            runner = self.runner(Path(directory))
            source = runner.history / spec["file"]
            source.parent.mkdir(parents=True)
            source.write_text(f'func {spec["test"]}(t *testing.T) {{\n}}\n')
            refusal = {"status": "policy-refusal",
                       "selection": {"unresolved": [spec["package"] + ":TestModern"], "tasks": []}}
            green = self.miss_worker(runner, task, self.miss_events(spec, green=True), rc=0)
            with patch.object(runner, "project", return_value=[task]) as project, \
                    patch.object(runner, "select_unit", return_value={"status": "selected", "matches": [task]}), \
                    patch.object(runner, "worker", return_value=green):
                projected = self.a.diagnose_miss(runner, {"sha": sha}, {}, refusal)
            self.assertEqual(projected["status"], "complete-no-red", projected)
            self.assertEqual(projected["task_origin"], "target-projection-from-refused-inspect")
            self.assertTrue(projected["candidate"]["projected_unit"])
            self.assertFalse(projected["full_plan_executable"])
            project.assert_called_once()
            unreached = self.miss_worker(runner, task, self.miss_events(spec, green=True), rc=0, names=["TestPeer"])
            with patch.object(runner, "select_unit", return_value={"status": "selected", "matches": [task]}), \
                    patch.object(runner, "worker", return_value=unreached):
                result = self.a.diagnose_miss(runner, {"sha": sha}, {}, {"status": "selected",
                                                                        "selection": {"unresolved": [], "tasks": [task]}})
            self.assertEqual(result["status"], "selected-but-unreached")
            self.assertTrue(result["candidate"]["selected_but_unreached"])
            self.assertFalse(result["candidate"]["caught_original_defect"])
            with patch.object(runner, "select_unit", return_value={"status": "wrong-mode", "matches": [], "reasons": []}), \
                    patch.object(runner, "worker") as no_worker:
                result = self.a.diagnose_miss(runner, {"sha": sha}, {}, {"status": "selected",
                                                                        "selection": {"unresolved": [], "tasks": [task]}})
            self.assertEqual(result["status"], "wrong-mode")
            self.assertFalse(result["candidate"]["caught_original_defect"])
            no_worker.assert_not_called()

    def test_miss_finish_distinguishes_observations_from_infrastructure(self):
        def row(status):
            return {**CASE, "source_state": "exact", "selector": {"status": "selected"},
                    "diagnostic": {"status": status}}
        for status in ("not-selected", "selected-but-unreached", "original-assertion-unconfirmed",
                       "structural-refusal", "red-reproduced", "complete-no-red"):
            with self.subTest(status=status):
                index = {"errors": [], "request": {"phase": "misses"}, "rows": [row(status)]}
                self.a.finish(index)
                self.assertEqual(index["status"], "complete")
        for status in ("unsupported-worker-shape", "wrong-mode", "worker-error", "timeout-infrastructure", "unknown"):
            with self.subTest(status=status):
                index = {"errors": [], "request": {"phase": "misses"}, "rows": [row(status)]}
                self.a.finish(index)
                self.assertEqual(index["status"], "infrastructure-error")
        default = {"errors": [], "request": {"phase": "both"}, "rows": [row("structural-refusal")]}
        self.a.finish(default)
        self.assertEqual(default["status"], "infrastructure-error")
    def test_miss_phase_runs_only_the_selected_original_and_marks_not_selected_complete(self):
        sha = "5aa8203bda038d3ce08ef51bfef1e20ba8c5dca1"
        with tempfile.TemporaryDirectory() as directory:
            runner = self.runner(Path(directory))
            scripts = runner.root / "controller/.github/scripts"
            scripts.mkdir(parents=True)
            (scripts / "ci-historical-cases.json").write_bytes(SCRIPT.with_name("ci-historical-cases.json").read_bytes())
            event = runner.root / "event.json"
            event.write_text(json.dumps({"inputs": {"policy_sha": "a" * 40, "phase": "misses",
                                                     "focus_sha": sha, "offset": "112", "count": "1"}}))
            def git(root, *args):
                return {("rev-parse", "HEAD"): ("b" * 40 + "\n").encode(), ("--version",): b"git fixture"}.get(args, b"")
            with patch.object(self.a, "HERE", scripts), patch.object(self.a, "Runner", return_value=runner), \
                    patch.object(self.a, "context", return_value={"sha": "b" * 40}), \
                    patch.object(self.a, "public_run", return_value={}), \
                    patch.object(self.a.platform, "platform", return_value="fixture"), \
                    patch.object(self.a.subprocess, "run") as version, \
                    patch.dict(os.environ, {"GITHUB_EVENT_PATH": str(event)}), \
                    patch.object(runner, "identity", return_value={"sha": "b" * 40}), \
                    patch.object(runner, "git", side_effect=git), \
                    patch.object(runner, "prepare_policy", return_value={"sha": "a" * 40}), \
                    patch.object(runner, "prefetch", return_value=[]) as fetch, \
                    patch.object(runner, "checkout_source", return_value={"sha": sha}) as checkout, \
                    patch.object(runner, "inspect", return_value={"status": "selected"}) as inspect, \
                    patch.object(self.a, "diagnose_miss", return_value={"status": "not-selected"}) as miss, \
                    patch.object(self.a, "diagnose", return_value={"status": "unknown"}) as diagnostic:
                version.return_value.stdout = b"go version go1.26.0 linux/amd64"
                result = self.a.run_audit(runner.root)
            self.assertEqual(fetch.call_args.args[0], [sha], result["errors"])
            self.assertEqual(checkout.call_count, 1)
            self.assertEqual(inspect.call_count, 1)
            self.assertEqual(miss.call_count, 1)
            diagnostic.assert_not_called()
            self.assertEqual([row["sha"] for row in result["rows"]], [sha])
            self.assertEqual(result["request"]["phase"], "misses")
            self.assertIn("三例", result["scope"])
            self.assertEqual(result["rows"][0]["diagnostic"], {"status": "not-selected"})
            self.assertEqual(result["status"], "complete")
    def partial_events(self, package, target, peer, output=None, subtest=None, target_fail=True,
                       peer_fail=False, target_terminal=True):
        rows = [{"Action": "start", "Package": package},
                {"Action": "run", "Package": package, "Test": target}]
        if subtest:
            rows.append({"Action": "run", "Package": package, "Test": subtest})
        if output is not None:
            rows.append({"Action": "output", "Package": package, "Test": subtest or target, "Output": output})
        if target_terminal:
            if subtest:
                rows.append({"Action": "fail" if target_fail else "pass", "Package": package, "Test": subtest})
            rows.append({"Action": "fail" if target_fail else "pass", "Package": package, "Test": target})
        rows.append({"Action": "run", "Package": package, "Test": peer})
        rows.append({"Action": "fail" if peer_fail else "pass", "Package": package, "Test": peer})
        rows.append({"Action": "fail" if target_fail or peer_fail else "pass", "Package": package})
        return "".join(json.dumps(row) + "\n" for row in rows)

    def partial_worker(self, runner, task, package, pattern, text, selected):
        raw = runner.output / ("partial-" + task["unit"].replace("/", "-") + ".json")
        raw.write_text(text)
        (runner.output / "partial.stderr").write_text("")
        return {"directory": ".", "tracked_unchanged": True,
                "command": {"rc": 1, "timeout": False, "stdout": raw.name, "stderr": "partial.stderr"},
                "receipt": {"task": task, "api_invoked": True, "returned": False,
                            "exception": "CalledProcessError", "error": "go test failed",
                            "commands": [{"argv": ["go", "test", "-count=1", "-race", "-json", "-run",
                                                    pattern, package], "rc": 1, "stdout": raw.name}],
                            "partition": {"selected": dict.fromkeys(selected, 1)}}}

    def test_partial_effect_target_red_survives_unexecuted_slots_but_truncated_target_does_not(self):
        sha = "7d70dc49ff1f3c3ae3876b2a519059c60d88ec3d"
        inspection, tasks = self.full_store_selection()
        task = next(row for row in tasks if row["worker"] == 5)
        with tempfile.TemporaryDirectory() as directory:
            runner = self.runner(Path(directory))
            source = runner.history / "internal/store/task_work_input_effect_test.go"
            source.parent.mkdir(parents=True)
            source.write_text(f'func {self.a.EFFECT}(t *testing.T) {{\n' + "\n" * 73 +
                              'conflict(f.s.HandBackWorkInputWithKey(ctx))\nt.Fatal("expected conflict without a receipt")\n}\n')
            selected = [self.a.EFFECT, "TestPeer", "TestLater"]
            pattern = "^(" + self.a.EFFECT + "|TestPeer)$"
            def diagnose(events):
                worker = self.partial_worker(runner, task, PACKAGE, pattern, events, selected)
                with patch.object(runner, "project", return_value=tasks), \
                        patch.object(runner, "worker", return_value=worker), \
                        patch.object(runner, "record", side_effect=AssertionError("no isolated probe")):
                    return self.a.diagnose(runner, {"sha": sha}, {}, inspection)
            output = "task_work_input_effect_test.go:75: expected conflict without a receipt, got unexpected\n"
            red = diagnose(self.partial_events(PACKAGE, self.a.EFFECT, "TestPeer", output))
            self.assertEqual(red["status"], "red-reproduced", red)
            self.assertTrue(red["candidate"]["caught_original_defect"])
            self.assertTrue(red["candidate"]["partial_coverage"])
            self.assertEqual(red["candidate"]["target_command"]["argv"][-1], PACKAGE)
            self.assertNotIn("isolated_original", red)
            truncated = diagnose(self.partial_events(PACKAGE, self.a.EFFECT, "TestPeer", output,
                                                     target_terminal=False))
            self.assertNotEqual(truncated["status"], "red-reproduced", truncated)
            self.assertFalse(truncated["candidate"]["caught_original_defect"])
            self.assertTrue(truncated["candidate"]["partial_coverage"])
            peer = diagnose(self.partial_events(PACKAGE, self.a.EFFECT, "TestPeer", output,
                                                target_fail=False, peer_fail=True))
            self.assertNotEqual(peer["status"], "red-reproduced", peer)
            self.assertFalse(peer["candidate"]["caught_original_defect"])
            first_raw = runner.output / "partial-first.json"
            first_raw.write_text(self.partial_events(PACKAGE, self.a.EFFECT, "TestPeer", output, target_fail=False))
            second_raw = runner.output / "partial-second.json"
            second_raw.write_text(self.partial_events(PACKAGE, "TestOther", "TestPeer",
                                                      "other_test.go:1: unrelated failure\n"))
            other_worker = {"directory": ".", "tracked_unchanged": True,
                            "command": {"rc": 1, "timeout": False, "stdout": first_raw.name, "stderr": "partial.stderr"},
                            "receipt": {"task": task, "api_invoked": True, "returned": False,
                                        "exception": "CalledProcessError", "error": "go test failed",
                                        "commands": [
                                            {"argv": ["go", "test", "-count=1", "-race", "-json", "-run",
                                                       pattern, PACKAGE], "rc": 0, "stdout": first_raw.name},
                                            {"argv": ["go", "test", "-count=1", "-race", "-json", "-run",
                                                       "^(TestOther|TestPeer)$", PACKAGE], "rc": 1, "stdout": second_raw.name}],
                                        "partition": {"selected": dict.fromkeys(
                                            [self.a.EFFECT, "TestPeer", "TestOther", "TestLater"], 1)}}}
            with patch.object(runner, "project", return_value=tasks), \
                    patch.object(runner, "worker", return_value=other_worker), \
                    patch.object(runner, "record", side_effect=AssertionError("no isolated probe")):
                other = self.a.diagnose(runner, {"sha": sha}, {}, inspection)
            self.assertNotEqual(other["status"], "red-reproduced", other)
            self.assertFalse(other["candidate"]["caught_original_defect"])

    def test_partial_miss_target_red_survives_unexecuted_slots_but_truncated_target_does_not(self):
        sha = "4b87e75cfcaeaac19d7e1e8e1db076bf673651ba"
        spec = self.a.MISSES[sha]
        task = {"unit": "race-coordination-0", "kind": "go", "package": spec["package"], "race": True,
                "profile": "coordination", "worker": 0, "bucket": None, "buckets": None,
                "names": None, "required": []}
        with tempfile.TemporaryDirectory() as directory:
            runner = self.runner(Path(directory))
            source = runner.history / spec["file"]
            source.parent.mkdir(parents=True)
            source.write_text(f'func {spec["test"]}(t *testing.T) {{\n}}\n')
            inspection = {"status": "selected", "selection": {"unresolved": [], "tasks": [task]}}
            selected = [spec["test"], "TestPeer", "TestLater"]
            pattern = "^(" + spec["test"] + "|TestPeer)$"
            def diagnose(events):
                worker = self.partial_worker(runner, task, spec["package"], pattern, events, selected)
                with patch.object(runner, "select_unit", return_value={"status": "selected", "matches": [task]}), \
                        patch.object(runner, "worker", return_value=worker), \
                        patch.object(runner, "record", side_effect=AssertionError("no fallback")):
                    return self.a.diagnose_miss(runner, {"sha": sha}, {}, inspection)
            output = "intent_pump_test.go:789: context deadline exceeded\n"
            red = diagnose(self.partial_events(spec["package"], spec["test"], "TestPeer", output,
                                               subtest=spec["case"]))
            self.assertEqual(red["status"], "red-reproduced", red)
            self.assertTrue(red["candidate"]["caught_original_defect"])
            self.assertTrue(red["candidate"]["partial_coverage"])
            self.assertEqual(red["candidate"]["target_command"]["argv"][-1], spec["package"])
            self.assertEqual(red["task_origin"], "emitted-task")
            self.assertNotIn("isolated_original", red)
            truncated = diagnose(self.partial_events(spec["package"], spec["test"], "TestPeer", output,
                                                     subtest=spec["case"], target_terminal=False))
            self.assertNotEqual(truncated["status"], "red-reproduced", truncated)
            self.assertFalse(truncated["candidate"]["caught_original_defect"])
            self.assertTrue(truncated["candidate"]["partial_coverage"])


if __name__ == "__main__":
    unittest.main()
