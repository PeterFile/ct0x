#!/usr/bin/env python3
"""Offline canonical redirects: immutable small receipts only, no new release evidence."""
import copy
import importlib.util
import io
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import call, patch
import zipfile

HERE = Path(__file__).resolve().parent
REPO, OWNER, ATTEMPT = "owner/project", 10, 2
SHA, TREE, SOURCE = "a" * 40, "b" * 40, "c" * 40


def load(name):
    path = HERE / (name + ".py")
    assert path.is_file(), "missing canonical redirect verifier: " + name
    spec = importlib.util.spec_from_file_location(name.replace("-", "_"), path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


class DuplicateTests(unittest.TestCase):
    def setUp(self):
        self.duplicate = load("release-duplicate")
        self.core = self.duplicate.core
        scope = dict(schema=1, repo=REPO, sha=SHA, tree=TREE, run_id=OWNER, run_attempt=ATTEMPT)
        self.intent = scope | dict(kind="release-intent", workflow_sha=SOURCE, sources={"policy": "d" * 40},
                                   policy_sha=SOURCE, policy="caller-policy-v1")
        self.run = dict(id=OWNER, run_attempt=ATTEMPT, workflow_id=23, path=self.core.WORKFLOW,
                        repository={"full_name": REPO}, head_repository={"full_name": REPO},
                        head_sha=SOURCE, head_commit={"id": SOURCE}, run_started_at="2026-11-26T00:59:59Z",
                        head_branch="master", event="workflow_dispatch", status="completed", conclusion="success",
                        referenced_workflows=[{"path": REPO + "/.github/workflows/ci-full.yml", "sha": SOURCE}])
        names = ["Release candidate verdict", "Release Full evidence", "Release machine evidence", "Release Full / Full suite evidence", "Release candidate evidence"]
        self.jobs = [dict(id=index + 30, run_id=OWNER, run_attempt=ATTEMPT, name=name,
                          status="completed", conclusion="success", started_at="2026-11-26T01:00:00Z",
                          completed_at="2026-11-26T01:01:00Z") for index, name in enumerate(names)]
        fact = lambda number: dict(id=number, digest="sha256:" + "d" * 64, receipt_sha256="e" * 64)
        self.target = fact(40)
        self.full = scope | dict(kind="release-full", status="success", verified=True, workflow_sha=SOURCE,
                                 artifact_sha256="f" * 64, target_artifact=self.target, artifacts=[], jobs=[self.jobs[3]])
        self.binding = scope | dict(kind="release-full-binding", workflow_sha=SOURCE, artifact_sha256="f" * 64,
                                    job_id=self.jobs[1]["id"], producer=self.jobs[1]["name"])
        self.lanes = [scope | dict(name=name, status="success", artifact_sha256="f" * 64) for name in ("6a", "6b", "6c")]
        self.machine = scope | dict(kind="release-machine", workflow_sha=SOURCE, artifact_sha256="f" * 64,
                                    target_artifact=self.target, lanes=self.lanes, full_binding=self.binding,
                                    sources={"audited-driver": "d" * 64})
        self.collected = scope | dict(kind="release-machine-collected", machine=self.machine, intake_artifact=fact(41), machine_signature="isolated-signature")
        self.value = scope | dict(kind="release-evidence", workflow_sha=SOURCE, status="success", verified=True,
                                  authoritative=True, publishable=False, verification="openssh-machine-witness-v1",
                                  artifact_sha256="f" * 64, target=self.target, full=self.binding,
                                  machine_lanes=self.lanes, full_lanes=[], intake=self.collected["intake_artifact"],
                                  machine_receipt=self.machine, machine_signature=self.collected["machine_signature"])
        self.artifacts, self.archives, self.requests = [], {}, []
        self.add_artifact(50, f"release-full-{OWNER}-{ATTEMPT}", "receipt.json", self.full)
        self.binding.update(self.duplicate.evidence.artifact_fact(self.artifacts[-1], self.core.hashed(self.core.encoded(self.full))))
        self.add_artifact(51, f"release-machine-collected-{OWNER}-{ATTEMPT}", "receipt.json", self.collected)
        self.value["machine"] = self.duplicate.evidence.artifact_fact(self.artifacts[-1], self.core.hashed(self.core.encoded(self.collected)))
        self.refresh_final()
        self.api = self.core.GitHub(REPO, "offline-test-token")
        self.api.request = self.request
        # The producer owns source/lane validation. Keep this unit seam explicit:
        # guard must authenticate completed owner provenance, validate_machine must
        # accept the mandatory Full join and reject changed source manifests.
        self.guard = self.enterContext(patch.object(self.duplicate.control, "guard", return_value=self.intent))
        self.validate = self.enterContext(patch.object(self.duplicate.evidence, "validate_machine", side_effect=self.validate_machine))
        # Real SSHSIG/replay tests live in test-release-host-proof.py; this suite
        # retains genuine native API, artifact and completed-guard verification.
        self.host_validation = self.enterContext(patch.object(self.duplicate.host, "validate_evidence", return_value=None))

    def validate_machine(self, value, require_full=False):
        self.assertTrue(require_full, "redirect cannot accept a pre-Full machine receipt")
        self.core.check(value["sources"] == {"audited-driver": "d" * 64}, "machine source mismatch")
        self.core.check([lane["name"] for lane in value["lanes"]] == ["6a", "6b", "6c"], "missing native lane")
        for lane in value["lanes"]:
            self.core.scope(lane, REPO, SHA, TREE, OWNER, ATTEMPT)
            self.core.check(lane["status"] == "success" and lane["artifact_sha256"] == "f" * 64, "bad native lane")

    def add_artifact(self, identity, name, filename, value, extra=None):
        stream = io.BytesIO()
        with zipfile.ZipFile(stream, "w") as archive:
            archive.writestr(filename, self.core.encoded(value))
            for other, content in (extra or {}).items():
                archive.writestr(other, self.core.encoded(content))
        raw = stream.getvalue()
        artifact = dict(id=identity, name=name, expired=False, digest="sha256:" + self.core.hashed(raw),
                        workflow_run={"id": OWNER}, created_at="2026-11-26T01:00:30Z", updated_at="2026-11-26T01:00:30Z")
        self.artifacts[:] = [row for row in self.artifacts if row["id"] != identity] + [artifact]
        self.archives[identity] = raw

    def refresh_final(self):
        materials = {key: value for key, value in self.value.items() if key not in ("host_proof", "materials", "publishable", "verification")}
        materials.update(kind="release-materials", status="collected", verified=False, authoritative=False, publishable=False)
        decision = {key: materials[key] for key in ("schema", "repo", "sha", "tree", "run_id", "run_attempt", "workflow_sha")}
        decision.update(kind="release-host-proof", status="verified", signer_sha256="1" * 64,
                        materials_sha256=self.core.hashed(self.core.encoded(materials)))
        self.add_artifact(53, f"release-materials-{OWNER}-{ATTEMPT}", "receipt.json", materials, {"decision.json": decision})
        self.value["host_proof"] = decision
        self.value["materials"] = self.duplicate.evidence.artifact_fact(self.artifacts[-1], self.core.hashed(self.core.encoded(materials))) | {
            "decision_sha256": self.core.hashed(self.core.encoded(decision))}
        self.add_artifact(52, f"release-evidence-{SHA}-{OWNER}-{ATTEMPT}", "evidence.json", self.value)

    def request(self, method, path, data=None, binary=False):
        self.assertEqual(method, "GET", "duplicates must be read-only")
        self.requests.append(path)
        prefix = self.api.prefix
        if binary:
            identity = int(path.rsplit("/", 2)[1])
            return self.archives[identity]
        if path == prefix + "/actions/workflows/release-candidate.yml":
            return {"id": 23, "path": self.core.WORKFLOW}
        if path.split("?", 1)[0].endswith("/jobs"):
            return {"jobs": copy.deepcopy(self.jobs)}
        if path.split("?", 1)[0].endswith("/artifacts"):
            return {"artifacts": copy.deepcopy(self.artifacts)}
        if path in (prefix + f"/actions/runs/{OWNER}", prefix + f"/actions/runs/{OWNER}/attempts/{ATTEMPT}"):
            return copy.deepcopy(self.run)
        raise AssertionError("unexpected or unbounded API request: " + path)

    def reuse(self):
        return self.duplicate.reuse(self.api, OWNER, SHA, TREE)

    def test_success_redirects_only_to_existing_owner_evidence_and_never_creates_new_identity(self):
        result = self.reuse()
        self.assertEqual(result, {"owner_run_id": OWNER, "owner_attempt": ATTEMPT,
                                  "evidence_url": f"https://github.com/{REPO}/actions/runs/{OWNER}/artifacts/52"})
        self.assertEqual(self.guard.call_args_list, [call(self.api, OWNER, ATTEMPT, completed=True)] * 2)
        self.validate.assert_called_once_with(self.machine, require_full=True)
        self.host_validation.assert_called_once_with(self.value, HERE.parents[1], SOURCE)
        downloads = [path for path in self.requests if path.endswith("/zip")]
        self.assertEqual(len(downloads), 3)
        self.assertEqual({int(path.rsplit("/", 2)[1]) for path in downloads}, {50, 51, 52})
        self.assertNotIn("verified", result)
        self.assertNotIn("run_id", result)

    def native_guard_api(self):
        fixture = load("test-release-control")
        api = fixture.NativeAPI()
        api.native.update(workflow_id=23, referenced_workflows=self.run["referenced_workflows"],
                          status="completed", conclusion="failure")
        api.job_rows[1].update(status="completed", conclusion="success", completed_at="2026-11-26T15:56:54Z")
        api.second_run = api.native | {"run_attempt": ATTEMPT, "run_started_at": "2026-11-26T16:00:00Z",
                                     "status": "completed", "conclusion": "success"}
        api.second_jobs = [
            api.job_rows[0] | {"id": 120, "run_attempt": ATTEMPT, "started_at": "2026-11-26T16:00:01Z", "completed_at": "2026-11-26T16:00:04Z"},
            api.job_rows[1] | {"id": 121, "run_attempt": ATTEMPT, "started_at": "2026-11-26T16:00:05Z", "completed_at": "2026-11-26T16:00:06Z"}]
        api.intent.update(run_attempt=ATTEMPT)
        api.artifact_time = "2026-11-26T16:00:03Z"
        self.run, self.intent, self.api = api.second_run, api.intent, api
        for job in self.jobs:
            job.update(started_at="2026-11-26T16:00:07Z", completed_at="2026-11-26T16:01:00Z")
        self.add_artifact(50, f"release-full-{OWNER}-{ATTEMPT}", "receipt.json", self.full)
        self.binding.update(self.duplicate.evidence.artifact_fact(self.artifacts[-1], self.core.hashed(self.core.encoded(self.full))))
        self.add_artifact(51, f"release-machine-collected-{OWNER}-{ATTEMPT}", "receipt.json", self.collected)
        self.value["machine"] = self.duplicate.evidence.artifact_fact(self.artifacts[-1], self.core.hashed(self.core.encoded(self.collected)))
        self.refresh_final()
        for artifact in self.artifacts:
            artifact.update(created_at="2026-11-26T16:00:30Z", updated_at="2026-11-26T16:00:30Z")
        original = api.request
        def request(method, path, data=None, binary=False):
            self.assertEqual(method, "GET", "genuine completed guard must remain read-only")
            prefix = api.prefix
            if path == prefix + "/actions/workflows/release-candidate.yml":
                return {"id": 23, "path": self.core.WORKFLOW}
            if path.startswith(prefix + f"/actions/runs/{OWNER}/attempts/{ATTEMPT}/jobs?"):
                return {"jobs": copy.deepcopy(api.second_jobs + self.jobs)}
            if path.startswith(prefix + f"/actions/runs/{OWNER}/artifacts?"):
                result = original(method, path, data, binary)
                return {"artifacts": result["artifacts"] + copy.deepcopy(self.artifacts)}
            if binary and int(path.rsplit("/", 2)[1]) in self.archives:
                return self.request(method, path, data, binary)
            return original(method, path, data, binary)
        api.request = request
        # Spy on the real guard; only unrelated machine-driver validation is a seam.
        self.guard.reset_mock()
        self.guard.side_effect = load("release-control").guard

    def test_successful_redirect_runs_genuine_completed_guard_before_and_after_receipts(self):
        self.native_guard_api()
        result = self.reuse()
        self.assertEqual(result["owner_attempt"], ATTEMPT)
        self.assertTrue(result["evidence_url"].endswith("/artifacts/52"))
        self.assertEqual(self.guard.call_args_list, [call(self.api, OWNER, ATTEMPT, completed=True)] * 2)
        self.assertEqual(self.api.posts, 0)
        self.assertTrue(all(method == "GET" for method, _ in self.api.calls))

    def test_genuine_completed_guard_rejects_missing_source_inherited_or_other_attempt_admission(self):
        for change in ("source", "inherited-normalize", "inherited-admission", "other-attempt-admission"):
            self.native_guard_api()
            if change == "source":
                self.api.intent["sources"] = {}
            else:
                job = self.api.second_jobs[0 if change == "inherited-normalize" else 1]
                if change == "other-attempt-admission":
                    job["run_attempt"] = 1
                else:
                    job.update(started_at="2026-11-26T15:56:48Z", completed_at="2026-11-26T15:56:52Z")
            with self.subTest(change=change), self.assertRaises(ValueError):
                self.reuse()
            self.assertEqual(self.api.posts, 0)

    def test_genuine_final_guard_rejects_superseding_attempt_and_unknown_after_first_run_read(self):
        self.native_guard_api()
        real_guard = self.guard.side_effect
        def superseded(*args, **kwargs):
            if self.guard.call_count == 2:
                self.run["run_attempt"] = ATTEMPT + 1
            return real_guard(*args, **kwargs)
        self.guard.side_effect = superseded
        with self.assertRaises(ValueError):
            self.reuse()
        self.assertEqual(self.guard.call_count, 2)
        self.assertEqual(self.api.posts, 0)
        self.native_guard_api()
        original, reads = self.api.request, []
        def unknown(method, path, data=None, binary=False):
            if path == self.api.prefix + f"/actions/runs/{OWNER}":
                reads.append(path)
                if len(reads) > 1:
                    raise OSError("canonical native outcome unknown")
            return original(method, path, data, binary)
        self.api.request = unknown
        with self.assertRaises(OSError):
            self.reuse()
        self.assertEqual(self.api.posts, 0)

    def test_signature_assessment_staging_and_collected_signature_cannot_be_bypassed(self):
        self.host_validation.side_effect = ValueError("invalid SSHSIG or no reviewed anchor")
        with self.assertRaises(ValueError):
            self.reuse()
        self.host_validation.side_effect = None
        for value in self.artifacts:
            if value["id"] == 53:
                value["expired"] = True
        with self.assertRaises(ValueError):
            self.reuse()
        self.refresh_final()
        self.collected["machine_signature"] = "different-signature"
        self.add_artifact(51, f"release-machine-collected-{OWNER}-{ATTEMPT}", "receipt.json", self.collected)
        self.value["machine"] = self.duplicate.evidence.artifact_fact(self.artifacts[-1], self.core.hashed(self.core.encoded(self.collected)))
        self.refresh_final()
        with self.assertRaises(ValueError):
            self.reuse()
        self.collected["machine_signature"] = self.value["machine_signature"]
        self.value["verification"] = "owner-transport-native-full-join-v1"
        self.refresh_final()
        with self.assertRaises(ValueError):
            self.reuse()

    def test_pending_failed_cancelled_unknown_owner_never_downloads_evidence(self):
        original = self.run.copy()
        for change in ({"status": "in_progress", "conclusion": None}, {"status": "queued"},
                       {"conclusion": "failure"}, {"conclusion": "cancelled"}, {"conclusion": None}):
            with self.subTest(change=change), self.assertRaises(ValueError):
                self.run = original | change
                self.reuse()
        self.assertFalse(any(path.endswith("/zip") for path in self.requests))

    def test_missing_expired_foreign_or_forged_artifact_is_denied(self):
        for change in ({"expired": True}, {"name": "release-evidence-forged"}, {"workflow_run": {"id": 11}},
                       {"digest": "sha256:" + "0" * 64}, {"created_at": "2026-11-25T01:00:00Z"}):
            original = copy.deepcopy(self.artifacts)
            with self.subTest(change=change), self.assertRaises(ValueError):
                next(row for row in self.artifacts if row["id"] == 52).update(change)
                self.reuse()
            self.artifacts = original
        self.artifacts = [row for row in self.artifacts if row["id"] != 52]
        with self.assertRaises(ValueError):
            self.reuse()

    def test_stale_attempt_wrong_workflow_verdict_or_guard_is_denied(self):
        for change in ({"workflow_id": 24}, {"path": ".github/workflows/ci.yml"},
                       {"head_branch": "untrusted"}, {"head_repository": {"full_name": "other/repo"}},
                       {"head_sha": TREE}):
            original = self.run.copy()
            with self.subTest(change=change), self.assertRaises(ValueError):
                self.run = original | change
                self.reuse()
            self.run = original
        for change in ({"name": "Release verdict"}, {"run_attempt": 1}, {"conclusion": "skipped"}):
            original = self.jobs[0].copy()
            with self.subTest(change=change), self.assertRaises(ValueError):
                self.jobs[0].update(change)
                self.reuse()
            self.jobs[0] = original
        self.guard.side_effect = ValueError("source identity or canonical guard rejected")
        with self.assertRaises(ValueError):
            self.reuse()

    def test_wrong_scope_non_authoritative_awaiting_and_changed_machine_sources_are_denied(self):
        original = copy.deepcopy(self.value)
        changes = [{"run_id": 11}, {"run_attempt": 1}, {"sha": TREE}, {"tree": SHA}, {"workflow_sha": TREE},
                   {"verified": False}, {"verified": "true"}, {"authoritative": False}, {"publishable": True},
                   {"status": "awaiting-local-native-verification"}, {"verification": "unverified"}]
        for change in changes:
            with self.subTest(change=change), self.assertRaises(ValueError):
                self.value = original | change
                self.refresh_final()
                self.reuse()
        self.value = copy.deepcopy(original)
        self.value["full"]["run_attempt"] = 1
        self.refresh_final()
        with self.assertRaises(ValueError):
            self.reuse()
        self.value = copy.deepcopy(original)
        self.value["machine_lanes"][0]["sha"] = TREE
        self.refresh_final()
        with self.assertRaises(ValueError):
            self.reuse()
        self.value = original
        self.machine["sources"] = {"forged-driver": "d" * 64}
        self.add_artifact(51, f"release-machine-collected-{OWNER}-{ATTEMPT}", "receipt.json", self.collected)
        self.value["machine"] = self.duplicate.evidence.artifact_fact(self.artifacts[-1], self.core.hashed(self.core.encoded(self.collected)))
        self.refresh_final()
        with self.assertRaises(ValueError):
            self.reuse()

    def test_cli_keeps_original_pointer_on_failure_and_writes_no_release_json(self):
        with tempfile.TemporaryDirectory(prefix="release-duplicate-") as temporary:
            root = Path(temporary)
            env = dict(GITHUB_REPOSITORY=REPO, OWNER_RUN_ID=str(OWNER), CANDIDATE_SHA=SHA, CANDIDATE_TREE=TREE,
                       GITHUB_STEP_SUMMARY=str(root / "summary"), GITHUB_OUTPUT=str(root / "outputs"))
            with patch.dict(os.environ, env), patch.object(self.duplicate.control, "AdmissionAPI", return_value=self.api), \
                 patch.object(self.duplicate, "reuse", side_effect=ValueError("pending")):
                with self.assertRaises(ValueError):
                    self.duplicate.main()
            self.assertIn(f"https://github.com/{REPO}/actions/runs/{OWNER}", (root / "summary").read_text())
            self.assertEqual({path.name for path in root.iterdir()}, {"summary"})


if __name__ == "__main__":
    unittest.main()
