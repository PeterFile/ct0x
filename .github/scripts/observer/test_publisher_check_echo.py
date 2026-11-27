"""Offline publisher-job normalization using synchronized #273 REST projections.

No Observer, SQLite or file locks are started here. Missing native issuer metadata
stays unknown; the cross-publisher success case explicitly supplies a synthetic
issuer, never claims hosted authentication, and still cannot route a result.
"""
import copy
import hashlib
import json
import os
from pathlib import Path
import sys
from types import SimpleNamespace
import unittest

sys.dont_write_bytecode = True
sys.path.insert(0, str(Path(__file__).resolve().parent))
from github import Unknown
from merge_reports import reconcile_publisher

REPO = "PeterFile/ct0x"
ROOT = "/repos/" + REPO
OWN_ECHO, OTHER_ECHO = 140474151181, 140474409168
OTHER_ISSUER = 45989699759


def native_rows():
    fixture = json.loads(Path(__file__).with_name("publisher_check_echo_v2.json").read_text())
    location = os.environ.get("OBSERVER_MERGE_EVIDENCE_ROOT")
    if location is None:
        return fixture["run"], fixture["jobs"], fixture["checks"]
    if not location.strip():
        raise AssertionError("OBSERVER_MERGE_EVIDENCE_ROOT must name the archived GitHub directory")
    documents = {}
    for name, digest in fixture["provenance"]["files"].items():
        data = (Path(location) / name).read_bytes()
        if hashlib.sha256(data).hexdigest() != digest:
            raise AssertionError("Archived publisher echo digest mismatch: " + name)
        documents[name] = json.loads(data)
    run, = [row for row in documents["pr-273-synced-native-runs.json"]["workflow_runs"]
            if row["id"] == fixture["run"]["id"]]
    jobs = documents["pr-273-synced-45989633708-jobs.json"]["jobs"]
    checks = [row for row in documents["pr-273-synced-check-rows-1924.json"]["check_runs"] if row["name"] == "Merge gate"]

    def matches(projected, actual):
        if isinstance(projected, dict):
            return isinstance(actual, dict) and all(key in actual and matches(value, actual[key]) for key, value in projected.items())
        if isinstance(projected, list):
            return isinstance(actual, list) and len(projected) == len(actual) and all(matches(a, b) for a, b in zip(projected, actual))
        return type(projected) is type(actual) and projected == actual

    if not all(matches(fixture[key], actual) for key, actual in (("run", run), ("jobs", jobs), ("checks", checks))):
        raise AssertionError("Bounded publisher echo projection differs from native responses")
    return run, jobs, checks


class PublisherCheckEcho(unittest.TestCase):
    def world(self, synthetic_issuer=True):
        run, jobs, checks = native_rows()
        problems, metadata, calls = {}, {}, []
        issuers = {run["id"]: run}
        if synthetic_issuer:
            # Only the Check summary was archived for this issuer. This models a
            # separately authenticated native run; it is NOT a hosted response.
            issuers[OTHER_ISSUER] = dict(run, id=OTHER_ISSUER,
                html_url=f"https://github.com/{REPO}/actions/runs/{OTHER_ISSUER}")
        member = json.loads(checks[0]["output"]["summary"])["membership"][0]
        # An open PR snapshot for exercising real matrix/current-Check ownership,
        # not evidence of live PR state or a successful tested target.
        pr = dict(number=member["pr"], state="open", merged=False, merged_at=None, draft=member["draft"],
            html_url=f"https://github.com/{REPO}/pull/{member['pr']}",
            head=dict(sha=member["head"], ref=member["head_ref"], repo=run["head_repository"]),
            base=dict(sha=member["base"], ref=member["base_ref"], repo=run["repository"]))
        responses = {
            ROOT + "/actions/workflows/merge-conflict.yml": dict(id=run["workflow_id"], path=run["path"], state="active"),
            ROOT + "/git/ref/heads/master": dict(ref="refs/heads/master", object=dict(type="commit", sha=member["base"])),
            ROOT + "/pulls/273": pr,
            ROOT + f"/commits/{run['head_sha']}/check-runs?filter=all&per_page=100&page=1":
                dict(total_count=len(checks), check_runs=checks)}
        for identity, issuer in issuers.items():
            responses[ROOT + f"/actions/runs/{identity}"] = issuer
            responses[ROOT + f"/actions/runs/{identity}/attempts/1"] = issuer
        for check in checks:
            responses[ROOT + f"/check-runs/{check['id']}"] = check

        def get(path):
            calls.append(path)
            if path not in responses:
                raise Unknown("离线 fixture 缺少认证响应，保持未知: " + path)
            return copy.deepcopy(responses[path])

        def problem(scope, reason=None):
            if reason is None:
                problems.pop(scope, None)
            else:
                problems[scope] = reason

        store = SimpleNamespace(problem=problem, meta_get=metadata.get, meta_put=metadata.__setitem__)
        observer = SimpleNamespace(repo=REPO, root=ROOT, lock=object(), store=store,
            api=SimpleNamespace(get=get, document=lambda path: (get(path), None)))
        return SimpleNamespace(run=run, jobs=jobs, checks=checks, issuers=issuers, responses=responses,
            problems=problems, metadata=metadata, calls=calls, observer=observer, scope=f"merge-report:{run['id']}:1")

    def reconcile(self, world):
        original = copy.deepcopy((world.run, world.jobs, world.checks))
        self.assertFalse(reconcile_publisher(world.observer, world.run, world.jobs))
        self.assertEqual((world.run, world.jobs, world.checks), original)

    def test_authenticated_archived_rows_are_not_publisher_candidates(self):
        world = self.world()
        self.reconcile(world)
        self.assertEqual(world.problems, {world.scope + ":pr:273": "此来源的 Merge gate 不属于本次 publisher run/attempt"})
        self.assertEqual([job["id"] for job in world.jobs], [140473895156, 140473962124, OWN_ECHO, OTHER_ECHO])
        for identity in (OWN_ECHO, OTHER_ECHO):
            self.assertIn(ROOT + f"/check-runs/{identity}", world.calls)
        self.assertIn(ROOT + f"/actions/runs/{OTHER_ISSUER}", world.calls)
        self.assertEqual(world.metadata[f"merge-report-source:{REPO}:{world.run['id']}:1:273"]["state"], "waiting")

    def test_unarchived_issuer_remains_unknown_without_hiding_the_authenticated_own_echo(self):
        world = self.world(synthetic_issuer=False)
        self.reconcile(world)
        self.assertNotIn(world.scope + ":job:2", world.problems)
        self.assertIn("离线 fixture 缺少认证响应", world.problems[world.scope + ":job:3"])
        self.assertEqual(world.problems[world.scope + ":pr:273"], "此来源的 Merge gate 不属于本次 publisher run/attempt")

    def test_pending_own_check_does_not_authorize_a_terminal_report(self):
        world = self.world()
        world.jobs.pop()
        world.checks.pop(0)
        page = world.responses[ROOT + f"/commits/{world.run['head_sha']}/check-runs?filter=all&per_page=100&page=1"]
        page["total_count"] = 1
        self.reconcile(world)
        self.assertEqual(world.problems, {world.scope + ":pr:273": "待定 PR 事件 Check 不能授权终态 reporter"})

    def test_missing_failed_or_inherited_reporter_cannot_be_replaced_by_echoes(self):
        for changed in ("missing", "failed", "inherited", "duplicate"):
            with self.subTest(changed=changed):
                world = self.world()
                if changed == "missing":
                    world.jobs.pop(1)
                elif changed == "duplicate":
                    world.jobs.insert(1, dict(world.jobs[1], id=140473962125,
                        html_url=f"https://github.com/{REPO}/actions/runs/{world.run['id']}/job/140473962125"))
                else:
                    world.jobs[1].update(**(dict(conclusion="failure") if changed == "failed" else
                        dict(started_at="2026-11-26T11:20:29Z", completed_at="2026-11-26T11:20:57Z")))
                # All inherited reporters retain the existing successful no-op
                # reconciliation, but must not publish or gain a replacement.
                if changed == "inherited":
                    self.assertTrue(reconcile_publisher(world.observer, world.run, world.jobs))
                    state, = world.metadata.values()
                    self.assertEqual(state, dict(state="inherited"))
                else:
                    self.reconcile(world)
                    self.assertTrue(world.problems)
                self.assertFalse(any(":job:" in scope for scope in world.problems), world.problems)

    def test_malformed_spoofed_and_real_extra_jobs_remain_unknown(self):
        mutations = [("job", key, value) for key, value in (
            ("id", True), ("steps", None), ("steps", [dict(name="Run code")]), ("labels", ["ubuntu-latest"]),
            ("runner_id", 0), ("runner_name", "runner"), ("runner_group_id", 0), ("runner_group_name", "group"),
            ("run_id", 1), ("run_attempt", 2), ("head_sha", "f" * 40), ("head_branch", "other"),
            ("url", "https://example.invalid/job"), ("run_url", "https://example.invalid/run"),
            ("check_run_url", "https://example.invalid/check"), ("html_url", "https://example.invalid/check"),
            ("node_id", "spoof"), ("name", "Unexpected native job"))]
        mutations += [("check", key, value) for key, value in (
            ("id", "140474409168"), ("name", "Other"), ("head_sha", "f" * 40), ("external_id", "spoof"),
            ("app", dict(id=True, slug="github-actions")), ("app", dict(id=15368, slug="other")),
            ("url", "https://example.invalid/check"), ("html_url", "https://example.invalid/check"),
            ("node_id", "spoof"), ("check_suite", dict(id=1)), ("output", dict(summary="{}")))]
        mutations += [("publisher", key, value) for key, value in (
            ("workflow", ".github/workflows/merge-gate.yml"), ("workflow_sha", "f" * 40), ("run_id", "1"), ("run_attempt", "2"))]
        mutations += [("issuer", key, value) for key, value in (
            ("id", 1), ("path", ".github/workflows/merge-gate.yml"), ("workflow_id", 1), ("event", "push"),
            ("head_sha", "f" * 40), ("head_branch", "other"), ("pull_requests", []),
            ("repository", dict(id=1439910892, full_name="other/repo")), ("run_attempt", 2))]
        for kind, key, value in mutations:
            with self.subTest(kind=kind, key=key, value=value):
                world = self.world()
                job, check, issuer = world.jobs[3], world.checks[0], world.issuers[OTHER_ISSUER]
                metadata = json.loads(check["output"]["summary"])
                owner = dict(job=job, check=check, publisher=metadata["publisher"], issuer=issuer)[kind]
                owner[key] = value
                if kind == "publisher":
                    check["output"]["summary"] = json.dumps(metadata)
                self.reconcile(world)
                self.assertIn(world.scope + ":job:3", world.problems)
                self.assertIn(world.scope + ":pr:273", world.problems)

    def test_echo_uniqueness_and_complete_empty_execution_proof_are_required(self):
        for changed in ("duplicate", "native-id-collision", "missing-runner", "missing-steps", "missing-suite", "source-pair"):
            with self.subTest(changed=changed):
                world = self.world()
                if changed == "duplicate":
                    world.jobs.append(copy.deepcopy(world.jobs[3]))
                elif changed == "native-id-collision":
                    world.jobs[0]["id"] = OTHER_ECHO
                elif changed == "missing-runner":
                    del world.jobs[3]["runner_id"]
                elif changed == "missing-steps":
                    del world.jobs[3]["steps"]
                elif changed == "missing-suite":
                    del world.run["check_suite_id"]
                else:
                    metadata = json.loads(world.checks[0]["output"]["summary"])
                    metadata["targets"][0]["head_ref"] = "wrong"
                    world.checks[0]["output"]["summary"] = json.dumps(metadata)
                self.reconcile(world)
                self.assertIn(world.scope + ":job:3", world.problems)

    def test_no_echo_adds_no_echo_authentication_requests(self):
        world = self.world(synthetic_issuer=False)
        world.jobs = world.jobs[:2]
        self.reconcile(world)
        self.assertFalse(any(path.startswith(ROOT + "/check-runs/") or path == ROOT + f"/actions/runs/{OTHER_ISSUER}"
                             for path in world.calls))


if __name__ == "__main__":
    unittest.main()
