"""Offline contracts for trusted publisher Checks, not native job fanout.

The native Check/log fixture records its producer commit and SHA-256 digests.
For cross-source QA, explicitly select a checkout or scoped archive of a pinned
producer with OBSERVER_MERGE_PUBLISHER_ROOT=/path/to/producer. This runs the same
suite against that source, checks its output against the fixture, fails if it is
missing, and never discovers siblings.
"""
import ast
import copy
from contextlib import contextmanager, redirect_stdout
import hashlib
import io
import json
import os
from pathlib import Path
import sys
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch
from urllib.parse import parse_qs, urlsplit

sys.dont_write_bytecode = True
sys.path.insert(0, str(Path(__file__).resolve().parent))
from github import API, Reply
from observer import Observer
from store import Key, Store
from merge_reports import FAILURE, RESULT, reconcile_publisher

REPO = "example/project"
ROOT = "/repos/" + REPO
PUBLISHER = ".github/workflows/merge-conflict.yml"
NATIVE = ".github/workflows/merge-gate.yml"
HEAD, BASE, MERGE, TREE, POLICY, OTHER = (c * 40 for c in "abcdef")
COORDINATOR, AUTHOR = "coordinator-test", "author-test"


def reply(data, status=200, headers=None):
    return Reply(status, headers or {}, json.dumps(data).encode())


def source(pr):
    return dict(repo_id=pr["head"]["repo"]["id"], repo=pr["head"]["repo"]["full_name"],
                ref=pr["head"]["ref"], base_ref=pr["base"]["ref"])


class World:
    def __init__(self):
        self.repo = {"id": 321, "full_name": REPO, "default_branch": "master"}
        self.pr = {"number": 7, "state": "open", "merged": False, "merged_at": None,
                   "html_url": f"https://github.com/{REPO}/pull/7",
                   "head": {"sha": HEAD, "ref": "feature/one", "repo": {"id": 456, "full_name": "writer/project"}},
                   "base": {"sha": BASE, "ref": "master", "repo": self.repo},
                   "mergeable": False, "mergeable_state": "dirty", "merge_commit_sha": None}
        self.run = {"id": 201, "run_attempt": 2, "path": PUBLISHER, "workflow_id": 17,
                    "repository": self.repo, "head_repository": self.repo, "head_sha": POLICY,
                    "head_branch": "master", "event": "pull_request_target", "pull_requests": [],
                    "html_url": f"https://github.com/{REPO}/actions/runs/201",
                    "status": "completed", "conclusion": "success"}
        self.jobs = [{"id": 701, "name": "Trusted Merge report", "run_id": 201, "run_attempt": 2,
                      "head_sha": POLICY, "status": "completed", "conclusion": "success",
                      "html_url": f"https://github.com/{REPO}/actions/runs/201/job/701"}]
        self.check = {"id": 801, "name": "Merge gate", "head_sha": HEAD,
                      "app": {"id": 15368, "slug": "github-actions"},
                      "external_id": f"merge-gate:v1:{REPO}:pr:7:head:{HEAD}",
                      "html_url": f"https://github.com/{REPO}/runs/801?check_suite_focus=true",
                      "status": "completed", "conclusion": "failure"}
        self.metadata = {"schema": 1, "repo": REPO, "pr": 7, "head": HEAD, "result": "failure",
                         "publisher": {"workflow": PUBLISHER, "run_id": "201", "run_attempt": "2"},
                         "reason": "确认存在合并冲突；原生 pull_request 不会创建测试合并运行"}
        self.open_prs = [self.pr]
        self.checks = [self.check]
        self.validation = None
        self.calls, self.overrides = [], {}

    def native(self, event="pull_request"):
        self.validation = {"id": 101, "run_attempt": 3, "path": NATIVE, "workflow_id": 18,
                           "repository": self.repo, "head_repository": self.pr["head"]["repo"],
                           "head_sha": HEAD, "head_branch": self.pr["head"]["ref"], "event": event,
                           "pull_requests": [], "status": "completed", "conclusion": "success",
                           "html_url": f"https://github.com/{REPO}/actions/runs/101"}
        if event == "workflow_dispatch":
            self.validation.update(head_repository=self.repo, head_sha=POLICY, head_branch="master",
                                   display_title=f"Merge validation PR 999 head {OTHER} base {OTHER}")
        return self.validation

    def success(self, event="pull_request"):
        self.native(event)
        self.pr.update(mergeable=True, mergeable_state="clean", merge_commit_sha=MERGE)
        self.metadata.update(result="success", validation={
            "identity": {"repo": REPO, "pr": 7, "head": HEAD, "base": BASE,
                         "sha": MERGE, "tree": TREE, "run_id": "101", "run_attempt": "3"},
            "source": source(self.pr), "controller_sha": POLICY if event == "workflow_dispatch" else "",
            "artifact_id": 901, "artifact_digest": "sha256:" + "1" * 64, "policy_digest": "2" * 64})
        self.metadata.pop("reason", None)
        self.check.update(conclusion="success")

    def check_row(self, row=None):
        row = copy.deepcopy(row or self.check)
        if row["id"] == 801:
            row["output"] = {"title": "合并已阻塞", "summary": json.dumps(self.metadata)}
        return row

    def __call__(self, path, etag=None):
        self.calls.append((path, etag))
        if path in self.overrides:
            value = self.overrides[path]
            return value(path, etag) if callable(value) else value
        parsed = urlsplit(path)
        query = parse_qs(parsed.query, keep_blank_values=True)
        if parsed.path == ROOT:
            data = self.repo
        elif parsed.path == ROOT + "/pulls":
            assert query == {"state": ["open"], "per_page": ["100"], "page": ["1"]}, path
            data = self.open_prs
        elif parsed.path == ROOT + "/pulls/7":
            data = self.pr
        elif parsed.path == ROOT + "/actions/workflows/merge-conflict.yml":
            data = {"id": 17, "path": PUBLISHER, "state": "active"}
        elif parsed.path == ROOT + "/actions/workflows/merge-gate.yml":
            data = {"id": 18, "path": NATIVE, "state": "active"}
        elif parsed.path in {ROOT + "/actions/runs/201", ROOT + f"/actions/runs/201/attempts/{self.run['run_attempt']}"}:
            data = self.run
        elif parsed.path in {ROOT + "/actions/runs/101", ROOT + "/actions/runs/101/attempts/3"} and self.validation:
            data = self.validation
        elif parsed.path == ROOT + f"/commits/{self.pr['head']['sha']}/check-runs":
            assert query == {"filter": ["all"], "per_page": ["100"], "page": ["1"]}, path
            data = {"total_count": len(self.checks), "check_runs": [self.check_row(row) for row in self.checks]}
        elif parsed.path == ROOT + "/check-runs/801":
            data = self.check_row()
        elif parsed.path == ROOT + "/git/commits/" + MERGE:
            data = {"sha": MERGE, "tree": {"sha": TREE}, "parents": [{"sha": BASE}, {"sha": HEAD}]}
        elif parsed.path == ROOT + "/git/ref/heads/" + self.pr["base"]["ref"]:
            data = {"ref": "refs/heads/" + self.pr["base"]["ref"], "object": {"type": "commit", "sha": BASE}}
        elif parsed.path == ROOT + "/git/ref/heads/master":
            data = {"ref": "refs/heads/master", "object": {"type": "commit", "sha": POLICY}}
        elif parsed.path.startswith(ROOT + "/compare/"):
            old, new = parsed.path.split("/compare/")[1].split("...")
            data = {"status": "identical" if old == new else "ahead", "merge_base_commit": {"sha": old}}
        else:
            return reply({}, 404)
        body = json.dumps(data, sort_keys=True).encode()
        current = '"' + hashlib.sha256(body).hexdigest() + '"'
        return Reply(304, {}, b"") if etag == current else Reply(200, {"etag": current}, body)


class Case(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.path = Path(self.tmp.name) / "observer.db"
        self.store = Store(self.path)
        self.store.configure(REPO, COORDINATOR)
        self.store.assign(REPO, 7, AUTHOR, "local-assignment:7")
        self.world = World()

    def tearDown(self):
        self.store.close()
        self.tmp.cleanup()

    def tick(self, deliver=True):
        with Observer(self.store, API(self.store, self.world)) as observer:
            return reconcile_publisher(observer, copy.deepcopy(self.world.run), copy.deepcopy(self.world.jobs), deliver)

    def payloads(self):
        return [row["payload"] for row in self.store.actions() if row["kind"].startswith("merge_gate_")]

    def reopen(self):
        self.store.close()
        self.store = Store(self.path)


class PublisherReports(Case):
    def test_real_conflict_without_native_run_goes_to_author_and_coordinator(self):
        self.assertTrue(self.tick())
        payload, = self.payloads()
        self.assertEqual(payload["action"], "merge_gate_failure")
        self.assertEqual(payload["failure_kind"], "merge_conflict")
        self.assertEqual((payload["pr"], payload["sha"], payload["source_head"]), (7, HEAD, HEAD))
        self.assertIsNone(payload["tested_sha"])
        self.assertEqual((payload["run_id"], payload["attempt"]), (201, 2))
        self.assertEqual(payload["reporter_job_id"], 701)
        self.assertEqual(payload["reporter_job_conclusion"], "success")
        self.assertEqual(payload["reporter_job_url"], self.world.jobs[0]["html_url"])
        self.assertEqual((payload["check_id"], payload["check_conclusion"]), (801, "failure"))
        self.assertEqual(payload["check_url"], self.world.check["html_url"])
        self.assertTrue(payload["investigation"])
        self.assertEqual(payload["tests"], [])
        self.assertEqual({row["recipient"] for row in self.store.outbox()}, {COORDINATOR, AUTHOR})
        self.assertEqual(len(self.store.inbox(AUTHOR)), 1)
        self.assertTrue(self.store.events())
        self.reopen()
        self.assertTrue(self.tick())
        self.assertEqual(len(self.payloads()), 1)
        self.assertEqual(len(self.store.inbox(AUTHOR)), 1)

    def test_unconfirmed_conflict_is_evidence_failure_not_fabricated_test_failure(self):
        self.world.pr.update(mergeable=None, mergeable_state="unknown")
        self.assertTrue(self.tick())
        payload, = self.payloads()
        self.assertEqual(payload["failure_kind"], "validation_evidence")
        self.assertIn("校验证据失败", payload["message"])
        self.assertNotIn("存在合并冲突", payload["message"])
        self.assertEqual(payload["tests"], [])
        self.assertIsNone(payload["tested_sha"])

    def test_legacy_synthetic_sha_check_is_not_the_required_source_head_gate(self):
        self.world.success()
        self.world.check["head_sha"] = MERGE
        self.world.checks = []
        self.world.overrides[ROOT + f"/commits/{MERGE}/check-runs?filter=all&per_page=100&page=1"] = reply(
            {"total_count": 1, "check_runs": [self.world.check_row()]})
        self.assertFalse(self.tick())
        self.assertEqual(self.payloads(), [])
        self.assertEqual(self.store.inbox(AUTHOR), [])
        self.assertEqual(self.store.events(), [])
        self.assertTrue(self.store.db.execute("SELECT 1 FROM problems WHERE scope='merge-report:201:2:legacy'").fetchone())
        self.assertFalse(any(f"/commits/{MERGE}/check-runs" in path for path, _ in self.world.calls))

    def test_stacked_base_is_a_valid_pr_target(self):
        self.world.pr["base"]["ref"] = "feature/stacked"
        self.assertTrue(self.tick())
        self.assertEqual(len(self.store.inbox(AUTHOR)), 1)

    def test_pending_check_or_reporter_is_never_terminal(self):
        self.world.metadata["result"] = "pending"
        self.world.check.update(status="in_progress", conclusion=None)
        self.assertFalse(self.tick())
        self.assertEqual(self.payloads(), [])
        self.assertEqual(self.store.events(), [])
        self.world.run.update(status="in_progress", conclusion=None)
        self.assertFalse(self.tick())
        self.assertEqual(self.payloads(), [])

    def test_native_first_failure_is_not_dispatched_again_as_a_new_investigation(self):
        native = self.world.native()
        native["conclusion"] = "failure"
        self.store.observe(REPO, 101, 3, {"run": native, "jobs": []})
        key = Key(REPO, HEAD, 101, 3, "501")
        payload = dict(workflow=NATIVE, event="pull_request", workflow_sha=HEAD, source_head=HEAD,
                       tested_sha=None, pr=7, job_id=501, conclusion="failure")
        self.store.publish(key, "first_failure", payload, [COORDINATOR, AUTHOR])
        self.assertTrue(self.tick())
        result, = self.payloads()
        self.assertEqual(result["action"], "merge_gate_result")
        self.assertFalse(result["investigation"])
        self.assertEqual(result["native_failure_run_id"], 101)
        self.assertEqual(len(self.store.inbox(AUTHOR)), 1)
        self.assertEqual(self.store.inbox(AUTHOR)[0]["payload"]["action"], "merge_gate_result")
        self.assertTrue(all(row["state"] == "pending" for row in self.store.outbox() if row["payload"]["action"] == "first_failure"))

    def test_publisher_success_with_verified_native_target_is_only_a_check_update(self):
        self.world.success()
        self.assertTrue(self.tick())
        payload, = self.payloads()
        self.assertEqual(payload["action"], "merge_gate_result")
        self.assertFalse(payload["investigation"])
        self.assertEqual((payload["tested_sha"], payload["tested_tree"]), (MERGE, TREE))
        self.assertEqual((payload["validation_run_id"], payload["validation_attempt"]), (101, 3))
        self.assertNotIn("release_binary_ready", {r["kind"] for r in self.store.actions()})

    def test_default_branch_dispatch_uses_publisher_proof_not_the_run_title(self):
        self.world.success("workflow_dispatch")
        self.world.run["event"] = "workflow_run"
        self.assertTrue(self.tick())
        payload, = self.payloads()
        self.assertEqual(payload["pr"], 7)
        self.assertEqual(payload["source_head"], HEAD)
        self.assertEqual(payload["workflow_sha"], POLICY)
        self.assertEqual(payload["tested_sha"], MERGE)
        self.assertEqual(payload["validation_event"], "workflow_dispatch")
        self.assertEqual(len(self.store.inbox(AUTHOR)), 1)


class Provenance(Case):
    def test_wrong_reporter_workflow_event_repo_attempt_or_job_is_rejected(self):
        original_run, original_jobs = copy.deepcopy(self.world.run), copy.deepcopy(self.world.jobs)
        changes = [("path", NATIVE), ("workflow_id", 999), ("event", "workflow_dispatch"),
                   ("repository", {"id": 123, "full_name": "other/project"}),
                   ("head_repository", {"id": 456, "full_name": "writer/project"})]
        for field, value in changes:
            with self.subTest(field=field):
                self.world.run = dict(original_run, **{field: value})
                self.assertFalse(self.tick())
                self.assertEqual(self.payloads(), [])
        self.world.run = original_run
        for field, value in (("name", "Other report"), ("run_attempt", 3), ("run_id", 1),
                             ("head_sha", OTHER), ("conclusion", "failure")):
            with self.subTest(job=field):
                self.world.jobs = [dict(original_jobs[0], **{field: value})]
                self.assertFalse(self.tick())
                self.assertEqual(self.payloads(), [])
        self.world.jobs = [original_jobs[0], dict(original_jobs[0], id=702,
                           html_url=f"https://github.com/{REPO}/actions/runs/201/job/702")]
        self.assertFalse(self.tick())

    def test_duplicate_or_foreign_same_named_checks_cannot_authorize(self):
        original = copy.deepcopy(self.world.check)
        for field, value in (("app", {"id": 9, "slug": "github-actions"}),
                             ("app", {"id": 15368, "slug": "foreign"}),
                             ("external_id", "untrusted"), ("head_sha", OTHER)):
            with self.subTest(field=field):
                self.world.check = dict(original, **{field: value})
                self.world.checks = [self.world.check]
                self.assertFalse(self.tick())
                self.assertEqual(self.payloads(), [])
        self.world.check = original
        self.world.checks = [original, dict(original, id=802)]
        self.assertFalse(self.tick())
        self.assertEqual(self.payloads(), [])

    def test_summary_is_bound_to_exact_schema_repo_pr_head_run_and_attempt(self):
        original = copy.deepcopy(self.world.metadata)
        changes = [("schema", True), ("repo", "other/project"), ("pr", 8), ("head", OTHER),
                   ("result", "success"), ("publisher", {"workflow": NATIVE, "run_id": "201", "run_attempt": "2"}),
                   ("publisher", {"workflow": PUBLISHER, "run_id": "202", "run_attempt": "2"}),
                   ("publisher", {"workflow": PUBLISHER, "run_id": 201, "run_attempt": "2"}),
                   ("publisher", {"workflow": PUBLISHER, "run_id": "201", "run_attempt": "02"})]
        for field, value in changes:
            with self.subTest(field=field, value=value):
                self.world.metadata = dict(original, **{field: value})
                self.assertFalse(self.tick())
                self.assertEqual(self.payloads(), [])

    def test_final_check_identity_and_integral_workflow_id_are_required(self):
        changed = self.world.check_row()
        changed.update(id=802, html_url=f"https://github.com/{REPO}/runs/802")
        self.world.overrides[ROOT + "/check-runs/801"] = reply(changed)
        self.assertFalse(self.tick())
        self.assertEqual(self.payloads(), [])
        self.world.overrides.clear()
        self.world.run["workflow_id"] = 17.0
        self.assertFalse(self.tick())
        self.assertEqual(self.payloads(), [])

    def test_source_tree_and_real_validation_attempt_are_required_for_fallback(self):
        self.world.success("workflow_dispatch")
        original = copy.deepcopy(self.world.metadata["validation"])
        invalid = [dict(original, source=dict(original["source"], ref="forged")),
                   dict(original, source=dict(original["source"], base_ref="forged")),
                   dict(original, identity=dict(original["identity"], tree=OTHER)),
                   dict(original, identity=dict(original["identity"], sha=OTHER)),
                   dict(original, identity=dict(original["identity"], base=OTHER)),
                   dict(original, identity=dict(original["identity"], run_attempt="4")),
                   dict(original, identity=dict(original["identity"], head=OTHER)),
                   dict(original, controller_sha=OTHER)]
        for validation in invalid:
            with self.subTest(validation=validation):
                self.world.metadata["validation"] = validation
                self.assertFalse(self.tick())
                self.assertEqual(self.payloads(), [])
        self.world.metadata["validation"] = original
        self.assertTrue(self.tick())

    def test_moved_head_and_closed_or_foreign_pr_never_route_to_an_author(self):
        original = copy.deepcopy(self.world.pr)
        for changes in ({"state": "closed"}, {"head": dict(original["head"], sha=OTHER)},
                        {"base": dict(original["base"], repo={"id": 321, "full_name": "other/project"})}):
            with self.subTest(changes=changes):
                self.world.pr = dict(original, **changes)
                self.world.open_prs = [self.world.pr]
                self.assertFalse(self.tick())
                self.assertEqual(self.store.inbox(AUTHOR), [])

    def test_pr_and_check_are_rechecked_before_delivery(self):
        self.world.run["pull_requests"] = [{"number": 7}]
        reads = 0

        def moving(path, etag):
            nonlocal reads
            reads += 1
            pr = copy.deepcopy(self.world.pr)
            if reads > 1:
                pr["head"]["sha"] = OTHER
            return reply(pr)

        self.world.overrides[ROOT + "/pulls/7"] = moving
        self.assertFalse(self.tick())
        self.assertEqual(self.payloads(), [])
        self.world.overrides = {ROOT + "/check-runs/801": reply(self.world.check_row() | {"conclusion": "success"})}
        self.assertFalse(self.tick())
        self.assertEqual(self.payloads(), [])


class DiscoveryAndRecovery(Case):
    def test_empty_pull_requests_discovery_is_once_per_attempt_and_survives_restart(self):
        self.assertTrue(self.tick())
        self.reopen()
        self.assertTrue(self.tick())
        discoveries = [p for p, _ in self.world.calls if urlsplit(p).path == ROOT + "/pulls"]
        self.assertEqual(len(discoveries), 1)
        self.assertFalse(any("/actions/runs?" in p for p, _ in self.world.calls))
        self.world.run["run_attempt"] = 3
        self.world.jobs[0]["run_attempt"] = 3
        self.world.metadata["publisher"]["run_attempt"] = "3"
        self.assertTrue(self.tick())
        discoveries = [p for p, _ in self.world.calls if urlsplit(p).path == ROOT + "/pulls"]
        self.assertEqual(len(discoveries), 2)
        self.assertEqual(sum(p["action"] == "merge_gate_failure" for p in self.payloads()), 1)

    def test_unrelated_cached_pr_closing_does_not_poison_discovery(self):
        other = copy.deepcopy(self.world.pr)
        other.update(number=8, html_url=f"https://github.com/{REPO}/pull/8")
        other["head"]["sha"] = OTHER
        self.world.open_prs = [other, self.world.pr]
        self.world.overrides[ROOT + "/pulls/8"] = reply(dict(other, state="closed"))
        self.assertTrue(self.tick())
        self.assertEqual(len(self.store.inbox(AUTHOR)), 1)

    def test_two_valid_claims_for_one_publisher_attempt_are_ambiguous(self):
        other = copy.deepcopy(self.world.pr)
        other.update(number=8, html_url=f"https://github.com/{REPO}/pull/8")
        other["head"]["sha"] = OTHER
        self.world.open_prs = [self.world.pr, other]
        self.world.overrides[ROOT + "/pulls/8"] = reply(other)
        check = self.world.check_row()
        metadata = dict(self.world.metadata, pr=8, head=OTHER)
        check.update(id=802, head_sha=OTHER, html_url=f"https://github.com/{REPO}/runs/802",
                     external_id=f"merge-gate:v1:{REPO}:pr:8:head:{OTHER}",
                     output={"summary": json.dumps(metadata)})
        self.world.overrides[ROOT + f"/commits/{OTHER}/check-runs?filter=all&per_page=100&page=1"] = reply({
            "total_count": 1, "check_runs": [check]})
        self.assertFalse(self.tick())
        self.assertEqual(self.payloads(), [])

    def test_native_pr_link_avoids_open_pr_enumeration(self):
        self.world.run["pull_requests"] = [{"number": 7, "head": {"sha": HEAD}}]
        self.assertTrue(self.tick())
        self.assertFalse(any(urlsplit(p).path == ROOT + "/pulls" for p, _ in self.world.calls))

    def test_unknown_503_is_not_success_and_recovers_without_rediscovery(self):
        page = ROOT + f"/commits/{HEAD}/check-runs?filter=all&per_page=100&page=1"
        self.world.overrides[page] = reply({}, 503)
        self.assertFalse(self.tick())
        self.assertEqual(self.payloads(), [])
        self.reopen()
        self.world.overrides.clear()
        self.assertTrue(self.tick())
        self.assertEqual(len(self.store.inbox(AUTHOR)), 1)
        self.assertEqual(len([p for p, _ in self.world.calls if urlsplit(p).path == ROOT + "/pulls"]), 1)

    def test_numeric_repository_pagination_preserves_filter_and_partial_failure(self):
        page1 = ROOT + f"/commits/{HEAD}/check-runs?filter=all&per_page=100&page=1"
        page2 = ROOT + f"/commits/{HEAD}/check-runs?filter=all&per_page=100&page=2"
        numeric = f"https://api.github.com/repositories/321/commits/{HEAD}/check-runs?filter=all&per_page=100&page=2"
        self.world.overrides[page1] = reply({"total_count": 101, "check_runs": [{"name": "Other"}] * 100},
                                          headers={"link": f'<{numeric}>; rel="next"', "etag": '"page1"'})
        self.world.overrides[page2] = reply({}, 503)
        self.assertFalse(self.tick())
        self.assertEqual(self.payloads(), [])
        self.world.overrides[page1] = Reply(304, {}, b"")
        self.world.overrides[page2] = reply({"total_count": 101, "check_runs": [self.world.check_row()]})
        self.assertTrue(self.tick())
        self.assertEqual(len(self.payloads()), 1)
        self.assertIn((page1, '"page1"'), self.world.calls)
        self.assertFalse(any(p.startswith("/repositories/") for p, _ in self.world.calls))

    def test_late_unique_assignment_adds_only_the_author_receipt(self):
        with self.store.db:
            self.store.db.execute("DELETE FROM assignments")
        self.assertFalse(self.tick())
        first = self.store.inbox(COORDINATOR)[0]
        self.assertEqual(self.store.inbox(AUTHOR), [])
        self.store.assign(REPO, 7, AUTHOR, "local-assignment:late")
        self.reopen()
        self.assertTrue(self.tick())
        self.assertEqual(len(self.payloads()), 1)
        self.assertEqual([r["receipt_id"] for r in self.store.inbox(COORDINATOR)], [first["receipt_id"]])
        self.assertEqual(len(self.store.inbox(AUTHOR)), 1)

    def test_conflicting_assignment_is_coordinator_only_then_can_be_reconciled(self):
        self.store.assign(REPO, 7, "another-author-test", "local-assignment:conflict")
        self.assertFalse(self.tick())
        self.assertEqual(self.store.inbox(AUTHOR), [])
        self.assertEqual(len(self.store.inbox(COORDINATOR)), 1)
        with self.store.db:
            self.store.db.execute("DELETE FROM assignments WHERE session='another-author-test'")
        self.assertTrue(self.tick())
        self.assertEqual(len(self.store.inbox(AUTHOR)), 1)

    def test_outbox_commit_interruption_retries_with_stable_identity_and_scoped_delivery(self):
        unrelated = Key(REPO, HEAD, 999, 1, "999")
        self.store.publish(unrelated, "terminal", dict(source_head=HEAD, tested_sha=None, pr=7), [COORDINATOR])
        original = self.store.publish

        def interrupted(*args, **kwargs):
            original(*args, **kwargs)
            raise SystemExit(17)

        with patch.object(self.store, "publish", interrupted), self.assertRaises(SystemExit):
            self.tick()
        self.assertEqual(len(self.payloads()), 1)
        self.assertEqual(self.store.inbox(COORDINATOR), [])
        self.reopen()
        with patch.object(self.store, "deliver", side_effect=AssertionError("unscoped")):
            self.assertTrue(self.tick())
        self.assertEqual(len(self.payloads()), 1)
        self.assertEqual(len(self.store.inbox(AUTHOR)), 1)
        self.assertEqual(len(self.store.inbox(COORDINATOR)), 1)
        self.assertEqual(next(r for r in self.store.outbox() if r["payload"]["action"] == "terminal")["state"], "pending")

    def test_deliver_false_and_lock_requirement(self):
        observer = Observer(self.store, API(self.store, self.world))
        with self.assertRaises(RuntimeError):
            reconcile_publisher(observer, self.world.run, self.world.jobs)
        self.assertTrue(self.tick(deliver=False))
        self.assertEqual(self.store.inbox(AUTHOR), [])
        self.assertTrue(self.tick())
        self.assertEqual(len(self.store.inbox(AUTHOR)), 1)


def publisher_fixture():
    return json.loads(Path(__file__).with_name("merge_publisher_v2.json").read_text())


def contract_files():
    location = os.environ.get("OBSERVER_MERGE_PUBLISHER_ROOT")
    if location is None:
        return None
    if not location.strip():
        raise AssertionError("OBSERVER_MERGE_PUBLISHER_ROOT 必须显式指定 producer 路径")
    root = Path(location).resolve()
    publisher = root / ".github/scripts/merge-gate-publish.py"
    workflow = root / ".github/workflows/merge-conflict.yml"
    if not all(path.is_file() for path in (publisher, workflow, workflow.with_name("merge-gate.yml"))):
        raise AssertionError("显式指定的 producer 缺少 publisher 或原生 workflow：" + str(root))
    return publisher, workflow


def workflow_text(filename="merge-conflict.yml"):
    files = contract_files()
    return files[1].with_name(filename).read_text() if files else publisher_fixture()["workflows"][filename]


def workflow_names(filename="merge-conflict.yml", jobs=("resolve", "report")):
    import re
    text = workflow_text(filename)
    names = {}
    for name in jobs:
        block = re.search(r"(?ms)^  " + name + r":\n(.*?)(?=^  [a-z_]+:|\Z)", text)
        if not block:
            raise AssertionError("Missing actual workflow job: " + name)
        names[name] = re.search(r"(?m)^    name: (.+)$", block[1])[1]
    return names


def native_publisher_check(publisher, pr, metadata, run, check_id, targets=None):
    # Load actual definitions, never imports, main execution or the GitHub transport.
    # API writes below are in-memory fixture operations, never GitHub calls.
    tree = ast.parse(publisher.read_text())
    selected = ast.Module(body=[node for node in tree.body if isinstance(node, ast.FunctionDef) or
                              isinstance(node, ast.ClassDef) and all(isinstance(base, ast.Name) and base.id == "ValueError"
                                                                    for base in node.bases)], type_ignores=[])

    def check(condition, message):
        if not condition:
            raise ValueError(message)

    chosen = targets[0] if targets is not None and len(targets) == 1 else pr
    merge = None
    validation = metadata.get("validation")
    if chosen.get("merge_commit_sha"):
        merge = {"sha": chosen["merge_commit_sha"], "tree": {"sha": validation["identity"]["tree"] if validation else TREE},
                 "parents": [{"sha": chosen["base"]["sha"]}, {"sha": chosen["head"]["sha"]}]}
    state = (chosen, chosen["base"]["sha"], merge)
    native = {"id": int(validation["identity"]["run_id"]), "run_attempt": int(validation["identity"]["run_attempt"]),
              "event": "pull_request", "status": "completed", "conclusion": "success"} if validation else None
    import re
    namespace = dict(REPO=REPO, APP_ID=15368, PUBLISHER=PUBLISHER, WORKFLOW=NATIVE, re=re, json=json, check=check,
                     contextmanager=contextmanager,
                     gate=SimpleNamespace(SHA=r"[0-9a-f]{40}", verifier=SimpleNamespace(decode=json.loads)),
                     subprocess=SimpleNamespace(CalledProcessError=type("FixtureAPIError", (Exception,), {})),
                     zipfile=SimpleNamespace(BadZipFile=type("FixtureArchiveError", (Exception,), {})))
    exec(compile(selected, publisher.name, "exec"), namespace)
    namespace.update(stable_snapshot=lambda *args: state, snapshot=lambda *args: state,
                     latest_run=lambda *args: native, candidate_policy_changed=lambda *args: False,
                     matching_prs=lambda *args: [chosen], proof=lambda *args: validation)

    class MemoryAPI:
        def __init__(self):
            self.rows = []

        def get(self, endpoint):
            if endpoint == "git/ref/heads/master":
                return {"object": {"sha": chosen["base"]["sha"]}}
            for current in [pr, *(targets or [])]:
                if endpoint == f"pulls/{current['number']}":
                    return copy.deepcopy(current)
            raise AssertionError(endpoint)

        def pages(self, endpoint, field):
            if endpoint == "pulls?state=open" and field is None:
                return copy.deepcopy(targets if targets is not None else [pr])
            assert field == "check_runs" and endpoint.endswith("/check-runs?filter=all")
            target = endpoint.split("/")[1]
            return [copy.deepcopy(row) for row in self.rows if row["head_sha"] == target]

        def write(self, method, endpoint, data):
            assert method == "POST" and endpoint == "check-runs"
            row = dict(data, id=check_id, app={"id": 15368, "slug": "github-actions"},
                       html_url=f"https://github.com/{REPO}/runs/{check_id}")
            self.rows.append(row)
            return row

    api = MemoryAPI()
    output = io.StringIO()
    with redirect_stdout(output):
        namespace["reconcile"](api, pr["number"], Path("."), {
            "GITHUB_RUN_ID": str(run["id"]), "GITHUB_RUN_ATTEMPT": str(run["run_attempt"]),
            "MERGE_TRUSTED_WORKFLOW_SHA": run["head_sha"], "MERGE_SOURCE_SHA": pr["head"]["sha"]})
    row, = api.rows
    record, = [json.loads(line.removeprefix("MERGE_CHECK ")) for line in output.getvalue().splitlines()]
    assert namespace["owned"](row, pr)
    return {"check": row, "log": record}


def publisher_check(pr, metadata, run, check_id, targets=None):
    request = dict(pr=pr, validation=metadata.get("validation"),
                   publisher={key: run[key] for key in ("id", "run_attempt", "head_sha")},
                   check_id=check_id, targets=targets)
    key = hashlib.sha256(json.dumps(request, sort_keys=True).encode()).hexdigest()
    exchange = publisher_fixture()["exchanges"].get(key)
    if exchange is None:
        raise AssertionError("原生 publisher fixture 不匹配；请用显式固定的 producer 重新核对")
    files = contract_files()
    if files:
        actual = native_publisher_check(files[0], pr, metadata, run, check_id, targets)
        if actual != exchange:
            raise AssertionError("显式 producer 与原生 fixture 不一致；请核对源码 SHA 和摘要")
        exchange = actual
    row, record = exchange["check"], exchange["log"]
    validation = metadata.get("validation")
    assert row["name"] == "Merge gate"
    assert row["app"] == {"id": 15368, "slug": "github-actions"}
    assert row["external_id"] == f"merge-gate:v2:{REPO}:source:{pr['head']['sha']}"
    assert record["target_sha"] == row["head_sha"] == pr["head"]["sha"]
    assert record["check_id"] == row["id"] and record["conclusion"] == row.get("conclusion")
    assert (record["publisher_run_id"], record["publisher_run_attempt"]) == (str(run["id"]), str(run["run_attempt"]))
    if validation:
        assert (record["validated_sha"], record["base"], record["tree"]) == tuple(validation["identity"][key] for key in ("sha", "base", "tree"))
        assert record["target_sha"] != record["validated_sha"]
    return row


class MatrixWorld(World):
    def __init__(self):
        super().__init__()
        self.run["run_started_at"] = "2026-11-26T02:00:00Z"
        self.pr["draft"] = False
        self.prs, self.signals, self.validations, self.commits = {}, {}, {}, {}
        self.names = workflow_names()
        self.jobs = [self.job(self.names["resolve"], 700)]

    def job(self, name, job_id, **changes):
        return dict(id=job_id, name=name, run_id=self.run["id"], run_attempt=self.run["run_attempt"], head_sha=POLICY,
                    status="completed", conclusion="success", started_at="2026-11-26T02:01:00Z",
                    completed_at="2026-11-26T02:02:00Z", html_url=f"https://github.com/{REPO}/actions/runs/201/job/{job_id}", **changes)

    def add(self, number, success=False):
        pr = copy.deepcopy(self.pr)
        pr.update(number=number, html_url=f"https://github.com/{REPO}/pull/{number}")
        pr["head"].update(sha=f"{100 + number:040x}", ref=f"feature/pr-{number}")
        metadata = copy.deepcopy(self.metadata)
        metadata.update(pr=number, head=pr["head"]["sha"])
        if success:
            merged, target_tree = f"{1000 + number:040x}", f"{2000 + number:040x}"
            pr.update(mergeable=True, mergeable_state="clean", merge_commit_sha=merged)
            identity = dict(repo=REPO, pr=number, head=pr["head"]["sha"], base=BASE, sha=merged, tree=target_tree,
                            run_id=str(100 + number), run_attempt="1")
            metadata.update(result="success", validation={"identity": identity, "source": source(pr), "controller_sha": ""})
            self.validations[100 + number] = dict(id=100 + number, run_attempt=1, path=NATIVE, workflow_id=18,
                repository=self.repo, head_repository=pr["head"]["repo"], head_sha=pr["head"]["sha"],
                head_branch=pr["head"]["ref"], event="pull_request", status="completed", conclusion="success",
                html_url=f"https://github.com/{REPO}/actions/runs/{100 + number}")
            self.commits[merged] = {"sha": merged, "tree": {"sha": target_tree},
                                    "parents": [{"sha": BASE}, {"sha": pr["head"]["sha"]}]}
        row = publisher_check(pr, metadata, self.run, 800 + number)
        self.signals[number] = {"row": row, "metadata": json.loads(row["output"]["summary"])}
        self.prs[number] = pr
        self.jobs.append(self.job(self.names["report"].replace("${{ matrix.pr }}", str(number)), 700 + number))
        return pr

    def row(self, number):
        signal = self.signals[number]
        return dict(signal["row"], output={"summary": json.dumps(signal["metadata"])})

    def __call__(self, path, etag=None):
        parsed = urlsplit(path)
        data = None
        if parsed.path == ROOT + "/actions/workflows/18":
            data = {"id": 18, "path": NATIVE, "state": "active"}
        if path in self.overrides:
            self.calls.append((path, etag))
            value = self.overrides[path]
            return value(path, etag) if callable(value) else value
        for number, pr in self.prs.items():
            if parsed.path == ROOT + f"/pulls/{number}":
                data = pr
            elif parsed.path == ROOT + f"/commits/{self.signals[number]['row']['head_sha']}/check-runs":
                data = {"total_count": 1, "check_runs": [self.row(number)]}
            elif parsed.path == ROOT + f"/check-runs/{800 + number}":
                data = self.row(number)
        for run_id, native in self.validations.items():
            if parsed.path in {ROOT + f"/actions/runs/{run_id}", ROOT + f"/actions/runs/{run_id}/attempts/1"}:
                data = native
        if parsed.path.startswith(ROOT + "/git/commits/"):
            data = self.commits.get(parsed.path.rsplit("/", 1)[1])
        if data is None:
            return super().__call__(path, etag)
        self.calls.append((path, etag))
        return reply(data)


class MatrixProtocol(Case):
    def setUp(self):
        super().setUp()
        self.world = MatrixWorld()
        for number in range(8, 15):
            self.store.assign(REPO, number, f"author-test-{number}", f"local-assignment:{number}")

    def test_native_workflow_and_publisher_contract_with_multiple_prs(self):
        text = workflow_text()
        self.assertIn("fail-fast: false", text)
        self.assertIn("group: merge-publisher-${{ github.repository }}-${{ matrix.source_sha }}", text)
        self.assertEqual(self.world.names["resolve"], "Resolve Merge PR identities")
        self.world.add(7)
        self.world.add(8, success=True)
        self.assertEqual([j["name"] for j in self.world.jobs], [
            "Resolve Merge PR identities", "Trusted Merge report / PR 7", "Trusted Merge report / PR 8"])
        self.assertEqual(self.world.signals[8]["row"]["head_sha"], self.world.prs[8]["head"]["sha"])
        self.assertEqual(self.world.signals[8]["row"]["external_id"], f"merge-gate:v2:{REPO}:source:{self.world.prs[8]['head']['sha']}")
        self.assertTrue(self.tick())
        self.assertEqual({p["pr"] for p in self.payloads()}, {7, 8})
        self.assertEqual({p["check_conclusion"] for p in self.payloads()}, {"success", "failure"})
        failed, = [p for p in self.payloads() if p["check_conclusion"] == "failure"]
        self.assertEqual((failed["reporter_job_id"], failed["reporter_job_conclusion"], failed["failure_kind"]), (707, "success", "merge_conflict"))
        self.assertIsNone(failed["tested_sha"])
        self.assertEqual(failed["tests"], [])
        self.assertEqual(len(self.store.inbox(AUTHOR)), 1)
        self.assertEqual(len(self.store.inbox("author-test-8")), 1)
        self.assertFalse(any(urlsplit(p).path == ROOT + "/pulls" for p, _ in self.world.calls))

    def test_mixed_reporter_failures_foreign_checks_and_unknown_do_not_suppress_valid_prs(self):
        for number in range(7, 13):
            self.world.add(number, success=number == 8)
        self.world.run["conclusion"] = "failure"
        self.world.jobs[3]["conclusion"] = "failure"
        self.world.signals[10]["metadata"]["publisher"]["run_id"] = "999"
        pending = self.world.signals[12]
        pending["metadata"]["result"] = "pending"
        pending["row"].update(status="in_progress", conclusion=None)
        target = self.world.signals[11]["row"]["head_sha"]
        self.world.overrides[ROOT + f"/commits/{target}/check-runs?filter=all&per_page=100&page=1"] = reply({}, 503)
        self.assertFalse(self.tick())
        self.assertEqual({p["pr"] for p in self.payloads()}, {7, 8})
        self.assertEqual({p["reporter_job_conclusion"] for p in self.payloads()}, {"success"})
        self.assertEqual(len(self.store.inbox(AUTHOR)), 1)
        self.assertEqual(len(self.store.inbox("author-test-8")), 1)
        self.reopen()
        self.world.overrides.clear()
        self.assertFalse(self.tick())
        self.assertEqual({p["pr"] for p in self.payloads()}, {7, 8, 11})
        self.assertEqual(len(self.store.inbox(AUTHOR)), 1)
        self.assertEqual(len(self.store.inbox(COORDINATOR)), 3)

    def test_shared_source_pending_does_not_block_other_sources(self):
        first = self.world.add(7)
        self.world.add(8)
        second = copy.deepcopy(first)
        second.update(number=9, html_url=f"https://github.com/{REPO}/pull/9")
        second["base"]["ref"] = "stacked"
        row = publisher_check(first, {}, self.world.run, 807, targets=[first, second])
        self.world.signals[7] = {"row": row, "metadata": json.loads(row["output"]["summary"])}
        self.assertEqual(self.world.signals[7]["metadata"]["reason_code"], "shared_head_multiple_targets")
        self.assertFalse(self.tick())
        self.assertEqual({p["pr"] for p in self.payloads()}, {8})
        self.assertEqual(self.store.inbox(AUTHOR), [])
        self.assertEqual(self.store.meta_get(f"merge-report-source:{REPO}:201:2:7", {}).get("state"), "waiting")
        self.assertEqual(self.store.meta_get(f"merge-report-source:{REPO}:201:2:8")["state"], "completed")

    def test_v2_job_candidate_can_resolve_to_a_different_verified_open_pr(self):
        old = self.world.add(7)
        old["state"] = "closed"
        current = copy.deepcopy(old)
        current.update(number=8, state="open", html_url=f"https://github.com/{REPO}/pull/8")
        row = publisher_check(old, {}, self.world.run, 808, targets=[current])
        signal = {"row": row, "metadata": json.loads(row["output"]["summary"])}
        self.world.prs[8] = current
        self.world.signals[7] = self.world.signals[8] = signal
        self.assertEqual(signal["metadata"]["pr"], 8)
        self.assertTrue(self.tick())
        payload, = self.payloads()
        self.assertEqual((payload["pr"], payload["reporter_job_id"]), (8, 707))
        self.assertEqual(self.store.inbox(AUTHOR), [])
        self.assertEqual(len(self.store.inbox("author-test-8")), 1)

    def test_two_successful_prs_do_not_share_one_store_run_target(self):
        self.world.add(7, success=True)
        self.world.add(8, success=True)
        self.assertTrue(self.tick())
        self.assertEqual({p["tested_sha"] for p in self.payloads()}, {f"{1007:040x}", f"{1008:040x}"})
        self.assertIsNone(self.store.target(REPO, 201, 2))
        self.assertTrue(self.tick())
        self.assertEqual(len(self.payloads()), 2)

    def test_native_workflow_job_shape_keeps_first_failure_and_only_adds_check_result(self):
        pr = self.world.add(7)
        self.world.add(8)
        names = workflow_names("merge-gate.yml", ("plan", "tests", "evidence"))
        self.assertEqual((names["plan"], names["evidence"]), ("Merge plan", "Merge evidence"))
        native = dict(id=101, run_attempt=1, path=NATIVE, workflow_id=18, repository=self.world.repo,
                      head_repository=pr["head"]["repo"], head_sha=pr["head"]["sha"], head_branch=pr["head"]["ref"],
                      event="pull_request", status="completed", conclusion="failure", run_started_at="2026-11-26T01:00:00Z",
                      html_url=f"https://github.com/{REPO}/actions/runs/101", pull_requests=[pr])
        job = dict(id=501, run_id=101, run_attempt=1, head_sha=native["head_sha"], status="completed", conclusion="failure",
                   name=names["tests"].replace("${{ matrix.unit }}", "observer-contract"),
                   started_at="2026-11-26T01:01:00Z", completed_at="2026-11-26T01:02:00Z",
                   html_url=f"https://github.com/{REPO}/actions/runs/101/job/501")
        self.world.overrides[ROOT + "/actions/jobs/501/logs"] = Reply(200, {}, b"--- FAIL: TestObservedFailure (0.01s)\n")
        self.store.observe(REPO, 101, 1, {"run": native, "jobs": [job]})
        with Observer(self.store, API(self.store, self.world)) as observer:
            observer.notice(observer.prepare(native, True), [job])
        self.assertTrue(self.tick())
        native_action, = [row["payload"] for row in self.store.actions() if row["kind"] == "first_failure"]
        update, = [p for p in self.payloads() if p["action"] == RESULT]
        self.assertEqual(native_action["job_name"], "Merge tests / observer-contract")
        self.assertEqual(native_action["tests"][0]["name"], "TestObservedFailure")
        self.assertEqual((update["pr"], update["native_failure_run_id"], update["investigation"], update["tests"]), (7, 101, False, []))
        self.assertEqual({p["pr"] for p in self.payloads() if p["action"] == FAILURE}, {8})
        self.assertEqual(len(self.store.inbox(AUTHOR)), 2)

    def test_noncanonical_candidate_name_is_unknown_only_for_that_job_and_can_recover(self):
        self.world.add(7)
        self.world.add(8)
        valid = self.world.jobs[1]["name"]
        self.world.jobs[1]["name"] = valid.replace("PR 7", "PR 07")
        self.assertFalse(self.tick())
        self.assertEqual({p["pr"] for p in self.payloads()}, {8})
        self.world.jobs[1]["name"] = valid
        self.assertTrue(self.tick())
        self.assertEqual({p["pr"] for p in self.payloads()}, {7, 8})
        self.assertEqual(self.store.db.execute("SELECT scope FROM problems").fetchall(), [])

    def test_missing_job_times_do_not_authorize_even_when_other_pr_finished_in_active_run(self):
        self.world.add(7)
        self.world.add(8)
        self.world.run.update(status="in_progress", conclusion=None)
        self.world.jobs[1]["started_at"] = None
        self.assertFalse(self.tick())
        self.assertEqual({p["pr"] for p in self.payloads()}, {8})
        self.world.jobs[1].update(started_at="2026-11-26T02:01:00Z", completed_at=None)
        self.assertFalse(self.tick())
        self.assertEqual({p["pr"] for p in self.payloads()}, {8})

    def test_author_conflict_during_delivery_stays_retryable_without_repeating_coordinator(self):
        self.world.add(7)
        self.world.add(8)
        deliver = self.store.deliver_actions

        def conflict(actions, coordinator):
            self.store.assign(REPO, 7, "late-conflicting-author", "local-conflict:7")
            return deliver(actions, coordinator)

        with patch.object(self.store, "deliver_actions", conflict):
            self.assertFalse(self.tick())
        self.assertEqual(self.store.inbox(AUTHOR), [])
        self.assertEqual(len(self.store.inbox("author-test-8")), 1)
        self.assertEqual(len(self.store.inbox(COORDINATOR)), 2)
        with self.store.db:
            self.store.db.execute("DELETE FROM assignments WHERE session='late-conflicting-author'")
        self.reopen()
        self.assertTrue(self.tick())
        self.assertEqual(len(self.store.inbox(AUTHOR)), 1)
        self.assertEqual(len(self.store.inbox(COORDINATOR)), 2)

    def test_terminal_v2_metadata_with_multiple_targets_is_unknown_only_for_its_source(self):
        self.world.add(7)
        self.world.add(8)
        self.world.signals[7]["metadata"]["targets"].extend(self.world.signals[8]["metadata"]["targets"])
        self.assertFalse(self.tick())
        self.assertEqual({p["pr"] for p in self.payloads()}, {8})
        self.assertEqual(self.store.inbox(AUTHOR), [])

    def test_wrong_publisher_workflow_sha_does_not_authorize_its_pr(self):
        self.world.add(7)
        self.world.add(8)
        self.world.signals[7]["metadata"]["publisher"]["workflow_sha"] = HEAD
        self.assertFalse(self.tick())
        self.assertEqual({p["pr"] for p in self.payloads()}, {8})
        self.assertEqual(self.store.inbox(AUTHOR), [])

    def test_resolve_green_without_reporters_is_not_an_all_pr_success(self):
        self.assertFalse(self.tick())
        self.assertEqual(self.payloads(), [])
        self.assertEqual(self.store.events(), [])

    def test_job_name_is_only_a_canonical_candidate_not_route_authority(self):
        self.world.add(7)
        self.world.add(8)
        self.world.signals[7]["row"]["external_id"] = f"merge-gate:v1:{REPO}:pr:8:head:{self.world.prs[7]['head']['sha']}"
        self.assertFalse(self.tick())
        self.assertEqual({p["pr"] for p in self.payloads()}, {8})
        self.assertEqual(self.store.inbox(AUTHOR), [])

    def test_duplicate_reporters_only_block_their_own_pr_and_exact_duplicates_are_idempotent(self):
        self.world.add(7)
        self.world.add(8)
        duplicate = dict(self.world.jobs[1], id=999, html_url=f"https://github.com/{REPO}/actions/runs/201/job/999")
        self.world.jobs.append(duplicate)
        self.assertFalse(self.tick())
        self.assertEqual({p["pr"] for p in self.payloads()}, {8})
        self.world.jobs.pop()
        self.world.jobs.append(copy.deepcopy(self.world.jobs[1]))
        self.assertTrue(self.tick())
        self.assertEqual({p["pr"] for p in self.payloads()}, {7, 8})
        self.assertEqual(len(self.store.inbox(COORDINATOR)), 2)

    def test_rerun_inherited_green_relabelled_attempt_does_not_become_new_attempt_work(self):
        self.world.add(7)
        self.assertTrue(self.tick())
        first, = self.payloads()
        self.world.run.update(run_attempt=3, run_started_at="2026-11-26T03:00:00Z", conclusion="failure")
        for job in self.world.jobs:
            job["run_attempt"] = 3
        self.world.add(8)
        self.world.signals[8]["metadata"]["publisher"]["run_attempt"] = "3"
        self.world.jobs[-1].update(started_at="2026-11-26T03:01:00Z", completed_at="2026-11-26T03:02:00Z")
        self.assertTrue(self.tick())
        self.assertEqual({(p["pr"], p["attempt"]) for p in self.payloads()}, {(7, 2), (8, 3)})
        self.assertEqual(first["reporter_job_id"], 707)
        self.assertEqual(len(self.store.inbox(AUTHOR)), 1)
        self.assertTrue(self.tick())
        self.assertEqual(len(self.store.inbox(COORDINATOR)), 2)


if __name__ == "__main__":
    unittest.main()
