#!/usr/bin/env python3
"""Synthetic registry, process-death and SQLite fixtures; no native/API execution."""
from concurrent.futures import ThreadPoolExecutor
from contextlib import closing, ExitStack, redirect_stdout
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import sqlite3
import subprocess
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

HERE = Path(__file__).resolve().parent
REPO, SHA, TREE = "owner/project", "a" * 40, "b" * 40
PHASES = ("6c-prepare", "6a-execute", "6c-execute", "6b-prepare", "6b-execute")


def encoded(value):
    return (json.dumps(value, sort_keys=True, separators=(",", ":")) + "\n").encode()


def save(path, value):
    path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    path.write_bytes(encoded(value))
    path.chmod(0o600)


def hashed(path):
    with path.open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


class RegistryFixture:
    def __init__(self, root):
        root = root.resolve()
        self.STATE_ROOT = root / "registry"
        self.STATE_ROOT.mkdir(mode=0o700)
        self.scratch = root / "hdd"
        self.scratch.mkdir(mode=0o700)
        self.host = SimpleNamespace(registry_root=lambda: self.STATE_ROOT)
        self.core = SimpleNamespace(file_hash=hashed, load_script=lambda _: self.finalizer)
        self.finalizer = SimpleNamespace(verify_local=self.verify_local, raw_file=self.raw_file)
        self.verifications = 0

    def private_directory(self, path):
        if path.is_symlink() or not path.is_dir() or path.stat().st_mode & 0o777 != 0o700:
            raise ValueError("必须使用私有目录")

    def authorized_host(self, path):
        self.private_directory(path)
        if not path.resolve().is_relative_to(self.scratch.resolve()):
            raise ValueError("不在绑定的合成 HDD")

    def verify_control_files(self, intent):
        if intent["workflow_sha"] != SHA:
            raise ValueError("控制源码不符")

    def claim_path(self, root, repo, run_id, attempt):
        return root / "claims" / f"{hashlib.sha256(repo.encode()).hexdigest()}-{run_id}-{attempt}"

    def reserve(self, root, repo, run_id, attempt):
        self.private_directory(root)
        active = self.STATE_ROOT / "machine-active"
        try:
            active.mkdir(mode=0o700)
        except FileExistsError:
            raise ValueError("机器忙") from None
        claim = self.claim_path(root, repo, run_id, attempt)
        permanent = self.claim_path(self.STATE_ROOT, repo, run_id, attempt)
        if permanent.exists() or claim.exists():
            active.rmdir()
            raise ValueError("同一 attempt 禁止重放")
        claim.mkdir(mode=0o700, parents=True)
        permanent.mkdir(mode=0o700, parents=True)
        identity = {"repo": repo, "run_id": run_id, "run_attempt": attempt}
        save(active / "owner.json", identity)
        save(permanent / "claim.json", identity | {"claim": str(claim)})
        save(claim / "reserved.json", identity)
        return claim

    def raw_file(self, root, path, limit=32 << 20):
        resolved = path.resolve(strict=True)
        if path.is_symlink() or not resolved.is_relative_to(root.resolve()) or not resolved.is_file() or resolved.stat().st_size > limit:
            raise ValueError("原始文件缺失或越界")
        return resolved

    def terminal(self, receipt, session, outcomes):
        if not (receipt.get("native_session_id") == session and receipt.get("terminal") == receipt.get("outcome") in outcomes
                and receipt.get("delivery_failed") is False and all(receipt.get(key) is True for key in
                    ("started", "accepted", "direct_child_reaped", "output_complete"))):
            raise ValueError("原生终态未结清")

    def verify_local(self, args, api, intent, claim, build, target_fact):
        self.verifications += 1
        self.verify_control_files(intent)
        self.assert_fixture(api is None, "recovery must not contact an API")
        value = json.loads((claim / "receipt.json").read_bytes())
        self.assert_fixture(value["artifact_sha256"] == hashed(claim / "workspace"), "actual binary changed")
        self.assert_fixture(build["artifact_sha256"] == value["artifact_sha256"] and target_fact == value["target_artifact"], "identity mismatch")
        # The injected original auditor consumes real bytes; it is not a passed flag.
        for lane in ("6a", "6b", "6c-0", "6c-200"):
            path = claim / (lane + "-original-proof.jsonl")
            event = json.loads(path.read_bytes())
            self.assert_fixture(event == {"type": "prompt_terminal", "tools_settled": True}, "original protocol not settled")
        return value

    @staticmethod
    def assert_fixture(condition, message):
        if not condition:
            raise ValueError(message)

    def args(self, run_id=10, attempt=1):
        return SimpleNamespace(work_dir=self.scratch, run_id=run_id, attempt=attempt, go=Path("/fixture/go"), harness_root=None)

    def intent(self, run_id=10, attempt=1):
        return {"schema": 1, "repo": REPO, "sha": SHA, "tree": TREE, "workflow_sha": SHA,
                "run_id": run_id, "run_attempt": attempt, "sources": {".github/scripts/release-reconcile.py": SHA}}

    def complete(self, claim):
        binary = claim / "workspace"
        binary.write_bytes(b"executed candidate bytes")
        value = self.intent() | {"kind": "release-machine", "artifact_sha256": hashed(binary), "target_artifact": {"id": 123}}
        save(claim / "receipt.json", value)
        save(claim / "complete.json", {"receipt_sha256": hashed(claim / "receipt.json")})
        for lane, relative in (
            ("6a", "acceptance-6a-release"), ("6b", "acceptance-6b-release"),
            ("6c-0", "acceptance-6c-release/acceptance-000"), ("6c-200", "acceptance-6c-release/acceptance-200")):
            data = claim / relative / "data"
            data.mkdir(mode=0o700, parents=True)
            (data / "workspace.lock").touch(mode=0o600)
            receipt = {"native_session_id": lane + "-session", "terminal": "succeeded", "outcome": "succeeded",
                       "delivery_failed": False, "started": True, "accepted": True, "direct_child_reaped": True, "output_complete": True}
            with closing(sqlite3.connect(data / "workspace.db")) as db, db:
                db.executescript("CREATE TABLE turns(id TEXT,state TEXT,native_session_id TEXT,receipt_json TEXT); CREATE TABLE resource_claims(resource_key TEXT);")
                db.execute("INSERT INTO turns VALUES(?,?,?,?)", (lane + "-turn", "succeeded", lane + "-session", json.dumps(receipt)))
            save(claim / (lane + "-original-proof.jsonl"), {"type": "prompt_terminal", "tools_settled": True})


class LeaseTests(unittest.TestCase):
    def setUp(self):
        path = HERE / "release-reconcile.py"
        self.assertTrue(path.is_file(), "缺少准备失败与已知终态的精确 owner 恢复模块")
        spec = importlib.util.spec_from_file_location("release_reconcile", path)
        self.reconcile = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(self.reconcile)
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.fixture = RegistryFixture(Path(self.temporary.name))
        self.args, self.intent = self.fixture.args(), self.fixture.intent()

    def recover(self, run_id=10):
        return self.reconcile.reconcile(self.fixture, REPO, run_id, 1, go=self.args.go)

    def killed(self, stage="seed-helper", external=False, complete=False):
        child = os.fork()
        if child == 0:
            try:
                with self.reconcile.execution(self.fixture, self.fixture.args(), self.intent) as lease:
                    lease.stage(stage)
                    if external:
                        for phase in PHASES if complete else PHASES[:1]:
                            lease.external(phase)
                    if complete:
                        self.fixture.complete(lease.claim)
                    os._exit(23)
            except BaseException:
                os._exit(99)
        _, status = os.waitpid(child, 0)
        self.assertEqual(os.waitstatus_to_exitcode(status), 23, "synthetic worker did not reach its kill window")
        return self.fixture.claim_path(self.fixture.scratch, REPO, 10, 1)

    def test_clone_install_and_seed_failures_release_only_machine_lock_and_keep_tombstone(self):
        for index, stage in enumerate(("clean-snapshot", "install-binary", "seed-helper"), start=10):
            args, intent = self.fixture.args(index), self.fixture.intent(index)
            error = subprocess.CalledProcessError(1, ["secret-token-in-command"], output="private prompt and credential")
            with self.subTest(stage=stage), self.assertRaises(subprocess.CalledProcessError):
                with self.reconcile.execution(self.fixture, args, intent) as lease:
                    lease.stage(stage)
                    raise error
            self.assertFalse((self.fixture.STATE_ROOT / "machine-active").exists())
            permanent = self.fixture.claim_path(self.fixture.STATE_ROOT, REPO, index, 1)
            self.assertTrue((permanent / "claim.json").is_file())
            diagnostic = json.loads((lease.claim / "lease-failure.json").read_bytes())
            self.assertEqual((diagnostic["repo"], diagnostic["run_id"], diagnostic["run_attempt"], diagnostic["stage"]), (REPO, index, 1, stage))
            self.assertNotIn("secret", json.dumps(diagnostic))
            self.assertNotIn("credential", json.dumps(diagnostic))
            with self.assertRaises(ValueError):
                with self.reconcile.execution(self.fixture, args, intent):
                    self.fail("permanent attempt was replayed")

    def test_busy_a_leaves_no_b_claim_and_b_retries_after_known_prep_failure(self):
        with self.reconcile.execution(self.fixture, self.args, self.intent) as a:
            self.assertEqual(self.recover()["status"], "busy")
            with self.assertRaises(ValueError):
                with self.reconcile.execution(self.fixture, self.fixture.args(11), self.fixture.intent(11)):
                    self.fail("busy candidate B entered")
            for root in (self.fixture.scratch, self.fixture.STATE_ROOT):
                self.assertFalse(self.fixture.claim_path(root, REPO, 11, 1).exists())
            a.stage("seed-helper")
        with self.reconcile.execution(self.fixture, self.fixture.args(11), self.fixture.intent(11)):
            pass
        self.assertFalse((self.fixture.STATE_ROOT / "machine-active").exists())

    def test_dead_pre_native_worker_can_be_settled_once_without_replay(self):
        claim = self.killed()
        self.assertEqual(self.recover()["status"], "settled")
        first = (claim / "lease-settlement.json").read_bytes()
        self.assertEqual(self.recover()["status"], "settled")
        self.assertEqual((claim / "lease-settlement.json").read_bytes(), first)
        with self.assertRaises(ValueError):
            with self.reconcile.execution(self.fixture, self.args, self.intent):
                self.fail("dead original attempt replayed")

    def test_live_worker_before_prep_publication_is_busy_not_recoverable(self):
        claim = self.fixture.reserve(self.args.work_dir, REPO, 10, 1)
        with self.reconcile.flock(claim / "worker.lock", create=True):
            self.assertEqual(self.recover()["status"], "busy")
        self.assertEqual(self.recover()["status"], "unknown")
        self.assertTrue((self.fixture.STATE_ROOT / "machine-active").exists())

    def test_replacing_the_worker_inode_is_not_evidence_that_the_original_worker_died(self):
        claim = self.killed()
        worker = claim / "worker.lock"
        worker.rename(claim / "original-worker.lock")
        worker.touch(mode=0o600)
        self.assertEqual(self.recover()["status"], "unknown")
        self.assertTrue((self.fixture.STATE_ROOT / "machine-active").exists())

    def test_gap_before_worker_attachment_or_missing_prep_record_remains_ambiguous(self):
        claim = self.fixture.reserve(self.args.work_dir, REPO, 10, 1)
        result = self.recover()
        self.assertEqual(result["status"], "unknown")
        self.assertIn("lease-prep.json", " ".join(result["missing"]))
        self.assertTrue((self.fixture.STATE_ROOT / "machine-active").is_dir())
        self.assertFalse((claim / "lease-settlement.json").exists())

    def test_kill_after_first_external_boundary_is_unknown_not_a_prep_failure(self):
        claim = self.killed(external=True)
        result = self.recover()
        self.assertEqual(result["status"], "unknown")
        self.assertIn("complete.json", " ".join(result["missing"]))
        self.assertTrue((self.fixture.STATE_ROOT / "machine-active").is_dir())
        self.assertFalse((claim / "lease-settlement.json").exists())

    def test_external_exception_and_timeout_are_not_terminal_receipts(self):
        with self.assertRaises(subprocess.TimeoutExpired):
            with self.reconcile.execution(self.fixture, self.args, self.intent) as lease:
                lease.external("6c-prepare")
                raise subprocess.TimeoutExpired("sensitive CLI", 5)
        self.assertEqual(self.recover()["status"], "unknown")
        self.assertTrue((self.fixture.STATE_ROOT / "machine-active").is_dir())
        self.assertEqual(json.loads((lease.claim / "lease-failure.json").read_bytes())["stage"], "6c-prepare")

    def test_completed_crash_rechecks_original_bytes_and_physical_sqlite_terminal_records(self):
        claim = self.killed(external=True, complete=True)
        self.assertEqual(self.recover()["status"], "settled")
        self.assertEqual(self.fixture.verifications, 1)
        self.assertFalse((self.fixture.STATE_ROOT / "machine-active").exists())
        self.assertEqual(json.loads((claim / "lease-settlement.json").read_bytes())["kind"], "native-complete")

    def test_shape_correct_complete_cannot_hide_raw_or_unsettled_claims(self):
        for damage in ("claims", "unknown", "receipt", "null-receipt", "original", "binary"):
            with self.subTest(damage=damage):
                # Each fixture models a separate dead owner; no native attempt reruns.
                with tempfile.TemporaryDirectory() as temporary:
                    previous = self.fixture
                    self.fixture = RegistryFixture(Path(temporary))
                    claim = self.killed(external=True, complete=True)
                    db_path = claim / "acceptance-6b-release/data/workspace.db"
                    if damage == "original":
                        save(claim / "6b-original-proof.jsonl", {"type": "prompt_terminal", "tools_settled": False})
                    elif damage == "binary":
                        (claim / "workspace").write_bytes(b"changed executed bytes")
                    else:
                        with closing(sqlite3.connect(db_path)) as db, db:
                            if damage == "claims":
                                db.execute("INSERT INTO resource_claims VALUES('still-held')")
                            elif damage == "unknown":
                                db.execute("UPDATE turns SET state='unknown'")
                            elif damage == "null-receipt":
                                db.execute("UPDATE turns SET receipt_json='null'")
                            else:
                                db.execute("UPDATE turns SET receipt_json='{}'")
                    result = self.recover()
                    self.assertEqual(result["status"], "unknown")
                    self.assertTrue(result["missing"])
                    self.assertTrue((self.fixture.STATE_ROOT / "machine-active").exists())
                    self.assertFalse((claim / "lease-settlement.json").exists())
                    self.fixture = previous

    def test_a_closed_worker_does_not_prove_that_the_native_service_released_its_lock(self):
        claim = self.killed(external=True, complete=True)
        path = claim / "acceptance-6c-release/acceptance-200/data/workspace.lock"
        with self.reconcile.flock(path):
            result = self.recover()
            self.assertEqual(result["status"], "unknown")
            self.assertIn("acceptance-200", " ".join(result["missing"]))
        self.assertTrue((self.fixture.STATE_ROOT / "machine-active").is_dir())
        self.assertEqual(self.recover()["status"], "settled")

    def test_wrong_owner_or_changed_registry_binding_never_unlocks(self):
        claim = self.killed()
        owner = self.fixture.STATE_ROOT / "machine-active/owner.json"
        replacement = {"repo": REPO, "run_id": 11, "run_attempt": 1}
        save(owner, replacement)
        result = self.recover()
        self.assertEqual(result["status"], "unknown")
        self.assertEqual(json.loads(owner.read_bytes()), replacement)
        self.assertFalse((claim / "lease-settlement.json").exists())

    def test_boolean_attempt_is_not_an_exact_native_owner(self):
        self.killed()
        owner = self.fixture.STATE_ROOT / "machine-active/owner.json"
        save(owner, {"repo": REPO, "run_id": 10, "run_attempt": True})
        self.assertEqual(self.recover()["status"], "unknown")
        self.assertTrue(owner.is_file())

    def test_help_does_not_load_machine_or_any_host_binding(self):
        with patch.object(self.reconcile.sys, "argv", ["release-reconcile.py", "--help"]), \
             patch.object(self.reconcile.importlib.util, "spec_from_file_location", side_effect=AssertionError("help loaded machine")), \
             redirect_stdout(io.StringIO()), self.assertRaises(SystemExit) as stopped:
            self.reconcile.main()
        self.assertEqual(stopped.exception.code, 0)

    def test_double_reconcile_and_new_b_cannot_remove_b_owner(self):
        self.killed()
        with ThreadPoolExecutor(max_workers=2) as pool:
            results = list(pool.map(lambda _: self.recover(), range(2)))
        self.assertTrue(all(row["status"] in ("settled", "busy") for row in results))
        self.assertIn("settled", [row["status"] for row in results])
        with self.reconcile.execution(self.fixture, self.fixture.args(11), self.fixture.intent(11)):
            self.assertEqual(self.recover()["status"], "settled")
            owner = json.loads((self.fixture.STATE_ROOT / "machine-active/owner.json").read_bytes())
            self.assertEqual(owner["run_id"], 11)

    def test_releasing_exact_owner_is_atomic_and_keeps_its_archived_identity(self):
        self.killed()
        self.assertEqual(self.recover()["status"], "settled")
        permanent = self.fixture.claim_path(self.fixture.STATE_ROOT, REPO, 10, 1)
        self.assertEqual(json.loads((permanent / "released-machine-active/owner.json").read_bytes()),
                         {"repo": REPO, "run_id": 10, "run_attempt": 1})

    def test_changed_prep_identity_is_not_recovery_authority(self):
        claim = self.killed()
        prep_path = claim / "lease-prep.json"
        prep = json.loads(prep_path.read_bytes())
        prep["run_attempt"] = 2
        save(prep_path, prep)
        self.assertEqual(self.recover()["status"], "unknown")
        self.assertTrue((self.fixture.STATE_ROOT / "machine-active").exists())

    def test_symlinked_worker_or_changed_registry_binding_cannot_authorize_unlock(self):
        claim = self.killed()
        worker = claim / "worker.lock"
        worker.rename(claim / "original-worker.lock")
        worker.symlink_to(claim / "original-worker.lock")
        self.assertEqual(self.recover()["status"], "unknown")
        worker.unlink()
        (claim / "original-worker.lock").rename(worker)
        registration = self.fixture.claim_path(self.fixture.STATE_ROOT, REPO, 10, 1) / "claim.json"
        save(registration, {"repo": REPO, "run_id": 10, "run_attempt": 2, "claim": str(claim)})
        self.assertEqual(self.recover()["status"], "unknown")
        self.assertTrue((self.fixture.STATE_ROOT / "machine-active").exists())

    def test_crash_after_prep_settlement_before_lock_rename_preserves_the_failed_attempt(self):
        child = os.fork()
        if child == 0:
            try:
                with patch.object(self.reconcile.os, "rename", side_effect=lambda *args: os._exit(23)):
                    with self.reconcile.execution(self.fixture, self.args, self.intent) as lease:
                        lease.stage("seed-helper")
                        raise OSError("synthetic seed failure")
            except BaseException:
                os._exit(99)
        _, status = os.waitpid(child, 0)
        self.assertEqual(os.waitstatus_to_exitcode(status), 23)
        claim = self.fixture.claim_path(self.fixture.scratch, REPO, 10, 1)
        prior = (claim / "lease-settlement.json").read_bytes()
        self.assertEqual(self.recover()["status"], "settled")
        self.assertEqual((claim / "lease-settlement.json").read_bytes(), prior)
        self.assertEqual(json.loads(prior)["kind"], "pre-native-failed")

    def test_durable_registry_and_scratch_links_precede_the_prepared_worker(self):
        synced = []
        original = self.reconcile.sync_directory
        def sync(path):
            synced.append(path)
            original(path)
        with patch.object(self.reconcile, "sync_directory", side_effect=sync):
            with self.reconcile.execution(self.fixture, self.args, self.intent) as lease:
                permanent = self.fixture.claim_path(self.fixture.STATE_ROOT, REPO, 10, 1)
                expected = {self.fixture.STATE_ROOT, self.fixture.STATE_ROOT / "claims", permanent,
                            self.fixture.STATE_ROOT / "machine-active", self.fixture.scratch, lease.claim.parent, lease.claim}
                self.assertTrue(expected <= set(synced), "permanent claim links must survive a power-loss before another scratch is used")

    def test_untrusted_prep_policy_cannot_create_registry_state_during_recovery(self):
        claim = self.fixture.reserve(self.args.work_dir, REPO, 10, 1)
        active = self.fixture.STATE_ROOT / "machine-active"
        (claim / "worker.lock").touch(mode=0o600)
        owner = {"repo": REPO, "run_id": 10, "run_attempt": 1}
        save(claim / "lease-prep.json", owner | {"schema": 1, "kind": "release-native-prep", "claim": str(claim),
             "active": [active.stat().st_dev, active.stat().st_ino],
             "worker": [(claim / "worker.lock").stat().st_dev, (claim / "worker.lock").stat().st_ino],
             "intent": self.intent | {"workflow_sha": "c" * 40}})
        self.assertEqual(self.recover()["status"], "unknown")
        self.assertFalse((self.fixture.STATE_ROOT / "lease-mutation.lock").exists())
        self.assertTrue(active.is_dir())

    def test_boundary_publication_failure_never_starts_work_and_a_visible_marker_stays_unknown(self):
        for published in (False, True):
            with self.subTest(published=published), tempfile.TemporaryDirectory() as temporary:
                fixture = RegistryFixture(Path(temporary))
                original = self.reconcile.save
                def fail(path, value, **kwargs):
                    if path.name == "lease-external-6c-prepare.json":
                        if published:
                            original(path, value, **kwargs)
                        raise OSError("synthetic fsync failure")
                    return original(path, value, **kwargs)
                with self.assertRaises(OSError), patch.object(self.reconcile, "save", side_effect=fail):
                    with self.reconcile.execution(fixture, fixture.args(), fixture.intent()) as lease:
                        lease.external("6c-prepare")
                        self.fail("external command reached after failed durable publication")
                self.assertEqual((fixture.STATE_ROOT / "machine-active").exists(), published)


class RuntimeIntegrationTests(unittest.TestCase):
    def setUp(self):
        spec = importlib.util.spec_from_file_location("release_machine", HERE / "release-machine.py")
        self.machine = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(self.machine)
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.fixture = RegistryFixture(Path(self.temporary.name))
        self.args, self.intent = self.fixture.args(), self.fixture.intent()
        self.args.execute_native, self.args.schema_version = True, 42
        self.args.agy = self.fixture.scratch / "synthetic-unused-cli"
        self.args.agy.write_bytes(b"not executable")
        self.stack = ExitStack()
        self.addCleanup(self.stack.close)
        previous = os.umask(0o077)
        self.addCleanup(os.umask, previous)
        patches = ((self.machine, "STATE_ROOT", self.fixture.STATE_ROOT),
                   (self.machine.host, "registry_root", lambda: self.fixture.STATE_ROOT),
                   (self.machine, "authorized_host", self.fixture.authorized_host),
                   (self.machine, "verify_control_files", self.fixture.verify_control_files),
                   (self.machine, "ensure_active", lambda *args: self.intent),
                   (self.machine, "source_manifest", lambda *args: ({name: Path(name) for name in ("driver_6a", "driver_6c", "seed_6b")}, {}, {})),
                   (self.machine.evidence, "target", lambda *args: ({"artifact_sha256": hashlib.sha256(b"candidate").hexdigest()}, b"candidate", {})),
                   (self.machine.evidence, "binary_identity", lambda *args: {"sha256": hashlib.sha256(b"candidate").hexdigest()}),
                   (self.machine, "clean_snapshot", lambda *args: self.fixture.scratch),
                   (self.machine, "seed_helper", lambda *args: self.fixture.scratch / "unused-seeder"),
                   (self.machine.runpy, "run_path", lambda *args: {"AGY": str(self.args.agy)}))
        for target, name, value in patches:
            self.stack.enter_context(patch.object(target, name, value))
        self.command = self.stack.enter_context(patch.object(self.machine, "command", side_effect=AssertionError("real command forbidden")))
        self.driver = self.stack.enter_context(patch.object(self.machine, "driver", side_effect=AssertionError("real driver forbidden")))
        self.api = SimpleNamespace(repo=REPO)

    def test_real_run_wraps_install_clone_and_seed_failures_in_owned_prep_lease(self):
        for index, operation in enumerate(("install_binary", "clean_snapshot", "seed_helper"), start=10):
            with self.subTest(operation=operation):
                self.args.run_id, self.intent["run_id"] = index, index
                with patch.object(self.machine, operation, side_effect=OSError("secret native output")), self.assertRaises(OSError):
                    self.machine.run(self.args, self.api)
                self.assertFalse((self.fixture.STATE_ROOT / "machine-active").exists(), "known preparation failure orphaned machine-wide owner")
                claim = self.machine.claim_path(self.fixture.scratch, REPO, index, 1)
                self.assertTrue((claim / "lease-prep.json").is_file())
                self.assertTrue((claim / "lease-failure.json").is_file())
                self.assertTrue(self.machine.claim_path(self.fixture.STATE_ROOT, REPO, index, 1).is_dir())
        self.command.assert_not_called()
        self.driver.assert_not_called()

    def test_first_original_6c_prepare_is_already_external_and_worker_locked(self):
        def first(args, *unused):
            claim = self.machine.claim_path(self.fixture.scratch, REPO, 10, 1)
            self.assertTrue((claim / "lease-external-6c-prepare.json").is_file(), "6C prepare starts a service before execute")
            module = self.machine.core.load_script("release-reconcile")
            self.assertEqual(module.reconcile(self.machine, REPO, 10, 1, go=self.args.go)["status"], "busy")
            raise subprocess.TimeoutExpired("synthetic-driver", 1)
        self.driver.side_effect = first
        with self.assertRaises(subprocess.TimeoutExpired):
            self.machine.run(self.args, self.api)
        self.assertTrue((self.fixture.STATE_ROOT / "machine-active").exists())
        self.command.assert_not_called()

    def test_new_runtime_blob_is_an_immutable_control_source(self):
        spec = importlib.util.spec_from_file_location("release_machine_identity", HERE / "release-machine.py")
        machine = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(machine)
        self.assertIn("release_reconcile", machine.evidence.CONTROL_SOURCES)
        paths = {".github/scripts/" + name.replace("_", "-") + ".py" for name in machine.evidence.CONTROL_SOURCES} | set(machine.control.FULL_PATHS)
        intent = {"sources": {name: SHA for name in paths}}
        def changed(root, command, path):
            self.assertEqual(command, "hash-object")
            return "c" * 40 if Path(path).name == "release-reconcile.py" else SHA
        with patch.object(machine.core, "git", side_effect=changed), self.assertRaises(ValueError):
            machine.verify_control_files(intent)


if __name__ == "__main__":
    unittest.main()
