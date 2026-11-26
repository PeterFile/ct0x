#!/usr/bin/env python3
"""Offline tests for immutable targeting and the serialized admission protocol."""
import copy
import hashlib
import importlib.util
import io
import json
from pathlib import Path
from types import SimpleNamespace
import threading
import unittest
from unittest.mock import patch
import urllib.error
from urllib.parse import quote, unquote
import zipfile

HERE = Path(__file__).resolve().parent
SHA, TREE, MASTER = "a" * 40, "b" * 40, "c" * 40
REPO = "owner/project"


def load(name):
    path = HERE / (name + ".py")
    assert path.is_file(), "缺少候选发布行为：" + name
    spec = importlib.util.spec_from_file_location(name.replace("-", "_"), path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


class TargetAPI:
    def get(self, path):
        if path.endswith("/commits/master"):
            return {"sha": MASTER, "commit": {"tree": {"sha": TREE}}}
        if "/git/ref/tags/" in path:
            return {"ref": "refs/tags/" + unquote(path.split("/git/ref/tags/", 1)[1]),
                    "object": {"type": "tag", "sha": "d" * 40}}
        if path.endswith("/git/tags/" + "d" * 40):
            return {"sha": "d" * 40, "object": {"type": "commit", "sha": SHA}}
        if "/git/commits/" in path:
            return {"sha": SHA, "tree": {"sha": TREE}, "parents": [{"sha": MASTER}]}
        if "/compare/" in path:
            return {"status": "ahead", "merge_base_commit": {"sha": SHA}}
        raise AssertionError(path)


class TargetTests(unittest.TestCase):
    def setUp(self):
        self.control = load("release-control")
        self.api = TargetAPI()
        self.event = {"repository": {"full_name": REPO}, "inputs": {"sha": SHA.upper()}}

    def test_dispatch_normalizes_only_a_full_commit_on_master(self):
        result = self.control.resolve_target(self.api, REPO, "workflow_dispatch", self.event, "refs/heads/master")
        self.assertEqual((result["sha"], result["tree"], result["master_sha"]), (SHA, TREE, MASTER))
        for value in ["master", SHA[:12], SHA + "\n", "refs/tags/rc/x"]:
            with self.subTest(value=value), self.assertRaises(ValueError):
                self.control.resolve_target(self.api, REPO, "workflow_dispatch", {**self.event, "inputs": {"sha": value}}, "refs/heads/master")
        with self.assertRaises(ValueError):
            self.control.resolve_target(self.api, REPO, "workflow_dispatch", self.event, "refs/heads/other")

    def test_tag_peels_and_matches_the_native_create_event(self):
        event = {"repository": {"full_name": REPO}, "ref": "refs/tags/rc/test", "before": "0" * 40,
                 "after": "d" * 40, "created": True, "deleted": False, "forced": False}
        for after in ["d" * 40, SHA]:
            for native in [SHA, "d" * 40]:
                with self.subTest(after=after, native=native):
                    self.assertEqual(self.control.resolve_target(self.api, REPO, "push", event | {"after": after}, event["ref"], native)["sha"], SHA)
        for change in [{"deleted": True}, {"created": False}, {"forced": True}, {"before": SHA}, {"after": MASTER}]:
            with self.subTest(change=change), self.assertRaises(ValueError):
                self.control.resolve_target(self.api, REPO, "push", event | change, event["ref"], SHA)
        original = self.api.get
        self.api.get = lambda path: {"ref": event["ref"], "object": {"type": "commit", "sha": SHA}} if "/git/ref/" in path else original(path)
        self.assertEqual(self.control.resolve_target(self.api, REPO, "push", event | {"after": SHA}, event["ref"], SHA)["sha"], SHA)

    def test_historical_target_uses_explicit_caller_policy_not_candidate_scripts(self):
        from unittest.mock import patch
        calls = []
        def git(root, *args):
            calls.append(args)
            if args == ("rev-parse", "HEAD"):
                return TREE
            if args[0] == "merge-base":
                self.assertEqual(args[2], MASTER)
                return args[1]
            if args[0] == "ls-tree":
                self.assertIn(TREE, args)
                return ".github/scripts/release-core.py\n.github/scripts/release-control.py"
            if args[0] == "show":
                self.assertTrue(args[1].startswith(TREE + ":"))
                return "on:\n  workflow_call:\n    inputs:\n      policy_sha:\n"
            if args[0] == "rev-parse":
                self.assertTrue(args[1].startswith(TREE + ":"), "must not read product or newer master policy")
                return "9" * 40
            raise AssertionError(args)
        with patch.object(self.control.core, "git", side_effect=git):
            try:
                result = self.control.source_identity(HERE, SHA, TREE, MASTER)
            except ValueError as error:
                self.fail("trusted caller policy must permit a historical product checkout: " + str(error))
        self.assertIn(".github/workflows/ci-full.yml", result)
        self.assertFalse(any(args[0] == "rev-parse" and args[1].startswith(SHA + ":") for args in calls))

    def test_rc_ref_format_is_single_component_but_allows_unicode_and_plus(self):
        original = self.api.get
        for tag in ["rc/候选+构建%1", "rc/-candidate", "rc/@", "rc/v1.2"]:
            calls = []
            def get(path):
                calls.append(path)
                return original(path)
            self.api.get = get
            ref = "refs/tags/" + tag
            event = {"repository": {"full_name": REPO}, "ref": ref, "before": "0" * 40,
                     "after": SHA, "created": True, "deleted": False, "forced": False}
            self.assertEqual(self.control.resolve_target(self.api, REPO, "push", event, ref, SHA)["sha"], SHA)
            self.assertTrue(any(path.endswith("/git/ref/tags/" + quote(tag, safe="")) for path in calls))
        for tag in ["rc/", "rc/x/y", "rc/.x", "rc/x.lock", "rc/x.", "rc/x..y", "rc/x@{y", "rc/x y",
                    "rc/x~y", "rc/x^y", "rc/x:y", "rc/x?y", "rc/x*y", "rc/x[y", "rc/x\\y", "rc/x\x7f", "rc/x\x01"]:
            ref = "refs/tags/" + tag
            event = {"repository": {"full_name": REPO}, "ref": ref, "before": "0" * 40,
                     "after": SHA, "created": True, "deleted": False, "forced": False}
            with self.subTest(tag=tag), self.assertRaises(ValueError):
                self.control.resolve_target(self.api, REPO, "push", event, ref, SHA)

    def test_nested_tag_pins_event_and_native_sha_without_accepting_inner_or_unrelated_after(self):
        outer, inner = "d" * 40, "e" * 40
        original = self.api.get
        def get(path):
            if path.endswith("/git/tags/" + outer):
                return {"sha": outer, "object": {"type": "tag", "sha": inner}}
            if path.endswith("/git/tags/" + inner):
                return {"sha": inner, "object": {"type": "commit", "sha": SHA}}
            return original(path)
        self.api.get = get
        ref = "refs/tags/rc/nested"
        event = {"repository": {"full_name": REPO}, "ref": ref, "before": "0" * 40,
                 "after": outer, "created": True, "deleted": False, "forced": False}
        for after in [outer, SHA]:
            self.assertEqual(self.control.resolve_target(self.api, REPO, "push", event | {"after": after}, ref, SHA)["sha"], SHA)
        for after, native in [(inner, SHA), (MASTER, SHA), (outer, MASTER), (outer, None)]:
            with self.subTest(after=after, native=native), self.assertRaises(ValueError):
                self.control.resolve_target(self.api, REPO, "push", event | {"after": after}, ref, native)

    def test_source_identity_rejects_wrong_checkout_unreachable_controller_and_unknown(self):
        with patch.object(self.control.core, "git", return_value=MASTER):
            with self.assertRaises(ValueError):
                self.control.source_identity(HERE, SHA, TREE, MASTER)
        def git(root, *args):
            return TREE if args == ("rev-parse", "HEAD") else MASTER
        with patch.object(self.control.core, "git", side_effect=git), self.assertRaises(ValueError):
            self.control.source_identity(HERE, SHA, TREE, MASTER)
        with patch.object(self.control.core, "git", side_effect=OSError("opaque native error")), self.assertRaises(OSError):
            self.control.source_identity(HERE, SHA, TREE, MASTER)

    def test_source_identity_handles_master_advance_without_reading_new_local_master_files(self):
        api = NativeAPI()
        newer = "f" * 40
        api.master = newer
        api.reachable[newer] = {newer, MASTER, SHA}
        calls = []
        def git(root, *args):
            calls.append(args)
            if args == ("rev-parse", "HEAD"):
                return MASTER
            if args[0] == "ls-tree":
                self.assertIn(MASTER, args)
                return ".github/scripts/release-core.py\n.github/scripts/release-control.py"
            if args[0] == "show":
                return "on:\n  workflow_call:\n    inputs:\n      policy_sha:\n"
            if args[0] == "rev-parse":
                self.assertTrue(args[1].startswith(MASTER + ":"))
                return "9" * 40
            raise AssertionError("must not require the newly advanced master in the local checkout: " + repr(args))
        with patch.object(self.control.core, "git", side_effect=git):
            result = self.control.source_identity(HERE, SHA, MASTER, newer, api)
        self.assertIn(".github/scripts/ci-plan.py", result)
        self.assertTrue(any("/compare/" + MASTER + "..." + newer in path for _, path in api.calls))

    def test_unreachable_or_mismatched_commit_fails_closed(self):
        original = self.api.get
        for response in [{"status": "diverged", "merge_base_commit": {"sha": MASTER}},
                         {"status": "ahead", "merge_base_commit": {"sha": MASTER}}]:
            self.api.get = lambda path: response if "/compare/" in path else original(path)
            with self.assertRaises(ValueError):
                self.control.resolve_target(self.api, REPO, "workflow_dispatch", self.event, "refs/heads/master")


class NativeAPI(load("release-control").AdmissionAPI):
    """REST-shaped data at the HTTP boundary; never opens a network socket."""
    def __init__(self):
        super().__init__(REPO, "offline-only")
        control = load("release-control")
        self.paths = [control.core.WORKFLOW, control.core.INTAKE,
                      ".github/scripts/release-core.py", ".github/scripts/release-control.py",
                      ".github/scripts/release-machine.py"] + list(control.FULL_PATHS)
        self.blobs = {path: hashlib.sha1(path.encode()).hexdigest() for path in self.paths}
        self.repo_info = {"id": 5, "full_name": REPO, "owner": {"id": 42, "login": "owner", "type": "User"}}
        self.actor = {"id": 42, "login": "owner", "type": "User"}
        self.permission = {"permission": "write", "user": {"id": 52, "login": "maintainer", "type": "User"}}
        self.master, self.tag_objects = MASTER, {}
        self.old = "d" * 40
        self.trees = {"1" * 40: self.blobs.copy(), TREE: self.blobs.copy(), "2" * 40: self.blobs.copy()}
        self.commits = {MASTER: {"sha": MASTER, "tree": {"sha": "1" * 40}, "parents": [{"sha": SHA}]},
                        SHA: {"sha": SHA, "tree": {"sha": TREE}, "parents": [{"sha": "e" * 40}]},
                        self.old: {"sha": self.old, "tree": {"sha": "2" * 40}, "parents": [{"sha": SHA}]}}
        self.reachable = {MASTER: {MASTER, SHA, self.old}, self.old: {self.old, SHA}}
        self.native = {"id": 10, "run_attempt": 1, "path": control.core.WORKFLOW, "event": "workflow_dispatch",
                       "head_branch": "master", "head_sha": MASTER, "head_commit": {"id": MASTER, "tree_id": "1" * 40},
                       "repository": {"full_name": REPO},
                       "head_repository": {"full_name": REPO}, "run_started_at": "2026-11-26T15:56:47Z",
                       "status": "in_progress", "conclusion": None, "actor": self.actor.copy(), "triggering_actor": self.actor.copy()}
        self.intent = {"schema": 1, "kind": "release-intent", "repo": REPO, "sha": SHA, "tree": TREE,
                       "run_id": 10, "run_attempt": 1, "workflow_sha": MASTER, "master_sha": MASTER,
                       "policy_sha": MASTER, "policy": "caller-policy-v1", "head": SHA, "base": "e" * 40, "allow_retry": False, "sources": self.blobs.copy()}
        self.job_rows = [{"id": 20, "run_id": 10, "run_attempt": 1, "name": "Release normalize",
                          "status": "completed", "conclusion": "success", "started_at": "2026-11-26T15:56:48Z",
                          "completed_at": "2026-11-26T15:56:52Z"},
                         {"id": 21, "run_id": 10, "run_attempt": 1, "name": "Release admission " + SHA,
                          "status": "in_progress", "conclusion": None, "started_at": "2026-11-26T15:56:53Z", "completed_at": None}]
        self.status_rows = [{"id": 40, "context": control.core.CONTEXT, "state": "pending", "description": "release-owner-v1",
                             "creator": control.core.BOT.copy(), "target_url": f"https://github.com/{REPO}/actions/runs/10",
                             "created_at": "2026-11-26T15:56:53.1Z"}]
        self.status_rows_template = copy.deepcopy(self.status_rows[0])
        self.second_run, self.second_jobs = None, []
        self.artifact_time = "2026-11-26T15:56:51Z"
        self.expired, self.tag, self.calls, self.posts = False, SHA, [], 0

    def request(self, method, path, data=None, binary=False):
        self.calls.append((method, path))
        prefix = self.prefix
        if method == "POST" and path == prefix + "/statuses/" + SHA:
            self.posts += 1
            self.status_rows.append(self.status_rows_template | data)
            return self.status_rows[-1]
        assert method == "GET", (method, path)
        if path == prefix:
            return copy.deepcopy(self.repo_info)
        if "/collaborators/" in path and path.endswith("/permission"):
            return copy.deepcopy(self.permission)
        if path == prefix + "/commits/master":
            return {"sha": self.master}
        if path.startswith(prefix + "/git/commits/"):
            identity = path.rsplit("/", 1)[1]
            if identity not in self.commits:
                raise OSError("unknown commit")
            return copy.deepcopy(self.commits[identity])
        if path.startswith(prefix + "/git/trees/"):
            identity = path.rsplit("/", 1)[1].split("?")[0]
            return self.tree_response(identity, "recursive=1" in path)
        if path.startswith(prefix + "/compare/"):
            base, head = path.rsplit("/", 1)[1].split("...")
            return {"status": "identical" if base == head else "ahead" if base in self.reachable.get(head, set()) else "diverged",
                    "merge_base_commit": {"sha": base if base in self.reachable.get(head, set()) else MASTER}}
        if path.startswith(prefix + "/git/ref/tags/"):
            if isinstance(self.tag, Exception):
                raise self.tag
            return {"ref": "refs/tags/" + unquote(path.split("/git/ref/tags/", 1)[1]),
                    "object": self.tag if isinstance(self.tag, dict) else {"type": "commit", "sha": self.tag}}
        if path.startswith(prefix + "/git/tags/"):
            identity = path.rsplit("/", 1)[1]
            if identity not in self.tag_objects:
                raise OSError("HTTP 404: native tag identity unavailable")
            return copy.deepcopy(self.tag_objects[identity])
        if path == prefix + "/actions/runs/10/attempts/2":
            return copy.deepcopy(self.second_run)
        if path.startswith(prefix + "/actions/runs/10/attempts/2/jobs?"):
            return {"total_count": len(self.second_jobs), "jobs": copy.deepcopy(self.second_jobs)}
        if path == prefix + "/actions/runs/10":
            return copy.deepcopy(self.second_run or self.native)
        if path == prefix + "/actions/runs/10/attempts/1":
            return copy.deepcopy(self.native)
        if path.startswith(prefix + "/actions/runs/10/attempts/1/jobs?"):
            return {"total_count": len(self.job_rows), "jobs": copy.deepcopy(self.job_rows)}
        if path.startswith(prefix + "/commits/" + SHA + "/statuses?"):
            return copy.deepcopy(self.status_rows)
        if path.startswith(prefix + "/actions/runs/10/artifacts?"):
            raw = self.receipt_zip()
            return {"total_count": 1, "artifacts": [{"id": 30, "name": f"release-intent-10-{self.intent['run_attempt']}", "expired": self.expired,
                    "digest": "sha256:" + hashlib.sha256(raw).hexdigest(), "workflow_run": {"id": 10},
                    "created_at": self.artifact_time, "updated_at": self.artifact_time}]}
        if path == prefix + "/actions/artifacts/30/zip":
            return self.receipt_zip()
        raise AssertionError("unexpected offline API request: " + path)

    def tree_response(self, identity, recursive):
        for root, files in self.trees.items():
            directories = {""} | {str(Path(path).parent) for path in files} | {".github"}
            for directory in directories:
                node = root if not directory else hashlib.sha1((root + ":" + directory).encode()).hexdigest()
                if node != identity:
                    continue
                prefix = directory + "/" if directory else ""
                entries = {}
                for path, value in files.items():
                    if not path.startswith(prefix):
                        continue
                    relative = path[len(prefix):]
                    if not recursive and "/" in relative:
                        name = relative.split("/", 1)[0]
                        subtree = prefix + name
                        entries[name] = {"path": name, "type": "tree", "mode": "040000",
                                         "sha": hashlib.sha1((root + ":" + subtree).encode()).hexdigest()}
                    else:
                        entries[relative] = {"path": relative, "type": "blob", "mode": "100644", "sha": value}
                return {"sha": node, "truncated": False, "tree": list(entries.values())}
        raise AssertionError("unknown immutable tree " + identity)

    def receipt_zip(self):
        stream = io.BytesIO()
        with zipfile.ZipFile(stream, "w") as archive:
            archive.writestr(zipfile.ZipInfo("receipt.json", (2026, 11, 26, 15, 56, 51)), json.dumps(self.intent))
        return stream.getvalue()


class ConsumerAuthorityTests(unittest.TestCase):
    def setUp(self):
        self.control, self.api = load("release-control"), NativeAPI()

    def read(self):
        return self.control.read_intent(self.api, 10, 1)

    def test_dispatch_reads_only_native_published_sources_and_actual_target(self):
        self.assertEqual(self.read(), self.api.intent)
        paths = [path for _, path in self.api.calls]
        self.assertTrue(any("/compare/" in path for path in paths), "consumer never verified native master ancestry")
        self.assertTrue(any("/git/trees/" in path for path in paths), "consumer trusted self-declared source hashes")

    def test_spoofed_branch_path_event_and_foreign_repository_are_rejected(self):
        for change in [{"head_branch": "feature/spoof"}, {"head_branch": "refs/tags/rc/spoof"},
                       {"path": ".github/workflows/other.yml"}, {"event": "pull_request"},
                       {"repository": {"full_name": "foreign/project"}}, {"head_repository": {"full_name": "foreign/project"}}]:
            self.api = NativeAPI()
            self.api.native.update(change)
            with self.subTest(change=change), self.assertRaises(ValueError):
                self.read()

    def test_target_tree_and_source_self_declarations_are_not_authority(self):
        for change in [{"tree": "f" * 40}, {"head": MASTER}, {"base": MASTER},
                       {"workflow_sha": SHA}, {"sources": {}}, {"kind": "release-full"},
                       {"allow_retry": "true"}, {"schema": True}]:
            self.api = NativeAPI()
            self.api.intent.update(change)
            with self.subTest(change=change), self.assertRaises(ValueError):
                self.read()
        self.api = NativeAPI()
        for path in self.control.FULL_PATHS:
            self.api.trees[TREE][path] = "f" * 40
        self.assertEqual(self.read(), self.api.intent, "product CI policy is deliberately independent")

    def test_unreachable_control_or_target_and_api_unknown_fail_closed(self):
        self.api.reachable[MASTER].remove(SHA)
        with self.assertRaises(ValueError):
            self.read()
        self.api = NativeAPI()
        self.api.native["head_sha"] = self.api.old
        self.api.intent["workflow_sha"] = self.api.old
        self.api.reachable[MASTER].remove(self.api.old)
        with self.assertRaises(ValueError):
            self.read()
        self.api = NativeAPI()
        del self.api.commits[SHA]
        with self.assertRaises(OSError):
            self.read()

    def test_older_published_dispatch_is_compatible_with_prior_installed_consumer(self):
        old = self.api.old
        self.api.native.update(head_sha=old, head_commit={"id": old, "tree_id": "2" * 40})
        self.api.trees["2" * 40][".github/scripts/release-control.py"] = "f" * 40
        self.api.trees["2" * 40][".github/scripts/ci-plan.py"] = "8" * 40
        self.api.trees[TREE][".github/scripts/ci-plan.py"] = "7" * 40
        self.api.intent.update(workflow_sha=old, policy_sha=old, master_sha=old, sources=self.api.trees["2" * 40].copy())
        self.assertEqual(self.read(), self.api.intent)
        self.assertTrue(any("/compare/" + old + "..." + MASTER in path for _, path in self.api.calls))

    def tag_run(self):
        self.api.native.update(event="push", head_branch="rc/test", head_sha=SHA, head_commit={"id": SHA, "tree_id": TREE})
        self.api.intent.update(workflow_sha=SHA, policy_sha=SHA)

    def test_native_tag_head_and_tree_must_match_actual_target(self):
        self.tag_run()
        self.api.status_rows = []
        self.assertEqual(self.read(), self.api.intent)
        self.api.native["head_sha"] = MASTER
        self.api.intent["workflow_sha"] = MASTER
        with self.assertRaises((ValueError, OSError)):
            self.read()
        self.api = NativeAPI()
        self.tag_run()
        self.api.tag, self.api.status_rows = MASTER, []
        with self.assertRaises(ValueError):
            self.read()

    def test_native_recorded_head_tree_is_checked_not_just_receipt_tree(self):
        self.tag_run()
        for change in [{"id": MASTER, "tree_id": TREE}, {"id": SHA, "tree_id": "f" * 40}, None]:
            self.api.native["head_commit"] = change
            with self.subTest(change=change), self.assertRaises((ValueError, OSError)):
                self.read()

    def complete_owner(self, attempt=1):
        if attempt > 1:
            self.prepare_retry()
            self.api.native.update(status="completed", conclusion="failure")
        else:
            self.api.job_rows[1].update(status="completed", conclusion="success", completed_at="2026-11-26T15:56:54Z")
        run = self.api.second_run if attempt > 1 else self.api.native
        run.update(status="completed", conclusion="success")

    def test_guard_explicitly_selects_active_or_successfully_completed_read_only_state(self):
        states = [("in_progress", None), ("completed", "success"), ("completed", None),
                  ("completed", "failure"), ("completed", "cancelled"), ("completed", "timed_out"),
                  ("queued", None), (None, None), ("in_progress", "cancelled")]
        for status, conclusion in states:
            for completed in (False, True):
                self.api = NativeAPI()
                self.complete_owner()
                self.api.native.update(status=status, conclusion=conclusion)
                expected = (status, conclusion) == (("completed", "success") if completed else ("in_progress", None))
                with self.subTest(status=status, conclusion=conclusion, completed=completed):
                    if expected:
                        self.assertEqual(self.control.guard(self.api, 10, 1, completed=completed), self.api.intent)
                    else:
                        with self.assertRaises(ValueError):
                            self.control.guard(self.api, 10, 1, completed=completed)
                self.assertTrue(all(method == "GET" for method, _ in self.api.calls))
                self.assertEqual(self.api.posts, 0)
        for invalid in (None, 1, "false"):
            with self.subTest(invalid=invalid), self.assertRaises(ValueError):
                self.control.guard(self.api, 10, 1, completed=invalid)

    def test_completed_guard_retains_canonical_source_and_ancestry_checks(self):
        for change in ("source", "tree", "ancestry", "missing-owner", "failed-normalize", "inherited-admission"):
            self.api = NativeAPI()
            self.complete_owner()
            if change == "source":
                self.api.intent["sources"] = {}
            elif change == "tree":
                self.api.intent["tree"] = "f" * 40
            elif change == "ancestry":
                self.api.reachable[MASTER].remove(SHA)
            elif change == "missing-owner":
                self.api.status_rows = []
            elif change == "failed-normalize":
                self.api.job_rows[0]["conclusion"] = "failure"
            else:
                self.api.job_rows[1].update(started_at="2026-11-26T15:56:46Z", completed_at="2026-11-26T15:56:46Z")
            with self.subTest(change=change), self.assertRaises(ValueError):
                self.control.guard(self.api, 10, 1, completed=True)
            self.assertEqual(self.api.posts, 0)

    def test_completed_retry_guard_keeps_actor_attempt_and_fresh_job_proofs(self):
        for event in ("workflow_dispatch", "push"):
            self.api = NativeAPI()
            self.complete_owner(2)
            if event == "push":
                self.api = NativeAPI()
                self.prepare_retry(event)
                self.api.native.update(status="completed", conclusion="failure")
                self.api.second_run.update(status="completed", conclusion="success")
            self.assertEqual(self.control.guard(self.api, 10, 2, completed=True), self.api.intent)
            with self.assertRaises(ValueError):
                self.control.guard(self.api, 10, 2)
            with self.assertRaises(ValueError):
                self.control.guard(self.api, 10, 1, completed=True)
            self.assertEqual(self.api.posts, 0)
        for change in ("old-attempt", "inherited-normalize", "inherited-admission", "other-attempt-admission", "unauthorized-actor"):
            self.api = NativeAPI()
            self.complete_owner(2)
            if change == "old-attempt":
                self.api.second_run["run_attempt"] = 3
            elif change == "unauthorized-actor":
                self.api.second_run["triggering_actor"] = self.api.permission["user"].copy()
                self.api.permission["permission"] = "read"
            else:
                job = self.api.second_jobs[0 if change == "inherited-normalize" else 1]
                if change == "other-attempt-admission":
                    job["run_attempt"] = 1
                else:
                    job.update(started_at="2026-11-26T15:56:48Z", completed_at="2026-11-26T15:56:52Z")
            with self.subTest(change=change), self.assertRaises(ValueError):
                self.control.guard(self.api, 10, 2, completed=True)
            self.assertEqual(self.api.posts, 0)

    def test_guard_rechecks_latest_state_after_validation_and_keeps_api_unknown(self):
        for completed in (False, True):
            self.api = NativeAPI()
            self.complete_owner()
            if not completed:
                self.api.native.update(status="in_progress", conclusion=None)
            original, reads = self.api.request, []
            def changed(method, path, data=None, binary=False):
                if path == self.api.prefix + "/actions/runs/10":
                    reads.append(path)
                    if len(reads) > 1:
                        return self.api.native | {"run_attempt": 2}
                return original(method, path, data, binary)
            self.api.request = changed
            with self.subTest(completed=completed), self.assertRaises(ValueError):
                self.control.guard(self.api, 10, 1, completed=completed)
            self.assertEqual(self.api.posts, 0)
            def unknown(method, path, data=None, binary=False):
                if path == self.api.prefix + "/actions/runs/10":
                    raise OSError("native outcome unknown")
                return original(method, path, data, binary)
            self.api.request = unknown
            with self.assertRaises(OSError):
                self.control.guard(self.api, 10, 1, completed=completed)

    def test_guard_independently_verifies_sources_and_same_attempt_admission(self):
        self.api.job_rows[1].update(status="completed", conclusion="success", completed_at="2026-11-26T15:56:54Z")
        self.assertEqual(self.control.guard(self.api, 10, 1), self.api.intent)
        self.api = NativeAPI()
        self.api.job_rows[1].update(status="completed", conclusion="success", completed_at="2026-11-26T15:56:54Z")
        self.api.trees["1" * 40][".github/scripts/release-core.py"] = "f" * 40
        with self.assertRaises(ValueError):
            self.control.guard(self.api, 10, 1)
        self.api = NativeAPI()
        self.api.job_rows[1].update(status="completed", conclusion="success", started_at="2026-11-26T15:56:46Z",
                                    completed_at="2026-11-26T15:56:46Z")
        with self.assertRaises(ValueError):
            self.control.guard(self.api, 10, 1)

    def test_whole_retry_guard_rejects_partial_native_job_inheritance(self):
        self.api.job_rows[1].update(status="completed", conclusion="success", completed_at="2026-11-26T15:56:54Z")
        self.api.second_run = self.api.native | {"run_attempt": 2, "run_started_at": "2026-11-26T16:00:00Z"}
        self.api.second_jobs = [row | {"run_attempt": 2} for row in self.api.job_rows]
        self.api.intent.update(run_attempt=2, allow_retry=False)
        self.api.artifact_time = "2026-11-26T16:00:03Z"
        with self.assertRaises(ValueError):
            self.control.guard(self.api, 10, 2)
        self.api.second_jobs = [
            self.api.job_rows[0] | {"id": 120, "run_attempt": 2, "started_at": "2026-11-26T16:00:01Z", "completed_at": "2026-11-26T16:00:04Z"},
            self.api.job_rows[1] | {"id": 121, "run_attempt": 2, "started_at": "2026-11-26T16:00:05Z", "completed_at": "2026-11-26T16:00:06Z"}]
        self.assertEqual(self.control.guard(self.api, 10, 2), self.api.intent)
        self.api.second_run["triggering_actor"] = {"id": 52, "login": "maintainer", "type": "User"}
        self.api.permission["permission"] = "read"
        with self.assertRaises(ValueError):
            self.control.guard(self.api, 10, 2)

    def test_owner_target_cannot_be_rebound_by_a_matching_receipt_alone(self):
        self.tag_run()
        self.api.native["head_sha"] = MASTER
        self.api.intent.update(workflow_sha=MASTER)
        with self.assertRaises((ValueError, OSError)):
            self.control.canonical_status(self.api, REPO, SHA)

    def test_new_independent_sha_owner_requires_no_archive_or_expired_receipt(self):
        self.api.status_rows, self.api.expired = [], True
        self.assertTrue(self.control.admit(self.api, REPO, SHA, 10, 1)["admitted"])
        self.assertEqual(self.api.posts, 1)
        self.assertEqual(self.control.canonical_status(self.api, REPO, SHA), 10)
        self.assertFalse(any("/artifacts" in path or "/actions/workflows/" in path for _, path in self.api.calls))

    def test_historical_tag_target_requires_independent_durable_owner(self):
        self.tag_run()
        self.api.tag = MASTER
        self.assertEqual(self.read(), self.api.intent)
        self.api.status_rows[0]["creator"]["id"] = 1
        with self.assertRaises(ValueError):
            self.read()

    def test_historical_tag_needs_no_live_ref_but_unknown_without_record_stops(self):
        self.tag_run()
        self.api.tag = OSError("tag ref unavailable")
        self.assertEqual(self.read(), self.api.intent)
        self.assertFalse(any("/git/ref/tags/" in path for _, path in self.api.calls))
        self.api.status_rows = []
        with self.assertRaises(OSError):
            self.read()

    def test_expired_intent_still_suppresses_same_target_without_old_evidence(self):
        self.api.expired = True
        self.assertEqual(self.control.canonical_status(self.api, REPO, SHA), 10)
        self.assertFalse(self.control.admit(self.api, REPO, SHA, 11, 1)["admitted"])
        self.assertFalse(any("/artifacts" in path for _, path in self.api.calls))
        self.assertEqual(self.api.posts, 0)
        self.api.native["head_branch"] = "feature/spoof"
        with self.assertRaises(ValueError):
            self.control.canonical_status(self.api, REPO, SHA)

    def test_legacy_static_admission_remains_compatible_with_verified_intent(self):
        self.api.job_rows[1].update(name="Release admission", status="completed", conclusion="success",
                                    completed_at="2026-11-26T15:56:54Z")
        self.assertEqual(self.control.guard(self.api, 10, 1), self.api.intent)
        self.api.intent["tree"] = "f" * 40
        with self.assertRaises(ValueError):
            self.control.canonical_status(self.api, REPO, SHA)

    def test_owner_without_sha_suffix_cannot_publish_expired_self_declarations(self):
        self.api.job_rows[1]["name"] = "Release admission"
        self.api.expired = True
        with self.assertRaises(ValueError):
            self.control.canonical_status(self.api, REPO, SHA)
        self.assertEqual(self.api.posts, 0)

    def prepare_retry(self, event="workflow_dispatch"):
        if event == "push":
            self.tag_run()
        self.api.job_rows[1].update(status="completed", conclusion="success", completed_at="2026-11-26T15:56:54Z")
        self.api.second_run = self.api.native | {"run_attempt": 2, "run_started_at": "2026-11-26T16:00:00Z"}
        self.api.second_jobs = [
            self.api.job_rows[0] | {"id": 120, "run_attempt": 2, "started_at": "2026-11-26T16:00:01Z", "completed_at": "2026-11-26T16:00:04Z"},
            self.api.job_rows[1] | {"id": 121, "run_attempt": 2, "started_at": "2026-11-26T16:00:05Z", "completed_at": "2026-11-26T16:00:06Z"}]
        self.api.intent.update(run_attempt=2, allow_retry=False)
        self.api.artifact_time = "2026-11-26T16:00:03Z"

    def test_authorized_whole_native_rerun_does_not_need_original_retry_input(self):
        for event in ["workflow_dispatch", "push"]:
            self.api = NativeAPI()
            self.prepare_retry(event)
            self.assertTrue(self.control.admit(self.api, REPO, SHA, 10, 2)["admitted"])
            self.assertEqual(self.control.guard(self.api, 10, 2), self.api.intent)
            self.api.second_run["triggering_actor"] = self.api.permission["user"].copy()
            self.assertEqual(self.control.guard(self.api, 10, 2), self.api.intent)
            self.assertEqual(self.api.posts, 0)
            full = self.api.job_rows[0] | {"id": 122, "name": "Release Full evidence", "run_attempt": 2}
            self.api.second_jobs.append(full)
            with self.assertRaises(ValueError):
                self.control.core.require_jobs(self.api.jobs(10, 2), 10, 2, ["Release Full evidence"])

    def test_rerun_actor_root_permission_and_active_attempt_are_native_proofs(self):
        for change in ["read", "wrong-permission-user", "missing-actor", "foreign-root", "stopped", "old-attempt", "changed-original"]:
            self.api = NativeAPI()
            self.prepare_retry()
            self.api.second_run["triggering_actor"] = self.api.permission["user"].copy()
            if change == "read":
                self.api.permission["permission"] = "read"
            elif change == "wrong-permission-user":
                self.api.permission["user"]["id"] = 1
            elif change == "missing-actor":
                self.api.second_run["triggering_actor"] = None
            elif change == "foreign-root":
                self.api.repo_info["full_name"] = "foreign/project"
            elif change == "stopped":
                self.api.second_run["status"] = "completed"
            elif change == "old-attempt":
                self.api.second_run["run_attempt"] = 3
            else:
                self.api.second_run["actor"] = {"id": 1, "login": "other", "type": "User"}
            with self.subTest(change=change), self.assertRaises(ValueError):
                self.control.guard(self.api, 10, 2)
            with self.subTest(change=change), self.assertRaises(ValueError):
                self.control.admit(self.api, REPO, SHA, 10, 2, True)

    def test_new_master_does_not_replace_running_controller_policy(self):
        newer = "f" * 40
        self.api.master = newer
        self.api.commits[newer] = {"sha": newer, "tree": {"sha": "3" * 40}, "parents": [{"sha": MASTER}]}
        self.api.reachable[newer] = {newer, MASTER, SHA, self.api.old}
        self.api.trees["3" * 40] = {path: "6" * 40 for path in self.api.paths}
        self.api.trees[TREE] = {path: "7" * 40 for path in self.api.paths}
        self.assertEqual(self.read(), self.api.intent)
        self.assertFalse(any("/git/trees/" + TREE in path or "/git/trees/" + "3" * 40 in path for _, path in self.api.calls))

    def test_guard_caches_only_immutable_controller_objects_not_run_or_jobs(self):
        self.api.job_rows[1].update(status="completed", conclusion="success", completed_at="2026-11-26T15:56:54Z")
        for _ in range(3):
            self.assertEqual(self.control.guard(self.api, 10, 1), self.api.intent)
        trees = [path for _, path in self.api.calls if "/git/trees/" in path]
        self.assertEqual(len(trees), len(set(trees)), "immutable trees were refetched in a tight poll")
        self.assertFalse(any("/git/trees/" + "1" * 40 + "?recursive=" in path for path in trees))
        self.api.native["status"] = "completed"
        with self.assertRaises(ValueError):
            self.control.guard(self.api, 10, 1)
        self.api.native["status"] = "in_progress"
        self.api.job_rows[1]["conclusion"] = "cancelled"
        with self.assertRaises(ValueError):
            self.control.guard(self.api, 10, 1)

    def test_normalize_binds_native_tag_ref_target_and_new_policy_receipt(self):
        def normalize(ref, workflow):
            event = {"repository": {"full_name": REPO}, "ref": ref, "created": True,
                     "deleted": False, "forced": False, "before": "0" * 40, "after": SHA}
            env = {"GITHUB_REPOSITORY": REPO, "GITHUB_RUN_ID": "10", "GITHUB_RUN_ATTEMPT": "1",
                   "GITHUB_EVENT_NAME": "push", "GITHUB_EVENT_PATH": "offline-event.json", "GITHUB_REF": ref,
                   "GITHUB_SHA": SHA, "GITHUB_WORKFLOW_SHA": workflow}
            planner = SimpleNamespace(shard_matrix=lambda: {"include": []})
            with patch.dict(self.control.os.environ, env, clear=True), \
                 patch.object(self.control.sys, "argv", ["release-control.py", "normalize", "--output", "offline-output"]), \
                 patch.object(self.control, "AdmissionAPI", return_value=self.api), \
                 patch.object(Path, "read_bytes", return_value=json.dumps(event).encode()), \
                 patch.object(self.control, "source_identity", return_value=self.api.blobs), \
                 patch.object(self.control.core, "load_script", return_value=planner), \
                 patch.object(self.control.core, "outputs"), patch.object(self.control.core, "save") as saved:
                self.control.main()
                return saved.call_args.args[1]
        self.tag_run()
        result = normalize("refs/tags/rc/test", SHA)
        self.assertEqual((result["policy_sha"], result["policy"]), (SHA, "caller-policy-v1"))
        self.assertNotIn("allow_retry", result)
        with self.assertRaises(ValueError):
            normalize("refs/tags/rc/other", SHA)
        self.api = NativeAPI()
        self.api.native.update(event="push", head_branch="rc/test")
        with self.assertRaises(ValueError):
            normalize("refs/tags/rc/test", MASTER)

    def test_new_policy_receipt_has_no_once_at_creation_retry_flag(self):
        self.api.intent.pop("allow_retry")
        self.assertEqual(self.read(), self.api.intent)

    def test_policy_sha_is_native_controller_not_candidate_or_receipt_choice(self):
        for change in [{"policy_sha": SHA}, {"policy": "candidate-policy"}]:
            self.api = NativeAPI()
            self.api.intent.update(change)
            with self.subTest(change=change), self.assertRaises(ValueError):
                self.read()

    def test_native_tag_name_and_raw_head_are_verified_independently(self):
        self.tag_run()
        self.api.native["head_branch"] = "rc/候选+构建"
        self.assertEqual(self.read(), self.api.intent)
        self.api.native["head_branch"] = "rc/x/y"
        with self.assertRaises(ValueError):
            self.read()
        self.api = NativeAPI()
        self.tag_run()
        outer = "e" * 40
        self.api.native["head_sha"] = outer
        self.api.tag_objects[outer] = {"sha": outer, "object": {"type": "commit", "sha": SHA}}
        self.assertEqual(self.read(), self.api.intent)
        self.api = NativeAPI()
        self.tag_run()
        self.api.native["head_sha"] = outer
        self.api.tag_objects[outer] = {"sha": outer, "object": {"type": "commit", "sha": MASTER}}
        with self.assertRaises(ValueError):
            self.read()

    def test_status_is_bound_to_first_native_admission_interval(self):
        for created in ["", "2026-11-26T15:56:52.999999Z"]:
            self.api = NativeAPI()
            self.api.status_rows[0]["created_at"] = created
            with self.subTest(created=created), self.assertRaises(ValueError):
                self.control.canonical_status(self.api, REPO, SHA)
        self.api = NativeAPI()
        self.api.job_rows[1].update(status="completed", conclusion="cancelled", completed_at="2026-11-26T15:56:54Z")
        self.assertEqual(self.control.canonical_status(self.api, REPO, SHA), 10)
        self.api.status_rows[0]["created_at"] = "2026-11-26T15:56:54.000001Z"
        with self.assertRaises(ValueError):
            self.control.canonical_status(self.api, REPO, SHA)


class ClaimAPI:
    def __init__(self):
        self.statuses = []
        self.started = []
        self.posts = 0
        self.uncertain = False
        self.commit_on_timeout = True
        self.retry_authorized = False

    def authorize_retry(self, run_id, attempt):
        if not self.retry_authorized:
            raise ValueError("native rerun actor is not authorized")

    def jobs(self, run_id, attempt):
        return [{"id": 1 + index, "run_id": run_id, "run_attempt": attempt, "name": name,
                 "status": "completed", "conclusion": "success"}
                for index, name in enumerate(["Release normalize", "Release admission " + SHA])]

    def statuses_for(self, sha):
        return copy.deepcopy(self.statuses)

    def check_owner(self, sha, status):
        return int(status["target_url"].rsplit("/", 1)[1])

    def prior_admissions(self, sha, run_id, attempt):
        return [item for item in self.started if item != (run_id, attempt)]

    def post_claim(self, sha, body):
        self.posts += 1
        if not self.uncertain or self.commit_on_timeout:
            self.statuses.append(body | {"id": self.posts, "creator": {"login": "github-actions[bot]", "id": 41898282, "type": "Bot"}})
        if self.uncertain:
            raise OSError("network outcome unknown")


class ClaimTests(unittest.TestCase):
    def setUp(self):
        self.control = load("release-control")
        self.api = ClaimAPI()

    def admit(self, run_id=10, attempt=1, retry=False):
        return self.control.admit(self.api, REPO, SHA, run_id, attempt, retry)

    def test_sequential_duplicate_after_any_owner_outcome_never_claims(self):
        self.assertTrue(self.admit()["admitted"])
        for conclusion in [None, "success", "failure", "cancelled", "timed_out"]:
            self.api.conclusion = conclusion
            result = self.admit(11)
            self.assertFalse(result["admitted"])
            self.assertEqual(result["owner_run_id"], 10)
            self.assertNotIn("verified", result)
        self.assertEqual(self.api.posts, 1)

    def test_duplicate_arrival_race_uses_one_serialized_durable_owner(self):
        lock, results = threading.Lock(), []
        def arrival(run):
            with lock:
                results.append(self.admit(run))
        threads = [threading.Thread(target=arrival, args=(run,)) for run in range(10, 22)]
        for thread in threads:
            thread.start()
        for thread in threads:
            thread.join()
        self.assertEqual(sum(row["admitted"] for row in results), 1)
        self.assertEqual(self.api.posts, 1)
        self.assertEqual(len({row["owner_run_id"] for row in results}), 1)

    def test_uncertain_post_is_only_reconciled_not_reposted(self):
        self.api.uncertain = True
        self.assertTrue(self.admit()["admitted"])
        self.assertEqual(self.api.posts, 1)
        self.api = ClaimAPI()
        self.api.uncertain, self.api.commit_on_timeout = True, False
        with self.assertRaisesRegex(ValueError, "不确定"):
            self.admit()
        # No owner was observable, so this invocation never admitted downstream work.
        with self.assertRaises(ValueError):
            self.admit(attempt=2, retry=True)
        self.assertEqual(self.api.posts, 1)

    def test_unrelated_expired_history_is_never_scanned_for_a_new_sha(self):
        def unavailable_history(*_):
            raise ValueError("unrelated intent expired among 1000 archived runs")
        self.api.prior_admissions = unavailable_history
        self.assertTrue(self.admit()["admitted"])
        self.assertEqual(self.api.posts, 1)

    def test_cancelled_admission_without_observable_owner_never_started_work(self):
        self.api.started.append((9, 1))
        self.assertTrue(self.admit()["admitted"])
        self.assertEqual(self.api.posts, 1)

    def test_new_owner_register_does_not_leave_pending_validation_status(self):
        self.assertTrue(self.admit()["admitted"])
        self.assertEqual(self.api.statuses[0]["state"], "success")
        self.assertIn("尚未验证", self.api.statuses[0]["description"])

    def test_same_run_retry_uses_native_authorization_not_creation_flag(self):
        self.admit()
        with self.assertRaises(ValueError):
            self.admit(attempt=2, retry=True)
        self.api.retry_authorized = True
        self.assertTrue(self.admit(attempt=2, retry=False)["admitted"])
        self.assertEqual(self.api.posts, 1)

    def test_original_run_attempt_duplicate_is_idempotent(self):
        self.assertTrue(self.admit()["admitted"])
        self.assertTrue(self.admit()["admitted"])
        self.assertEqual(self.api.posts, 1)

    def test_forged_or_replaced_first_status_is_not_an_owner(self):
        self.admit()
        first = self.api.statuses[0]
        for change in [{"creator": {"login": "github-actions[bot]", "id": 1, "type": "Bot"}},
                       {"state": "failure"}, {"target_url": "https://elsewhere.invalid/10"},
                       {"description": "validated"}]:
            self.api.statuses[0] = first | change
            with self.assertRaises(ValueError):
                self.admit(11)
        self.api.statuses[0] = first
        self.api.statuses.append(first | {"id": 2, "target_url": f"https://github.com/{REPO}/actions/runs/11"})
        with self.assertRaises(ValueError):
            self.admit(12)


class RequestTests(unittest.TestCase):
    class Response(io.BytesIO):
        def __init__(self, body, status=200):
            super().__init__(body)
            self.status = status

    def test_opaque_git_errors_stop_without_exposing_local_paths_or_command(self):
        core = load("release-core")
        error = core.subprocess.CalledProcessError(128, ["git", "private-path"], stderr=b"private source")
        with patch.object(core.subprocess, "check_output", side_effect=error):
            with self.assertRaisesRegex(OSError, "身份") as raised:
                core.git("private-path", "rev-parse", "HEAD")
        self.assertNotIn("private", str(raised.exception))

    def test_successful_204_is_none_and_200_is_json_without_reposting(self):
        core = load("release-core")
        api = core.GitHub(REPO, "never-log-this-token")
        with patch.object(api.opener, "open", return_value=self.Response(b"", 204)) as opened:
            self.assertIsNone(api.request("POST", api.prefix + "/statuses/" + SHA, {"state": "success"}))
            self.assertEqual(opened.call_count, 1)
        with patch.object(api.opener, "open", return_value=self.Response(b'{"id":5}')):
            self.assertEqual(api.get(api.prefix), {"id": 5})
        with patch.object(api.opener, "open", return_value=self.Response(b"")), self.assertRaises(ValueError):
            api.get(api.prefix)

    def test_http_status_is_useful_but_response_and_credentials_are_not_reported(self):
        core = load("release-core")
        api = core.GitHub(REPO, "never-log-this-token")
        for method, status, count in [("GET", 403, 1), ("GET", 503, 2), ("POST", 503, 1)]:
            error = urllib.error.HTTPError("https://api.github.com" + api.prefix, status,
                                           "private response never-log-this-token", {}, io.BytesIO(b"sensitive body"))
            with patch.object(api.opener, "open", side_effect=error) as opened, patch.object(core.time, "sleep"):
                with self.assertRaisesRegex(OSError, str(status)) as raised:
                    api.request(method, api.prefix + "/statuses/" + SHA)
                self.assertEqual(opened.call_count, count)
                for secret in ["never-log-this-token", "private response", "sensitive body"]:
                    self.assertNotIn(secret, str(raised.exception))
            error.close()


class AttemptAndArchiveTests(unittest.TestCase):
    def test_native_metadata_allows_repository_root_and_compares_real_timestamps(self):
        import io
        from unittest.mock import patch
        core = load("release-core")
        api = core.GitHub(REPO, "offline-token")
        class Response(io.BytesIO):
            def __enter__(self):
                return self
            def __exit__(self, *args):
                self.close()
        with patch.object(api.opener, "open", return_value=Response(b'{"owner":{"id":42}}')):
            try:
                self.assertEqual(api.get(api.prefix)["owner"]["id"], 42)
            except ValueError as error:
                self.fail("repository root GET must be allowed: " + str(error))
        artifact = {"id": 1, "name": "proof", "expired": False, "digest": "sha256:" + "0" * 64,
                    "workflow_run": {"id": 10}, "created_at": "2026-11-26T01:00:00.500Z", "updated_at": "2026-11-26T01:00:00.500Z"}
        api.artifacts = lambda run_id: [artifact]
        producer = {"run_id": 10, "run_attempt": 1, "status": "completed", "conclusion": "success",
                    "started_at": "2026-11-26T01:00:00Z", "completed_at": "2026-11-26T01:00:01Z"}
        self.assertEqual(api.artifact(10, "proof", producer)["id"], 1)
        with self.assertRaises(ValueError):
            api.artifact(10, "proof", producer | {"started_at": "2026-11-26T01:00:01Z"})

    def attempt_api(self, jobs, attempt=2, started="2026-11-26T15:56:47Z", previous=None):
        core = load("release-core")
        api = core.GitHub(REPO, "offline-only")
        run = {"id": 10, "run_attempt": attempt, "repository": {"full_name": REPO},
               "head_repository": {"full_name": REPO}, "run_started_at": started}
        def get(path):
            if path.endswith(f"/attempts/{attempt}"):
                return copy.deepcopy(run)
            if path.endswith(f"/attempts/{attempt - 1}"):
                return run | {"run_attempt": attempt - 1, "run_started_at": "2026-11-26T14:55:35Z"}
            if f"/attempts/{attempt}/jobs?" in path:
                return {"total_count": len(jobs), "jobs": copy.deepcopy(jobs)}
            if f"/attempts/{attempt - 1}/jobs?" in path:
                if isinstance(previous, Exception):
                    raise previous
                return {"total_count": len(previous or []), "jobs": copy.deepcopy(previous or [])}
            raise AssertionError(path)
        api.get = get
        return api

    def test_native_attempt_excludes_reissued_inherited_jobs_from_rest_sample(self):
        # Captured REST run 45843880742, attempt 2; embedded for portable offline tests.
        # All 28 completed rows were relabelled run_attempt=2 by the native API.
        observed = [
            (140027648923, "CI preflight and feedback", "14:55:36", "15:00:09"),
            (140027649393, "Full suite / Go platform worker 1", "15:00:11", "15:13:02"),
            (140027650141, "Full suite / Go platform worker 5", "15:00:12", "15:14:22"),
            (140027650224, "Full suite / Go store worker 9", "15:10:54", "15:34:47"),
            (140027650562, "Full suite / Go store worker 1", "15:00:11", "15:13:58"),
            (140027650565, "Full suite / Go store worker 0", "15:19:54", "15:32:20"),
            (140027651008, "Full suite / Go store worker 15", "15:14:24", "15:41:11"),
            (140027651011, "Full suite / Go store worker 3", "15:20:40", "15:31:58"),
            (140027651020, "Full suite / Go platform worker 3", "15:56:52", None),
            (140027651124, "Full suite / Go store worker 12", "15:13:03", "15:28:49"),
            (140027651419, "Full suite / Go store worker 14", "15:14:00", "15:29:42"),
            (140027651704, "Full suite / Go store worker 5", "15:28:50", "15:41:48"),
            (140027651956, "Full suite / Go store worker 4", "15:25:45", "15:37:19"),
            (140027652733, "Full suite / Go store worker 7", "15:32:00", "15:44:09"),
            (140027652796, "Full suite / Go store worker 8", "15:32:21", "15:38:55"),
            (140027652928, "Full suite / Go httpapi worker 0", "15:38:57", "15:48:35"),
            (140027652954, "Full suite / Go store worker 11", "15:37:21", "15:46:08"),
            (140027653002, "Full suite / Go store worker 6", "15:29:43", "15:37:56"),
            (140027653183, "Full suite / Go store worker 10", "15:34:49", "15:44:19"),
            (140027653773, "Full suite / Go httpapi worker 1", "15:41:13", "15:54:25"),
            (140027653972, "Full suite / Go store worker 13", "15:37:58", "15:50:01"),
            (140027696649, "Full suite / Go store worker 2", "15:08:54", "15:25:43"),
            (140027703448, "Full suite / Go vet and CI gates", "15:00:11", "15:03:43"),
            (140027711905, "Full suite / Go benchmarks", "15:00:11", "15:03:14"),
            (140027713031, "Full suite / Go platform worker 0", "15:00:11", "15:08:52"),
            (140027717159, "Full suite / Go ordinary and remaining race", "15:00:11", "15:18:00"),
            (140027717187, "Full suite / Go workspace race", "15:00:11", "15:16:14"),
            (140027717599, "Full suite / Go platform worker 2", "15:00:12", "15:19:52"),
            (140027719668, "Full suite / Go platform worker 4", "15:00:11", "15:20:38"),
        ]
        jobs = [{"id": identity, "name": name, "run_id": 10, "run_attempt": 2,
                 "status": "completed" if end else "in_progress", "conclusion": "success" if end else None,
                 "started_at": "2026-11-26T" + start + "Z", "completed_at": "2026-11-26T" + end + "Z" if end else None}
                for identity, name, start, end in observed]
        api = self.attempt_api(jobs)
        actual = api.jobs(10, 2)
        self.assertEqual([row["id"] for row in actual], [140027651020])
        with self.assertRaises(ValueError):
            load("release-core").require_jobs(actual, 10, 2, ["CI preflight and feedback"])

    def test_precise_intervals_keep_live_admission_but_not_empty_or_equal_completion(self):
        job = {"id": 20, "run_id": 10, "run_attempt": 2, "name": "Release admission",
               "status": "in_progress", "conclusion": None, "started_at": "2026-11-26T15:56:47.1Z", "completed_at": None}
        bad = [job | {"id": 21, "started_at": ""}, job | {"id": 22, "started_at": None},
               job | {"id": 23, "started_at": "2026-11-26T15:56:46.999999Z"},
               job | {"id": 24, "status": "completed", "completed_at": None},
               job | {"id": 25, "started_at": "2026-11-26T15:56:47Z", "status": "completed", "completed_at": "2026-11-26T15:56:47.000000Z"}]
        self.assertEqual([row["id"] for row in self.attempt_api(bad + [job]).jobs(10, 2)], [20])
        equal = job | {"started_at": "2026-11-26T15:56:47Z"}
        self.assertEqual(self.attempt_api([equal]).jobs(10, 2), [equal])
        self.assertEqual(self.attempt_api([equal], previous=[equal | {"run_attempt": 1}]).jobs(10, 2), [])
        self.assertEqual(self.attempt_api([equal], previous=[equal | {"id": 99, "run_attempt": 1}]).jobs(10, 2), [])
        with self.assertRaises(OSError):
            self.attempt_api([equal], previous=OSError("unknown previous attempt")).jobs(10, 2)
        first = equal | {"run_attempt": 1}
        self.assertEqual(self.attempt_api([first], attempt=1).jobs(10, 1), [first])

    def test_artifact_window_uses_instants_and_requires_completed_producer(self):
        core = load("release-core")
        api = core.GitHub(REPO, "offline-only")
        producer = {"id": 20, "run_id": 10, "run_attempt": 2, "status": "completed", "conclusion": "success",
                    "started_at": "2026-11-26T15:56:47Z", "completed_at": "2026-11-26T15:56:48Z"}
        row = {"id": 30, "name": "receipt", "expired": False, "digest": "sha256:" + "a" * 64,
               "workflow_run": {"id": 10}, "created_at": "2026-11-26T15:56:47.500000+00:00",
               "updated_at": "2026-11-26T15:56:48Z"}
        api.artifacts = lambda _: [row]
        self.assertEqual(api.artifact(10, "receipt", producer), row)
        for change in [{"updated_at": "2026-11-26T15:56:48.000001Z"}, {"created_at": ""},
                       {"updated_at": "2026-11-26T15:56:47.4Z"}]:
            original = row.copy()
            row.update(change)
            with self.subTest(change=change), self.assertRaises(ValueError):
                api.artifact(10, "receipt", producer)
            row.update(original)
        for change in [{"run_id": 11}, {"completed_at": None}, {"status": "in_progress"}]:
            with self.subTest(change=change), self.assertRaises(ValueError):
                api.artifact(10, "receipt", producer | change)

    def test_partial_or_cancelled_attempt_cannot_reuse_successful_jobs(self):
        core = load("release-core")
        jobs = [{"id": 1, "run_id": 10, "run_attempt": 2, "name": "Release admission", "status": "completed", "conclusion": "success"}]
        self.assertEqual(core.require_jobs(jobs, 10, 2, ["Release admission"])[0]["id"], 1)
        for changes in [{"run_attempt": 1}, {"conclusion": "cancelled"}, {"status": "in_progress"}]:
            with self.assertRaises(ValueError):
                core.require_jobs([jobs[0] | changes], 10, 2, ["Release admission"])
        with self.assertRaises(ValueError):
            core.require_jobs(jobs, 10, 2, ["Release admission", "Release normalize"])

    def test_archive_rejects_paths_and_checks_actual_bytes(self):
        import hashlib
        import io
        import zipfile
        core = load("release-core")
        def archive(entries):
            stream = io.BytesIO()
            with zipfile.ZipFile(stream, "w") as output:
                for name, body in entries.items():
                    output.writestr(name, body)
            return stream.getvalue()
        raw = archive({"receipt.json": b"{}", "workspace": b"binary"})
        digest = "sha256:" + hashlib.sha256(raw).hexdigest()
        self.assertEqual(core.unpack(raw, digest, {"receipt.json", "workspace"})["workspace"], b"binary")
        for bad, value in [(raw, "sha256:" + "0" * 64), (archive({"../receipt.json": b"{}"}), None)]:
            with self.assertRaises(ValueError):
                core.unpack(bad, value or "sha256:" + hashlib.sha256(bad).hexdigest(), {"receipt.json", "workspace"})


if __name__ == "__main__":
    program = unittest.main(exit=False)
    if program.result.wasSuccessful():
        print(f"离线契约测试通过：{program.result.testsRun} 项；未访问网络、执行候选二进制或调用 provider。", flush=True)
    raise SystemExit(0 if program.result.wasSuccessful() else 1)
