#!/usr/bin/env python3
"""Offline WAL settlement regressions. Only private fixtures are mutated."""
from contextlib import closing
import importlib.util
from pathlib import Path
import sqlite3
import subprocess
import tempfile
import unittest
from unittest.mock import patch

HERE = Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location("failed_settlement", HERE / "test-release-failed-settlement.py")
failed = importlib.util.module_from_spec(spec)
spec.loader.exec_module(failed)
helpers, recovery = failed.helpers, failed.recovery


def inventory(root):
    return {str(path.relative_to(root)): helpers.hashed(path) for path in root.rglob("*") if path.is_file()}


class WALSettlementTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(dir=HERE.parents[1])
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name).resolve()
        ports = patch.object(recovery, "closed_ports", return_value=None)
        ports.start()
        self.addCleanup(ports.stop)
        self.sources = {}

    def wal(self, database, held=False, committed=False):
        with closing(sqlite3.connect(database)) as db:
            self.assertEqual(db.execute("PRAGMA journal_mode=WAL").fetchone(), ("wal",))
        self.assertFalse(Path(str(database) + "-wal").exists())
        self.assertFalse(Path(str(database) + "-shm").exists())
        if held or committed:
            writer = sqlite3.connect(database)
            self.addCleanup(writer.close)
            writer.execute("PRAGMA wal_checkpoint(TRUNCATE)")
            main = helpers.hashed(database)
            writer.execute("INSERT INTO resource_claims VALUES('still-held-in-wal')")
            if not held:
                writer.execute("DELETE FROM resource_claims")
            writer.commit()
            self.assertEqual(helpers.hashed(database), main)
            self.assertGreater(Path(str(database) + "-wal").stat().st_size, 32)
        self.sources[database.parent] = inventory(database.parent)

    def partial(self, *, held=False, unwind=False, committed=False):
        fixture = self.fixture = failed.PartialFixture(self.root)
        def populate(lease, phase):
            root = fixture.data(lease, "acceptance-6c-release/acceptance-000")
            self.wal(root / "data/workspace.db", held, committed)
        fixture.populate = populate
        try:
            with recovery.execution(fixture, fixture.args(), fixture.intent()) as lease:
                fixture.inputs(lease)
                try:
                    fixture.invoke(lease, "6c-prepare")
                except subprocess.CalledProcessError:
                    pass
                if unwind:
                    raise ValueError("candidate failed, not release evidence")
        except ValueError:
            if not unwind:
                raise
        self.claim = lease.claim

    def complete(self, *, held=False):
        fixture = self.fixture = helpers.RegistryFixture(self.root)
        with recovery.execution(fixture, fixture.args(), fixture.intent()) as lease:
            for phase in helpers.PHASES:
                lease.external(phase)
            fixture.complete(lease.claim)
            for index, relative in enumerate(recovery.DATABASES):
                self.wal(lease.claim / relative / "workspace.db", held and index == 3)
        self.claim = lease.claim
        original = fixture.verify_local
        def audit(*args):
            # The complete verifier also invokes original auditors that open SQLite.
            for relative in recovery.DATABASES:
                database = self.claim / relative / "workspace.db"
                with closing(sqlite3.connect(database.as_uri() + "?mode=ro", uri=True)) as db:
                    db.execute("SELECT COUNT(*) FROM resource_claims").fetchone()
            return original(*args)
        fixture.finalizer.verify_local = audit

    def recover(self):
        return recovery.reconcile(self.fixture, helpers.REPO, 10, 1, go=self.fixture.args().go)

    def unchanged(self):
        for source, before in self.sources.items():
            self.assertEqual(inventory(source), before)
        self.assertFalse(list(self.claim.glob(".settlement-sqlite-*")))

    def test_failed_wal_prepare_without_sidecars_settles_without_creating_original_sidecars(self):
        self.partial()
        result = self.recover()
        self.assertEqual(result["status"], "settled", result)
        self.unchanged()
        self.assertFalse((self.claim / "complete.json").exists())
        self.assertFalse((self.claim / "receipt.json").exists())
        with self.assertRaises(ValueError):
            with recovery.execution(self.fixture, self.fixture.args(), self.fixture.intent()):
                self.fail("failed attempt replayed")

    def test_nonempty_committed_wal_without_claims_can_settle(self):
        self.partial(committed=True)
        result = self.recover()
        self.assertEqual(result["status"], "settled", result)
        self.unchanged()

    def test_failure_unwind_also_preserves_wal_source_and_settles(self):
        self.partial(unwind=True)
        self.assertFalse((self.fixture.STATE_ROOT / "machine-active").exists())
        self.assertEqual(self.recover()["status"], "settled")
        self.unchanged()

    def test_committed_claim_only_in_wal_keeps_failed_prepare_unknown(self):
        self.partial(held=True)
        result = self.recover()
        self.assertEqual(result["status"], "unknown", result)
        self.assertTrue((self.fixture.STATE_ROOT / "machine-active").is_dir())
        self.assertFalse((self.claim / "lease-settlement.json").exists())
        self.unchanged()

    def test_complete_proof_and_original_auditor_do_not_create_source_sidecars(self):
        self.complete()
        result = self.recover()
        self.assertEqual(result["status"], "settled", result)
        self.assertEqual(self.fixture.verifications, 1)
        self.unchanged()

    def test_complete_proof_cannot_ignore_a_committed_wal_claim(self):
        self.complete(held=True)
        result = self.recover()
        self.assertEqual(result["status"], "unknown", result)
        self.assertTrue((self.fixture.STATE_ROOT / "machine-active").is_dir())
        self.assertFalse((self.claim / "lease-settlement.json").exists())
        self.unchanged()

    def test_changed_original_bytes_during_complete_audit_stay_unknown(self):
        self.complete()
        audit = self.fixture.finalizer.verify_local
        def changed(*args):
            result = audit(*args)
            # A valid user_version change is not a terminal-state or health failure.
            database = self.claim / recovery.DATABASES[0] / "workspace.db"
            with database.open("r+b") as stream:
                stream.seek(60)
                stream.write((1).to_bytes(4, "big"))
            return result
        self.fixture.finalizer.verify_local = changed
        result = self.recover()
        self.assertEqual(result["status"], "unknown", result)
        self.assertTrue((self.fixture.STATE_ROOT / "machine-active").is_dir())
        self.assertFalse((self.claim / "lease-settlement.json").exists())
        self.assertFalse(list(self.claim.glob(".settlement-sqlite-*")))

    def test_live_workspace_lock_prevents_even_the_complete_auditor_opening_sqlite(self):
        self.complete()
        lock = self.claim / recovery.DATABASES[-1] / "workspace.lock"
        with recovery.flock(lock), patch.object(sqlite3, "connect", side_effect=AssertionError("opened live source")):
            result = self.recover()
        self.assertEqual(result["status"], "unknown", result)
        self.assertEqual(self.fixture.verifications, 0)
        self.unchanged()

    def test_cold_zeroed_rollback_journal_does_not_prevent_settlement(self):
        self.complete()
        database = self.claim / recovery.DATABASES[0] / "workspace.db"
        Path(str(database) + "-journal").write_bytes(bytes(1024))
        self.sources[database.parent] = inventory(database.parent)
        result = self.recover()
        self.assertEqual(result["status"], "settled", result)
        self.unchanged()

    def test_possible_hot_rollback_journal_stays_unknown(self):
        self.complete()
        database = self.claim / recovery.DATABASES[0] / "workspace.db"
        journal = Path(str(database) + "-journal")
        journal.write_bytes(bytes.fromhex("d9d505f920a163d7") + bytes(1024))
        self.sources[database.parent] = inventory(database.parent)
        self.assertEqual(self.recover()["status"], "unknown")
        self.assertTrue((self.fixture.STATE_ROOT / "machine-active").is_dir())
        self.unchanged()


if __name__ == "__main__":
    unittest.main()
