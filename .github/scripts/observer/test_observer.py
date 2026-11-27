"""Offline contracts; never invokes GitHub or an agent session."""
import copy
import hashlib
import io
import json
import os
from pathlib import Path
import re
import signal
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from unittest.mock import patch
import zipfile

sys.dont_write_bytecode = True
sys.path.insert(0, str(Path(__file__).resolve().parent))
from github import API, GhTransport, Reply, Unknown, run_gh
from store import Key, ReceiverLock, Store
from observer import Observer, edge_hint, extract_tests, main, watch

REPO = "example/project"
ROOT = f"/repos/{REPO}"
SHA, TARGET, GOOD, COMMIT, TREE = (c * 40 for c in "abcde")
MERGE = ".github/workflows/merge-gate.yml"
PERIODIC = ".github/workflows/periodic-full.yml"
RELEASE = ".github/workflows/release-candidate.yml"


def reply(data, status=200, headers=None):
    return Reply(status, headers or {}, json.dumps(data).encode())


def archive(data, extra=False, binary=False, member="receipt.json"):
    buf = io.BytesIO()
    with zipfile.ZipFile(buf, "w") as z:
        z.writestr(member, json.dumps(data))
        if binary:
            z.writestr("workspace", b"not executable" * 100000)
        if extra:
            z.writestr("../command", "do not execute")
    return Reply(200, {}, buf.getvalue())


def go_failures(failures, package="agentworkspace/internal/store"):
    rows = []
    for name, message in failures:
        rows.extend([{"Action": "run", "Package": package, "Test": name},
                     {"Action": "output", "Package": package, "Test": name, "Output": "   store_test.go:11: " + message},
                     {"Action": "fail", "Package": package, "Test": name}])
    rows.append({"Action": "fail", "Package": package})
    return ("\n".join(json.dumps(row, separators=(",", ":")) for row in rows) + "\n").encode()


def run(attempt=1, workflow=MERGE, status="in_progress", conclusion=None):
    return {
        "id": 91, "run_attempt": attempt, "workflow_id": {MERGE: 7, PERIODIC: 8, RELEASE: 9}[workflow], "path": workflow,
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
        "started_at": "2026-11-26T01:01:00Z", "completed_at": when, "html_url": f"https://github.com/{REPO}/actions/runs/91/job/{jid}",
    }


def pr(number=12, merged=False):
    return {
        "number": number, "head": {"sha": SHA, "repo": {"full_name": REPO}},
        "base": {"ref": "master", "sha": GOOD, "repo": {"full_name": REPO}},
        "merged_at": "2026-11-26T00:00:00Z" if merged else None,
        "merge_commit_sha": COMMIT if merged else TARGET,
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
        self.producers = {}
        self.extra_runs = []
        self.commit_associations = None
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
        if path == ROOT:
            return reply({"id": 123, "full_name": REPO})
        if path == ROOT + "/git/matching-refs/tags/rc/":
            return reply([])
        paths = {7: MERGE, 8: PERIODIC, 9: RELEASE}
        for wid, workflow in paths.items():
            summaries = [x for x in [self.runs[max(self.runs)], *self.extra_runs] if x["path"] == workflow]
            if path == ROOT + "/actions/workflows/" + workflow.rsplit("/", 1)[1]:
                return reply({"id": wid, "path": workflow}) if summaries else reply({}, 404)
            if path == ROOT + f"/actions/workflows/{wid}":
                return reply({"id": wid, "path": workflow})
            if path == ROOT + f"/actions/workflows/{wid}/runs?per_page=100&page=1":
                return reply({"total_count": len(summaries), "workflow_runs": summaries}, headers={"etag": '"runs"'})
        if path == ROOT + "/actions/runs?per_page=100&page=1":
            return reply({"total_count": 1, "workflow_runs": [self.runs[max(self.runs)]]},
                         headers={"etag": '"runs"'})
        if path == ROOT + "/actions/runs/91":
            return reply(self.runs[max(self.runs)])
        if path.startswith(ROOT + "/actions/runs/91/attempts/"):
            suffix = path.split("/attempts/")[1]
            attempt = int(suffix.split("/")[0])
            if "/jobs?" in suffix:
                jobs = [*self.jobs[attempt], *self.producers.get(attempt, [])]
                return reply({"total_count": len(jobs), "jobs": jobs},
                             headers={"etag": f'"jobs-{attempt}"'})
            return reply(self.runs[attempt])
        if path.startswith(ROOT + "/pulls/"):
            return reply(self.prs[int(path.rsplit("/", 1)[1])])
        if path == ROOT + f"/commits/{SHA}/pulls?per_page=100&page=1":
            return reply(self.commit_associations if self.commit_associations is not None else [self.prs[12]])
        if path == ROOT + f"/commits/{COMMIT}/pulls?per_page=100&page=1":
            return reply(self.associations)
        if path.startswith(ROOT + "/commits/"):
            value = path.rsplit("/", 1)[1]
            return reply({"sha": TARGET if value == "master" else value, "commit": {"tree": {"sha": TREE}},
                          "parents": [{"sha": GOOD}, {"sha": SHA}] if value == TARGET else [{"sha": GOOD}]})
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
        data = {"schema": 1, "kind": "periodic-full" if workflow == PERIODIC else "release-intent",
                "repo": REPO, "run_id": 91, "run_attempt": attempt, "sha": sha, "tree": TREE}
        if workflow == PERIODIC:
            data.update(status="failure", verified=False, attribution={
                "status": "confirmed", "pr": 22, "good_sha": GOOD, "bad_sha": COMMIT, "commit": COMMIT})
        if workflow == RELEASE:
            data["workflow_sha"] = SHA
        data.update(updates)
        self.artifacts = []
        producer = "Periodic attribution evidence" if workflow == PERIODIC else "Release normalize"
        self.add_artifact(data["kind"], data, producer, attempt=attempt)
        return data

    def add_artifact(self, kind, data, producer, member="receipt.json", attempt=1):
        identity = 8 + len(self.artifacts)
        body = archive(data, member=member)
        self.artifacts.append({"id": identity, "name": f"{kind}-91-{attempt}", "expired": False,
                               "created_at": "2026-11-26T01:03:30Z", "digest": "sha256:" + hashlib.sha256(body.body).hexdigest(),
                               "workflow_run": {"id": 91, "head_sha": SHA}})
        self.blobs[ROOT + f"/actions/artifacts/{identity}/zip"] = body
        jobs = self.producers.setdefault(attempt, [])
        jobs.append(job(400 + identity, attempt, "success", "2026-11-26T01:04:00Z", producer))
        return data

    def replace_blob(self, identity, body):
        self.blobs[ROOT + f"/actions/artifacts/{identity}/zip"] = body
        for artifact in self.artifacts:
            if artifact["id"] == identity:
                artifact["digest"] = "sha256:" + hashlib.sha256(body.body).hexdigest()

    def plan(self, workflow=MERGE, attempt=1):
        self.runs[attempt] = run(attempt, workflow)
        identity = dict(repo=REPO, sha=TARGET, tree=TREE, head=TARGET if workflow == PERIODIC else SHA,
                        base=GOOD, run_id=91, run_attempt=attempt)
        if workflow == MERGE:
            identity.update(pr=12, run_id="91", run_attempt=str(attempt))
            return self.add_artifact("merge-plan", identity, "Merge plan", "merge-plan.json", attempt)
        return self.add_artifact("periodic-full-plan", {"identity": identity}, "Resolve trusted master", "plan.json", attempt)


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
        page1 = ROOT + "/actions/workflows/7/runs?per_page=100&page=1"
        page2 = ROOT + "/actions/workflows/7/runs?per_page=100&page=2"
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
        self.assertEqual({t["name"] for t in self.actions("first_failure")[0]["payload"]["tests"]}, {"TestRetry/sub", "TestDurable"})
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
        self.assertEqual(len(self.store.inbox("author-test")), 1)
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
        self.world.plan(attempt=3)
        self.world.runs[3].update(status="completed", conclusion="success")
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

    def test_one_bounded_log_read_precedes_first_notice_without_holding_for_retry(self):
        self.assign()
        reads = []
        def download(_path, _etag):
            reads.append(_path)
            self.assertEqual(len(self.actions("first_failure")), 1)
            self.assertEqual(self.store.outbox(), [])
            raise Unknown("fixture log timeout")
        self.world.overrides[ROOT + "/actions/jobs/501/logs"] = download
        self.tick()
        self.assertEqual(len(reads), 1)
        for recipient in ("author-test", "coordinator-test"):
            self.assertTrue(self.store.inbox(recipient)[0]["payload"]["logs_pending"])

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
        self.assertEqual(attribution[0]["job"], f"@attribution:{COMMIT}:22")
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
                world.replace_blob(8, archive(data, extra=change == "zip"))
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

    def test_light_intent_is_used_without_downloading_sibling_binary_archive(self):
        data = self.world.receipt(workflow=RELEASE)
        self.world.add_artifact("release-target", dict(data, kind="release-target"), "Legacy binary archive")
        self.world.blobs[ROOT + "/actions/artifacts/9/zip"] = archive(dict(data, kind="release-target"), binary=True)
        self.world.jobs[1] = [job(conclusion="success", name="Release candidate binary")]
        self.tick()
        self.assertEqual(len(self.actions("release_binary_ready")), 1)
        self.assertFalse(any(p == ROOT + "/actions/artifacts/9/zip" for p, _ in self.world.calls))

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
        self.world.overrides[ROOT + f"/git/tags/{GOOD}"] = reply({"sha": GOOD, "object": {"type": "commit", "sha": SHA}})
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
            self.assertEqual(len(self.store.inbox(recipient)), 1)
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

    def test_exact_background_waits_are_independent_and_ack_only_on_explicit_start(self):
        self.assign()
        self.tick()
        selector = dict(repo=REPO, sha=SHA, run_id=91, attempt=1, action="terminal")
        self.assertIsNone(self.store.wait("author-test", selector, timeout=0))
        self.assertIsNotNone(self.store.wait("author-test", dict(selector, action="first_failure"), timeout=0))
        self.assertEqual(self.store.db.execute("SELECT count(*) FROM waits").fetchone()[0], 2)
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
                         [{"name": "TestCases/value_(x)", "package": None, "outcome": "failed"}])
        self.assertEqual(extract_tests('--- FAIL: TestOK (1s)\n{"Action":"fail","Test":"TestJSON"}'),
                         [{"name": "TestJSON", "package": None, "outcome": "failed"}, {"name": "TestOK", "package": None, "outcome": "failed"}])

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


class ReviewEngine(Case):
    def test_empty_native_prs_use_unique_authenticated_commit_association(self):
        self.assign()
        self.world.runs[1]["pull_requests"] = []
        self.tick()
        self.assertTrue(self.store.inbox("author-test"))
        self.assertIn((ROOT + f"/commits/{SHA}/pulls?per_page=100&page=1", None), self.world.calls)
        self.world.commit_associations = [pr(12), pr(13)]
        self.world.runs[1].update(status="completed", conclusion="failure")
        self.tick()
        self.assertFalse(any(r["payload"]["action"] == "terminal" for r in self.store.inbox("author-test")))

    def test_workflow_discovery_ignores_absent_and_unrelated_runs_and_prioritizes_first_notice(self):
        self.world.overrides[ROOT + "/actions/runs?per_page=100&page=1"] = reply({}, 503)
        self.world.extra_runs = [dict(run(), id=999, path=".github/workflows/unrelated.yml")]
        self.tick()
        self.assertEqual(len(self.actions("first_failure")), 1)
        self.assertFalse(any(p == ROOT + "/actions/runs?per_page=100&page=1" for p, _ in self.world.calls))
        self.assertFalse(any("/runs/999" in p for p, _ in self.world.calls))
        self.assertLess(len(self.world.calls), 25)

    def test_settled_history_does_not_refetch_all_jobs_but_offline_attempts_are_recovered(self):
        self.world.runs[1].update(status="completed", conclusion="success")
        self.world.jobs[1] = [job(conclusion="success")]
        self.world.plan()
        self.world.runs[1].update(status="completed", conclusion="success")
        self.tick()
        count = sum("/jobs?" in p for p, _ in self.world.calls)
        self.tick()
        self.assertEqual(sum("/jobs?" in p for p, _ in self.world.calls), count)
        self.store.close()
        self.store = Store(self.path)
        self.api = API(self.store, self.world)
        for attempt in (2, 3):
            self.world.runs[attempt] = run(attempt, status="completed", conclusion="failure")
            self.world.jobs[attempt] = [job(500 + attempt, attempt)]
        self.tick()
        self.assertEqual({a["attempt"] for a in self.actions("first_failure")}, {2, 3})
        self.assertTrue(self.store.db.execute("SELECT count(*) FROM meta WHERE key LIKE 'collect:%'").fetchone()[0])

    def test_partial_rerun_inherited_and_missing_start_jobs_do_not_create_new_actions(self):
        self.world.runs[1].update(status="completed", conclusion="failure")
        self.world.runs[2] = dict(run(2), run_started_at="2026-11-26T02:00:00Z")
        inherited = dict(job(601, 2), started_at="2026-11-26T01:00:00Z", completed_at="2026-11-26T01:30:00Z")
        current = dict(job(602, 2), started_at="2026-11-26T02:01:00Z", completed_at="2026-11-26T02:02:00Z")
        self.world.jobs[2] = [inherited, current]
        self.tick()
        self.assertEqual([a["job"] for a in self.actions("first_failure") if a["attempt"] == 2], ["602"])
        self.world.runs[3] = dict(run(3), run_started_at="2026-11-26T03:00:00Z")
        self.world.jobs[3] = [dict(job(701, 3), started_at=None, completed_at="2026-11-26T03:05:00Z")]
        self.tick()
        self.assertFalse(any(a["attempt"] == 3 for a in self.actions("first_failure")))

    def test_first_notice_has_available_names_without_a_second_details_notice(self):
        self.tick()
        first = self.actions("first_failure")[0]
        self.assertEqual({t["name"] for t in first["payload"]["tests"]}, {"TestRetry/sub", "TestDurable"})
        self.assertFalse(first["payload"]["logs_pending"])
        self.assertEqual(self.actions("job_details"), [])
        self.tick()
        self.assertEqual(len(self.actions("first_failure")), 1)

    def test_release_reports_unbound_failure_then_binds_light_intent_and_both_ready_jobs(self):
        self.world.runs[1] = run(workflow=RELEASE)
        self.tick()
        first = self.actions("first_failure")[0]
        self.assertIsNone(first["payload"]["tested_sha"])
        self.assertTrue(first["payload"]["target_unknown"])
        self.world.receipt(workflow=RELEASE)
        self.world.jobs[1] += [job(502, conclusion="success", name="Release candidate binary"),
                               job(503, conclusion="success", name="Release Full evidence")]
        self.tick()
        self.tick()
        self.assertEqual(len(self.actions("first_failure")), 1)
        for kind in ("release_binary_ready", "release_full_ready"):
            action = self.actions(kind)[0]
            self.assertEqual(action["sha"], TARGET)
            self.assertEqual(action["payload"]["completed_at"], job()["completed_at"])
        detail = next(a for a in self.actions("job_details") if a["payload"].get("detail_kind") == "target_binding")
        self.assertEqual(detail["payload"]["update_of"], first["id"])
        self.assertEqual(detail["payload"]["tested_sha"], TARGET)
        self.assertFalse(any("release-target" in p for p, _ in self.world.calls))

    def test_partial_release_success_is_not_new_work_even_when_api_relabels_attempt(self):
        intent = self.world.receipt(workflow=RELEASE, attempt=2)
        self.world.add_artifact("release-target", dict(intent, kind="release-target"), "Legacy binary archive", attempt=2)
        self.world.runs[1] = run(workflow=RELEASE, status="completed", conclusion="failure")
        self.world.runs[2]["run_started_at"] = "2026-11-26T02:00:00Z"
        self.world.jobs[1] = []
        self.world.jobs[2] = [job(601, 2, conclusion="success", name="Release candidate binary")]
        self.tick()
        self.assertEqual(self.actions("release_binary_ready"), [])
        self.assertFalse(any(a["attempt"] == 2 for a in self.actions("first_failure")))

    def test_merge_plan_late_binding_releases_only_exact_target_subscription(self):
        self.assign()
        self.tick()
        original = self.actions("first_failure")[0]
        selected = dict(repo=REPO, sha=TARGET, run_id=91, attempt=1, action="first_failure", job="501")
        self.assertIsNone(self.store.wait("author-test", selected, timeout=0))
        self.world.plan()
        self.tick()
        receipt = self.store.wait("author-test", selected, timeout=0)
        self.assertIsNotNone(receipt)
        self.assertEqual(receipt["binding"]["sha"], TARGET)
        self.assertEqual(self.actions("first_failure")[0], original)
        self.assertEqual(len(self.actions("first_failure")), 1)

    def test_periodic_plan_digest_producer_and_multiple_confirmed_causes(self):
        self.assign(number=22)
        self.assign("second-author", 23)
        self.world.receipt()
        second = "f" * 40
        self.world.prs[23] = dict(pr(23, True), merge_commit_sha=second)
        self.world.overrides[ROOT + f"/commits/{second}/pulls?per_page=100&page=1"] = reply([self.world.prs[23]])
        receipt = self.world.receipt(attribution={"status": "unknown", "items": [
            {"status": "confirmed", "pr": 22, "good_sha": GOOD, "bad_sha": COMMIT, "commit": COMMIT, "job_id": 501},
            {"status": "confirmed", "pr": 23, "good_sha": GOOD, "bad_sha": second, "commit": second, "job_id": 501},
            {"status": "unknown", "job_id": 501}]})
        self.tick()
        self.tick()
        self.assertEqual({a["job"] for a in self.actions("attribution")},
                         {f"@attribution:{COMMIT}:22", f"@attribution:{second}:23"})
        self.assertEqual(len(self.store.inbox("author-test")), 1)
        self.assertEqual(len(self.store.inbox("second-author")), 1)


class ReviewIntegration(Case):
    def second_run(self, workflow=MERGE):
        current = dict(run(workflow=workflow), id=92, html_url=f"https://github.com/{REPO}/actions/runs/92")
        jobs = [dict(job(602), run_id=92, html_url=f"https://github.com/{REPO}/actions/runs/92/job/602")]
        self.world.extra_runs.append(current)
        self.world.overrides[ROOT + "/actions/runs/92"] = reply(current)
        self.world.overrides[ROOT + "/actions/runs/92/attempts/1"] = reply(current)
        self.world.overrides[ROOT + "/actions/runs/92/attempts/1/jobs?per_page=100&page=1"] = reply({"total_count": 1, "jobs": jobs})
        return current, jobs

    def test_multi_run_only_freshly_verified_actions_leave_pending(self):
        self.assign()
        self.tick(deliver=False)
        self.second_run()
        self.world.overrides[ROOT + "/actions/runs/91/attempts/1/jobs?per_page=100&page=1"] = reply({}, 403)
        self.tick()
        for recipient in ("author-test", "coordinator-test"):
            self.assertEqual({r["payload"]["run_id"] for r in self.store.inbox(recipient)}, {92})
        self.assertTrue(all(r["state"] == "pending" for r in self.store.outbox() if r["payload"]["run_id"] == 91))

    def test_first_job_page_notice_survives_later_jobs_page_error(self):
        self.assign()
        endpoint = ROOT + "/actions/runs/91/attempts/1/jobs"
        jobs = [job()] + [dict(job(n), status="queued", conclusion=None, started_at=None, completed_at=None) for n in range(600, 699)]
        self.world.overrides[endpoint + "?per_page=100&page=1"] = reply({"total_count": 101, "jobs": jobs}, headers={
            "link": f'<https://api.github.com{endpoint}?per_page=100&page=2>; rel="next"'})
        def failed_page(*_):
            for recipient in ("author-test", "coordinator-test"):
                self.assertEqual(len(self.store.inbox(recipient)), 1)
            return reply({}, 503)
        self.world.overrides[endpoint + "?per_page=100&page=2"] = failed_page
        self.assertFalse(self.tick())
        self.assertEqual(len(self.actions("first_failure")), 1)

    def test_original_four_legacy_pending_receipts_recover_on_same_database(self):
        self.assign()
        self.world.log = reply({}, 503)
        self.tick(deliver=False)
        self.world.log = Reply(200, {}, b'--- FAIL: TestOriginalReceipt (0.1s)\n')
        self.tick(deliver=False)
        self.assertEqual(len(self.store.outbox()), 4)
        with self.store.db:
            for row in self.store.actions():
                payload = dict(row["payload"])
                for key in ("workflow_sha", "event", "provenance", "target_unknown", "tested_tree"):
                    payload.pop(key, None)
                self.store.db.execute("UPDATE actions SET payload=? WHERE id=?", (json.dumps(payload), row["id"]))
        original = self.store.actions()
        ids = {r["receipt_id"] for r in self.store.outbox()}
        self.store.close()
        self.store = Store(self.path)
        self.api = API(self.store, self.world)
        self.tick()
        self.assertEqual(self.store.actions(), original)
        self.assertEqual({r["receipt_id"] for r in self.store.outbox()}, ids)
        self.assertTrue(all(r["state"] == "delivered" for r in self.store.outbox()))
        receipt = self.store.inbox("author-test")[0]
        self.store.ack("author-test", receipt["receipt_id"])
        acked = self.store.inbox("author-test")[0]["acked_at"]
        self.tick()
        self.assertEqual(self.store.actions(), original)
        self.assertEqual(self.store.inbox("author-test")[0]["acked_at"], acked)

    def periodic_release_fixture(self, raw, name="Release Full / Go store worker 0", change=None, multiple=False):
        self.assign(number=22)
        failures = [("TestPeriodicFixture", "expected Widget 4 but got Widget 9")]
        if multiple:
            failures.append(("TestSecondFixture", "second assertion"))
        previous = go_failures(failures)
        source = dict(id=501, name="Full suite / Go store worker 0", run_id=91, run_attempt=1,
                      log_sha256=hashlib.sha256(previous).hexdigest())
        items = []
        for test, message in failures:
            signature = dict(tests=[test], messages=[message])
            candidate = dict(package="agentworkspace/internal/store", test=test, signature=signature, race=True, sources=[source])
            encoded = json.dumps(candidate, sort_keys=True).encode()
            item = dict(status="confirmed", pr=22, good_sha=GOOD, bad_sha=TARGET, commit=COMMIT,
                        id=hashlib.sha256(encoded).hexdigest()[:16], sources=[dict(source)],
                        candidate_sha256=hashlib.sha256(encoded).hexdigest(),
                        candidate=dict(package=candidate["package"], test=test, race=True,
                                       signature_sha256=hashlib.sha256(json.dumps(signature, sort_keys=True).encode()).hexdigest()))
            if change:
                change(item)
            items.append(item)
        self.world.receipt(attribution=dict(status="confirmed", items=items))
        self.world.jobs[1][0]["name"] = source["name"]
        self.world.log = Reply(200, {}, previous)
        self.tick()
        current, jobs = self.second_run(RELEASE)
        jobs[0]["name"] = name
        self.world.overrides[ROOT + "/actions/jobs/602/logs"] = Reply(200, {}, raw)
        normalize = dict(job(603, conclusion="success", when="2026-11-26T01:04:00Z", name="Release normalize"),
                         run_id=92, html_url=f"https://github.com/{REPO}/actions/runs/92/job/603")
        self.world.overrides[ROOT + "/actions/runs/92/attempts/1/jobs?per_page=100&page=1"] = reply({"total_count": 2, "jobs": [*jobs, normalize]})
        intent = dict(schema=1, kind="release-intent", repo=REPO, sha=TARGET, tree=TREE, run_id=92, run_attempt=1, workflow_sha=SHA)
        body = archive(intent)
        artifact = dict(id=18, name="release-intent-92-1", expired=False, created_at="2026-11-26T01:03:30Z",
                        digest="sha256:" + hashlib.sha256(body.body).hexdigest(), workflow_run={"id": 92, "head_sha": SHA})
        self.world.overrides[ROOT + "/actions/runs/92/artifacts?per_page=100&page=1"] = reply({"total_count": 1, "artifacts": [artifact]})
        self.world.blobs[ROOT + "/actions/artifacts/18/zip"] = body
        return current, jobs

    def test_release_reuses_reauthenticated_same_sha_cause_without_reassigning_author(self):
        self.periodic_release_fixture(go_failures([("TestPeriodicFixture", "expected Widget 4 but got Widget 9")]))
        def transport(path, etag=None):
            self.assertFalse(self.store.db.in_transaction, "reattribution network work must not hold a SQLite writer")
            return self.world(path, etag)
        self.api = API(self.store, transport)
        self.tick()
        self.tick()
        self.assertEqual(len(self.store.inbox("author-test")), 1)
        reused = [a for a in self.actions("attribution") if a["run_id"] == 92]
        self.assertEqual(len(reused), 1)
        self.assertEqual(reused[0]["payload"]["evidence_run_id"], 91)
        self.assertEqual(sum(p == ROOT + "/actions/jobs/501/logs" for p, _ in self.world.calls), 2)

    def test_release_reuse_distinguishes_infrastructure_test_assertion_and_exact_failure(self):
        cases = [("Release machine evidence", b"missing trusted machine receipt\n", False),
                 ("Release Full / Go store worker 0", go_failures([("TestOtherFailure", "expected Widget 4 but got Widget 9")]), False),
                 ("Release Full / Go store worker 0", go_failures([("TestPeriodicFixture", "expected Quota 1 but got Quota 0")]), False),
                 ("Release Full / Go store worker 0", go_failures([("TestPeriodicFixture", "expected Widget 4 but got Widget 9")]), True)]
        for name, raw, expected in cases:
            with self.subTest(name=name, raw=raw):
                fixture = ReviewIntegration()
                fixture.setUp()
                try:
                    fixture.periodic_release_fixture(raw, name)
                    fixture.tick()
                    first = next(a for a in fixture.actions("first_failure") if a["run_id"] == 92)
                    fixture.tick()
                    matches = [a for a in fixture.actions("attribution") if a["run_id"] == 92]
                    self.assertEqual(len(matches), int(expected))
                    self.assertIsNone(first["payload"]["pr"])
                    self.assertIn(first, fixture.actions("first_failure"))
                    if expected:
                        self.assertEqual(matches[0]["payload"]["tests"][0]["name"], "TestPeriodicFixture")
                        self.assertEqual(matches[0]["payload"]["failure"]["source_job_id"], 501)
                        self.assertEqual(matches[0]["payload"]["job_id"], 602)
                finally:
                    fixture.tearDown()

    def test_release_reuse_fails_closed_for_lane_digest_source_and_timeout(self):
        raw = go_failures([("TestPeriodicFixture", "expected Widget 4 but got Widget 9")])
        for mode in ("lane", "digest", "source", "signature", "timeout", "unnamed", "no_package_end", "denied"):
            with self.subTest(mode=mode):
                fixture = ReviewIntegration()
                fixture.setUp()
                try:
                    def change(item):
                        if mode == "digest":
                            item["sources"][0]["log_sha256"] = "0" * 64
                        elif mode == "source":
                            item["sources"][0]["id"] = 999
                        elif mode == "signature":
                            item["candidate"]["signature_sha256"] = "0" * 64
                    log = raw
                    if mode == "timeout":
                        log = b'panic: test timed out after 10m\nrunning tests:\n\tTestPeriodicFixture (25s)\n'
                    elif mode == "unnamed":
                        log = b'FAIL\tagentworkspace/internal/store\t1.0s\n'
                    elif mode == "no_package_end":
                        log = b'\n'.join(raw.splitlines()[:-1]) + b'\n'
                    fixture.periodic_release_fixture(log, "Release Full / Go store worker 1" if mode == "lane" else "Release Full / Go store worker 0", change)
                    if mode == "denied":
                        fixture.world.overrides[ROOT + "/actions/runs/91/attempts/1/jobs?per_page=100&page=1"] = reply({}, 403)
                    fixture.tick()
                    fixture.tick()
                    self.assertFalse(any(a["run_id"] == 92 for a in fixture.actions("attribution")))
                    self.assertTrue(any(a["run_id"] == 92 for a in fixture.actions("first_failure")))
                finally:
                    fixture.tearDown()

    def test_release_multiple_failures_are_matched_individually(self):
        self.periodic_release_fixture(go_failures([("TestPeriodicFixture", "expected Widget 4 but got Widget 9"),
                                                  ("TestSecondFixture", "different second assertion")]), multiple=True)
        self.tick()
        self.tick()
        matched = [a for a in self.actions("attribution") if a["run_id"] == 92]
        self.assertEqual(len(matched), 1)
        self.assertEqual(matched[0]["payload"]["tests"][0]["name"], "TestPeriodicFixture")
        problems = {row["scope"]: row["reason"] for row in self.store.db.execute("SELECT * FROM problems")}
        self.assertIn("未知", problems["attribution:92:1"])

    def test_release_matches_all_proven_items_once_and_can_match_only_a_later_item(self):
        self.periodic_release_fixture(go_failures([("TestPeriodicFixture", "expected Widget 4 but got Widget 9"),
                                                  ("TestSecondFixture", "second assertion")]), multiple=True)
        self.tick()
        matched = [a for a in self.actions("attribution") if a["run_id"] == 92]
        self.assertEqual({a["payload"]["tests"][0]["name"] for a in matched}, {"TestPeriodicFixture", "TestSecondFixture"})
        self.tick()
        self.assertEqual([a for a in self.actions("attribution") if a["run_id"] == 92], matched)
        fixture = ReviewIntegration()
        fixture.setUp()
        try:
            fixture.periodic_release_fixture(go_failures([("TestPeriodicFixture", "unrelated first assertion"),
                                                          ("TestSecondFixture", "second assertion")]), multiple=True)
            fixture.tick()
            matched = [a for a in fixture.actions("attribution") if a["run_id"] == 92]
            self.assertEqual([a["payload"]["tests"][0]["name"] for a in matched], ["TestSecondFixture"])
        finally:
            fixture.tearDown()

    def test_release_completed_legacy_logs_gain_signatures_without_rewriting_first_receipt(self):
        current, jobs = self.periodic_release_fixture(go_failures([("TestPeriodicFixture", "expected Widget 4 but got Widget 9")]))
        with Observer(self.store, self.api) as observer:
            observer.notice(observer.prepare(current, True), jobs)
        first = next(a for a in self.actions("first_failure") if a["run_id"] == 92)
        rows = self.store.db.execute("SELECT rowid,payload FROM events WHERE repo=? AND run_id IN (91,92) AND job IN ('501','602')", (REPO,)).fetchall()
        with self.store.db:
            for row in rows:
                payload = json.loads(row["payload"])
                self.assertFalse(payload["logs_pending"])
                payload.pop("log_summary", None)
                self.store.db.execute("UPDATE events SET payload=? WHERE rowid=?", (json.dumps(payload), row["rowid"]))
        self.tick()
        matched = [a for a in self.actions("attribution") if a["run_id"] == 92]
        self.assertEqual(len(matched), 1)
        self.assertEqual(matched[0]["payload"]["tests"][0]["name"], "TestPeriodicFixture")
        self.assertIn(first, self.actions("first_failure"))
        reads = sum(p.endswith("/logs") for p, _ in self.world.calls)
        self.tick()
        self.assertEqual(sum(p.endswith("/logs") for p, _ in self.world.calls), reads)

    def test_signature_parser_keeps_missing_assertions_and_malformed_frames_unknown(self):
        from observer import FailureSignatures
        parser = FailureSignatures()
        for line in go_failures([("TestKnown", "known assertion"), ("TestUnnamedReason", "")]).decode().splitlines():
            parser.feed(line)
        snapshot = parser.result()
        self.assertEqual([r["test"] for r in snapshot["records"]], ["TestKnown"])
        self.assertTrue(snapshot["unknown"])
        for frame in ('{"Action":{},"Package":[]}', '{"Action":"fail","Action":"pass","Package":"agentworkspace/internal/store"}'):
            parser = FailureSignatures()
            parser.feed(frame)
            self.assertEqual(parser.result(), {"records": [], "unknown": True})

    def test_cause_owner_is_atomic_with_published_attribution_before_crash(self):
        self.assign(number=22)
        self.world.receipt()
        publish = Observer.publish
        def interrupted(observer, context, key, kind, payload, *args, **kwargs):
            result = publish(observer, context, key, kind, payload, *args, **kwargs)
            if kind == "attribution":
                raise RuntimeError("fixture cut after durable publication")
            return result
        with patch.object(Observer, "publish", interrupted), self.assertRaises(RuntimeError):
            self.tick()
        self.assertEqual(self.store.db.execute("SELECT count(*) FROM cause_owners").fetchone()[0], 1)
        self.assertEqual(self.store.db.execute("SELECT count(*) FROM causes").fetchone()[0], 1)

    def test_authenticated_rate_limit_is_durable_and_does_not_reuse_cached_body(self):
        self.api.get("/user")
        self.world.overrides["/user"] = reply({}, 403, {"retry-after": "60", "x-ratelimit-remaining": "0"})
        with self.assertRaises(Unknown) as error:
            self.api.get("/user")
        self.assertEqual(error.exception.status, 403)
        count = len(self.world.calls)
        with self.assertRaises(Unknown) as cooldown:
            API(self.store, self.world).get("/user")
        self.assertGreater(cooldown.exception.retry_at, 0)
        self.assertEqual(len(self.world.calls), count)

    def test_native_webhook_fragments_wake_before_poll_interval(self):
        clock, chunks = [0.0], []
        payload = json.dumps({"action": "completed", "workflow_job": {"steps": ["x" * 40000]}}).encode() + b"\n"
        chunks.extend(payload[i:i + 4096] for i in range(0, len(payload), 4096))
        observer = unittest.mock.Mock()
        observer.reconcile.side_effect = [True, KeyboardInterrupt]
        def ready(_read, _write, _error, timeout):
            clock[0] += 0.001 if chunks else timeout
            return ([0], [], []) if chunks else ([], [], [])
        with patch("observer.time.monotonic", side_effect=lambda: clock[0]), patch("observer.sys.stdin.fileno", return_value=0), \
                patch("observer.select.select", side_effect=ready), patch("observer.os.read", side_effect=lambda *_: chunks.pop(0)):
            with self.assertRaises(KeyboardInterrupt):
                watch(observer, 30, edge_stdin=True)
        self.assertLess(clock[0], 2)

    def test_trusted_merge_publisher_uses_separate_verified_check_path(self):
        from test_merge_reports import World as PublisherWorld
        world = PublisherWorld()
        world.run.update(run_attempt=1, run_started_at="2026-11-26T01:00:00Z")
        world.jobs[0].update(run_attempt=1, started_at="2026-11-26T01:01:00Z", completed_at="2026-11-26T01:02:00Z")
        world.metadata["publisher"]["run_attempt"] = "1"
        world.overrides.update({
            "/user": reply({"id": 1, "login": "operator"}),
            ROOT + "/actions/workflows/17/runs?per_page=100&page=1": reply({"total_count": 1, "workflow_runs": [world.run]}),
            ROOT + "/actions/workflows/18/runs?per_page=100&page=1": reply({"total_count": 0, "workflow_runs": []}),
            ROOT + "/actions/runs/201/attempts/1/jobs?per_page=100&page=1": reply({"total_count": 1, "jobs": world.jobs}),
            ROOT + "/git/matching-refs/tags/rc/": reply([]),
        })
        self.assign(number=7)
        with Observer(self.store, API(self.store, world)) as observer:
            observer.reconcile()
        self.assertEqual([a["kind"] for a in self.actions()], ["merge_gate_failure"])
        self.assertEqual(len(self.store.inbox("author-test")), 1)
        self.assertTrue(all(a["kind"] not in {"first_failure", "terminal"} for a in self.actions()))
        with tempfile.TemporaryDirectory() as directory:
            store = Store(Path(directory) / "db")
            store.configure(REPO, "coordinator-test")
            store.assign(REPO, 7, "author-test", "fixture local assignment")
            world.run["event"] = "pull_request"
            world.overrides[ROOT + "/actions/workflows/17/runs?per_page=100&page=1"] = reply({"total_count": 1, "workflow_runs": [world.run]})
            with Observer(store, API(store, world)) as observer:
                observer.reconcile()
            self.assertEqual(store.actions(), [])
            store.close()

    def test_standard_runner_includes_node_failure_in_exit_status(self):
        import runpy
        runner = runpy.run_path(str(Path(__file__).parents[1] / "test-ci-observer.py"))
        with patch("unittest.defaultTestLoader.discover", return_value=unittest.TestSuite()), \
                patch("subprocess.run", return_value=subprocess.CompletedProcess([], 7)) as node, \
                patch("sys.stderr", new_callable=io.StringIO):
            self.assertNotEqual(runner["main"]([]), 0)
        self.assertEqual(node.call_args.args[0][0:2], ["node", "--test"])

    def test_pending_inbox_pages_and_exact_read_do_not_include_acked_history(self):
        self.tick()
        receipt = self.store.inbox("coordinator-test")[0]
        self.store.ack("coordinator-test", receipt["receipt_id"])
        self.second_run()
        self.tick()
        with patch("sys.stdout", new_callable=io.StringIO) as output, patch("sys.stderr", new_callable=io.StringIO):
            code = main(["--db", str(self.path), "inbox", "--recipient", "coordinator-test", "--pending", "--limit", "1"])
        self.assertEqual(code, 0)
        rows = json.loads(output.getvalue())
        self.assertEqual(len(rows), 1)
        self.assertEqual(rows[0]["payload"]["run_id"], 92)
        self.assertEqual(self.store.inbox("coordinator-test", pending=True, limit=1, after=rows[0]["receipt_id"]), [])
        original = self.store.inbox("coordinator-test", receipt_id=receipt["receipt_id"])
        self.assertEqual(len(original), 1)
        self.assertEqual(original[0]["state"], "acked")
        self.assertEqual(self.store.inbox("another-session", receipt_id=receipt["receipt_id"]), [])

    def test_cli_multi_wait_job_identity_and_precise_cancel(self):
        self.tick()
        common = ["--recipient", "coordinator-test", "--repo", REPO, "--sha", SHA, "--run-id", "91", "--attempt", "1"]
        with patch("sys.stdout", new_callable=io.StringIO), patch("sys.stderr", new_callable=io.StringIO):
            self.assertEqual(main(["--db", str(self.path), "wait", *common, "--action", "first_failure", "--job", "501", "--timeout", "0"]), 0)
            self.assertEqual(main(["--db", str(self.path), "wait", *common, "--action", "terminal", "--timeout", "0"]), 3)
            self.assertEqual(main(["--db", str(self.path), "cancel-wait", *common, "--action", "first_failure", "--job", "501"]), 0)
        rows = self.store.db.execute("SELECT selector FROM waits").fetchall()
        self.assertEqual(len(rows), 1)
        self.assertEqual(json.loads(rows[0][0])["action"], "terminal")

    def test_wait_registration_recovers_one_safe_busy_without_ack_or_cancel(self):
        actor = Store(self.path, initialize=False)
        actor.db.execute("PRAGMA busy_timeout=0")
        self.store.db.execute("BEGIN IMMEDIATE")
        selected = dict(repo=REPO, sha=SHA, run_id=91, attempt=1, action="terminal")
        try:
            with patch("store.time.sleep", side_effect=lambda _: self.store.db.rollback()) as recovery:
                self.assertIsNone(actor.wait("coordinator-test", selected, timeout=0))
            self.assertEqual(recovery.call_count, 1)
            self.assertEqual(actor.db.execute("SELECT count(*) FROM waits").fetchone()[0], 1)
        finally:
            self.store.db.rollback()
            actor.close()

    def test_unknown_job_conclusion_can_later_become_authoritatively_known(self):
        self.world.jobs[1] = [job(conclusion="future_unknown")]
        self.tick()
        self.assertEqual(self.actions(), [])
        self.world.jobs[1] = [job()]
        self.tick()
        self.assertEqual(len(self.actions("first_failure")), 1)

    def test_strict_pagination_rejects_foreign_last_and_wrong_query(self):
        endpoint = ROOT + "/actions/runs?per_page=100&page=1"
        for link in ('<https://evil.invalid/runs?page=1>; rel="last"',
                     '<https://api.github.com' + ROOT + '/actions/runs?per_page=100&page=2&actor=other>; rel="next"'):
            self.world.overrides[endpoint] = reply({"total_count": 1, "workflow_runs": [run()]}, headers={"link": link})
            with self.assertRaises(Unknown):
                self.api.pages(ROOT + "/actions/runs", "workflow_runs")

    def test_malformed_summary_does_not_hold_an_independent_verified_run(self):
        bad = dict(run(), id=999, repository={"full_name": "other/repo"})
        self.world.overrides[ROOT + "/actions/workflows/7/runs?per_page=100&page=1"] = reply({"total_count": 2, "workflow_runs": [bad, run()]})
        self.tick()
        self.assertEqual(len(self.actions("first_failure")), 1)

    def test_gh_json_requests_have_a_short_timeout_budget(self):
        with patch("github.run_gh", return_value=(0, b"HTTP/2.0 200 OK\r\n\r\n{}")) as runner:
            GhTransport()("/user")
        self.assertLessEqual(runner.call_args.kwargs["timeout"], 5)


class ReviewSafety(Case):
    def test_active_run_is_not_queued_behind_large_settled_history(self):
        history = [dict(run(status="completed", conclusion="success"), id=n,
                        html_url=f"https://github.com/{REPO}/actions/runs/{n}") for n in range(1000, 1030)]
        self.world.extra_runs = history
        self.tick()
        self.assertEqual(len(self.actions("first_failure")), 1)
        native_calls = [p for p, _ in self.world.calls if re.fullmatch(re.escape(ROOT) + r"/actions/runs/[0-9]+", p)]
        self.assertEqual(native_calls[0], ROOT + "/actions/runs/91")

    def test_success_wait_uses_tested_sha_not_immutable_source_key(self):
        self.world.plan()
        self.world.runs[1].update(status="completed", conclusion="success")
        self.world.jobs[1] = [job(conclusion="success")]
        self.tick()
        selected = dict(repo=REPO, sha=SHA, run_id=91, attempt=1, action="terminal")
        self.assertIsNone(self.store.wait("coordinator-test", selected, timeout=0))
        self.assertIsNotNone(self.store.wait("coordinator-test", dict(selected, sha=TARGET), timeout=0))

    def test_corrupt_legacy_source_does_not_prevent_an_independent_run(self):
        self.store.publish(Key(REPO, SHA, 90, 1, "500"), "first_failure", {}, ["coordinator-test"])
        self.tick()
        self.assertTrue(any(a["run_id"] == 91 for a in self.actions("first_failure")))
        self.assertTrue(all(r["state"] == "pending" for r in self.store.outbox() if r["payload"]["run_id"] == 90))

    def test_large_logs_stream_without_retaining_the_download_and_timeout_is_pending(self):
        import tracemalloc
        from observer import TestLog
        program = ("import sys; s=sys.stdout.buffer; s.write(b'HTTP/2.0 200 OK\\r\\n\\r\\n'); "
                   "[s.write(b'x'*60000+b'\\n') for _ in range(180)]; "
                   "s.write(b'--- FAIL: TestAfterTenMiB (0.1s)\\n')")
        parser = TestLog()
        tracemalloc.start()
        try:
            with patch.object(GhTransport, "command", return_value=[sys.executable, "-B", "-c", program]):
                complete = GhTransport().stream(ROOT + "/actions/jobs/501/logs", parser.feed, 5, 16 * 1024 * 1024)
            _, peak = tracemalloc.get_traced_memory()
        finally:
            tracemalloc.stop()
        self.assertTrue(complete)
        self.assertEqual(parser.result()[0]["name"], "TestAfterTenMiB")
        self.assertLess(peak, 4 * 1024 * 1024)
        slow = "import sys,time; print('HTTP/2.0 200 OK\\r\\n\\r\\n--- FAIL: TestBeforeTimeout (1s)',flush=True); time.sleep(10)"
        parser = TestLog()
        with patch.object(GhTransport, "command", return_value=[sys.executable, "-B", "-c", slow]):
            self.assertFalse(GhTransport().stream(ROOT + "/actions/jobs/501/logs", parser.feed, 0.1, 1024 * 1024))
        self.assertEqual(parser.result()[0]["name"], "TestBeforeTimeout")

    def test_stream_oversized_lines_are_pending_and_oversized_headers_are_not_evidence(self):
        from observer import TestLog
        parser = TestLog()
        def oversized_line(_args, **kwargs):
            kwargs["consume"](b"HTTP/2.0 200 OK\r\n\r\n" + b"x" * 65536)
            kwargs["consume"](b"x\n--- FAIL: TestAfterLongLine (0.1s)\n")
            return 0, b""
        with patch("github.run_gh", side_effect=oversized_line):
            self.assertFalse(GhTransport().stream(ROOT + "/actions/jobs/501/logs", parser.feed, 1, 1024 * 1024))
        self.assertEqual(parser.result()[0]["name"], "TestAfterLongLine")
        def oversized_header(_args, **kwargs):
            raw = b"HTTP/2.0 200 OK\r\nX-Large: " + b"x" * 70000 + b"\r\n\r\n--- FAIL: TestUntrustedHeader (1s)\n"
            kwargs["consume"](raw[:65536])
            kwargs["consume"](raw[65536:])
            return 0, b""
        with patch("github.run_gh", side_effect=oversized_header), self.assertRaises(Unknown):
            GhTransport().stream(ROOT + "/actions/jobs/501/logs", lambda _: self.fail("invalid headers must not emit test evidence"), 1, 1024 * 1024)

    def test_legacy_single_wait_schema_migrates_without_losing_registration(self):
        selected = dict(repo=REPO, sha=SHA, run_id=91, attempt=1, action="terminal")
        self.store.wait("coordinator-test", selected, timeout=0)
        original = self.store.db.execute("SELECT * FROM waits").fetchone()
        with self.store.db:
            self.store.db.execute("DROP TABLE waits")
            self.store.db.execute("CREATE TABLE waits(recipient TEXT PRIMARY KEY,selector TEXT NOT NULL)")
            self.store.db.execute("INSERT INTO waits VALUES(?,?)", tuple(original))
            self.store.db.execute("PRAGMA user_version=0")
        self.store.close()
        self.store = Store(self.path)
        self.store.wait("coordinator-test", dict(selected, action="first_failure"), timeout=0)
        self.store.cancel_wait("coordinator-test", dict(selected, action="first_failure"))
        self.assertEqual(tuple(self.store.db.execute("SELECT * FROM waits").fetchone()), tuple(original))

    def test_schema_upgrade_is_excluded_by_the_existing_receiver_lock(self):
        with self.store.db:
            self.store.db.execute("PRAGMA user_version=0")
        with ReceiverLock(self.path), self.assertRaises(RuntimeError):
            Store(self.path)
        self.assertEqual(self.store.db.execute("PRAGMA user_version").fetchone()[0], 0)

    def test_sigkill_between_legacy_wait_rename_and_create_preserves_subscription(self):
        selected = dict(repo=REPO, sha=SHA, run_id=91, attempt=1, action="terminal")
        with self.store.db:
            self.store.db.execute("DROP TABLE waits")
            self.store.db.execute("CREATE TABLE waits(recipient TEXT PRIMARY KEY,selector TEXT NOT NULL)")
            self.store.db.execute("INSERT INTO waits VALUES(?,?)", ("coordinator-test", json.dumps(selected, sort_keys=True, separators=(",", ":"))))
            self.store.db.execute("PRAGMA user_version=0")
        original = tuple(self.store.db.execute("SELECT * FROM waits").fetchone())
        script = '''import os, signal, sys
sys.path.insert(0, sys.argv[1])
from store import Store
import sqlite3
class Crash:
    def __init__(self, db): self.db = db
    def __getattr__(self, name): return getattr(self.db, name)
    def __enter__(self): return self.db.__enter__()
    def __exit__(self, *args): return self.db.__exit__(*args)
    def execute(self, sql, *args):
        if sql.startswith("CREATE TABLE waits "):
            os.kill(os.getpid(), signal.SIGKILL)
        return self.db.execute(sql, *args)
connect = sqlite3.connect
sqlite3.connect = lambda *a, **k: Crash(connect(*a, **k))
store = Store(sys.argv[2])
store.close()
'''
        result = subprocess.run([sys.executable, "-B", "-c", script, str(Path(__file__).resolve().parent), str(self.path)], capture_output=True, timeout=5)
        self.assertEqual(result.returncode, -signal.SIGKILL)
        self.store.close()
        self.store = Store(self.path)
        migrated = self.store.db.execute("SELECT * FROM waits").fetchone()
        self.assertIsNotNone(migrated, "the pre-upgrade subscription must survive the killed migration")
        self.assertEqual(tuple(migrated), original)

    def test_periodic_actual_bundle_members_and_strict_plan_identity(self):
        self.world.plan(PERIODIC)
        body = io.BytesIO(self.world.blobs[ROOT + "/actions/artifacts/8/zip"].body)
        with zipfile.ZipFile(body, "a") as bundle:
            bundle.writestr("full-plan.json", "{}")
            bundle.writestr("master-ref.json", "{}")
            bundle.writestr("api/response.json", "{}")
        self.world.replace_blob(8, Reply(200, {}, body.getvalue()))
        self.tick()
        self.assertEqual(self.store.target(REPO, 91, 1)["sha"], TARGET)
        self.assertEqual(self.store.target(REPO, 91, 1)["provenance"], "periodic-full-plan")
        for change in ("typed", "producer", "digest", "time", "tree"):
            with self.subTest(change=change), tempfile.TemporaryDirectory() as directory:
                store = Store(Path(directory) / "db")
                store.configure(REPO, "coordinator-test")
                world = World()
                data = world.plan(PERIODIC)
                if change == "typed":
                    data["identity"]["run_id"] = "91"
                if change == "tree":
                    data["identity"]["tree"] = GOOD
                world.replace_blob(8, archive(data, member="plan.json"))
                if change == "producer":
                    world.producers[1][0]["conclusion"] = "failure"
                elif change == "digest":
                    world.artifacts[0]["digest"] = "sha256:" + "0" * 64
                elif change == "time":
                    world.artifacts[0]["created_at"] = "2026-11-25T01:00:00Z"
                with Observer(store, API(store, world)) as observer:
                    observer.reconcile()
                self.assertIsNone(store.target(REPO, 91, 1))
                self.assertEqual(sum(a["kind"] == "first_failure" for a in store.actions()), 1)
                store.close()

    def test_rc_hold_offline_and_partial_rerun_cannot_release_old_ready_as_new_attempt(self):
        self.world.receipt(RELEASE)
        self.world.jobs[1] = [job(conclusion="success", name="Release candidate binary")]
        self.tick(deliver=False)
        original = self.actions("release_binary_ready")[0]["id"]
        self.world.runs[2] = dict(run(2, RELEASE), run_started_at="2026-11-26T02:00:00Z")
        self.world.jobs[2] = [dict(job(conclusion="success", name="Release candidate binary"), run_attempt=2)]
        path = ROOT + "/actions/runs/91/attempts/1/jobs?per_page=100&page=1"
        self.world.overrides[path] = reply({}, 403)
        self.store.close()
        self.store = Store(self.path)
        self.api = API(self.store, self.world)
        self.tick()
        self.assertEqual(self.store.inbox("coordinator-test"), [])
        self.assertEqual([a["id"] for a in self.actions("release_binary_ready")], [original])
        self.world.overrides.pop(path)
        self.tick()
        self.assertEqual({r["payload"]["attempt"] for r in self.store.inbox("coordinator-test") if r["payload"]["action"] == "release_binary_ready"}, {1})
        selected = dict(repo=REPO, sha=TARGET, run_id=91, attempt=2, action="release_binary_ready", job="501")
        self.assertIsNone(self.store.wait("coordinator-test", selected, timeout=0))

    def test_master_controlled_merge_dispatch_uses_verified_plan_not_run_title_for_author(self):
        self.assign()
        data = self.world.plan()
        self.world.runs[1].update(event="workflow_dispatch", head_branch="master", head_sha=GOOD,
                                  display_title="Merge validation PR 999 head untrusted")
        for item in self.world.jobs[1] + self.world.producers[1]:
            item["head_sha"] = GOOD
        self.world.artifacts[0]["workflow_run"]["head_sha"] = GOOD
        self.world.prs[12]["head"]["repo"]["id"] = 123
        self.world.prs[12]["head"]["ref"] = "feature"
        data.update(controller_sha=GOOD, source={"repo_id": 123, "repo": REPO, "ref": "feature", "base_ref": "master"})
        self.world.replace_blob(8, archive(data, member="merge-plan.json"))
        self.tick()
        first = self.actions("first_failure")[0]
        self.assertEqual(first["payload"]["source_head"], GOOD)
        self.assertIsNone(first["payload"]["tested_sha"])
        self.assertEqual(self.store.target(REPO, 91, 1)["pr_head"], SHA)
        self.assertEqual(sum(r["payload"]["action"] == "first_failure" for r in self.store.inbox("author-test")), 1)
        details = [r["payload"] for r in self.store.inbox("author-test") if r["payload"]["action"] == "job_details"]
        self.assertEqual(details[0]["pr"], 12)
        self.assertEqual(details[0]["tested_sha"], TARGET)
        self.tick()
        self.assertEqual(self.actions("first_failure")[0], first)
        self.assertFalse(any("/pulls/999" in p for p, _ in self.world.calls))

    def test_target_proof_cannot_change_control_or_pr_while_sha_tree_stay_equal(self):
        proof = dict(source_head=SHA, workflow_sha=SHA, provenance="merge-plan", pr=12, pr_head=SHA,
                     source={"repo_id": 123, "repo": REPO, "ref": "feature", "base_ref": "master"})
        self.store.bind_target(REPO, 91, 1, "merge-plan", TARGET, TREE, proof)
        before = self.store.target(REPO, 91, 1)
        for updates in ({"workflow_sha": GOOD}, {"pr": 13}, {"pr_head": COMMIT}, {"source": dict(proof["source"], repo_id=999)}):
            with self.subTest(updates=updates), self.assertRaises(ValueError):
                self.store.bind_target(REPO, 91, 1, "merge-plan", TARGET, TREE, dict(proof, **updates))
            self.assertEqual(self.store.target(REPO, 91, 1), before)

    def test_native_lower_attempt_inheritance_does_not_hide_a_genuine_new_binary(self):
        self.world.receipt(RELEASE, attempt=2)
        self.world.runs[1] = run(workflow=RELEASE, status="completed", conclusion="failure")
        self.world.runs[2]["run_started_at"] = "2026-11-26T02:00:00Z"
        producer = self.world.producers[2][0]
        producer.update(started_at="2026-11-26T02:00:05Z", completed_at="2026-11-26T02:01:00Z")
        self.world.artifacts[0]["created_at"] = "2026-11-26T02:00:30Z"
        old = job(601, 1)
        self.world.jobs[1] = [old]
        self.world.jobs[2] = [old, dict(job(602, 2, conclusion="success", name="Release candidate binary"),
                                      started_at="2026-11-26T02:02:00Z", completed_at="2026-11-26T02:03:00Z")]
        self.tick()
        self.assertEqual([(a["attempt"], a["job"]) for a in self.actions("release_binary_ready")], [(2, "602")])
        self.assertFalse(any(a["attempt"] == 2 for a in self.actions("first_failure")))

    def test_controller_plan_forgeries_keep_author_unknown(self):
        for change in ("controller", "source", "tree"):
            with self.subTest(change=change), tempfile.TemporaryDirectory() as directory:
                store = Store(Path(directory) / "db")
                store.configure(REPO, "coordinator-test")
                store.assign(REPO, 12, "author-test", "fixture assignment")
                world = World()
                data = world.plan()
                world.runs[1].update(event="workflow_dispatch", head_branch="master", head_sha=GOOD)
                for item in world.jobs[1] + world.producers[1]:
                    item["head_sha"] = GOOD
                world.artifacts[0]["workflow_run"]["head_sha"] = GOOD
                world.prs[12]["head"]["repo"]["id"] = 123
                world.prs[12]["head"]["ref"] = "feature"
                data.update(controller_sha=GOOD, source={"repo_id": 123, "repo": REPO, "ref": "feature", "base_ref": "master"})
                if change == "controller":
                    data["controller_sha"] = COMMIT
                elif change == "source":
                    data["source"]["repo_id"] = 999
                else:
                    data["tree"] = GOOD
                world.replace_blob(8, archive(data, member="merge-plan.json"))
                with Observer(store, API(store, world)) as observer:
                    observer.reconcile()
                self.assertEqual(store.inbox("author-test"), [])
                self.assertIsNone(store.target(REPO, 91, 1))
                self.assertTrue(store.inbox("coordinator-test"))
                store.close()

    def test_deferred_log_budget_advances_past_an_already_completed_second_job(self):
        for identity in ("502", "503"):
            self.store.wait("coordinator-test", dict(repo=REPO, sha=SHA, run_id=91, attempt=1, action="job_details", job=identity), timeout=0)
        self.world.jobs[1] += [job(502), job(503)]
        self.tick()
        self.assertEqual([a["job"] for a in self.actions("job_details")], ["502"])
        self.tick()
        self.assertEqual({a["job"] for a in self.actions("job_details")}, {"502", "503"})
        self.assertEqual(len(self.actions("first_failure")), 1)

    def test_tag_worker_shares_receiver_lock_and_failure_does_not_hold_workflow_notice(self):
        calls = []
        def tags(observer, deliver, *, auth_context):
            self.assertEqual(auth_context["repo"], REPO)
            self.assertEqual(auth_context["identity"]["id"], 1)
            self.assertIsNotNone(observer.lock)
            self.assertIs(observer.store, self.store)
            calls.append(deliver)
            raise Unknown("fixture tag listing unavailable")
        with patch("rc_refs.reconcile_tags", side_effect=tags):
            self.tick(deliver=False)
            self.tick()
        self.assertEqual(calls, [False, True])
        self.assertTrue(self.store.inbox("coordinator-test"))


class FirstNoticeCheckpoint(Case):
    def test_first_action_is_committed_but_not_deliverable_before_log_io(self):
        self.assign()
        checkpoint = []
        def download(_path, _consume, *, timeout):
            self.assertFalse(self.store.db.in_transaction)
            reader = Store(self.path, initialize=False)
            try:
                actions = [a for a in reader.actions() if a["kind"] == "first_failure"]
                self.assertEqual(len(actions), 1)
                checkpoint.append(actions[0])
                self.assertEqual((actions[0]["job"], actions[0]["payload"]["source_head"], actions[0]["payload"]["pr"]), ("501", SHA, 12))
                self.assertEqual(timeout, 5)
                self.assertEqual(reader.outbox(), [])
                reader.deliver((REPO, SHA, 91, 1), "coordinator-test", 12)
                self.assertEqual(reader.inbox("coordinator-test"), [])
                self.assertEqual(reader.inbox("author-test"), [])
            finally:
                reader.close()
            raise Unknown("fixture log not yet available", status=404)
        with patch.object(self.api, "lines", side_effect=download):
            self.tick()
        first = self.actions("first_failure")[0]
        self.assertEqual((first["id"], first["created_at"]), (checkpoint[0]["id"], checkpoint[0]["created_at"]))
        self.assertEqual(first["payload"]["tests"], [])
        self.assertEqual(len(self.store.inbox("author-test")), 1)

    def test_sigkill_at_first_log_read_recovers_the_same_winner_and_binding(self):
        self.assign()
        code = '''import os, signal, sys
sys.path.insert(0, sys.argv[1])
from github import API
from observer import Observer
from store import Store
from test_observer import World
s = Store(sys.argv[2], initialize=False)
a = API(s, World())
def stop(*args, **kwargs):
 assert not s.db.in_transaction
 assert len(s.actions()) == 1 and not s.outbox()
 os.kill(os.getpid(), signal.SIGKILL)
a.lines = stop
with Observer(s, a) as observer:
 observer.reconcile()
'''
        result = subprocess.run([sys.executable, "-B", "-c", code, str(Path(__file__).parent.resolve()), str(self.path)], capture_output=True, timeout=15)
        self.assertEqual(result.returncode, -signal.SIGKILL, result.stderr.decode())
        self.store.close()
        self.store = Store(self.path, initialize=False)
        self.api = API(self.store, self.world)
        original = self.actions("first_failure")[0]
        binding = self.store.root_sha(REPO, 91, 1, SHA, None)
        self.assertEqual(self.store.outbox(), [])
        self.world.jobs[1].insert(0, job(502, when="2026-11-26T01:01:30Z"))
        self.world.log = Reply(200, {}, b'--- FAIL: TestRecoveredWinner (0.1s)\n')
        self.tick()
        first = self.actions("first_failure")[0]
        self.assertEqual((first["id"], first["created_at"], first["job"], first["sha"]),
                         (original["id"], original["created_at"], "501", binding))
        self.assertEqual(first["payload"]["pr"], original["payload"]["pr"])
        before = {recipient: self.store.inbox(recipient) for recipient in ("author-test", "coordinator-test")}
        self.tick()
        for recipient, receipts in before.items():
            self.assertEqual(self.store.inbox(recipient), receipts)
            self.assertEqual(len([r for r in receipts if r["payload"]["action"] == "first_failure"]), 1)

    def test_delayed_authenticated_named_stream_does_not_wait_for_a_slow_tail(self):
        self.assign()
        owner, budgets, elapsed = self, [], []
        class Transport:
            def __call__(self, path, etag=None):
                return owner.world(path, etag)
            def stream(self, path, consume, timeout, limit):
                budgets.append(timeout)
                started = time.monotonic()
                try:
                    return GhTransport().stream(path, consume, timeout, limit)
                finally:
                    elapsed.append(time.monotonic() - started)
        self.api = API(self.store, Transport())
        named = json.dumps({"Action": "fail", "Package": "agentworkspace/internal/preflight", "Test": "TestCIObserverRequiredFailureReceipt"})
        script = "import time; time.sleep(1.1); print('HTTP/2.0 200 OK\\r\\n\\r\\n',flush=True); time.sleep(1.1); print(" + repr(named) + ",flush=True); time.sleep(30)"
        with patch.object(GhTransport, "command", return_value=[sys.executable, "-B", "-c", script]):
            self.tick()
        self.assertEqual(budgets, [5])
        self.assertLess(elapsed[0], 4.8, "first named evidence must not wait for the slow tail or the full five seconds")
        first = self.actions("first_failure")[0]
        for recipient in ("author-test", "coordinator-test"):
            notice = next(r["payload"] for r in self.store.inbox(recipient) if r["payload"]["action"] == "first_failure")
            self.assertEqual(notice["tests"], [{"name": "TestCIObserverRequiredFailureReceipt", "package": "agentworkspace/internal/preflight", "outcome": "failed"}])
            self.assertEqual((notice["job_id"], notice["job_url"]), (501, job()["html_url"]))
            self.assertTrue(notice["logs_pending"])
        self.api = API(self.store, self.world)
        self.world.log = Reply(200, {}, (named + "\n").encode())
        self.tick()
        self.tick()
        self.assertEqual(self.actions("first_failure"), [first])
        self.assertEqual(len(self.actions("job_details")), 1)

    def test_partial_names_survive_unknown_rereads_and_delivered_history_is_frozen(self):
        self.assign()
        def partial(_path, consume, *, timeout):
            self.assertEqual(timeout, 5)
            consume('--- FAIL: TestAlreadyKnown (0.1s)')
            return False
        with patch.object(self.api, "lines", side_effect=partial):
            self.tick()
        receipt = self.store.inbox("author-test")[0]
        self.store.ack("author-test", receipt["receipt_id"])
        frozen = self.store.inbox("author-test")
        first = self.actions("first_failure")[0]
        self.store.close()
        self.store = Store(self.path, initialize=False)
        self.api = API(self.store, self.world)
        self.world.log = reply({}, 404)
        self.tick()
        known = next(e["payload"] for e in self.store.events() if e["job"] == "501")
        self.assertEqual(known["tests"][0]["name"], "TestAlreadyKnown")
        self.assertEqual(self.store.inbox("author-test"), frozen)
        self.world.log = Reply(200, {}, b'--- FAIL: TestAfterRetry (0.1s)\n')
        self.tick()
        self.tick()
        self.assertEqual(self.actions("first_failure"), [first])
        self.assertEqual(next(r for r in self.store.inbox("author-test") if r["receipt_id"] == receipt["receipt_id"]), frozen[0])
        self.assertEqual({t["name"] for t in self.actions("job_details")[0]["payload"]["tests"]}, {"TestAlreadyKnown", "TestAfterRetry"})

    def test_pending_recipient_gets_names_on_first_delivery_without_rewriting_the_action(self):
        self.assign()
        self.world.log = reply({}, 404)
        self.tick(deliver=False)
        original = self.actions("first_failure")[0]
        ids = {r["receipt_id"] for r in self.store.outbox()}
        self.world.log = Reply(200, {}, b'--- FAIL: TestBeforeActualDelivery (0.1s)\n')
        lines = self.api.lines
        def first_delivery(path, consume, *, timeout):
            self.assertEqual(timeout, 5, "a queued but never delivered first receipt still uses the short first window")
            return lines(path, consume, timeout=timeout)
        with patch.object(self.api, "lines", side_effect=first_delivery):
            self.tick()
        self.assertEqual(self.actions("first_failure"), [original])
        self.assertTrue(ids <= {r["receipt_id"] for r in self.store.outbox()})
        for recipient in ("author-test", "coordinator-test"):
            receipt = next(r for r in self.store.inbox(recipient) if r["payload"]["action"] == "first_failure")
            self.assertIn(receipt["receipt_id"], ids)
            self.assertEqual(receipt["payload"]["tests"][0]["name"], "TestBeforeActualDelivery")

    def test_environment_failure_does_not_invent_a_test_from_an_incidental_name(self):
        self.world.log = Reply(200, {}, b'go: dependency discovery failed; TestNeverExecuted was not run\n')
        self.tick()
        first = self.actions("first_failure")[0]["payload"]
        self.assertEqual(first["tests"], [])
        self.assertEqual((first["job_id"], first["job_url"]), (501, job()["html_url"]))
        self.assertFalse(first["logs_pending"])

    def test_already_delivered_ready_native_recovery_and_sqlite_ack_progress_before_log_returns(self):
        import codex_receiver as receiver
        from test_codex_receiver import ROOT as actor, QUEUE
        self.store.close()
        self.path = self.path.with_name("native-ready.sqlite")
        self.store = Store(self.path)
        self.store.configure(REPO, actor)
        self.api = API(self.store, self.world)
        rollout = self.path.parent / ("rollout-first-log-" + actor + ".jsonl")
        rollout.write_text(json.dumps({"type": "session_meta", "payload": {"id": actor, "source": "cli", "thread_source": "user"}}) + "\n")
        rollout.chmod(0o600)
        receiver.register(self.store, actor, str(rollout))
        payload = dict(workflow=RELEASE, workflow_sha=SHA, source_head=SHA, tested_sha=TARGET, target_unknown=False,
                       provenance="release-intent", conclusion="success", job_id=602, job_url=f"https://github.com/{REPO}/actions/runs/92/job/602", pr=None)
        identity = self.store.publish(Key(REPO, TARGET, 92, 1, "602"), "release_binary_ready", payload, [actor])
        self.store.deliver_actions({identity: None}, actor)
        ready = self.store.inbox(actor)[0]
        sent = threading.Event()
        def queue(_recipient, message):
            if ready["receipt_id"] in message:
                sent.set()
            return QUEUE
        def slow(_path, consume, *, timeout):
            self.assertFalse(self.store.db.in_transaction)
            self.assertTrue(sent.wait(2), "already prepared native readiness must not wait behind this log")
            code = "import sys;sys.path.insert(0,sys.argv[1]);from store import Store;s=Store(sys.argv[2],initialize=False);s.db.execute('PRAGMA busy_timeout=0');s.ack(sys.argv[3],sys.argv[4]);s.close()"
            ack = subprocess.run([sys.executable, "-B", "-c", code, str(Path(__file__).parent.resolve()), str(self.path), actor, ready["receipt_id"]], capture_output=True, timeout=3)
            self.assertEqual(ack.returncode, 0, ack.stderr.decode())
            consume('--- FAIL: TestAfterConcurrentAck (0.1s)')
            return True
        with patch.object(receiver, "_queue", side_effect=queue), patch.object(self.api, "lines", side_effect=slow):
            self.tick()
        self.assertIsNotNone(self.store.inbox(actor, receipt_id=ready["receipt_id"])[0]["acked_at"])
        self.assertEqual(len(self.actions("first_failure")), 1)

    def test_unverified_earlier_job_cannot_reserve_the_first_action(self):
        self.world.jobs[1] = [dict(job(501, when="2026-11-26T01:01:30Z"), head_sha=GOOD), job(502)]
        self.tick()
        self.assertEqual([a["job"] for a in self.actions("first_failure")], ["502"])
        self.assertFalse(any("/jobs/501/logs" in path for path, _ in self.world.calls))


class EarlyMergeIdentity(Case):
    def identity_fixture(self, event="pull_request"):
        self.world.prs[12].update(state="open", merged=False)
        self.world.prs[12]["head"].update(ref="feature", repo={"id": 123, "full_name": REPO})
        native = self.world.runs[1]
        native.update(event=event, status="completed", conclusion="failure")
        if event == "workflow_dispatch":
            native.update(head_sha=GOOD, head_branch="master", head_repository={"id": 123, "full_name": REPO}, pull_requests=[])
        self.world.overrides[ROOT + "/commits/master"] = reply({"sha": GOOD})
        data = dict(schema=1, kind="merge-identity", repo=REPO, pr=12, head=SHA, base=GOOD, sha=TARGET, tree=TREE,
                    run_id="91", run_attempt="1", source=dict(repo_id=123, repo=REPO, ref="feature", base_ref="master"),
                    controller_sha=GOOD if event == "workflow_dispatch" else "", policy_digest="d" * 64)
        producer = dict(job(name="Merge plan", when="2026-11-26T01:05:00Z"), head_sha=native["head_sha"], steps=[
            dict(number=3, name="Preserve true merge identity before fallible planning", status="completed", conclusion="success",
                 started_at="2026-11-26T01:01:00Z", completed_at="2026-11-26T01:02:00Z"),
            dict(number=4, name="Run actions/upload-artifact@v7", status="completed", conclusion="success",
                 started_at="2026-11-26T01:02:00Z", completed_at="2026-11-26T01:03:00Z"),
            dict(number=6, name="Verify true merge parents and select complete impacted suites", status="completed", conclusion="failure",
                 started_at="2026-11-26T01:03:00Z", completed_at="2026-11-26T01:04:00Z")])
        self.world.jobs[1] = [producer]
        self.world.add_artifact("merge-identity", data, "Merge plan", member="merge-identity.json")
        self.world.producers.clear()
        self.world.artifacts[0].update(created_at="2026-11-26T01:02:45Z", updated_at="2026-11-26T01:02:46Z",
                                       workflow_run={"id": 91, "head_sha": native["head_sha"]})
        self.world.log = Reply(200, {}, b'a/code.go:3:8: package fixture/missingdep is not in std\n')
        return data, producer

    def test_failed_plan_job_successful_identity_steps_bind_native_pr_tree_without_tests(self):
        self.assign()
        self.identity_fixture()
        self.tick()
        target = self.store.target(REPO, 91, 1)
        self.assertIsNotNone(target)
        self.assertEqual((target["sha"], target["tree"], target["provenance"]), (TARGET, TREE, "merge-identity"))
        author = self.store.inbox("author-test")
        self.assertTrue(any(r["payload"].get("tested_sha") == TARGET and r["payload"].get("tested_tree") == TREE for r in author))
        self.assertTrue(all(not r["payload"].get("tests") for r in author))
        self.assertFalse(any(a["kind"] == "terminal" and a["payload"]["conclusion"] == "success" for a in self.actions()))
        self.assertFalse(self.store.db.execute("SELECT 1 FROM targets WHERE kind='merge-plan'").fetchone())

    def test_late_dispatch_identity_creates_one_auditable_author_receipt_preserving_coordinator(self):
        self.assign()
        self.identity_fixture("workflow_dispatch")
        artifacts = self.world.artifacts
        self.world.artifacts = []
        self.tick()
        coordinator = next(r for r in self.store.inbox("coordinator-test") if r["payload"]["action"] == "first_failure")
        original = self.actions("first_failure")
        self.assertIsNone(coordinator["payload"]["pr"])
        self.assertEqual(self.store.inbox("author-test"), [])
        self.world.artifacts = artifacts
        self.tick()
        author = [r for r in self.store.inbox("author-test") if r["payload"]["action"] == "first_failure"]
        self.assertEqual(len(author), 1)
        payload = author[0]["payload"]
        self.assertEqual((payload["pr"], payload["source_head"], payload["tested_sha"], payload["tested_tree"]), (12, GOOD, TARGET, TREE))
        self.assertEqual(payload["provenance"], "merge-identity")
        self.assertEqual(payload["identity_evidence"]["artifact_id"], 8)
        self.assertEqual(payload["tests"], [])
        self.assertNotEqual(author[0]["receipt_id"], coordinator["receipt_id"])
        self.assertEqual(self.actions("first_failure"), original)
        self.assertEqual(self.store.inbox("coordinator-test", receipt_id=coordinator["receipt_id"]), [coordinator])
        self.store.close()
        self.store = Store(self.path)
        self.api = API(self.store, self.world)
        self.tick()
        self.assertEqual([r for r in self.store.inbox("author-test") if r["payload"]["action"] == "first_failure"], author)
        self.assertEqual(self.store.inbox("coordinator-test", receipt_id=coordinator["receipt_id"]), [coordinator])

    def test_identity_alone_cannot_satisfy_success_terminal_wait(self):
        self.identity_fixture()
        self.world.runs[1]["conclusion"] = "success"
        self.world.jobs[1][0]["conclusion"] = "success"
        self.world.jobs[1][0]["steps"][-1]["conclusion"] = "skipped"
        self.tick()
        self.assertIsNotNone(self.store.target(REPO, 91, 1))
        self.assertEqual(self.actions("terminal"), [])
        self.assertIsNone(self.store.wait("coordinator-test", dict(repo=REPO, sha=TARGET, run_id=91, attempt=1, action="terminal"), timeout=0))

    def test_bad_identity_step_artifact_or_binding_stays_unknown(self):
        modes = ("digest", "identity_step", "upload_step", "window", "step_order", "attempt", "tree", "parents", "head", "source", "controller", "schema", "policy", "extra", "zip", "artifact_run", "artifact_head", "artifact_name", "id_type")
        for mode in modes:
            with self.subTest(mode=mode):
                fixture = EarlyMergeIdentity()
                fixture.setUp()
                try:
                    fixture.assign()
                    data, producer = fixture.identity_fixture("workflow_dispatch")
                    artifact = fixture.world.artifacts[0]
                    if mode == "identity_step":
                        producer["steps"][0]["conclusion"] = "failure"
                    elif mode == "upload_step":
                        producer["steps"][1]["conclusion"] = "skipped"
                        producer["steps"].append(dict(producer["steps"][1], number=8, conclusion="success"))
                    elif mode == "window":
                        artifact["created_at"] = "2026-11-26T01:04:30Z"
                    elif mode == "step_order":
                        producer["steps"][0]["completed_at"] = "2026-11-26T01:02:30Z"
                    elif mode == "attempt":
                        data["run_attempt"] = "2"
                    elif mode == "tree":
                        data["tree"] = GOOD
                    elif mode == "parents":
                        fixture.world.overrides[ROOT + "/commits/" + TARGET] = reply({"sha": TARGET, "commit": {"tree": {"sha": TREE}}, "parents": [{"sha": SHA}, {"sha": GOOD}]})
                    elif mode == "head":
                        data["head"] = COMMIT
                    elif mode == "source":
                        data["source"]["ref"] = "forged"
                    elif mode == "controller":
                        data["controller_sha"] = SHA
                    elif mode == "schema":
                        data["schema"] = True
                    elif mode == "policy":
                        data["policy_digest"] = "not-a-digest"
                    elif mode == "extra":
                        data["verified"] = True
                    elif mode == "artifact_run":
                        artifact["workflow_run"]["id"] = 92
                    elif mode == "artifact_head":
                        artifact["workflow_run"]["head_sha"] = COMMIT
                    elif mode == "artifact_name":
                        artifact["name"] = "merge-identity-91-2"
                    elif mode == "id_type":
                        data["run_id"] = 91
                    fixture.world.replace_blob(8, archive(data, member="merge-identity.json", extra=mode == "zip"))
                    if mode == "digest":
                        artifact["digest"] = "sha256:" + "0" * 64
                    fixture.tick()
                    self.assertIsNone(fixture.store.target(REPO, 91, 1))
                    self.assertEqual(fixture.store.inbox("author-test"), [])
                    self.assertTrue(fixture.store.inbox("coordinator-test"))
                finally:
                    fixture.tearDown()

    def test_identity_must_match_the_native_pr_not_another_pr_on_the_same_head(self):
        self.assign()
        data, _ = self.identity_fixture()
        other = copy.deepcopy(self.world.prs[12])
        other.update(number=23, html_url=f"https://github.com/{REPO}/pull/23")
        self.world.prs[23] = other
        data["pr"] = 23
        self.world.replace_blob(8, archive(data, member="merge-identity.json"))
        self.tick()
        self.assertIsNone(self.store.target(REPO, 91, 1))
        self.assertFalse(any(r["payload"].get("tested_sha") for r in self.store.inbox("author-test")))

    def test_later_successful_plan_upgrades_identity_without_losing_its_audit(self):
        self.assign()
        identity, producer = self.identity_fixture()
        self.world.runs[1].update(status="in_progress", conclusion=None)
        producer.update(status="in_progress", conclusion=None, completed_at=None)
        producer["steps"][-1].update(status="in_progress", conclusion=None, completed_at=None)
        self.tick()
        early = self.store.target(REPO, 91, 1)
        self.assertEqual(early["provenance"], "merge-identity")
        self.assertEqual(self.actions(), [])
        producer.update(status="completed", conclusion="success", completed_at="2026-11-26T01:05:00Z")
        producer["steps"][-1].update(status="completed", conclusion="success", completed_at="2026-11-26T01:04:00Z")
        self.world.runs[1].update(status="completed", conclusion="success")
        plan = {k: v for k, v in identity.items() if k not in {"kind", "schema"}}
        body = archive(plan, member="merge-plan.json")
        self.world.artifacts.append(dict(id=9, name="merge-plan-91-1", expired=False, created_at="2026-11-26T01:04:30Z",
                                         digest="sha256:" + hashlib.sha256(body.body).hexdigest(), workflow_run={"id": 91, "head_sha": SHA}))
        self.world.blobs[ROOT + "/actions/artifacts/9/zip"] = body
        self.tick()
        target = self.store.target(REPO, 91, 1)
        self.assertEqual(target["provenance"], "merge-plan")
        self.assertEqual(target["early_identity"], early["early_identity"])
        self.assertEqual([a["payload"]["conclusion"] for a in self.actions("terminal")], ["success"])
        self.assertIsNotNone(self.store.wait("coordinator-test", dict(repo=REPO, sha=TARGET, run_id=91, attempt=1, action="terminal"), timeout=0))


class FinalReview(Case):
    def test_normal_reconcile_pumps_only_explicitly_registered_codex(self):
        import codex_receiver as receiver
        from test_codex_receiver import ROOT as actor, QUEUE
        with tempfile.TemporaryDirectory() as directory:
            folder = Path(directory)
            store = Store(folder / "db")
            store.configure(REPO, actor)
            rollout = folder / ("rollout-example-" + actor + ".jsonl")
            rollout.write_text(json.dumps({"type": "session_meta", "payload": {
                "id": actor, "source": "cli", "thread_source": "user"}}) + "\n")
            rollout.chmod(0o600)
            receiver.register(store, actor, str(rollout))
            with patch.object(receiver, "_queue", return_value=QUEUE) as queue:
                with Observer(store, API(store, World())) as observer:
                    observer.reconcile()
                self.assertGreater(queue.call_count, 0, "normal reconcile must connect the registered native queue")
                self.assertEqual(queue.call_args.args[0], actor)
            store.close()
        with patch.object(receiver, "_queue", side_effect=AssertionError("unregistered transport")):
            self.tick()

    def test_version_four_orphan_waits_are_recovered_on_normal_reopen(self):
        selected = dict(repo=REPO, sha=SHA, run_id=91, attempt=1, action="terminal")
        self.store.wait("author-test", selected, timeout=0)
        original = tuple(self.store.db.execute("SELECT * FROM waits").fetchone())
        with self.store.db:
            self.store.db.execute("ALTER TABLE waits RENAME TO waits_legacy")
            self.store.db.execute("CREATE TABLE waits(recipient TEXT,selector TEXT NOT NULL,PRIMARY KEY(recipient,selector))")
            self.store.db.execute("PRAGMA user_version=4")
        self.store.close()
        self.store = Store(self.path)
        self.assertEqual([tuple(r) for r in self.store.db.execute("SELECT * FROM waits")], [original])
        self.assertIsNone(self.store.db.execute("SELECT name FROM sqlite_master WHERE name='waits_legacy'").fetchone())
        self.store.close()
        self.store = Store(self.path)
        self.assertEqual([tuple(r) for r in self.store.db.execute("SELECT * FROM waits")], [original])

    def test_bootstrap_schema_is_one_atomic_transaction(self):
        import sqlite3
        statements = []
        connect = sqlite3.connect
        def traced(*args, **kwargs):
            db = connect(*args, **kwargs)
            db.set_trace_callback(statements.append)
            return db
        with tempfile.TemporaryDirectory() as directory, patch("store.sqlite3.connect", side_effect=traced):
            Store(Path(directory) / "db").close()
        sql = [s.strip().upper() for s in statements]
        begin = next(i for i, s in enumerate(sql) if s.startswith("BEGIN"))
        ddl = [i for i, s in enumerate(sql) if s.startswith(("CREATE", "ALTER", "DROP"))]
        self.assertTrue(all(i > begin for i in ddl), "no schema DDL may autocommit before the migration transaction")
        self.assertEqual(sum(s == "COMMIT" for s in sql), 1)

    def test_transient_native_pr_lookup_recovers_author_without_rewriting_coordinator(self):
        self.assign()
        self.world.plan()
        self.world.runs[1].update(status="completed", conclusion="failure")
        self.world.overrides[ROOT + "/pulls/12"] = reply({}, 503)
        self.tick()
        original = self.actions("first_failure")[0]
        coordinator = next(r for r in self.store.inbox("coordinator-test") if r["payload"]["action"] == "first_failure")
        self.assertIsNone(coordinator["payload"]["pr"])
        self.assertEqual(self.store.inbox("author-test"), [])
        del self.world.overrides[ROOT + "/pulls/12"]
        self.tick()
        self.tick()
        author = [r for r in self.store.inbox("author-test") if r["payload"]["action"] == "first_failure"]
        self.assertEqual(len(author), 1)
        self.assertEqual(author[0]["payload"]["pr"], 12)
        self.assertEqual(author[0]["payload"]["source_head"], SHA)
        self.assertEqual(self.actions("first_failure"), [original])
        self.assertEqual([r for r in self.store.inbox("coordinator-test") if r["payload"]["action"] == "first_failure"], [coordinator])

    def test_stacked_pr_author_routes_but_ref_change_and_fork_do_not(self):
        self.assign()
        self.world.prs[12]["base"]["ref"] = "codex/parent-slice"
        self.world.prs[12]["head"].update(ref="feature", repo={"id": 123, "full_name": REPO})
        self.world.plan()
        self.tick()
        self.assertTrue(any(r["payload"]["action"] == "first_failure" for r in self.store.inbox("author-test")))
        baseline = self.store.meta_get(f"author-source:{REPO}:12")
        self.assertEqual(baseline["ref"], "feature")
        self.world.prs[12]["head"]["ref"] = "different"
        with Observer(self.store, self.api) as observer:
            self.assertEqual(observer.merge_recipients(self.world.runs[1]), (["coordinator-test"], None))
        self.world.prs[12]["head"].update(ref="feature", repo={"id": 999, "full_name": "foreign/project"})
        with Observer(self.store, self.api) as observer:
            self.assertEqual(observer.merge_recipients(self.world.runs[1]), (["coordinator-test"], None))
        self.assertEqual(self.store.meta_get(f"author-source:{REPO}:12"), baseline)

    def test_native_go_json_timeout_preserves_running_state_across_output_records(self):
        lines = [json.dumps({"Action": "output", "Package": "agentworkspace/internal/store", "Output": output,
                             **({"Test": "TestWorkInputReturnDecision"} if output.startswith("\t") else {})})
                 for output in ("panic: test timed out after 10m0s\n", "running tests:\n", "\tTestWorkInputReturnDecision (25s)\n")]
        tests = extract_tests("\n".join(lines))
        self.assertEqual(tests, [{"name": "TestWorkInputReturnDecision", "package": "agentworkspace/internal/store", "outcome": "running_at_timeout"}])
        self.assertEqual(extract_tests("\x1b[31m--- FAIL: TestSafe\u202eName (1s)\x1b[0m\n")[0]["name"], "TestSafeName")

    def test_release_accepts_legal_plus_unicode_and_six_annotated_tags(self):
        from urllib.parse import quote
        for branch, depth, raw in (("rc/1.0+build", 0, False), ("rc/候选", 1, False), ("rc/deep", 6, False), ("rc/raw", 6, True)):
            with self.subTest(branch=branch):
                world = World()
                current = run(workflow=RELEASE)
                obj = {"type": "commit", "sha": SHA}
                for index in range(depth):
                    identity = f"{100 + index:040x}"
                    world.overrides[ROOT + "/git/tags/" + identity] = reply({"sha": identity, "object": obj})
                    obj = {"type": "tag", "sha": identity}
                current.update(event="push", head_branch=branch, head_sha=obj["sha"] if raw else SHA,
                               head_commit={"id": SHA, "tree_id": TREE})
                world.overrides[ROOT + "/git/ref/tags/" + quote(branch, safe="/")] = reply({"ref": "refs/tags/" + branch, "object": obj})
                world.overrides[ROOT + "/git/commits/" + SHA] = reply({"sha": SHA, "tree": {"sha": TREE}})
                with Observer(self.store, API(self.store, world)) as observer:
                    observer.trusted_source(current)

    def raw_release(self, artifact_head):
        data = self.world.receipt(RELEASE)
        data["workflow_sha"] = TARGET
        raw = "f" * 40
        self.world.runs[1].update(event="push", head_branch="rc/raw", head_sha=raw,
                                  head_commit={"id": TARGET, "tree_id": TREE})
        self.world.jobs[1].append(job(602, conclusion="success", name="Release candidate binary"))
        for item in self.world.jobs[1] + self.world.producers[1]:
            item["head_sha"] = TARGET
        self.world.artifacts[0]["workflow_run"]["head_sha"] = artifact_head
        self.world.replace_blob(8, archive(data))
        self.world.overrides[ROOT + "/git/ref/tags/rc/raw"] = reply({"ref": "refs/tags/rc/raw", "object": {"type": "tag", "sha": raw}})
        self.world.overrides[ROOT + "/git/tags/" + raw] = reply({"sha": raw, "object": {"type": "commit", "sha": TARGET}})
        self.world.overrides[ROOT + "/git/commits/" + TARGET] = reply({"sha": TARGET, "tree": {"sha": TREE}})

    def test_release_intent_and_jobs_bind_peeled_commit_not_raw_tag_object(self):
        self.raw_release("f" * 40)
        self.tick()
        ready = self.actions("release_binary_ready")
        self.assertEqual(len(ready), 1)
        self.assertEqual((ready[0]["payload"]["source_head"], ready[0]["payload"]["tested_sha"]), ("f" * 40, TARGET))

    def test_release_artifact_accepts_verified_peeled_head_and_remains_idempotent(self):
        self.raw_release(TARGET)
        self.world.jobs[1].append(dict(job(603, conclusion="success", name="Release Full evidence"), head_sha=TARGET))
        self.tick()
        ready = [a for a in self.actions() if a["kind"] in {"release_binary_ready", "release_full_ready"}]
        self.assertEqual({a["kind"] for a in ready}, {"release_binary_ready", "release_full_ready"})
        self.assertTrue(all(a["payload"]["tested_sha"] == TARGET for a in ready))
        self.tick()
        self.assertEqual([a for a in self.actions() if a["kind"] in {"release_binary_ready", "release_full_ready"}], ready)

    def test_release_artifact_rejects_unrelated_head_or_run(self):
        self.raw_release(GOOD)
        self.tick()
        self.assertEqual(self.actions("release_binary_ready"), [])
        self.assertIsNone(self.store.target(REPO, 91, 1))
        self.world.artifacts[0]["workflow_run"].update(head_sha=TARGET, id=92)
        self.tick()
        self.assertEqual(self.actions("release_binary_ready"), [])
        self.assertIsNone(self.store.target(REPO, 91, 1))

    def test_release_rejects_invalid_tag_paths_cycles_objects_and_non_master_commit(self):
        from urllib.parse import quote
        for mode in ("invalid", "nested", "cycle", "blob", "outside"):
            world = World()
            branch = "rc/../bad" if mode == "invalid" else "rc/a/b" if mode == "nested" else "rc/valid"
            current = dict(run(workflow=RELEASE), event="push", head_branch=branch)
            obj = {"type": "blob" if mode == "blob" else "tag" if mode == "cycle" else "commit", "sha": SHA}
            world.overrides[ROOT + "/git/ref/tags/" + quote(branch, safe="/")] = reply({"ref": "refs/tags/" + branch, "object": obj})
            world.overrides[ROOT + "/git/tags/" + SHA] = reply({"sha": SHA, "object": obj})
            if mode == "outside":
                world.overrides[ROOT + f"/compare/{SHA}...{TARGET}"] = reply({"status": "diverged", "merge_base_commit": {"sha": GOOD}})
            with self.subTest(mode=mode), Observer(self.store, API(self.store, world)) as observer, self.assertRaises(Unknown):
                observer.trusted_source(current)

    def test_fast_first_notice_then_larger_deferred_log_budget_succeeds_once(self):
        self.assign()
        budgets = []
        def delayed(_path, consume, *, timeout, **_kwargs):
            self.assertFalse(self.store.db.in_transaction)
            budgets.append(timeout)
            if timeout <= 5:
                return False
            consume('--- FAIL: TestDeferredLog (0.1s)')
            return True
        with patch.object(self.api, "lines", side_effect=delayed):
            self.tick()
            first = self.actions("first_failure")[0]
            self.assertTrue(first["payload"]["logs_pending"])
            self.assertTrue(self.store.inbox("author-test"))
            self.tick()
            self.tick()
        details = [a for a in self.actions("job_details") if a["job"] == "501"]
        self.assertEqual(len(details), 1)
        self.assertEqual(details[0]["payload"]["tests"][0]["name"], "TestDeferredLog")
        self.assertEqual(budgets[0], 5)
        self.assertEqual(budgets[1], 15)
        self.assertEqual(self.actions("first_failure"), [first])

    def test_12_failed_jobs_have_no_unsolicited_sibling_detail_fanout(self):
        self.assign()
        self.world.runs[1].update(status="completed", conclusion="failure")
        self.world.jobs[1] = [job(i) for i in range(501, 513)]
        for _ in range(13):
            self.tick()
        self.assertEqual(len(self.actions("first_failure")), 1)
        self.assertEqual(len(self.actions("terminal")), 1)
        self.assertFalse(any(a["job"] != "501" for a in self.actions("job_details")))
        evidence = {e["job"]: e["payload"] for e in self.store.events()}
        self.assertTrue(evidence["512"]["tests"])

    def test_unchanged_cache_observation_summary_and_metadata_do_not_write(self):
        snapshot = dict(run=run(), jobs=[job()], jobs_fresh=True)
        def record():
            self.store.cache_put(ROOT, '"same"', {"id": 123}, None)
            self.store.observe(REPO, 91, 1, snapshot)
            self.store.track(REPO, run())
            self.store.meta_put("stable", {"value": 1})
            self.store.problem("stable", "仍待证据")
            self.store.bind_target(REPO, 91, 1, "merge-plan", TARGET, TREE, {"source_head": SHA})
        record()
        before = self.store.db.total_changes
        record()
        self.assertEqual(self.store.db.total_changes - before, 0)

    def test_first_of_59_jobs_commits_before_bulk_facts_and_no_network_holds_writer(self):
        self.world.jobs[1] = [job(i, conclusion="success") for i in range(501, 559)] + [job(599)]
        statements, first_writes = [], []
        self.store.db.set_trace_callback(statements.append)
        deliver = self.store._deliver_rows
        def observed(rows, *args):
            if rows and json.loads(rows[0]["payload"])["action"] == "first_failure" and not first_writes:
                first_writes.append(sum("INTO EVENTS" in s.upper() for s in statements))
            return deliver(rows, *args)
        def transport(path, etag=None):
            self.assertFalse(self.store.db.in_transaction, "network must not run under a SQLite write transaction")
            return self.world(path, etag)
        self.api = API(self.store, transport)
        with patch.object(self.store, "_deliver_rows", side_effect=observed):
            self.tick()
        self.assertLessEqual(first_writes[0], 2)
        batches, count = [], 0
        for sql in statements:
            sql = sql.strip().upper()
            if sql.startswith("BEGIN"):
                count = 0
            elif "INTO EVENTS" in sql:
                count += 1
            elif sql == "COMMIT":
                batches.append(count)
        self.assertGreaterEqual(max(batches), 57, "same-page changed facts need one short local transaction")

    def test_core_and_tag_worker_share_only_the_current_authentication(self):
        self.tick()
        self.assertEqual(sum(p == "/user" for p, _ in self.world.calls), 1)
        self.world.overrides["/user"] = reply({}, 401)
        before = len(self.store.inbox("coordinator-test"))
        self.assertFalse(self.tick())
        self.assertEqual(len(self.store.inbox("coordinator-test")), before)

    def test_31_unchanged_events_have_zero_sqlite_changes_and_one_change_is_one(self):
        rows = [(Key(REPO, SHA, 91, 1, str(i)), dict(status="completed", conclusion="success", job_name="unit")) for i in range(501, 532)]
        for key, payload in rows:
            self.store.event(key, payload)
        before = self.store.db.total_changes
        statements = []
        self.store.db.set_trace_callback(statements.append)
        for key, payload in rows:
            self.store.event(key, payload)
        self.assertEqual(self.store.db.total_changes - before, 0)
        self.assertFalse(any(s.startswith(("INSERT", "BEGIN", "COMMIT")) for s in statements))
        self.store.event(rows[0][0], dict(rows[0][1], job_name="renamed"))
        self.assertEqual(self.store.db.total_changes - before, 1)


class ReviewState(Case):
    def test_authenticated_numeric_repo_link_is_reconstructed_not_followed(self):
        first = ROOT + "/actions/runs?per_page=100&page=1"
        second = ROOT + "/actions/runs?per_page=100&page=2"
        self.world.overrides[ROOT] = reply({"id": 123, "full_name": REPO})
        self.world.overrides[first] = reply({"total_count": 2, "workflow_runs": [run()]}, headers={
            "link": '<https://api.github.com/repositories/123/actions/runs?per_page=100&page=2>; rel="next"'})
        self.world.overrides[second] = reply({"total_count": 2, "workflow_runs": [dict(run(), id=92)]})
        self.assertEqual(len(self.api.pages(ROOT + "/actions/runs", "workflow_runs")), 2)
        self.assertFalse(any(p.startswith("/repositories/") for p, _ in self.world.calls))
        self.world.overrides[first].headers["link"] = '<https://api.github.com/repositories/124/actions/runs?per_page=100&page=2>; rel="next"'
        with self.assertRaises(Unknown):
            self.api.pages(ROOT + "/actions/runs", "workflow_runs")

    def test_legacy_missing_control_field_recovers_without_rewriting_pending(self):
        self.assign()
        key = Key(REPO, SHA, 91, 1, "501")
        old = dict(workflow=MERGE, source_head=SHA, tested_sha=None, pr=12, logs_pending=True, tests=[])
        self.store.publish(key, "first_failure", old, ["coordinator-test", "author-test"])
        before = self.store.actions()[0]
        current = dict(old, event="pull_request", workflow_sha=SHA, logs_pending=False,
                       tests=[{"name": "TestKnown", "package": None}])
        self.store.publish(key, "first_failure", current, ["coordinator-test", "author-test"])
        self.store.deliver((REPO, SHA, 91, 1), "coordinator-test", 12)
        self.assertEqual(self.store.actions()[0], before)
        self.assertEqual(len(self.store.inbox("author-test")), 1)
        with self.assertRaises(ValueError):
            self.store.publish(key, "first_failure", dict(current, workflow_sha=GOOD), ["coordinator-test"])
        rc = Key(REPO, SHA, 92, 1, "502")
        self.store.publish(rc, "first_failure", dict(old, workflow=RELEASE), ["coordinator-test"])
        with self.assertRaises(ValueError):
            self.store.publish(rc, "first_failure", dict(current, workflow=RELEASE), ["coordinator-test"])

    def test_multiple_precise_waits_and_cancel_do_not_block_unrelated_work(self):
        a = dict(repo=REPO, sha=SHA, run_id=91, attempt=1, action="first_failure", job="501")
        b = dict(a, action="terminal", job="@terminal")
        self.assertIsNone(self.store.wait("author-test", a, timeout=0))
        self.assertIsNone(self.store.wait("author-test", b, timeout=0))
        self.store.cancel_wait("author-test", a)
        self.assertEqual(self.store.db.execute("SELECT count(*) FROM waits").fetchone()[0], 1)
        self.store.publish(Key(REPO, SHA, 91, 1, "@terminal"), "terminal", {"pr": 12}, ["author-test"])
        self.assign()
        self.store.deliver((REPO, SHA, 91, 1), "coordinator-test", 12)
        receipt = self.store.wait("author-test", b, timeout=0)
        self.assertIsNotNone(receipt)
        self.store.ack("author-test", receipt["receipt_id"])
        self.assertEqual(self.store.db.execute("SELECT count(*) FROM waits").fetchone()[0], 0)

    def test_inbox_cli_is_readonly_even_while_writer_holds_reserved_lock(self):
        self.store.publish(Key(REPO, SHA, 91, 1, "501"), "first_failure", {}, ["coordinator-test"])
        self.store.deliver((REPO, SHA, 91, 1), "coordinator-test")
        before = self.path.stat().st_mtime_ns
        statements = []
        import sqlite3
        connect = sqlite3.connect
        def traced(*args, **kwargs):
            self.assertIn("mode=ro", args[0])
            connection = connect(*args, **kwargs)
            connection.set_trace_callback(statements.append)
            return connection
        self.store.db.execute("BEGIN IMMEDIATE")
        try:
            with patch("store.sqlite3.connect", side_effect=traced), patch("sys.stdout", new_callable=io.StringIO) as output, patch("sys.stderr", new_callable=io.StringIO):
                self.assertEqual(main(["--db", str(self.path), "inbox", "--recipient", "coordinator-test"]), 0)
            self.assertEqual(len(json.loads(output.getvalue())), 1)
            self.assertFalse(any("journal_mode" in sql or "CREATE " in sql for sql in statements), statements)
        finally:
            self.store.db.rollback()
        self.assertEqual(self.path.stat().st_mtime_ns, before)

    def test_native_webhook_payload_is_only_a_bounded_wakeup(self):
        native = {"action": "completed", "repository": {"full_name": "evil/repo"},
                  "workflow_job": {"id": 999, "command": "never execute", "steps": ["x" * 20000]}}
        self.assertTrue(edge_hint(json.dumps(native)))
        self.assertTrue(edge_hint(json.dumps({"action": "completed", "workflow_run": {"id": 999}})))
        self.assertFalse(edge_hint(json.dumps(dict(native, action="queued"))))
        self.assertFalse(edge_hint(json.dumps({"action": "completed", "issue": {}})))

    def test_timeout_names_are_not_assertion_failures(self):
        text = ('panic: test timed out after 14m0s\nrunning tests:\n'
                '\tTestPiFreshInvestigationWorkComposition (17s)\n'
                '{"Action":"fail","Package":"example/store","Test":"TestTaskWorkInputEffectKeys"}\n')
        rows = {t["name"]: t for t in extract_tests(text)}
        self.assertEqual(rows["TestPiFreshInvestigationWorkComposition"]["outcome"], "running_at_timeout")
        self.assertEqual(rows["TestTaskWorkInputEffectKeys"]["outcome"], "failed")


if __name__ == "__main__":
    unittest.main(verbosity=2)
