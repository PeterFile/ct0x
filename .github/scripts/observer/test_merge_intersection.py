#!/usr/bin/env python3
"""Offline ingest regressions for the real source-head intersection protocol.

The bounded fixture follows publisher blob 090ccfd08c15f7873d472babf5c59ff97aa03ff7
and producer-test blob 8fe0d6e6ffdabb037537ab9e7d87e4e364cff79e. Ordinary
behavior always runs without another checkout. When the publisher is in this
checkout, or OBSERVER_MERGE_PRODUCER names its exact file, the same assertions
also ingest its real reconcile output through its in-memory GitHub transport.
"""
import copy
from contextlib import closing, redirect_stdout
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch
from urllib.parse import parse_qs, unquote, urlsplit
import zipfile

sys.dont_write_bytecode = True
sys.path.insert(0, str(Path(__file__).resolve().parent))
from github import API, Reply
from observer import Observer
from merge_reports import reconcile_publisher
from store import Store

REPO = "PeterFile/ct0x"
ROOT = "/repos/" + REPO
PUBLISHER = ".github/workflows/merge-conflict.yml"
NATIVE = ".github/workflows/merge-gate.yml"
HEAD, BASE, MERGE, TREE, STACK, OTHER = (c * 40 for c in "abcdfe")
COORDINATOR = "intersection-coordinator"


def source(pr):
    return dict(repo_id=pr["head"]["repo"]["id"], repo=pr["head"]["repo"]["full_name"],
                ref=pr["head"]["ref"], base_ref=pr["base"]["ref"])


def details(pr):
    return dict(pr=pr["number"], head=pr["head"]["sha"], head_ref=pr["head"]["ref"],
                head_repo_id=pr["head"]["repo"]["id"], head_repo=pr["head"]["repo"]["full_name"],
                base_ref=pr["base"]["ref"], base=pr["base"]["sha"], draft=pr["draft"])


def archive(receipt):
    output = io.BytesIO()
    with zipfile.ZipFile(output, "w") as target:
        target.writestr(zipfile.ZipInfo("merge-gate.json"), json.dumps(receipt))
    return output.getvalue()


def producer_file():
    explicit = os.environ.get("OBSERVER_MERGE_PRODUCER")
    if explicit is not None and not explicit.strip():
        raise AssertionError("OBSERVER_MERGE_PRODUCER must name an exact publisher file")
    path = Path(explicit).resolve() if explicit else Path(__file__).resolve().parents[3] / ".github/scripts/merge-gate-publish.py"
    if explicit and not path.is_file():
        raise AssertionError("OBSERVER_MERGE_PRODUCER must name an existing publisher file")
    return path if path.is_file() else None


class World:
    def __init__(self, variant="single-success", producer=None):
        self.repo = dict(id=1439910892, full_name=REPO, default_branch="master")
        self.prs, self.native, self.jobs, self.artifacts, self.blobs, self.commits = {}, {}, {}, {}, {}, {}
        self.calls, self.overrides = [], {}
        self.publisher = dict(id=201, run_attempt=1, path=PUBLISHER, workflow_id=100,
            repository=self.repo, head_repository=self.repo, head_sha=BASE, head_branch="master",
            event="workflow_run", pull_requests=[], status="completed", conclusion="success",
            run_started_at="2026-11-26T02:00:00Z", html_url=f"https://github.com/{REPO}/actions/runs/201")
        self.reporters = []
        self.add(7)
        if variant.startswith("shared"):
            self.add(8)
        conflict = 7 if variant == "single-conflict" else 8 if variant == "shared-conflict" else None
        failed = 7 if variant == "single-validation-failure" else 8 if variant == "shared-validation-failure" else None
        pending = 7 if variant == "single-waiting" else 8 if variant == "shared-waiting" else None
        if failed:
            self.native[94 + failed].update(conclusion="failure", display_title=f"Merge validation PR {failed}")
            self.jobs[94 + failed][1]["conclusion"] = "failure"
        for number in (conflict, pending):
            if number is not None:
                self.prs[number].update(mergeable=False if number == conflict else None,
                    mergeable_state="dirty" if number == conflict else "unknown", merge_commit_sha=None)
        targets = []
        for number, pr in self.prs.items():
            target = dict(details(pr), merge_sha=pr["merge_commit_sha"],
                tree=TREE if number == 7 else "1" * 40, verification=True,
                result="success", reason_code="exact_target_verified")
            if number in (conflict, pending):
                target.update(merge_sha=None, tree=None, verification=False,
                    result="failure" if number == conflict else "pending",
                    reason_code="confirmed_conflict" if number == conflict else "awaiting_validation")
            else:
                native = self.native[94 + number]
                target["authority"] = {key: native[key] for key in ("id", "run_attempt", "status", "conclusion", "event")}
                receipt = self.artifacts[native["id"]]["receipt"]
                artifact = self.artifacts[native["id"]]["row"]
                target["validation"] = dict(identity={key: receipt[key] for key in
                    ("repo", "pr", "head", "base", "sha", "tree", "run_id", "run_attempt")},
                    source=source(pr), artifact_id=artifact["id"], artifact_digest=artifact["digest"],
                    policy_digest=receipt["policy_digest"], controller_sha="")
                if number == failed:
                    target.update(result="failure", verification=False, reason_code="terminal_validation_unsuccessful")
                    target.pop("validation")
            targets.append(target)
        result = "failure" if conflict or failed else "pending" if pending else "success"
        self.metadata = dict(schema=1, repo=REPO, pr=7, head=HEAD, result=result,
            reason_code="source_head_intersection", membership=[details(pr) for pr in self.prs.values()],
            targets=targets, failed_prs=[conflict or failed] if conflict or failed else [], pending_prs=[pending] if pending else [],
            publisher=dict(workflow=PUBLISHER, workflow_sha=BASE, run_id="201", run_attempt="1"))
        if len(targets) == 1 and "validation" in targets[0]:
            self.metadata["validation"] = copy.deepcopy(targets[0]["validation"])
        self.check = dict(id=10, name="Merge gate", head_sha=HEAD,
            external_id=f"merge-gate:v2:{REPO}:source:{HEAD}", app=dict(id=15368, slug="github-actions"),
            html_url=f"https://github.com/{REPO}/runs/10?check_suite_focus=true",
            status="in_progress" if result == "pending" else "completed", conclusion=None if result == "pending" else result)
        if producer:
            self.from_producer(producer, variant)

    def add(self, number):
        base, merge, tree = (BASE, MERGE, TREE) if number == 7 else (STACK, "2" * 40, "1" * 40)
        pr = dict(number=number, state="open", draft=False, merged=False, merged_at=None,
            html_url=f"https://github.com/{REPO}/pull/{number}",
            head=dict(sha=HEAD, ref="feature/one", repo=dict(id=17, full_name="contributor/ct0x")),
            base=dict(sha=base, ref="master" if number == 7 else "stacked", repo=self.repo),
            mergeable=True, mergeable_state="clean", merge_commit_sha=merge)
        self.prs[number] = pr
        run_id = 94 + number
        native = dict(id=run_id, run_attempt=2, path=NATIVE, workflow_id=99,
            repository=self.repo, head_repository=pr["head"]["repo"], head_sha=HEAD, head_branch=pr["head"]["ref"],
            event="pull_request", pull_requests=[], display_title="",
            status="completed", conclusion="success", run_started_at="2026-11-26T01:00:00Z",
            html_url=f"https://github.com/{REPO}/actions/runs/{run_id}")
        self.native[run_id] = native
        self.jobs[run_id] = [self.job(run_id, name, 1000 + 10 * number + i) for i, name in enumerate(
            ("Merge plan", "Merge tests / identity-diff", "Merge evidence"))]
        identity = dict(repo=REPO, pr=number, head=HEAD, base=base, sha=merge, tree=tree, run_id=str(run_id), run_attempt="2")
        receipt = dict(identity, schema=1, result="success", source=source(pr), policy_digest="e" * 64,
            tasks=[dict(unit="identity-diff", kind="identity")], evidence=[dict(identity,
                source=source(pr), unit="identity-diff", result="success")])
        self.install_artifact(run_id, receipt, 42 if number == 7 else 1102)
        self.commits[merge] = dict(sha=merge, tree=dict(sha=tree), parents=[dict(sha=base), dict(sha=HEAD)])
        self.reporters.append(self.job(201, f"Trusted Merge report / PR {number}", 700 + number))

    def job(self, run_id, name, identity):
        run = self.publisher if run_id == 201 else self.native[run_id]
        hour = "02" if run_id == 201 else "01"
        return dict(id=identity, name=name, run_id=run_id, run_attempt=run["run_attempt"], head_sha=run["head_sha"],
            status="completed", conclusion="success", started_at=f"2026-11-26T{hour}:01:00Z",
            completed_at=f"2026-11-26T{hour}:02:00Z", html_url=f"https://github.com/{REPO}/actions/runs/{run_id}/job/{identity}")

    def install_artifact(self, run_id, receipt, identity, data=None):
        data = archive(receipt) if data is None else data
        row = dict(id=identity, name=f"merge-gate-{run_id}-2", expired=False, size_in_bytes=len(data),
            digest="sha256:" + hashlib.sha256(data).hexdigest(), workflow_run=dict(id=run_id, head_sha=HEAD))
        self.artifacts[run_id] = dict(row=row, receipt=receipt)
        self.blobs[identity] = data

    def from_producer(self, publisher, variant):
        fixture = publisher.with_name("test-merge-gate-publish.py")
        spec = importlib.util.spec_from_file_location("intersection_producer_fixture", fixture)
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        case = module.PublisherTests()
        case.setUp()
        case.pr["head"]["ref"] = "feature/one"
        case.run.update(head_branch="feature/one")
        case.receipt["source"]["ref"] = "feature/one"
        case.receipt["evidence"][0]["source"]["ref"] = "feature/one"
        api = case.api_fixture()
        if variant.startswith("shared"):
            other = copy.deepcopy(self.prs[8])
            api.extra_prs.append(other)
            api.add_validation(other, self.commits["2" * 40], 102)
        for number, pr in self.prs.items():
            live = api.pr if number == 7 else api.extra_prs[0]
            live.update({key: pr[key] for key in ("mergeable", "mergeable_state", "merge_commit_sha")})
            if self.native[94 + number]["conclusion"] != "success":
                native = case.run if number == 7 else api.validations[94 + number]["run"]
                native.update(conclusion="failure", display_title=f"Merge validation PR {number}")
        root = publisher.parents[2]
        with patch.object(case.p.subprocess, "run", side_effect=AssertionError("outbound process")), \
                patch.object(case.p.time, "sleep"), redirect_stdout(io.StringIO()):
            case.p.reconcile(api, 7, root, dict(GITHUB_RUN_ID="201", GITHUB_RUN_ATTEMPT="1",
                MERGE_TRUSTED_WORKFLOW_SHA=BASE, MERGE_SOURCE_SHA=HEAD))
        row, = api.checks
        self.check = dict(row, html_url=self.check["html_url"])
        self.metadata = json.loads(row["output"]["summary"])
        for run_id in self.native:
            artifact, = [row for row in api.pages(f"actions/runs/{run_id}/artifacts", "artifacts")
                         if row["name"] == f"merge-gate-{run_id}-2"]
            data = api.archive(artifact["id"])
            with zipfile.ZipFile(io.BytesIO(data)) as zipped:
                receipt = json.loads(zipped.read("merge-gate.json"))
            self.install_artifact(run_id, receipt, artifact["id"], data)

    def row(self):
        return dict(self.check, output=dict(summary=json.dumps(self.metadata)))

    def __call__(self, path, etag=None):
        self.calls.append(path)
        if path in self.overrides:
            value = self.overrides[path]
            return value() if callable(value) else value
        parsed = urlsplit(path)
        endpoint = unquote(parsed.path.removeprefix(ROOT + "/"))
        query = parse_qs(parsed.query)
        data = None
        if parsed.path == "/user":
            data = dict(id=1, login="offline-operator")
        elif parsed.path == ROOT:
            data = self.repo
        elif endpoint == "git/matching-refs/tags/rc/":
            data = []
        elif endpoint == "actions/workflows/100/runs":
            data = dict(total_count=1, workflow_runs=[self.publisher])
        elif endpoint == "actions/workflows/99/runs":
            data = dict(total_count=0, workflow_runs=[])
        elif endpoint == "actions/workflows/merge-conflict.yml":
            data = dict(id=100, path=PUBLISHER, state="active")
        elif endpoint == "actions/workflows/merge-gate.yml":
            data = dict(id=99, path=NATIVE, state="active")
        elif endpoint.startswith("actions/workflows/merge-gate.yml/runs"):
            rows = [run for run in self.native.values() if run["event"] == query["event"][0]]
            data = dict(total_count=len(rows), workflow_runs=rows)
        elif endpoint.startswith("actions/runs/"):
            parts = endpoint.split("/")
            identity = int(parts[2])
            if parts[-1] == "jobs":
                rows = self.reporters if identity == 201 else self.jobs[identity]
                data = dict(total_count=len(rows), jobs=rows)
            elif parts[-1] == "artifacts":
                data = dict(total_count=1, artifacts=[self.artifacts[identity]["row"]])
            else:
                data = self.publisher if identity == 201 else self.native[identity]
        elif endpoint == "pulls":
            data = [pr for pr in self.prs.values() if pr["state"] == "open"]
        elif endpoint.startswith("pulls/"):
            data = self.prs[int(endpoint.split("/")[1])]
        elif endpoint.startswith("commits/") and endpoint.endswith("/check-runs"):
            data = dict(total_count=1, check_runs=[self.row()])
        elif endpoint == "check-runs/10":
            data = self.row()
        elif endpoint.startswith("git/ref/heads/"):
            ref = endpoint.removeprefix("git/ref/heads/")
            base = BASE if ref == "master" else next(pr["base"]["sha"] for pr in self.prs.values() if pr["base"]["ref"] == ref)
            data = dict(ref="refs/heads/" + ref, object=dict(type="commit", sha=base))
        elif endpoint.startswith("git/commits/"):
            data = self.commits.get(endpoint.rsplit("/", 1)[1])
        elif endpoint.startswith("compare/"):
            old, new = endpoint.split("/", 1)[1].split("...")
            data = dict(status="identical" if old == new else "ahead", merge_base_commit=dict(sha=old))
        elif endpoint.startswith("actions/artifacts/"):
            return Reply(200, {}, self.blobs[int(endpoint.split("/")[2])])
        return Reply(200 if data is not None else 404, {}, json.dumps(data).encode())


class EarlyWorld(World):
    def __init__(self, event, shared=False):
        super().__init__("shared-conflict" if shared else "single-validation-failure")
        pr = self.prs[7]
        self.run_id = 101 if event == "pull_request" else 102
        native = self.native.pop(101)
        native.update(id=self.run_id, run_attempt=1, event=event, conclusion="failure",
            head_sha=HEAD if event == "pull_request" else BASE,
            head_branch=pr["head"]["ref"] if event == "pull_request" else "master",
            head_repository=pr["head"]["repo"] if event == "pull_request" else self.repo,
            pull_requests=[dict(number=7, head=dict(sha=HEAD), base=dict(sha=BASE))] if event == "pull_request" else [],
            display_title="Merge validation PR 7" if event == "pull_request" else f"Merge validation PR 7 head {HEAD} base {BASE}",
            html_url=f"https://github.com/{REPO}/actions/runs/{self.run_id}")
        self.native[self.run_id] = native
        self.jobs.pop(101)
        job = self.job(self.run_id, "Merge plan", 1000 + self.run_id)
        job.update(conclusion="failure", completed_at="2026-11-26T01:05:00Z", steps=[dict(number=4 + i, name=name,
            status="completed", conclusion="success" if i < 2 else "failure",
            started_at=f"2026-11-26T01:0{i + 1}:00Z", completed_at=f"2026-11-26T01:0{i + 2}:00Z") for i, name in enumerate((
                "Preserve true merge identity before fallible planning", "Run actions/upload-artifact@v7",
                "Verify true merge parents and select complete impacted suites"))])
        self.jobs[self.run_id] = [job]
        identity = dict(schema=1, kind="merge-identity", repo=REPO, pr=7, head=HEAD, base=BASE, sha=MERGE, tree=TREE,
            run_id=str(self.run_id), run_attempt="1", source=source(pr),
            controller_sha="" if event == "pull_request" else BASE, policy_digest="e" * 64)
        packet = io.BytesIO()
        with zipfile.ZipFile(packet, "w") as zipped:
            zipped.writestr("merge-identity.json", json.dumps(identity))
        data = packet.getvalue()
        self.artifacts[self.run_id] = dict(row=dict(id=838, name=f"merge-identity-{self.run_id}-1", expired=False,
            size_in_bytes=len(data), digest="sha256:" + hashlib.sha256(data).hexdigest(),
            workflow_run=dict(id=self.run_id, head_sha=native["head_sha"]),
            created_at="2026-11-26T01:02:45Z", updated_at="2026-11-26T01:02:46Z"))
        self.blobs[838] = data
        target = self.metadata["targets"][0]
        target.update(result="failure", verification=False, reason_code="terminal_validation_unsuccessful",
            authority={key: native[key] for key in ("id", "run_attempt", "status", "conclusion", "event")})
        target.pop("validation", None)
        self.metadata.pop("validation", None)
        self.metadata.update(result="failure", failed_prs=[7, 8] if shared else [7])
        self.check.update(status="completed", conclusion="failure")
        self.overrides.update({ROOT + path: Reply(200, {}, json.dumps(value).encode()) for path, value in {
            "/actions/workflows/99": dict(id=99, path=NATIVE),
            "/commits/master": dict(sha=BASE),
            "/commits/" + MERGE: dict(sha=MERGE, commit=dict(tree=dict(sha=TREE)), parents=[dict(sha=BASE), dict(sha=HEAD)]),
            "/commits/" + HEAD + "/pulls": [pr]}.items()})
        self.overrides[ROOT + f"/actions/jobs/{job['id']}/logs"] = Reply(200, {}, b"fixture/missingdep is not in std\n")


class IntersectionProtocol(unittest.TestCase):
    def worlds(self, variant):
        yield "frozen", World(variant)
        producer = producer_file()
        if producer:
            yield str(producer), World(variant, producer)

    def ingest(self, world, assertions, publisher_only=False):
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / "observer.db"
            with closing(Store(path)) as store:
                store.configure(REPO, COORDINATOR)
                for number in world.prs:
                    store.assign(REPO, number, f"intersection-author-{number}", f"local-assignment:{number}")
                def tick():
                    with Observer(store, API(store, world)) as observer:
                        if publisher_only:
                            return reconcile_publisher(observer, world.publisher, world.reporters)
                        return observer.reconcile(force=True)
                assertions(store, tick)

    def native_notice(self, store, world, deliver=True):
        native, jobs = world.native[world.run_id], world.jobs[world.run_id]
        store.observe(REPO, native["id"], native["run_attempt"], dict(run=native, jobs=jobs, jobs_fresh=True))
        with Observer(store, API(store, world)) as observer:
            observer.pump = lambda: None
            observer.process(native, jobs, deliver)
        first, = [row for row in store.actions() if row["kind"] == "first_failure"]
        return first

    def test_early_native_delivery_suppresses_only_the_same_authoritative_investigation(self):
        for event in ("pull_request", "workflow_dispatch"):
            for prior in (False, True, "acked"):
                world = EarlyWorld(event)
                with self.subTest(event=event, prior=prior):
                    def check(store, tick):
                        first = self.native_notice(store, world) if prior else None
                        original = copy.deepcopy(store.actions())
                        delivered = copy.deepcopy(store.inbox("intersection-author-7"))
                        if prior == "acked":
                            for row in delivered:
                                store.ack("intersection-author-7", row["receipt_id"])
                            delivered = copy.deepcopy(store.inbox("intersection-author-7"))
                        self.assertTrue(tick(), [dict(row) for row in store.db.execute("SELECT * FROM problems")])
                        result, = [row["payload"] for row in store.actions() if row["payload"]["workflow"] == PUBLISHER]
                        self.assertEqual((result["action"], result["investigation"]),
                            ("merge_gate_result", False) if prior else ("merge_gate_failure", True))
                        if prior:
                            self.assertEqual((result["native_failure_run_id"], result["native_failure_attempt"]), (world.run_id, 1))
                            self.assertEqual(result["native_failure_action_id"], first["id"])
                            self.assertEqual([row for row in store.actions() if row["payload"]["workflow"] == NATIVE], original)
                            self.assertTrue(all(row in store.inbox("intersection-author-7") for row in delivered))
                        else:
                            self.assertNotIn("native_failure_run_id", result)
                        before = store.inbox("intersection-author-7")
                        store.close()
                        with closing(Store(store.path)) as restored:
                            with Observer(restored, API(restored, world)) as observer:
                                self.assertTrue(reconcile_publisher(observer, world.publisher, world.reporters))
                            self.assertEqual(restored.inbox("intersection-author-7"), before)
                    self.ingest(world, check, publisher_only=True)

    def test_late_identity_uses_the_author_receipt_but_keeps_the_original_first_failure_id(self):
        world = EarlyWorld("workflow_dispatch")
        endpoint = ROOT + f"/actions/runs/{world.run_id}/artifacts?per_page=100&page=1"
        world.overrides[endpoint] = Reply(200, {}, b'{"total_count":0,"artifacts":[]}')
        def check(store, tick):
            first = self.native_notice(store, world)
            coordinator = store.inbox(COORDINATOR)
            self.assertIsNone(first["payload"]["pr"])
            self.assertEqual(store.inbox("intersection-author-7"), [])
            del world.overrides[endpoint]
            self.assertEqual(self.native_notice(store, world), first)
            author, = [row for row in store.inbox("intersection-author-7") if row["payload"]["action"] == "first_failure"]
            self.assertEqual((author["payload"]["pr"], author["payload"]["tested_sha"], author["payload"]["tested_tree"]), (7, MERGE, TREE))
            self.assertTrue(all(row in store.inbox(COORDINATOR) for row in coordinator))
            self.assertTrue(tick())
            result, = [row["payload"] for row in store.actions() if row["payload"]["workflow"] == PUBLISHER]
            self.assertEqual((result["action"], result["investigation"], result["native_failure_action_id"]), ("merge_gate_result", False, first["id"]))
        self.ingest(world, check, publisher_only=True)

    def test_uncommitted_or_mismatched_native_receipts_do_not_suppress(self):
        cases = ("pending", "missing-first-receipt", "coordinator-only", "previous-recipient", "previous-attempt", "previous-run",
            "pr", "source_head", "workflow_sha", "event", "work_origin", "tested_sha", "tested_tree", "job_id", "identity_evidence",
            "binding-pr", "binding-head", "binding-source", "binding-sha", "binding-tree", "binding-provenance", "binding-artifact", "binding-digest",
            "observed-event", "observed-source", "route", "missing-target-detail")
        for event in ("pull_request", "workflow_dispatch"):
            for case in cases:
                world = EarlyWorld(event)
                with self.subTest(event=event, case=case):
                    def check(store, tick):
                        first = self.native_notice(store, world, deliver=case != "pending")
                        author = "intersection-author-7"
                        if case in ("missing-first-receipt", "coordinator-only", "missing-target-detail"):
                            condition = "action_id=?" if case == "missing-first-receipt" else "1" if case == "coordinator-only" else "action_id!=?"
                            args = (first["id"],) if "?" in condition else ()
                            receipts = [row[0] for row in store.db.execute(f"SELECT receipt_id FROM outbox WHERE recipient=? AND {condition}", (author, *args))]
                            with store.db:
                                for receipt in receipts:
                                    store.db.execute("DELETE FROM inbox WHERE receipt_id=?", (receipt,))
                                    store.db.execute("UPDATE outbox SET state='pending',delivered_at=NULL WHERE receipt_id=?", (receipt,))
                        elif case == "previous-recipient":
                            with store.db:
                                store.db.execute("DELETE FROM assignments WHERE repo=? AND pr=7", (REPO,))
                            author = "replacement-author-7"
                            store.assign(REPO, 7, author, "new exact assignment")
                        elif case == "previous-attempt":
                            world.native[world.run_id]["run_attempt"] = 2
                            world.jobs[world.run_id][0]["run_attempt"] = 2
                            world.metadata["targets"][0]["authority"]["run_attempt"] = 2
                        elif case == "previous-run":
                            native = dict(world.native[world.run_id], id=103, html_url=f"https://github.com/{REPO}/actions/runs/103")
                            world.native[103] = native
                            world.jobs[103] = [dict(world.jobs[world.run_id][0], run_id=103,
                                html_url=native["html_url"] + f"/job/{1000 + world.run_id}")]
                            world.metadata["targets"][0]["authority"]["id"] = 103
                        elif case.startswith("binding-"):
                            binding = store.target(REPO, world.run_id, 1)
                            key, value = {"pr": ("pr", 8), "head": ("pr_head", OTHER), "source": ("source", {}),
                                "sha": ("sha", OTHER), "tree": ("tree", OTHER), "provenance": ("provenance", "source_only"),
                                "artifact": ("artifact_id", None), "digest": ("digest", None)}[case[8:]]
                            binding[key] = value
                            store.meta_put(f"target:{REPO}:{world.run_id}:1", binding)
                        elif case.startswith("observed-"):
                            native = copy.deepcopy(world.native[world.run_id])
                            native["event" if case == "observed-event" else "head_sha"] = "push" if case == "observed-event" else OTHER
                            with store.db:
                                store.db.execute("UPDATE observations SET body=? WHERE repo=? AND run_id=? AND attempt=1",
                                    (json.dumps(dict(run=native, jobs=world.jobs[world.run_id])), REPO, world.run_id))
                        elif case == "route":
                            route = store.meta_get(f"pr-route:{REPO}:{world.run_id}:1")
                            route["pr"] = 8
                            store.meta_put(f"pr-route:{REPO}:{world.run_id}:1", route)
                        elif case != "pending":
                            value = {"pr": 8, "source_head": OTHER, "workflow_sha": OTHER, "event": "push", "work_origin": "inherited",
                                "tested_sha": OTHER, "tested_tree": OTHER, "job_id": 999, "identity_evidence": {}}[case]
                            with store.db:
                                for row in store.db.execute("SELECT receipt_id,payload FROM inbox WHERE recipient=?", (author,)).fetchall():
                                    payload = json.loads(row["payload"])
                                    payload[case] = value
                                    store.db.execute("UPDATE inbox SET payload=? WHERE receipt_id=?", (json.dumps(payload), row["receipt_id"]))
                        self.assertTrue(tick(), [dict(row) for row in store.db.execute("SELECT * FROM problems")])
                        result, = [row["payload"] for row in store.inbox(author) if row["payload"]["workflow"] == PUBLISHER]
                        # A bound dispatch receipt needs no later target-detail receipt.
                        suppressed = event == "workflow_dispatch" and case == "missing-target-detail"
                        self.assertEqual((result["action"], result["investigation"]),
                            ("merge_gate_result", False) if suppressed else ("merge_gate_failure", True))
                        if not suppressed:
                            self.assertNotIn("native_failure_run_id", result)
                    self.ingest(world, check, publisher_only=True)

    def test_author_reassignment_during_check_validation_retries_without_borrowing_the_old_receipt(self):
        for event in ("pull_request", "workflow_dispatch"):
            world = EarlyWorld(event)
            with self.subTest(event=event):
                def check(store, tick):
                    self.native_notice(store, world)
                    before = store.inbox("intersection-author-7")
                    def replace_author():
                        with store.db:
                            store.db.execute("DELETE FROM assignments WHERE repo=? AND pr=7", (REPO,))
                        store.assign(REPO, 7, "replacement-author-7", "new exact assignment")
                        return Reply(200, {}, json.dumps(world.row()).encode())
                    endpoint = ROOT + "/check-runs/10"
                    world.overrides[endpoint] = replace_author
                    self.assertFalse(tick())
                    self.assertFalse(any(row["payload"]["workflow"] == PUBLISHER for row in store.actions()))
                    self.assertEqual(store.inbox("replacement-author-7"), [])
                    del world.overrides[endpoint]
                    self.assertTrue(tick())
                    result, = [row["payload"] for row in store.inbox("replacement-author-7")]
                    self.assertEqual((result["action"], result["investigation"]), ("merge_gate_failure", True))
                    self.assertNotIn("native_failure_run_id", result)
                    self.assertEqual(store.inbox("intersection-author-7"), before)
                self.ingest(world, check, publisher_only=True)

    def test_a_queued_publisher_fact_cannot_replace_a_delivered_native_investigation(self):
        world = EarlyWorld("workflow_dispatch")
        def check(store, tick):
            with Observer(store, API(store, world)) as observer:
                self.assertTrue(reconcile_publisher(observer, world.publisher, world.reporters, deliver=False))
            self.assertEqual(store.inbox("intersection-author-7"), [])
            world.publisher["run_attempt"] = 2
            world.reporters[0]["run_attempt"] = 2
            world.metadata["publisher"]["run_attempt"] = "2"
            self.assertTrue(tick())
            result, = [row["payload"] for row in store.inbox("intersection-author-7")]
            self.assertEqual((result["action"], result["investigation"]), ("merge_gate_failure", True))
            self.assertNotIn("native_failure_run_id", result)
        self.ingest(world, check, publisher_only=True)

    def test_early_native_receipt_does_not_suppress_a_shared_sibling(self):
        for event in ("pull_request", "workflow_dispatch"):
            world = EarlyWorld(event, shared=True)
            world.publisher["event"] = "pull_request_target"
            world.reporters = world.reporters[:1]
            with self.subTest(event=event):
                def check(store, tick):
                    first = self.native_notice(store, world)
                    self.assertTrue(tick())
                    results = {row["payload"]["pr"]: row["payload"] for row in store.actions() if row["payload"]["workflow"] == PUBLISHER}
                    self.assertEqual((results[7]["action"], results[7]["investigation"], results[7]["native_failure_action_id"]),
                        ("merge_gate_result", False, first["id"]))
                    self.assertEqual((results[8]["action"], results[8]["investigation"]), ("merge_gate_failure", True))
                    self.assertNotIn("native_failure_run_id", results[8])
                    self.assertEqual(len(store.inbox("intersection-author-8")), 1)
                self.ingest(world, check, publisher_only=True)

    def test_ordinary_success_and_actual_conflict_follow_normal_ingest(self):
        for variant in ("single-success", "single-conflict", "single-validation-failure"):
            for mode, world in self.worlds(variant):
                with self.subTest(variant=variant, producer=mode):
                    def check(store, tick):
                        self.assertTrue(tick(), [dict(row) for row in store.db.execute("SELECT * FROM problems")])
                        payload, = [row["payload"] for row in store.actions()]
                        expected = "success" if variant.endswith("success") else "failure"
                        self.assertEqual((payload["pr"], payload["conclusion"], payload["check_conclusion"]), (7, expected, expected))
                        self.assertEqual(payload["tested_sha"], MERGE if expected == "success" else None)
                        kind = None if expected == "success" else "validation_evidence" if "validation" in variant else "merge_conflict"
                        self.assertEqual(payload["failure_kind"], kind)
                        self.assertEqual(len(store.inbox("intersection-author-7")), 1)
                    self.ingest(world, check)

    def test_shared_success_and_mixed_verdicts_are_independent_and_retry_is_idempotent(self):
        for variant in ("shared-success", "shared-conflict", "shared-validation-failure"):
            for mode, world in self.worlds(variant):
                with self.subTest(variant=variant, producer=mode):
                    def check(store, tick):
                        self.assertTrue(tick(), [dict(row) for row in store.db.execute("SELECT * FROM problems")])
                        payloads = {row["payload"]["pr"]: row["payload"] for row in store.actions()}
                        self.assertEqual(set(payloads), {7, 8})
                        self.assertEqual(payloads[7]["conclusion"], "success")
                        self.assertEqual(payloads[7]["tested_sha"], MERGE)
                        self.assertFalse(payloads[7]["investigation"])
                        self.assertEqual(payloads[8]["conclusion"], "success" if variant.endswith("success") else "failure")
                        self.assertEqual(payloads[8]["tested_sha"], "2" * 40 if variant.endswith("success") else None)
                        if not variant.endswith("success"):
                            self.assertNotIn("最终必需检查已通过", payloads[7]["message"])
                        self.assertEqual({p["check_conclusion"] for p in payloads.values()}, {world.check["conclusion"]})
                        self.assertEqual({p["reporter_job_id"] for p in payloads.values()}, {707, 708})
                        receipts = {number: store.inbox(f"intersection-author-{number}")[0]["receipt_id"] for number in (7, 8)}
                        self.assertTrue(tick())
                        self.assertEqual(len(store.actions()), 2)
                        self.assertEqual(len(store.inbox(COORDINATOR)), 2)
                        self.assertEqual({number: store.inbox(f"intersection-author-{number}")[0]["receipt_id"] for number in (7, 8)}, receipts)
                        store.close()
                        with closing(Store(store.path)) as restored:
                            with Observer(restored, API(restored, world)) as observer:
                                self.assertTrue(observer.reconcile(force=True))
                            self.assertEqual(len(restored.actions()), 2)
                            self.assertEqual(len(restored.inbox(COORDINATOR)), 2)
                            self.assertEqual({number: restored.inbox(f"intersection-author-{number}")[0]["receipt_id"] for number in (7, 8)}, receipts)
                    self.ingest(world, check)

    def test_single_reporter_covers_the_authenticated_source_and_survives_reopen(self):
        for variant in ("shared-success", "shared-conflict", "shared-validation-failure"):
            for mode, world in self.worlds(variant):
                world.publisher.update(event="pull_request_target", pull_requests=[dict(number=7)])
                world.reporters = world.reporters[:1]
                with self.subTest(variant=variant, producer=mode):
                    def check(store, tick):
                        self.assertTrue(tick(), [dict(row) for row in store.db.execute("SELECT * FROM problems")])
                        payloads = {row["payload"]["pr"]: row["payload"] for row in store.actions()}
                        self.assertEqual(set(payloads), {7, 8})
                        self.assertEqual({payload["reporter_job_id"] for payload in payloads.values()}, {707})
                        self.assertEqual(payloads[7]["conclusion"], "success")
                        self.assertFalse(payloads[7]["investigation"])
                        self.assertEqual(payloads[8]["conclusion"], "success" if variant == "shared-success" else "failure")
                        failures = [row["payload"]["pr"] for row in store.actions() if row["kind"] == "merge_gate_failure"]
                        self.assertEqual(failures, [] if variant == "shared-success" else [8])
                        self.assertEqual(len(store.inbox(COORDINATOR)), 2)
                        receipts = {number: store.inbox(f"intersection-author-{number}")[0]["receipt_id"] for number in (7, 8)}
                        store.close()
                        with closing(Store(store.path)) as restored:
                            with Observer(restored, API(restored, world)) as observer:
                                self.assertTrue(observer.reconcile(force=True))
                            self.assertEqual(len(restored.actions()), 2)
                            self.assertEqual(len(restored.inbox(COORDINATOR)), 2)
                            self.assertEqual({number: restored.inbox(f"intersection-author-{number}")[0]["receipt_id"] for number in (7, 8)}, receipts)
                    self.ingest(world, check)

    def test_later_second_reporter_does_not_rebind_or_duplicate_shared_target_actions(self):
        for mode, world in self.worlds("shared-conflict"):
            second = world.reporters.pop()
            world.publisher.update(event="pull_request_target", status="in_progress", conclusion=None)
            with self.subTest(producer=mode):
                def check(store, tick):
                    self.assertTrue(tick())
                    before = {row["receipt_id"] for row in store.inbox(COORDINATOR)}
                    self.assertEqual(len(before), 2)
                    world.reporters.append(second)
                    self.assertTrue(tick(), [dict(row) for row in store.db.execute("SELECT * FROM problems")])
                    self.assertEqual(len(store.actions()), 2)
                    self.assertEqual({row["receipt_id"] for row in store.inbox(COORDINATOR)}, before)
                    self.assertEqual(len(store.inbox("intersection-author-8")), 1)
                self.ingest(world, check)

    def test_unresolved_sibling_stays_hot_without_cancelling_the_verified_target(self):
        for unresolved in (7, 8):
            verified = 15 - unresolved
            for mode, world in self.worlds("shared-success"):
                world.publisher["event"] = "pull_request_target"
                world.reporters = world.reporters[:1]
                target = world.metadata["targets"][unresolved - 7]
                proof = copy.deepcopy(target["validation"])
                target["validation"] = copy.deepcopy(world.metadata["targets"][verified - 7]["validation"])
                with self.subTest(unresolved=unresolved, producer=mode):
                    def check(store, tick):
                        self.assertFalse(tick())
                        self.assertEqual([row["payload"]["pr"] for row in store.actions()], [verified])
                        self.assertEqual(store.inbox(f"intersection-author-{unresolved}"), [])
                        self.assertEqual(store.meta_get(f"merge-report-source:{REPO}:201:1:7")["state"], "waiting")
                        first = store.inbox(f"intersection-author-{verified}")[0]["receipt_id"]
                        target["validation"] = proof
                        store.close()
                        with closing(Store(store.path)) as restored:
                            with Observer(restored, API(restored, world)) as observer:
                                self.assertTrue(observer.reconcile(force=True))
                            self.assertEqual(len(restored.actions()), 2)
                            self.assertEqual(len(restored.inbox(COORDINATOR)), 2)
                            self.assertEqual(restored.inbox(f"intersection-author-{verified}")[0]["receipt_id"], first)
                            self.assertEqual(len(restored.inbox(f"intersection-author-{unresolved}")), 1)
                    self.ingest(world, check)

    def test_live_shared_membership_is_checked_without_guessing_another_owner(self):
        for mutation in ("moved", "closed", "wrong-source", "wrong-base", "unlisted"):
            world = World("shared-success")
            world.reporters = world.reporters[:1]
            if mutation == "moved":
                world.prs[8]["head"]["sha"] = OTHER
            elif mutation == "closed":
                world.prs[8]["state"] = "closed"
            elif mutation == "wrong-source":
                world.prs[8]["head"]["repo"] = dict(id=18, full_name="other/ct0x")
            elif mutation == "wrong-base":
                world.prs[8]["base"]["sha"] = OTHER
            else:
                extra = copy.deepcopy(world.prs[8])
                extra.update(number=9, html_url=f"https://github.com/{REPO}/pull/9")
                world.prs[9] = extra
            with self.subTest(mutation=mutation):
                def check(store, tick):
                    self.assertFalse(tick())
                    expected = {7, 8} if mutation == "unlisted" else {7}
                    self.assertEqual({row["payload"]["pr"] for row in store.actions()}, expected)
                    self.assertEqual(store.inbox("intersection-author-9"), [])
                    self.assertEqual(store.meta_get(f"merge-report-source:{REPO}:201:1:7")["state"], "waiting")
                self.ingest(world, check)

    def test_waiting_target_never_creates_an_author_action(self):
        for variant in ("single-waiting", "shared-waiting"):
            for mode, world in self.worlds(variant):
                with self.subTest(variant=variant, producer=mode):
                    def check(store, tick):
                        self.assertFalse(tick())
                        if variant == "single-waiting":
                            self.assertEqual(store.actions(), [])
                            self.assertEqual(store.events(), [])
                        else:
                            self.assertEqual([row["payload"]["pr"] for row in store.actions()], [7])
                            self.assertEqual(store.inbox("intersection-author-8"), [])
                            self.assertIsNone(store.inbox("intersection-author-7")[0]["payload"]["check_conclusion"])
                            self.assertEqual(store.meta_get(f"merge-report-source:{REPO}:201:1:7")["state"], "waiting")
                    self.ingest(world, check)

    def test_invalid_or_ambiguous_intersection_never_authorizes(self):
        mutations = {
            "duplicate-target": lambda w: w.metadata["targets"].append(copy.deepcopy(w.metadata["targets"][0])),
            "missing-target": lambda w: w.metadata.update(targets=[]),
            "membership": lambda w: w.metadata["membership"][0].update(base=OTHER),
            "wrong-target-source": lambda w: w.metadata["targets"][0].update(head_repo_id=99),
            "wrong-target-pr": lambda w: w.metadata["targets"][0].update(pr=8),
            "missing-membership": lambda w: w.metadata.pop("membership"),
            "unknown-contract": lambda w: w.metadata.update(reason_code="unverified_contract"),
            "wrong-target-base": lambda w: w.metadata["targets"][0].update(base=OTHER),
            "unverified": lambda w: w.metadata["targets"][0].update(verification=False),
            "wrong-result": lambda w: w.metadata.update(failed_prs=[7]),
            "wrong-authority": lambda w: w.metadata["targets"][0]["authority"].update(id=102),
            "wrong-proof": lambda w: w.metadata["targets"][0]["validation"]["identity"].update(pr=8),
            "prior-publisher": lambda w: w.metadata["publisher"].update(run_attempt="2"),
            "foreign-issuer": lambda w: w.check["app"].update(id=9),
            "duplicate-membership": lambda w: w.metadata["membership"].append(copy.deepcopy(w.metadata["membership"][0])),
        }
        for label, mutate in mutations.items():
            world = World()
            mutate(world)
            with self.subTest(case=label):
                self.ingest(world, lambda store, tick: (self.assertFalse(tick()), self.assertEqual(store.actions(), [])))

    def test_independent_run_job_artifact_receipt_and_merge_checks_fail_closed(self):
        def corrupt_receipt(w):
            receipt = copy.deepcopy(w.artifacts[101]["receipt"])
            receipt["evidence"][0]["tree"] = OTHER
            w.install_artifact(101, receipt, 42)
            proof = w.metadata["targets"][0]["validation"]
            proof["artifact_digest"] = w.artifacts[101]["row"]["digest"]
        mutations = {
            "prior-native-attempt": lambda w: w.native[101].update(run_attempt=3),
            "native-failure": lambda w: w.native[101].update(conclusion="failure"),
            "foreign-native-workflow": lambda w: w.native[101].update(workflow_id=98),
            "foreign-native-source": lambda w: w.native[101].update(head_repository=dict(id=18, full_name="other/ct0x")),
            "job-skipped": lambda w: w.jobs[101][1].update(conclusion="skipped"),
            "job-other-attempt": lambda w: w.jobs[101][1].update(run_attempt=1),
            "missing-job": lambda w: w.jobs[101].pop(),
            "expired-artifact": lambda w: w.artifacts[101]["row"].update(expired=True),
            "wrong-artifact-digest": lambda w: w.blobs.update({42: b"not the archive"}),
            "wrong-evidence-tree": corrupt_receipt,
            "wrong-merge-tree": lambda w: w.commits[MERGE]["tree"].update(sha=OTHER),
            "wrong-merge-parents": lambda w: w.commits[MERGE].update(parents=[dict(sha=HEAD), dict(sha=BASE)]),
            "newer-native-run": lambda w: w.native.update({103: dict(w.native[101], id=103, run_attempt=1, conclusion="failure",
                html_url=f"https://github.com/{REPO}/actions/runs/103")}),
        }
        for label, mutate in mutations.items():
            world = World()
            mutate(world)
            with self.subTest(case=label):
                self.ingest(world, lambda store, tick: (self.assertFalse(tick()), self.assertEqual(store.actions(), [])))

    def test_check_pr_and_latest_authority_are_rechecked_before_delivery(self):
        for changed in ("pr", "check", "authority", "newer-run"):
            world = World()
            if changed in ("pr", "check"):
                endpoint = ROOT + ("/pulls/7" if changed == "pr" else "/check-runs/10")
                original = copy.deepcopy(world.prs[7] if changed == "pr" else world.row())
                reads = []
                def moving():
                    reads.append(True)
                    row = copy.deepcopy(original)
                    if changed == "check" or len(reads) > 1:
                        if changed == "pr":
                            row["head"]["sha"] = OTHER
                        else:
                            metadata = json.loads(row["output"]["summary"])
                            metadata["publisher"]["run_attempt"] = "2"
                            row["output"]["summary"] = json.dumps(metadata)
                    return Reply(200, {}, json.dumps(row).encode())
                world.overrides[endpoint] = moving
            else:
                original = copy.deepcopy(world.native[101])
                reads = []
                def moving():
                    reads.append(True)
                    row = copy.deepcopy(original)
                    if len(reads) > 1:
                        if changed == "authority":
                            row["run_attempt"] = 3
                        else:
                            world.native[103] = dict(original, id=103, run_attempt=1, status="in_progress", conclusion=None,
                                html_url=f"https://github.com/{REPO}/actions/runs/103")
                    return Reply(200, {}, json.dumps(row).encode())
                world.overrides[ROOT + "/actions/runs/101"] = moving
            with self.subTest(changed=changed):
                self.ingest(world, lambda store, tick: (self.assertFalse(tick()), self.assertEqual(store.actions(), [])))

    def test_closed_trigger_can_route_only_one_verified_surviving_target(self):
        world = World("single-conflict")
        old = world.prs[7]
        survivor = copy.deepcopy(old)
        survivor.update(number=8, html_url=f"https://github.com/{REPO}/pull/8")
        world.prs[8] = survivor
        old["state"] = "closed"
        world.metadata.update(pr=8, failed_prs=[8], membership=[details(survivor)])
        world.metadata["targets"][0]["pr"] = 8
        def check(store, tick):
            self.assertTrue(tick())
            payload, = [row["payload"] for row in store.actions()]
            self.assertEqual((payload["pr"], payload["reporter_job_id"]), (8, 707))
            self.assertEqual(store.inbox("intersection-author-7"), [])
            self.assertEqual(len(store.inbox("intersection-author-8")), 1)
        self.ingest(world, check)

    def test_legacy_reporter_discovers_one_source_check_not_two_conflicting_pr_claims(self):
        for mode, world in self.worlds("shared-conflict"):
            world.reporters = world.reporters[:1]
            world.reporters[0]["name"] = "Trusted Merge report"
            with self.subTest(producer=mode):
                def check(store, tick):
                    self.assertTrue(tick(), [dict(row) for row in store.db.execute("SELECT * FROM problems")])
                    self.assertEqual({row["payload"]["pr"] for row in store.actions()}, {7, 8})
                    self.assertEqual(len(store.inbox(COORDINATOR)), 2)
                    self.assertEqual(len(store.inbox("intersection-author-8")), 1)
                self.ingest(world, check)

    def test_closed_alias_reporter_uses_the_authenticated_shared_check_not_an_arbitrary_pr(self):
        for mode, world in self.worlds("shared-conflict"):
            alias = copy.deepcopy(world.prs[7])
            alias.update(number=9, state="closed", html_url=f"https://github.com/{REPO}/pull/9")
            world.prs[9] = alias
            world.reporters = [world.job(201, "Trusted Merge report / PR 9", 709)]
            with self.subTest(producer=mode):
                def check(store, tick):
                    self.assertTrue(tick(), [dict(row) for row in store.db.execute("SELECT * FROM problems")])
                    self.assertEqual({row["payload"]["pr"] for row in store.actions()}, {7, 8})
                    self.assertEqual(store.inbox("intersection-author-9"), [])
                    self.assertEqual(len(store.inbox("intersection-author-8")), 1)
                self.ingest(world, check)

    def test_shared_target_does_not_use_the_other_targets_proof(self):
        world = World("shared-success")
        world.metadata["targets"][1]["validation"] = copy.deepcopy(world.metadata["targets"][0]["validation"])
        def check(store, tick):
            self.assertFalse(tick())
            self.assertEqual([row["payload"]["pr"] for row in store.actions()], [7])
            self.assertEqual(store.inbox("intersection-author-8"), [])
        self.ingest(world, check)


if __name__ == "__main__":
    unittest.main()
