#!/usr/bin/env python3
"""Standalone stdlib, offline Git/SQLite/API fixtures; never contacts GitHub."""
import concurrent.futures
import copy
from contextlib import closing
from datetime import datetime, timedelta, timezone
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import sqlite3
import subprocess
import tempfile
import threading
import unittest
from unittest import mock
from urllib.parse import parse_qs, quote, unquote, urlsplit
import zipfile

SCRIPT = Path(__file__).with_name("release-tag-forward.py")
spec = importlib.util.spec_from_file_location("release_tag_forward", SCRIPT)
forwarder = importlib.util.module_from_spec(spec)
spec.loader.exec_module(forwarder)
REPO, COORDINATOR = "owner/project", "coordinator-test"
REF = "refs/tags/rc/historic"
STAMP = "2026-11-26T00:01:00+00:00"


def encode(value):
    return json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":"))


def digest(value):
    return hashlib.sha256(encode(value).encode()).hexdigest()


class GitFixture:
    def __init__(self, directory):
        self.root = Path(directory) / "git"
        self.root.mkdir()
        self.cache = {}
        self.git("init", "-b", "master", "-q")
        self.git("config", "user.name", "Offline fixture")
        self.git("config", "user.email", "offline@example.invalid")
        (self.root / "README").write_text("historic\n")
        self.git("add", ".")
        self.git("commit", "-qm", "historic target")
        self.target = self.git("rev-parse", "HEAD")
        self.git("checkout", "-qb", "foreign")
        (self.root / "foreign").write_text("not on master\n")
        self.git("add", ".")
        self.git("commit", "-qm", "foreign commit")
        self.foreign = self.git("rev-parse", "HEAD")
        self.git("checkout", "-q", "master")
        paths = {forwarder.core.WORKFLOW, forwarder.core.INTAKE, ".github/scripts/release-core.py",
                 ".github/scripts/release-control.py", *forwarder.control.FULL_PATHS}
        for name in paths:
            path = self.root / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text("# Offline published control source: " + name + "\n")
        (self.root / forwarder.core.WORKFLOW).write_text("name: Release candidate\non: workflow_dispatch\njobs: {}\n")
        self.git("add", ".")
        self.git("commit", "-qm", "trusted workflow")
        self.master = self.git("rev-parse", "HEAD")
        self.tag(REF, self.target, "nested")

    def git(self, *args):
        readonly = args[0] in ("rev-parse", "cat-file", "ls-tree", "merge-base", "show")
        if readonly and args in self.cache:
            return self.cache[args]
        value = subprocess.check_output(["git", "-C", str(self.root), *args], stderr=subprocess.PIPE).decode().strip()
        if readonly:
            self.cache[args] = value
        else:
            self.cache.clear()
        return value

    def sources(self, source):
        result = {}
        for line in self.git("ls-tree", "-r", source, ".github").splitlines():
            metadata, path = line.split("\t")
            if path in (forwarder.core.WORKFLOW, forwarder.core.INTAKE, *forwarder.control.FULL_PATHS) or (
                    path.startswith(".github/scripts/") and Path(path).name.startswith("release-")):
                result[path] = metadata.split()[2]
        return result

    def tag(self, ref, target, kind="lightweight"):
        name = ref.removeprefix("refs/tags/")
        if kind == "nested":
            self.git("tag", "-a", "-m", "inner", "inner", target)
            target = "inner"
        if kind in ("nested", "annotated"):
            self.git("-c", "advice.nestedTag=false", "tag", "-a", "-m", "annotation", name, target)
        else:
            self.git("tag", name, target)
        return self.git("rev-parse", ref)


class ObserverFixture:
    """The actual producer's schema and canonical ID encoding, not a JSON authority stub."""
    def __init__(self, path, fixture):
        self.path, self.fixture = path, fixture
        with closing(sqlite3.connect(path)) as db, db:
            db.executescript("""
                BEGIN;
                CREATE TABLE config(id INTEGER PRIMARY KEY,repo TEXT,coordinator TEXT);
                CREATE TABLE rc_ref_baselines(repo TEXT PRIMARY KEY,completed_at TEXT NOT NULL);
                CREATE TABLE rc_ref_candidates(repo TEXT,ref TEXT,tag_object TEXT,object_type TEXT,state TEXT,
                    sha TEXT,reason TEXT,updated_at TEXT,PRIMARY KEY(repo,ref,tag_object));
                CREATE TABLE actions(id TEXT PRIMARY KEY,repo TEXT,sha TEXT,run_id INTEGER,attempt INTEGER,
                    job TEXT,kind TEXT,payload TEXT,created_at TEXT);
                CREATE TABLE outbox(action_id TEXT,recipient TEXT,receipt_id TEXT UNIQUE,state TEXT,delivered_at TEXT,acked_at TEXT);
                CREATE TABLE inbox(receipt_id TEXT PRIMARY KEY,recipient TEXT,payload TEXT,delivered_at TEXT,acked_at TEXT);
            """)
            db.execute("INSERT INTO config VALUES(1,?,?)", (REPO, COORDINATOR))
            db.execute("INSERT INTO rc_ref_baselines VALUES(?,?)", (REPO, "2026-11-26T00:00:00+00:00"))
        path.chmod(0o600)

    def publish(self, ref=REF, target=None):
        f = self.fixture
        target = target or f.target
        obj = f.git("rev-parse", ref)
        kind = f.git("cat-file", "-t", obj)
        job = f"@tag:{ref}:{obj}"
        action_id = digest([REPO, target, None, None, job, "release_tag_forward"])
        receipt = digest([action_id, COORDINATOR])
        payload = dict(repo=REPO, sha=target, run_id=None, attempt=None, job=job, action="release_tag_forward",
                       tag=ref, tag_object=obj, tag_object_type=kind, source_head=target, tested_sha=None, pr=None,
                       workflow=forwarder.core.WORKFLOW, master_sha=f.master, native_workflow_absent=True,
                       native_workflow_absence={"verified": True, "sha": target,
                                                "tree": f.git("rev-parse", target + "^{tree}"),
                                                "path": forwarder.core.WORKFLOW})
        with closing(sqlite3.connect(self.path)) as db, db:
            db.execute("INSERT INTO rc_ref_candidates VALUES(?,?,?,?,?,?,NULL,?)", (REPO, ref, obj, kind, "forwarded", target, STAMP))
            db.execute("INSERT INTO actions VALUES(?,?,?,NULL,NULL,?,?,?,?)", (action_id, REPO, target, job, "release_tag_forward", encode(payload), STAMP))
            db.execute("INSERT INTO outbox VALUES(?,?,?,'delivered',?,NULL)", (action_id, COORDINATOR, receipt, STAMP))
            db.execute("INSERT INTO inbox VALUES(?,?,?,?,NULL)", (receipt, COORDINATOR, encode(payload), STAMP))
        return receipt

    def execute(self, sql, values=()):
        with closing(sqlite3.connect(self.path)) as db, db:
            db.execute(sql, values)


def make_api(fixture):
    class FixtureAPI(forwarder.API):
        def __init__(self):
            super().__init__(REPO)
            self.calls, self.posts, self.runs, self.statuses = [], [], [], []
            self.overrides, self.archives, self.artifact_rows, self.jobs_by_run, self.intents = {}, {}, {}, {}, {}
            self.on_post = lambda: (204, None)
            self.master_reads = 0
            self.on_master = lambda: fixture.master

        def request(self, method, path, data=None, binary=False):
            self.calls.append((method, path, copy.deepcopy(data)))
            if method == "POST":
                self.posts.append((path, data))
                return self.on_post()
            if path in self.overrides:
                value = self.overrides[path]
                if isinstance(value, BaseException):
                    raise value
                return copy.deepcopy(value)
            parsed, prefix = urlsplit(path), "/repos/" + REPO
            route, query = unquote(parsed.path), parse_qs(parsed.query)
            if route == "/user":
                return {"id": 9, "login": "owner"}
            if route == prefix:
                return {"id": 7, "full_name": REPO, "private": False, "default_branch": "master", "owner": {"id": 9}}
            if route == prefix + "/actions/workflows/release-candidate.yml":
                return {"id": 42, "path": forwarder.core.WORKFLOW, "state": "active"}
            if "/events" in route:
                raise AssertionError("Events API is never an authorization source (page 4 would be HTTP422)")
            if route.startswith(prefix + "/git/ref/"):
                ref = "refs/" + route.split("/git/ref/", 1)[1]
                try:
                    obj = fixture.git("rev-parse", "--verify", ref)
                except subprocess.CalledProcessError:
                    raise forwarder.HTTPError(404)
                return {"ref": ref, "object": {"sha": obj, "type": fixture.git("cat-file", "-t", obj)}}
            if route.startswith(prefix + "/git/tags/"):
                identity = route.rsplit("/", 1)[1]
                lines = fixture.git("cat-file", "-p", identity).splitlines()
                return {"sha": identity, "object": {"sha": lines[0].split()[1], "type": lines[1].split()[1]}}
            if route.startswith(prefix + "/git/commits/"):
                identity = route.rsplit("/", 1)[1]
                if fixture.git("cat-file", "-t", identity) != "commit":
                    raise forwarder.HTTPError(422)
                parents = fixture.git("show", "-s", "--format=%P", identity).split()
                return {"sha": identity, "tree": {"sha": fixture.git("rev-parse", identity + "^{tree}")},
                        "parents": [{"sha": parent} for parent in parents]}
            if route == prefix + "/commits/master":
                self.master_reads += 1
                return {"sha": self.on_master()}
            if route.startswith(prefix + "/compare/"):
                base, head = route.rsplit("/", 1)[1].split("...")
                ancestor = fixture.git("merge-base", base, head)
                return {"status": "identical" if base == head else "ahead" if ancestor == base else "diverged", "merge_base_commit": {"sha": ancestor}}
            if route.startswith(prefix + "/git/trees/"):
                identity = route.rsplit("/", 1)[1]
                rows = []
                args = ("ls-tree", "-r", identity) if query.get("recursive") == ["1"] else ("ls-tree", identity)
                for line in fixture.git(*args).splitlines():
                    metadata, name = line.split("\t")
                    mode, kind, sha = metadata.split()
                    rows.append({"path": name, "mode": mode, "type": kind, "sha": sha})
                return {"sha": identity, "truncated": False, "tree": rows}
            if route.startswith(prefix + "/contents/"):
                name, identity = route.split("/contents/", 1)[1], query["ref"][0]
                try:
                    blob = fixture.git("rev-parse", "--verify", identity + ":" + name)
                except subprocess.CalledProcessError:
                    raise forwarder.HTTPError(404)
                return {"type": "file", "path": name, "sha": blob}
            if route.endswith("/statuses"):
                return self.statuses
            if route == prefix + "/actions/workflows/release-candidate.yml/runs":
                self.assert_scoped(query)
                since = forwarder.timestamp(query["created"][0].removeprefix(">="))
                rows = [r for r in self.runs if forwarder.timestamp(r["created_at"]) >= since and r["event"] == query["event"][0]
                        and r["head_branch"] == query["branch"][0] and r["status"] == query["status"][0]]
                return {"total_count": len(rows), "workflow_runs": rows[:100]}
            if route.startswith(prefix + "/actions/runs/"):
                parts = route.split("/actions/runs/", 1)[1].split("/")
                identity = int(parts[0])
                if parts[-1] == "artifacts":
                    return {"artifacts": self.artifact_rows.get(identity, [])}
                if parts[-1] == "jobs":
                    return {"jobs": self.jobs_by_run.get(identity, [])}
                rows = [run for run in self.runs if run["id"] == identity]
                if not rows:
                    raise forwarder.HTTPError(404)
                return rows[0]
            if route.startswith(prefix + "/actions/artifacts/") and route.endswith("/zip"):
                return self.archives[int(route.split("/actions/artifacts/", 1)[1].split("/")[0])]
            raise AssertionError("Unexpected offline API request: " + path)

        def assert_scoped(self, query):
            if not (query.get("event") == ["workflow_dispatch"] and query.get("branch") == ["master"]
                    and query.get("created", [""])[0].startswith(">=") and query.get("status")
                    and query.get("page") == ["1"]):
                raise AssertionError("Unscoped/all-history RPC: " + str(query))

        def add_run(self, target=None, identity=71, intent=True, status="completed", created="2026-11-26T00:02:00Z"):
            target = target or fixture.target
            origin = forwarder.timestamp(created)
            def at(seconds):
                return (origin + timedelta(seconds=seconds)).isoformat()
            source = fixture.master
            actor = {"id": 9, "login": "owner", "type": "User"}
            run = dict(id=identity, workflow_id=42, path=forwarder.core.WORKFLOW, event="workflow_dispatch",
                       display_title="Release candidate " + target, head_branch="master", head_sha=source,
                       head_commit={"id": source, "tree_id": fixture.git("rev-parse", source + "^{tree}")},
                       repository={"id": 7, "full_name": REPO}, head_repository={"id": 7, "full_name": REPO},
                       actor=actor.copy(), triggering_actor=actor.copy(), run_attempt=1, status=status,
                       conclusion="success" if status == "completed" else None, created_at=created,
                       run_started_at=at(0) if status != "queued" else None, updated_at=at(7),
                       html_url=f"https://github.com/{REPO}/actions/runs/{identity}")
            self.runs.append(run)
            names = ("Release normalize", forwarder.control.admission_name(target))
            self.jobs_by_run[identity] = [dict(id=identity * 10 + i, name=name, run_id=identity, run_attempt=1,
                                              head_sha=source, status="completed", conclusion="success",
                                              started_at=at(1 + i * 3), completed_at=at(3 + i * 3))
                                          for i, name in enumerate(names)] if status != "queued" else []
            if intent:
                parents = fixture.git("show", "-s", "--format=%P", target).split()
                self.set_intent(identity, dict(schema=1, kind="release-intent", repo=REPO, sha=target,
                                               tree=fixture.git("rev-parse", target + "^{tree}"), run_id=identity,
                                               run_attempt=1, workflow_sha=source, policy_sha=source,
                                               policy="caller-policy-v1", master_sha=source, head=target,
                                               base=parents[0] if parents else target, sources=fixture.sources(source)))
            return run

        def set_intent(self, identity, value):
            self.intents[identity] = copy.deepcopy(value)
            producer = self.jobs_by_run[identity][0]
            created = forwarder.timestamp(producer["started_at"]) + timedelta(seconds=0.5)
            updated = created + timedelta(seconds=0.5)
            stream = io.BytesIO()
            with zipfile.ZipFile(stream, "w") as archive:
                archive.writestr("receipt.json", forwarder.core.encoded(value))
            raw = stream.getvalue()
            self.archives[identity * 100] = raw
            self.artifact_rows[identity] = [dict(id=identity * 100, name=f"release-intent-{identity}-1",
                                                digest="sha256:" + forwarder.core.hashed(raw), expired=False,
                                                workflow_run={"id": identity, "repository_id": 7, "head_repository_id": 7,
                                                              "head_sha": value["workflow_sha"], "head_branch": "master"},
                                                created_at=created.isoformat(), updated_at=updated.isoformat())]

        def owner(self):
            self.add_run()
            created = forwarder.timestamp(self.jobs_by_run[71][1]["started_at"]) + timedelta(seconds=0.5)
            self.statuses.append(dict(id=1001, context=forwarder.core.CONTEXT, state="success", description=forwarder.core.DESCRIPTION,
                                      creator=forwarder.core.BOT.copy(), target_url=f"https://github.com/{REPO}/actions/runs/71",
                                      created_at=created.isoformat()))
    return FixtureAPI()


class ForwardTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.tmp = tempfile.TemporaryDirectory(prefix="release-tag-template-")
        cls.addClassCleanup(cls.tmp.cleanup)
        cls.template = GitFixture(cls.tmp.name)

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(prefix="release-tag-test-")
        self.addCleanup(self.tmp.cleanup)
        # Share immutable Git objects; each test adds distinct disposable refs only.
        self.fixture = copy.copy(self.template)
        self.fixture.cache = self.template.cache.copy()
        self.api = make_api(self.fixture)
        self.db = Path(self.tmp.name) / "private/forward.sqlite"
        self.observer = ObserverFixture(Path(self.tmp.name) / "observer.sqlite", self.fixture)
        self.receipt = self.observer.publish()

    def consume(self, receipt=None):
        return forwarder.consume(self.api, self.observer.path, receipt or self.receipt, COORDINATOR, self.db)

    def contents(self, target=None):
        return f"/repos/{REPO}/contents/" + quote(forwarder.core.WORKFLOW, safe="") + "?ref=" + (target or self.fixture.target)

    def test_nested_tag_204_and_second_invocation_never_double_carry(self):
        result = self.consume()
        self.assertEqual((result["status"], result["http_status"], result["submitted"]), ("pending", 204, True))
        self.assertEqual(self.api.posts, [(f"/repos/{REPO}/actions/workflows/release-candidate.yml/dispatches",
                                          {"ref": "master", "inputs": {"sha": self.fixture.target}, "return_run_details": True})])
        self.assertIn(f"/repos/{REPO}/git/ref/tags%2Frc%2Fhistoric", [p for _, p, _ in self.api.calls])
        self.consume()
        self.assertEqual(len(self.api.posts), 1)
        self.assertEqual(self.db.stat().st_mode & 0o777, 0o600)

    def test_real_observer_identity_is_readonly_and_unmodified(self):
        before = self.observer.path.read_bytes()
        self.consume()
        self.assertEqual(self.observer.path.read_bytes(), before)
        with closing(sqlite3.connect(self.observer.path)) as db:
            action, receipt = db.execute("SELECT a.id,o.receipt_id FROM actions a JOIN outbox o ON a.id=o.action_id").fetchone()
        self.assertEqual(receipt, digest([action, COORDINATOR]))
        with self.assertRaises(ValueError):
            forwarder.consume(self.api, self.observer.path, receipt, "foreign-coordinator", self.db)

    def test_forged_receipt_or_payload_or_baseline_or_candidate_is_rejected(self):
        for sql in ("UPDATE inbox SET payload='{}'", "UPDATE actions SET id='fake'", "UPDATE actions SET run_id=71",
                    "UPDATE config SET repo='foreign/repo'", "UPDATE rc_ref_candidates SET state='baseline'",
                    "UPDATE rc_ref_candidates SET state='not_current'", "DELETE FROM rc_ref_baselines",
                    "UPDATE rc_ref_baselines SET completed_at='2026-11-27T00:00:00+00:00'",
                    "UPDATE actions SET job='@tag:fake'", "UPDATE outbox SET state='pending'"):
            with self.subTest(sql=sql):
                original = self.observer.path.read_bytes()
                self.observer.execute(sql)
                with self.assertRaises(ValueError):
                    self.consume()
                self.observer.path.write_bytes(original)
        with self.assertRaises(ValueError):
            self.consume("f" * 64)
        self.assertEqual(self.api.calls, [])

    def test_stable_id_does_not_override_tampered_payload_attributes(self):
        with closing(sqlite3.connect(self.observer.path)) as db:
            original = json.loads(db.execute("SELECT payload FROM actions").fetchone()[0])
        for field, value in (("tested_sha", self.fixture.target), ("attempt", 1), ("run_id", 71),
                             ("tag_object_type", "blob"), ("native_workflow_absent", False),
                             ("workflow", ".github/workflows/foreign.yml"), ("source_head", "f" * 40)):
            with self.subTest(field=field):
                payload = dict(original, **{field: value})
                self.observer.execute("UPDATE actions SET payload=?", (encode(payload),))
                self.observer.execute("UPDATE inbox SET payload=?", (encode(payload),))
                with self.assertRaises(ValueError):
                    self.consume()
        self.assertEqual(self.api.calls, [])

    def test_missing_observer_db_is_not_created(self):
        missing = Path(self.tmp.name) / "absent.sqlite"
        with self.assertRaises((ValueError, OSError, sqlite3.Error)):
            forwarder.consume(self.api, missing, self.receipt, COORDINATOR, self.db)
        self.assertFalse(missing.exists())

    def test_lightweight_annotated_and_unicode_plus_single_component_refs(self):
        for kind, name in (("lightweight", "候选+1"), ("annotated", "v1+2")):
            ref = "refs/tags/rc/" + name
            self.fixture.tag(ref, self.fixture.target, kind)
            receipt = self.observer.publish(ref)
            self.consume(receipt)
            escaped = f"/repos/{REPO}/git/ref/" + quote(ref[5:], safe="")
            self.assertIn(escaped, [p for _, p, _ in self.api.calls])
        self.assertEqual(len(self.api.posts), 1)

    def test_git_ref_rules_and_exact_rc_star_matcher(self):
        for ref in ("refs/tags/rc/a/b", "refs/tags/rc/a..b", "refs/tags/rc/a.lock", "refs/tags/rc/a@{b}",
                    "refs/tags/rc/", "refs/tags/rc/a\nb", "refs/tags/rc/.hidden", "refs/tags/not-rc/x"):
            with self.subTest(ref=ref), self.assertRaises(ValueError):
                forwarder.valid_tag(ref)
        forwarder.valid_tag("refs/tags/rc/候选+1")
        forwarder.valid_tag("refs/tags/rc/a;b")  # Legal Git name; never interpolated into a shell.

    def test_modern_target_goes_native_without_submitted_marker(self):
        ref = "refs/tags/rc/modern"
        self.fixture.tag(ref, self.fixture.master)
        receipt = self.observer.publish(ref, self.fixture.master)
        self.assertEqual(self.consume(receipt)["status"], "native")
        self.assertEqual(self.api.posts, [])
        with closing(sqlite3.connect(self.db)) as db:
            self.assertEqual(db.execute("SELECT count(*) FROM forwards").fetchone()[0], 0)

    def test_moved_deleted_mismatched_peel_and_foreign_master_fail_closed(self):
        path = f"/repos/{REPO}/git/ref/" + quote(REF[5:], safe="")
        raw = self.fixture.git("rev-parse", REF)
        for value in (forwarder.HTTPError(404), {"ref": REF, "object": {"sha": self.fixture.master, "type": "commit"}}):
            self.api.overrides[path] = value
            with self.assertRaises((ValueError, OSError)):
                self.consume()
        self.api.overrides.clear()
        self.api.overrides[f"/repos/{REPO}/git/tags/{raw}"] = {"sha": raw, "object": {"type": "commit", "sha": self.fixture.master}}
        with self.assertRaises(ValueError):
            self.consume()
        self.api.overrides.clear()
        ref = "refs/tags/rc/foreign"
        self.fixture.tag(ref, self.fixture.foreign)
        with self.assertRaises(ValueError):
            self.consume(self.observer.publish(ref, self.fixture.foreign))
        self.assertEqual(self.api.posts, [])

    def test_peel_cycles_and_depth_are_bounded(self):
        raw = self.fixture.git("rev-parse", REF)
        self.api.overrides[f"/repos/{REPO}/git/tags/{raw}"] = {"sha": raw, "object": {"type": "tag", "sha": raw}}
        with self.assertRaises(ValueError):
            self.consume()
        for i in range(18):
            identity = raw if i == 0 else f"{i:040x}"
            self.api.overrides[f"/repos/{REPO}/git/tags/{identity}"] = {"sha": identity, "object": {"type": "tag", "sha": f"{i+1:040x}"}}
        with self.assertRaises(ValueError):
            self.consume()
        self.assertEqual(self.api.posts, [])

    def test_only_contents_http404_and_complete_exact_tree_prove_absence(self):
        for error in (forwarder.HTTPError(401), forwarder.HTTPError(403), forwarder.HTTPError(429), forwarder.HTTPError(500), OSError("timeout")):
            self.api.overrides[self.contents()] = error
            with self.assertRaises((ValueError, OSError)):
                self.consume()
        self.api.overrides.clear()
        tree = self.fixture.git("rev-parse", self.fixture.target + "^{tree}")
        self.api.overrides[f"/repos/{REPO}/git/trees/{tree}"] = {"sha": tree, "truncated": True, "tree": []}
        with self.assertRaises(ValueError):
            self.consume()
        self.api.overrides.clear()
        ref = "refs/tags/rc/masked"
        self.fixture.tag(ref, self.fixture.master)
        self.api.overrides[self.contents(self.fixture.master)] = forwarder.HTTPError(404)
        with self.assertRaises(ValueError):
            self.consume(self.observer.publish(ref, self.fixture.master))
        self.assertEqual(self.api.posts, [])

    def test_owner_must_be_authenticated_and_master_workflow_active(self):
        for path, value in (("/user", {"id": 99}), (f"/repos/{REPO}/actions/workflows/release-candidate.yml",
                                                  {"id": 42, "path": forwarder.core.WORKFLOW, "state": "disabled_manually"})):
            self.api.overrides[path] = value
            with self.assertRaises(ValueError):
                self.consume()
            self.api.overrides.clear()
        self.assertEqual(self.api.posts, [])

    def test_harmless_master_advance_proves_both_snapshots(self):
        old = self.fixture.master
        tree = self.fixture.git("rev-parse", old + "^{tree}")
        new = self.fixture.git("commit-tree", tree, "-p", old, "-m", "advance")
        self.api.on_master = lambda: old if self.api.master_reads == 1 else new
        self.consume()
        paths = [path for _, path, _ in self.api.calls]
        for head in (old, new):
            self.assertIn(f"/repos/{REPO}/compare/{self.fixture.target}...{head}", paths)
        self.assertEqual(len(self.api.posts), 1)

    def test_durable_owner_survives_expired_artifacts_but_spoof_fails(self):
        self.api.owner()
        self.api.artifact_rows[71][0]["expired"] = True
        self.api.archives.clear()
        self.assertEqual(self.api.statuses[0]["state"], "success")
        self.assertEqual(self.api.jobs_by_run[71][1]["name"], "Release admission " + self.fixture.target)
        self.assertEqual((self.consume()["status"], self.consume()["run_id"]), ("existing", 71))
        self.api.statuses[0]["creator"]["id"] = 99
        with self.assertRaises(ValueError):
            self.consume()
        self.assertEqual(self.api.posts, [])
        self.assertFalse(any("/artifacts" in p for _, p, _ in self.api.calls))

    def test_over_300_events_and_1000_retired_candidates_never_require_history(self):
        for i in range(1001):
            self.api.runs.append(dict(id=i+1000, event="workflow_dispatch", head_branch="master", status="completed", created_at="2025-01-01T00:00:00Z"))
        self.consume()
        self.assertEqual(len(self.api.posts), 1)
        self.assertFalse(any("/events" in p or "/actions/runs/" in p for _, p, _ in self.api.calls))

    def test_matching_native_proof_checks_actual_tree_and_trusted_identity(self):
        run = self.api.add_run()
        self.api.set_intent(71, dict(self.api.intents[71], tree="f" * 40))
        with self.assertRaises(ValueError):
            self.consume()
        run["actor"] = {"id": 99}
        with self.assertRaises(ValueError):
            self.consume()
        self.assertEqual(self.api.posts, [])

    def test_trusted_run_rejects_self_consistent_foreign_repository(self):
        run = self.api.add_run()
        run["repository"] = run["head_repository"] = {"id": 8, "full_name": "owner/foreign"}
        self.api.trust = {"owner_id": 9, "workflow_id": 42, "master_sha": self.fixture.master}
        with self.assertRaisesRegex(ValueError, "仓库"):
            forwarder.trusted_run(self.api, run)

    def test_native_attempt_boundaries_and_job_run_ids_remain_required(self):
        run = self.api.add_run()
        for target, field, value in ((run, "run_started_at", None),
                                     (run, "run_started_at", "2026-11-26T00:04:00Z"),
                                     (self.api.jobs_by_run[71][0], "run_id", 72),
                                     (self.api.jobs_by_run[71][0], "run_attempt", 2)):
            with self.subTest(field=field, value=value):
                original = target[field]
                target[field] = value
                with self.assertRaises(ValueError):
                    self.consume()
                target[field] = original
        self.assertEqual(self.api.posts, [])
        self.assertFalse(any("/zip" in path for _, path, _ in self.api.calls))

    def test_native_archive_window_and_published_policy_cannot_be_bypassed(self):
        self.api.add_run()
        artifact = self.api.artifact_rows[71][0]
        original = artifact["updated_at"]
        for value in (None, "2026-11-26T00:04:00Z"):
            artifact["updated_at"] = value
            with self.subTest(updated_at=value), self.assertRaises(ValueError):
                self.consume()
        artifact["updated_at"] = original
        intent = copy.deepcopy(self.api.intents[71])
        intent["sources"][".github/scripts/release-control.py"] = "f" * 40
        self.api.set_intent(71, intent)
        with self.assertRaisesRegex(ValueError, "策略源码"):
            self.consume()
        self.assertEqual(self.api.posts, [])

    def test_owner_admission_name_must_bind_the_exact_target_sha(self):
        self.api.owner()
        self.api.jobs_by_run[71][1]["name"] = "Release admission " + self.fixture.master
        with self.assertRaisesRegex(ValueError, "绑定目标 SHA"):
            self.consume()
        self.assertEqual(self.api.posts, [])

    def test_unrelated_expired_intent_does_not_hide_matching_target(self):
        self.api.add_run(target=self.fixture.master, identity=70)
        self.api.artifact_rows[70][0]["expired"] = True
        self.api.add_run()
        self.assertEqual(self.consume()["run_id"], 71)
        self.assertEqual(self.api.posts, [])
        self.assertFalse(any("/runs/70/" in p for _, p, _ in self.api.calls))

    def test_matching_inflight_without_intent_skips_preparation_without_marker_and_resumes(self):
        run = self.api.add_run(intent=False, status="queued")
        value = self.consume()
        self.assertEqual((value["status"], value["submitted"]), ("pending", False))
        with closing(sqlite3.connect(self.db)) as db:
            self.assertEqual(db.execute("SELECT count(*) FROM forwards").fetchone()[0], 0)
        self.api.runs.remove(run)
        self.consume()
        self.assertEqual(len(self.api.posts), 1)

    def test_200_locator_requires_native_target_proof(self):
        def post():
            self.api.add_run(created=datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"))
            return 200, {"workflow_run_id": 71, "run_url": f"https://api.github.com/repos/{REPO}/actions/runs/71",
                         "html_url": f"https://github.com/{REPO}/actions/runs/71"}
        self.api.on_post = post
        result = self.consume()
        self.assertEqual((result["status"], result["run_id"], result["http_status"]), ("forwarded", 71, 200))
        self.consume()
        self.assertEqual(len(self.api.posts), 1)

    def test_200_ack_without_native_run_stays_pending_and_never_reposts(self):
        self.api.on_post = lambda: (200, {"workflow_run_id": 71,
                                         "run_url": f"https://api.github.com/repos/{REPO}/actions/runs/71",
                                         "html_url": f"https://github.com/{REPO}/actions/runs/71"})
        self.assertEqual(self.consume()["status"], "pending")
        self.assertEqual(self.consume()["status"], "pending")
        self.assertEqual(len(self.api.posts), 1)
        self.api.add_run(created=datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"))
        self.assertEqual(self.consume()["status"], "forwarded")
        self.assertEqual(len(self.api.posts), 1)

    def test_unknown_post_is_never_repeated_and_reconciles_only_after_attempt(self):
        def timeout():
            raise OSError("lost response")
        self.api.on_post = timeout
        value = self.consume()
        self.assertEqual((value["status"], value["submitted"]), ("pending", True))
        self.consume()
        self.assertEqual(len(self.api.posts), 1)
        run = self.api.add_run(created="2025-01-01T00:00:00Z")
        self.assertEqual(self.consume()["status"], "pending")
        self.api.runs.remove(run)
        self.api.add_run(created=datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"))
        self.assertEqual(self.consume()["status"], "forwarded")
        self.assertEqual(len(self.api.posts), 1)

    def test_crash_after_marker_and_same_sha_second_tag_never_replay(self):
        self.api.on_post = lambda: (_ for _ in ()).throw(KeyboardInterrupt())
        with self.assertRaises(KeyboardInterrupt):
            self.consume()
        ref = "refs/tags/rc/second"
        self.fixture.tag(ref, self.fixture.target)
        self.assertEqual(self.consume(self.observer.publish(ref))["status"], "pending")
        self.assertEqual(len(self.api.posts), 1)

    def test_two_local_invocations_and_busy_preparation_recover_without_double_send(self):
        entered, release = threading.Event(), threading.Event()
        def slow_post():
            entered.set()
            self.assertTrue(release.wait(10))
            return 204, None
        self.api.on_post = slow_post
        with concurrent.futures.ThreadPoolExecutor(max_workers=2) as pool:
            first = pool.submit(self.consume)
            self.assertTrue(entered.wait(10))
            try:
                self.assertEqual(pool.submit(self.consume).result(timeout=5)["status"], "busy")
            finally:
                release.set()
            first.result(timeout=10)
        self.assertEqual(len(self.api.posts), 1)

    def test_busy_preparation_without_submission_resumes_after_lock_release(self):
        action = forwarder.load_receipt(self.observer.path, REPO, COORDINATOR, self.receipt)
        with forwarder.State(self.db) as state, state.target_lock(action) as locked:
            self.assertTrue(locked)
            value = self.consume()
            self.assertEqual((value["status"], value["submitted"]), ("busy", False))
            self.assertEqual(state.db.execute("SELECT count(*) FROM forwards").fetchone()[0], 0)
        self.consume()
        self.assertEqual(len(self.api.posts), 1)

    def test_off_repo_cli_with_real_receipt_and_fake_gh_is_one_shot(self):
        responses = {}
        request = self.api.request
        def record(method, path, data=None, binary=False):
            try:
                value = request(method, path, data, binary)
            except forwarder.HTTPError as error:
                responses[path] = [error.status, None]
                raise
            if method == "GET":
                responses[path] = [200, value]
            return value
        with mock.patch.object(self.api, "request", side_effect=record):
            self.consume()
        directory = Path(self.tmp.name)
        response_file, post_log = directory / "responses.json", directory / "posts.jsonl"
        response_file.write_text(json.dumps(responses))
        executable = directory / "gh"
        executable.write_text('''#!/usr/bin/env python3
import json, os, pathlib, sys
path = sys.argv[2]
if sys.argv[sys.argv.index("--method") + 1] == "POST":
    body = json.load(sys.stdin)
    assert path.endswith("/actions/workflows/release-candidate.yml/dispatches")
    assert body == {"ref":"master", "inputs":{"sha":os.environ["OFFLINE_SHA"]}, "return_run_details":True}
    with open(os.environ["OFFLINE_POSTS"], "a") as output:
        output.write(json.dumps(body) + "\\n")
    print("HTTP/2.0 204 No Content\\n\\n", end="")
else:
    responses = json.loads(pathlib.Path(os.environ["OFFLINE_RESPONSES"]).read_text())
    status, body = [200,{"workflow_runs":[]}] if "/runs?" in path else responses[path]
    print("HTTP/2.0 " + str(status) + " Response\\n\\n" + json.dumps(body), end="")
    sys.exit(0 if status < 400 else 1)
''')
        executable.chmod(0o700)
        state = directory / "cli.sqlite"
        env = dict(os.environ, PATH=str(directory) + os.pathsep + os.environ["PATH"], OFFLINE_SHA=self.fixture.target,
                   OFFLINE_RESPONSES=str(response_file), OFFLINE_POSTS=str(post_log))
        command = ["python3", "-B", str(SCRIPT.resolve()), "--repo", REPO, "--observer-db", str(self.observer.path),
                   "--receipt", self.receipt, "--coordinator", COORDINATOR, "--state", str(state)]
        for _ in range(2):
            process = subprocess.run(command, cwd=directory, env=env, capture_output=True, text=True, timeout=30)
            self.assertEqual(process.returncode, 2, process.stderr)
            self.assertEqual(json.loads(process.stdout)["status"], "pending")
        self.assertEqual(len(post_log.read_text().splitlines()), 1)

    def test_preparation_failure_before_marker_can_resume(self):
        self.api.overrides[self.contents()] = forwarder.HTTPError(503)
        with self.assertRaises(OSError):
            self.consume()
        self.api.overrides.clear()
        self.consume()
        self.assertEqual(len(self.api.posts), 1)

    def test_503_unknown_and_422_rejection_are_not_retried(self):
        for code, status in ((503, "pending"), (422, "rejected")):
            with self.subTest(code=code):
                self.db = Path(self.tmp.name) / f"state-{code}.sqlite"
                self.api.on_post = lambda: (_ for _ in ()).throw(forwarder.HTTPError(code))
                result = self.consume()
                self.assertEqual((result["status"], result["http_status"]), (status, code))
                self.consume()
        self.assertEqual(len(self.api.posts), 2)


class TransportTests(unittest.TestCase):
    def test_host_api_supports_core_immutable_provenance_reads(self):
        api = forwarder.API(REPO)
        sha = "a" * 40
        body = {"sha": sha, "tree": {"sha": "b" * 40}}
        response = b"HTTP/2.0 200 OK\r\n\r\n" + json.dumps(body).encode()
        with mock.patch.object(forwarder.subprocess, "run", return_value=subprocess.CompletedProcess([], 0, response, b"")) as run:
            try:
                self.assertEqual(api.immutable(f"/repos/{REPO}/git/commits/{sha}"), body)
                self.assertEqual(api.immutable(f"/repos/{REPO}/git/commits/{sha}"), body)
            except AttributeError as error:
                self.fail("Host API does not initialize the trusted core contract: " + str(error))
            self.assertEqual(run.call_count, 1)

    def test_only_fixed_dispatch_write_via_gh_and_204_is_empty_success(self):
        api = forwarder.API(REPO)
        with mock.patch.dict(os.environ, {"GH_DEBUG": "api", "GH_HTTP_LOG": "/private/log"}), mock.patch.object(
                forwarder.subprocess, "run", return_value=subprocess.CompletedProcess([], 0, b"HTTP/2.0 204 No Content\r\n\r\n", b"")) as run:
            self.assertEqual(api.dispatch("a" * 40), (204, None))
            argv, kwargs = run.call_args
            self.assertEqual(argv[0][:2], ["gh", "api"])
            self.assertIn("--include", argv[0])
            self.assertIn("github.com", argv[0])
            self.assertNotIn("GH_DEBUG", kwargs["env"])
            self.assertNotIn("GH_HTTP_LOG", kwargs["env"])
            self.assertNotIn("shell", kwargs)
            for method, path in (("DELETE", f"/repos/{REPO}/git/refs/tags/x"), ("POST", f"/repos/{REPO}/statuses/x"), ("GET", "/repos/foreign/project")):
                with self.assertRaises(ValueError):
                    api.request(method, path, {})
            self.assertEqual(run.call_count, 1)

    def test_only_native_http_status_is_authoritative_no_raw_errors_or_tokens(self):
        api = forwarder.API(REPO)
        for raw, code in ((b'HTTP/2.0 404 Not Found\n\n{}', 404), (b'HTTP/2.0 503 Unavailable\r\n\r\n{}', 503)):
            with mock.patch.object(forwarder.subprocess, "run", return_value=subprocess.CompletedProcess([], 1, raw, b"private token")):
                with self.assertRaises(forwarder.HTTPError) as error:
                    api.get("/user")
                self.assertEqual(error.exception.status, code)
                self.assertNotIn("private", str(error.exception))
        with mock.patch.object(forwarder.subprocess, "run", return_value=subprocess.CompletedProcess([], 1, b"", b"private token")):
            with self.assertRaises(OSError) as error:
                api.get("/user")
            self.assertNotIn("private", str(error.exception))

    def test_off_repo_cli_accepts_only_db_receipt_not_json_authority(self):
        with tempfile.TemporaryDirectory(prefix="release-tag-cli-") as directory:
            value = subprocess.run(["python3", "-B", str(SCRIPT.resolve()), "--help"], cwd=directory, capture_output=True, text=True, timeout=10)
        self.assertEqual(value.returncode, 0, value.stderr)
        for flag in ("--observer-db", "--receipt", "--coordinator", "--state", "--repo"):
            self.assertIn(flag, value.stdout)
        self.assertNotIn("--action", value.stdout)


if __name__ == "__main__":
    unittest.main()
