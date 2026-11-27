#!/usr/bin/env python3
"""Trusted publisher/provenance/lock tests; all API mutations stay in memory."""
import copy
from contextlib import redirect_stdout
import hashlib
import importlib.util
import io
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest import mock
import zipfile

HERE = Path(__file__).resolve().parent
REPO = "PeterFile/ct0x"


def load():
    path = HERE / "merge-gate-publish.py"
    if not path.exists():
        return None
    spec = importlib.util.spec_from_file_location("merge_publish", path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


class PublisherTests(unittest.TestCase):
    def setUp(self):
        self.p = load()
        self.assertIsNotNone(self.p, "缺少可信单一发布器与锁内消费实现")
        self.head, self.base, self.sha, self.tree = (c * 40 for c in "abcd")
        repo = {"id": 1439910892, "full_name": REPO}
        fork = {"id": 17, "full_name": "contributor/ct0x"}
        self.pr = {"number": 7, "state": "open", "draft": False, "merged": False,
                   "head": {"sha": self.head, "ref": "branch ; $()", "repo": fork},
                   "base": {"sha": self.base, "ref": "master", "repo": repo},
                   "merge_commit_sha": self.sha, "mergeable": True, "mergeable_state": "clean"}
        self.run = {"id": 101, "run_attempt": 2, "repository": repo, "head_repository": fork,
                    "head_sha": self.head, "head_branch": self.pr["head"]["ref"], "event": "pull_request",
                    "path": ".github/workflows/merge-gate.yml", "workflow_id": 99,
                    "status": "completed", "conclusion": "success", "pull_requests": []}
        self.merge = {"sha": self.sha, "tree": {"sha": self.tree},
                      "parents": [{"sha": self.base}, {"sha": self.head}]}
        identity = {"repo": REPO, "pr": 7, "head": self.head, "base": self.base,
                    "sha": self.sha, "tree": self.tree, "run_id": "101", "run_attempt": "2"}
        self.receipt = {**identity, "schema": 1, "result": "success", "policy_digest": "e" * 64,
                        "source": {"repo_id": 17, "repo": "contributor/ct0x", "ref": self.pr["head"]["ref"], "base_ref": "master"},
                        "tasks": [{"unit": "identity-diff", "kind": "identity"}],
                        "evidence": [{**identity, "unit": "identity-diff", "result": "success"}]}
        self.receipt["evidence"][0]["source"] = copy.deepcopy(self.receipt["source"])
        self.jobs = [{"name": name, "run_id": 101, "run_attempt": 2, "status": "completed", "conclusion": "success"}
                     for name in ("Merge plan", "Merge tests / identity-diff", "Merge evidence")]

    def validate(self, **changes):
        values = dict(receipt=self.receipt, run=self.run, jobs=self.jobs, pr=self.pr,
                      master=self.base, merge=self.merge, workflow_id=99)
        return self.p.validate_proof(**{**values, **changes})

    def test_empty_pull_requests_is_valid_but_fork_and_workflow_identity_are_not_optional(self):
        self.validate()
        for key, value in (("path", ".github/workflows/other.yml"), ("workflow_id", 98),
                           ("event", "push"), ("head_repository", {"id": 18, "full_name": "other/ct0x"}),
                           ("head_sha", self.base), ("run_attempt", 1)):
            with self.subTest(key=key), self.assertRaises(ValueError):
                self.validate(run={**self.run, key: value})

    def test_identity_attempt_repo_tree_and_terminal_job_failures(self):
        for key in ("repo", "pr", "head", "base", "sha", "tree", "run_id", "run_attempt"):
            with self.subTest(key=key), self.assertRaises(ValueError):
                self.validate(receipt={**self.receipt, key: "wrong"})
        for state in ("cancelled", "skipped", "failure", None):
            jobs = copy.deepcopy(self.jobs)
            jobs[1]["conclusion"] = state
            with self.subTest(state=state), self.assertRaises(ValueError):
                self.validate(jobs=jobs)
        with self.assertRaises(ValueError):
            self.validate(jobs=self.jobs[:-1])
        with self.assertRaises(ValueError):
            self.validate(merge={**self.merge, "parents": [{"sha": self.head}, {"sha": self.base}]})

    def test_consumption_rejects_closed_merged_draft_retarget_and_review_mismatch(self):
        self.p.validate_live(self.pr, self.head, consuming=True)
        for key, value in (("state", "closed"), ("merged", True), ("draft", True),
                           ("base", {**self.pr["base"], "ref": "other"})):
            with self.subTest(key=key), self.assertRaises(ValueError):
                self.p.validate_live({**self.pr, key: value}, self.head, consuming=True)
        with self.assertRaises(ValueError):
            self.p.validate_live(self.pr, self.base, consuming=True)
        with self.assertRaises(ValueError):
            self.validate(master="f" * 40)

    def test_artifact_zip_digest_and_exact_single_data_member(self):
        def archive(extra=False):
            output = io.BytesIO()
            with zipfile.ZipFile(output, "w") as target:
                target.writestr("merge-gate.json", json.dumps(self.receipt))
                if extra:
                    target.writestr("../execute.py", "raise Exception()")
            return output.getvalue()
        data = archive()
        digest = "sha256:" + hashlib.sha256(data).hexdigest()
        self.assertEqual(self.p.decode_artifact(data, digest), self.receipt)
        with self.assertRaises(ValueError):
            self.p.decode_artifact(data, "sha256:" + "0" * 64)
        with self.assertRaises(ValueError):
            self.p.decode_artifact(archive(True), "sha256:" + hashlib.sha256(archive(True)).hexdigest())

    def test_conflict_requires_live_confirmation_and_does_not_trust_old_merge(self):
        conflict = {**self.pr, "mergeable": False, "mergeable_state": "dirty", "merge_commit_sha": None}
        self.assertEqual(self.p.merge_state(conflict, self.base, None), "conflict")
        self.assertEqual(self.p.merge_state({**conflict, "mergeable": None}, self.base, None), "unknown")
        self.assertEqual(self.p.merge_state({**conflict, "merge_commit_sha": self.sha}, self.base, self.merge), "unknown")
        stale = {**self.merge, "parents": [{"sha": "f" * 40}, {"sha": self.head}]}
        self.assertEqual(self.p.merge_state(conflict, self.base, stale), "conflict")

    def test_idempotent_single_check_updates_same_head_recovery_and_repairs_duplicates(self):
        api = CheckAPI()
        self.p.publish_check(api, self.pr, "failure", {"reason": "合并冲突"})
        self.p.publish_check(api, self.pr, "failure", {"reason": "合并冲突"})
        self.assertEqual(len(api.checks), 1)
        self.p.publish_check(api, self.pr, "success", {"run_id": "101", "run_attempt": "2"})
        self.assertEqual(len(api.checks), 1)
        self.assertEqual(api.checks[0]["conclusion"], "success")
        duplicate = copy.deepcopy(api.checks[0])
        duplicate["id"] = 22
        api.checks.append(duplicate)
        self.p.publish_check(api, self.pr, "failure", {"reason": "合并冲突"})
        self.assertEqual(len([row for row in api.checks if row["name"] == "Merge gate"]), 1)
        self.assertFalse(any("contributor/" in endpoint for endpoint in api.writes))

    def test_foreign_same_name_or_pr_collision_cannot_authorize_or_be_modified(self):
        api = CheckAPI()
        api.checks.append({"id": 4, "name": "Merge gate", "head_sha": self.head,
                           "app": {"id": 3, "slug": "other"}, "external_id": "untrusted"})
        with self.assertRaises(ValueError):
            self.p.publish_check(api, self.pr, "success", {})
        self.assertEqual(api.writes, [])

    def test_mutation_rechecks_live_snapshot_and_newer_attempt(self):
        api = CheckAPI()
        with self.assertRaises(ValueError):
            self.p.publish_check(api, self.pr, "success", {}, guard=lambda: False)
        self.assertEqual(api.writes, [])

    def test_shared_mkdir_lock_blocks_other_worktree_and_does_not_release_owner(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            subprocess.run(["git", "init", "-q", str(root)], check=True)
            lock = root / ".local/merge.lock"
            lock.parent.mkdir()
            lock.mkdir()
            with self.assertRaises((ValueError, FileExistsError)):
                with self.p.merge_lock(root):
                    self.fail("entered another writer's lock")
            self.assertTrue(lock.is_dir())
            lock.rmdir()
            with self.p.merge_lock(root):
                self.assertTrue(lock.is_dir())
            self.assertFalse(lock.exists())

    def api_fixture(self, root=None):
        root = root or HERE.parents[1]
        self.receipt["policy_digest"] = self.p.gate.policy_digest(root)
        return ProvenanceAPI(self, root)

    def test_real_reconcile_conflict_recovery_and_async_unknown(self):
        api = self.api_fixture()
        original = copy.deepcopy(api.pr)
        api.pr.update(mergeable=False, mergeable_state="dirty", merge_commit_sha=None)
        api.runs = []
        context = {"GITHUB_RUN_ID": "201", "GITHUB_RUN_ATTEMPT": "1"}
        self.p.reconcile(api, 7, HERE.parents[1], context)
        self.assertEqual(api.checks[0]["conclusion"], "failure")
        api.pr = original
        api.runs = [self.run]
        self.p.reconcile(api, 7, HERE.parents[1], context)
        required = [row for row in api.checks if row["name"] == "Merge gate"]
        self.assertEqual(len(required), 1)
        self.assertEqual(required[0]["conclusion"], "success")
        self.assertEqual(required[0]["head_sha"], self.head)
        self.assertEqual(len(api.checks), 1)
        api.pr["mergeable"] = None
        self.p.reconcile(api, 7, HERE.parents[1], context)
        self.assertEqual(next(row for row in api.checks if row["name"] == "Merge gate")["status"], "in_progress")

    def test_terminal_failure_is_bound_to_exact_current_plan_not_only_shared_head(self):
        context = {"GITHUB_RUN_ID": "201", "GITHUB_RUN_ATTEMPT": "1"}
        for conclusion in ("success", "failure"):
            for moved in (False, True):
                with self.subTest(conclusion=conclusion, moved=moved):
                    self.run.update(conclusion=conclusion, pull_requests=[{
                        "number": 7, "base": {"sha": self.base}, "head": {"sha": self.head}}])
                    api = self.api_fixture()
                    if moved:
                        api.master = "f" * 40
                        api.pr["base"]["sha"] = api.master
                        api.pr["merge_commit_sha"] = "e" * 40
                        api.current_merge = {"sha": "e" * 40, "tree": {"sha": "0" * 40},
                                             "parents": [{"sha": api.master}, {"sha": self.head}]}
                    self.p.reconcile(api, 7, HERE.parents[1], context)
                    summary = json.loads(api.checks[0]["output"]["summary"])
                    self.assertEqual(summary["result"], "pending" if moved else conclusion)
                    self.assertEqual(summary["failed_prs"], [7] if conclusion == "failure" and not moved else [])
                    self.assertEqual(api.dispatches, int(moved))
                    if moved:
                        self.assertEqual(api.checks[0]["status"], "in_progress")

    def test_failed_run_without_exact_plan_or_with_wrong_tree_remains_pending_and_refreshes(self):
        self.run["conclusion"] = "failure"
        for kind in ("missing", "wrong-tree"):
            with self.subTest(kind=kind):
                api = self.api_fixture()
                if kind == "missing":
                    api.plan_artifact = None
                else:
                    plan = {**copy.deepcopy(self.receipt), "tree": "0" * 40}
                    output = io.BytesIO()
                    with zipfile.ZipFile(output, "w") as archive:
                        archive.writestr("merge-plan.json", json.dumps(plan))
                    api.plan_data = output.getvalue()
                    api.plan_artifact.update(size_in_bytes=len(api.plan_data), digest="sha256:" + hashlib.sha256(api.plan_data).hexdigest())
                self.p.reconcile(api, 7, HERE.parents[1], {"GITHUB_RUN_ID": "201", "GITHUB_RUN_ATTEMPT": "1"})
                summary = json.loads(api.checks[0]["output"]["summary"])
                self.assertEqual(summary["result"], "pending")
                self.assertEqual(summary["failed_prs"], [])
                self.assertEqual(api.dispatches, 1)

    def test_planning_failure_with_early_identity_is_red_only_for_its_exact_tree(self):
        self.run["conclusion"] = "failure"
        self.jobs[0]["conclusion"] = "failure"
        for kind in ("current", "stale", "missing"):
            with self.subTest(kind=kind):
                api = self.api_fixture()
                api.plan_artifact = None
                if kind != "missing":
                    identity = {key: self.receipt[key] for key in self.p.gate.IDENTITY}
                    identity.update(schema=1, kind="merge-identity", source=self.receipt["source"],
                                    policy_digest=self.receipt["policy_digest"], controller_sha="")
                    output = io.BytesIO()
                    with zipfile.ZipFile(output, "w") as archive:
                        archive.writestr("merge-identity.json", json.dumps(identity))
                    api.identity_data = output.getvalue()
                    api.identity_artifact = {"id": 44, "name": "merge-identity-101-2", "expired": False,
                                             "workflow_run": {"id": 101}, "size_in_bytes": len(api.identity_data),
                                             "digest": "sha256:" + hashlib.sha256(api.identity_data).hexdigest()}
                if kind == "stale":
                    api.master = "f" * 40
                    api.pr["base"]["sha"] = api.master
                    api.pr["merge_commit_sha"] = "e" * 40
                    api.current_merge = {"sha": "e" * 40, "tree": {"sha": "0" * 40},
                                         "parents": [{"sha": api.master}, {"sha": self.head}]}
                self.p.reconcile(api, 7, HERE.parents[1], {"GITHUB_RUN_ID": "201", "GITHUB_RUN_ATTEMPT": "1"})
                summary = json.loads(api.checks[0]["output"]["summary"])
                self.assertEqual(summary["result"], "failure" if kind == "current" else "pending")
                self.assertEqual(summary["failed_prs"], [7] if kind == "current" else [])
                self.assertEqual(api.dispatches, 0 if kind == "current" else 1)

    def test_trusted_controller_planning_failure_consumes_early_identity_not_missing_plan(self):
        self.run.update(event="workflow_dispatch", head_repository=self.pr["base"]["repo"], head_sha=self.base,
                        head_branch="master", display_title=self.p.dispatch_title(self.pr), conclusion="failure")
        self.jobs[0]["conclusion"] = "failure"
        self.receipt["controller_sha"] = self.base
        api = self.api_fixture()
        api.policy_changed = True
        api.plan_artifact = None
        identity = {key: self.receipt[key] for key in self.p.gate.IDENTITY}
        identity.update(schema=1, kind="merge-identity", source=self.receipt["source"],
                        policy_digest=self.receipt["policy_digest"], controller_sha=self.base)
        output = io.BytesIO()
        with zipfile.ZipFile(output, "w") as archive:
            archive.writestr("merge-identity.json", json.dumps(identity))
        api.identity_data = output.getvalue()
        api.identity_artifact = {"id": 44, "name": "merge-identity-101-2", "expired": False, "workflow_run": {"id": 101},
                                 "size_in_bytes": len(api.identity_data), "digest": "sha256:" + hashlib.sha256(api.identity_data).hexdigest()}
        self.p.reconcile(api, 7, HERE.parents[1], {"GITHUB_RUN_ID": "201", "GITHUB_RUN_ATTEMPT": "1"})
        summary = json.loads(api.checks[0]["output"]["summary"])
        self.assertEqual(summary["result"], "failure")
        self.assertEqual(summary["failed_prs"], [7])
        self.assertEqual(api.dispatches, 0)

    def test_dispatch_204_has_no_json_body_but_get_and_check_mutations_require_one(self):
        api = self.p.GitHub()
        completed = subprocess.CompletedProcess([], 0, stdout=b"", stderr=b"")
        with mock.patch.object(self.p.subprocess, "run", return_value=completed):
            try:
                result = api.write("POST", "actions/workflows/merge-gate.yml/dispatches", {"ref": "master", "inputs": {}})
            except ValueError as error:
                self.fail("HTTP 204 dispatch was rejected: " + str(error))
            self.assertIsNone(result)
            for method, endpoint in (("GET", "pulls/7"), ("POST", "check-runs")):
                with self.subTest(method=method), self.assertRaises(ValueError):
                    api.request(method, endpoint)

    def test_policy_change_dispatches_before_broken_candidate_protocol_and_unknown_write_is_not_retried(self):
        api = self.api_fixture()
        api.policy_changed = True
        api.archive = lambda _: self.fail("candidate protocol must not control fallback admission")
        api.dispatch_unknown = True
        context = {"GITHUB_RUN_ID": "201", "GITHUB_RUN_ATTEMPT": "1"}
        with self.assertRaises(subprocess.CalledProcessError):
            self.p.reconcile(api, 7, HERE.parents[1], context)
        self.assertEqual(api.dispatches, 1)
        self.p.reconcile(api, 7, HERE.parents[1], context)
        self.assertEqual(api.dispatches, 1)
        api.dispatch_unknown = False
        self.p.reconcile(api, 7, HERE.parents[1], {**context, "MERGE_REFRESH_ID": "202"})
        self.assertEqual(api.dispatches, 2)

    def test_log_is_bounded_structured_and_does_not_print_raw_metadata(self):
        output = io.StringIO()
        metadata = {"reason": "NOT_FOR_LOG", "publisher": {"run_id": "201", "run_attempt": "1"}}
        with redirect_stdout(output):
            self.p.publish_check(CheckAPI(), self.pr, "failure", metadata)
        lines = output.getvalue().splitlines()
        self.assertEqual(len(lines), 1)
        record = json.loads(lines[0].removeprefix("MERGE_CHECK "))
        self.assertEqual(record["pr"], 7)
        self.assertEqual(record["publisher_run_id"], "201")
        self.assertNotIn("NOT_FOR_LOG", output.getvalue())
        self.assertLess(len(lines[0]), 2048)

    def test_dispatch_resolver_separates_distinct_prs_with_master_as_run_branch(self):
        api = self.api_fixture()
        for number in (7, 8):
            live = {**copy.deepcopy(self.pr), "number": number}
            run = {**self.run, "event": "workflow_dispatch", "head_branch": "master", "display_title": self.p.dispatch_title(live)}
            original_get = api.get
            api.get = lambda endpoint, run=run, live=live: copy.deepcopy(run if endpoint == "actions/runs/101" else live)
            self.assertEqual(self.p.resolve_event(api, {"workflow_run": {"id": 101}}, "workflow_run"), [number])
            api.get = original_get
        second = {**self.pr, "number": 8, "base": {**self.pr["base"], "ref": "stacked", "sha": "f" * 40}}
        self.p.publish_check(api, {**self.pr, "_check_sha": self.sha}, "success", {})
        self.p.publish_check(api, {**second, "_check_sha": "e" * 40}, "failure", {})
        required = [row for row in api.checks if row["name"] == "Merge gate"]
        self.assertEqual(len(required), 1)
        self.assertEqual(required[0]["head_sha"], self.head)
        self.assertEqual(required[0]["conclusion"], "failure")

    def test_prior_green_shared_head_new_target_blocks_and_close_recovers_one_source_row(self):
        api = self.api_fixture()
        context = {"GITHUB_RUN_ID": "201", "GITHUB_RUN_ATTEMPT": "1"}
        self.p.reconcile(api, 7, HERE.parents[1], context)
        identifier = api.checks[0]["id"]
        other = copy.deepcopy(self.pr)
        other.update(number=8)
        other["base"].update(ref="stacked", sha="f" * 40)
        api.extra_prs = [other]
        self.p.reconcile(api, 8, HERE.parents[1], context)
        self.assertEqual(len(api.checks), 1)
        self.assertEqual(api.checks[0]["id"], identifier)
        self.assertEqual(api.checks[0]["status"], "in_progress")
        metadata = json.loads(api.checks[0]["output"]["summary"])
        self.assertEqual(metadata["reason_code"], "source_head_intersection")
        self.assertEqual(metadata["result"], "pending")
        self.assertEqual(metadata["pending_prs"], [8])
        self.assertEqual([(target["pr"], target["result"]) for target in metadata["targets"]], [(7, "success"), (8, "pending")])
        other["base"]["ref"] = "retargeted"
        self.p.reconcile(api, 8, HERE.parents[1], context)
        self.assertEqual(api.checks[0]["status"], "in_progress")
        other["state"] = "closed"
        self.p.reconcile(api, 8, HERE.parents[1], context)
        self.assertEqual(api.checks[0]["status"], "completed")
        self.assertEqual(api.checks[0]["conclusion"], "success")
        self.assertEqual(api.checks[0]["head_sha"], self.head)
        self.assertEqual(len(api.checks), 1)

    def test_new_target_during_publish_withdraws_previous_green_and_recomputes(self):
        api = self.api_fixture()
        context = {"GITHUB_RUN_ID": "201", "GITHUB_RUN_ATTEMPT": "1", "MERGE_SOURCE_SHA": self.head}
        self.p.reconcile(api, 7, HERE.parents[1], context)
        original_pages = api.pages
        fired = []
        def pages(endpoint, key):
            if endpoint.startswith("commits/") and not fired:
                other = copy.deepcopy(self.pr)
                other.update(number=8)
                other["base"]["ref"] = "stacked"
                api.extra_prs.append(other)
                fired.append(True)
            return original_pages(endpoint, key)
        api.pages = pages
        self.p.reconcile(api, 7, HERE.parents[1], context)
        self.assertEqual(len(api.checks), 1)
        self.assertEqual(api.checks[0]["status"], "in_progress")
        metadata = json.loads(api.checks[0]["output"]["summary"])
        self.assertEqual(metadata["reason_code"], "source_head_intersection")
        self.assertEqual(metadata["result"], "pending")
        self.assertEqual(metadata["pending_prs"], [8])

    def test_old_source_concurrency_group_does_not_write_current_new_head(self):
        api = self.api_fixture()
        context = {"GITHUB_RUN_ID": "201", "GITHUB_RUN_ATTEMPT": "1", "MERGE_SOURCE_SHA": self.head}
        self.p.reconcile(api, 7, HERE.parents[1], context)
        api.pr["head"]["sha"] = "f" * 40
        self.p.reconcile(api, 7, HERE.parents[1], context)
        self.assertEqual(len(api.checks), 1)
        self.assertEqual(api.checks[0]["head_sha"], self.head)
        self.assertEqual(api.checks[0]["status"], "in_progress")

    def test_pending_is_not_a_terminal_failure_and_ignores_mutable_repo_metadata(self):
        api = CheckAPI()
        self.p.publish_check(api, self.pr, "pending", {"reason": "等待原生验证"})
        self.assertEqual(api.checks[0]["status"], "in_progress")
        self.assertIsNone(api.checks[0].get("conclusion"))
        first = (copy.deepcopy(self.pr), self.base, self.merge)
        second = copy.deepcopy(first)
        second[0]["head"]["repo"]["updated_at"] = "later"
        second[0]["mergeable_state"] = "blocked"
        self.assertEqual(self.p.fingerprint(first), self.p.fingerprint(second))

    def test_policy_change_uses_a_trusted_base_dispatch_without_byte_equality_deadlock(self):
        api = self.api_fixture()
        api.policy_changed = True
        with self.assertRaises(self.p.PolicyChange):
            self.p.proof(api, (self.pr, self.base, self.merge), 101, 2, HERE.parents[1])
        self.receipt["controller_sha"] = self.base
        self.run.update(event="workflow_dispatch", head_sha=self.base, head_branch="master",
                        head_repository=self.pr["base"]["repo"], display_title=self.p.dispatch_title(self.pr))
        api = self.api_fixture()
        api.policy_changed = True
        verified = self.p.proof(api, (self.pr, self.base, self.merge), 101, 2, HERE.parents[1])
        self.assertEqual(verified["identity"]["sha"], self.sha)
        self.run["head_sha"] = self.head
        with self.assertRaises(ValueError):
            self.p.proof(api, (self.pr, self.base, self.merge), 101, 2, HERE.parents[1])

    def test_stale_receipt_refreshes_only_explicit_consume_and_never_merges(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            subprocess.run(["git", "init", "-q", str(root)], check=True)
            for relative in self.p.gate.POLICY_PATHS:
                target = root / relative
                target.parent.mkdir(parents=True, exist_ok=True)
                target.write_bytes((HERE.parents[1] / relative).read_bytes())
            api = self.api_fixture(root)
            api.master = "f" * 40
            api.pr["base"]["sha"] = api.master
            api.pr["merge_commit_sha"] = "e" * 40
            api.current_merge = {**self.merge, "sha": "e" * 40,
                                 "parents": [{"sha": api.master}, {"sha": self.head}]}
            command = ["gh", "pr", "merge", "7", "--repo", REPO, "--merge", "--match-head-commit", self.head]
            for consume in (False, True):
                with self.subTest(consume=consume), self.assertRaises(ValueError):
                    self.p.premerge(api, root, 7, self.head, 101, 2, consume, command if consume else [])
                self.assertEqual(any(path == "issues/7/labels" for path in api.writes), consume)
                self.assertFalse((root / ".local/merge.lock").exists())

    def test_consumption_stays_locked_and_reconciles_nonzero_merge_return(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            subprocess.run(["git", "init", "-q", str(root)], check=True)
            api = self.api_fixture()
            context = {"GITHUB_RUN_ID": "201", "GITHUB_RUN_ATTEMPT": "1"}
            self.p.reconcile(api, 7, HERE.parents[1], context)
            evidence = root / "actual-review.txt"
            evidence.write_text("Independent reviewer artifact fixture\n")
            review = root / "review.json"
            review.write_text(json.dumps({"result": "approved", "independent": True, "reviewer": "independent-fixture",
                                          "head": self.head, "evidence_path": str(evidence),
                                          "evidence_sha256": hashlib.sha256(evidence.read_bytes()).hexdigest()}))
            command = ["gh", "pr", "merge", "7", "--repo", REPO, "--merge", "--match-head-commit", self.head]
            outcome = root / "outcome.json"
            calls = []
            def operation(argv, cwd):
                self.assertTrue((root / ".local/merge.lock").is_dir())
                calls.append(argv)
                api.pr.update(state="closed", merged=True, merge_commit_sha="f" * 40)
                api.current_merge = {**self.merge, "sha": "f" * 40}
                api.master = "f" * 40
                return 1
            self.p.premerge(api, root, 7, self.head, 101, 2, True, command, review, outcome, operation)
            self.assertEqual(len(calls), 1)
            self.assertEqual(json.loads(outcome.read_text())["result"], "merged")
            self.assertEqual(json.loads(outcome.read_text())["command_returncode"], 1)
            self.assertFalse((root / ".local/merge.lock").exists())

    def test_unknown_submission_keeps_mkdir_lock_across_process_and_readonly_restart_reconciliation(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            subprocess.run(["git", "init", "-q", str(root)], check=True)
            api = self.api_fixture()
            evidence, review, outcome = (root / name for name in ("review.txt", "review.json", "incident.json"))
            evidence.write_text("Actual independent review fixture\n")
            review.write_text(json.dumps({"result": "approved", "independent": True, "reviewer": "independent-fixture", "head": self.head,
                                          "evidence_path": str(evidence), "evidence_sha256": hashlib.sha256(evidence.read_bytes()).hexdigest()}))
            validation = {"identity": {key: self.receipt[key] for key in self.p.gate.IDENTITY}, "source": self.receipt["source"]}
            command = ["gh", "pr", "merge", "7", "--repo", REPO, "--merge", "--match-head-commit", self.head]
            with self.assertRaises(self.p.UnknownMerge):
                with self.p.merge_lock(root):
                    self.p.consume_operation(api, root, 7, self.head, validation, command, review, outcome, execute=lambda argv, cwd: 1)
            lock = root / ".local/merge.lock"
            self.assertTrue(lock.is_dir())
            self.assertEqual(json.loads(outcome.read_text())["result"], "unknown")
            old_client = subprocess.run(["mkdir", str(lock)], capture_output=True)
            self.assertNotEqual(old_client.returncode, 0)
            script = ('import importlib.util,sys; from pathlib import Path; '
                      's=importlib.util.spec_from_file_location("publisher",sys.argv[1]); m=importlib.util.module_from_spec(s); '
                      's.loader.exec_module(m); c=m.merge_lock(Path(sys.argv[2])); c.__enter__()')
            restarted = subprocess.run(["python3", "-B", "-c", script, str(HERE / "merge-gate-publish.py"), str(root)], capture_output=True)
            self.assertNotEqual(restarted.returncode, 0)
            fresh = load()
            with self.assertRaises(ValueError):
                fresh.reconcile_incident(api, root, outcome)
            self.assertTrue(lock.is_dir())
            api.pr.update(state="closed", merged=False)
            fresh.reconcile_incident(api, root, outcome)
            self.assertFalse(lock.exists())
            fresh.reconcile_incident(api, root, outcome)
            self.assertEqual(json.loads(outcome.read_text())["result"], "not_merged")
            self.assertEqual(api.writes, [])
            lock.mkdir()
            with self.assertRaises((ValueError, OSError)):
                fresh.reconcile_incident(api, root, outcome)
            self.assertTrue(lock.is_dir())

    def test_killed_before_send_recovers_only_dead_kernel_owner_and_unsubmitted_incident(self):
        script = r'''
import importlib.util,json,signal,sys
from pathlib import Path
spec=importlib.util.spec_from_file_location("publisher",sys.argv[1]); p=importlib.util.module_from_spec(spec); spec.loader.exec_module(p)
root=Path(sys.argv[2]); outcome=root/"incident.json"
with p.merge_lock(root) as lock:
    if sys.argv[3] != "idle":
        owner=p.gate.read_json(lock/"merge-gate-owner")
        record={"schema":1,"result":"unknown","submitted":False,"identity":json.loads(sys.argv[4]),
                "source":json.loads(sys.argv[5]),"lock":{"path":str(lock),**{k:owner[k] for k in ("token","inode","lease_inode") if k in owner}}}
        p.durable_json(outcome,record,exclusive=True)
        owner["incident"]=str(outcome.resolve()); p.durable_json(lock/"merge-gate-owner",owner)
    print("ready",flush=True)
    signal.pause()
'''
        identity = {key: self.receipt[key] for key in self.p.gate.IDENTITY}
        for phase in ("idle", "prepared-auto", "prepared-reconcile"):
            with self.subTest(phase=phase), tempfile.TemporaryDirectory() as temporary:
                root = Path(temporary)
                subprocess.run(["git", "init", "-q", str(root)], check=True)
                child = subprocess.Popen([sys.executable, "-B", "-c", script, str(HERE / "merge-gate-publish.py"), str(root),
                                          phase, json.dumps(identity), json.dumps(self.receipt["source"])],
                                         stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
                try:
                    self.assertEqual(child.stdout.readline().strip(), "ready")
                    lock = self.p.shared_lock_path(root)
                    original = (lock / "merge-gate-owner").read_bytes()
                    with self.assertRaises((ValueError, OSError)):
                        with self.p.merge_lock(root):
                            self.fail("live read-only/prepared owner was stolen")
                    self.assertEqual((lock / "merge-gate-owner").read_bytes(), original)
                    child.kill()
                    child.wait(timeout=8)
                    if phase == "prepared-reconcile":
                        class NoRemote:
                            def get(self, path):
                                raise AssertionError("provably unsent recovery must not contact GitHub")
                        self.p.reconcile_incident(NoRemote(), root, root / "incident.json")
                    else:
                        with self.p.merge_lock(root):
                            self.assertTrue(lock.is_dir())
                    self.assertFalse(lock.exists())
                    if phase != "idle":
                        record = self.p.gate.read_json(root / "incident.json")
                        self.assertIs(record["submitted"], False)
                        self.assertEqual(record["result"], "not_submitted")
                finally:
                    if child.poll() is None:
                        child.kill()
                        child.wait(timeout=8)
                    child.stdout.close()
                    child.stderr.close()

    def test_legacy_or_altered_lock_is_never_reclaimed_as_a_dead_reader(self):
        for kind in ("empty", "legacy-owner", "foreign-inode", "extra-file"):
            with self.subTest(kind=kind), tempfile.TemporaryDirectory() as temporary:
                root = Path(temporary)
                subprocess.run(["git", "init", "-q", str(root)], check=True)
                if kind in ("empty", "legacy-owner"):
                    lock = self.p.shared_lock_path(root)
                    lock.parent.mkdir()
                    lock.mkdir()
                    if kind == "legacy-owner":
                        self.p.durable_json(lock / "merge-gate-owner", {"schema": 1, "token": "a" * 48,
                                             "inode": lock.stat().st_ino, "incident": None}, exclusive=True)
                else:
                    with self.assertRaises(self.p.UnknownMerge):
                        with self.p.merge_lock(root) as lock:
                            raise self.p.UnknownMerge("fixture interrupts without normal cleanup")
                    if kind == "foreign-inode":
                        owner = self.p.gate.read_json(lock / "merge-gate-owner")
                        owner["inode"] += 1
                        self.p.durable_json(lock / "merge-gate-owner", owner)
                    else:
                        (lock / "other-worker-data").write_text("must not delete\n")
                before = {path.name: path.read_bytes() for path in lock.iterdir()}
                with self.assertRaises((ValueError, OSError)):
                    with self.p.merge_lock(root):
                        self.fail("legacy/altered lock was reclaimed")
                self.assertEqual({path.name: path.read_bytes() for path in lock.iterdir()}, before)

    def test_premerge_refuses_old_green_after_new_failure_or_unidentified_all_skipped(self):
        for status, conclusion, jobs in (("completed", "failure", "failure"), ("completed", "success", "skipped"), ("queued", None, "skipped")):
            with self.subTest(status=status, conclusion=conclusion, jobs=jobs), tempfile.TemporaryDirectory() as temporary:
                root = Path(temporary)
                subprocess.run(["git", "init", "-q", str(root)], check=True)
                api = self.api_fixture()
                self.p.reconcile(api, 7, HERE.parents[1], {"GITHUB_RUN_ID": "201", "GITHUB_RUN_ATTEMPT": "1"})
                newer = {**self.run, "id": 102, "status": status, "conclusion": conclusion, "run_attempt": 1, "job_conclusion": jobs}
                api.runs.append(newer)
                evidence, review, outcome = (root / name for name in ("review.txt", "review.json", "incident.json"))
                evidence.write_text("Actual independent review fixture\n")
                review.write_text(json.dumps({"result": "approved", "independent": True, "reviewer": "independent-fixture", "head": self.head,
                                              "evidence_path": str(evidence), "evidence_sha256": hashlib.sha256(evidence.read_bytes()).hexdigest()}))
                command = ["gh", "pr", "merge", "7", "--repo", REPO, "--merge", "--match-head-commit", self.head]
                sent = []
                with self.assertRaises(ValueError):
                    self.p.premerge(api, root, 7, self.head, 101, 2, True, command, review, outcome,
                                    execute=lambda argv, cwd: sent.append(argv) or 1)
                self.assertEqual(sent, [], "旧绿色不得盖过较新失败/未知/全跳过权威运行")
                self.assertFalse((root / ".local/merge.lock").exists())

    def test_only_explicit_irrelevant_metadata_marker_is_ignored_even_when_pending(self):
        for status in ("queued", "completed"):
            with self.subTest(status=status):
                api = self.api_fixture()
                newer = {**self.run, "id": 102, "status": status, "conclusion": "success" if status == "completed" else None,
                         "run_attempt": 1, "job_conclusion": "skipped",
                         "display_title": f"Merge metadata ignored PR 7 head {self.head} action edited"}
                api.runs.append(newer)
                self.assertEqual(self.p.latest_run(api, self.pr)["id"], 101)
                newer["display_title"] = f"Merge metadata ignored PR 7 head {self.head} action synchronize"
                self.assertEqual(self.p.latest_run(api, self.pr)["id"], 102)

    def test_shared_head_green_is_intersection_of_distinct_native_receipts_and_latest_target_authorities(self):
        api = self.api_fixture()
        context = {"GITHUB_RUN_ID": "201", "GITHUB_RUN_ATTEMPT": "1"}
        other = copy.deepcopy(self.pr)
        other.update(number=8, merge_commit_sha="2" * 40)
        other["base"].update(ref="stacked", sha="f" * 40)
        api.extra_prs.append(other)
        merge = {"sha": "2" * 40, "tree": {"sha": "1" * 40}, "parents": [{"sha": "f" * 40}, {"sha": self.head}]}
        api.add_validation(other, merge, 102)
        self.p.reconcile(api, 8, HERE.parents[1], context)
        self.assertEqual(len(api.checks), 1)
        self.assertEqual(api.checks[0]["conclusion"], "success")
        summary = json.loads(api.checks[0]["output"]["summary"])
        self.assertEqual([(target["pr"], target["result"]) for target in summary["targets"]], [(7, "success"), (8, "success")])
        self.assertEqual([target["validation"]["identity"]["run_id"] for target in summary["targets"]], ["101", "102"])
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            subprocess.run(["git", "init", "-q", str(root)], check=True)
            self.p.premerge(api, root, 7, self.head, 101, 2, False, [])
        api.add_validation(other, merge, 103)
        newer = api.validations[103]["run"]
        newer["conclusion"] = "failure"
        self.p.reconcile(api, 7, HERE.parents[1], context)
        summary = json.loads(api.checks[0]["output"]["summary"])
        self.assertEqual(api.checks[0]["conclusion"], "failure")
        self.assertEqual(summary["failed_prs"], [8])
        self.assertEqual(summary["targets"][0]["result"], "success", "不能把兄弟目标的失败误归因给 PR 7")
        api.runs.remove(newer)
        other["base"]["sha"] = "3" * 40
        self.p.reconcile(api, 8, HERE.parents[1], context)
        self.assertEqual(api.checks[0]["status"], "in_progress")

    def test_no_options_and_unauthorized_command_fail_without_external_calls(self):
        result = subprocess.run(["python3", str(HERE / "merge-gate-publish.py"), "premerge"], capture_output=True)
        self.assertNotEqual(result.returncode, 0)
        expected = ["gh", "pr", "merge", "7", "--repo", REPO, "--merge", "--match-head-commit", self.head]
        self.p.validate_command(expected, 7, self.head)
        for command in ([], ["sh", "-c", "true"], expected + ["--admin"]):
            with self.assertRaises(ValueError):
                self.p.validate_command(command, 7, self.head)


class CheckAPI:
    def __init__(self):
        self.checks = []
        self.writes = []

    def pages(self, endpoint, key):
        assert endpoint.startswith("commits/") and key == "check_runs"
        target = endpoint.split("/", 2)[1]
        return copy.deepcopy([row for row in self.checks if row["head_sha"] == target])

    def write(self, method, endpoint, data):
        self.writes.append(endpoint)
        if method == "POST":
            row = {**data, "id": max([9, *[r["id"] for r in self.checks]]) + 1, "app": {"id": 15368, "slug": "github-actions"}}
            self.checks.append(row)
        else:
            row = next(row for row in self.checks if row["id"] == int(endpoint.rsplit("/", 1)[-1]))
            row.update(data)
        return copy.deepcopy(row)


class ProvenanceAPI(CheckAPI):
    def __init__(self, test, root):
        super().__init__()
        self.test, self.root = test, root
        self.pr, self.master = copy.deepcopy(test.pr), test.base
        self.extra_prs = []
        self.current_merge = copy.deepcopy(test.merge)
        self.runs = [test.run]
        self.policy_changed = False
        self.validations, self.commits = {}, {}
        self.dispatch_unknown, self.dispatches = False, 0
        output = io.BytesIO()
        with zipfile.ZipFile(output, "w") as archive:
            archive.writestr("merge-gate.json", json.dumps(test.receipt))
        self.data = output.getvalue()
        self.artifact = {"id": 42, "name": "merge-gate-101-2", "expired": False,
                         "size_in_bytes": len(self.data), "digest": "sha256:" + hashlib.sha256(self.data).hexdigest(),
                         "workflow_run": {"id": 101}}
        output = io.BytesIO()
        with zipfile.ZipFile(output, "w") as archive:
            archive.writestr("merge-plan.json", json.dumps(test.receipt))
        self.plan_data = output.getvalue()
        self.identity_artifact, self.identity_data = None, None
        self.plan_artifact = {"id": 43, "name": "merge-plan-101-2", "expired": False, "workflow_run": {"id": 101},
                              "size_in_bytes": len(self.plan_data), "digest": "sha256:" + hashlib.sha256(self.plan_data).hexdigest()}

    def add_validation(self, pr, merge, run_id):
        run = {**copy.deepcopy(self.test.run), "id": run_id, "display_title": f"Merge validation PR {pr['number']}"}
        identity = {"repo": REPO, "pr": pr["number"], "head": pr["head"]["sha"], "base": pr["base"]["sha"],
                    "sha": merge["sha"], "tree": merge["tree"]["sha"], "run_id": str(run_id), "run_attempt": "2"}
        receipt = copy.deepcopy(self.test.receipt)
        receipt.update(identity, source=self.test.p.source_identity(pr))
        for evidence in receipt["evidence"]:
            evidence.update(identity, source=receipt["source"])
        output = io.BytesIO()
        with zipfile.ZipFile(output, "w") as archive:
            archive.writestr("merge-gate.json", json.dumps(receipt))
        data = output.getvalue()
        artifact = {"id": run_id + 1000, "name": f"merge-gate-{run_id}-2", "expired": False, "size_in_bytes": len(data),
                    "digest": "sha256:" + hashlib.sha256(data).hexdigest(), "workflow_run": {"id": run_id}}
        jobs = [{**job, "run_id": run_id} for job in self.test.jobs]
        output = io.BytesIO()
        with zipfile.ZipFile(output, "w") as archive:
            archive.writestr("merge-plan.json", json.dumps(receipt))
        plan_data = output.getvalue()
        plan_artifact = {"id": run_id + 2000, "name": f"merge-plan-{run_id}-2", "expired": False,
                         "size_in_bytes": len(plan_data), "digest": "sha256:" + hashlib.sha256(plan_data).hexdigest(),
                         "workflow_run": {"id": run_id}}
        self.validations[run_id] = {"run": run, "artifact": artifact, "data": data, "jobs": jobs,
                                    "plan_artifact": plan_artifact, "plan_data": plan_data}
        self.commits[merge["sha"]] = merge
        self.runs.append(run)

    def get(self, endpoint):
        if endpoint.startswith("pulls/") and endpoint.split("/")[-1].isdigit():
            return copy.deepcopy(next(pr for pr in [self.pr, *self.extra_prs] if pr["number"] == int(endpoint.split("/")[-1])))
        if endpoint == "git/ref/heads/master":
            return {"object": {"sha": self.master}}
        if endpoint.startswith("git/ref/heads/"):
            from urllib.parse import unquote
            ref = unquote(endpoint.removeprefix("git/ref/heads/"))
            return {"object": {"sha": next(pr["base"]["sha"] for pr in self.extra_prs if pr["base"]["ref"] == ref)}}
        if endpoint.startswith("git/commits/"):
            commit = endpoint.rsplit("/", 1)[-1]
            return copy.deepcopy(self.commits.get(commit, self.test.merge if endpoint.endswith(self.test.sha) else self.current_merge))
        if endpoint in ("actions/runs/101", "actions/runs/101/attempts/2"):
            return copy.deepcopy(self.test.run)
        if endpoint.startswith("actions/runs/") and int(endpoint.split("/")[2]) in self.validations:
            return copy.deepcopy(self.validations[int(endpoint.split("/")[2])]["run"])
        if endpoint == "actions/workflows/merge-gate.yml":
            return {"id": 99, "path": ".github/workflows/merge-gate.yml", "state": "active"}
        if endpoint == "actions/workflows/merge-conflict.yml":
            return {"id": 100, "path": ".github/workflows/merge-conflict.yml", "state": "active"}
        if endpoint in ("actions/runs/201", "actions/runs/201/attempts/1"):
            return {"id": 201, "run_attempt": 1, "path": ".github/workflows/merge-conflict.yml", "workflow_id": 100,
                    "repository": {"full_name": REPO}, "head_sha": self.test.base,
                    "event": "workflow_run", "status": "completed", "conclusion": "success"}
        if endpoint.startswith("compare/"):
            return {"status": "ahead", "merge_base_commit": {"sha": endpoint.split("/", 1)[1].split("...")[0]}}
        if endpoint.startswith("contents/"):
            import base64
            data = (self.root / endpoint.removeprefix("contents/").split("?", 1)[0]).read_bytes()
            blob = hashlib.sha1(b"blob " + str(len(data)).encode() + b"\0" + data).hexdigest()
            if self.policy_changed and endpoint.endswith(self.test.sha):
                blob = "0" * 40
            return {"type": "file", "sha": blob, "size": len(data), "encoding": "base64", "content": base64.b64encode(data).decode()}
        raise AssertionError("unexpected read: " + endpoint)

    def pages(self, endpoint, key):
        if endpoint.startswith("actions/workflows/"):
            event = "workflow_dispatch" if "event=workflow_dispatch" in endpoint else "pull_request"
            return copy.deepcopy([run for run in self.runs if run["event"] == event])
        if endpoint.startswith("pulls?"):
            rows = [self.pr, *self.extra_prs]
            if "base=" in endpoint:
                from urllib.parse import parse_qs
                base_ref = parse_qs(endpoint.split("?", 1)[1])["base"][0]
                rows = [pr for pr in rows if pr["base"]["ref"] == base_ref]
            return copy.deepcopy([pr for pr in rows if pr["state"] == "open"])
        if endpoint == "issues/7/labels":
            return []
        if endpoint == "actions/runs/101/artifacts":
            return [self.artifact, *([self.plan_artifact] if self.plan_artifact is not None else []),
                    *([self.identity_artifact] if self.identity_artifact is not None else [])]
        if endpoint == "actions/runs/101/attempts/2/jobs":
            return copy.deepcopy(self.test.jobs)
        if endpoint.startswith("actions/runs/"):
            run_id = int(endpoint.split("/")[2])
            if run_id in self.validations:
                value = self.validations[run_id]
                return copy.deepcopy([value["artifact"], value["plan_artifact"]] if key == "artifacts" else value["jobs"])
            if key == "jobs":
                run = next(run for run in self.runs if run["id"] == run_id)
                return [{"name": "Merge plan", "status": "completed", "conclusion": run.get("job_conclusion", "success")}]
        return super().pages(endpoint, key)

    def archive(self, artifact_id):
        if artifact_id == 44:
            return self.identity_data
        if artifact_id in (42, 43):
            return self.data if artifact_id == 42 else self.plan_data
        for value in self.validations.values():
            if value["artifact"]["id"] == artifact_id:
                return value["data"]
            if value["plan_artifact"]["id"] == artifact_id:
                return value["plan_data"]
        raise AssertionError("unexpected archive identity")

    def write(self, method, endpoint, data):
        if endpoint == "actions/workflows/merge-gate.yml/dispatches":
            self.dispatches += 1
            if self.dispatch_unknown:
                raise subprocess.CalledProcessError(1, ["gh", "api"], stderr=b"connection lost")
            return None
        if endpoint == "issues/7/labels":
            self.writes.append(endpoint)
            assert data == {"labels": ["ci:refresh"]}
            return []
        return super().write(method, endpoint, data)


if __name__ == "__main__":
    unittest.main()
