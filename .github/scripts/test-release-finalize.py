#!/usr/bin/env python3
"""Read-only finalization tests. Synthetic native files; no GitHub or provider calls."""
import copy
from contextlib import ExitStack, closing
import importlib.util
import json
import os
import runpy
import sqlite3
import subprocess
import base64
from pathlib import Path
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

HERE = Path(__file__).resolve().parent


def load(name):
    spec = importlib.util.spec_from_file_location(name.replace("-", "_"), HERE / (name + ".py"))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


class FinalizeTests(unittest.TestCase):
    def setUp(self):
        self.assertTrue((HERE / "release-finalize.py").is_file(), "缺少完成后的独立本机 finalize")
        self.finalizer = load("release-finalize")
        self.machine, self.evidence = self.finalizer.machine, self.finalizer.evidence
        self.proof = self.finalizer.proof
        self.core = self.finalizer.core
        helpers = load("release-test-evidence")
        self.value = helpers.machine_receipt()
        self.scope = {key: self.value[key] for key in ("schema", "repo", "sha", "tree", "run_id", "run_attempt", "workflow_sha")}
        self.intent = self.scope | {"head": self.value["sha"], "base": self.value["tree"], "sources": {},
                                    "policy_sha": self.value["workflow_sha"], "policy": "caller-policy-v1"}
        self.target = self.value["target_artifact"]
        self.binary = helpers.BINARY
        self.full = self.scope | {"kind": "release-full", "status": "success", "verified": True,
                                  "artifact_sha256": self.value["artifact_sha256"], "target_artifact": self.target,
                                  "artifacts": [], "jobs": []}
        self.jobs = [{"id": i + 100, "run_id": 10, "run_attempt": 2, "name": name,
                      "status": "completed", "conclusion": "success", "started_at": "2026-11-26T16:00:00Z", "completed_at": "2026-11-26T16:00:30Z"} for i, name in enumerate(
                          ("Release Full evidence", "Release machine evidence", "Release clients",
                           "Release candidate binary", "Release candidate evidence", "Release candidate verdict"))]
        fact = lambda i: {"id": i, "digest": "sha256:" + str(i % 10) * 64}
        self.intake_artifact, self.full_artifact, self.machine_artifact, self.hosted_artifact = map(fact, (41, 42, 43, 44))
        self.intake_hash, self.full_hash, self.machine_hash = "7" * 64, "8" * 64, "9" * 64
        rows = [{"Action": action, "Test": name, "Package": package} for package, name in self.machine.UNKNOWN_TESTS for action in ("run", "pass")]
        rows += [{"Action": "pass", "Package": package} for package in sorted({p for p, _ in self.machine.UNKNOWN_TESTS})]
        self.race_bytes = b"".join(self.core.encoded(row) for row in rows)
        self.value["unknown_guard"] = {"mode": "race", "proof_sha256": self.core.hashed(self.race_bytes)}
        self.native_value = copy.deepcopy(self.value)
        self.value["full_binding"] = self.scope | {"kind": "release-full-binding", "artifact_sha256": self.value["artifact_sha256"],
            **self.evidence.artifact_fact(self.full_artifact, self.full_hash), "job_id": 100, "producer": "Release Full evidence"}
        for lane in self.value["lanes"]:
            lane["full_binding_sha256"] = self.core.hashed(self.core.encoded(self.value["full_binding"]))
        self.key_dir = tempfile.TemporaryDirectory(prefix="finalize-test-key-")
        self.addCleanup(self.key_dir.cleanup)
        key = Path(self.key_dir.name) / "test-key"
        subprocess.run(["/usr/bin/ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", str(key)], check=True, capture_output=True)
        self.key = key.with_suffix(".pub").read_text().strip()
        signed = subprocess.run(["/usr/bin/ssh-keygen", "-Y", "sign", "-f", str(key), "-n", self.proof.NAMESPACE],
                                input=self.core.encoded(self.value), check=True, capture_output=True).stdout
        self.signature = base64.b64encode(signed).decode()
        self.wrapper = {key: self.scope[key] for key in ("schema", "repo", "sha", "tree", "run_id", "run_attempt")} | {
            "kind": "release-machine-intake", "machine": self.value, "machine_signature": self.signature, **self.evidence.AWAITING,
            "intake": {"run_id": 99, "run_attempt": 1, "actor_id": 42, "workflow_sha": self.value["sha"]}}
        self.collected = {key: self.wrapper[key] for key in ("schema", "repo", "sha", "tree", "run_id", "run_attempt")} | {
            "kind": "release-machine-collected", "intake_run_id": 99, **self.evidence.AWAITING,
            "intake_artifact": self.evidence.artifact_fact(self.intake_artifact, self.intake_hash), "machine": self.value,
            "machine_signature": self.signature}
        outer = self
        class API:
            repo, prefix = outer.value["repo"], "/repos/" + outer.value["repo"]
            status, conclusion, attempt = "completed", "success", 2
            writes = 0
            def request(self, method, *args, **kwargs):
                if method != "GET":
                    self.writes += 1
                    raise AssertionError("finalize must never write remotely")
                raise AssertionError("unexpected request")
            def get(self, path):
                return {"owner": {"id": 42}}
            def run(self, run_id, attempt=None):
                if run_id == 99:
                    return {"id": 99, "path": outer.core.INTAKE, "head_branch": "master", "event": "workflow_dispatch",
                            "run_attempt": 1, "status": "completed", "conclusion": "success", "head_sha": outer.value["sha"],
                            "actor": {"id": 42}, "triggering_actor": {"id": 42}, "display_title": "RC receipt 10 attempt 2",
                            "created_at": "2026-11-26T16:01:00Z"}
                return {"id": 10, "path": outer.core.WORKFLOW, "event": "workflow_dispatch", "head_branch": "master", "head_sha": outer.value["workflow_sha"], "run_attempt": self.attempt,
                        "repository": {"full_name": self.repo}, "head_repository": {"full_name": self.repo},
                        "status": self.status, "conclusion": self.conclusion, "run_started_at": "2026-11-26T15:59:00Z",
                        "referenced_workflows": [{"path": self.repo + "/.github/workflows/ci-full.yml@master", "sha": outer.value["workflow_sha"]}]}
            def jobs(self, run_id, attempt):
                if run_id == 99:
                    return [{"id": 500, "name": "Release receipt intake", "run_id": 99, "run_attempt": 1,
                             "status": "completed", "conclusion": "success"}]
                return outer.jobs
            def receipt(self, run_id, name, producer=None):
                if name == "release-full-10-2":
                    return outer.full, outer.full_artifact, outer.full_hash
                if name == "release-machine-collected-10-2":
                    return outer.collected, outer.machine_artifact, outer.machine_hash
                if name == "release-machine-10-2-99":
                    return outer.wrapper, outer.intake_artifact, outer.intake_hash
                raise AssertionError("unexpected artifact: " + name)
            def artifact(self, run_id, name, producer):
                if name == "release-materials-10-2":
                    return outer.materials_artifact
                if name != outer.evidence.verdict_name(outer.intent):
                    raise AssertionError("unexpected hosted artifact name")
                return outer.hosted_artifact
            def download(self, artifact, names, *args):
                if artifact == outer.materials_artifact:
                    return {"receipt.json": outer.core.encoded(outer.materials), "decision.json": outer.core.encoded(outer.decision)}
                return {"evidence.json": outer.core.encoded(outer.hosted)}
        self.api = API()
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name).resolve()
        self.root.chmod(0o700)
        self.claim = self.machine.claim_path(self.root, self.api.repo, 10, 2)
        self.claim.mkdir(parents=True, mode=0o700)
        self.args = SimpleNamespace(work_dir=self.root, run_id=10, attempt=2, harness_root=self.root,
                                    go=Path("/trusted/go"), expected_sha=self.value["sha"], artifact=self.root / "prospective-workspace")
        self.args.artifact.write_bytes(self.binary)
        self.materials_artifact = fact(45)
        self.core.save(self.claim / "receipt.json", self.native_value)
        self.core.save(self.claim / "complete.json", {"receipt_sha256": self.core.file_hash(self.claim / "receipt.json")})
        self.core.save(self.claim / "intent.json", self.intent)
        self.core.save(self.claim / "source-manifest.json", self.evidence.trusted_bundle())
        (self.claim / "workspace").write_bytes(self.binary)
        (self.claim / "unknown-race.jsonl").write_bytes(self.race_bytes)
        for phase in ("before", "after"):
            self.core.save(self.claim / ("binary-" + phase + ".json"), {"path": str(self.claim / "workspace"),
                "package": "agentworkspace/cmd/workspace", "vcs_revision": self.value["sha"],
                "vcs_modified": False, "sha256": self.value["artifact_sha256"]})
        self.stack = ExitStack()
        self.addCleanup(self.stack.close)
        self.guard = self.stack.enter_context(patch.object(self.finalizer.control, "guard", return_value=self.intent))
        self.stack.enter_context(patch.object(self.machine, "authorized_host"))
        self.stack.enter_context(patch.object(self.machine, "machine_identity", return_value=self.native_value["machine_id"]))
        self.stack.enter_context(patch.object(self.machine, "STATE_ROOT", self.root / "test-state"))
        self.stack.enter_context(patch.object(self.machine, "verify_control_files"))
        self.stack.enter_context(patch.object(self.machine, "source_manifest", return_value=({}, self.value["sources"], self.evidence.trusted_bundle())))
        self.stack.enter_context(patch.object(self.evidence, "intake_source"))
        self.stack.enter_context(patch.object(self.evidence, "target", return_value=({"artifact_sha256": self.value["artifact_sha256"]}, self.binary, self.target)))
        # Full's own archive/shard validation is a separately tested producer.
        self.expected_full = copy.deepcopy(self.full)
        self.full_check = self.stack.enter_context(patch.object(self.evidence, "full_evidence", return_value=self.expected_full))
        self.raw = self.stack.enter_context(patch.object(self.finalizer, "recompute", return_value=self.native_value["lanes"]) )
        metadata = "\tpath\tagentworkspace/cmd/workspace\n" + "".join("\tbuild\t" + k + "=" + v + "\n" for k, v in {
            "vcs": "git", "vcs.revision": self.value["sha"], "vcs.modified": "false", "GOOS": "darwin", "GOARCH": "amd64",
            "CGO_ENABLED": "0", "-trimpath": "true"}.items())
        self.stack.enter_context(patch.object(self.evidence.subprocess, "check_output", return_value=metadata))
        self.stack.enter_context(patch.object(self.proof, "public_key", side_effect=lambda *args: self.key))
        self.materials = self.evidence.joined_evidence(self.api, self.intent, self.root)
        self.decision = self.proof.assess(self.materials, self.root, self.intent["workflow_sha"])
        self.assertEqual(self.decision["status"], "verified", "fixture must contain a real cryptographic witness")
        self.hosted = self.materials | {"kind": "release-evidence", "status": "success", "verified": True, "authoritative": True,
            "publishable": False, "verification": "openssh-machine-witness-v1", "host_proof": self.decision,
            "materials": self.proof._fact(self.materials, self.decision, self.materials_artifact)}

    def test_hosted_shape_correct_intake_is_never_authoritative_without_local_raw(self):
        self.assertEqual(self.wrapper["status"], "awaiting-local-native-verification")
        self.assertFalse(self.wrapper["authoritative"])
        self.value["full_binding"] = None
        with self.assertRaises(ValueError):
            self.evidence.joined_evidence(self.api, self.intent, self.root)
        self.value["full_binding"] = self.hosted["full"]
        self.raw.side_effect = ValueError("original machine raw files missing")
        with self.assertRaises(ValueError):
            self.finalizer.finalize(self.args, self.api)
        self.assertFalse((self.claim / "release-evidence.json").exists())

    def test_completed_success_finalizes_locally_and_reconciles_idempotently(self):
        with patch.object(self.machine, "ensure_active", side_effect=AssertionError("completed is not active")):
            first = self.finalizer.finalize(self.args, self.api)
            second = self.finalizer.finalize(self.args, self.api)
        self.assertEqual(first, second)
        self.assertTrue(first["authoritative"] and first["verified"])
        self.assertFalse(first["publishable"])
        self.assertEqual(first["full"]["run_id"], 10)
        self.assertEqual(first["full"]["run_attempt"], 2)
        self.assertEqual(first["artifact_sha256"], self.value["artifact_sha256"])
        self.assertEqual(self.raw.call_count, 2, "idempotence must still revalidate underlying bytes")
        self.assertGreaterEqual(self.guard.call_count, 4, "canonical guard before and after reconciliation")
        self.assertEqual(self.api.writes, 0)
        self.assertTrue(all(call.kwargs.get("completed") is True for call in self.guard.call_args_list), "all post-completion guards must be explicit")
        self.assertEqual(first["publication_artifact"]["path"], str(self.args.artifact.resolve()))
        self.assertEqual(first["publication_artifact"]["sha256"], self.value["artifact_sha256"])
        self.assertEqual((self.claim / "release-evidence.json").stat().st_mode & 0o777, 0o600)

    def test_missing_claim_waits_and_failed_full_does_not_look_like_success(self):
        self.temporary.cleanup()
        self.root.mkdir(mode=0o700)
        self.full_check.reset_mock()
        self.assertEqual(self.finalizer.prepare_submission(self.args, self.api)["status"], "waiting")
        self.full_check.assert_called_once()
        self.jobs[0]["conclusion"] = "failure"
        with self.assertRaises(ValueError):
            self.finalizer.prepare_submission(self.args, self.api)

    def test_submit_joins_full_and_original_raw_before_any_post(self):
        result = self.finalizer.prepare_submission(self.args, self.api)
        self.assertEqual(result, self.value)
        self.assertEqual(result["full_binding"], self.hosted["full"])
        self.raw.assert_called_once()
        self.assertEqual(self.api.writes, 0)
        self.raw.side_effect = ValueError("original audits now fail")
        with self.assertRaises(ValueError):
            self.finalizer.prepare_submission(self.args, self.api)

    def test_missing_native_or_pending_full_waits_without_submission_marker(self):
        self.jobs[0]["status"], self.jobs[0]["conclusion"] = "in_progress", None
        self.assertEqual(self.finalizer.prepare_submission(self.args, self.api)["status"], "waiting")
        self.jobs[0]["status"], self.jobs[0]["conclusion"] = "completed", "success"
        (self.claim / "complete.json").unlink()
        self.assertEqual(self.finalizer.prepare_submission(self.args, self.api)["status"], "waiting")
        self.assertFalse((self.claim / "submission-started.json").exists())
        self.assertEqual(self.api.writes, 0)

    def test_terminal_success_and_current_attempt_are_mandatory(self):
        for status, conclusion, attempt in (("in_progress", None, 2), ("completed", "cancelled", 2),
                                             ("completed", "failure", 2), ("completed", "success", 3)):
            self.api.status, self.api.conclusion, self.api.attempt = status, conclusion, attempt
            with self.subTest(status=status, conclusion=conclusion, attempt=attempt), self.assertRaises(ValueError):
                self.finalizer.finalize(self.args, self.api)
        self.assertFalse((self.claim / "release-evidence.json").exists())

    def test_separately_valid_but_mismatched_full_sha_binary_or_attempt_is_rejected(self):
        for key, value in (("sha", "c" * 40), ("tree", "c" * 40), ("artifact_sha256", "c" * 64),
                           ("run_id", 11), ("run_attempt", 1)):
            original = self.full[key]
            self.full[key] = value
            with self.subTest(key=key), self.assertRaises(ValueError):
                self.finalizer.finalize(self.args, self.api)
            self.full[key] = original

    def test_public_intake_and_verdict_independently_reject_wrong_actual_full_binding(self):
        self.api.status = "in_progress"
        event = {"sender": {"id": 42}, "repository": {"owner": {"id": 42}}}
        env = {"GITHUB_REF": "refs/heads/master", "GITHUB_WORKFLOW_SHA": self.value["workflow_sha"]}
        original_run = self.api.run
        def active_intake(run_id, attempt=None):
            native = original_run(run_id, attempt)
            return native | {"status": "in_progress", "conclusion": None} if run_id == 99 else native
        with patch.dict(os.environ, env), patch.object(self.evidence.control, "guard", return_value=self.intent), patch.object(self.api, "run", side_effect=active_intake):
            accepted = self.evidence.intake(self.api, event, self.value, 99, 1, self.root)
            self.assertFalse(accepted["authoritative"])
            for key, replacement in (("id", 999), ("job_id", 999), ("digest", "sha256:" + "0" * 64),
                                     ("receipt_sha256", "0" * 64)):
                original = self.value["full_binding"][key]
                self.value["full_binding"][key] = replacement
                for lane in self.value["lanes"]:
                    lane["full_binding_sha256"] = self.core.hashed(self.core.encoded(self.value["full_binding"]))
                with self.subTest(key=key), self.assertRaises(ValueError):
                    self.evidence.intake(self.api, event, self.value, 99, 1, self.root)
                with self.assertRaises(ValueError):
                    self.evidence.joined_evidence(self.api, self.intent, self.root)
                self.value["full_binding"][key] = original
                for lane in self.value["lanes"]:
                    lane["full_binding_sha256"] = self.core.hashed(self.core.encoded(self.value["full_binding"]))

    def test_copied_receipt_from_another_host_is_not_this_machine_execution(self):
        value = copy.deepcopy(self.native_value)
        value["machine_id"] = "0" * 64
        (self.claim / "receipt.json").write_bytes(self.core.encoded(value))
        (self.claim / "complete.json").write_bytes(self.core.encoded({"receipt_sha256": self.core.file_hash(self.claim / "receipt.json")}))
        with self.assertRaises(ValueError):
            self.finalizer.prepare_submission(self.args, self.api)

    def test_valid_but_different_build_metadata_is_not_actual_executed_candidate(self):
        with patch.object(self.evidence.subprocess, "check_output", return_value="\tpath\tagentworkspace/cmd/workspace\n\tbuild\tvcs.revision=" + "c" * 40 + "\n"):
            with self.assertRaises(ValueError):
                self.finalizer.prepare_submission(self.args, self.api)
        path = self.claim / "binary-before.json"
        before = self.core.decode(path.read_bytes())
        before["path"] = str(self.claim / "some-other-workspace")
        path.write_bytes(self.core.encoded(before))
        with self.assertRaises(ValueError):
            self.finalizer.prepare_submission(self.args, self.api)

    def test_verdict_full_artifact_id_producer_attempt_and_local_binary_are_bound(self):
        for mutate, undo in (
            (lambda: self.hosted["full"].update(id=999), lambda: self.hosted["full"].update(id=42)),
            (lambda: self.jobs[0].update(run_attempt=1), lambda: self.jobs[0].update(run_attempt=2)),
            (lambda: self.jobs[-1].update(conclusion="cancelled"), lambda: self.jobs[-1].update(conclusion="success")),
            (lambda: (self.claim / "workspace").write_bytes(b"changed"), lambda: (self.claim / "workspace").write_bytes(self.binary)),
        ):
            mutate()
            with self.assertRaises(ValueError):
                self.finalizer.finalize(self.args, self.api)
            undo()
        self.assertFalse((self.claim / "release-evidence.json").exists())

    def test_underlying_proof_or_existing_final_tampering_never_uses_cached_success(self):
        self.finalizer.finalize(self.args, self.api)
        changed = copy.deepcopy(self.native_value["lanes"])
        changed[0]["proofs"][0]["protocol_sha256"] = "0" * 64
        self.raw.return_value = changed
        with self.assertRaises(ValueError):
            self.finalizer.finalize(self.args, self.api)
        self.raw.return_value = self.native_value["lanes"]
        path = self.claim / "release-evidence.json"
        final = self.core.decode(path.read_bytes(), self.proof.MAX_MATERIALS + self.core.MAX_RECEIPT)
        final["full"]["run_attempt"] = 1
        path.write_bytes(self.core.encoded(final))
        with self.assertRaises(ValueError):
            self.finalizer.finalize(self.args, self.api)

    def test_selected_publication_file_must_exist_and_hash_actual_bytes_even_with_same_vcs_sha(self):
        self.args.artifact.write_bytes(self.binary + b"different real bytes with same embedded SHA")
        with self.assertRaises(ValueError):
            self.finalizer.finalize(self.args, self.api)
        self.args.artifact.unlink()
        with self.assertRaises((ValueError, OSError)):
            self.finalizer.finalize(self.args, self.api)
        self.assertFalse((self.claim / "release-evidence.json").exists())

    def test_expected_sha_is_required_and_cannot_switch_an_already_prepared_target(self):
        for expected in (None, "a" * 39, "c" * 40):
            self.args.expected_sha = expected
            with self.subTest(expected=expected), self.assertRaises(ValueError):
                self.finalizer.finalize(self.args, self.api)
        self.args.expected_sha = self.value["sha"]
        self.finalizer.finalize(self.args, self.api)
        self.args.expected_sha = "d" * 40
        with self.assertRaises(ValueError):
            self.finalizer.finalize(self.args, self.api)

    def test_provisional_or_bad_remote_signature_cannot_be_consumed_as_authoritative(self):
        for mutate in (lambda: self.hosted.update(authoritative=False),
                       lambda: self.hosted.update(verification="owner-transport-native-full-join-v1"),
                       lambda: self.hosted.update(machine_signature=""),
                       lambda: self.decision.update(status="no_host_anchor")):
            before_hosted, before_decision = copy.deepcopy(self.hosted), copy.deepcopy(self.decision)
            mutate()
            with self.assertRaises(ValueError):
                self.finalizer.finalize(self.args, self.api)
            self.hosted, self.decision = before_hosted, before_decision
        self.assertFalse((self.claim / "release-evidence.json").exists())


class NativeNoAnchorTests(unittest.TestCase):
    def setUp(self):
        fixtures = load("release-test-evidence")
        fixtures.EndToEndTests.setUp(self)
        self.finalizer = load("release-finalize")
        self.proof, self.machine, self.core = self.finalizer.proof, self.finalizer.machine, self.finalizer.core
        self.temp = tempfile.TemporaryDirectory(prefix="no-anchor-finalize-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name).resolve()
        self.root.chmod(0o700)
        self.controller = self.root / "controller"
        self.controller.mkdir()
        self.git("init", "-q")
        tree = self.git("mktree", data=b"").decode().strip()
        raw = f"tree {tree}\nauthor CI <ci@example.invalid> 1 +0000\ncommitter CI <ci@example.invalid> 1 +0000\n\nfixture\n".encode()
        source = self.git("hash-object", "-t", "commit", "-w", "--stdin", data=raw).decode().strip()
        self.git("checkout", "-q", "--detach", source)
        self.api.commits[source] = {"sha": source, "tree": {"sha": "4" * 40}, "parents": [{"sha": fixtures.SHA}]}
        self.api.trees["4" * 40] = self.api.blobs.copy()
        self.api.reachable[source] = {source, fixtures.SHA}
        self.api.reachable[self.api.master].add(source)
        for native in (self.api.native, self.api.runs[fixtures.RUN]):
            native.update(head_sha=source, head_commit={"id": source, "tree_id": "4" * 40})
        self.api.runs[fixtures.RUN]["referenced_workflows"][0]["sha"] = source
        self.intent.update(workflow_sha=source, policy_sha=source, master_sha=source)
        self.wrapper["machine"]["workflow_sha"] = source
        self.api.items[fixtures.RUN] = [row for row in self.api.items[fixtures.RUN] if not row["name"].startswith("release-intent-")]
        for attempt, at in ((1, "2026-11-26T15:56:51Z"), (fixtures.ATTEMPT, "2026-11-26T16:00:03Z")):
            self.api.upload(fixtures.RUN, f"release-intent-{fixtures.RUN}-{attempt}",
                            {"receipt.json": self.core.encoded(self.intent | {"run_attempt": attempt})}, time=at)
        self.api.items[fixtures.RUN] = [row for row in self.api.items[fixtures.RUN] if not row["name"].startswith("release-target-")]
        build = {key: self.intent[key] for key in ("repo", "sha", "tree", "run_id", "run_attempt", "workflow_sha")}
        build.update(schema=1, kind="release-target", artifact_sha256=self.core.hashed(fixtures.BINARY), goos="darwin", goarch="amd64",
                     cgo_enabled=False, vcs_modified=False, trimpath=True)
        target = self.api.upload(fixtures.RUN, "release-target-10-2", {"receipt.json": self.core.encoded(build), "workspace": fixtures.BINARY}, 30)
        self.wrapper["machine"]["target_artifact"] = self.evidence.artifact_fact(target, self.core.hashed(self.core.encoded(build)))
        fixtures.EndToEndTests.ready(self)
        self.collected = self.evidence.collect(self.api, self.intent, self.root, 0)
        self.api.upload(fixtures.RUN, "release-machine-collected-10-2", {"receipt.json": self.core.encoded(self.collected)})
        self.materials = self.evidence.materials(self.api, self.intent, self.needs, self.root)
        self.decision = self.proof.assess(self.materials, self.controller, source)
        self.assertEqual(self.decision["status"], "no_host_anchor")
        self.staging = self.api.upload(fixtures.RUN, "release-materials-10-2", {
            "receipt.json": self.core.encoded(self.materials), "decision.json": self.core.encoded(self.decision)})
        self.verdict = {"id": 900, "run_id": fixtures.RUN, "run_attempt": fixtures.ATTEMPT, "name": "Release candidate verdict",
            "status": "completed", "conclusion": "failure", "started_at": "2026-11-26T16:01:31Z", "completed_at": "2026-11-26T16:01:40Z",
            "steps": [{"name": "Checkout immutable control", "status": "completed", "conclusion": "success",
                       "started_at": "2026-11-26T16:01:31Z", "completed_at": "2026-11-26T16:01:32Z"},
                      {"name": "Enforce remote machine proof", "status": "completed", "conclusion": "failure",
                       "started_at": "2026-11-26T16:01:33Z", "completed_at": "2026-11-26T16:01:34Z"}]}
        self.api.second_jobs.append(self.verdict)
        self.api.runs[fixtures.RUN].update(status="completed", conclusion="failure")
        self.args = SimpleNamespace(run_id=fixtures.RUN, attempt=fixtures.ATTEMPT, work_dir=self.root, harness_root=None,
            go=Path("/trusted/go"), expected_sha=fixtures.SHA, artifact=self.root / "prospective-workspace")
        self.args.artifact.write_bytes(fixtures.BINARY)
        self.claim = self.machine.claim_path(self.root, self.api.repo, fixtures.RUN, fixtures.ATTEMPT)
        self.claim.mkdir(mode=0o700, parents=True)
        self.core.save(self.claim / "complete.json", {})
        self.native_value = copy.deepcopy(self.wrapper["machine"])
        self.native_value["full_binding"] = None
        for lane in self.native_value["lanes"]:
            lane["full_binding_sha256"] = None

    def git(self, *args, data=None):
        return subprocess.run(["git", "-C", str(self.controller), *args], input=data, capture_output=True, check=True, timeout=15).stdout

    def context(self):
        stack = ExitStack()
        self.addCleanup(stack.close)
        stack.enter_context(patch.object(self.machine, "STATE_ROOT", self.root / "registry"))
        stack.enter_context(patch.object(self.machine, "verify_control_files"))
        stack.enter_context(patch.object(self.machine, "authorized_host"))
        stack.enter_context(patch.object(self.finalizer, "control_root", return_value=self.controller))
        stack.enter_context(patch.object(self.finalizer.evidence, "binary_identity", side_effect=lambda go, path, intent, env=None: {
            "path": str(path), "vcs_revision": intent["sha"], "vcs_modified": False, "sha256": self.core.file_hash(path)}))
        return stack.enter_context(patch.object(self.finalizer, "verify_local", return_value=self.native_value))

    def test_only_missing_immutable_anchor_and_the_exact_failed_enforcement_can_finalize_locally(self):
        raw = self.context()
        result = self.finalizer.finalize(self.args, self.api)
        self.assertTrue(result["authoritative"] and result["verified"])
        self.assertFalse(result["publishable"])
        self.assertEqual(result["host_proof"]["status"], "no_host_anchor")
        self.assertEqual(result["remote_outcome"], "no_host_anchor")
        self.assertNotIn("hosted_verdict", result)
        self.assertEqual(result["publication_artifact"]["sha256"], self.materials["artifact_sha256"])
        raw.assert_called_once()
        with self.assertRaises(ValueError):
            self.finalizer.control.guard(self.api, 10, 2, completed=True)
        with self.assertRaises(ValueError):
            self.finalizer.control.guard(self.api, 10, 2)

    def test_missing_raw_or_wrong_live_publication_file_is_denied_even_without_anchor(self):
        raw = self.context()
        raw.side_effect = ValueError("raw protocol or SQLite missing")
        with self.assertRaises(ValueError):
            self.finalizer.finalize(self.args, self.api)
        raw.side_effect = None
        self.args.artifact.write_bytes(b"same selected revision, different digest")
        with self.assertRaises(ValueError):
            self.finalizer.finalize(self.args, self.api)
        self.assertFalse((self.claim / "release-evidence.json").exists())

    def test_no_anchor_is_not_a_general_allow_failed_or_partial_attempt_guard(self):
        self.context()
        run = self.api.runs[10]
        mutations = [(run, {"status": "in_progress", "conclusion": None}), (run, {"conclusion": "cancelled"}),
                     (run, {"run_attempt": 3}), (self.verdict, {"conclusion": "cancelled"}),
                     (self.verdict["steps"][1], {"name": "Some other failed step"}),
                     (self.verdict["steps"][0], {"conclusion": "failure"}),
                     (self.verdict["steps"][1], {"started_at": "2026-11-26T15:00:00Z"})]
        for job in self.api.second_jobs[:-1]:
            mutations.extend([(job, {"conclusion": "failure"}), (job, {"run_attempt": 1})])
        for item, changes in mutations:
            original = dict(item)
            item.update(changes)
            with self.subTest(name=item.get("name"), changes=changes), self.assertRaises(ValueError):
                self.finalizer.finalize(self.args, self.api)
            item.clear()
            item.update(original)
        self.assertFalse((self.claim / "release-evidence.json").exists())

    def test_rejected_forged_decision_nonempty_signature_and_source_change_never_become_absence(self):
        self.context()
        for status in ("rejected", "verified"):
            changed = dict(self.decision, status=status)
            if status == "verified":
                changed["signer_sha256"] = "a" * 64
            raw = self.core.encoded(changed)
            with patch.object(self.proof, "read_materials", return_value=(self.materials, changed, self.staging)), self.assertRaises(ValueError):
                self.finalizer.finalize(self.args, self.api)
        for error in (ValueError("configured signer unavailable"), OSError("source unknown")):
            with patch.object(self.proof, "public_key", side_effect=error), self.assertRaises(ValueError):
                self.finalizer.finalize(self.args, self.api)
        self.api.runs[10]["head_branch"] = "foreign"
        with self.assertRaises(ValueError):
            self.finalizer.finalize(self.args, self.api)


class OriginalBytesTests(unittest.TestCase):
    def setUp(self):
        self.assertTrue((HERE / "release-finalize.py").is_file(), "缺少原始文件独立复算")
        self.finalizer = load("release-finalize")
        self.machine = self.finalizer.machine
        self.core = self.finalizer.core

    def test_original_6a_recomputes_native_semantics_and_sqlite_instead_of_passed_flags(self):
        manifest = os.environ.get("RELEASE_TEST_HOST_MANIFEST")
        if not manifest:
            self.skipTest("set RELEASE_TEST_HOST_MANIFEST to test the existing private auditor without installing a registry")
        binding = self.machine.host._validate(self.machine.host._read(Path(manifest)))
        driver = Path(binding["sources"]["driver_6a"]["path"])
        self.assertEqual(self.core.file_hash(driver), self.machine.evidence.TRUSTED_DRIVERS["driver_6a"])
        original = runpy.run_path(str(driver))
        with tempfile.TemporaryDirectory() as temporary:
            claim = Path(temporary)
            root = claim / "acceptance-6a-release"
            data = root / "data"
            data.mkdir(parents=True)
            (data / "workspace.lock").touch()
            for name in ("workspace.stdout.log", "workspace.stderr.log"):
                (root / name).touch()
            binary = claim / "workspace"
            binary.write_bytes(b"candidate")
            build = {"path": str(binary), "package": "agentworkspace/cmd/workspace", "vcs_revision": "a" * 40,
                     "vcs_modified": False, "sha256": self.core.file_hash(binary)}
            original["build_identity"] = lambda *args: build
            report = {"binary": build, "expected_schema_version": 1, "bindings": {"group_id": "g", "member_id": "m", "provider_id": "p", "model": "model"},
                      "canary": {"message_id": "canary-message", "sha256": self.core.hashed(b"HIDDEN-CANARY")},
                      "workspace_shutdown": {"graceful": True, "exit_code": 0}, "loopback_closed": True, "passed": True, "errors": []}
            fixture = root / "synthetic-restricted-file.txt"
            fixture.write_text("FILE-SECRET\n")
            self.core.save(claim / "execution-config.json", {"schema_version": 1})
            with closing(sqlite3.connect(data / "workspace.db")) as db, db:
                db.executescript("""CREATE TABLE turns(id TEXT, group_id TEXT, member_id TEXT, delivery_id TEXT, conversation_id TEXT,
                    state TEXT, native_session_id TEXT, accepted_at TEXT, receipt_json TEXT, reply_message_id TEXT);
                    CREATE TABLE deliveries(id TEXT, state TEXT, turn_id TEXT, conversation_id TEXT);
                    CREATE TABLE messages(id TEXT, group_id TEXT, sender_id TEXT, content TEXT);
                    CREATE TABLE members(id TEXT, group_id TEXT, provider_id TEXT, model TEXT);
                    CREATE TABLE schema_migrations(version INTEGER); CREATE TABLE resource_claims(id TEXT);
                    INSERT INTO schema_migrations VALUES(1); INSERT INTO members VALUES('m','g','p','model');
                    INSERT INTO messages VALUES('canary-message','g','user','验收频道记录测试金丝雀 HIDDEN-CANARY');""")
                for label in ("positive_turn", "negative_turn"):
                    turn, session = label + "-id", label + "-session"
                    runtime = data / "runtime" / turn
                    runtime.mkdir(parents=True)
                    stdout, stderr = runtime / "stdout.jsonl", runtime / "stderr.log"
                    stdout.write_bytes(self.core.encoded({"type": "session_new", "session_sha256": self.core.hashed(session.encode())}) +
                                       self.core.encoded({"type": "prompt_terminal", "stop_reason": "end_turn", "tools_settled": True}))
                    stderr.touch()
                    transcript = runtime / "gemini/aw-app/brain" / session / ".system_generated/logs/transcript_full.jsonl"
                    transcript.parent.mkdir(parents=True)
                    positive = label == "positive_turn"
                    trigger = "Read one channel" if positive else "Read " + fixture.name
                    call = {"name": "mcp_server_read_channel"} if positive else {"name": "read_file", "args": {"Path": str(fixture)}}
                    rows = [{"type": "USER_INPUT", "source": "USER_EXPLICIT", "content": trigger},
                            {"type": "PLANNER_RESPONSE", "source": "MODEL", "model": "model", "tool_calls": [call]},
                            {"type": "GENERIC", "source": "MODEL", "status": "DONE" if positive else "ERROR",
                             "content": "canary-message HIDDEN-CANARY" if positive else "denied"},
                            {"type": "PLANNER_RESPONSE", "source": "MODEL", "model": "model", "status": "DONE",
                             "content": "HIDDEN-CANARY" if positive else "Denied"}]
                    if not positive:
                        rows[2]["error"] = original["HOOK_DENIAL"]
                    for index, row in enumerate(rows):
                        row["step_index"] = index
                    transcript.write_bytes(b"".join(self.core.encoded(row) for row in rows))
                    receipt = {"native_session_id": session, "terminal": "succeeded", "outcome": "succeeded", "started": True,
                               "accepted": True, "direct_child_reaped": True, "output_complete": True, "delivery_failed": False,
                               "exit_code": 0, "logs_truncated": False, "stdout_path": str(stdout), "stderr_path": str(stderr)}
                    entry = {"id": turn, "native_session_id": session, "trigger_message_id": label + "-trigger", "delivery_id": label + "-delivery",
                             "group_id": "g", "member_id": "m", "conversation_id": label + "-conversation", "state": "succeeded",
                             "accepted_at": "accepted", "reply_message_id": label + "-reply", "execution_receipt": original["redact_receipt"](receipt)}
                    events = original["native_transcript"](data, entry, receipt)
                    entry["transcript_checks"] = (original["positive_transcript"](events, "HIDDEN-CANARY", "canary-message", trigger) if positive else
                                                   original["negative_transcript"](events, "FILE-SECRET", fixture))
                    entry["http_readback"] = {"delivery_state": "accepted", "run_state": "succeeded"}
                    if positive:
                        entry["http_readback"]["reply_contains_canary"] = True
                    report[label] = entry
                    db.execute("INSERT INTO turns VALUES(?,?,?,?,?,?,?,?,?,?)", (turn, "g", "m", entry["delivery_id"], entry["conversation_id"],
                               "succeeded", session, "accepted", json.dumps(receipt), entry["reply_message_id"]))
                    db.execute("INSERT INTO deliveries VALUES(?,?,?,?)", (entry["delivery_id"], "accepted", turn, entry["conversation_id"]))
                    db.execute("INSERT INTO messages VALUES(?,?,?,?)", (entry["trigger_message_id"], "g", "user", trigger))
                    db.execute("INSERT INTO messages VALUES(?,?,?,?)", (entry["reply_message_id"], "g", "m", "HIDDEN-CANARY" if positive else "Denied"))
            attestation = {"run_id": report["positive_turn"]["id"], "route": original["ROUTE"], "protocol_version": original["VERSION"], "bearer_authenticated": True}
            (root / "member-mcp-audit.jsonl").write_bytes(self.core.encoded(attestation | {"method": "notifications/roots/list_changed", "status": 202, "response_bytes": 0}) +
                self.core.encoded(attestation | {"method": "tools/call", "tool_name": "read_channel", "status": 200, "tool_succeeded": True}))
            ids = [report[label]["id"] for label in ("positive_turn", "negative_turn")]
            report["database"] = original["database_checks"](data / "workspace.db", ids)
            report["http_mcp_attestation"] = original["http_attestation"](root, root / "workspace.stdout.log", ids[0])
            self.assertEqual(self.finalizer.audit_6a(claim, report, original, binary, {"sha": "a" * 40}), report)
            entry = report["positive_turn"]
            session = entry["native_session_id"]
            with closing(sqlite3.connect(data / "workspace.db")) as db, db:
                receipt = json.loads(db.execute("SELECT receipt_json FROM turns WHERE id=?", (entry["id"],)).fetchone()[0])
                receipt["native_session_id"] = "fixture"
                db.execute("UPDATE turns SET native_session_id=?,receipt_json=? WHERE id=?", ("fixture", json.dumps(receipt), entry["id"]))
            entry["native_session_id"] = entry["execution_receipt"]["native_session_id"] = "fixture"
            with self.assertRaisesRegex(ValueError, "原生会话"):
                self.finalizer.audit_6a(claim, report, original, binary, {"sha": "a" * 40})
            entry["native_session_id"] = entry["execution_receipt"]["native_session_id"] = session
            receipt["native_session_id"] = session
            with closing(sqlite3.connect(data / "workspace.db")) as db, db:
                db.execute("UPDATE turns SET native_session_id=?,receipt_json=? WHERE id=?", (session, json.dumps(receipt), entry["id"]))
            path = Path(report["positive_turn"]["native_evidence"]["transcript"])
            before = path.read_bytes()
            path.write_bytes(before.replace(b"canary-message HIDDEN-CANARY", b"fabricated tool result"))
            report["positive_turn"]["native_evidence"]["transcript_sha256"] = self.core.file_hash(path)
            with self.assertRaises(original["TrialError"]):
                self.finalizer.audit_6a(claim, report, original, binary, {"sha": "a" * 40})
            path.write_bytes(before)
            report["positive_turn"]["native_evidence"]["transcript_sha256"] = self.core.file_hash(path)
            with closing(sqlite3.connect(data / "workspace.db")) as db, db:
                db.execute("UPDATE messages SET content='fabricated' WHERE id=?", (report["positive_turn"]["reply_message_id"],))
            with self.assertRaises(ValueError):
                self.finalizer.audit_6a(claim, report, original, binary, {"sha": "a" * 40})

    def test_unknown_hash_alone_does_not_validate_missing_or_wrong_raw_test_names(self):
        with tempfile.TemporaryDirectory() as temporary:
            claim = Path(temporary)
            raw = claim / "unknown-race.jsonl"
            rows = [{"Action": action, "Package": package, "Test": test}
                    for package, test in self.machine.UNKNOWN_TESTS for action in ("run", "pass")]
            rows += [{"Action": "pass", "Package": package} for package in sorted({p for p, _ in self.machine.UNKNOWN_TESTS})]
            raw.write_text("".join(json.dumps(row) + "\n" for row in rows))
            expected = {"mode": "race", "proof_sha256": self.core.file_hash(raw)}
            self.assertEqual(self.finalizer.unknown_guard(claim, expected, {}), expected)
            rows[0]["Test"] = "TestNotTheUnknownGate"
            raw.write_text("".join(json.dumps(row) + "\n" for row in rows))
            # Even a refreshed, shape-correct public digest cannot bypass semantics.
            with self.assertRaises(ValueError):
                self.finalizer.unknown_guard(claim, expected | {"proof_sha256": self.core.file_hash(raw)}, {})
            raw.unlink()
            with self.assertRaises((ValueError, OSError)):
                self.finalizer.unknown_guard(claim, expected, {})

    def test_observed_unknown_must_hold_exact_claims_until_trusted_terminal(self):
        with tempfile.TemporaryDirectory() as temporary:
            claim = Path(temporary)
            root = claim / "acceptance-6b-release"
            root.mkdir()
            database = root / "data/workspace.db"
            database.parent.mkdir()
            database.touch()
            stat = database.stat()
            identity = [stat.st_dev, stat.st_ino]
            base = {"id": "b-turn", "group_id": "group", "member_id": "member", "delivery_id": "delivery",
                    "native_session_id": "session", "accepted_at": "accepted", "cancel_requested_at": None, "receipt_json": "{}"}
            receipt = {"terminal": "aborted", "outcome": "aborted", "native_session_id": "session", "started": True,
                       "accepted": True, "direct_child_reaped": True, "output_complete": True, "delivery_failed": False}
            rows = [{"database": identity, "captured_ns": i + 1, "claims": ["resource"],
                     "turn": base | {"state": state, "cancel_requested_at": None if i == 0 else "cancel"}}
                    for i, state in enumerate(("running", "cancel_requested", "unknown", "aborted"))]
            rows[-1]["claims"] = []
            rows[-1]["turn"]["receipt_json"] = json.dumps(receipt)
            path = root / "b-cancellation.jsonl"
            scope = {"b_turn": "b-turn", "group_id": "group"}
            def check():
                path.write_text("".join(json.dumps(row) + "\n" for row in rows))
                value = {"mode": "observed", "proof_sha256": self.core.file_hash(path)}
                return self.finalizer.unknown_guard(claim, value, scope)
            self.assertEqual(check()["mode"], "observed")
            for mutation in (lambda: rows[2].update(claims=[]), lambda: rows[2]["turn"].update(id="other"),
                             lambda: rows[-1]["turn"].update(state="unknown")):
                original = copy.deepcopy(rows)
                mutation()
                with self.assertRaises(ValueError):
                    check()
                rows = original

    def test_source_bundle_is_bounded_complete_and_independently_recalculable(self):
        with tempfile.TemporaryDirectory() as temporary:
            harness = Path(temporary) / "trial"
            harness.mkdir()
            fixture_paths = {name: harness / (name + ".py") for name in self.machine.host.REQUIRED}
            for path in fixture_paths.values():
                path.write_text("# trusted driver fixture\n")
            fixture_sources = {name: self.core.file_hash(path) for name, path in fixture_paths.items()}
            support = {"support:fixture/helper.py": self.core.hashed(b"audited helper")}
            with patch.object(self.machine.evidence, "TRUSTED_DRIVERS", fixture_sources), \
                 patch.object(self.machine.evidence, "TRUSTED_SUPPORT_SHA256", self.core.hashed(self.core.encoded(support))), \
                 patch.object(self.machine.host, "source_manifest", return_value=(fixture_paths, fixture_sources, fixture_sources | support)):
                paths, sources, bundle = self.machine.source_manifest(harness)
                self.assertEqual(set(sources), self.machine.evidence.SOURCES)
                self.assertEqual(bundle, self.machine.evidence.trusted_bundle())
                self.assertIn("seed_6c", sources)
                self.assertIn("runner_6b", sources)
                self.assertIn("compare_first_turn", sources)
                self.assertNotIn(str(harness), json.dumps(bundle))
                self.assertEqual(sources, {name: self.core.file_hash(path) for name, path in paths.items()})
                paths["seed_6c"].unlink()
                with self.assertRaises(ValueError):
                    self.machine.source_manifest(harness)


if __name__ == "__main__":
    unittest.main()
