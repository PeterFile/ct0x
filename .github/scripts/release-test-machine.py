#!/usr/bin/env python3
"""Private bridge tests use synthetic files only; no driver is executed."""
import hashlib
import io
from contextlib import closing, redirect_stdout
import importlib.util
import json
import os
import sqlite3
from pathlib import Path
import tempfile
import unittest
from types import SimpleNamespace
from unittest.mock import patch

HERE = Path(__file__).resolve().parent
SHA, BINARY_HASH = "a" * 40, "b" * 64


def load():
    path = HERE / "release-machine.py"
    assert path.is_file(), "缺少本地原生验收桥接"
    spec = importlib.util.spec_from_file_location("release_machine", path)
    result = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(result)
    return result


class LockTests(unittest.TestCase):
    def setUp(self):
        self.state = tempfile.TemporaryDirectory()
        self.addCleanup(self.state.cleanup)
        self.machine = load()
        self.machine.STATE_ROOT = Path(self.state.name) / "state"

    def test_changing_scratch_cannot_bypass_global_lock_or_permanent_attempt(self):
        machine = self.machine
        with tempfile.TemporaryDirectory() as first, tempfile.TemporaryDirectory() as second:
            a, b = Path(first), Path(second)
            a.chmod(0o700)
            b.chmod(0o700)
            machine.reserve(a, "owner/repo", 10, 1)
            with self.assertRaises(ValueError):
                machine.reserve(b, "owner/repo", 11, 1)
            self.assertFalse(machine.claim_path(b, "owner/repo", 11, 1).exists())
            self.assertFalse(machine.claim_path(machine.STATE_ROOT, "owner/repo", 11, 1).exists())
            (machine.STATE_ROOT / "machine-active/owner.json").unlink()
            (machine.STATE_ROOT / "machine-active").rmdir()
            with self.assertRaises(ValueError):
                machine.reserve(b, "owner/repo", 10, 1)
            self.assertTrue(machine.reserve(b, "owner/repo", 11, 1).exists())

    def test_atomic_attempt_claim_and_machine_lock_survive_interruption(self):
        machine = self.machine
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            root.chmod(0o700)
            claim = machine.reserve(root, "owner/repo", 10, 1)
            self.assertTrue((claim / "reserved.json").is_file())
            with self.assertRaises(ValueError):
                machine.reserve(root, "owner/repo", 10, 1)
            with self.assertRaises(ValueError):
                machine.reserve(root, "owner/repo", 11, 1)
            self.assertTrue((machine.STATE_ROOT / "machine-active").is_dir())
            # A process restart cannot implicitly steal an unknown execution.
            restarted = load()
            restarted.STATE_ROOT = machine.STATE_ROOT
            with self.assertRaises(ValueError):
                restarted.reserve(root, "owner/repo", 10, 2)

    def test_busy_machine_does_not_leave_a_permanent_attempt_claim(self):
        machine = self.machine
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            root.chmod(0o700)
            machine.reserve(root, "owner/repo", 10, 1)
            with self.assertRaises(ValueError):
                machine.reserve(root, "owner/repo", 11, 1)
            self.assertFalse(machine.claim_path(root, "owner/repo", 11, 1).exists(), "busy refusal orphaned B's claim")
            (machine.STATE_ROOT / "machine-active/owner.json").unlink()
            (machine.STATE_ROOT / "machine-active").rmdir()
            self.assertTrue(machine.reserve(root, "owner/repo", 11, 1).is_dir())

    def test_native_environment_is_an_allowlist_not_a_token_denylist(self):
        machine = load()
        hostile = {"HOME": "/private/operator-home", "PATH": "/private/token/bin:/usr/bin",
                   "GITHUB_TOKEN": "secret", "CUSTOM_SERVICE_CREDENTIAL": "secret", "OPENAI_API_KEY": "secret",
                   "ANTHROPIC_API_KEY": "secret", "PYTHONPATH": "/private/hook", "DYLD_INSERT_LIBRARIES": "/private/hook"}
        with patch.dict(os.environ, hostile, clear=True):
            env = machine.native_environment()
        self.assertEqual(env["HOME"], hostile["HOME"])
        self.assertEqual(env["PATH"], "/usr/bin:/bin:/usr/sbin:/sbin")
        self.assertFalse(set(hostile) - {"HOME", "PATH"} & set(env))

    def test_private_work_directory_is_required(self):
        machine = load()
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            root.chmod(0o755)
            with self.assertRaises(ValueError):
                machine.reserve(root, "owner/repo", 10, 1)

    def test_actual_download_sets_mode_and_rehashes_bytes(self):
        machine = load()
        with tempfile.TemporaryDirectory() as temporary:
            binary = Path(temporary) / "workspace"
            machine.install_binary(binary, b"actual artifact bytes", hashlib.sha256(b"actual artifact bytes").hexdigest())
            self.assertEqual(binary.stat().st_mode & 0o777, 0o700)
            self.assertEqual(binary.read_bytes(), b"actual artifact bytes")
            with self.assertRaises(ValueError):
                machine.install_binary(Path(temporary) / "wrong", b"changed", "0" * 64)


class NativeNormalizationTests(unittest.TestCase):
    def fixture(self, root):
        proofs = []
        report = {"passed": True, "errors": [], "binary": {"vcs_revision": SHA, "vcs_modified": False, "sha256": BINARY_HASH},
                  "workspace_shutdown": {"graceful": True, "exit_code": 0}, "loopback_closed": True,
                  "http_mcp_attestation": {"roots_list_changed_202_empty": True, "bearer_http_mcp_2025_06_18_read_channel": True}}
        for label in ("positive_turn", "negative_turn"):
            turn, session = label + "-id", label + "-session"
            runtime = root / "data" / "runtime" / turn
            runtime.mkdir(parents=True)
            protocol = runtime / "stdout.jsonl"
            protocol.write_text(json.dumps({"type": "session_new", "session_sha256": hashlib.sha256(session.encode()).hexdigest()}) + "\n" +
                                json.dumps({"type": "prompt_terminal", "stop_reason": "end_turn", "tools_settled": True}) + "\n")
            transcript = runtime / "transcript_full.jsonl"
            transcript.write_text('{"type":"PLANNER_RESPONSE"}\n')
            report[label] = {"id": turn, "native_session_id": session,
                             "execution_receipt": {"native_session_id": session, "started": True, "accepted": True,
                                                   "direct_child_reaped": True, "output_complete": True, "delivery_failed": False,
                                                   "terminal": "succeeded", "outcome": "succeeded", "exit_code": 0},
                             "native_evidence": {"transcript": str(transcript), "transcript_sha256": hashlib.sha256(transcript.read_bytes()).hexdigest(),
                                                 "stdout_sha256": hashlib.sha256(protocol.read_bytes()).hexdigest()}}
        report["positive_turn"]["transcript_checks"] = {"canary_absent_from_prompt": True, "canary_recovered_from_tool": True}
        report["negative_turn"]["transcript_checks"] = {"pretool_hard_denied": True, "synthetic_file_canary_absent": True}
        return report

    def test_normalization_requires_actual_native_file_hashes_not_passed_boolean(self):
        machine = load()
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            report = self.fixture(root)
            normalized = machine.normalize_6a(report, root, SHA, BINARY_HASH)
            self.assertEqual(len(normalized["proofs"]), 2)
            self.assertNotIn(str(root), json.dumps(normalized))
            Path(report["positive_turn"]["native_evidence"]["transcript"]).write_text("modified native proof")
            with self.assertRaises(ValueError):
                machine.normalize_6a(report, root, SHA, BINARY_HASH)
            with self.assertRaises(ValueError):
                machine.normalize_6a({"passed": True}, root, SHA, BINARY_HASH)

    def test_rehashed_protocol_without_native_settled_terminal_is_not_proof(self):
        machine = load()
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            report = self.fixture(root)
            entry = report["positive_turn"]
            protocol = root / "data/runtime" / entry["id"] / "stdout.jsonl"
            protocol.write_text(protocol.read_text().splitlines()[0] + "\n")
            entry["native_evidence"]["stdout_sha256"] = machine.core.file_hash(protocol)
            with self.assertRaises(ValueError):
                machine.normalize_6a(report, root, SHA, BINARY_HASH)

    def test_audit_commit_mismatch_or_failed_positive_assertion_is_rejected(self):
        machine = load()
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            report = self.fixture(root)
            for change in [{"vcs_revision": "c" * 40}, {"sha256": "c" * 64}, {"vcs_modified": True}]:
                original = dict(report["binary"])
                report["binary"].update(change)
                with self.assertRaises(ValueError):
                    machine.normalize_6a(report, root, SHA, BINARY_HASH)
                report["binary"] = original
            report["positive_turn"]["transcript_checks"]["canary_recovered_from_tool"] = False
            with self.assertRaises(ValueError):
                machine.normalize_6a(report, root, SHA, BINARY_HASH)

    def test_6b_normalizes_actual_sqlite_turns_and_rejects_changed_native_bytes(self):
        import sqlite3
        machine = load()
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            (root / "data").mkdir()
            database = root / "data/workspace.db"
            required = machine.evidence.LANE_CHECKS["6b"]
            report = {"passed": True, "build": {"vcs_revision": SHA, "vcs_modified": False, "sha256": BINARY_HASH},
                      "checks": dict.fromkeys(required, True), "database": str(database), "native": {}}
            with closing(sqlite3.connect(database)) as db, db:
                db.execute("CREATE TABLE turns (id TEXT,native_session_id TEXT,state TEXT,receipt_json TEXT)")
                db.execute("CREATE TABLE task_attempts (id TEXT,turn_id TEXT)")
                for role in ("b", "a2", "c", "constraint", "handback", "answer"):
                    turn, session = role + "-turn", role + "-session"
                    runtime = root / "data/runtime" / turn
                    transcript = runtime / "gemini/aw-app/brain" / session / ".system_generated/logs/transcript_full.jsonl"
                    transcript.parent.mkdir(parents=True)
                    transcript.write_text('{"type":"PLANNER_RESPONSE"}\n')
                    protocol = runtime / "stdout.jsonl"
                    protocol.write_text(json.dumps({"type": "session_new", "session_sha256": hashlib.sha256(session.encode()).hexdigest()}) + "\n" +
                                        json.dumps({"type": "prompt_terminal", "stop_reason": "end_turn", "tools_settled": True}) + "\n")
                    receipt = {"native_session_id": session, "started": True, "accepted": True, "direct_child_reaped": True,
                               "output_complete": True, "delivery_failed": False, "outcome": "aborted" if role == "b" else "succeeded"}
                    receipt["terminal"] = receipt["outcome"]
                    db.execute("INSERT INTO turns VALUES (?,?,?,?)", (turn, session, receipt["outcome"], json.dumps(receipt)))
                    db.execute("INSERT INTO task_attempts VALUES (?,?)", (role + "-attempt", turn))
                    report["native"][role] = {"session_bound": True, "model_matches": True, "matched_call": True,
                                              "protocol_sha256": machine.core.file_hash(protocol), "transcript_sha256": machine.core.file_hash(transcript)}
            result = machine.normalize_6b(report, root, SHA, BINARY_HASH)
            self.assertEqual((len(result["proofs"]), len(result["attempt_ids"])), (6, 6))
            missing = report["checks"].pop("same_trial_database_and_final_b_receipt")
            with self.assertRaises(ValueError):
                machine.normalize_6b(report, root, SHA, BINARY_HASH)
            report["checks"]["same_trial_database_and_final_b_receipt"] = missing
            protocol.write_text("changed")
            with self.assertRaises(ValueError):
                machine.normalize_6b(report, root, SHA, BINARY_HASH)

    def test_6c_pair_requires_both_native_fixtures_and_all_original_positive_assertions(self):
        machine = load()
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            required = {key.removeprefix("0_") for key in machine.evidence.LANE_CHECKS["6c"] if key.startswith("0_")}
            report = {"passed": True, "revision": SHA, "binary_sha256": BINARY_HASH, "cases": [],
                      "paired": dict.fromkeys(("distinct_native_session", "distinct_member_id", "distinct_port", "distinct_database_inode", "same_related_initial_context"), True)}
            for count in (0, 200):
                turn, session = f"turn-{count}", f"session-{count}"
                runtime = root / f"acceptance-{count:03d}" / "data/runtime" / turn
                runtime.mkdir(parents=True)
                protocol = runtime / "stdout.jsonl"
                protocol.write_text(json.dumps({"type": "session_new", "session_sha256": hashlib.sha256(session.encode()).hexdigest()}) + "\n" +
                                    json.dumps({"type": "prompt_terminal", "stop_reason": "end_turn", "tools_settled": True}) + "\n")
                transcript = runtime / "transcript.jsonl"
                transcript.write_text('{"type":"PLANNER_RESPONSE"}\n')
                report["cases"].append({"noise": count, "turn_id": turn, "native_session": session, "transcript": str(transcript),
                                        "protocol_sha256": machine.core.file_hash(protocol), "transcript_sha256": machine.core.file_hash(transcript),
                                        "checks": dict.fromkeys(required, True)})
            self.assertEqual(len(machine.normalize_6c(report, root, SHA, BINARY_HASH)["proofs"]), 2)
            missing = report["cases"][0]["checks"].pop("prompt_bound_to_fresh_member")
            with self.assertRaises(ValueError):
                machine.normalize_6c(report, root, SHA, BINARY_HASH)
            report["cases"][0]["checks"]["prompt_bound_to_fresh_member"] = missing
            report["paired"]["same_related_initial_context"] = False
            with self.assertRaises(ValueError):
                machine.normalize_6c(report, root, SHA, BINARY_HASH)
    def test_all_lanes_revalidate_actual_protocol_and_transcript_bytes(self):
        machine = load()
        with tempfile.TemporaryDirectory() as temporary:
            base = Path(temporary)
            a, b, c = (base / name for name in ("6a", "6b", "6c"))
            report_a = self.fixture(a)
            proof_paths = {"6a": [a / "data/runtime/positive_turn-id/stdout.jsonl", Path(report_a["positive_turn"]["native_evidence"]["transcript"])]}
            def native(root, turn, session):
                runtime = root / "data/runtime" / turn
                runtime.mkdir(parents=True, exist_ok=True)
                protocol = runtime / "stdout.jsonl"
                protocol.write_bytes(machine.core.encoded({"type": "session_new", "session_sha256": machine.core.hashed(session.encode())}) +
                                     machine.core.encoded({"type": "prompt_terminal", "stop_reason": "end_turn", "tools_settled": True}))
                transcript = runtime / "gemini/aw-app/brain" / session / ".system_generated/logs/transcript_full.jsonl"
                transcript.parent.mkdir(parents=True)
                transcript.write_bytes(machine.core.encoded({"type": "PLANNER_RESPONSE", "content": "synthetic native fixture"}))
                return protocol, transcript
            (b / "data").mkdir(parents=True)
            report_b = {"passed": True, "build": {"vcs_revision": SHA, "vcs_modified": False, "sha256": BINARY_HASH},
                        "checks": {key: True for key in machine.evidence.LANE_CHECKS["6b"]}, "database": str(b / "data/workspace.db"), "native": {}}
            with sqlite3.connect(b / "data/workspace.db") as db:
                db.executescript("CREATE TABLE turns(id TEXT,native_session_id TEXT,state TEXT,receipt_json TEXT); CREATE TABLE task_attempts(id TEXT,turn_id TEXT);")
                for role in ("b", "a2", "c", "constraint", "handback", "answer"):
                    turn, session = "6b-turn-" + role, "6b-session-" + role
                    protocol, transcript = native(b, turn, session)
                    state = "aborted" if role == "b" else "succeeded"
                    receipt = {"native_session_id": session, "terminal": state, "outcome": state, "started": True, "accepted": True,
                               "direct_child_reaped": True, "output_complete": True, "delivery_failed": False}
                    db.execute("INSERT INTO turns VALUES(?,?,?,?)", (turn, session, state, json.dumps(receipt)))
                    db.execute("INSERT INTO task_attempts VALUES(?,?)", ("attempt-" + role, turn))
                    report_b["native"][role] = {"session_bound": True, "model_matches": True, "matched_call": True,
                        "protocol_sha256": machine.core.file_hash(protocol), "transcript_sha256": machine.core.file_hash(transcript)}
                    proof_paths.setdefault("6b", [protocol, transcript])
            db.close()
            report_c = {"passed": True, "revision": SHA, "binary_sha256": BINARY_HASH,
                        "paired": {name.removeprefix("pair_"): True for name in machine.evidence.LANE_CHECKS["6c"] if name.startswith("pair_")}, "cases": []}
            for noise in (0, 200):
                turn, session = f"6c-turn-{noise}", f"6c-session-{noise}"
                protocol, transcript = native(c / f"acceptance-{noise:03d}", turn, session)
                report_c["cases"].append({"noise": noise, "turn_id": turn, "native_session": session, "transcript": str(transcript),
                    "protocol_sha256": machine.core.file_hash(protocol), "transcript_sha256": machine.core.file_hash(transcript),
                    "checks": {name.removeprefix(str(noise) + "_"): True for name in machine.evidence.LANE_CHECKS["6c"] if name.startswith(str(noise) + "_")}})
                proof_paths.setdefault("6c", [protocol, transcript])
            for name, normalize, report, root in (("6a", machine.normalize_6a, report_a, a), ("6b", machine.normalize_6b, report_b, b), ("6c", machine.normalize_6c, report_c, c)):
                self.assertEqual(normalize(report, root, SHA, BINARY_HASH)["name"], name)
                for path in proof_paths[name]:
                    before = path.read_bytes()
                    path.write_bytes(before + b"tampered underlying bytes\n")
                    with self.subTest(lane=name, file=path.name), self.assertRaises(ValueError):
                        normalize(report, root, SHA, BINARY_HASH)
                    path.write_bytes(before)
                with self.assertRaises(ValueError):
                    normalize(report, root, "c" * 40, BINARY_HASH)
            with sqlite3.connect(b / "data/workspace.db") as db:
                db.execute("UPDATE turns SET state='unknown' WHERE id='6b-turn-b'")
            db.close()
            with self.assertRaises(ValueError):
                machine.normalize_6b(report_b, b, SHA, BINARY_HASH)

    def test_unknown_fallback_requires_all_four_actual_race_test_passes(self):
        machine = load()
        names = machine.UNKNOWN_TESTS
        rows = [{"Action": action, "Test": name, "Package": package} for package, name in names for action in ("run", "pass")]
        rows += [{"Action": "pass", "Package": package} for package in sorted({package for package, _ in names})]
        self.assertTrue(machine.verify_unknown_race(rows))
        for missing in range(len(rows)):
            with self.assertRaises(ValueError):
                machine.verify_unknown_race(rows[:missing] + rows[missing + 1:])
        with self.assertRaises(ValueError):
            machine.verify_unknown_race(rows + [{"Action": "fail", "Package": "agentworkspace/internal/provider"}])


class SubmissionTests(unittest.TestCase):
    def test_cli_wait_is_not_a_successful_submission_exit(self):
        machine = load()
        with patch.object(machine.control, "AdmissionAPI"), patch.object(machine, "submit", return_value={"status": "waiting"}), \
             patch.object(machine.sys, "argv", ["release-machine.py", "finalize-submit", "--repo", "owner/repo", "--run-id", "10",
                                                "--attempt", "1", "--work-dir", "/private/scratch"]), redirect_stdout(io.StringIO()):
            self.assertEqual(machine.main(), 75)

    def test_real_204_is_persisted_and_second_submit_never_posts_again(self):
        self.exercise(None)

    def test_requested_200_run_details_are_persisted(self):
        self.exercise({"workflow_run_id": 99, "html_url": "https://github.com/owner/repo/actions/runs/99",
                       "run_url": "https://api.github.com/repos/owner/repo/actions/runs/99"})

    def test_ambiguous_post_is_reobserved_read_only_without_duplicate_post(self):
        self.exercise(OSError("response lost"), reconcile=True)

    def test_unexpected_dispatch_body_requires_native_reobservation_not_assumed_success(self):
        self.exercise([], reconcile=True)

    def test_unknown_response_can_be_reconciled_later_without_second_post(self):
        self.exercise(OSError("response lost"), reconcile=True, first_unknown=True)

    def exercise(self, response, reconcile=False, first_unknown=False):
        machine = load()
        self.assertTrue(hasattr(machine, "reconcile_submission"), "缺少持久提交结果和只读对账")
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            root.chmod(0o700)
            machine.STATE_ROOT = root / "state"
            claim = machine.reserve(root, "owner/repo", 10, 1)
            value = {"artifact_sha256": machine.core.hashed(b"binary"), "lanes": []}
            machine.core.save(claim / "receipt.json", value)
            machine.core.save(claim / "complete.json", {"receipt_sha256": machine.core.file_hash(claim / "receipt.json")})
            (claim / "workspace").write_bytes(b"binary")
            posts = []
            class API:
                repo, prefix = "owner/repo", "/repos/owner/repo"
                status = "in_progress"
                def run(self, run_id):
                    return {"run_attempt": 1, "status": self.status, "conclusion": "success" if self.status == "completed" else None}
                def request(self, method, path, data):
                    posts.append(data)
                    if isinstance(response, Exception):
                        raise response
                    return response
            args = SimpleNamespace(work_dir=root, run_id=10, attempt=1)
            intent = {"run_id": 10, "run_attempt": 1, "workflow_sha": SHA}
            preparer = SimpleNamespace(prepare_submission=lambda *args, **kwargs: value)
            signer = SimpleNamespace(sign_payload=lambda *args: "")
            with patch.object(machine.core, "load_script", side_effect=lambda name: preparer if name == "release-finalize" else signer), \
                 patch.object(machine.control, "guard", return_value=intent), \
                 patch.object(machine, "ensure_active", return_value=intent) as active, \
                 patch.object(machine, "verify_control_files"), \
                 patch.object(machine.evidence, "target", return_value=(value, b"binary", {})), \
                 patch.object(machine.evidence, "bind_machine"), patch.object(machine, "reports", return_value=[]), \
                 patch.object(machine, "reconcile_submission", return_value={"accepted": True, "intake_run_id": 99}) as observed:
                if first_unknown:
                    observed.side_effect = [{"accepted": None, "status": "unknown"}, {"accepted": True, "intake_run_id": 99}]
                    self.assertEqual(machine.submit(args, API())["status"], "unknown")
                    self.assertFalse((claim / "submission-result.json").exists())
                    active.side_effect = AssertionError("reconcile a stopped owner without ensure_active")
                api = API()
                result = machine.submit(args, api)
                self.assertEqual(result["accepted"], True)
                api.status = "completed"
                self.assertEqual(machine.submit(args, api), result)
                self.assertEqual(len(posts), 1)
                self.assertIs(posts[0]["return_run_details"], True)
                self.assertEqual(posts[0]["inputs"]["machine_signature"], "")
                self.assertEqual(observed.call_count, 2 if first_unknown else 1 if reconcile else 0)
                active.assert_called_once()
                self.assertTrue((claim / "submission-result.json").is_file())


class ActiveAdmissionTests(unittest.TestCase):
    def test_native_machine_entry_uses_corroborated_first_owner_activity(self):
        machine = load()
        fixtures = machine.core.load_script("test-release-control")
        api = fixtures.queued_owner("Release Full / Go ordinary and remaining race")
        try:
            self.assertEqual(machine.ensure_active(api, 10, 1), api.intent)
        except ValueError as error:
            self.fail("machine entry must accept authenticated queued-owner native activity: " + str(error))
        api.job_rows.pop()
        with self.assertRaises(ValueError):
            machine.ensure_active(api, 10, 1)
        self.assertTrue(all(method == "GET" for method, _ in api.calls))
        self.assertEqual(api.posts, 0)


class HostAdapterTests(unittest.TestCase):
    def test_source_manifest_uses_private_binding_and_verifies_auxiliary_bundle(self):
        machine = load()
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            paths = {name: root / (name + ".py") for name in machine.host.REQUIRED}
            for path in paths.values():
                path.write_bytes(b"# audited fixture\n")
            public = {name: machine.core.file_hash(path) for name, path in paths.items()}
            support = {"support:fixture/helper.py": machine.core.hashed(b"helper")}
            with patch.object(machine.host, "source_manifest", return_value=(paths, public, public | support)), \
                 patch.object(machine.evidence, "TRUSTED_DRIVERS", public), \
                 patch.object(machine.evidence, "TRUSTED_SUPPORT_SHA256", machine.core.hashed(machine.core.encoded(support))):
                actual_paths, sources, bundle = machine.source_manifest(root)
                self.assertEqual(set(sources), machine.evidence.SOURCES)
                self.assertEqual(set(actual_paths), machine.evidence.SOURCES)
                self.assertEqual(bundle, machine.evidence.trusted_bundle())
                self.assertNotIn(str(root), json.dumps(bundle))
                support["support:fixture/helper.py"] = "0" * 64
                with patch.object(machine.host, "source_manifest", return_value=(paths, public, public | support)), self.assertRaises(ValueError):
                    machine.source_manifest(root)

    def test_all_loaded_control_adapters_are_bound_to_native_policy_blobs(self):
        machine = load()
        sources = {".github/scripts/" + name.replace("_", "-") + ".py": "a" * 40 for name in machine.evidence.CONTROL_SOURCES}
        sources.update(dict.fromkeys(machine.control.FULL_PATHS, "a" * 40))
        with patch.object(machine.core, "git", return_value="a" * 40) as git:
            machine.verify_control_files({"sources": sources})
        checked = {Path(call.args[-1]).name for call in git.call_args_list}
        self.assertEqual(checked, {Path(path).name for path in sources})
        for name in ("release-host.py", "release-host-proof.py", "release-finalize.py", "ci-shards.py", "ci-plan.py"):
            changed = dict(sources)
            changed[".github/scripts/" + name] = "b" * 40
            with patch.object(machine.core, "git", return_value="a" * 40), self.assertRaises(ValueError):
                machine.verify_control_files({"sources": changed})

    def test_waiting_submission_never_signs_or_posts(self):
        machine = load()
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            root.chmod(0o700)
            preparer = SimpleNamespace(prepare_submission=lambda *args, **kwargs: {"status": "waiting"})
            api = SimpleNamespace(run=lambda _: {"run_attempt": 1, "status": "in_progress"}, repo="owner/repo")
            with patch.object(machine.core, "load_script", return_value=preparer) as loader:
                self.assertEqual(machine.submit(SimpleNamespace(work_dir=root, run_id=10, attempt=1), api)["status"], "waiting")
            self.assertEqual([call.args[0] for call in loader.call_args_list], ["release-finalize"])


if __name__ == "__main__":
    unittest.main()
