"""Offline rc tag contracts; all API responses and tags are local fixtures."""
import copy
from concurrent.futures import ThreadPoolExecutor
import hashlib
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import threading
import unittest
from unittest.mock import patch

sys.dont_write_bytecode = True
sys.path.insert(0, str(Path(__file__).resolve().parent))
from github import API, Reply, Unknown
from observer import Observer
from rc_refs import reconcile_tags
from store import Key, Store

REPO = "example/project"
ROOT = f"/repos/{REPO}"
REFS = ROOT + "/git/matching-refs/tags/rc/"
MASTER_REF = ROOT + "/git/ref/heads/master"
RELEASE = ".github/workflows/release-candidate.yml"
TARGET, MASTER, TAG, TAG2, TREE, GH_TREE, WF_TREE, BLOB = (c * 40 for c in "abcdef12")
COORDINATOR = "coordinator-test"


def reply(data, status=200, headers=None):
    return Reply(status, headers or {}, json.dumps(data).encode())


def ref(name="new", obj=TARGET, kind="commit"):
    return {"ref": "refs/tags/rc/" + name, "object": {"sha": obj, "type": kind}}


def ref_path(item):
    from urllib.parse import quote
    return ROOT + "/git/ref/" + quote(item["ref"][5:], safe="/")


def entry(path, obj, kind="tree", mode=None):
    return {"path": path, "sha": obj, "type": kind,
            "mode": mode or ("040000" if kind == "tree" else "100644")}


class World:
    def __init__(self):
        self.refs = []
        self.routes = {
            "/user": {"id": 1, "login": "operator"},
            MASTER_REF: {"ref": "refs/heads/master", "object": {"type": "commit", "sha": MASTER}},
            ROOT + "/git/commits/" + TARGET: {"sha": TARGET, "tree": {"sha": TREE}},
            ROOT + "/git/trees/" + TREE: {"sha": TREE, "truncated": False,
                                          "tree": [entry(".github", GH_TREE)]},
            ROOT + "/git/trees/" + GH_TREE: {"sha": GH_TREE, "truncated": False,
                                             "tree": [entry("workflows", WF_TREE)]},
            ROOT + "/git/trees/" + WF_TREE: {"sha": WF_TREE, "truncated": False, "tree": []},
        }
        self.calls = []
        self.overrides = {}
        self.relationship("ahead")

    def relationship(self, status):
        merge_base = TARGET if status in {"ahead", "identical"} else MASTER
        if status == "diverged":
            merge_base = BLOB
        self.routes[ROOT + f"/compare/{TARGET}...{MASTER}"] = {
            "status": status, "base_commit": {"sha": TARGET},
            "merge_base_commit": {"sha": merge_base},
            "ahead_by": int(status in {"ahead", "diverged"}),
            "behind_by": int(status in {"behind", "diverged"}),
        }

    def add(self, item=None):
        item = copy.deepcopy(item or ref())
        self.refs.append(item)
        self.routes[ref_path(item)] = item
        return item

    def native(self):
        self.routes[ROOT + "/git/trees/" + WF_TREE]["tree"] = [
            entry("release-candidate.yml", BLOB, "blob")]

    def __call__(self, path, etag=None):
        self.calls.append((path, etag))
        if path in self.overrides:
            value = self.overrides[path]
            if isinstance(value, Exception):
                raise value
            return value(path, etag) if callable(value) else value
        if path == REFS:
            data = self.refs
        elif path in self.routes:
            data = self.routes[path]
        else:
            return reply({}, 404)
        encoded = json.dumps(data, sort_keys=True).encode()
        current = '"' + hashlib.sha256(encoded).hexdigest() + '"'
        return Reply(304, {}, b"") if etag == current else Reply(200, {"etag": current}, encoded)


class Case(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.path = Path(self.tmp.name) / "observer.db"
        self.store = Store(self.path)
        self.store.configure(REPO, COORDINATOR)
        self.world = World()

    def tearDown(self):
        self.store.close()
        self.tmp.cleanup()

    def tick(self, deliver=True):
        with Observer(self.store, API(self.store, self.world)) as observer:
            return reconcile_tags(observer, deliver=deliver)

    def reopen(self):
        self.store.close()
        self.store = Store(self.path)

    def candidate(self, item=None):
        item = item or ref()
        return dict(self.store.db.execute(
            "SELECT * FROM rc_ref_candidates WHERE repo=? AND ref=? AND tag_object=?",
            (REPO, item["ref"], item["object"]["sha"])).fetchone())

    def payloads(self):
        return [row["payload"] for row in self.store.actions() if row["kind"] == "release_tag_forward"]

    def baseline_then_add(self, item=None):
        self.assertTrue(self.tick())
        return self.world.add(item)


class BaselineAndIdentity(Case):
    def test_first_complete_snapshot_is_a_durable_non_replaying_baseline(self):
        old = self.world.add(ref("old"))
        self.assertTrue(self.tick())
        self.reopen()
        self.assertTrue(self.tick())
        self.assertEqual(self.payloads(), [])
        self.assertEqual(self.candidate(old)["state"], "baseline")
        self.assertFalse(any("/git/ref/tags/" in path for path, _ in self.world.calls))
        new = self.world.add()
        self.assertTrue(self.tick())
        self.assertEqual([p["tag"] for p in self.payloads()], [new["ref"]])
        self.assertEqual(self.candidate(new)["state"], "forwarded")

    def test_empty_etag_baseline_then_new_tag_and_304_are_not_lost_or_duplicated(self):
        self.baseline_then_add()
        self.assertTrue(self.tick())
        receipt = self.store.inbox(COORDINATOR)[0]
        self.reopen()
        self.assertTrue(self.tick())
        self.assertTrue(self.tick())
        self.assertEqual(len(self.payloads()), 1)
        self.assertEqual([r["receipt_id"] for r in self.store.inbox(COORDINATOR)], [receipt["receipt_id"]])
        self.assertTrue(any(path == REFS and etag for path, etag in self.world.calls))
        payload = receipt["payload"]
        self.assertEqual(payload["repo"], REPO)
        self.assertEqual(payload["sha"], TARGET)
        self.assertEqual(payload["tag_object"], TARGET)
        self.assertEqual(payload["action"], "release_tag_forward")
        self.assertEqual(payload["job"], f"@tag:refs/tags/rc/new:{TARGET}")
        self.assertTrue(payload["native_workflow_absent"])
        proof = payload["native_workflow_absence"]
        self.assertTrue(proof["verified"])
        self.assertEqual((proof["sha"], proof["tree"], proof["path"]), (TARGET, TREE, RELEASE))
        self.assertEqual(payload["master_sha"], MASTER)
        for field in ("run_id", "attempt", "tested_sha"):
            self.assertIn(field, payload)
            self.assertIsNone(payload[field])
        self.assertIsInstance(payload["title"], str)

    def test_only_single_level_push_pattern_matches_and_unicode_is_quoted(self):
        self.assertTrue(self.tick())
        self.world.add(ref("nested/ignored"))
        self.world.add(ref("候选-1"))
        self.assertTrue(self.tick())
        self.assertEqual([p["tag"] for p in self.payloads()], ["refs/tags/rc/候选-1"])
        self.assertTrue(any("%E5%80%99" in path for path, _ in self.world.calls))

    def test_same_ref_new_original_object_is_a_new_identity_even_for_same_commit(self):
        self.baseline_then_add()
        self.assertTrue(self.tick())
        item = ref(obj=TAG, kind="tag")
        self.world.refs = []
        self.world.add(item)
        self.world.routes[ROOT + "/git/tags/" + TAG] = {"sha": TAG, "object": {"type": "commit", "sha": TARGET}}
        self.assertTrue(self.tick())
        self.world.refs = [ref()]
        self.world.routes[ref_path(ref())] = ref()
        self.assertTrue(self.tick())
        self.assertEqual({p["tag_object"] for p in self.payloads()}, {TARGET, TAG})
        self.assertEqual(len(self.store.inbox(COORDINATOR)), 2)

    def test_offline_create_then_delete_is_not_claimed_observed(self):
        self.assertTrue(self.tick())
        self.reopen()
        self.assertTrue(self.tick())
        self.assertEqual(self.payloads(), [])
        self.assertEqual(self.store.db.execute("SELECT count(*) FROM rc_ref_candidates").fetchone()[0], 0)

    def test_annotated_tags_are_recursively_peeled_and_exact_sha_checked(self):
        self.baseline_then_add(ref(obj=TAG, kind="tag"))
        self.world.routes[ROOT + "/git/tags/" + TAG] = {"sha": TAG, "object": {"type": "tag", "sha": TAG2}}
        self.world.routes[ROOT + "/git/tags/" + TAG2] = {"sha": TAG2, "object": {"type": "commit", "sha": TARGET}}
        self.assertTrue(self.tick())
        self.assertEqual(self.payloads()[0]["sha"], TARGET)
        self.assertEqual(self.payloads()[0]["tag_object"], TAG)

    def test_tag_target_equal_to_master_is_confirmed_by_compare(self):
        self.baseline_then_add()
        self.world.routes[MASTER_REF]["object"]["sha"] = TARGET
        self.world.routes[ROOT + f"/compare/{TARGET}...{TARGET}"] = {
            "status": "identical", "base_commit": {"sha": TARGET},
            "merge_base_commit": {"sha": TARGET}, "ahead_by": 0, "behind_by": 0}
        self.assertTrue(self.tick())
        self.assertEqual(len(self.payloads()), 1)

    def test_native_workflow_is_not_forwarded_or_replayed(self):
        self.baseline_then_add()
        self.world.native()
        self.assertTrue(self.tick())
        self.reopen()
        self.assertTrue(self.tick())
        self.assertEqual(self.payloads(), [])
        self.assertEqual(self.candidate()["state"], "native_workflow")

    def test_known_outside_master_is_retained_and_retried_after_merge(self):
        self.baseline_then_add()
        for status in ("behind", "diverged"):
            self.world.relationship(status)
            self.assertTrue(self.tick())
            self.assertEqual(self.candidate()["state"], "outside_master")
            self.assertEqual(self.payloads(), [])
        self.reopen()
        self.world.relationship("ahead")
        self.assertTrue(self.tick())
        self.assertEqual(len(self.payloads()), 1)


class UnknownAndCompleteness(Case):
    def test_api_errors_do_not_initialize_baseline_or_deliver_pending(self):
        for status in (401, 403, 404, 409, 422, 500, 503):
            with self.subTest(status=status):
                self.world.overrides[REFS] = reply({}, status)
                self.assertFalse(self.tick())
                self.assertEqual(self.store.db.execute("SELECT count(*) FROM rc_ref_baselines").fetchone()[0], 0)
        self.world.overrides.clear()
        self.baseline_then_add()
        self.assertTrue(self.tick(deliver=False))
        for endpoint in (REFS, "/user", ref_path(ref())):
            self.world.overrides[endpoint] = reply({}, 403)
            self.assertFalse(self.tick())
            self.assertEqual(self.store.inbox(COORDINATOR), [])
            self.world.overrides.clear()
        self.assertTrue(self.tick())
        self.assertEqual(len(self.store.inbox(COORDINATOR)), 1)

    def test_incomplete_content_and_duplicate_refs_never_establish_a_baseline(self):
        malformed = [reply({"message": "not a refs list"}), Reply(200, {}, b"["),
                     reply([ref(), {}]), reply([ref(), ref()]),
                     reply([ref("invalid..name")]), reply([ref(obj="not-a-sha")])]
        for response in malformed:
            with self.subTest(body=response.body):
                self.world.overrides[REFS] = response
                self.assertFalse(self.tick())
                self.assertEqual(self.store.db.execute("SELECT count(*) FROM rc_ref_baselines").fetchone()[0], 0)
                self.assertEqual(self.store.db.execute("SELECT count(*) FROM rc_ref_candidates").fetchone()[0], 0)
        self.world.overrides.clear()
        self.assertTrue(self.tick())

    def test_partial_pagination_does_not_become_baseline_and_complete_pages_are_cached(self):
        page2 = REFS + "?page=2"
        old1, old2 = ref("old-1"), ref("old-2")
        self.world.overrides[REFS] = reply([old1], headers={"etag": '"first"',
            "link": f'<https://api.github.com{page2}>; rel="next"'})
        self.world.overrides[page2] = reply({}, 503)
        self.assertFalse(self.tick())
        self.assertEqual(self.store.db.execute("SELECT count(*) FROM rc_ref_baselines").fetchone()[0], 0)
        self.world.overrides[REFS] = Reply(304, {}, b"")
        self.world.overrides[page2] = reply([old2], headers={"etag": '"second"'})
        self.assertTrue(self.tick())
        self.assertEqual(self.candidate(old1)["state"], "baseline")
        self.assertEqual(self.candidate(old2)["state"], "baseline")
        self.assertIn((REFS, '"first"'), self.world.calls)
        self.world.overrides[page2] = Reply(304, {}, b"")
        self.reopen()
        self.assertTrue(self.tick())
        self.assertIn((page2, '"second"'), self.world.calls)
        self.assertEqual(self.payloads(), [])

    def test_invalid_pagination_links_and_partial_page_shapes_stay_unknown(self):
        links = ["https://evil.invalid/steal", "https://api.github.com" + REFS,
                 "https://api.github.com" + REFS + "?page=3", "https://[broken/",
                 "https://api.github.com" + REFS + "?page=2&token=secret",
                 "https://api.github.com" + REFS + "?page=2&extra="]
        for link in links:
            self.world.overrides[REFS] = reply([ref()], headers={"link": f'<{link}>; rel="next"'})
            self.assertFalse(self.tick())
        for link in ("broken", f'<https://api.github.com{REFS}?page=2>; rel="unknown"'):
            self.world.overrides[REFS] = reply([], headers={"link": link})
            self.assertFalse(self.tick())
        self.assertEqual(self.store.db.execute("SELECT count(*) FROM rc_ref_baselines").fetchone()[0], 0)

    def test_timeout_and_unknown_candidate_survive_restart_and_304_listing(self):
        item = self.baseline_then_add()
        self.world.overrides[ref_path(item)] = Unknown("本地 fixture 超时")
        self.assertFalse(self.tick())
        self.assertEqual(self.candidate()["state"], "unknown")
        self.reopen()
        self.world.overrides.clear()
        self.assertTrue(self.tick())
        self.assertEqual(len(self.payloads()), 1)

    def test_ref_original_object_or_type_mismatch_never_dispatches(self):
        item = self.baseline_then_add()
        for replacement in (ref(obj=TAG), ref(kind="tag"), dict(ref(), ref="refs/tags/rc/forged")):
            self.world.routes[ref_path(item)] = replacement
            self.assertFalse(self.tick())
            self.assertEqual(self.payloads(), [])
        self.world.routes[ref_path(item)] = item
        self.assertTrue(self.tick())
        self.assertEqual(len(self.payloads()), 1)

    def test_ref_and_master_are_rechecked_before_publish_or_delivery(self):
        item = self.baseline_then_add()
        original = self.world.__call__
        reads = 0

        def moving(path, etag=None):
            nonlocal reads
            if path == ref_path(item):
                reads += 1
                return reply(item if reads == 1 else ref(obj=TAG))
            return original(path, etag)

        with Observer(self.store, API(self.store, moving)) as observer:
            self.assertFalse(reconcile_tags(observer))
        self.assertEqual(self.payloads(), [])
        self.world.overrides[MASTER_REF] = reply({"ref": "refs/heads/master", "object": {"type": "commit", "sha": MASTER}})
        master_reads = 0

        def moving_master(path, etag=None):
            nonlocal master_reads
            if path == MASTER_REF:
                master_reads += 1
                return reply({"ref": "refs/heads/master", "object": {"type": "commit", "sha": MASTER if master_reads == 1 else TAG}})
            return original(path, etag)

        with Observer(self.store, API(self.store, moving_master)) as observer:
            self.assertFalse(reconcile_tags(observer))
        self.assertEqual(self.payloads(), [])
        self.world.overrides.clear()
        self.assertTrue(self.tick(deliver=False))
        self.world.routes[ref_path(item)] = ref(obj=TAG)
        self.assertFalse(self.tick())
        self.assertEqual(self.store.inbox(COORDINATOR), [])
        self.world.routes[ref_path(item)] = item
        self.assertTrue(self.tick())

    def test_absent_or_replaced_candidate_is_retained_and_can_reappear(self):
        self.baseline_then_add()
        self.world.overrides[ref_path(ref())] = reply({}, 404)
        self.assertFalse(self.tick())
        self.world.refs = []
        self.world.overrides.clear()
        self.assertTrue(self.tick())
        self.assertEqual(self.candidate()["state"], "not_current")
        self.assertEqual(self.payloads(), [])
        self.world.refs = [ref()]
        self.assertTrue(self.tick())
        self.assertEqual(len(self.payloads()), 1)

    def test_tag_cycle_invalid_type_identity_and_depth_are_unknown_not_consumed(self):
        item = self.baseline_then_add(ref(obj=TAG, kind="tag"))
        endpoint = ROOT + "/git/tags/" + TAG
        for body in ({"sha": TAG, "object": {"type": "tag", "sha": TAG}},
                     {"sha": TAG2, "object": {"type": "commit", "sha": TARGET}},
                     {"sha": TAG, "object": {"type": "tree", "sha": TREE}}):
            self.world.routes[endpoint] = body
            self.assertFalse(self.tick())
            self.assertEqual(self.candidate(item)["state"], "unknown")
            self.assertEqual(self.payloads(), [])
        for index in range(100):
            current = TAG if index == 0 else f"{index:040x}"
            self.world.routes[ROOT + "/git/tags/" + current] = {
                "sha": current, "object": {"type": "tag", "sha": f"{index + 1:040x}"}}
        self.assertFalse(self.tick())
        self.assertLess(len([p for p, _ in self.world.calls if "/git/tags/" in p]), 80)
        self.world.routes[endpoint] = {"sha": TAG, "object": {"type": "commit", "sha": TARGET}}
        self.assertTrue(self.tick())
        self.assertEqual(len(self.payloads()), 1)

    def test_tree_404_truncation_wrong_identity_and_nonfiles_are_never_absence(self):
        self.baseline_then_add()
        endpoint = ROOT + "/git/trees/" + WF_TREE
        invalid = [reply({}, 404), reply({"sha": WF_TREE, "tree": [], "truncated": True}),
                   reply({"sha": BLOB, "tree": [], "truncated": False}),
                   reply({"sha": WF_TREE, "tree": []}),
                   reply({"sha": WF_TREE, "truncated": False, "tree": [entry("release-candidate.yml", BLOB, "blob", "120000")]}),
                   reply({"sha": WF_TREE, "truncated": False, "tree": [entry("release-candidate.yml", BLOB)]}),
                   reply({"sha": WF_TREE, "truncated": False, "tree": [entry("other", BLOB, "blob")] * 2}),
                   reply({"sha": WF_TREE, "truncated": False, "tree": [entry("dir/file", BLOB, "blob")]})]
        for response in invalid:
            with self.subTest(body=response.body):
                self.world.overrides[endpoint] = response
                self.assertFalse(self.tick())
                self.assertEqual(self.candidate()["state"], "unknown")
                self.assertEqual(self.payloads(), [])
        self.world.overrides.clear()
        self.assertTrue(self.tick())
        self.assertEqual(len(self.payloads()), 1)

    def test_malformed_nested_fields_are_unknown_not_uncaught_exceptions(self):
        self.baseline_then_add()
        tree_path = ROOT + "/git/trees/" + WF_TREE
        malformed = [
            (ref_path(ref()), dict(ref(), object={"sha": TARGET, "type": []})),
            (ref_path(ref()), dict(ref(), object={"sha": TARGET, "type": {}})),
            (tree_path, {"sha": WF_TREE, "truncated": False,
                         "tree": [dict(entry("other", BLOB, "blob"), mode=[])]}),
            (tree_path, {"sha": WF_TREE, "truncated": False,
                         "tree": [dict(entry("other", BLOB), type=[])]}),
            (tree_path, {"sha": WF_TREE, "truncated": False,
                         "tree": [entry("invalid\u0000name", BLOB, "blob")]}),
        ]
        for endpoint, body in malformed:
            with self.subTest(body=body):
                self.world.overrides = {endpoint: reply(body)}
                self.assertFalse(self.tick())
                self.assertEqual(self.candidate()["state"], "unknown")
                self.assertEqual(self.payloads(), [])
        self.world.overrides.clear()
        self.assertTrue(self.tick())
        self.assertEqual(len(self.payloads()), 1)

    def test_verified_missing_parent_directory_also_proves_absence(self):
        self.baseline_then_add()
        self.world.routes[ROOT + "/git/trees/" + TREE]["tree"] = []
        self.assertTrue(self.tick())
        self.assertEqual(len(self.payloads()), 1)
        self.assertFalse(any(path.endswith("/git/trees/" + GH_TREE) for path, _ in self.world.calls))

    def test_compare_incomplete_or_conflicting_identity_stays_unknown(self):
        self.baseline_then_add()
        endpoint = ROOT + f"/compare/{TARGET}...{MASTER}"
        good = copy.deepcopy(self.world.routes[endpoint])
        for body in ({"status": "ahead"}, dict(good, base_commit={"sha": TAG}),
                     dict(good, merge_base_commit={"sha": TAG}), dict(good, status="unknown")):
            self.world.routes[endpoint] = body
            self.assertFalse(self.tick())
            self.assertEqual(self.payloads(), [])
        self.world.routes[endpoint] = good
        self.assertTrue(self.tick())


class DurabilityAndDelivery(Case):
    def crash_child(self, phase):
        code = """
import os, sys
from test_rc_refs import World, REPO
from store import Store
from github import API
from observer import Observer
from rc_refs import reconcile_tags
store = Store(sys.argv[1])
world = World()
item = world.add()
if sys.argv[2] == 'after_publish':
    original = store.publish
    def publish(*args, **kwargs):
        original(*args, **kwargs)
        os._exit(17)
    store.publish = publish
elif sys.argv[2] == 'after_inbox':
    import rc_refs
    original = rc_refs.source_delivery
    def delivery(*args, **kwargs):
        original(*args, **kwargs)
        os._exit(23)
    rc_refs.source_delivery = delivery
else:
    from test_rc_refs import ref_path
    world.overrides[ref_path(item)] = lambda *args: os._exit(19)
with Observer(store, API(store, world)) as observer:
    reconcile_tags(observer)
"""
        return subprocess.run([sys.executable, "-B", "-c", code, str(self.path), phase],
                              cwd=Path(__file__).resolve().parent, timeout=15, capture_output=True)

    def test_kill_after_candidate_commit_rechecks_candidate_after_restart(self):
        self.assertTrue(self.tick())
        result = self.crash_child("after_seen")
        self.assertEqual(result.returncode, 19, result.stderr.decode())
        self.reopen()
        self.assertEqual(self.candidate()["state"], "pending")
        self.world.add()
        self.assertTrue(self.tick())
        self.assertEqual(len(self.payloads()), 1)
        self.assertEqual(len(self.store.inbox(COORDINATOR)), 1)

    def test_kill_after_outbox_commit_recovers_without_duplicate_action_or_receipt(self):
        self.assertTrue(self.tick())
        result = self.crash_child("after_publish")
        self.assertEqual(result.returncode, 17, result.stderr.decode())
        self.reopen()
        self.assertEqual(len(self.payloads()), 1)
        self.assertEqual(self.store.inbox(COORDINATOR), [])
        self.assertEqual(self.candidate()["sha"], TARGET)
        action_id = self.store.actions()[0]["id"]
        self.world.add()
        self.assertTrue(self.tick())
        self.assertTrue(self.tick())
        self.assertEqual([row["id"] for row in self.store.actions()], [action_id])
        self.assertEqual(len(self.store.inbox(COORDINATOR)), 1)

    def test_kill_after_inbox_commit_does_not_redeliver_on_restart(self):
        self.assertTrue(self.tick())
        result = self.crash_child("after_inbox")
        self.assertEqual(result.returncode, 23, result.stderr.decode())
        self.reopen()
        receipt = self.store.inbox(COORDINATOR)[0]
        self.world.add()
        self.assertTrue(self.tick())
        self.assertEqual([row["receipt_id"] for row in self.store.inbox(COORDINATOR)], [receipt["receipt_id"]])
        self.assertEqual(self.candidate()["state"], "forwarded")
        self.assertEqual(len(self.payloads()), 1)

    def test_same_ref_object_cannot_publish_again_for_conflicting_peel_target(self):
        item = self.baseline_then_add(ref(obj=TAG, kind="tag"))
        tag_path = ROOT + "/git/tags/" + TAG
        self.world.routes[tag_path] = {"sha": TAG, "object": {"type": "commit", "sha": TARGET}}
        self.assertTrue(self.tick(deliver=False))
        self.world.routes[tag_path]["object"]["sha"] = MASTER
        self.world.routes[ROOT + "/git/commits/" + MASTER] = {"sha": MASTER, "tree": {"sha": TREE}}
        self.world.routes[ROOT + f"/compare/{MASTER}...{MASTER}"] = {
            "status": "identical", "base_commit": {"sha": MASTER},
            "merge_base_commit": {"sha": MASTER}, "ahead_by": 0, "behind_by": 0}
        self.assertFalse(self.tick())
        self.assertEqual(len(self.payloads()), 1)
        self.assertEqual(self.candidate(item)["state"], "unknown")
        self.assertEqual(self.store.inbox(COORDINATOR), [])
        self.world.routes[tag_path]["object"]["sha"] = TARGET
        self.assertTrue(self.tick())
        self.assertEqual(len(self.payloads()), 1)
        self.assertEqual(len(self.store.inbox(COORDINATOR)), 1)

    def test_delivery_is_scoped_to_just_verified_ref_object_not_run_or_other_tag(self):
        self.baseline_then_add()
        self.assertTrue(self.tick(deliver=False))
        first = self.world.refs[0]
        second = self.world.add(ref("second"))
        self.assertTrue(self.tick(deliver=False))
        unrelated = Key(REPO, TARGET, 91, 1, "501")
        self.store.publish(unrelated, "first_failure", {"tested_sha": None}, [COORDINATOR])
        self.world.overrides[ref_path(first)] = reply({}, 403)
        self.assertFalse(self.tick())
        inbox = self.store.inbox(COORDINATOR)
        self.assertEqual([row["payload"]["tag"] for row in inbox], [second["ref"]])
        pending = [row for row in self.store.outbox() if row["state"] == "pending"]
        self.assertEqual({row["payload"]["action"] for row in pending}, {"first_failure", "release_tag_forward"})
        self.world.overrides.clear()
        self.assertTrue(self.tick())
        self.assertEqual(len(self.store.inbox(COORDINATOR)), 2)

    def test_store_nullable_tag_identity_wait_ack_and_restart_are_compatible(self):
        self.baseline_then_add()
        self.assertTrue(self.tick())
        receipt = self.store.inbox(COORDINATOR)[0]
        selector = {k: receipt["payload"][k] for k in ("repo", "sha", "run_id", "attempt", "action")}
        self.assertEqual(self.store.wait(COORDINATOR, selector, timeout=0)["receipt_id"], receipt["receipt_id"])
        self.store.ack(COORDINATOR, receipt["receipt_id"])
        self.reopen()
        self.assertTrue(self.tick())
        self.assertEqual(len(self.store.inbox(COORDINATOR)), 1)
        self.assertEqual(self.store.inbox(COORDINATOR)[0]["state"], "acked")
        self.assertEqual(self.store.db.execute("SELECT count(*) FROM waits").fetchone()[0], 0)

    def test_existing_receiver_lock_prevents_concurrent_tag_reconciler(self):
        self.baseline_then_add()
        entered, attempted = threading.Event(), threading.Event()

        def holder():
            store = Store(self.path)
            world = World()
            world.add()
            try:
                with Observer(store, API(store, world)) as observer:
                    entered.set()
                    self.assertTrue(attempted.wait(5))
                    return reconcile_tags(observer)
            finally:
                store.close()

        with ThreadPoolExecutor(max_workers=1) as pool:
            task = pool.submit(holder)
            self.assertTrue(entered.wait(5))
            try:
                with self.assertRaises(RuntimeError):
                    self.tick()
            finally:
                attempted.set()
            self.assertTrue(task.result(timeout=10))
        self.assertTrue(self.tick())
        self.assertEqual(len(self.payloads()), 1)
        self.assertEqual(len(self.store.inbox(COORDINATOR)), 1)

    def test_lock_is_required_and_no_unconditional_delivery_is_used(self):
        observer = Observer(self.store, API(self.store, self.world))
        with self.assertRaises(RuntimeError):
            reconcile_tags(observer)
        self.baseline_then_add()
        with patch.object(self.store, "deliver", side_effect=AssertionError("Unscoped delivery")):
            self.assertTrue(self.tick())
        self.assertEqual(len(self.store.inbox(COORDINATOR)), 1)


if __name__ == "__main__":
    unittest.main()
