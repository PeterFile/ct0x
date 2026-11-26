#!/usr/bin/env python3
"""Offline periodic Full policy, authenticated-evidence and real replay regressions."""
import copy
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import tempfile
import unittest
from unittest import mock
import zipfile
from types import SimpleNamespace
from contextlib import redirect_stdout


SCRIPTS = Path(__file__).resolve().parent
WORKFLOWS = SCRIPTS.parent / "workflows"
REPO = "owner/workspace"
SHA, TREE, BASE = (letter * 40 for letter in "abc")
RUN, ATTEMPT = 1234, 2


def load(name):
    path = SCRIPTS / (name + ".py")
    if not path.exists():
        return None
    spec = importlib.util.spec_from_file_location(name.replace("-", "_"), path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


POLICY, REPLAY = load("periodic-full"), load("periodic-replay")


def identity():
    return {"sha": SHA, "tree": TREE, "head": SHA, "base": BASE,
            "run_id": RUN, "run_attempt": ATTEMPT, "repo": REPO}


def full_needs():
    outputs = {key: str(value) for key, value in identity().items() if key != "repo"}
    return {"resolve": {"result": "success"},
            "preflight": {"result": "success", "outputs": {"sha": SHA, "tree": TREE}},
            "full": {"result": "success", "outputs": {**outputs, "verified": "true"}}}


def periodic_receipt():
    return {"schema": 1, "kind": "periodic-full", **identity(), "status": "success", "verified": True,
            "checkout": {"sha": SHA, "tree": TREE},
            "components": {key: "success" for key in ("resolve", "preflight", "full")},
            "full": {**identity(), "verified": True},
            "attribution": {"status": "unknown", "good_sha": None, "bad_sha": SHA,
                            "commit": None, "reason": "完整套件成功"}}


class FakeAPI:
    def __init__(self, kind="legacy", event=None, trigger_sha=SHA):
        self.calls = []
        self.repo = {"id": 7, "full_name": REPO, "fork": False, "default_branch": "master"}
        path = {"legacy": "ci.yml", "periodic": "periodic-full.yml", "release": "release-candidate.yml"}[kind]
        self.run = {"id": RUN, "run_attempt": ATTEMPT, "workflow_id": 9,
                    "path": ".github/workflows/" + path, "event": event or ("push" if kind == "legacy" else "schedule"),
                    "head_branch": "master", "head_sha": trigger_sha, "repository": self.repo,
                    "head_repository": self.repo, "status": "completed", "conclusion": "success",
                    "run_started_at": "2026-11-26T00:00:00Z", "updated_at": "2026-11-26T01:00:00Z"}
        self.rows = {f"repos/{REPO}": self.repo,
                     f"repos/{REPO}/actions/workflows/9": {"id": 9, "path": self.run["path"]},
                     f"repos/{REPO}/actions/runs/{RUN}/attempts/{ATTEMPT}": self.run,
                     f"repos/{REPO}/commits/{SHA}": {"sha": SHA, "commit": {"tree": {"sha": TREE}}, "parents": [{"sha": BASE}]}}
        producer = {"legacy": "Full CI", "periodic": "Periodic Full evidence", "release": "Release Full evidence"}[kind]
        self.jobs = [self.job(producer, 100), self.job("Full suite / Full suite evidence", 101)]
        if kind == "legacy":
            self.jobs.append(self.job("CI preflight and feedback", 102))
        elif kind == "periodic":
            self.jobs.append(self.job("Periodic Full preflight", 102))
        self.rows[f"repos/{REPO}/actions/runs/{RUN}/attempts/{ATTEMPT}/jobs?per_page=100&page=1"] = {
            "total_count": len(self.jobs), "jobs": self.jobs}
        if kind == "legacy":
            receipt = {"base": BASE, "head": SHA, "run_id": str(RUN), "sha": SHA, "tree": TREE, "verified": True}
        elif kind == "periodic":
            receipt = periodic_receipt()
        else:
            receipt = {"schema": 1, "kind": "release-full", **identity(), "status": "success", "verified": True,
                       "artifact_sha256": "d" * 64}
        self.receipt = receipt
        self.set_log(receipt)
        if kind != "legacy":
            self.set_artifact(receipt, "periodic-full" if kind == "periodic" else "release-full")

    def job(self, name, job_id, state="success"):
        return {"id": job_id, "name": name, "run_id": RUN, "run_attempt": ATTEMPT, "head_sha": self.run["head_sha"],
                "status": "completed", "conclusion": state,
                "html_url": f"https://github.com/{REPO}/actions/runs/{RUN}/job/{job_id}",
                "started_at": "2026-11-26T00:00:00Z", "steps": []}

    def set_log(self, receipt):
        self.rows[f"repos/{REPO}/actions/jobs/100/logs"] = (
            "2026-11-26T11:22:11.4302912Z " + json.dumps(receipt, sort_keys=True) + "\n").encode()

    def set_artifact(self, receipt, prefix):
        data = io.BytesIO()
        with zipfile.ZipFile(data, "w") as archive:
            archive.writestr("receipt.json", json.dumps(receipt))
        self.zip = data.getvalue()
        self.artifact = {"id": 77, "name": f"{prefix}-{RUN}-{ATTEMPT}", "expired": False,
                         "digest": "sha256:" + hashlib.sha256(self.zip).hexdigest(),
                         "created_at": "2026-11-26T00:30:00Z",
                         "workflow_run": {"id": RUN, "repository_id": 7, "head_repository_id": 7,
                                          "head_sha": self.run["head_sha"], "head_branch": self.run["head_branch"]}}
        self.rows[f"repos/{REPO}/actions/runs/{RUN}/artifacts?per_page=100&page=1"] = {
            "total_count": 1, "artifacts": [self.artifact]}
        self.rows[f"repos/{REPO}/actions/artifacts/77/zip"] = self.zip

    def get(self, endpoint, raw=False):
        self.calls.append(endpoint)
        if endpoint not in self.rows:
            raise ValueError("离线夹具没有此证据：" + endpoint)
        return self.rows[endpoint]


class EvidenceTests(unittest.TestCase):
    def setUp(self):
        self.assertIsNotNone(POLICY, "periodic-full.py 尚未实现")

    def evidence(self, api):
        return POLICY.Evidence(api, REPO, lambda sha: sha in {SHA, BASE, "e" * 40})

    def test_real_timestamped_legacy_outer_line_and_attempt_specific_jobs(self):
        api = FakeAPI()
        proof = self.evidence(api).verify(RUN, ATTEMPT)
        self.assertEqual((proof["sha"], proof["tree"], proof["run_id"], proof["run_attempt"]), (SHA, TREE, RUN, ATTEMPT))
        self.assertIn(f"repos/{REPO}/actions/runs/{RUN}/attempts/{ATTEMPT}/jobs?per_page=100&page=1", api.calls)
        self.assertEqual(proof["producer_job_id"], 100)

    def test_api_workflow_path_ref_suffix_must_match_authenticated_source(self):
        api = FakeAPI()
        api.run["path"] += "@refs/heads/master"
        self.assertEqual(self.evidence(api).verify(RUN, ATTEMPT)["sha"], SHA)
        api.run["path"] = ".github/workflows/ci.yml@refs/heads/feature"
        with self.assertRaises(ValueError):
            self.evidence(api).verify(RUN, ATTEMPT)

    def test_green_workflow_is_not_full_proof_and_all_identity_fields_are_typed(self):
        cases = [lambda a: a.jobs[0].update(conclusion="skipped"),
                 lambda a: a.jobs[1].update(conclusion="failure"),
                 lambda a: a.jobs[2].update(conclusion="failure"),
                 lambda a: a.jobs[0].update(run_attempt=1),
                 lambda a: a.jobs[0].update(run_id=True),
                 lambda a: a.jobs[0].update(head_sha=BASE),
                 lambda a: a.run.update(event="pull_request"),
                 lambda a: a.run.update(head_branch="feature"),
                 lambda a: a.run.update(path=".github/workflows/unrelated.yml"),
                 lambda a: a.run.update(head_repository={**a.repo, "fork": True}),
                 lambda a: a.run.update(repository={**a.repo, "full_name": "attacker/repo"}),
                 lambda a: a.run.update(id=True),
                 lambda a: a.run.update(run_attempt=2.0),
                 lambda a: a.rows.pop(f"repos/{REPO}/actions/jobs/100/logs")]
        for change in cases:
            api = FakeAPI()
            change(api)
            with self.subTest(change=change), self.assertRaises(ValueError):
                self.evidence(api).verify(RUN, ATTEMPT)
        for field, value in [("run_id", RUN), ("verified", "true"), ("sha", BASE), ("tree", SHA),
                             ("head", BASE), ("base", SHA)]:
            api = FakeAPI()
            api.set_log({**api.receipt, field: value})
            with self.subTest(field=field), self.assertRaises(ValueError):
                self.evidence(api).verify(RUN, ATTEMPT)

    def test_same_sha_periodic_receipt_accepts_moved_schedule_target_not_trigger_sha(self):
        api = FakeAPI("periodic", trigger_sha=BASE)
        proof = self.evidence(api).verify(RUN, ATTEMPT)
        self.assertEqual(proof["sha"], SHA)
        for change in [lambda a: a.artifact.update(expired=True),
                       lambda a: a.artifact["workflow_run"].update(head_sha=SHA),
                       lambda a: a.artifact.update(name=f"periodic-full-{RUN}-1"),
                       lambda a: a.artifact.update(digest="sha256:" + "0" * 64),
                       lambda a: a.run.update(event="workflow_dispatch", head_branch="feature")]:
            api = FakeAPI("periodic", trigger_sha=BASE)
            change(api)
            with self.subTest(change=change), self.assertRaises(ValueError):
                self.evidence(api).verify(RUN, ATTEMPT)
        for field, value in [("verified", 1), ("run_id", str(RUN)), ("run_attempt", True), ("schema", True)]:
            api = FakeAPI("periodic")
            receipt = {**api.receipt, field: value}
            api.set_log(receipt)
            api.set_artifact(receipt, "periodic-full")
            with self.subTest(field=field), self.assertRaises(ValueError):
                self.evidence(api).verify(RUN, ATTEMPT)

    def test_release_full_survives_later_hardware_failure_but_requires_trusted_producer(self):
        api = FakeAPI("release", event="workflow_dispatch", trigger_sha=BASE)
        api.run["conclusion"] = "failure"
        api.rows.pop(f"repos/{REPO}/actions/jobs/100/logs")
        self.assertEqual(self.evidence(api).verify(RUN, ATTEMPT)["sha"], SHA)
        api.jobs[0]["name"] = "Release candidate green"
        with self.assertRaises(ValueError):
            self.evidence(api).verify(RUN, ATTEMPT)
        api = FakeAPI("release", event="push")
        api.run["head_branch"] = "rc/v1"
        # A tag/receipt assertion without API-bound master workflow provenance is unknown.
        with self.assertRaises(ValueError):
            self.evidence(api).verify(RUN, ATTEMPT)

    def test_rc_requires_api_pinned_master_workflow_and_exact_tag_commit(self):
        api = FakeAPI("release", event="push", trigger_sha=BASE)
        api.run["head_branch"] = "rc/v1"
        expected = REPO + "/.github/workflows/release-candidate.yml@refs/heads/master"
        api.run["referenced_workflows"] = [{"path": expected, "sha": BASE}]
        receipt = {**api.receipt, "workflow_ref": expected, "workflow_sha": BASE}
        api.set_artifact(receipt, "release-full")
        api.rows[f"repos/{REPO}/git/ref/tags/rc/v1"] = {"ref": "refs/tags/rc/v1", "object": {"type": "commit", "sha": BASE}}
        self.assertEqual(self.evidence(api).verify(RUN, ATTEMPT)["sha"], SHA)
        api.rows[f"repos/{REPO}/git/ref/tags/rc/v1"]["object"]["sha"] = SHA
        with self.assertRaises(ValueError):
            self.evidence(api).verify(RUN, ATTEMPT)

    def test_api_is_readonly_bounded_cached_and_never_saves_credentials(self):
        with tempfile.TemporaryDirectory() as temporary, mock.patch.dict(os.environ, {"GH_TOKEN": "secret-token", "PRIVATE_SECRET": "hidden"}):
            calls = []
            def fetch(argv, **kwargs):
                calls.append((argv, kwargs["env"]))
                if len(calls) == 1:
                    return subprocess.CompletedProcess(argv, 1, stderr=b"HTTP 503")
                kwargs["stdout"].write(b'{"value":1}')
                return subprocess.CompletedProcess(argv, 0, stderr=b"")
            api = POLICY.API(Path(temporary))
            with mock.patch.object(POLICY.subprocess, "run", side_effect=fetch), mock.patch.object(POLICY.time, "sleep"):
                self.assertEqual(api.get("repos/owner/workspace"), {"value": 1})
                self.assertEqual(api.get("repos/owner/workspace", raw=True), b'{"value":1}')
            self.assertEqual(len(calls), 2)
            for argv, env in calls:
                self.assertEqual(argv[:4], ["gh", "api", "--method", "GET"])
                self.assertNotIn("PRIVATE_SECRET", env)
            self.assertNotIn("secret-token", "".join(path.read_text() for path in Path(temporary).iterdir()))
            with mock.patch.object(POLICY, "MAX_API", 1), self.assertRaises(ValueError):
                api.get("repos/owner/other")

    def test_receipt_artifact_must_equal_successful_producer_log(self):
        api = FakeAPI("periodic")
        api.set_artifact({**api.receipt, "tree": BASE}, "periodic-full")
        with self.assertRaises(ValueError):
            self.evidence(api).verify(RUN, ATTEMPT)
        api = FakeAPI()
        api.rows[f"repos/{REPO}/actions/jobs/100/logs"] += b'{"verified":true,"verified":false}\n'
        with self.assertRaises(ValueError):
            self.evidence(api).verify(RUN, ATTEMPT)

    def test_typed_artifact_values_and_malformed_lookup_fail_conservatively(self):
        api = FakeAPI("periodic")
        api.set_artifact({**api.receipt, "verified": 1}, "periodic-full")
        with self.assertRaises(ValueError):
            self.evidence(api).verify(RUN, ATTEMPT)
        api.rows[f"repos/{REPO}/actions/workflows/ci.yml/runs?per_page=100&page=1"] = {"total_count": 1, "workflow_runs": [None]}
        found, reasons = self.evidence(api).search([SHA])
        self.assertEqual(found, {})
        self.assertTrue(reasons)

    def test_failed_job_collection_retains_raw_job_identity_and_blocks_infra_attribution(self):
        api = FakeAPI("periodic")
        failed = api.job("Full suite / Go workspace race", 501, "failure")
        failed["steps"] = [{"number": 3, "name": "Race test workspace package", "conclusion": "failure",
                            "started_at": "2026-11-26T00:01:00Z"}]
        api.jobs.append(failed)
        api.rows[f"repos/{REPO}/actions/runs/{RUN}/attempts/{ATTEMPT}/jobs?per_page=100&page=1"]["total_count"] = len(api.jobs)
        api.rows[f"repos/{REPO}/actions/jobs/501/logs"] = b'--- FAIL: TestRegression (0.01s)\n    main_test.go:8: deterministic regression\nFAIL\tagentworkspace/cmd/workspace\t0.01s\n'
        result = {}
        POLICY.collect_failures(self.evidence(api), api.run, result)
        self.assertEqual(result["first_blocking_event"]["id"], 501)
        self.assertEqual(result["failure_candidates"][0]["sources"][0]["id"], 501)
        self.assertEqual(result["failure_candidates"][0]["signature"]["messages"], ["deterministic regression"])
        failed["name"] = "Full suite / Unrelated diagnostic"
        result = {}
        POLICY.collect_failures(self.evidence(api), api.run, result)
        self.assertEqual(result["failure_candidates"], [])
        self.assertEqual(result["diagnostic_status"], "unknown")

    def test_lookup_does_not_filter_on_head_sha_and_absence_runs_conservatively(self):
        api = FakeAPI("periodic", trigger_sha=BASE)
        for workflow in ("ci.yml", "periodic-full.yml", "release-candidate.yml"):
            api.rows[f"repos/{REPO}/actions/workflows/{workflow}/runs?per_page=100&page=1"] = {
                "total_count": 1 if workflow == "periodic-full.yml" else 0,
                "workflow_runs": [api.run] if workflow == "periodic-full.yml" else []}
        found, reasons = self.evidence(api).search([SHA, BASE])
        self.assertIn(SHA, found)
        self.assertFalse(any("head_sha=" in call for call in api.calls))
        api.rows.pop(f"repos/{REPO}/actions/jobs/100/logs")
        found, reasons = self.evidence(api).search([SHA])
        self.assertEqual(found, {})
        self.assertTrue(reasons)

    def test_admission_resolves_latest_master_even_for_manual_branch_without_trusting_branch_proof(self):
        api = FakeAPI("periodic", event="workflow_dispatch", trigger_sha=BASE)
        api.run["head_branch"] = "test-periodic"
        for value in (SHA, BASE):
            api.rows[f"repos/{REPO}/contents/.github/workflows/ci-full.yml?ref={value}"] = {"sha": "f" * 40}
        def git(root, *args):
            return {("rev-parse", "HEAD"): SHA, ("rev-parse", "HEAD^{tree}"): TREE,
                    ("rev-list", "--first-parent", "--max-count=33", SHA): SHA + "\n" + BASE}[args]
        control = SimpleNamespace(git=git)
        plan = SimpleNamespace(shard_matrix=lambda: {"include": []})
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            ref = root / "ref.json"
            ref.write_text(json.dumps({"ref": "refs/heads/master", "object": {"type": "commit", "sha": SHA}}))
            args = SimpleNamespace(root=root, ref=ref, output=root)
            env = {"GITHUB_SHA": BASE, "GITHUB_RUN_ID": str(RUN), "GITHUB_RUN_ATTEMPT": str(ATTEMPT),
                   "GITHUB_OUTPUT": str(root / "outputs")}
            with mock.patch.dict(os.environ, env), mock.patch.object(POLICY, "load", side_effect=lambda name, root=None: control if name == "periodic-replay" else plan), redirect_stdout(io.StringIO()):
                POLICY.admit(args, api, self.evidence(api))
                recorded = json.loads((root / "plan.json").read_text())
                self.assertEqual(recorded["identity"]["sha"], SHA)
                self.assertEqual(recorded["decision"], "run")
                evidence = self.evidence(api)
                proof = {**identity(), "verified": True}
                with mock.patch.object(evidence, "search", return_value=({SHA: proof}, [])):
                    for force, decision in (("false", "skip"), ("true", "run")):
                        with mock.patch.dict(os.environ, {"PERIODIC_FORCE": force}):
                            POLICY.admit(args, api, evidence)
                            self.assertEqual(json.loads((root / "plan.json").read_text())["decision"], decision)
                api.repo["fork"] = True
                with self.assertRaises(ValueError):
                    POLICY.admit(args, api, self.evidence(api))
        with self.assertRaises(ValueError):
            self.evidence(FakeAPI("periodic", event="workflow_dispatch")).source(api.run, periodic_receipt())

    def test_success_receipt_requires_exact_needs_and_full_aggregate_identity(self):
        receipt = POLICY.receipt(identity(), full_needs(), "run", None)
        self.assertTrue(receipt["verified"])
        self.assertEqual(receipt["status"], "success")
        skipped = full_needs()
        for key in ("preflight", "full"):
            skipped[key] = {"result": "skipped"}
        result = POLICY.receipt(identity(), skipped, "skip", {**identity(), "verified": True})
        self.assertEqual((result["status"], result["verified"]), ("skipped", False))
        for key in full_needs():
            needs = full_needs()
            needs[key]["result"] = "failure"
            result = POLICY.receipt(identity(), needs, "run", None)
            self.assertFalse(result["verified"])
            self.assertEqual(result["status"], "failure")
        for field in ("sha", "tree", "head", "base", "run_id"):
            needs = full_needs()
            needs["full"]["outputs"][field] = "wrong"
            with self.subTest(field=field), self.assertRaises(ValueError):
                POLICY.receipt(identity(), needs, "run", None)
        with self.assertRaises(ValueError):
            POLICY.receipt(identity(), {}, "skip", None)

    def test_exact_authenticated_merged_pr_only_no_subject_guessing(self):
        local = {"localization": "reproduced", "status": "unknown", "commit": SHA,
                 "good_sha": BASE, "bad_sha": SHA, "transition": "regression"}
        pr = {"number": 17, "merged_at": "2026-11-26T01:00:00Z", "merge_commit_sha": SHA,
              "base": {"ref": "master", "repo": {"full_name": REPO}}}
        result = POLICY.attribute(local, [pr], REPO)
        self.assertEqual((result["status"], result["pr"]), ("confirmed", 17))
        for rows in ([], [pr, {**pr, "number": 18}], [{**pr, "merged_at": None}],
                     [{**pr, "merge_commit_sha": BASE}], [{**pr, "number": True}], [{**pr, "merged_at": ""}],
                     [{**pr, "base": {"ref": "feature", "repo": {"full_name": REPO}}}]):
            result = POLICY.attribute(local, rows, REPO)
            self.assertEqual(result["status"], "unknown")
            self.assertNotIn("pr", result)


class ReplayTests(unittest.TestCase):
    def setUp(self):
        self.assertIsNotNone(REPLAY, "periodic-replay.py 尚未实现")

    def candidate(self):
        return {"package": "agentworkspace/cmd/workspace", "test": "TestRegression", "race": False,
                "signature": {"tests": ["TestRegression"], "messages": ["deterministic regression"]}}

    def test_empirical_plain_and_json_go_failures_bind_to_known_package(self):
        root = "TestRunExplanationParentCancelAfterSendKeepsExactJournal"
        log = (f"2026-11-24T08:37:11.5459693Z --- FAIL: {root} (3.09s)\n"
               "2026-11-24T08:37:11.5459693Z FAIL\tagentworkspace/cmd/workspace\t20.703s\n")
        parsed = REPLAY.failures(log)
        self.assertEqual([(row["package"], row["test"]) for row in parsed], [("agentworkspace/cmd/workspace", root)])
        root = "TestIntentPassCancellationAndQueryFailure"
        log = (f"--- FAIL: {root} (1.0s)\n    --- FAIL: {root}/cancel (0.5s)\n"
               "FAIL\tagentworkspace/internal/coordination\t345.465s\n")
        self.assertEqual(REPLAY.failures(log)[0]["signature"]["tests"], [root, root + "/cancel"])
        rows = [{"Action": "run", "Package": "agentworkspace/cmd/workspace", "Test": "TestRegression"},
                {"Action": "output", "Package": "agentworkspace/cmd/workspace", "Test": "TestRegression",
                 "Output": "    main_test.go:8: deterministic regression\n"},
                {"Action": "fail", "Package": "agentworkspace/cmd/workspace", "Test": "TestRegression"},
                {"Action": "fail", "Package": "agentworkspace/cmd/workspace"}]
        self.assertEqual(REPLAY.failures("\n".join(map(json.dumps, rows)))[0]["signature"], self.candidate()["signature"])
        self.assertEqual(REPLAY.failures(log.replace("agentworkspace/", "foreign/")), [])
        self.assertEqual(REPLAY.failures("panic: test timed out after 10m\n" + log), [])
        self.assertEqual(REPLAY.failures("unrelated mentions TestRegression\nFAIL\n"), [])

    def test_probe_log_validation_requires_complete_exact_named_lifecycle(self):
        candidate = self.candidate()
        package, test = candidate["package"], candidate["test"]
        def row(action, name=None, **fields):
            return {"Action": action, "Package": package, **({"Test": name} if name else {}), **fields}
        def text(rows):
            return "".join(json.dumps(value) + "\n" for value in rows)
        listing = text([row("start"), row("output", Output=test + "\n"), row("pass")])
        good = [row("start"), row("run", test), row("pass", test), row("pass")]
        self.assertEqual(REPLAY.assess(listing, 0, text(good), 0, candidate)["state"], "pass")
        absent = text([row("start"), row("output", Output="ok\t" + package + "\n"), row("pass")])
        self.assertEqual(REPLAY.assess(absent, 0, "", None, candidate)["state"], "absent")
        for rows in (good[:-1], good[1:], [row("start"), row("pass")],
                     [row("start"), row("run", test), row("skip", test), row("pass")],
                     [row("start"), row("run", test), row("run", test), row("pass", test), row("pass")],
                     [row("start"), row("run", test + "Extra"), row("pass", test + "Extra"), row("pass")]):
            self.assertEqual(REPLAY.assess(listing, 0, text(rows), 0, candidate)["state"], "unknown", rows)
        self.assertEqual(REPLAY.assess("toolchain unavailable", 1, "", None, candidate)["state"], "unknown")
        timed_out = [row("start"), row("run", test), row("output", test, Output="main_test.go:1: context deadline exceeded\n"),
                     row("fail", test), row("fail")]
        self.assertEqual(REPLAY.assess(listing, 0, text(timed_out), 1, candidate)["state"], "unknown")

    def test_screen_entire_first_parent_chain_and_repeat_pair_not_binary_search(self):
        chain = [BASE, "d" * 40, SHA]
        calls = []
        def probe(sha, candidate):
            calls.append(sha)
            return {"state": "pass" if sha == BASE else "fail", "signature": candidate["signature"]}
        result = REPLAY.localize(chain, self.candidate(), probe)
        self.assertEqual(result["localization"], "reproduced")
        self.assertEqual((result["good_sha"], result["commit"]), (BASE, "d" * 40))
        self.assertTrue(all(calls.count(sha) >= 3 for sha in chain))
        self.assertNotIn("pr", result)

    def test_intermittent_absent_missing_infra_and_multiple_transitions_are_unknown(self):
        chain = [BASE, "d" * 40, "e" * 40, SHA]
        for states in (["pass", "fail", "pass", "fail"], ["pass", "unknown", "fail", "fail"],
                       ["pass", "absent", "fail", "fail"], ["pass", "pass", "pass", "pass"]):
            def probe(sha, candidate):
                return {"state": states[chain.index(sha)], "signature": candidate["signature"]}
            with self.subTest(states=states):
                self.assertNotEqual(REPLAY.localize(chain, self.candidate(), probe).get("localization"), "reproduced")
        counts = {}
        def intermittent(sha, candidate):
            counts[sha] = counts.get(sha, 0) + 1
            return {"state": "pass" if sha == BASE or counts[sha] % 2 == 0 else "fail", "signature": candidate["signature"]}
        self.assertNotEqual(REPLAY.localize([BASE, SHA], self.candidate(), intermittent).get("localization"), "reproduced")
        def changed_signature(sha, candidate):
            return {"state": "pass" if sha == BASE else "fail", "signature": {"tests": ["TestOther"], "messages": []}}
        self.assertNotEqual(REPLAY.localize([BASE, SHA], self.candidate(), changed_signature).get("localization"), "reproduced")

    def test_exact_first_failing_test_introduction_is_distinct(self):
        def probe(sha, candidate):
            return {"state": "absent" if sha == BASE else "fail", "signature": candidate["signature"],
                    "definition": {"state": "absent" if sha == BASE else "present"}}
        result = REPLAY.localize([BASE, SHA], self.candidate(), probe)
        self.assertEqual(result["transition"], "new-test")
        self.assertEqual(result["localization"], "reproduced")
        def excluded_test(sha, candidate):
            return {**probe(sha, candidate), "definition": {"state": "present"}}
        self.assertNotEqual(REPLAY.localize([BASE, SHA], self.candidate(), excluded_test).get("localization"), "reproduced")

    @unittest.skipUnless(shutil.which("go"), "Go fixture requires a local Go toolchain")
    def test_real_git_history_exact_named_go_test_and_secret_free_child_environment(self):
        with tempfile.TemporaryDirectory(prefix="periodic-fixture-") as directory:
            root = Path(directory) / "repo"
            root.mkdir()
            subprocess.run(["git", "init", "-q", str(root)], check=True)
            def git(*args, data=None):
                return subprocess.check_output(["git", "-C", str(root), *args], input=data).decode().strip()
            def commit(failing, parent=None):
                files = {"go.mod": "module agentworkspace\n\ngo 1.20\n",
                         "cmd/workspace/main_test.go": 'package main\nimport ("testing"; "os")\n'
                         'func TestRegression(t *testing.T) {\n'
                         ' for _, k := range []string{"GH_TOKEN", "GITHUB_TOKEN", "ACTIONS_RUNTIME_TOKEN", "PRIVATE_SECRET"} {'
                         ' if os.Getenv(k) != "" { t.Fatal("credential leaked") } }\n'
                         + (' t.Fatal("deterministic regression")\n' if failing else '') + '}\n'
                         'func TestRegressionExtra(t *testing.T) { t.Fatal("must not run") }\n'}
                # Objects in a disposable repository; no project commits or branch writes.
                def tree(entries):
                    nodes = {}
                    for path, content in entries.items():
                        part, sep, rest = path.partition("/")
                        if sep:
                            nodes.setdefault(part, {})[rest] = content
                        else:
                            nodes[part] = git("hash-object", "-w", "--stdin", data=content.encode())
                    rows = []
                    for name, node in sorted(nodes.items()):
                        mode, kind, oid = ("040000", "tree", tree(node)) if isinstance(node, dict) else ("100644", "blob", node)
                        rows.append(f"{mode} {kind} {oid}\t{name}\0")
                    return git("mktree", "-z", data="".join(rows).encode())
                data = f"tree {tree(files)}\n" + (f"parent {parent}\n" if parent else "")
                data += "author Fixture <ci@example.invalid> 1 +0000\ncommitter Fixture <ci@example.invalid> 1 +0000\n\nfixture\n"
                return git("hash-object", "-w", "-t", "commit", "--stdin", data=data.encode())
            good = commit(False)
            bad = commit(True, good)
            git("update-ref", "refs/heads/master", bad)
            with mock.patch.dict(os.environ, {key: "must-not-leak" for key in
                                             ("GH_TOKEN", "GITHUB_TOKEN", "ACTIONS_RUNTIME_TOKEN", "PRIVATE_SECRET")}):
                runner = REPLAY.Probe(root, Path(directory) / "evidence", timeout=30)
                self.addCleanup(runner.close)
                result = REPLAY.localize([good, bad], self.candidate(), runner)
            self.assertEqual(result["localization"], "reproduced", result)
            self.assertEqual(result["commit"], bad)
            initial = {"sha": bad, "tree": git("rev-parse", bad + "^{tree}"),
                       "ancestor": {"sha": good, "tree": git("rev-parse", good + "^{tree}")},
                       "failure_candidates": [self.candidate()]}
            initial_dir = Path(directory) / "initial"
            initial_dir.mkdir()
            (initial_dir / "receipt.json").write_text(json.dumps(initial))
            result["initial_sha256"] = hashlib.sha256((initial_dir / "receipt.json").read_bytes()).hexdigest()
            verified = POLICY.verify_localization(initial, result, Path(directory) / "evidence", root)
            self.assertEqual(verified["localization"], "reproduced")
            corrupted = copy.deepcopy(result)
            corrupted["probes"][0]["results"][0]["state"] = "absent"
            with self.assertRaises(ValueError):
                POLICY.verify_localization(initial, corrupted, Path(directory) / "evidence", root)
            self.assertTrue(list((Path(directory) / "evidence").glob("*.jsonl")))
            self.assertFalse(any(path.name == "SHOULD_NOT_EXIST" for path in Path(directory).rglob("*")))
            with self.assertRaises(ValueError):
                runner(bad, {**self.candidate(), "test": "TestRegression;touch SHOULD_NOT_EXIST"})


class WorkflowTests(unittest.TestCase):
    def test_periodic_workflow_exists_and_pins_control_target_and_readonly_permissions(self):
        path = WORKFLOWS / "periodic-full.yml"
        self.assertTrue(path.exists(), "periodic-full.yml 尚未实现")
        text = path.read_text()
        for fragment in ["cron: '17 */6 * * *'", "group: main-master-periodic", "cancel-in-progress: false",
                         "contents: read", "actions: read", "pull-requests: read", "persist-credentials: false",
                         "uses: ./.github/workflows/ci-full.yml", "go test -count=1 ./cmd/workspace",
                         'ci-plan.py" web', 'ci-plan.py" bridge', "Periodic Full evidence",
                         "periodic-full-${{ github.run_id }}-${{ github.run_attempt }}", "receipt.json"]:
            self.assertIn(fragment, text)
        self.assertNotIn("gh run", text)
        self.assertNotIn("write-all", text)
        self.assertNotIn("secrets: inherit", text)
        self.assertNotIn("overwrite: true", text)
        self.assertNotIn("environment:", text)
        self.assertEqual(text.count("git/ref/heads/master"), 1)
        replay = re.search(r"^  replay:\n(.*?)(?=^  [\w-]+:|\Z)", text, re.M | re.S)[1]
        self.assertNotRegex(replay, r"GH_TOKEN|GITHUB_TOKEN|github.token|secrets\.")
        preflight = re.search(r"^  preflight:\n(.*?)(?=^  [\w-]+:|\Z)", text, re.M | re.S)[1]
        self.assertNotIn("GH_TOKEN", preflight)
        self.assertIn("CHECKOUT_TREE", preflight)


if __name__ == "__main__":
    unittest.main()
