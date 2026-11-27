"""Same-checkout replay of the real #269 producer exchange; no external transport."""
import base64
import copy
import hashlib
import io
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch
from urllib.parse import parse_qs, urlsplit
import zipfile

from github import API, Reply
from observer import Observer, RELEASE
from store import Key, Store
import codex_receiver as native

HERE = Path(__file__).resolve().parent
REPO = "owner/project"
ROOT = "/repos/" + REPO
COORDINATOR = "00000000-0000-4000-8000-000000000001"
SHA, TREE, SOURCE = (c * 40 for c in "abc")
OWNER = "https://github.com/owner/project/actions/runs/10"


class Exchange:
    def __init__(self):
        self.data = json.loads((HERE / "release_duplicate_v1.json").read_text())
        self.responses = self.data["responses"]
        self.calls = []
        self.denied = set()

    def __call__(self, path, etag=None):
        self.calls.append(path)
        parsed = urlsplit(path)
        query = parse_qs(parsed.query)
        endpoint = parsed.path if "per_page" in query else path
        if endpoint in self.denied or endpoint not in self.responses and path not in self.data["archives"] and path not in self.data["logs"]:
            return Reply(404, {}, b"{}")
        if path in self.data["archives"]:
            return Reply(200, {}, base64.b64decode(self.data["archives"][path]))
        if path in self.data["logs"]:
            return Reply(200, {}, self.data["logs"][path].encode())
        return Reply(200, {}, json.dumps(self.responses[endpoint]).encode())

    def retry(self):
        run = copy.deepcopy(self.run)
        run.update(run_attempt=2, run_started_at="2026-11-26T15:58:00Z")
        jobs = copy.deepcopy(self.jobs)
        for job in jobs:
            job.update(id=job["id"] + 1000, run_attempt=2, started_at="2026-11-26T15:58:01Z", completed_at="2026-11-26T15:58:30Z")
            job["html_url"] = f"https://github.com/{REPO}/actions/runs/11/job/{job['id']}"
        self.responses[ROOT + "/actions/runs/11"] = run
        self.responses[ROOT + "/actions/runs/11/attempts/2"] = run
        self.responses[ROOT + "/actions/runs/11/attempts/2/jobs"] = {"total_count": len(jobs), "jobs": jobs}
        old_archive = base64.b64decode(self.data["archives"][ROOT + "/actions/artifacts/144/zip"])
        with zipfile.ZipFile(io.BytesIO(old_archive)) as archive:
            intent = json.loads(archive.read("receipt.json"))
        intent["run_attempt"] = 2
        stream = io.BytesIO()
        with zipfile.ZipFile(stream, "w") as archive:
            archive.writestr("receipt.json", json.dumps(intent))
        body = stream.getvalue()
        artifact = dict(self.responses[ROOT + "/actions/runs/11/artifacts"]["artifacts"][0], id=1144,
                        name="release-intent-11-2", created_at="2026-11-26T15:58:15Z", digest="sha256:" + hashlib.sha256(body).hexdigest())
        self.responses[ROOT + "/actions/runs/11/artifacts"]["artifacts"].append(artifact)
        self.responses[ROOT + "/actions/runs/11/artifacts"]["total_count"] += 1
        self.data["archives"][ROOT + "/actions/artifacts/1144/zip"] = base64.b64encode(body).decode()
        self.data["logs"][ROOT + "/actions/jobs/1122/logs"] = self.data["logs"][ROOT + "/actions/jobs/122/logs"]
        return run, jobs

    @property
    def run(self):
        return self.responses[ROOT + "/actions/runs/11/attempts/1"]

    @property
    def jobs(self):
        return self.responses[ROOT + "/actions/runs/11/attempts/1/jobs"]["jobs"]

    @property
    def status(self):
        return self.responses[ROOT + "/commits/" + SHA + "/statuses"][0]


class DuplicateContracts(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(prefix="observer-dupe-", dir=os.environ.get("TMPDIR", "/tmp"))
        self.addCleanup(self.tmp.cleanup)
        self.path = Path(self.tmp.name) / "observer.sqlite"
        self.store = Store(self.path)
        self.addCleanup(lambda: self.store.close())
        self.store.configure(REPO, COORDINATOR)
        self.world = Exchange()
        self.api = API(self.store, self.world)

    def tick(self, jobs=None):
        watcher = Observer(self.store, self.api)
        watcher.pump = lambda: None
        return watcher.process(copy.deepcopy(self.world.run), copy.deepcopy(self.world.jobs if jobs is None else jobs), True)

    def reopen(self):
        self.store.close()
        self.store = Store(self.path)
        self.api = API(self.store, self.world)

    def register(self):
        rollout = Path(self.tmp.name) / ("rollout-fixture-" + COORDINATOR + ".jsonl")
        rollout.write_text(json.dumps({"type": "session_meta", "payload": {
            "id": COORDINATOR, "source": "cli", "thread_source": "user",
        }}) + "\n")
        rollout.chmod(0o600)
        native.register(self.store, COORDINATOR, str(rollout))

    def test_real_pending_owner_yields_one_informational_terminal_across_restart(self):
        self.assertTrue(self.tick())
        self.reopen()
        self.assertTrue(self.tick())
        actions = self.store.actions()
        self.assertEqual([a["kind"] for a in actions], ["terminal"])
        payload = self.store.inbox(COORDINATOR)[0]["payload"]
        self.assertEqual(payload["conclusion"], "failure")
        self.assertIs(payload["investigation"], False)
        self.assertEqual((payload["tested_sha"], payload["tested_tree"]), (SHA, TREE))
        proof = payload["release_duplicate"]
        self.assertEqual((proof["owner_run_id"], proof["owner_url"]), (10, OWNER))
        self.assertEqual(proof["owner_status"], "in_progress")
        self.assertEqual((proof["status_id"], proof["admission_job_id"], proof["duplicate_job_id"]), (40, 21, 122))
        self.assertEqual(proof["provenance"], "native-release-owner-v1")
        self.assertFalse(any("/logs" in path and "/122/" not in path for path in self.world.calls))

    def test_restart_during_api_outage_and_later_owner_success_do_not_create_new_work(self):
        self.assertTrue(self.tick())
        receipt = self.store.inbox(COORDINATOR)[0]["receipt_id"]
        self.reopen()
        self.world.denied.add(ROOT + "/commits/" + SHA + "/statuses")
        self.assertFalse(self.tick(), "missing fresh evidence remains pending, not a new investigation")
        self.assertEqual([a["kind"] for a in self.store.actions()], ["terminal"])
        self.assertEqual(self.store.inbox(COORDINATOR)[0]["receipt_id"], receipt)
        self.world.denied.clear()
        self.world.responses[ROOT + "/actions/runs/10"].update(status="completed", conclusion="success")
        self.assertTrue(self.tick())
        self.assertEqual([a["kind"] for a in self.store.actions()], ["terminal"])
        row = self.store.inbox(COORDINATOR)[0]
        self.assertEqual(row["receipt_id"], receipt)
        self.assertEqual(row["payload"]["release_duplicate"]["owner_conclusion"], "success")

    def test_previous_duplicate_proof_does_not_hide_new_abnormal_evidence(self):
        self.assertTrue(self.tick())
        self.reopen()
        self.world.data["logs"][ROOT + "/actions/jobs/122/logs"] += "Traceback (most recent call last):\n"
        self.tick()
        self.assertEqual([a["payload"]["job_id"] for a in self.store.actions() if a["kind"] == "first_failure"], [122])

    def test_complete_proof_precedes_streaming_first_failure(self):
        watcher = Observer(self.store, self.api)
        watcher.pump = lambda: None
        context = watcher.prepare(self.world.run, True)
        watcher.notice(context, [self.world.jobs[2]])
        self.assertEqual(self.store.actions(), [])
        self.assertTrue(watcher.process(self.world.run, self.world.jobs, True, context))
        self.assertEqual([a["kind"] for a in self.store.actions()], ["terminal"])

    def test_unverified_suspicious_and_executed_product_cases_keep_first_blocker(self):
        def modify(world, name):
            owner = world.responses[ROOT + "/actions/runs/10/attempts/1"]
            owner_jobs = world.responses[ROOT + "/actions/runs/10/attempts/1/jobs"]["jobs"]
            if name == "unknown_status": world.denied.add(ROOT + "/commits/" + SHA + "/statuses")
            elif name == "wrong_creator": world.status["creator"]["id"] = 7
            elif name == "wrong_context": world.status["context"] = "untrusted-owner"
            elif name == "wrong_repo": owner["repository"]["full_name"] = "other/project"
            elif name == "wrong_workflow": owner["workflow_id"] = 99
            elif name == "wrong_time": world.status["created_at"] = "2026-11-26T15:56:40Z"
            elif name == "incomplete_jobs": world.responses[ROOT + "/actions/runs/11/attempts/1/jobs"]["total_count"] += 1
            elif name == "wrong_source": owner["head_commit"]["tree_id"] = TREE
            elif name == "wrong_blob":
                rows = world.responses[ROOT + "/git/trees/" + "1" * 40 + "?recursive=1"]["tree"]
                next(row for row in rows if row["path"] == RELEASE)["sha"] = "f" * 40
            elif name == "unknown_source": world.denied.add(ROOT + "/git/trees/" + "1" * 40 + "?recursive=1")
            elif name == "wrong_admission": owner_jobs[1]["name"] = "Release admission " + SOURCE
            elif name == "inherited_admission": owner_jobs[1]["started_at"] = "2026-11-25T15:56:53Z"
            elif name == "wrong_job_head": world.jobs[2]["head_sha"] = SHA
            elif name == "wrong_native_run": world.run["head_commit"]["tree_id"] = TREE
            elif name == "canonical_success": world.responses[ROOT + "/actions/runs/10"].update(status="completed", conclusion="success")
            elif name == "same_owner": world.status["target_url"] = "https://github.com/owner/project/actions/runs/11"
            elif name == "conflicting_claim": world.responses[ROOT + "/commits/" + SHA + "/statuses"].append(dict(world.status, id=41, target_url="https://github.com/owner/project/actions/runs/12"))
            elif name == "unknown_tree": world.denied.add(ROOT + "/commits/" + SHA)
            elif name == "independent_product": world.jobs[3]["conclusion"] = "failure"
            elif name == "product_success": world.jobs[3]["conclusion"] = "success"
            elif name == "unknown_job": world.jobs[3]["name"] = "Unexpected job"
            elif name == "abnormal_verdict": world.jobs[2]["conclusion"] = "timed_out"
            elif name == "traceback": world.data["logs"][ROOT + "/actions/jobs/122/logs"] += "Traceback (most recent call last):\n"
            elif name == "unknown_logs": world.data["logs"].pop(ROOT + "/actions/jobs/122/logs")
        names = ("unknown_status wrong_creator wrong_context wrong_repo wrong_workflow wrong_time incomplete_jobs wrong_source wrong_blob unknown_source "
                 "wrong_admission inherited_admission wrong_job_head wrong_native_run canonical_success "
                 "same_owner conflicting_claim unknown_tree independent_product product_success unknown_job abnormal_verdict traceback unknown_logs").split()
        for name in names:
            with self.subTest(name=name):
                case = DuplicateContracts()
                case.setUp()
                try:
                    modify(case.world, name)
                    case.tick()
                    first = [a for a in case.store.actions() if a["kind"] == "first_failure"]
                    self.assertEqual(len(first), 1)
                    self.assertEqual(first[0]["payload"]["job_id"], 122)
                    self.assertNotIn("release_duplicate", first[0]["payload"])
                    self.assertTrue(any(a["kind"] == "terminal" for a in case.store.actions()))
                finally:
                    case.doCleanups()

    def test_job_completion_before_run_terminal_does_not_request_investigation(self):
        self.world.run.update(status="in_progress", conclusion=None)
        self.assertFalse(self.tick())
        self.assertEqual(self.store.inbox(COORDINATOR), [])
        self.world.run.update(status="completed", conclusion="failure")
        self.assertTrue(self.tick())
        self.assertEqual([a["kind"] for a in self.store.actions()], ["terminal"])

    def test_retry_cannot_hide_product_execution_from_an_older_attempt(self):
        for executed in (False, True):
            with self.subTest(executed=executed):
                case = DuplicateContracts()
                case.setUp()
                try:
                    if executed:
                        case.world.jobs[3]["conclusion"] = "success"
                    run, jobs = case.world.retry()
                    # The retry itself never executed product jobs.
                    jobs[3]["conclusion"] = "skipped"
                    watcher = Observer(case.store, case.api)
                    watcher.pump = lambda: None
                    watcher.process(run, jobs, True)
                    actions = case.store.actions()
                    self.assertEqual(any(a["kind"] == "first_failure" for a in actions), executed)
                    terminal = next(a for a in actions if a["kind"] == "terminal")
                    self.assertEqual("release_duplicate" in terminal["payload"], not executed)
                finally:
                    case.doCleanups()

    def test_independent_earliest_product_failure_keeps_the_original_first_blocker(self):
        self.world.jobs[3].update(conclusion="failure", completed_at="2026-11-26T15:57:00Z")
        self.tick()
        first = next(a for a in self.store.actions() if a["kind"] == "first_failure")
        self.assertEqual(first["payload"]["job_id"], 130)
        self.reopen()
        self.tick()
        self.assertEqual([a["id"] for a in self.store.actions() if a["kind"] == "first_failure"], [first["id"]])
        self.assertTrue(all("release_duplicate" not in a["payload"] for a in self.store.actions()))

    def test_legacy_product_ready_record_cannot_be_hidden_as_a_duplicate(self):
        old = self.store.publish(Key(REPO, SOURCE, 11, 1, "130"), "release_binary_ready",
                                 dict(source_head=SOURCE, workflow_sha=SOURCE, tested_sha=SHA, workflow=RELEASE, conclusion="success"), [COORDINATOR])
        self.tick()
        self.assertIn(old, [a["id"] for a in self.store.actions()])
        self.assertEqual([a["payload"]["job_id"] for a in self.store.actions() if a["kind"] == "first_failure"], [122])
        self.assertTrue(all("release_duplicate" not in a["payload"] for a in self.store.actions()))

    def test_failed_canonical_remains_distinct_from_ancillary_duplicate(self):
        self.world.responses[ROOT + "/actions/runs/10"].update(status="completed", conclusion="failure")
        self.assertTrue(self.tick())
        proof = self.store.inbox(COORDINATOR)[0]["payload"]["release_duplicate"]
        self.assertEqual((proof["owner_status"], proof["owner_conclusion"]), ("completed", "failure"))
        # The canonical's own failure is still an ordinary first blocker.
        canonical = copy.deepcopy(self.world.responses[ROOT + "/actions/runs/10"])
        canonical["html_url"] = OWNER
        failed = dict(self.world.jobs[2], id=222, run_id=10, name="Release Full / Go store", html_url=OWNER + "/job/222")
        watcher = Observer(self.store, self.api)
        watcher.pump = lambda: None
        watcher.notice(watcher.prepare(canonical, True), [failed])
        self.assertEqual([(a["run_id"], a["kind"]) for a in self.store.actions() if a["kind"] == "first_failure"], [(10, "first_failure")])

    def test_late_verified_retry_reconciles_old_local_projections_without_ack_or_new_receipt(self):
        self.register()
        endpoint = ROOT + "/commits/" + SHA + "/statuses"
        self.world.denied.add(endpoint)
        self.tick()
        native._prepare(self.store)
        terminal = next(r for r in self.store.inbox(COORDINATOR) if r["payload"]["action"] == "terminal")
        self.world.denied.clear()
        self.reopen()
        self.assertTrue(self.tick())
        self.assertIsNone(native._next(self.store, COORDINATOR), "retired work is inert before rebuilding projections")
        native._prepare(self.store)
        visible = self.store.inbox(COORDINATOR)
        self.assertEqual([r["receipt_id"] for r in visible], [terminal["receipt_id"]])
        self.assertIsNone(visible[0]["acked_at"])
        self.assertEqual(visible[0]["payload"]["release_duplicate"]["owner_url"], OWNER)
        self.assertIsNone(native._next(self.store, COORDINATOR))
        self.assertGreater(len(self.store.actions()), 1, "old audit rows are retained")
        self.assertEqual(self.store.db.execute("SELECT scope FROM problems").fetchall(), [])

    def test_fake_duplicate_metadata_cannot_make_an_ordinary_codex_terminal_informational(self):
        self.assertTrue(self.tick())
        payload = self.store.inbox(COORDINATOR)[0]["payload"]
        changes = ({"owner_url": "https://github.com/other/project/actions/runs/10"}, {"run_id": 12},
                   {"candidate_tree": "f" * 40}, {"provenance": "unverified"}, {"status_creator_id": 7}, {"owner_status": {}})
        for change in changes:
            with self.subTest(change=change):
                case = DuplicateContracts()
                case.setUp()
                try:
                    case.register()
                    value = copy.deepcopy(payload)
                    value["release_duplicate"].update(change)
                    action = case.store.publish(Key(REPO, SOURCE, 11, 1, "@terminal"), "terminal", value, [COORDINATOR])
                    case.store.deliver_actions({action: None}, COORDINATOR)
                    native._prepare(case.store)
                    row = native._next(case.store, COORDINATOR)
                    self.assertIsNotNone(row)
                    self.assertIn("主动开始处理时", row["message"])
                    self.assertNotIn("native-release-owner-v1", row["message"])
                finally:
                    case.doCleanups()

    def test_codex_information_keeps_owner_and_provenance_without_begin_or_queue(self):
        self.register()
        self.assertTrue(self.tick())
        with patch.object(native, "_queue", side_effect=AssertionError("no native submission")):
            self.assertIsNone(native.pump(self.store))
        row = self.store.db.execute("SELECT * FROM codex_receiver_receipts").fetchone()
        self.assertEqual(row["state"], "information")
        self.assertIn(OWNER, row["message"])
        self.assertIn("native-release-owner-v1", row["message"])
        self.assertNotIn("主动开始处理时", row["message"])
        self.assertNotIn(" begin ", row["message"])
        with patch.dict(os.environ, CODEX_THREAD_ID=COORDINATOR), self.assertRaises(ValueError):
            native.begin(self.store, row["receipt_id"])


if __name__ == "__main__":
    unittest.main()
