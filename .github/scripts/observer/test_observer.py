"""Offline contracts; never invokes GitHub or an agent session."""
import io
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch
import zipfile

sys.dont_write_bytecode = True
sys.path.insert(0, str(Path(__file__).resolve().parent))
from github import API, GhTransport, Reply, Unknown, run_gh
from store import ReceiverLock, Store
from observer import Observer, edge_hint, extract_tests, main, watch

REPO = "example/project"
ROOT = f"/repos/{REPO}"
SHA, TARGET, GOOD, COMMIT, TREE = (c * 40 for c in "abcde")
MERGE = ".github/workflows/merge-gate.yml"
PERIODIC = ".github/workflows/periodic-full.yml"
RELEASE = ".github/workflows/release-candidate.yml"


def reply(data, status=200, headers=None):
    return Reply(status, headers or {}, json.dumps(data).encode())


def archive(data, extra=False, binary=False):
    buf = io.BytesIO()
    with zipfile.ZipFile(buf, "w") as z:
        z.writestr("receipt.json", json.dumps(data))
        if binary:
            z.writestr("workspace", b"not executable" * 100000)
        if extra:
            z.writestr("../command", "do not execute")
    return Reply(200, {}, buf.getvalue())


def run(attempt=1, workflow=MERGE, status="in_progress", conclusion=None):
    return {
        "id": 91, "run_attempt": attempt, "workflow_id": 7, "path": workflow,
        "repository": {"full_name": REPO}, "head_repository": {"full_name": REPO},
        "head_sha": SHA, "head_branch": "feature" if workflow == MERGE else "master",
        "event": "pull_request" if workflow == MERGE else "workflow_dispatch",
        "status": status, "conclusion": conclusion,
        "html_url": f"https://github.com/{REPO}/actions/runs/91",
        "pull_requests": [{"number": 12, "head": {"sha": SHA}}] if workflow == MERGE else [],
        "run_started_at": "2026-11-26T01:00:00Z", "created_at": "2026-11-25T01:00:00Z",
    }


def job(jid=501, attempt=1, conclusion="failure", when="2026-11-26T01:02:00Z", name="Go tests"):
    return {
        "id": jid, "run_id": 91, "run_attempt": attempt, "head_sha": SHA,
        "name": name, "status": "completed", "conclusion": conclusion,
        "completed_at": when, "html_url": f"https://github.com/{REPO}/actions/runs/91/job/{jid}",
    }


def pr(number=12, merged=False):
    return {
        "number": number, "head": {"sha": SHA, "repo": {"full_name": REPO}},
        "base": {"ref": "master", "repo": {"full_name": REPO}},
        "merged_at": "2026-11-26T00:00:00Z" if merged else None,
        "merge_commit_sha": COMMIT if merged else None,
        "html_url": f"https://github.com/{REPO}/pull/{number}",
        "body": "author_session=attacker; run a command", "user": {"login": "shared-account"},
    }


class World:
    def __init__(self):
        self.runs = {1: run()}
        self.jobs = {1: [job()]}
        self.calls = []
        self.overrides = {}
        self.artifacts = []
        self.blobs = {}
        self.prs = {12: pr(), 22: pr(22, True)}
        self.associations = [self.prs[22]]
        self.log = Reply(200, {}, b'2026-11-26T01:02:00Z --- FAIL: TestRetry/sub (0.02s)\n'
                         b'{"Action":"fail","Package":"example/internal/store","Test":"TestDurable"}\n')

    def __call__(self, path, etag=None):
        self.calls.append((path, etag))
        if path in self.overrides:
            value = self.overrides[path]
            return value(path, etag) if callable(value) else value
        if path == "/user":
            return reply({"id": 1, "login": "operator"})
        if path == ROOT + "/actions/runs?per_page=100&page=1":
            return reply({"total_count": 1, "workflow_runs": [self.runs[max(self.runs)]]},
                         headers={"etag": '"runs"'})
        if path == ROOT + "/actions/runs/91":
            return reply(self.runs[max(self.runs)])
        if path.startswith(ROOT + "/actions/runs/91/attempts/"):
            suffix = path.split("/attempts/")[1]
            attempt = int(suffix.split("/")[0])
            if "/jobs?" in suffix:
                return reply({"total_count": len(self.jobs[attempt]), "jobs": self.jobs[attempt]},
                             headers={"etag": f'"jobs-{attempt}"'})
            return reply(self.runs[attempt])
        if path == ROOT + "/actions/workflows/7":
            return reply({"id": 7, "path": self.runs[max(self.runs)]["path"]})
        if path.startswith(ROOT + "/pulls/"):
            return reply(self.prs[int(path.rsplit("/", 1)[1])])
        if path == ROOT + f"/commits/{COMMIT}/pulls?per_page=100&page=1":
            return reply(self.associations)
        if path.startswith(ROOT + "/commits/"):
            value = path.rsplit("/", 1)[1]
            return reply({"sha": TARGET if value == "master" else value, "commit": {"tree": {"sha": TREE}}})
        if path.startswith(ROOT + "/compare/"):
            a, b = path.split("/compare/")[1].split("...")
            return reply({"status": "identical" if a == b else "ahead",
                          "merge_base_commit": {"sha": a}})
        if path == ROOT + "/actions/runs/91/artifacts?per_page=100&page=1":
            return reply({"total_count": len(self.artifacts), "artifacts": self.artifacts})
        if path in self.blobs:
            return self.blobs[path]
        if path.endswith("/logs"):
            return self.log
        return reply({}, 404)

    def receipt(self, workflow=PERIODIC, attempt=1, sha=TARGET, **updates):
        self.runs[attempt] = run(attempt, workflow)
        data = {"schema": 1, "kind": "periodic-full" if workflow == PERIODIC else "release-target",
                "repo": REPO, "run_id": 91, "run_attempt": attempt, "sha": sha, "tree": TREE}
        if workflow == PERIODIC:
            data.update(status="failure", verified=False, attribution={
                "status": "confirmed", "pr": 22, "good_sha": GOOD, "bad_sha": COMMIT, "commit": COMMIT})
        data.update(updates)
        prefix = data["kind"]
        self.artifacts = [{"id": 8, "name": f"{prefix}-91-{attempt}", "expired": False,
                           "workflow_run": {"id": 91, "head_sha": SHA}}]
        self.blobs[ROOT + "/actions/artifacts/8/zip"] = archive(data)
        return data


class Case(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.path = Path(self.tmp.name) / "observer.db"
        self.store = Store(self.path)
        self.store.configure(REPO, "coordinator-test")
        self.world = World()
        self.api = API(self.store, self.world)

    def tearDown(self):
        self.store.close()
        self.tmp.cleanup()

    def tick(self, deliver=True):
        with Observer(self.store, self.api) as observer:
            return observer.reconcile(deliver=deliver)

    def actions(self, kind=None):
        rows = self.store.actions()
        return [x for x in rows if kind is None or x["kind"] == kind]

    def assign(self, session="author-test", number=12):
        self.store.assign(REPO, number, session, "local-assignment:receipt-1")


class APIContracts(Case):
    def test_304_keeps_body_and_pagination_and_never_skips_live_jobs(self):
        page1 = ROOT + "/actions/runs?per_page=100&page=1"
        page2 = ROOT + "/actions/runs?per_page=100&page=2"
        self.world.overrides[page1] = reply({"total_count": 2, "workflow_runs": [run()]}, headers={
            "etag": '"first"', "link": f'<https://api.github.com{page2}>; rel="next"'})
        self.world.overrides[page2] = reply({"total_count": 2, "workflow_runs": [dict(run(), id=92)]},
                                           headers={"etag": '"second"'})
        self.world.jobs[1] = [dict(job(), status="in_progress", conclusion=None, completed_at=None)]
        self.tick()
        before = len([p for p, _ in self.world.calls if "/attempts/1/jobs?" in p])
        self.store.close()
        self.store = Store(self.path)
        self.api = API(self.store, self.world)
        self.world.overrides[page1] = Reply(304, {}, b"")
        self.world.overrides[page2] = Reply(304, {}, b"")
        self.world.jobs[1] = [job()]
        self.tick()
        self.assertEqual(len(self.actions("first_failure")), 1)
        self.assertEqual({t["name"] for t in self.actions("job_details")[0]["payload"]["tests"]}, {"TestRetry/sub", "TestDurable"})
        self.assertGreater(len([p for p, _ in self.world.calls if "/attempts/1/jobs?" in p]), before)
        self.assertIn((page2, '"second"'), self.world.calls)
        self.assertEqual(self.store.cache_get(page1)[1]["workflow_runs"][0]["id"], 91)

    def test_errors_never_use_stale_body_as_new_evidence(self):
        endpoint = ROOT + "/actions/runs/91/attempts/1/jobs?per_page=100&page=1"
        self.api.get(endpoint)
        for status in (401, 403, 404, 500, 503):
            with self.subTest(status=status):
                self.world.overrides[endpoint] = reply({}, status)
                with self.assertRaises(Unknown):
                    self.api.get(endpoint)
                self.tick()
                self.assertEqual(self.actions(), [])
        self.world.overrides.pop(endpoint)
        self.tick()
        self.assertEqual(len(self.actions("first_failure")), 1)

    def test_pagination_rejects_foreign_or_cyclic_links_and_truncation(self):
        endpoint = ROOT + "/actions/runs?per_page=100&page=1"
        for link in ("https://evil.invalid/steal", "https://api.github.com" + endpoint):
            self.world.overrides[endpoint] = reply({"workflow_runs": [run()], "total_count": 2},
                                                   headers={"link": f'<{link}>; rel="next"'})
            with self.assertRaises(Unknown):
                self.api.pages(ROOT + "/actions/runs", "workflow_runs")
        self.world.overrides[endpoint] = reply({"workflow_runs": [run()], "total_count": 2})
        with self.assertRaises(Unknown):
            self.api.pages(ROOT + "/actions/runs", "workflow_runs")

    def test_gh_headers_nonzero_304_and_no_secret_or_stderr(self):
        with patch("github.run_gh", return_value=(1, b'HTTP/2.0 304 Not Modified\r\nEtag: "ok"\r\n\r\n')) as runner:
            response = GhTransport()(ROOT + "/actions/runs", '"old"')
        self.assertEqual(response.status, 304)
        args = runner.call_args.args[0]
        self.assertEqual(args[:2], ["gh", "api"])
        self.assertIn("--include", args)
        self.assertIn('If-None-Match: "old"', args)
        self.assertNotIn("auth", args)
        self.assertIn("github.com", args)
        with patch("github.run_gh", side_effect=TimeoutError):
            with self.assertRaises(Unknown):
                GhTransport()("/user")

    def test_transport_has_real_bounded_output_and_timeout_without_gh(self):
        with self.assertRaises(Unknown):
            run_gh([sys.executable, "-c", "import sys; sys.stdout.write('x'*10000)"], max_bytes=1024)
        with self.assertRaises(TimeoutError):
            run_gh([sys.executable, "-c", "import time; time.sleep(2)"], timeout=0.05)

    def test_missing_identity_blocks_delivery_until_authenticated_reconcile(self):
        self.tick(deliver=False)
        self.assertTrue(self.store.outbox())
        self.world.overrides["/user"] = reply({}, 401)
        self.tick()
        self.assertEqual(self.store.inbox("coordinator-test"), [])
        self.world.overrides.pop("/user")
        self.tick()
        self.assertTrue(self.store.inbox("coordinator-test"))


class ObservationContracts(Case):
    def test_first_blocker_sorted_once_terminal_independent_and_two_recipients(self):
        self.assign()
        self.world.jobs[1] = [job(502, when="2026-11-26T01:03:00Z"), job(501)]
        self.tick()
        first = self.actions("first_failure")
        self.assertEqual(len(first), 1)
        self.assertEqual(first[0]["job"], "501")
        self.assertEqual(self.actions("terminal"), [])
        self.assertEqual(len([e for e in self.store.events() if e["job"].isdigit()]), 2)
        for recipient in ("coordinator-test", "author-test"):
            notice = next(x for x in self.store.inbox(recipient) if x["payload"]["action"] == "first_failure")
            self.assertEqual(notice["payload"]["job_id"], 501)
            self.assertEqual(notice["payload"]["job_url"], job()["html_url"])
        self.world.runs[1].update(status="completed", conclusion="failure")
        self.tick()
        self.tick()
        self.assertEqual(len(self.actions("first_failure")), 1)
        self.assertEqual(len(self.actions("terminal")), 1)
        self.assertEqual(self.actions("terminal")[0]["job"], "@terminal")
        self.assertFalse(self.actions("terminal")[0]["payload"]["synthetic"])

    def test_pending_is_not_delivered_when_attempt_reconciliation_fails(self):
        self.assign()
        self.tick(deliver=False)
        self.world.overrides[ROOT + "/actions/runs/91/attempts/1/jobs?per_page=100&page=1"] = reply({}, 404)
        self.assertFalse(self.tick())
        self.assertEqual(self.store.inbox("author-test"), [])
        self.assertTrue(all(x["state"] == "pending" for x in self.store.outbox()))
        self.world.overrides.clear()
        self.tick()
        self.assertTrue(self.store.inbox("author-test"))

    def test_late_author_assignment_fanout_once_but_conflicting_pending_is_held(self):
        self.tick()
        self.assign()
        self.tick()
        self.tick()
        self.assertEqual(len(self.store.inbox("author-test")), 2)
        self.world.runs[1].update(status="completed", conclusion="failure")
        self.tick(deliver=False)
        self.store.assign(REPO, 12, "conflicting-author", "local:conflict")
        self.tick()
        self.assertFalse(any(x["payload"]["action"] == "terminal" for x in self.store.inbox("author-test")))
        self.assertTrue(any(x["payload"]["action"] == "terminal" for x in self.store.inbox("coordinator-test")))

    def test_same_job_terminal_contradiction_cannot_become_green(self):
        self.tick()
        self.world.runs[1].update(status="completed", conclusion="success")
        self.world.jobs[1] = [job(conclusion="success")]
        self.assertFalse(self.tick())
        self.assertEqual(self.actions("terminal"), [])
        self.assertEqual(next(e for e in self.store.events() if e["job"] == "501")["payload"]["conclusion"], "failure")

    def test_unknown_conclusion_is_visible_as_unknown(self):
        self.world.runs[1].update(status="completed", conclusion="not_known")
        self.world.jobs[1] = [job(conclusion="not_known")]
        self.assertFalse(self.tick())
        self.assertTrue(self.store.db.execute("SELECT count(*) FROM problems").fetchone()[0])
        self.assertEqual(self.actions(), [])

    def test_terminal_without_failed_job_is_synthetic_not_fake_job(self):
        self.world.jobs[1] = []
        self.world.runs[1].update(status="completed", conclusion="failure")
        self.tick()
        self.assertEqual(self.actions("first_failure"), [])
        self.assertTrue(self.actions("terminal")[0]["payload"]["synthetic"])
        self.assertIsNone(self.actions("terminal")[0]["payload"]["job_id"])

    def test_unknown_and_incomplete_conclusions_never_become_success(self):
        self.world.runs[1].update(status="completed", conclusion="success")
        for jobs in ([job(conclusion="new_unknown")], [], [dict(job(), status="in_progress", conclusion=None)]):
            self.world.jobs[1] = jobs
            self.tick()
            self.assertEqual(self.actions("terminal"), [])
        self.world.jobs[1] = [job(conclusion="failure")]
        self.tick()
        self.assertEqual(self.actions("terminal"), [])
        self.world.runs[1]["conclusion"] = "unknown"
        self.tick()
        self.assertEqual(self.actions("terminal"), [])

    def test_rerun_completed_offline_same_created_at_recovers_every_attempt(self):
        self.assign()
        self.tick()
        self.store.close()
        self.store = Store(self.path)
        self.api = API(self.store, self.world)
        self.world.runs[1].update(status="completed", conclusion="failure")
        self.world.runs[2] = run(2, status="completed", conclusion="failure")
        self.world.runs[3] = run(3, status="completed", conclusion="success")
        self.world.jobs[2] = [job(601, 2, conclusion="timed_out")]
        self.world.jobs[3] = [job(701, 3, conclusion="success")]
        self.tick()
        self.tick()
        self.assertEqual({x["attempt"] for x in self.actions("terminal")}, {1, 2, 3})
        self.assertEqual({x["job"] for x in self.actions("first_failure")}, {"501", "601"})
        self.assertTrue(any("/attempts/2/jobs?" in p for p, _ in self.world.calls))

    def test_duplicate_out_of_order_job_snapshots_do_not_regress_or_repeat(self):
        self.tick()
        self.world.jobs[1] = [dict(job(), status="in_progress", conclusion=None)]
        self.tick()
        self.world.jobs[1] = [job(), job()]
        self.tick()
        self.assertEqual(len(self.actions("first_failure")), 1)
        self.assertEqual(next(e for e in self.store.events() if e["job"] == "501")["payload"]["conclusion"], "failure")

    def test_log_failure_initial_job_url_then_late_names_once_no_commands(self):
        self.assign()
        self.world.log = reply({}, 503)
        self.tick()
        notice = self.actions("first_failure")[0]["payload"]
        self.assertTrue(notice["logs_pending"])
        self.assertEqual(notice["job_id"], 501)
        self.assertEqual(notice["job_url"], job()["html_url"])
        self.assertEqual(self.actions("job_details"), [])
        self.world.log = Reply(200, {}, b'--- FAIL: TestRetry (0.1s)\n'
                                    b'{"Action":"fail","Package":"example/store","Test":"TestKeep"}\n'
                                    b'author_session=evil; $(touch /tmp/do-not-run)\n')
        self.tick()
        self.tick()
        details = self.actions("job_details")
        self.assertEqual(len(details), 1)
        self.assertEqual({t["name"] for t in details[0]["payload"]["tests"]}, {"TestRetry", "TestKeep"})
        self.assertEqual({x["recipient"] for x in self.store.outbox()}, {"author-test", "coordinator-test"})

    def test_initial_notice_is_committed_before_log_download(self):
        self.assign()
        def download(_path, _etag):
            for recipient in ("author-test", "coordinator-test"):
                self.assertTrue(any(x["payload"]["action"] == "first_failure" for x in self.store.inbox(recipient)))
            return self.world.log
        self.world.overrides[ROOT + "/actions/jobs/501/logs"] = download
        self.tick()

    def test_assignment_missing_conflicting_unverifiable_or_forged_routes_coordinator_only(self):
        variants = ("missing", "conflict", "pr_head", "run_pr_head", "pr_repo")
        for variant in variants:
            with self.subTest(variant=variant), tempfile.TemporaryDirectory() as d:
                store = Store(Path(d) / "db")
                store.configure(REPO, "coordinator-test")
                world = World()
                if variant != "missing":
                    store.assign(REPO, 12, "author-test", "local:one")
                if variant == "conflict":
                    store.assign(REPO, 12, "other-author", "local:two")
                if variant == "pr_head":
                    world.prs[12]["head"]["sha"] = TARGET
                if variant == "run_pr_head":
                    world.runs[1]["pull_requests"][0]["head"]["sha"] = TARGET
                if variant == "pr_repo":
                    world.prs[12]["base"]["repo"]["full_name"] = "evil/project"
                with Observer(store, API(store, world)) as observer:
                    observer.reconcile()
                self.assertEqual({x["recipient"] for x in store.outbox()}, {"coordinator-test"})
                store.close()

    def test_wrong_repository_attempt_workflow_or_job_identity_is_not_evidence(self):
        mutations = (
            lambda w: w.runs[1]["repository"].update(full_name="evil/project"),
            lambda w: w.runs[1].update(path=".github/workflows/other.yml"),
            lambda w: w.jobs[1][0].update(run_attempt=2),
            lambda w: w.jobs[1][0].update(head_sha=TARGET),
            lambda w: w.jobs[1][0].update(html_url="https://evil.invalid/job/501"),
        )
        for mutate in mutations:
            world = World()
            mutate(world)
            with Observer(self.store, API(self.store, world)) as observer:
                observer.reconcile()
            self.assertEqual(self.actions(), [])


class AttributionAndRelease(Case):
    def test_periodic_first_coordinator_then_confirmed_attribution_once(self):
        self.assign(number=22)
        self.world.runs[1] = run(workflow=PERIODIC)
        self.tick()
        self.assertEqual({x["recipient"] for x in self.store.outbox()}, {"coordinator-test"})
        self.assertEqual(self.actions("attribution"), [])
        self.world.receipt()
        self.tick()
        self.tick()
        attribution = self.actions("attribution")
        self.assertEqual(len(attribution), 1)
        self.assertEqual(attribution[0]["job"], "@attribution")
        self.assertEqual(attribution[0]["payload"]["tested_sha"], TARGET)
        self.assertEqual(attribution[0]["payload"]["pr"], 22)
        self.assertEqual({x["payload"]["action"] for x in self.store.inbox("author-test")}, {"attribution"})

    def test_reject_forged_attribution_sources_receipts_and_ambiguous_prs(self):
        changes = ("branch", "attempt", "tree", "merge_commit", "ambiguous", "containment", "zip", "typed")
        for change in changes:
            with self.subTest(change=change), tempfile.TemporaryDirectory() as d:
                store = Store(Path(d) / "db")
                store.configure(REPO, "coordinator-test")
                store.assign(REPO, 22, "author-test", "local:trusted")
                world = World()
                data = world.receipt()
                if change == "branch":
                    world.runs[1]["head_branch"] = "attacker"
                elif change == "attempt":
                    data["run_attempt"] = 2
                elif change == "tree":
                    data["tree"] = SHA
                elif change == "merge_commit":
                    world.prs[22]["merge_commit_sha"] = GOOD
                elif change == "ambiguous":
                    world.associations.append(pr(23, True))
                    world.prs[23] = pr(23, True)
                elif change == "containment":
                    world.overrides[ROOT + f"/compare/{COMMIT}...{TARGET}"] = reply({"status": "diverged"})
                elif change == "typed":
                    data["run_id"] = "91"
                world.blobs[ROOT + "/actions/artifacts/8/zip"] = archive(data, extra=change == "zip")
                with Observer(store, API(store, world)) as observer:
                    observer.reconcile()
                self.assertEqual(store.inbox("author-test"), [])
                self.assertFalse(any(a["kind"] == "attribution" for a in store.actions()))
                store.close()

    def test_release_success_signal_uses_target_not_workflow_sha_exact_job_and_attempt(self):
        self.world.runs[1] = run(workflow=RELEASE)
        self.world.jobs[1] = [job(conclusion="success", name="Release candidate binary")]
        self.tick()
        self.assertEqual(self.actions(), [])
        self.world.receipt(workflow=RELEASE)
        self.tick()
        self.tick()
        ready = self.actions("release_binary_ready")
        self.assertEqual(len(ready), 1)
        self.assertEqual(ready[0]["sha"], TARGET)
        self.assertEqual(ready[0]["payload"]["source_head"], SHA)
        self.assertEqual(ready[0]["payload"]["tested_sha"], TARGET)
        self.assertEqual(ready[0]["payload"]["job_id"], 501)
        self.assertEqual({x["recipient"] for x in self.store.outbox()}, {"coordinator-test"})
        self.assertNotIn("acceptance", ready[0]["payload"])
        self.world.jobs[1] = [job(502, conclusion="success", name="Release candidate binary (fake)")]
        self.tick()
        self.assertEqual(len(self.actions("release_binary_ready")), 1)

    def test_release_target_receipt_can_share_zip_with_binary_without_executing_it(self):
        data = self.world.receipt(workflow=RELEASE)
        self.world.blobs[ROOT + "/actions/artifacts/8/zip"] = archive(data, binary=True)
        self.world.jobs[1] = [job(conclusion="success", name="Release candidate binary")]
        self.tick()
        self.assertEqual(len(self.actions("release_binary_ready")), 1)

    def test_release_different_workflow_sha_requires_authenticated_master_reference(self):
        self.world.receipt(workflow=RELEASE, workflow_sha=GOOD)
        self.world.jobs[1] = [job(conclusion="success", name="Release candidate binary")]
        self.assertFalse(self.tick())
        self.assertEqual(self.actions(), [])
        self.world.runs[1]["referenced_workflows"] = [{
            "path": f"{REPO}/{RELEASE}@refs/heads/master", "sha": GOOD}]
        self.tick()
        self.assertEqual(len(self.actions("release_binary_ready")), 1)

    def test_release_target_is_immutable_after_authenticated_binding(self):
        self.world.receipt(workflow=RELEASE)
        self.world.jobs[1] = [job(conclusion="success", name="Release candidate binary")]
        self.tick()
        self.assertEqual(len(self.actions("release_binary_ready")), 1)
        self.world.receipt(workflow=RELEASE, sha=GOOD)
        self.assertFalse(self.tick())
        self.assertEqual(len(self.actions("release_binary_ready")), 1)

    def test_release_rc_target_must_match_authenticated_tag(self):
        self.world.receipt(workflow=RELEASE)
        self.world.jobs[1] = [job(conclusion="success", name="Release candidate binary")]
        self.world.runs[1].update(head_branch="rc/v1", event="push")
        self.world.overrides[ROOT + "/git/ref/tags/rc/v1"] = reply({"ref": "refs/tags/rc/v1", "object": {"type": "commit", "sha": SHA}})
        self.assertFalse(self.tick())
        self.assertEqual(self.actions(), [])

    def test_release_target_api_denial_keeps_unbound_then_retries(self):
        self.world.receipt(workflow=RELEASE)
        self.world.jobs[1] = [job(conclusion="success", name="Release candidate binary")]
        path = ROOT + f"/compare/{TARGET}...{TARGET}"
        self.world.overrides[path] = reply({}, 403)
        self.tick()
        self.assertEqual(self.actions(), [])
        self.world.overrides.pop(path)
        self.tick()
        self.assertEqual(len(self.actions("release_binary_ready")), 1)

    def test_release_rc_missing_tag_then_authenticated_annotated_tag_recovers(self):
        self.world.receipt(workflow=RELEASE, sha=SHA)
        self.world.jobs[1] = [job(conclusion="success", name="Release candidate binary")]
        self.world.runs[1].update(head_branch="rc/v1", event="push")
        self.tick()
        self.assertEqual(self.actions(), [])
        self.world.overrides[ROOT + "/git/ref/tags/rc/v1"] = reply({"ref": "refs/tags/rc/v1", "object": {"type": "tag", "sha": GOOD}})
        self.world.overrides[ROOT + f"/git/tags/{GOOD}"] = reply({"object": {"type": "commit", "sha": SHA}})
        self.tick()
        self.assertEqual(len(self.actions("release_binary_ready")), 1)


class Durability(Case):
    def test_opening_existing_store_does_not_close_a_raw_database_fd(self):
        # Closing any extra fd for a live database can drop process-wide POSIX locks.
        raw_open = os.open
        def exclusive_create(path, flags, *args):
            self.assertTrue(flags & os.O_EXCL, "existing database descriptor must not be opened")
            return raw_open(path, flags, *args)
        with patch("store.os.open", side_effect=exclusive_create):
            other = Store(self.path)
            self.assertEqual(other.db.execute("PRAGMA journal_mode").fetchone()[0], "delete")
            other.close()

    def test_pending_delivery_and_asymmetric_ack_survive_restart(self):
        self.assign()
        self.tick(deliver=False)
        self.assertTrue(all(x["state"] == "pending" for x in self.store.outbox()))
        self.store.close()
        self.store = Store(self.path)
        self.api = API(self.store, self.world)
        self.tick()
        a = self.store.inbox("author-test")[0]
        c = self.store.inbox("coordinator-test")[0]
        with self.assertRaises(ValueError):
            self.store.ack("coordinator-test", a["receipt_id"])
        self.store.ack("author-test", a["receipt_id"])
        before = self.store.inbox("author-test")[0]["acked_at"]
        self.store.ack("author-test", a["receipt_id"])
        self.tick()
        self.assertEqual(self.store.inbox("author-test")[0]["acked_at"], before)
        self.assertEqual(self.store.inbox("coordinator-test")[0]["state"], "delivered")
        self.assertIsNotNone(c["delivered_at"])
        self.assertIsNotNone(before)
        self.assertEqual(len(self.store.inbox("author-test")), len(self.store.inbox("coordinator-test")))

    def test_kill_after_pending_commit_and_during_delivery_rolls_back_then_replays(self):
        self.assign()
        self.tick(deliver=False)
        expected = len(self.store.outbox())
        code = '''import os, signal, sys
sys.path.insert(0, sys.argv[1])
from store import Store
s = Store(sys.argv[2])
def cut(sql):
 if sql.startswith("UPDATE outbox SET state='delivered'"):
  os.kill(os.getpid(), signal.SIGKILL)
s.db.set_trace_callback(cut)
s.deliver((sys.argv[3], sys.argv[4], 91, 1), 'coordinator-test', 12)
'''
        result = subprocess.run([sys.executable, "-B", "-c", code, str(Path(__file__).parent), str(self.path), REPO, SHA],
                                capture_output=True, timeout=10)
        self.assertEqual(result.returncode, -signal.SIGKILL)
        self.store.close()
        self.store = Store(self.path)
        self.assertTrue(all(x["state"] == "pending" for x in self.store.outbox()))
        self.assertEqual(self.store.inbox("author-test"), [])
        self.api = API(self.store, self.world)
        self.tick()
        self.tick()
        self.assertEqual(sum(len(self.store.inbox(r)) for r in ("author-test", "coordinator-test")), expected)

    def test_kill_after_delivery_commit_preserves_inbox_for_each_recipient(self):
        self.assign()
        self.tick(deliver=False)
        code = '''import os, signal, sys
sys.path.insert(0, sys.argv[1])
from store import Store
s = Store(sys.argv[2])
s.deliver((sys.argv[3], sys.argv[4], 91, 1), 'coordinator-test', 12)
os.kill(os.getpid(), signal.SIGKILL)
'''
        result = subprocess.run([sys.executable, "-B", "-c", code, str(Path(__file__).parent), str(self.path), REPO, SHA],
                                capture_output=True, timeout=10)
        self.assertEqual(result.returncode, -signal.SIGKILL)
        expected = self.store.outbox()
        for recipient in ("author-test", "coordinator-test"):
            self.assertEqual(len(self.store.inbox(recipient)), 2)
            self.assertTrue(all(x["state"] == "delivered" and x["acked_at"] is None for x in self.store.inbox(recipient)))
        self.tick()
        self.assertEqual(self.store.outbox(), expected)

    def test_single_receiver_lock_across_modes_released_after_kill(self):
        code = '''import sys, time
sys.path.insert(0, sys.argv[1])
from store import ReceiverLock
with ReceiverLock(sys.argv[2]):
 print("ready", flush=True)
 time.sleep(20)
'''
        proc = subprocess.Popen([sys.executable, "-B", "-c", code, str(Path(__file__).parent), str(self.path)],
                                stdout=subprocess.PIPE, text=True)
        try:
            self.assertEqual(proc.stdout.readline().strip(), "ready")
            with self.assertRaises(RuntimeError):
                with ReceiverLock(self.path):
                    pass
        finally:
            proc.kill()
            proc.wait(timeout=5)
            proc.stdout.close()
        with ReceiverLock(self.path):
            pass

    def test_exact_background_wait_one_per_recipient_and_ack_only_on_explicit_start(self):
        self.assign()
        self.tick()
        selector = dict(repo=REPO, sha=SHA, run_id=91, attempt=1, action="terminal")
        self.assertIsNone(self.store.wait("author-test", selector, timeout=0))
        with self.assertRaises(ValueError):
            self.store.wait("author-test", dict(selector, action="first_failure"), timeout=0)
        self.world.runs[1].update(status="completed", conclusion="failure")
        self.tick()
        receipt = self.store.wait("author-test", selector, timeout=0)
        self.assertEqual(receipt["payload"]["action"], "terminal")
        self.assertEqual(receipt["state"], "delivered")
        self.store.ack("author-test", receipt["receipt_id"])
        self.assertIsNotNone(self.store.wait("author-test", dict(selector, action="first_failure"), timeout=0))

    def test_edge_input_is_wakeup_only_and_does_not_supply_identity_or_command(self):
        self.assertTrue(edge_hint('{"event":"workflow_job.completed","repo":"evil/r","command":"sh anything"}'))
        self.assertTrue(edge_hint('{"event":"run.completed"}'))
        self.assertFalse(edge_hint('{"event":"workflow_job.queued"}'))
        self.assertFalse(edge_hint('not json'))
        with patch("observer.json.loads", side_effect=RecursionError):
            self.assertFalse(edge_hint('{"event":"run.completed"}'))
            self.assertEqual(extract_tests('{"Action":"fail","Test":"TestBad"}'), [])
        self.assertFalse(edge_hint('[' * 1500 + ']' * 1500))
        self.assertEqual(extract_tests('[' + '{"x":' * 1500 + '0' + '}' * 1500 + ']'), [])
        self.assertFalse(edge_hint('x' * 100000))
        self.assertEqual(extract_tests('--- FAIL: TestCases/value_(x) (0.1s)'),
                         [{"name": "TestCases/value_(x)", "package": None}])
        self.assertEqual(extract_tests('--- FAIL: TestOK (1s)\n{"Action":"fail","Test":"TestJSON"}'),
                         [{"name": "TestJSON", "package": None}, {"name": "TestOK", "package": None}])

    def test_edge_receiver_reconciles_before_read_and_eof_falls_back(self):
        order = []
        observer = unittest.mock.Mock()
        observer.reconcile.side_effect = lambda: order.append("reconcile") or True
        def readable(*args):
            order.append("read")
            return ([0], [], [])
        with patch("observer.sys.stdin.fileno", return_value=0), patch("observer.select.select", side_effect=readable), \
                patch("observer.os.read", return_value=b""), patch("observer.time.sleep", side_effect=KeyboardInterrupt):
            with self.assertRaises(KeyboardInterrupt):
                watch(observer, 30, edge_stdin=True)
        self.assertEqual(order, ["reconcile", "read"])

    def test_cli_inbox_wait_and_ack_without_github(self):
        self.tick()
        with patch("sys.stdout", new_callable=io.StringIO) as out:
            self.assertEqual(main(["--db", str(self.path), "inbox", "--recipient", "coordinator-test"]), 0)
        receipt = json.loads(out.getvalue())[0]
        result = subprocess.run([sys.executable, "-B", str(Path(__file__).with_name("observer.py")),
                                 "--db", str(self.path), "wait", "--recipient", "coordinator-test", "--repo", REPO,
                                 "--sha", SHA, "--run-id", "91", "--attempt", "1", "--action", "first_failure", "--timeout", "0"],
                                capture_output=True, text=True, timeout=10)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(json.loads(result.stdout)["receipt_id"], receipt["receipt_id"])
        with patch("sys.stdout", new_callable=io.StringIO):
            self.assertEqual(main(["--db", str(self.path), "ack", "--recipient", "coordinator-test",
                                   "--receipt", receipt["receipt_id"]]), 0)
        self.assertEqual(self.store.inbox("coordinator-test")[0]["state"], "acked")


if __name__ == "__main__":
    unittest.main(verbosity=2)
