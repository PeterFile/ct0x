#!/usr/bin/env python3
"""Failed native settlement fixtures. No provider, network, binding, or release writes."""
from contextlib import closing, contextmanager
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import shutil
import sqlite3
import subprocess
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


helpers = load("test-release-reconcile")
machine = load("release-machine")
recovery = load("release-reconcile")


class PartialFixture(helpers.RegistryFixture):
    def __init__(self, root):
        super().__init__(root)
        self.driver = self.execute
        self.native_proof = machine.native_proof
        self.evidence = SimpleNamespace(binary_identity=self.binary_identity)
        self.core.git = lambda root, *args: helpers.TREE if "HEAD^{tree}" in args else "" if "status" in args else helpers.SHA
        self.finalizer.originals = self.originals
        self.paths = {"driver_6a": root / "original-a.py", "driver_6c": root / "original-c.py"}
        for path in self.paths.values():
            path.write_text("# Synthetic audited driver bytes\n")
        self.populate = lambda lease, phase: None
        self.returncode = 1

    @contextmanager
    def originals(self, paths, lane):
        def transcript(data, turn, receipt):
            path = data / "runtime" / turn["id"] / "gemini/aw-app/brain" / turn["native_session_id"] / ".system_generated/logs/transcript_full.jsonl"
            rows = [json.loads(line) for line in path.read_bytes().splitlines()]
            if not rows or any(type(row.get("step_index")) is not int for row in rows):
                raise ValueError("invalid original transcript")
            return rows
        yield {"native_transcript": transcript, "redact_receipt": lambda value: {key: value.get(key) for key in (
            "started", "accepted", "outcome", "terminal", "delivery_failed", "native_session_id",
            "direct_child_reaped", "output_complete", "exit_code", "logs_truncated")}}

    def source_manifest(self, unused=None):
        sources = {name: helpers.hashed(path) for name, path in self.paths.items()}
        return self.paths, sources, {"sources": sources, "support_sha256": "f" * 64}

    def binary_identity(self, go, binary, intent, env):
        return {"path": str(binary), "package": "agentworkspace/cmd/workspace", "vcs_revision": intent["sha"],
                "vcs_modified": False, "sha256": helpers.hashed(binary)}

    native_environment = staticmethod(lambda: {})

    def inputs(self, lease):
        helpers.save(lease.claim / "intent.json", lease.intent)
        helpers.save(lease.claim / "source-manifest.json", self.source_manifest()[2])
        (lease.claim / "source").mkdir()
        (lease.claim / "workspace").write_bytes(b"executed failed candidate bytes")
        helpers.save(lease.claim / "binary-before.json", self.binary_identity(None, lease.claim / "workspace", lease.intent, {}))

    def execute(self, args, stdout, stderr):
        phase = self.phase
        stdout.write_text("original child output\n")
        stderr.write_text("original child failure\n")
        self.populate(self.lease, phase)
        if self.returncode:
            raise subprocess.CalledProcessError(self.returncode, [str(args[0])])

    def invoke(self, lease, phase, code=1):
        self.lease, self.phase, self.returncode = lease, phase, code
        original = self.paths["driver_6a" if phase == "6a-execute" else "driver_6c"]
        stdout = "6a.stdout" if phase == "6a-execute" else "6c.stdout" if phase == "6c-execute" else "6c-prepare.stdout"
        stderr = stdout.replace("stdout", "stderr")
        lease.run(phase, [original], lease.claim / stdout, lease.claim / stderr)

    def data(self, lease, relative, native=False):
        root = lease.claim / relative
        data = root / "data"
        data.mkdir(mode=0o700, parents=True, exist_ok=True)
        (data / "workspace.lock").touch(mode=0o600)
        index = len(list(lease.claim.rglob("workspace-*.stdout.log"))) + 1
        (root / ("workspace.stdout.log" if "6a" in relative else f"workspace-{index}.stdout.log")).write_text(
            f"本地 HTTP 进程已启动： http://127.0.0.1:{54000 + index} （执行已禁用）\n")
        with closing(sqlite3.connect(data / "workspace.db")) as db, db:
            db.executescript("""CREATE TABLE IF NOT EXISTS turns(id TEXT,group_id TEXT,member_id TEXT,delivery_id TEXT,
                conversation_id TEXT,state TEXT,native_session_id TEXT,accepted_at TEXT,receipt_json TEXT);
                CREATE TABLE IF NOT EXISTS deliveries(id TEXT,group_id TEXT,recipient_id TEXT,conversation_id TEXT,state TEXT,turn_id TEXT);
                CREATE TABLE IF NOT EXISTS resource_claims(resource_key TEXT);""")
            if native:
                turn, session = "turn-" + str(index), "native-" + str(index)
                runtime = data / "runtime" / turn
                runtime.mkdir(parents=True)
                protocol = runtime / "stdout.jsonl"
                protocol.write_bytes(b"".join(helpers.encoded(value) for value in (
                    {"type": "session_new", "session_sha256": hashlib.sha256(session.encode()).hexdigest()},
                    {"type": "prompt_terminal", "tools_settled": True, "stop_reason": "end_turn"})))
                (runtime / "stderr.log").touch()
                transcript = runtime / "gemini/aw-app/brain" / session / ".system_generated/logs/transcript_full.jsonl"
                helpers.save(transcript, {"step_index": 1, "type": "PLANNER_RESPONSE", "status": "DONE"})
                receipt = {"native_session_id": session, "terminal": "succeeded", "outcome": "succeeded", "delivery_failed": False,
                           "started": True, "accepted": True, "direct_child_reaped": True, "output_complete": True,
                           "exit_code": 0, "logs_truncated": False, "stdout_path": str(protocol), "stderr_path": str(runtime / "stderr.log")}
                db.execute("INSERT INTO turns VALUES(?,?,?,?,?,?,?,?,?)", (turn, "group", "member", "delivery-" + turn,
                           "conversation", "succeeded", session, "accepted", json.dumps(receipt)))
                db.execute("INSERT INTO deliveries VALUES(?,?,?,?,?,?)", ("delivery-" + turn, "group", "member", "conversation", "accepted", turn))
                if "6a" in relative:
                    with self.originals({}, "6a") as original:
                        redacted = original["redact_receipt"](receipt)
                    helpers.save(root / "6a-receipt.json", {"binary": self.binary_identity(None, lease.claim / "workspace", lease.intent, {}),
                        "positive_turn": {"id": turn, "native_session_id": session, "execution_receipt": redacted},
                        "loopback_closed": True, "workspace_shutdown": {"graceful": True, "exit_code": 0}, "passed": False})
        return root


class FailedSettlementTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(dir=HERE.parents[1])
        self.addCleanup(self.temporary.cleanup)
        self.fixture = PartialFixture(Path(self.temporary.name).resolve())
        self.ports = patch.object(recovery, "closed_ports", return_value=None)
        self.ports.start()
        self.addCleanup(self.ports.stop)

    def recover(self, run_id=10, attempt=1):
        return recovery.reconcile(self.fixture, helpers.REPO, run_id, attempt, go=self.fixture.args().go)

    def failed(self, phases=("6c-prepare",), dead=False):
        fixture = self.fixture
        def work():
            try:
                with recovery.execution(fixture, fixture.args(), fixture.intent()) as lease:
                    fixture.inputs(lease)
                    for phase in phases:
                        try:
                            fixture.invoke(lease, phase, code=1 if phase == phases[-1] else 0)
                        except subprocess.CalledProcessError:
                            pass
                    if dead:
                        os._exit(23)
                    raise ValueError("candidate failed, not release evidence")
            except ValueError:
                pass
        if dead:
            child = os.fork()
            if child == 0:
                try:
                    work()
                except BaseException:
                    os._exit(99)
            _, status = os.waitpid(child, 0)
            self.assertEqual(os.waitstatus_to_exitcode(status), 23)
        else:
            work()
        return fixture.claim_path(fixture.scratch, helpers.REPO, 10, 1)

    def test_normal_failed_prepare_settles_only_its_populated_fixture(self):
        self.fixture.populate = lambda lease, phase: self.fixture.data(lease, "acceptance-6c-release/acceptance-000")
        claim = self.failed()
        self.assertFalse((self.fixture.STATE_ROOT / "machine-active").exists())
        self.assertEqual(self.recover()["status"], "settled")
        settlement = json.loads((claim / "lease-settlement.json").read_bytes())
        self.assertEqual(settlement["kind"], "native-settled")
        for name in ("complete.json", "receipt.json", "release-evidence.json", "joined-receipt.json"):
            self.assertFalse((claim / name).exists())
        with self.assertRaises(ValueError):
            machine.evidence.validate_machine(settlement)
        self.assertTrue((claim / "lease-failure.json").is_file())
        with self.assertRaises(ValueError):
            with recovery.execution(self.fixture, self.fixture.args(), self.fixture.intent()):
                self.fail("failed attempt replayed")
        with recovery.execution(self.fixture, self.fixture.args(11), self.fixture.intent(11)):
            pass

    def test_dead_worker_with_acknowledged_failed_prepare_settles_without_b_or_success(self):
        self.fixture.populate = lambda lease, phase: self.fixture.data(lease, "acceptance-6c-release/acceptance-000")
        claim = self.failed(dead=True)
        self.assertTrue((self.fixture.STATE_ROOT / "machine-active").exists())
        self.assertEqual(self.recover()["status"], "settled")
        self.assertEqual(self.recover()["status"], "settled")
        self.assertFalse((claim / "complete.json").exists())
        self.assertEqual(self.recover(attempt=2)["status"], "unknown")

    def test_a_and_c_completed_b_never_started_can_settle_without_passing_candidate(self):
        def populate(lease, phase):
            if phase == "6a-execute":
                self.fixture.data(lease, "acceptance-6a-release", native=True)
            else:
                for case in ("000", "200"):
                    self.fixture.data(lease, "acceptance-6c-release/acceptance-" + case, native=phase == "6c-execute")
        self.fixture.populate = populate
        claim = self.failed(("6c-prepare", "6a-execute", "6c-execute"), dead=True)
        self.assertEqual(self.recover()["status"], "settled")
        self.assertFalse((claim / "acceptance-6b-release").exists())
        self.assertFalse((claim / "complete.json").exists())

    def test_changed_held_cancelled_or_unbound_evidence_never_settles(self):
        for damage in ("database", "binary", "source", "scope", "protocol", "transcript", "child", "exit", "start",
                       "phase", "attempt", "prep", "inode", "claims", "cancel_requested", "unknown", "receipt", "delivery",
                       "orphan-end", "missing-start", "unstarted-root"):
            with self.subTest(damage=damage), tempfile.TemporaryDirectory(dir=HERE.parents[1]) as temporary:
                self.fixture = PartialFixture(Path(temporary).resolve())
                def populate(lease, phase):
                    if phase == "6a-execute":
                        self.fixture.data(lease, "acceptance-6a-release", native=True)
                    else:
                        for case in ("000", "200"):
                            self.fixture.data(lease, "acceptance-6c-release/acceptance-" + case)
                self.fixture.populate = populate
                claim = self.failed(("6c-prepare", "6a-execute"), dead=True)
                end = claim / "lease-end-6a-execute.json"
                database = claim / "acceptance-6a-release/data/workspace.db"
                if damage == "binary":
                    (claim / "workspace").write_bytes(b"changed binary")
                elif damage == "source":
                    self.fixture.paths["driver_6a"].write_text("# Changed original\n")
                elif damage in ("child", "exit", "start", "phase", "attempt", "prep", "scope"):
                    value = json.loads(end.read_bytes())
                    field, replacement = {"child": ("direct_child_reaped", False), "exit": ("exit_code", -9),
                        "start": ("start_sha256", "0" * 64), "phase": ("stage", "6c-execute"), "attempt": ("run_attempt", 2),
                        "prep": ("prep_sha256", "0" * 64), "scope": ("scope", {"files": {}, "databases": {}})}[damage]
                    value[field] = replacement
                    helpers.save(end, value)
                elif damage in ("protocol", "transcript"):
                    target = next((claim / "acceptance-6a-release").rglob("stdout.jsonl" if damage == "protocol" else "transcript_full.jsonl"))
                    target.write_bytes(b"changed original native evidence\n")
                elif damage == "inode":
                    database.rename(database.with_suffix(".old"))
                    shutil.copyfile(database.with_suffix(".old"), database)
                elif damage == "orphan-end":
                    helpers.save(claim / "lease-end-6b-execute.json", json.loads(end.read_bytes()))
                elif damage == "missing-start":
                    (claim / "lease-external-6a-execute.json").unlink()
                elif damage == "unstarted-root":
                    (claim / "acceptance-6b-release").mkdir()
                else:
                    with closing(sqlite3.connect(database)) as db, db:
                        if damage == "claims":
                            db.execute("INSERT INTO resource_claims VALUES('still-held')")
                        elif damage in ("cancel_requested", "unknown"):
                            db.execute("UPDATE turns SET state=?", (damage,))
                        elif damage == "receipt":
                            db.execute("UPDATE turns SET receipt_json='{}'")
                        elif damage == "delivery":
                            db.execute("UPDATE deliveries SET turn_id='other-turn'")
                        else:
                            db.execute("CREATE TABLE changed_after_child_exit(value TEXT)")
                    # An unchanged terminal snapshot must also reject invalid native state.
                    if damage in ("claims", "cancel_requested", "unknown", "receipt", "delivery"):
                        value = json.loads(end.read_bytes())
                        key = "acceptance-6a-release/data/workspace.db"
                        value["scope"]["files"][key] = helpers.hashed(database)
                        helpers.save(end, value)
                self.assertEqual(self.recover()["status"], "unknown")
                self.assertTrue((self.fixture.STATE_ROOT / "machine-active").is_dir())
                self.assertFalse((claim / "lease-settlement.json").exists())

    def test_held_os_lock_and_live_port_block_even_with_complete_child_ack(self):
        self.fixture.populate = lambda lease, phase: self.fixture.data(lease, "acceptance-6c-release/acceptance-000")
        claim = self.failed(dead=True)
        with recovery.flock(claim / "acceptance-6c-release/acceptance-000/data/workspace.lock"):
            self.assertEqual(self.recover()["status"], "unknown")
        with patch.object(recovery, "closed_ports", side_effect=recovery.Unproven("原始服务仍运行")):
            self.assertEqual(self.recover()["status"], "unknown")
        self.assertEqual(self.recover()["status"], "settled")

    def test_signal_timeout_and_crash_without_child_ack_stay_unknown(self):
        for error in (subprocess.CalledProcessError(-9, ["synthetic"]), subprocess.TimeoutExpired("synthetic", 1)):
            with self.subTest(error=type(error).__name__), tempfile.TemporaryDirectory(dir=HERE.parents[1]) as temporary:
                self.fixture = PartialFixture(Path(temporary).resolve())
                def execute(args, stdout, stderr):
                    stdout.touch()
                    stderr.touch()
                    self.fixture.data(self.fixture.lease, "acceptance-6c-release/acceptance-000")
                    raise error
                self.fixture.driver = execute
                with self.assertRaises(type(error)):
                    with recovery.execution(self.fixture, self.fixture.args(), self.fixture.intent()) as lease:
                        self.fixture.inputs(lease)
                        self.fixture.invoke(lease, "6c-prepare")
                self.assertEqual(self.recover()["status"], "unknown")
                self.assertFalse((lease.claim / "lease-end-6c-prepare.json").exists())

    def test_completed_child_is_recorded_even_when_original_physical_proof_is_missing(self):
        with self.assertRaises(subprocess.CalledProcessError):
            with recovery.execution(self.fixture, self.fixture.args(), self.fixture.intent()) as lease:
                self.fixture.inputs(lease)
                self.fixture.invoke(lease, "6c-prepare")
        end = json.loads((lease.claim / "lease-end-6c-prepare.json").read_bytes())
        self.assertEqual(end["exit_code"], 1)
        self.assertTrue(end["direct_child_reaped"])
        self.assertIsNone(end["scope"])
        self.assertEqual(self.recover()["status"], "unknown")

    def test_execute_with_empty_tables_and_no_native_terminal_proof_stays_unknown(self):
        self.fixture.populate = lambda lease, phase: [self.fixture.data(lease, "acceptance-6c-release/acceptance-" + case)
                                                     for case in ("000", "200")]
        self.failed(("6c-prepare", "6c-execute"), dead=True)
        self.assertEqual(self.recover()["status"], "unknown")

    def test_empty_data_with_external_start_but_no_ack_is_not_physical_proof(self):
        try:
            with recovery.execution(self.fixture, self.fixture.args(), self.fixture.intent()) as lease:
                self.fixture.inputs(lease)
                lease.external("6c-prepare")
                self.fixture.data(lease, "acceptance-6c-release/acceptance-000")
                raise ValueError("external worker died before acknowledgment")
        except ValueError:
            pass
        self.assertEqual(self.recover()["status"], "unknown")
        self.assertTrue((self.fixture.STATE_ROOT / "machine-active").exists())

    def test_matching_snapshot_cannot_substitute_for_missing_full_native_protocol(self):
        def populate(lease, phase):
            if phase == "6a-execute":
                self.fixture.data(lease, "acceptance-6a-release", native=True)
                protocol = next((lease.claim / "acceptance-6a-release").rglob("stdout.jsonl"))
                protocol.write_bytes(helpers.encoded({"type": "session_new", "session_sha256": "0" * 64}) +
                                     helpers.encoded({"type": "prompt_terminal", "tools_settled": False, "stop_reason": "end_turn"}))
            else:
                for case in ("000", "200"):
                    self.fixture.data(lease, "acceptance-6c-release/acceptance-" + case)
        self.fixture.populate = populate
        self.failed(("6c-prepare", "6a-execute"), dead=True)
        self.assertEqual(self.recover()["status"], "unknown")

    def test_crash_after_partial_settlement_before_archive_reconciles_exact_proof(self):
        self.fixture.populate = lambda lease, phase: self.fixture.data(lease, "acceptance-6c-release/acceptance-000")
        child = os.fork()
        if child == 0:
            try:
                with patch.object(recovery.os, "rename", side_effect=lambda *args: os._exit(23)):
                    with recovery.execution(self.fixture, self.fixture.args(), self.fixture.intent()) as lease:
                        self.fixture.inputs(lease)
                        self.fixture.invoke(lease, "6c-prepare")
            except BaseException:
                os._exit(99)
        _, status = os.waitpid(child, 0)
        self.assertEqual(os.waitstatus_to_exitcode(status), 23)
        claim = self.fixture.claim_path(self.fixture.scratch, helpers.REPO, 10, 1)
        before = (claim / "lease-settlement.json").read_bytes()
        self.assertEqual(self.recover()["status"], "settled")
        self.assertEqual((claim / "lease-settlement.json").read_bytes(), before)
        self.assertTrue((claim / "lease-failure.json").is_file())
        self.assertFalse((claim / "complete.json").exists())

    def test_failure_is_not_joined_when_full_or_next_sha_is_eligible(self):
        self.fixture.populate = lambda lease, phase: self.fixture.data(lease, "acceptance-6c-release/acceptance-000")
        claim = self.failed()
        finalizer = load("release-finalize")
        with patch.object(finalizer.machine, "verify_control_files"), patch.object(finalizer.machine, "authorized_host"), \
             patch.object(finalizer, "full_ready", return_value=True), patch.object(finalizer.evidence, "full_binding", return_value={}), \
             patch.object(finalizer.machine, "locate_claim", return_value=claim), \
             patch.object(finalizer, "verify_local", side_effect=AssertionError("partial failure entered release verification")):
            result = finalizer.prepare_local(self.fixture.args(), SimpleNamespace(repo=helpers.REPO), self.fixture.intent())
        self.assertEqual(result, {"status": "waiting", "reason": "native"})
        next_intent = self.fixture.intent(11) | {"sha": "c" * 40}
        with recovery.execution(self.fixture, self.fixture.args(11), next_intent):
            self.assertFalse((claim / "complete.json").exists())
        self.assertFalse((claim / "release-evidence.json").exists())


class OriginalEvidenceTests(unittest.TestCase):
    def setUp(self):
        manifest, native = os.environ.get("RELEASE_TEST_HOST_MANIFEST"), os.environ.get("RELEASE_TEST_NATIVE_ROOT")
        if not manifest or not native:
            self.skipTest("requires explicit read-only original manifest and native evidence root")
        config = machine.host._validate(json.loads(Path(manifest).read_bytes()))
        self.paths = {name: Path(value["path"]) for name, value in config["sources"].items()}
        self.native = Path(native).resolve(strict=True)
        self.finalizer = load("release-finalize")

    def test_actual_raw_6c_zero_claims_and_genuine_6a_terminal_transcript(self):
        c = self.native / "acceptance-6c-release/acceptance-000"
        a = self.native / "acceptance-6a-release"
        before = {path: helpers.hashed(path) for path in (c / "data/workspace.db", a / "data/workspace.db", a / "6a-receipt.json")}
        with self.finalizer.originals(self.paths, "6a") as original:
            prepared = recovery.settled_database(machine, self.finalizer, original, c, prepare_only=True)
            executed = recovery.settled_database(machine, self.finalizer, original, a, prepare_only=False)
        self.assertEqual(prepared["proofs"], [])
        self.assertEqual(len(prepared["ports"]), 3)
        self.assertEqual(len(executed["proofs"]), 1)
        report = json.loads((a / "6a-receipt.json").read_bytes())
        self.assertFalse(report["passed"])
        turn = report["positive_turn"]
        proof = executed["proofs"][0]
        self.assertEqual((proof["turn_id"], proof["session_id"]), (turn["id"], turn["native_session_id"]))
        self.assertEqual(proof["protocol_sha256"], turn["native_evidence"]["stdout_sha256"])
        self.assertEqual(proof["transcript_sha256"], turn["native_evidence"]["transcript_sha256"])
        self.assertEqual(helpers.hashed(Path(report["binary"]["path"])), report["binary"]["sha256"])
        self.assertEqual(before, {path: helpers.hashed(path) for path in before})

    def test_actual_failed_6c_prepare_bytes_settle_normal_and_dead_fixture_owners(self):
        source = self.native / "acceptance-6c-release"
        code = json.loads((self.native / "6c-prepare-exit.json").read_bytes())["exit"]
        self.assertEqual(code, 1)
        for dead in (False, True):
            with self.subTest(dead=dead), tempfile.TemporaryDirectory(dir=HERE.parents[1]) as temporary:
                fixture = PartialFixture(Path(temporary).resolve())
                # Only registry/input/child acknowledgment are fixtures. Inspect authentic original data and helpers.
                fixture.finalizer = self.finalizer
                fixture.paths = self.paths
                fixture.populate = lambda lease, phase: shutil.copytree(source, lease.claim / "acceptance-6c-release")
                fixture.native_proof = machine.native_proof
                def work():
                    try:
                        with recovery.execution(fixture, fixture.args(), fixture.intent()) as lease:
                            fixture.inputs(lease)
                            try:
                                fixture.invoke(lease, "6c-prepare", code)
                            except subprocess.CalledProcessError:
                                if dead:
                                    os._exit(23)
                                raise
                    except subprocess.CalledProcessError:
                        pass
                if dead:
                    child = os.fork()
                    if child == 0:
                        try:
                            work()
                        except BaseException:
                            os._exit(99)
                    _, status = os.waitpid(child, 0)
                    self.assertEqual(os.waitstatus_to_exitcode(status), 23)
                else:
                    work()
                result = recovery.reconcile(fixture, helpers.REPO, 10, 1, go=fixture.args().go)
                self.assertEqual(result["status"], "settled", result)
                claim = fixture.claim_path(fixture.scratch, helpers.REPO, 10, 1)
                copied = claim / "acceptance-6c-release/acceptance-000/data/workspace.db"
                self.assertEqual(helpers.hashed(copied), helpers.hashed(source / "acceptance-000/data/workspace.db"))
                self.assertFalse((claim / "complete.json").exists())
                self.assertFalse((claim / "acceptance-6c-release/acceptance-200").exists())


if __name__ == "__main__":
    unittest.main()
