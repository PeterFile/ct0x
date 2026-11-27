#!/usr/bin/env python3
"""Offline periodic Full policy, authenticated-evidence and real replay regressions."""
import base64
import copy
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tempfile
import unittest
from unittest import mock
import zipfile
from types import SimpleNamespace
from contextlib import redirect_stdout
from datetime import timedelta


SCRIPTS = Path(__file__).resolve().parent
WORKFLOWS = SCRIPTS.parent / "workflows"
REPO = "owner/workspace"
SHA, TREE, BASE = (letter * 40 for letter in "abc")
RUN, ATTEMPT = 1234, 2


def load(name):
    path = SCRIPTS / (name + ".py")
    if not path.exists():
        return None
    spec = importlib.util.spec_from_file_location(name.replace("-", "_"), path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


POLICY, REPLAY = load("periodic-full"), load("periodic-replay")


def identity():
    return {"sha": SHA, "tree": TREE, "head": SHA, "base": BASE,
            "run_id": RUN, "run_attempt": ATTEMPT, "repo": REPO}


def full_needs():
    outputs = {key: str(value) for key, value in identity().items() if key != "repo"}
    return {"resolve": {"result": "success"},
            "preflight": {"result": "success", "outputs": {"sha": SHA, "tree": TREE}},
            "full": {"result": "success", "outputs": {**outputs, "verified": "true"}}}


def periodic_receipt():
    return {"schema": 1, "kind": "periodic-full", **identity(), "status": "success", "verified": True,
            "checkout": {"sha": SHA, "tree": TREE},
            "components": {key: "success" for key in ("resolve", "preflight", "full")},
            "full": {**identity(), "verified": True},
            "attribution": {"status": "unknown", "good_sha": None, "bad_sha": SHA,
                            "commit": None, "reason": "完整套件成功"}}


class FakeAPI:
    def __init__(self, kind="legacy", event=None, trigger_sha=SHA):
        self.calls = []
        self.repo = {"id": 7, "full_name": REPO, "fork": False, "default_branch": "master"}
        path = {"legacy": "ci.yml", "periodic": "periodic-full.yml", "release": "release-candidate.yml"}[kind]
        self.run = {"id": RUN, "run_attempt": ATTEMPT, "workflow_id": 9,
                    "path": ".github/workflows/" + path, "event": event or ("push" if kind == "legacy" else "schedule"),
                    "head_branch": "master", "head_sha": trigger_sha, "repository": self.repo,
                    "head_repository": self.repo, "status": "completed", "conclusion": "success",
                    "run_started_at": "2026-11-26T00:00:00Z", "updated_at": "2026-11-26T01:00:00Z",
                    "referenced_workflows": [{"path": REPO + "/.github/workflows/ci-full.yml@refs/heads/master", "sha": trigger_sha}]}
        self.rows = {f"repos/{REPO}": self.repo,
                     f"repos/{REPO}/actions/workflows/9": {"id": 9, "path": self.run["path"]},
                     f"repos/{REPO}/actions/runs/{RUN}/attempts/{ATTEMPT}": self.run,
                     f"repos/{REPO}/commits/{SHA}": {"sha": SHA, "commit": {"tree": {"sha": TREE}}, "parents": [{"sha": BASE}]}}
        if kind == "release":
            source = b"jobs:\n  full:\n    name: Release Full\n    uses: ./.github/workflows/ci-full.yml\n"
            self.rows[f"repos/{REPO}/contents/{self.run['path']}?ref={trigger_sha}"] = {
                "sha": hashlib.sha1(b"blob " + str(len(source)).encode() + b"\0" + source).hexdigest(),
                "encoding": "base64", "content": base64.b64encode(source).decode()}
        producer = {"legacy": "Full CI", "periodic": "Periodic Full evidence", "release": "Release Full evidence"}[kind]
        aggregate = ('Release Full' if kind == 'release' else 'Full suite') + ' / Full suite evidence'
        self.jobs = [self.job(producer, 100), self.job(aggregate, 101)]
        if kind == "legacy":
            self.jobs.append(self.job("CI preflight and feedback", 102))
        elif kind == "periodic":
            self.jobs.append(self.job("Periodic Full preflight", 102))
        self.rows[f"repos/{REPO}/actions/runs/{RUN}/attempts/{ATTEMPT}/jobs?per_page=100&page=1"] = {
            "total_count": len(self.jobs), "jobs": self.jobs}
        if kind == "legacy":
            receipt = {"base": BASE, "head": SHA, "run_id": str(RUN), "sha": SHA, "tree": TREE, "verified": True}
        elif kind == "periodic":
            receipt = periodic_receipt()
        else:
            receipt = {"schema": 1, "kind": "release-full", **identity(), "status": "success", "verified": True,
                       "artifact_sha256": "d" * 64}
        self.receipt = receipt
        self.set_log(receipt)
        if kind != "legacy":
            self.set_artifact(receipt, "periodic-full" if kind == "periodic" else "release-full")

    def job(self, name, job_id, state="success"):
        return {"id": job_id, "name": name, "run_id": RUN, "run_attempt": ATTEMPT, "head_sha": self.run["head_sha"],
                "status": "completed", "conclusion": state,
                "html_url": f"https://github.com/{REPO}/actions/runs/{RUN}/job/{job_id}",
                "started_at": "2026-11-26T00:00:00Z", "completed_at": "2026-11-26T01:00:00Z", "steps": []}

    def set_log(self, receipt):
        self.rows[f"repos/{REPO}/actions/jobs/100/logs"] = (
            "2026-11-26T00:30:11.4302912Z " + json.dumps(receipt, sort_keys=True) + "\n").encode()

    def set_artifact(self, receipt, prefix):
        data = io.BytesIO()
        with zipfile.ZipFile(data, "w") as archive:
            archive.writestr("receipt.json", json.dumps(receipt))
        self.zip = data.getvalue()
        self.artifact = {"id": 77, "name": f"{prefix}-{RUN}-{ATTEMPT}", "expired": False,
                         "digest": "sha256:" + hashlib.sha256(self.zip).hexdigest(),
                         "created_at": "2026-11-26T00:30:00Z",
                         "workflow_run": {"id": RUN, "repository_id": 7, "head_repository_id": 7,
                                          "head_sha": self.run["head_sha"], "head_branch": self.run["head_branch"]}}
        self.rows[f"repos/{REPO}/actions/runs/{RUN}/artifacts?per_page=100&page=1"] = {
            "total_count": 1, "artifacts": [self.artifact]}
        self.rows[f"repos/{REPO}/actions/artifacts/77/zip"] = self.zip

    def get(self, endpoint, raw=False):
        self.calls.append(endpoint)
        match = re.fullmatch(r"repos/owner/workspace/contents/(.+)\?ref=([a-f0-9]{40})", endpoint)
        if endpoint not in self.rows and match and (SCRIPTS.parent.parent / match[1]).is_file():
            data = (SCRIPTS.parent.parent / match[1]).read_bytes()
            self.rows[endpoint] = {"sha": hashlib.sha1(b"blob " + str(len(data)).encode() + b"\0" + data).hexdigest(),
                                   "encoding": "base64", "content": base64.b64encode(data).decode()}
        if endpoint not in self.rows:
            raise ValueError("离线夹具没有此证据：" + endpoint)
        return self.rows[endpoint]


class EvidenceTests(unittest.TestCase):
    def setUp(self):
        self.assertIsNotNone(POLICY, "periodic-full.py 尚未实现")

    def evidence(self, api):
        return POLICY.Evidence(api, REPO, lambda sha: sha in {SHA, BASE, "e" * 40})

    def test_real_timestamped_legacy_outer_line_and_attempt_specific_jobs(self):
        api = FakeAPI()
        proof = self.evidence(api).verify(RUN, ATTEMPT)
        self.assertEqual((proof["sha"], proof["tree"], proof["run_id"], proof["run_attempt"]), (SHA, TREE, RUN, ATTEMPT))
        self.assertIn(f"repos/{REPO}/actions/runs/{RUN}/attempts/{ATTEMPT}/jobs?per_page=100&page=1", api.calls)
        self.assertEqual(proof["producer_job_id"], 100)

    def test_api_workflow_path_ref_suffix_must_match_authenticated_source(self):
        api = FakeAPI()
        api.run["path"] += "@refs/heads/master"
        self.assertEqual(self.evidence(api).verify(RUN, ATTEMPT)["sha"], SHA)
        api.run["path"] = ".github/workflows/ci.yml@refs/heads/feature"
        with self.assertRaises(ValueError):
            self.evidence(api).verify(RUN, ATTEMPT)

    def test_green_workflow_is_not_full_proof_and_all_identity_fields_are_typed(self):
        cases = [lambda a: a.jobs[0].update(conclusion="skipped"),
                 lambda a: a.jobs[1].update(conclusion="failure"),
                 lambda a: a.jobs[2].update(conclusion="failure"),
                 lambda a: a.jobs[0].update(run_attempt=1),
                 lambda a: a.jobs[0].update(run_id=True),
                 lambda a: a.jobs[0].update(head_sha=BASE),
                 lambda a: a.run.update(event="pull_request"),
                 lambda a: a.run.update(head_branch="feature"),
                 lambda a: a.run.update(path=".github/workflows/unrelated.yml"),
                 lambda a: a.run.update(head_repository={**a.repo, "fork": True}),
                 lambda a: a.run.update(repository={**a.repo, "full_name": "attacker/repo"}),
                 lambda a: a.run.update(id=True),
                 lambda a: a.run.update(run_attempt=2.0),
                 lambda a: a.rows.pop(f"repos/{REPO}/actions/jobs/100/logs")]
        for change in cases:
            api = FakeAPI()
            change(api)
            with self.subTest(change=change), self.assertRaises(ValueError):
                self.evidence(api).verify(RUN, ATTEMPT)
        for field, value in [("run_id", RUN), ("verified", "true"), ("sha", BASE), ("tree", SHA),
                             ("head", BASE), ("base", SHA)]:
            api = FakeAPI()
            api.set_log({**api.receipt, field: value})
            with self.subTest(field=field), self.assertRaises(ValueError):
                self.evidence(api).verify(RUN, ATTEMPT)

    def test_same_sha_periodic_receipt_accepts_moved_schedule_target_not_trigger_sha(self):
        api = FakeAPI("periodic", trigger_sha=BASE)
        proof = self.evidence(api).verify(RUN, ATTEMPT)
        self.assertEqual(proof["sha"], SHA)
        for change in [lambda a: a.artifact.update(expired=True),
                       lambda a: a.artifact["workflow_run"].update(head_sha=SHA),
                       lambda a: a.artifact.update(name=f"periodic-full-{RUN}-1"),
                       lambda a: a.artifact.update(digest="sha256:" + "0" * 64),
                       lambda a: a.run.update(event="workflow_dispatch", head_branch="feature")]:
            api = FakeAPI("periodic", trigger_sha=BASE)
            change(api)
            with self.subTest(change=change), self.assertRaises(ValueError):
                self.evidence(api).verify(RUN, ATTEMPT)
        for field, value in [("verified", 1), ("run_id", str(RUN)), ("run_attempt", True), ("schema", True)]:
            api = FakeAPI("periodic")
            receipt = {**api.receipt, field: value}
            api.set_log(receipt)
            api.set_artifact(receipt, "periodic-full")
            with self.subTest(field=field), self.assertRaises(ValueError):
                self.evidence(api).verify(RUN, ATTEMPT)

    def queued_periodic_api(self, changed=None):
        api = FakeAPI('periodic', trigger_sha=BASE)
        for name in ('periodic-full.yml', 'ci-full.yml'):
            original = (WORKFLOWS / name).read_bytes()
            target = original
            if name == changed:
                if name == 'periodic-full.yml':
                    marker = b'      - name: Ordinary test cmd package\n'
                    self.assertEqual(original.count(marker), 1)
                    target = original.replace(marker, b'      - name: New mandatory preflight gate\n        run: exit 1\n' + marker)
                else:
                    target += b'\n# Updated reusable Full policy.\n'
            for value, data in ((BASE, original), (SHA, target)):
                api.rows[f'repos/{REPO}/contents/.github/workflows/{name}?ref={value}'] = {
                    'sha': hashlib.sha1(b'blob ' + str(len(data)).encode() + b'\0' + data).hexdigest(),
                    'encoding': 'base64', 'content': base64.b64encode(data).decode()}
        return api

    def test_queued_periodic_verify_requires_both_static_policy_blobs(self):
        for changed in (None, 'ci-full.yml', 'periodic-full.yml'):
            api = self.queued_periodic_api(changed)
            with self.subTest(changed=changed):
                if changed:
                    with self.assertRaisesRegex(ValueError, re.escape('.github/workflows/' + changed)):
                        self.evidence(api).verify(RUN, ATTEMPT)
                else:
                    self.assertEqual(self.evidence(api).verify(RUN, ATTEMPT)['sha'], SHA)
                    for name in ('periodic-full.yml', 'ci-full.yml'):
                        for value in (BASE, SHA):
                            self.assertIn(f'repos/{REPO}/contents/.github/workflows/{name}?ref={value}', api.calls)

    def test_queued_periodic_verify_rejects_unproven_target_policy_content(self):
        for name in ('periodic-full.yml', 'ci-full.yml'):
            for invalid in ('missing-content', 'forged-content'):
                api = self.queued_periodic_api()
                row = api.rows[f'repos/{REPO}/contents/.github/workflows/{name}?ref={SHA}']
                if invalid == 'missing-content':
                    del row['encoding']
                    del row['content']
                else:
                    row['content'] = base64.b64encode(base64.b64decode(row['content']) + b'\n# Changed policy\n').decode()
                with self.subTest(name=name, invalid=invalid), self.assertRaises(ValueError):
                    self.evidence(api).verify(RUN, ATTEMPT)

    def test_release_full_survives_later_hardware_failure_but_requires_trusted_producer(self):
        api = FakeAPI("release", event="workflow_dispatch", trigger_sha=BASE)
        api.run["conclusion"] = "failure"
        api.rows.pop(f"repos/{REPO}/actions/jobs/100/logs")
        self.assertEqual(self.evidence(api).verify(RUN, ATTEMPT)["sha"], SHA)
        api.jobs[0]["name"] = "Release candidate green"
        with self.assertRaises(ValueError):
            self.evidence(api).verify(RUN, ATTEMPT)
        api = FakeAPI("release", event="push")
        api.run["head_branch"] = "rc/v1"
        # A tag/receipt assertion without API-bound master workflow provenance is unknown.
        with self.assertRaises(ValueError):
            self.evidence(api).verify(RUN, ATTEMPT)

    def native_rc_api(self, tag='rc/v1', depth=1, raw_head=False):
        from urllib.parse import quote
        workflow = '''jobs:
  full:
    name: Release Full
    uses: ./.github/workflows/ci-full.yml
  full-evidence:
    name: Release Full evidence
    needs: [normalize, claim, full, clients, binary]
    steps:
      - name: Verify native Full attempt and independently rehash binary
        run: python3 -B .github/scripts/release-evidence.py full --output "$RUNNER_TEMP/release-full"
      - uses: actions/upload-artifact@v7
        with:
          name: release-full-${{ github.run_id }}-${{ github.run_attempt }}
          path: ${{ runner.temp }}/release-full/receipt.json
'''.encode()
        api = FakeAPI('release', event='push')
        api.run['head_branch'] = tag
        api.run['referenced_workflows'] = [{'path': REPO + '/.github/workflows/ci-full.yml@refs/tags/' + tag, 'sha': SHA}]
        minimal = {key: api.receipt[key] for key in ('schema', 'kind', 'repo', 'run_id', 'run_attempt', 'sha', 'tree', 'status', 'verified', 'artifact_sha256')}
        target = {'type': 'commit', 'sha': SHA}
        for index in reversed(range(depth)):
            value = BASE if index == 0 else f'{index:040x}'
            api.rows[f'repos/{REPO}/git/tags/{value}'] = {'sha': value, 'object': target}
            target = {'type': 'tag', 'sha': value}
        api.rows[f'repos/{REPO}/git/ref/tags/' + quote(tag, safe='')] = {'ref': 'refs/tags/' + tag, 'object': target}
        if raw_head:
            api.run.update(head_sha=target['sha'], head_commit={'id': SHA, 'tree_id': TREE})
            for job in api.jobs:
                job['head_sha'] = target['sha']
        api.set_artifact(minimal, 'release-full')
        api.rows[f'repos/{REPO}/contents/.github/workflows/release-candidate.yml?ref={SHA}'] = {
            'sha': hashlib.sha1(b'blob ' + str(len(workflow)).encode() + b'\0' + workflow).hexdigest(),
            'encoding': 'base64', 'content': base64.b64encode(workflow).decode()}
        return api

    def test_native_rc_proof_uses_only_real_protocol_and_exact_tag_commit(self):
        from urllib.parse import quote
        for tag in ('rc/v1', 'rc/v1+候选#2', 'rc/nested/v1'):
            api = self.native_rc_api(tag)
            endpoint = f'repos/{REPO}/git/ref/tags/' + quote(tag, safe='')
            with self.subTest(tag=tag):
                self.assertEqual(self.evidence(api).verify(RUN, ATTEMPT)['sha'], SHA)
                original = copy.deepcopy(api.rows)
                for mutate in (lambda: api.rows[endpoint]['object'].update(type='commit', sha=BASE),
                               lambda: api.rows[f'repos/{REPO}/git/tags/{BASE}']['object'].update(sha='f' * 40),
                               lambda: api.run.update(referenced_workflows=[]),
                               lambda: api.rows[f'repos/{REPO}/contents/.github/workflows/release-candidate.yml?ref={SHA}'].update(sha=BASE)):
                    mutate()
                    with self.assertRaises(ValueError): self.evidence(api).verify(RUN, ATTEMPT)
                    api.rows = copy.deepcopy(original)
                    api.run = api.rows[f'repos/{REPO}/actions/runs/{RUN}/attempts/{ATTEMPT}']
                with self.assertRaises(ValueError): POLICY.Evidence(api, REPO, lambda _: False).verify(RUN, ATTEMPT)

    def test_native_rc_peeled_and_raw_heads_bind_only_verified_commit_sources(self):
        for raw_head in (False, True):
            for depth in (1, 8):
                api = self.native_rc_api(depth=depth, raw_head=raw_head)
                api.run['head_commit'] = {'id': SHA, 'tree_id': TREE}
                checked = []
                def ancestor(value):
                    checked.append(value)
                    return value == SHA
                with self.subTest(raw_head=raw_head, depth=depth):
                    proof = POLICY.Evidence(api, REPO, ancestor).verify(RUN, ATTEMPT)
                    self.assertEqual((proof['sha'], proof['tree']), (SHA, TREE))
                    self.assertEqual(set(checked), {SHA})
                    native = BASE if raw_head else SHA
                    self.assertEqual(api.run['head_sha'], native)
                    self.assertTrue(all(job['head_sha'] == native for job in api.jobs))
                    self.assertEqual(api.artifact['workflow_run']['head_sha'], native)
                    for path in ('release-candidate.yml', 'ci-full.yml'):
                        self.assertIn(f'repos/{REPO}/contents/.github/workflows/{path}?ref={SHA}', api.calls)
                        self.assertNotIn(f'repos/{REPO}/contents/.github/workflows/{path}?ref={BASE}', api.calls)
                    self.assertNotIn(f'repos/{REPO}/commits/{BASE}', api.calls)

    def test_native_rc_head_commit_evidence_cannot_contradict_either_head_shape(self):
        for raw_head in (False, True):
            for invalid in (None, {}, {'id': BASE, 'tree_id': TREE}, {'id': SHA, 'tree_id': BASE},
                            {'id': SHA}, {'id': '../invalid', 'tree_id': TREE}):
                api = self.native_rc_api(depth=8, raw_head=raw_head)
                api.run['head_commit'] = invalid
                with self.subTest(raw_head=raw_head, invalid=invalid), self.assertRaises(ValueError):
                    self.evidence(api).verify(RUN, ATTEMPT)
        api = self.native_rc_api(depth=8, raw_head=True)
        del api.run['head_commit']
        with self.assertRaises(ValueError):
            self.evidence(api).verify(RUN, ATTEMPT)
        api = self.native_rc_api(depth=8, raw_head=True)
        api.rows[f'repos/{REPO}/commits/{SHA}']['commit']['tree']['sha'] = BASE
        api.set_artifact({**api.receipt, 'tree': BASE}, 'release-full')
        with self.assertRaises(ValueError):
            self.evidence(api).verify(RUN, ATTEMPT)

    def test_native_rc_raw_head_rejects_moved_refs_and_wrong_control_sources(self):
        endpoint = f'repos/{REPO}/git/ref/tags/rc%2Fv1'
        workflow = f'repos/{REPO}/contents/.github/workflows/release-candidate.yml?ref={SHA}'
        for name, mutate in (
                ('moved-outer-tag', lambda a: a.rows[endpoint]['object'].update(sha='d' * 40)),
                ('moved-to-inner-tag', lambda a: a.rows[endpoint]['object'].update(sha=f'{1:040x}')),
                ('moved-to-lightweight', lambda a: a.rows[endpoint].update(object={'type': 'commit', 'sha': SHA})),
                ('wrong-ref', lambda a: a.rows[endpoint].update(ref='refs/tags/rc/other')),
                ('wrong-target', lambda a: a.rows[f'repos/{REPO}/git/tags/{7:040x}']['object'].update(sha=BASE)),
                ('wrong-source-sha', lambda a: a.run['referenced_workflows'][0].update(sha=BASE)),
                ('wrong-source-ref', lambda a: a.run['referenced_workflows'][0].update(path=REPO + '/.github/workflows/ci-full.yml@refs/tags/rc/other')),
                ('wrong-source-repo', lambda a: a.run['referenced_workflows'][0].update(path='foreign/repo/.github/workflows/ci-full.yml@refs/tags/rc/v1')),
                ('wrong-workflow-ref', lambda a: a.run.update(path=a.run['path'] + '@refs/tags/rc/other')),
                ('wrong-source-blob', lambda a: a.rows[workflow].update(sha=BASE)),
                ('unavailable-commit-source', lambda a: a.rows.update({workflow: None}))):
            api = self.native_rc_api(depth=8, raw_head=True)
            api.rows[f'repos/{REPO}/contents/.github/workflows/release-candidate.yml?ref={BASE}'] = copy.deepcopy(api.rows[workflow])
            api.rows[f'repos/{REPO}/git/tags/' + 'd' * 40] = {'sha': 'd' * 40, 'object': {'type': 'commit', 'sha': SHA}}
            mutate(api)
            with self.subTest(name=name), self.assertRaises(ValueError):
                self.evidence(api).verify(RUN, ATTEMPT)

    def test_native_rc_raw_head_keeps_native_job_artifact_and_owner_identity(self):
        for name, mutate in (
                ('job-head', lambda a: a.jobs[0].update(head_sha=SHA)),
                ('artifact-head', lambda a: a.artifact['workflow_run'].update(head_sha=SHA)),
                ('foreign-repo', lambda a: a.run.update(repository={**a.repo, 'full_name': 'foreign/repo'})),
                ('foreign-head-repo', lambda a: a.run.update(head_repository={**a.repo, 'id': 8})),
                ('wrong-receipt-tree', lambda a: a.set_artifact({**a.receipt, 'tree': BASE}, 'release-full')),
                ('wrong-receipt-target', lambda a: a.set_artifact({**a.receipt, 'sha': BASE}, 'release-full'))):
            api = self.native_rc_api(depth=8, raw_head=True)
            mutate(api)
            with self.subTest(name=name), self.assertRaises(ValueError):
                self.evidence(api).verify(RUN, ATTEMPT)

    def test_native_rc_tag_peeling_has_exact_eight_object_bound(self):
        for depth in (0, 1, 3, 4, 6, 8):
            api = self.native_rc_api(depth=depth)
            with self.subTest(depth=depth):
                self.assertEqual(self.evidence(api).verify(RUN, ATTEMPT)['sha'], SHA)
                self.assertEqual(len([path for path in api.calls if '/git/tags/' in path]), depth)
        api = self.native_rc_api(depth=9)
        with self.assertRaisesRegex(ValueError, '深度'):
            self.evidence(api).verify(RUN, ATTEMPT)
        self.assertEqual(len([path for path in api.calls if '/git/tags/' in path]), 8)
        self.assertNotIn(f'repos/{REPO}/git/tags/{8:040x}', api.calls)

    def test_native_rc_tag_peeling_rejects_cycles_without_refetching(self):
        for depth in (1, 2, 8):
            api = self.native_rc_api(depth=depth)
            last = BASE if depth == 1 else f'{depth - 1:040x}'
            api.rows[f'repos/{REPO}/git/tags/{last}']['object'] = {'type': 'tag', 'sha': BASE}
            with self.subTest(depth=depth), self.assertRaisesRegex(ValueError, '循环'):
                self.evidence(api).verify(RUN, ATTEMPT)
            self.assertEqual(len([path for path in api.calls if '/git/tags/' in path]), depth)

    def test_native_rc_tag_chain_requires_each_object_identity_and_valid_links(self):
        for index in range(8):
            value = BASE if index == 0 else f'{index:040x}'
            for field, invalid in (('sha', 'f' * 40), ('object', None),
                                   ('object', {'type': 'tag', 'sha': '../invalid'}),
                                   ('object', {'type': 'tree', 'sha': SHA})):
                api = self.native_rc_api(depth=8)
                api.rows[f'repos/{REPO}/git/tags/{value}'][field] = invalid
                with self.subTest(index=index, field=field, invalid=invalid), self.assertRaises(ValueError):
                    self.evidence(api).verify(RUN, ATTEMPT)

    def test_native_rc_tag_ref_and_terminal_commit_must_match(self):
        endpoint = f'repos/{REPO}/git/ref/tags/rc%2Fv1'
        for mutate in (lambda a: a.rows[endpoint].update(ref='refs/tags/rc/other'),
                       lambda a: a.rows[endpoint].update(object=None),
                       lambda a: a.rows[endpoint]['object'].update(sha='../invalid'),
                       lambda a: a.rows[f'repos/{REPO}/git/tags/{7:040x}']['object'].update(sha=BASE),
                       lambda a: a.rows[f'repos/{REPO}/git/tags/{7:040x}']['object'].update(type='tree')):
            api = self.native_rc_api(depth=8)
            mutate(api)
            with self.subTest(mutate=mutate), self.assertRaises(ValueError):
                self.evidence(api).verify(RUN, ATTEMPT)

    def test_api_is_readonly_bounded_cached_and_never_saves_credentials(self):
        with tempfile.TemporaryDirectory() as temporary, mock.patch.dict(os.environ, {"GH_TOKEN": "secret-token", "PRIVATE_SECRET": "hidden"}):
            calls = []
            def fetch(argv, **kwargs):
                calls.append((argv, kwargs["env"]))
                if len(calls) == 1:
                    kwargs["stderr"].write(b"HTTP 503")
                    return subprocess.CompletedProcess(argv, 1)
                kwargs["stdout"].write(b'{"value":1}')
                return subprocess.CompletedProcess(argv, 0, stderr=b"")
            api = POLICY.API(Path(temporary))
            with mock.patch.object(POLICY.subprocess, "run", side_effect=fetch), mock.patch.object(POLICY.time, "sleep"):
                self.assertEqual(api.get("repos/owner/workspace"), {"value": 1})
                self.assertEqual(api.get("repos/owner/workspace", raw=True), b'{"value":1}')
            self.assertEqual(len(calls), 2)
            self.assertTrue(all(isinstance(path, Path) for path in api.cache.values()))
            for argv, env in calls:
                self.assertEqual(argv[:4], ["gh", "api", "--method", "GET"])
                self.assertNotIn("PRIVATE_SECRET", env)
            self.assertNotIn("secret-token", "".join(path.read_text() for path in Path(temporary).iterdir()))
            with mock.patch.object(POLICY, "MAX_API", 1), self.assertRaises(ValueError):
                api.get("repos/owner/other")

    def test_api_response_and_total_disk_cache_bounds_fail_closed(self):
        for limit, total in ((3, 100), (100, 3)):
            with tempfile.TemporaryDirectory() as temporary, mock.patch.dict(os.environ, {'GH_TOKEN': 'read-only'}), \
                 mock.patch.object(POLICY, 'MAX_BYTES', limit), mock.patch.object(POLICY, 'MAX_CACHE', total):
                api = POLICY.API(Path(temporary))
                def fetch(argv, **kwargs):
                    kwargs['stdout'].write(b'1234')
                    return subprocess.CompletedProcess(argv, 0, stderr=b'')
                with mock.patch.object(POLICY.subprocess, 'run', side_effect=fetch), self.assertRaises(ValueError):
                    api.get('repos/owner/workspace')
                self.assertEqual(api.cache, {})

    def test_raw_job_transport_retries_only_modern_local_escape_guard(self):
        guard = b'the response contains terminal escape sequences; pass --allow-escape-sequences to output it anyway\n'
        data = b'\x1b[32msetup\x1b[0m\n' + json.dumps(FakeAPI().receipt).encode() + b'\n'
        endpoint = f'repos/{REPO}/actions/jobs/100/logs'
        for failure, raw, path, expected in ((guard, True, endpoint, 2), (None, True, endpoint, 1),
                                             (b'HTTP 403 Forbidden', True, endpoint, 1),
                                             (guard, False, endpoint, 1), (guard, True, f'repos/{REPO}/actions/artifacts/77/zip', 1)):
            with self.subTest(failure=failure, raw=raw, path=path), tempfile.TemporaryDirectory() as temporary, mock.patch.dict(os.environ, {'GH_TOKEN': 'test-read-only-token'}):
                calls = []
                def fetch(argv, **kwargs):
                    calls.append(argv)
                    if len(calls) == 1 and failure:
                        kwargs['stderr'].write(failure)
                        return subprocess.CompletedProcess(argv, 1)
                    kwargs['stdout'].write(data)
                    return subprocess.CompletedProcess(argv, 0, stderr=b'')
                api = POLICY.API(Path(temporary))
                with mock.patch.object(POLICY.subprocess, 'run', side_effect=fetch), redirect_stdout(io.StringIO()) as stdout:
                    if failure and expected == 1:
                        with self.assertRaises(ValueError): api.get(path, raw=raw)
                    else:
                        self.assertEqual(api.get(path, raw=True), data)
                        self.assertEqual(api.get(path, raw=True), data)
                self.assertEqual(len(calls), expected)
                self.assertNotIn('--allow-escape-sequences', calls[0])
                if expected == 2: self.assertIn('--allow-escape-sequences', calls[1])
                self.assertEqual(stdout.getvalue(), '')

    def test_receipt_artifact_must_equal_successful_producer_log(self):
        api = FakeAPI("periodic")
        api.set_artifact({**api.receipt, "tree": BASE}, "periodic-full")
        with self.assertRaises(ValueError):
            self.evidence(api).verify(RUN, ATTEMPT)
        api = FakeAPI()
        api.rows[f"repos/{REPO}/actions/jobs/100/logs"] += b'{"verified":true,"verified":false}\n'
        with self.assertRaises(ValueError):
            self.evidence(api).verify(RUN, ATTEMPT)

    def test_typed_artifact_values_and_malformed_lookup_fail_conservatively(self):
        api = FakeAPI("periodic")
        api.set_artifact({**api.receipt, "verified": 1}, "periodic-full")
        with self.assertRaises(ValueError):
            self.evidence(api).verify(RUN, ATTEMPT)
        api.rows[f"repos/{REPO}/actions/workflows/ci.yml/runs?per_page=100&page=1"] = {"total_count": 1, "workflow_runs": [None]}
        found, reasons = self.evidence(api).search([SHA])
        self.assertEqual(found, {})
        self.assertTrue(reasons)

    def test_failed_job_collection_retains_raw_job_identity_and_blocks_infra_attribution(self):
        api = FakeAPI("periodic")
        failed = api.job("Full suite / Go workspace race", 501, "failure")
        failed["steps"] = [{"number": 3, "name": "Race test workspace package", "conclusion": "failure",
                            "started_at": "2026-11-26T00:01:00Z"}]
        api.jobs.append(failed)
        api.rows[f"repos/{REPO}/actions/runs/{RUN}/attempts/{ATTEMPT}/jobs?per_page=100&page=1"]["total_count"] = len(api.jobs)
        api.rows[f"repos/{REPO}/actions/jobs/501/logs"] = b'--- FAIL: TestRegression (0.01s)\n    main_test.go:8: deterministic regression\nFAIL\tagentworkspace/cmd/workspace\t0.01s\n'
        result = {}
        POLICY.collect_failures(self.evidence(api), api.run, result)
        self.assertEqual(result["first_blocking_event"]["id"], 501)
        self.assertEqual(result["failure_candidates"][0]["sources"][0]["id"], 501)
        self.assertEqual(result["failure_candidates"][0]["signature"]["messages"], ["deterministic regression"])
        failed["name"] = "Full suite / Unrelated diagnostic"
        result = {}
        POLICY.collect_failures(self.evidence(api), api.run, result)
        self.assertEqual(result["failure_candidates"], [])
        self.assertEqual(result["diagnostic_status"], "unknown")

    def test_lookup_does_not_filter_on_head_sha_and_absence_runs_conservatively(self):
        api = FakeAPI("periodic", trigger_sha=BASE)
        for workflow in ("ci.yml", "periodic-full.yml", "release-candidate.yml"):
            api.rows[f"repos/{REPO}/actions/workflows/{workflow}/runs?per_page=100&page=1"] = {
                "total_count": 1 if workflow == "periodic-full.yml" else 0,
                "workflow_runs": [api.run] if workflow == "periodic-full.yml" else []}
        found, reasons = self.evidence(api).search([SHA, BASE])
        self.assertIn(SHA, found)
        self.assertFalse(any("head_sha=" in call and "/workflows/ci.yml/" not in call for call in api.calls))
        api.rows.pop(f"repos/{REPO}/actions/jobs/100/logs")
        found, reasons = self.evidence(api).search([SHA])
        self.assertEqual(found, {})
        self.assertTrue(reasons)

    def test_queued_periodic_admit_requires_both_static_policy_blobs(self):
        def git(root, *args):
            return {('rev-parse', 'HEAD'): SHA, ('rev-parse', 'HEAD^{tree}'): TREE,
                    ('rev-list', '--first-parent', SHA): SHA + '\n' + BASE}[args]
        control = SimpleNamespace(git=git)
        plan = SimpleNamespace(shard_matrix=lambda: {'include': []})
        for changed in (None, 'ci-full.yml', 'periodic-full.yml'):
            api = self.queued_periodic_api(changed)
            evidence = self.evidence(api)
            with self.subTest(changed=changed), tempfile.TemporaryDirectory() as temporary:
                root = Path(temporary)
                ref = root / 'ref.json'
                ref.write_text(json.dumps({'ref': 'refs/heads/master', 'object': {'type': 'commit', 'sha': SHA}}))
                args = SimpleNamespace(root=root, ref=ref, output=root)
                env = {'GITHUB_SHA': BASE, 'GITHUB_RUN_ID': str(RUN), 'GITHUB_RUN_ATTEMPT': str(ATTEMPT),
                       'GITHUB_OUTPUT': str(root / 'outputs')}
                with mock.patch.dict(os.environ, env), redirect_stdout(io.StringIO()), \
                     mock.patch.object(POLICY, 'load', side_effect=lambda name, root=None: control if name == 'periodic-replay' else plan), \
                     mock.patch.object(evidence, 'search', return_value=({}, [])) as search:
                    if changed:
                        with self.assertRaisesRegex(ValueError, re.escape('.github/workflows/' + changed)):
                            POLICY.admit(args, api, evidence)
                        self.assertFalse((root / 'plan.json').exists())
                        search.assert_not_called()
                    else:
                        POLICY.admit(args, api, evidence)
                        recorded = json.loads((root / 'plan.json').read_text())
                        self.assertEqual((recorded['identity']['sha'], recorded['identity']['tree'], recorded['decision']),
                                         (SHA, TREE, 'run'))
                        for name in ('periodic-full.yml', 'ci-full.yml'):
                            for value in (BASE, SHA):
                                self.assertIn(f'repos/{REPO}/contents/.github/workflows/{name}?ref={value}', api.calls)

    def test_admission_resolves_latest_master_only_for_master_and_never_bypasses_same_sha(self):
        api = FakeAPI("periodic", event="workflow_dispatch", trigger_sha=BASE)
        def git(root, *args):
            return {("rev-parse", "HEAD"): SHA, ("rev-parse", "HEAD^{tree}"): TREE,
                    ("rev-list", "--first-parent", SHA): SHA + "\n" + BASE}[args]
        control = SimpleNamespace(git=git)
        plan = SimpleNamespace(shard_matrix=lambda: {"include": []})
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            ref = root / "ref.json"
            ref.write_text(json.dumps({"ref": "refs/heads/master", "object": {"type": "commit", "sha": SHA}}))
            args = SimpleNamespace(root=root, ref=ref, output=root)
            env = {"GITHUB_SHA": BASE, "GITHUB_RUN_ID": str(RUN), "GITHUB_RUN_ATTEMPT": str(ATTEMPT),
                   "GITHUB_OUTPUT": str(root / "outputs")}
            with mock.patch.dict(os.environ, env), mock.patch.object(POLICY, "load", side_effect=lambda name, root=None: control if name == "periodic-replay" else plan), redirect_stdout(io.StringIO()):
                POLICY.admit(args, api, self.evidence(api))
                recorded = json.loads((root / "plan.json").read_text())
                self.assertEqual(recorded["identity"]["sha"], SHA)
                self.assertEqual(recorded["decision"], "run")
                evidence = self.evidence(api)
                proof = {**identity(), "verified": True}
                with mock.patch.object(evidence, "search", return_value=({SHA: proof}, [])):
                    for force, decision in (("false", "skip"), ("true", "skip")):
                        with mock.patch.dict(os.environ, {"PERIODIC_FORCE": force}):
                            POLICY.admit(args, api, evidence)
                            self.assertEqual(json.loads((root / "plan.json").read_text())["decision"], decision)
                api.run['head_branch'] = 'feature'
                with self.assertRaises(ValueError): POLICY.admit(args, api, self.evidence(api))
                api.run['head_branch'] = 'master'
                api.rows[f'repos/{REPO}/contents/.github/workflows/ci-full.yml?ref={BASE}']['sha'] = BASE
                with self.assertRaises(ValueError): POLICY.admit(args, api, self.evidence(api))
                api.repo["fork"] = True
                with self.assertRaises(ValueError): POLICY.admit(args, api, self.evidence(api))
        with self.assertRaises(ValueError):
            self.evidence(FakeAPI('periodic', event='workflow_dispatch')).source({**api.run, 'head_branch': 'feature'}, periodic_receipt())

    def test_success_receipt_requires_exact_needs_and_full_aggregate_identity(self):
        receipt = POLICY.receipt(identity(), full_needs(), "run", None)
        self.assertTrue(receipt["verified"])
        self.assertEqual(receipt["status"], "success")
        skipped = full_needs()
        for key in ("preflight", "full"):
            skipped[key] = {"result": "skipped"}
        result = POLICY.receipt(identity(), skipped, "skip", {**identity(), "verified": True})
        self.assertEqual((result["status"], result["verified"]), ("skipped", False))
        for key in full_needs():
            needs = full_needs()
            needs[key]["result"] = "failure"
            result = POLICY.receipt(identity(), needs, "run", None)
            self.assertFalse(result["verified"])
            self.assertEqual(result["status"], "failure")
        for field in ("sha", "tree", "head", "base", "run_id"):
            needs = full_needs()
            needs["full"]["outputs"][field] = "wrong"
            with self.subTest(field=field), self.assertRaises(ValueError):
                POLICY.receipt(identity(), needs, "run", None)
        with self.assertRaises(ValueError):
            POLICY.receipt(identity(), {}, "skip", None)

    def test_exact_authenticated_merged_pr_only_no_subject_guessing(self):
        local = {"localization": "reproduced", "status": "unknown", "commit": SHA,
                 "good_sha": BASE, "bad_sha": SHA, "transition": "regression"}
        pr = {"number": 17, "merged_at": "2026-11-26T01:00:00Z", "merge_commit_sha": SHA,
              "base": {"ref": "master", "repo": {"full_name": REPO}}}
        result = POLICY.attribute(local, [pr], REPO)
        self.assertEqual((result["status"], result["pr"]), ("confirmed", 17))
        for rows in ([], [pr, {**pr, "number": 18}], [{**pr, "merged_at": None}],
                     [{**pr, "merge_commit_sha": BASE}], [{**pr, "number": True}], [{**pr, "merged_at": ""}],
                     [{**pr, "base": {"ref": "feature", "repo": {"full_name": REPO}}}]):
            result = POLICY.attribute(local, rows, REPO)
            self.assertEqual(result["status"], "unknown")
            self.assertNotIn("pr", result)


class InheritedPreflightTests(unittest.TestCase):
    def fixture(self):
        api = FakeAPI()
        api.run.update(run_started_at='2026-11-26T02:00:00Z', updated_at='2026-11-26T03:00:00Z')
        for job in api.jobs[:2]:
            job.update(started_at='2026-11-26T02:05:00Z', completed_at='2026-11-26T02:15:00Z')
        api.rows[f'repos/{REPO}/actions/jobs/100/logs'] = b'2026-11-26T02:10:00Z ' + json.dumps(api.receipt).encode() + b'\n'
        reported = api.jobs[2]
        reported.update(id=140027648923, runner_id=1000034353, started_at='2026-11-26T00:05:00Z', completed_at='2026-11-26T00:10:00Z',
                        html_url=f'https://github.com/{REPO}/actions/runs/{RUN}/job/140027648923')
        reported['steps'] = [{'name': f'Native step {index}', 'number': index, 'status': 'completed',
                              'conclusion': 'skipped' if index == 6 else 'success',
                              'started_at': '2026-11-26T00:06:00Z', 'completed_at': '2026-11-26T00:07:00Z'} for index in range(1, 17)]
        original = copy.deepcopy(reported)
        original.update(id=139992361691, run_attempt=1, html_url=f'https://github.com/{REPO}/actions/runs/{RUN}/job/139992361691')
        previous = copy.deepcopy(api.run)
        previous.update(run_attempt=1, conclusion='failure', run_started_at='2026-11-26T00:00:00Z', updated_at='2026-11-26T01:00:00Z')
        api.rows[f'repos/{REPO}/actions/runs/{RUN}/attempts/1'] = previous
        api.rows[f'repos/{REPO}/actions/runs/{RUN}/attempts/1/jobs?per_page=100&page=1'] = {'total_count': 1, 'jobs': [original]}
        for job in (reported, original):
            api.rows[f'repos/{REPO}/actions/jobs/{job["id"]}/logs'] = b'2026-11-26T00:07:00Z complete native preflight log\n'
        return api, reported, previous, original

    def test_inherited_preflight_is_bound_to_actual_attempt_and_raw_fingerprint_not_job_id(self):
        api, reported, previous, original = self.fixture()
        proof = POLICY.Evidence(api, REPO, lambda _: True).verify(RUN, ATTEMPT)
        source = proof['preflight_provenance']
        self.assertEqual(proof['run_attempt'], 2)
        self.assertTrue(source['inherited'])
        self.assertEqual(source['reported'], {'run_id': RUN, 'run_attempt': 2, 'job_id': 140027648923})
        self.assertEqual(source['actual'], {'run_id': RUN, 'run_attempt': 1, 'job_id': 139992361691})
        self.assertEqual(source['source']['sha'], SHA)
        self.assertEqual(source['source']['path'], '.github/workflows/ci.yml')
        self.assertEqual(source['source']['blob'], api.rows[f'repos/{REPO}/contents/.github/workflows/ci.yml?ref={SHA}']['sha'])
        self.assertEqual(source['log_sha256'], hashlib.sha256(api.rows[f'repos/{REPO}/actions/jobs/{original["id"]}/logs']).hexdigest())
        self.assertLess(POLICY.timestamp(source['completed_at']), POLICY.timestamp(api.run['run_started_at']))
        self.assertLessEqual(len(set(api.calls)), 12)

    def test_control_ref_alias_is_not_a_different_source_but_multiple_actual_windows_are_ambiguous(self):
        api, _, previous, _ = self.fixture()
        previous['path'] += '@refs/heads/master'
        self.assertEqual(POLICY.Evidence(api, REPO, lambda _: True).verify(RUN, ATTEMPT)['preflight_provenance']['actual']['run_attempt'], 1)
        api, _, previous, original = self.fixture()
        api.run['run_attempt'] = 3
        for job in api.jobs: job['run_attempt'] = 3
        api.rows[f'repos/{REPO}/actions/runs/{RUN}/attempts/3'] = api.run
        api.rows[f'repos/{REPO}/actions/runs/{RUN}/attempts/3/jobs?per_page=100&page=1'] = {'total_count': len(api.jobs), 'jobs': api.jobs}
        ambiguous = copy.deepcopy(previous); ambiguous['run_attempt'] = 2
        duplicate = copy.deepcopy(original); duplicate.update(run_attempt=2, id=139992361692,
            html_url=f'https://github.com/{REPO}/actions/runs/{RUN}/job/139992361692')
        api.rows[f'repos/{REPO}/actions/runs/{RUN}/attempts/2'] = ambiguous
        api.rows[f'repos/{REPO}/actions/runs/{RUN}/attempts/2/jobs?per_page=100&page=1'] = {'total_count': 1, 'jobs': [duplicate]}
        api.rows[f'repos/{REPO}/actions/jobs/{duplicate["id"]}/logs'] = api.rows[f'repos/{REPO}/actions/jobs/{original["id"]}/logs']
        with self.assertRaisesRegex(ValueError, '不唯一'):
            POLICY.Evidence(api, REPO, lambda _: True).verify(RUN, 3)

    def test_inherited_preflight_mismatch_ambiguity_and_missing_evidence_never_prove_full(self):
        for case in ('head', 'status', 'time', 'runner', 'steps', 'step-status', 'logs', 'duplicate',
                     'missing-run', 'missing-jobs', 'missing-log', 'missing-reported-log', 'missing-steps'):
            api, reported, previous, original = self.fixture()
            if case == 'head': previous['head_sha'] = BASE
            elif case == 'status': original['conclusion'] = 'failure'
            elif case == 'time': original['completed_at'] = '2026-11-26T00:11:00Z'
            elif case == 'runner': original['runner_id'] += 1
            elif case == 'steps': original['steps'].pop()
            elif case == 'step-status': original['steps'][0]['conclusion'] = 'failure'
            elif case == 'logs': api.rows[f'repos/{REPO}/actions/jobs/{original["id"]}/logs'] += b'changed\n'
            elif case == 'duplicate':
                duplicate = {**original, 'id': original['id'] + 1, 'html_url': f'https://github.com/{REPO}/actions/runs/{RUN}/job/{original["id"] + 1}'}
                api.rows[f'repos/{REPO}/actions/runs/{RUN}/attempts/1/jobs?per_page=100&page=1'] = {'total_count': 2, 'jobs': [original, duplicate]}
            elif case == 'missing-run': api.rows.pop(f'repos/{REPO}/actions/runs/{RUN}/attempts/1')
            elif case == 'missing-jobs': api.rows.pop(f'repos/{REPO}/actions/runs/{RUN}/attempts/1/jobs?per_page=100&page=1')
            elif case == 'missing-log': api.rows.pop(f'repos/{REPO}/actions/jobs/{original["id"]}/logs')
            elif case == 'missing-reported-log': api.rows.pop(f'repos/{REPO}/actions/jobs/{reported["id"]}/logs')
            elif case == 'missing-steps': reported.pop('steps')
            with self.subTest(case=case), self.assertRaises(ValueError):
                POLICY.Evidence(api, REPO, lambda _: True).verify(RUN, ATTEMPT)
        for index in (0, 1):
            api, _, _, _ = self.fixture()
            api.jobs[index].update(started_at='2026-11-26T00:05:00Z', completed_at='2026-11-26T00:10:00Z')
            with self.subTest(inherited_producer=index), self.assertRaises(ValueError):
                POLICY.Evidence(api, REPO, lambda _: True).verify(RUN, ATTEMPT)


class ReleaseLifecycleTests(unittest.TestCase):
    def fixture(self):
        api = FakeAPI('release', event='workflow_dispatch', trigger_sha=BASE)
        api.run.update(status='in_progress', conclusion=None, updated_at='2026-11-26T00:05:00Z')
        api.jobs[0].update(started_at='2026-11-26T00:30:00Z', completed_at='2026-11-26T00:45:00Z')
        api.jobs[1].update(started_at='2026-11-26T00:10:00Z', completed_at='2026-11-26T00:20:00Z')
        api.artifact['created_at'] = '2026-11-26T00:40:00Z'
        machine = api.job('Machine 6A', 103, None)
        machine.update(status='in_progress', completed_at=None)
        api.jobs.append(machine)
        api.rows[f'repos/{REPO}/actions/runs/{RUN}/attempts/{ATTEMPT}/jobs?per_page=100&page=1']['total_count'] = len(api.jobs)
        return api

    def test_completed_current_full_can_be_reused_while_machine_waits_and_run_updated_at_is_stale(self):
        api = self.fixture()
        observed = POLICY.timestamp('2026-11-26T02:00:00Z')
        with mock.patch.object(POLICY, 'datetime', wraps=POLICY.datetime) as clock:
            clock.now.return_value = observed
            proof = POLICY.Evidence(api, REPO, lambda _: True).verify(RUN, ATTEMPT)
        self.assertEqual((proof['sha'], proof['run_attempt'], proof['producer_job_id']), (SHA, ATTEMPT, 100))
        self.assertIsNone(api.run['conclusion'])
        self.assertEqual(api.jobs[-1]['status'], 'in_progress')
        self.assertNotIn('released', proof)
        self.assertNotIn('machines', proof)

    def test_open_release_requires_real_current_full_producers_and_available_bound_artifact(self):
        for case in ('producer-running', 'producer-failed', 'aggregate-running', 'aggregate-failed', 'old-producer', 'old-aggregate',
                     'future-producer', 'wrong-sha', 'wrong-digest', 'missing-artifact', 'before-producer', 'after-producer', 'queued'):
            api = self.fixture()
            if case == 'producer-running': api.jobs[0].update(status='in_progress', conclusion=None)
            elif case == 'producer-failed': api.jobs[0]['conclusion'] = 'failure'
            elif case == 'aggregate-running': api.jobs[1].update(status='in_progress', conclusion=None)
            elif case == 'aggregate-failed': api.jobs[1]['conclusion'] = 'failure'
            elif case == 'old-producer': api.jobs[0]['started_at'] = '2026-11-25T23:00:00Z'
            elif case == 'old-aggregate': api.jobs[1]['started_at'] = '2026-11-25T23:00:00Z'
            elif case == 'future-producer': api.jobs[0]['completed_at'] = '2026-11-26T03:00:00Z'
            elif case == 'wrong-sha': api.set_artifact({**api.receipt, 'sha': BASE}, 'release-full')
            elif case == 'wrong-digest': api.artifact['digest'] = 'sha256:' + '0' * 64
            elif case == 'missing-artifact': api.rows.pop(f'repos/{REPO}/actions/artifacts/77/zip')
            elif case == 'before-producer': api.artifact['created_at'] = '2026-11-26T00:29:00Z'
            elif case == 'after-producer': api.artifact['created_at'] = '2026-11-26T00:46:00Z'
            elif case == 'queued': api.run['status'] = 'queued'
            observed = POLICY.timestamp('2026-11-26T02:00:00Z')
            with self.subTest(case=case), mock.patch.object(POLICY, 'datetime', wraps=POLICY.datetime) as clock:
                clock.now.return_value = observed
                with self.assertRaises(ValueError): POLICY.Evidence(api, REPO, lambda _: True).verify(RUN, ATTEMPT)
        for kind in ('legacy', 'periodic'):
            api = FakeAPI(kind); api.run['status'] = 'in_progress'
            with self.subTest(kind=kind), self.assertRaises(ValueError):
                POLICY.Evidence(api, REPO, lambda _: True).verify(RUN, ATTEMPT)


class SkipLocatorTests(unittest.TestCase):
    def fixture(self, count=100):
        api = FakeAPI('periodic')
        api.run.update(run_started_at='2026-11-01T00:00:00Z', updated_at='2026-11-01T01:00:00Z')
        for job in api.jobs:
            job.update(started_at=api.run['run_started_at'], completed_at=api.run['updated_at'])
        api.rows[f'repos/{REPO}/actions/jobs/100/logs'] = b'2026-11-01T00:30:00Z ' + json.dumps(api.receipt).encode() + b'\n'
        api.artifact['created_at'] = '2026-11-01T00:30:00Z'
        pointer = {**identity(), 'verified': True}
        runs, receipts = [], []
        for index in range(count):
            run_id, job_id, artifact_id = 2000 + index, 10000 + index, 5000 + index
            start = POLICY.timestamp(api.run['run_started_at']) + timedelta(hours=6 * (index + 1))
            stamp = lambda minutes: (start + timedelta(minutes=minutes)).isoformat().replace('+00:00', 'Z')
            run = {**api.run, 'id': run_id, 'run_attempt': 1, 'run_started_at': stamp(0), 'updated_at': stamp(10)}
            target = {**identity(), 'run_id': run_id, 'run_attempt': 1}
            needs = full_needs()
            for key in ('preflight', 'full'): needs[key] = {'result': 'skipped'}
            skipped = POLICY.receipt(target, needs, 'skip', copy.deepcopy(pointer))
            job = {**api.job('Periodic Full evidence', job_id), 'run_id': run_id, 'run_attempt': 1,
                   'started_at': stamp(1), 'completed_at': stamp(9),
                   'html_url': f'https://github.com/{REPO}/actions/runs/{run_id}/job/{job_id}'}
            data = io.BytesIO()
            with zipfile.ZipFile(data, 'w') as archive: archive.writestr('receipt.json', json.dumps(skipped))
            raw = data.getvalue()
            artifact = {**api.artifact, 'id': artifact_id, 'name': f'periodic-full-{run_id}-1', 'created_at': stamp(5),
                        'digest': 'sha256:' + hashlib.sha256(raw).hexdigest(),
                        'workflow_run': {**api.artifact['workflow_run'], 'id': run_id}}
            api.rows.update({f'repos/{REPO}/actions/runs/{run_id}/attempts/1': run,
                             f'repos/{REPO}/actions/runs/{run_id}/attempts/1/jobs?per_page=100&page=1': {'total_count': 1, 'jobs': [job]},
                             f'repos/{REPO}/actions/jobs/{job_id}/logs': stamp(5).encode() + b' ' + json.dumps(skipped).encode() + b'\n',
                             f'repos/{REPO}/actions/runs/{run_id}/artifacts?per_page=100&page=1': {'total_count': 1, 'artifacts': [artifact]},
                             f'repos/{REPO}/actions/artifacts/{artifact_id}/zip': raw})
            runs.append(run); receipts.append(skipped)
        ordered = list(reversed(runs)) + [api.run]
        for page in range(1, (len(ordered) + 99) // 100 + 1):
            api.rows[f'repos/{REPO}/actions/workflows/periodic-full.yml/runs?per_page=100&page={page}'] = {
                'total_count': len(ordered), 'workflow_runs': ordered[(page - 1) * 100:page * 100]}
        for name in ('ci.yml', 'release-candidate.yml'):
            api.rows[f'repos/{REPO}/actions/workflows/{name}/runs?per_page=100&page=1'] = {'total_count': 0, 'workflow_runs': []}
        return api, runs[-1], receipts[-1]

    def test_100_retained_skips_locate_one_independently_verified_full_without_history_scan(self):
        api, latest, _ = self.fixture()
        evidence = POLICY.Evidence(api, REPO, lambda _: True)
        self.assertEqual(evidence.verify(RUN, ATTEMPT)['sha'], SHA)
        api.calls.clear()
        found, reasons = evidence.search([SHA] + [f'{index + 1:040x}' for index in range(257)])
        self.assertEqual((found[SHA]['run_id'], found[SHA]['run_attempt']), (RUN, ATTEMPT))
        self.assertLess(len(set(api.calls)), 24)
        self.assertFalse(any('/workflows/periodic-full.yml/runs?per_page=100&page=2' in call for call in api.calls))
        self.assertTrue(any(f'/actions/runs/{latest["id"]}/attempts/1' in call for call in api.calls))
        self.assertFalse(any('/actions/runs/2098/attempts/1' in call for call in api.calls))

    def test_skip_is_never_a_full_and_forged_missing_or_recursive_locators_fail_closed(self):
        for case in ('cross-repo', 'wrong-sha', 'wrong-tree', 'invalid-attempt', 'self', 'missing', 'unavailable', 'cycle'):
            api, latest, receipt = self.fixture(count=2)
            evidence = POLICY.Evidence(api, REPO, lambda _: True)
            with self.assertRaises(POLICY.SkippedFull): evidence.verify(latest['id'], 1)
            forged = copy.deepcopy(receipt)
            pointer = forged['skip_proof']
            if case == 'cross-repo': pointer['repo'] = 'other/repo'
            elif case == 'wrong-sha': pointer['sha'] = BASE
            elif case == 'wrong-tree': pointer['tree'] = BASE
            elif case == 'invalid-attempt': pointer['run_attempt'] = True
            elif case == 'self': pointer.update(run_id=latest['id'], run_attempt=1)
            elif case == 'missing': forged.pop('skip_proof')
            elif case == 'unavailable': api.rows.pop(f'repos/{REPO}/actions/runs/{RUN}/attempts/{ATTEMPT}')
            elif case == 'cycle': pointer.update(run_id=latest['id'] - 1, run_attempt=1)
            api.calls.clear()
            with self.subTest(case=case), self.assertRaises(ValueError): evidence.follow_skip(forged)
            if case == 'cycle':
                self.assertFalse(any(f'/actions/runs/{RUN}/attempts/{ATTEMPT}' in call for call in api.calls))
            self.assertLessEqual(len(set(api.calls)), 24)

    def test_locator_cannot_substitute_an_independently_valid_full_for_another_target(self):
        api, _, receipt = self.fixture(count=1)
        target = {**identity(), 'sha': BASE, 'tree': 'd' * 40, 'head': BASE, 'base': 'e' * 40}
        other = {**api.receipt, **target, 'checkout': {'sha': BASE, 'tree': 'd' * 40}, 'full': {**target, 'verified': True}}
        api.rows[f'repos/{REPO}/commits/{BASE}'] = {'sha': BASE, 'commit': {'tree': {'sha': 'd' * 40}}, 'parents': [{'sha': 'e' * 40}]}
        api.rows[f'repos/{REPO}/actions/jobs/100/logs'] = b'2026-11-01T00:30:00Z ' + json.dumps(other).encode() + b'\n'
        api.set_artifact(other, 'periodic-full'); api.artifact['created_at'] = '2026-11-01T00:30:00Z'
        evidence = POLICY.Evidence(api, REPO, lambda _: True)
        self.assertEqual(evidence.verify(RUN, ATTEMPT)['sha'], BASE)
        with self.assertRaisesRegex(ValueError, '目标不符'): evidence.follow_skip(receipt)

    def test_failed_hint_is_used_once_and_normal_full_discovery_can_continue(self):
        api, _, _ = self.fixture(count=3)
        evidence = POLICY.Evidence(api, REPO, lambda _: True)
        with mock.patch.object(evidence, 'follow_skip', side_effect=ValueError('定位提示缺少证据')) as hint:
            found, reasons = evidence.search([SHA])
        hint.assert_called_once()
        self.assertEqual(found[SHA]['run_id'], RUN)
        self.assertTrue(any('定位提示缺少证据' in reason for reason in reasons))

    def test_skip_authentication_and_per_hint_api_bound_remain_mandatory(self):
        for case in ('digest', 'artifact', 'log'):
            api, latest, receipt = self.fixture(count=1)
            if case == 'digest':
                api.rows[f'repos/{REPO}/actions/runs/{latest["id"]}/artifacts?per_page=100&page=1']['artifacts'][0]['digest'] = 'sha256:' + '0' * 64
            elif case == 'artifact': api.rows.pop(f'repos/{REPO}/actions/artifacts/5000/zip')
            elif case == 'log': api.rows[f'repos/{REPO}/actions/jobs/10000/logs'] = b'no canonical receipt\n'
            with self.subTest(case=case):
                try:
                    POLICY.Evidence(api, REPO, lambda _: True).verify(latest['id'], 1)
                except POLICY.SkippedFull:
                    self.fail('Unauthenticated skip supplied a locator')
                except ValueError:
                    pass
                else:
                    self.fail('A skip was accepted as a Full')
        api, _, receipt = self.fixture(count=1)
        get = api.get
        def flood(endpoint, raw=False):
            if f'/actions/runs/{RUN}/attempts/{ATTEMPT}/jobs?' in endpoint:
                api.calls.append(endpoint)
                return {'total_count': 10000, 'jobs': [api.job(f'Native job {index}', 1000 + index) for index in range(100)]}
            return get(endpoint, raw=raw)
        with mock.patch.object(api, 'get', side_effect=flood), self.assertRaisesRegex(ValueError, '24 个 GET'):
            POLICY.Evidence(api, REPO, lambda _: True).follow_skip(receipt)
        self.assertEqual(len(set(api.calls)), 24)


class HistoryLookupTests(unittest.TestCase):
    def test_proving_jobs_and_canonical_logs_must_be_fresh_not_just_current_api_labels(self):
        for kind in ('legacy', 'periodic', 'release'):
            for index in (0, 1, 2) if kind != 'release' else (0, 1):
                api = FakeAPI(kind, event='workflow_dispatch' if kind == 'release' else None)
                api.jobs[index]['started_at'] = '2026-11-25T23:00:00Z'
                api.jobs[index]['completed_at'] = '2026-11-25T23:59:00Z'
                with self.subTest(kind=kind, index=index), self.assertRaises(ValueError):
                    POLICY.Evidence(api, REPO, lambda _: True).verify(RUN, ATTEMPT)
        for mutate in (lambda api: api.jobs[1].update(name='Unrelated / Full suite evidence'),
                       lambda api: api.run.update(referenced_workflows=[]),
                       lambda api: api.run['referenced_workflows'][0].update(sha=BASE),
                       lambda api: api.jobs[0].pop('started_at'),
                       lambda api: api.rows.update({f'repos/{REPO}/actions/jobs/100/logs':
                            b'2026-11-25T23:00:00Z ' + json.dumps(api.receipt).encode()})):
            api = FakeAPI()
            mutate(api)
            with self.assertRaises(ValueError): POLICY.Evidence(api, REPO, lambda _: True).verify(RUN, ATTEMPT)
        # An old unrelated leaf does not invalidate genuinely fresh proving jobs.
        api = FakeAPI(); leaf = api.job('Unrelated leaf', 999)
        leaf.update(started_at='2026-11-25T23:00:00Z', completed_at='2026-11-25T23:01:00Z')
        api.jobs.append(leaf)
        api.rows[f'repos/{REPO}/actions/runs/{RUN}/attempts/{ATTEMPT}/jobs?per_page=100&page=1']['total_count'] += 1
        self.assertEqual(POLICY.Evidence(api, REPO, lambda _: True).verify(RUN, ATTEMPT)['sha'], SHA)

    def test_success_proof_reads_all_job_and_artifact_pages(self):
        api = FakeAPI('periodic')
        jobs = api.jobs + [api.job(f'Unrelated {index}', 1000 + index) for index in range(200)]
        artifacts = [{**api.artifact, 'id': 1000 + index, 'name': f'unrelated-{index}'} for index in range(220)] + [api.artifact]
        for rows, key, path in ((jobs, 'jobs', f'actions/runs/{RUN}/attempts/{ATTEMPT}/jobs'),
                                (artifacts, 'artifacts', f'actions/runs/{RUN}/artifacts')):
            for page in range(1, 4):
                api.rows[f'repos/{REPO}/{path}?per_page=100&page={page}'] = {
                    'total_count': len(rows), key: rows[(page - 1) * 100:page * 100]}
        proof = POLICY.Evidence(api, REPO, lambda _: True).verify(RUN, ATTEMPT)
        self.assertEqual(proof['sha'], SHA)
        self.assertEqual(sum('page=3' in path for path in api.calls), 2)

    def test_missing_ancestor_queries_cannot_starve_direct_periodic_proof(self):
        api = FakeAPI('periodic')
        api.rows[f'repos/{REPO}/actions/workflows/periodic-full.yml/runs?per_page=100&page=1'] = {
            'total_count': 2, 'workflow_runs': [api.run, {**api.run, 'id': RUN + 1}]}
        evidence = POLICY.Evidence(api, REPO, lambda _: True)
        get = api.get
        def budgeted(endpoint, raw=False):
            if len(api.calls) >= 6: raise POLICY.BudgetExceeded('真实 GET 预算耗尽')
            return get(endpoint, raw=raw)
        def verify(run_id, attempt):
            if run_id == RUN: raise ValueError('缺少成功证明')
            return {**identity(), 'verified': True}
        chain = [SHA] + [f'{index + 1:040x}' for index in range(1000)]
        with mock.patch.object(api, 'get', side_effect=budgeted), mock.patch.object(evidence, 'verify', side_effect=verify):
            found, reasons = evidence.search(chain)
        self.assertIn(SHA, found)
        self.assertLessEqual(len(api.calls), 6)

    def test_pages_and_attempts_have_no_historical_count_cutoff(self):
        api = FakeAPI('periodic')
        runs = [{**api.run, 'id': 8000 + index, 'run_attempt': 1} for index in range(221)]
        runs[-1]['run_attempt'] = 7
        for page in range(1, 4):
            api.rows[f'repos/{REPO}/actions/workflows/periodic-full.yml/runs?per_page=100&page={page}'] = {
                'total_count': len(runs), 'workflow_runs': runs[(page - 1) * 100:page * 100]}
        evidence = POLICY.Evidence(api, REPO, lambda _: True)
        tried = []
        def verify(run_id, attempt):
            tried.append((run_id, attempt))
            if (run_id, attempt) == (8220, 1): return {**identity(), 'verified': True}
            raise ValueError('未形成 Full 证明')
        with mock.patch.object(evidence, 'verify', side_effect=verify):
            found, reasons = evidence.search([SHA, BASE])
        self.assertEqual(found[SHA]['sha'], SHA)
        self.assertGreater(len(tried), 220)
        self.assertIn((8220, 7), tried)
        self.assertIn('repos/' + REPO + '/actions/workflows/periodic-full.yml/runs?per_page=100&page=3', api.calls)
        self.assertTrue(reasons)

    def test_noisy_recent_page_uses_exact_legacy_sha_and_nearest_real_prior(self):
        chain = [f'{index + 1:040x}' for index in range(70)]
        for direct in (True, False):
            api = FakeAPI()
            noise = [{**api.run, 'id': 8000 + index, 'head_sha': 'f' * 40} for index in range(100)]
            api.rows[f'repos/{REPO}/actions/workflows/ci.yml/runs?per_page=100&page=1'] = {
                'total_count': 5000, 'workflow_runs': noise}
            wanted = chain[0] if direct else chain[48]
            endpoint = f'repos/{REPO}/actions/workflows/ci.yml/runs?head_sha={wanted}&event=push&branch=master&per_page=100&page=1'
            api.rows[endpoint] = {'total_count': 1, 'workflow_runs': [{**api.run, 'head_sha': wanted}]}
            evidence = POLICY.Evidence(api, REPO, lambda _: True)
            with mock.patch.object(evidence, 'verify', return_value={**identity(), 'sha': wanted, 'verified': True}) as verified:
                found, reasons = evidence.search(chain)
            with self.subTest(direct=direct):
                self.assertEqual(set(found), {wanted})
                self.assertIn(endpoint, api.calls)
                verified.assert_called_once_with(RUN, ATTEMPT)
                self.assertFalse(any('page=2' in call for call in api.calls))
                if not direct: self.assertTrue(reasons)

    def test_real_api_or_time_budget_keeps_prior_proof_and_explicit_uncertainty(self):
        api = FakeAPI('periodic')
        api.rows[f'repos/{REPO}/actions/workflows/periodic-full.yml/runs?per_page=100&page=1'] = {
            'total_count': 2, 'workflow_runs': [api.run, {**api.run, 'id': RUN + 1}]}
        evidence = POLICY.Evidence(api, REPO, lambda _: True)
        def verify(run_id, attempt):
            if run_id == RUN: return {**identity(), 'sha': BASE, 'verified': True}
            raise POLICY.BudgetExceeded('真实 API 预算耗尽，未核验历史仍为 unknown')
        with mock.patch.object(evidence, 'verify', side_effect=verify):
            found, reasons = evidence.search([SHA, BASE])
        self.assertEqual(set(found), {BASE})
        self.assertTrue(any('预算耗尽' in reason for reason in reasons))
        with tempfile.TemporaryDirectory() as temporary:
            api = POLICY.API(Path(temporary), budget=0)
            with mock.patch.object(POLICY.subprocess, 'run') as command, self.assertRaises(POLICY.BudgetExceeded):
                api.get(f'repos/{REPO}')
            command.assert_not_called()


class ReplayTests(unittest.TestCase):
    def setUp(self):
        self.assertIsNotNone(REPLAY, "periodic-replay.py 尚未实现")

    def candidate(self):
        return {"package": "agentworkspace/cmd/workspace", "test": "TestRegression", "race": False,
                "signature": {"tests": ["TestRegression"], "messages": ["deterministic regression"]},
                "execution": {"mode": "ordinary", "timeout_seconds": 600, "timeout_arg": "10m", "setup_seconds": 300,
                              "proof": []}}

    def test_empirical_plain_and_json_go_failures_bind_to_known_package(self):
        root = "TestRunExplanationParentCancelAfterSendKeepsExactJournal"
        log = (f"2026-11-24T08:37:11.5459693Z --- FAIL: {root} (3.09s)\n"
               "2026-11-24T08:37:11.5459693Z FAIL\tagentworkspace/cmd/workspace\t20.703s\n")
        parsed = REPLAY.failures(log)
        self.assertEqual([(row["package"], row["test"]) for row in parsed], [("agentworkspace/cmd/workspace", root)])
        root = "TestIntentPassCancellationAndQueryFailure"
        log = (f"--- FAIL: {root} (1.0s)\n    --- FAIL: {root}/cancel (0.5s)\n"
               "FAIL\tagentworkspace/internal/coordination\t345.465s\n")
        self.assertEqual(REPLAY.failures(log)[0]["signature"]["tests"], [root, root + "/cancel"])
        rows = [{"Action": "run", "Package": "agentworkspace/cmd/workspace", "Test": "TestRegression"},
                {"Action": "output", "Package": "agentworkspace/cmd/workspace", "Test": "TestRegression",
                 "Output": "    main_test.go:8: deterministic regression\n"},
                {"Action": "fail", "Package": "agentworkspace/cmd/workspace", "Test": "TestRegression"},
                {"Action": "fail", "Package": "agentworkspace/cmd/workspace"}]
        self.assertEqual(REPLAY.failures("\n".join(map(json.dumps, rows)))[0]["signature"], self.candidate()["signature"])
        self.assertEqual(REPLAY.failures(log.replace("agentworkspace/", "foreign/")), [])
        self.assertEqual(REPLAY.failures("panic: test timed out after 10m\n" + log), [])
        self.assertEqual(REPLAY.failures("unrelated mentions TestRegression\nFAIL\n"), [])

    def test_probe_log_validation_requires_complete_exact_named_lifecycle(self):
        candidate = self.candidate()
        package, test = candidate["package"], candidate["test"]
        def row(action, name=None, **fields):
            return {"Action": action, "Package": package, **({"Test": name} if name else {}), **fields}
        def text(rows):
            return "".join(json.dumps(value) + "\n" for value in rows)
        listing = text([row("start"), row("output", Output=test + "\n"), row("pass")])
        good = [row("start"), row("run", test), row("pass", test), row("pass")]
        self.assertEqual(REPLAY.assess(listing, 0, text(good), 0, candidate)["state"], "pass")
        absent = text([row("start"), row("output", Output="ok\t" + package + "\n"), row("pass")])
        self.assertEqual(REPLAY.assess(absent, 0, "", None, candidate)["state"], "absent")
        for rows in (good[:-1], good[1:], [row("start"), row("pass")],
                     [row("start"), row("run", test), row("skip", test), row("pass")],
                     [row("start"), row("run", test), row("run", test), row("pass", test), row("pass")],
                     [row("start"), row("run", test + "Extra"), row("pass", test + "Extra"), row("pass")]):
            self.assertEqual(REPLAY.assess(listing, 0, text(rows), 0, candidate)["state"], "unknown", rows)
        self.assertEqual(REPLAY.assess("toolchain unavailable", 1, "", None, candidate)["state"], "unknown")
        assertion = [row("start"), row("run", test), row("output", test, Output="main_test.go:1: context deadline exceeded\n"),
                     row("fail", test), row("fail")]
        self.assertEqual(REPLAY.assess(listing, 0, text(assertion), 1, candidate)["state"], "fail")
        timed_out = copy.deepcopy(assertion)
        timed_out[2]["Output"] = "panic: test timed out after 10m0s\n"
        self.assertEqual(REPLAY.assess(listing, 0, text(timed_out), 1, candidate)["state"], "unknown")
        mixed = assertion[:-1] + [row('output', Output='panic: test timed out after 10m0s\n'), row('fail')]
        self.assertEqual(REPLAY.assess(listing, 0, text(mixed), 1, candidate)['state'], 'unknown')

    def test_screen_entire_first_parent_chain_and_repeat_pair_not_binary_search(self):
        chain = [BASE, "d" * 40, SHA]
        calls = []
        def probe(sha, candidate):
            calls.append(sha)
            return {"state": "pass" if sha == BASE else "fail", "signature": candidate["signature"]}
        result = REPLAY.localize(chain, self.candidate(), probe)
        self.assertEqual(result["localization"], "reproduced")
        self.assertEqual((result["good_sha"], result["commit"]), (BASE, "d" * 40))
        self.assertTrue(all(calls.count(sha) == 3 for sha in (BASE, "d" * 40, SHA)))
        longer = [BASE, "1" * 40, "2" * 40, "d" * 40, "3" * 40, SHA]
        calls.clear()
        def later(sha, candidate):
            calls.append(sha)
            return {"state": "pass" if longer.index(sha) < 3 else "fail", "signature": candidate["signature"]}
        self.assertEqual(REPLAY.localize(longer, self.candidate(), later)["localization"], "reproduced")
        self.assertLessEqual(calls.count("1" * 40), 1)
        self.assertEqual(calls.count("3" * 40), 1)
        self.assertEqual(calls.count("2" * 40), 3)
        self.assertEqual(calls.count("d" * 40), 3)
        self.assertNotIn("pr", result)

    def test_intermittent_absent_missing_infra_and_multiple_transitions_are_unknown(self):
        chain = [BASE, "d" * 40, "e" * 40, SHA]
        for states in (["pass", "unknown", "fail", "fail"],
                       ["pass", "absent", "fail", "fail"], ["pass", "pass", "pass", "pass"]):
            def probe(sha, candidate):
                return {"state": states[chain.index(sha)], "signature": candidate["signature"]}
            with self.subTest(states=states):
                self.assertNotEqual(REPLAY.localize(chain, self.candidate(), probe).get("localization"), "reproduced")
        repaired = REPLAY.localize(chain, self.candidate(), lambda value, candidate: {
            "state": "pass" if value in (BASE, "e" * 40) else "fail", "signature": candidate["signature"]})
        self.assertEqual((repaired["commit"], repaired["scope"]), (SHA, "latest-continuous-failing-episode"))
        counts = {}
        def intermittent(sha, candidate):
            counts[sha] = counts.get(sha, 0) + 1
            return {"state": "pass" if sha == BASE or counts[sha] % 2 == 0 else "fail", "signature": candidate["signature"]}
        self.assertNotEqual(REPLAY.localize([BASE, SHA], self.candidate(), intermittent).get("localization"), "reproduced")
        def changed_signature(sha, candidate):
            return {"state": "pass" if sha == BASE else "fail", "signature": {"tests": ["TestOther"], "messages": []}}
        self.assertNotEqual(REPLAY.localize([BASE, SHA], self.candidate(), changed_signature).get("localization"), "reproduced")

    def test_exact_first_failing_test_introduction_is_distinct(self):
        def probe(sha, candidate):
            return {"state": "absent" if sha == BASE else "fail", "signature": candidate["signature"],
                    "definition": {"state": "absent" if sha == BASE else "present"}}
        result = REPLAY.localize([BASE, SHA], self.candidate(), probe)
        self.assertEqual(result["transition"], "new-test")
        self.assertEqual(result["localization"], "reproduced")
        def excluded_test(sha, candidate):
            return {**probe(sha, candidate), "definition": {"state": "present"}}
        self.assertNotEqual(REPLAY.localize([BASE, SHA], self.candidate(), excluded_test).get("localization"), "reproduced")

    @unittest.skipUnless(shutil.which("go"), "Go fixture requires a local Go toolchain")
    def test_real_git_history_exact_named_go_test_and_secret_free_child_environment(self):
        with tempfile.TemporaryDirectory(prefix="periodic-fixture-") as directory:
            root = Path(directory) / "repo"
            root.mkdir()
            subprocess.run(["git", "init", "-q", str(root)], check=True)
            def git(*args, data=None):
                return subprocess.check_output(["git", "-C", str(root), *args], input=data).decode().strip()
            def commit(failing, parent=None):
                files = {"go.mod": "module agentworkspace\n\ngo 1.20\n",
                         "cmd/workspace/main_test.go": 'package main\nimport ("testing"; "os")\n'
                         'func TestRegression(t *testing.T) {\n'
                         ' for _, k := range []string{"GH_TOKEN", "GITHUB_TOKEN", "ACTIONS_RUNTIME_TOKEN", "PRIVATE_SECRET"} {'
                         ' if os.Getenv(k) != "" { t.Fatal("credential leaked") } }\n'
                         + (' t.Fatal("deterministic regression")\n' if failing else '') + '}\n'
                         'func TestRegressionExtra(t *testing.T) { t.Fatal("must not run") }\n'}
                # Objects in a disposable repository; no project commits or branch writes.
                def tree(entries):
                    nodes = {}
                    for path, content in entries.items():
                        part, sep, rest = path.partition("/")
                        if sep:
                            nodes.setdefault(part, {})[rest] = content
                        else:
                            nodes[part] = git("hash-object", "-w", "--stdin", data=content.encode())
                    rows = []
                    for name, node in sorted(nodes.items()):
                        mode, kind, oid = ("040000", "tree", tree(node)) if isinstance(node, dict) else ("100644", "blob", node)
                        rows.append(f"{mode} {kind} {oid}\t{name}\0")
                    return git("mktree", "-z", data="".join(rows).encode())
                data = f"tree {tree(files)}\n" + (f"parent {parent}\n" if parent else "")
                data += "author Fixture <ci@example.invalid> 1 +0000\ncommitter Fixture <ci@example.invalid> 1 +0000\n\nfixture\n"
                return git("hash-object", "-w", "-t", "commit", "--stdin", data=data.encode())
            good = commit(False)
            bad = commit(True, good)
            git("update-ref", "refs/heads/master", bad)
            with mock.patch.dict(os.environ, {key: "must-not-leak" for key in
                                             ("GH_TOKEN", "GITHUB_TOKEN", "ACTIONS_RUNTIME_TOKEN", "PRIVATE_SECRET")}):
                runner = REPLAY.Probe(root, Path(directory) / "evidence", budget=1800)
                try:
                    result = REPLAY.localize([good, bad], self.candidate(), runner)
                finally:
                    runner.close()
            tails = []
            for path in sorted((Path(directory) / 'evidence').glob('*.jsonl'))[-4:]:
                with path.open('rb') as stream:
                    stream.seek(max(0, path.stat().st_size - 2048))
                    tails.append(path.name + ':\n' + stream.read(2048).decode(errors='replace'))
            diagnosis = json.dumps(result, ensure_ascii=False, sort_keys=True) + '\n' + '\n'.join(tails)
            self.assertEqual(result.get('localization'), 'reproduced', diagnosis)
            self.assertEqual(result.get('commit'), bad, diagnosis)
            for group in result['probes']:
                raw_source = subprocess.check_output(['git', '-C', str(root), 'show', group['sha'] + ':cmd/workspace/main_test.go'])
                self.assertTrue(raw_source.endswith(b'\n'))
                self.assertEqual(group['results'][0]['definition']['files']['cmd/workspace/main_test.go'], hashlib.sha256(raw_source).hexdigest())
            initial = {"sha": bad, "tree": git("rev-parse", bad + "^{tree}"),
                       "ancestor": {"sha": good, "tree": git("rev-parse", good + "^{tree}")},
                       "failure_candidates": [self.candidate()]}
            initial_dir = Path(directory) / "initial"
            initial_dir.mkdir()
            (initial_dir / "receipt.json").write_text(json.dumps(initial))
            result["initial_sha256"] = hashlib.sha256((initial_dir / "receipt.json").read_bytes()).hexdigest()
            verified = POLICY.verify_localization(initial, result, Path(directory) / "evidence", root)
            self.assertEqual(verified.get('localization'), 'reproduced', verified)
            corrupted = copy.deepcopy(result)
            corrupted["probes"][0]["results"][0]["state"] = "absent"
            with self.assertRaises(ValueError):
                POLICY.verify_localization(initial, corrupted, Path(directory) / "evidence", root)
            self.assertTrue(list((Path(directory) / "evidence").glob("*.jsonl")))
            self.assertFalse(any(path.name == "SHOULD_NOT_EXIST" for path in Path(directory).rglob("*")))
            with self.assertRaises(ValueError):
                runner(bad, {**self.candidate(), "test": "TestRegression;touch SHOULD_NOT_EXIST"})


class GoFailureScopeTests(unittest.TestCase):
    package = 'agentworkspace/internal/platform'

    def row(self, action, name=None, **fields):
        return {'Action': action, 'Package': self.package, **({'Test': name} if name else {}), **fields}

    def text(self, rows):
        return ''.join('2026-11-19T00:33:00.6548088Z ' + json.dumps(row) + '\n' for row in rows)

    def test_completed_json_assertions_survive_same_package_timeout_without_peer_messages(self):
        root, peer = 'TestCompleted', 'TestDistinct'
        events = [self.row('start'), self.row('run', root), self.row('run', root + '/failed'),
                  self.row('output', root + '/failed', Output='    fixture_test.go:8: own assertion: context deadline exceeded\n'),
                  self.row('fail', root + '/failed'), self.row('run', root + '/passed'),
                  self.row('output', root + '/passed', Output='    fixture_test.go:9: passing diagnostic\n'),
                  self.row('pass', root + '/passed'), self.row('fail', root), self.row('run', peer),
                  self.row('output', peer, Output='    peer_test.go:3: distinct assertion\n'), self.row('fail', peer),
                  self.row('run', 'TestUnfinished'),
                  self.row('output', 'TestUnfinished', Output='    pending_test.go:4: not a completed assertion\n'),
                  self.row('output', 'TestUnfinished', Output='panic: test timed out after 10m0s\n'), self.row('fail')]
        known, unknown = REPLAY.failure_records(self.text(events))
        self.assertEqual([(row['test'], row['signature']) for row in known], [
            (root, {'tests': [root, root + '/failed'], 'messages': ['own assertion: context deadline exceeded']}),
            (peer, {'tests': [peer], 'messages': ['distinct assertion']})])
        self.assertEqual([row['package'] for row in unknown], [self.package])

    def test_json_retention_requires_own_run_assertion_and_fail_before_interruption(self):
        root = 'TestCompleted'
        run = self.row('run', root)
        diagnostic = self.row('output', root, Output='    fixture_test.go:8: own assertion\n')
        fail = self.row('fail', root)
        timeout = self.row('output', 'TestUnfinished', Output='panic: test timed out after 10m0s\n')
        for events in ([diagnostic, fail], [run, diagnostic], [run, fail], [fail, run, diagnostic],
                       [run, timeout, diagnostic, fail], [run, fail, diagnostic],
                       [run, {**diagnostic, 'Test': 'TestPeer'}, fail],
                       [run, {**diagnostic, 'Package': 'agentworkspace/internal/store'}, fail],
                       [run, {key: value for key, value in diagnostic.items() if key != 'Test'}, fail],
                       [run, self.row('run', root + '/failed'), diagnostic, self.row('fail', root + '/failed')],
                       [run, self.row('run', root + '/failed'), diagnostic, fail, self.row('fail', root + '/failed')],
                       [run, diagnostic, self.row('fail', root + '/without-run'), fail],
                       [run, self.row('run', root), diagnostic, fail]):
            with self.subTest(events=events):
                known, unknown = REPLAY.failure_records(self.text(events + [timeout, self.row('fail')]))
                self.assertEqual(known, [])
                self.assertTrue(unknown)
        for action in ('pass', 'skip'):
            events = [self.row('start'), run, diagnostic, self.row(action, root), self.row(action)]
            self.assertEqual(REPLAY.failure_records(self.text(events)), ([], []))
        for terminal in ([], [self.row('pass')], [self.row('skip')]):
            self.assertEqual(REPLAY.failures(self.text([run, diagnostic, fail] + terminal)), [])

    def test_plain_buffered_and_verbose_scopes_keep_only_completed_failed_diagnostics(self):
        buffered = ('--- FAIL: TestCompleted (0.2s)\n'
                    '    --- FAIL: TestCompleted/failed (0.1s)\n'
                    '        fixture_test.go:8: own assertion: context deadline exceeded\n'
                    '    fixture_test.go:10: root assertion\n')
        verbose = ('=== RUN   TestCompleted\n=== RUN   TestCompleted/failed\n'
                   '    fixture_test.go:8: own assertion: context deadline exceeded\n'
                   '--- FAIL: TestCompleted/failed (0.1s)\n=== RUN   TestCompleted/passed\n'
                   '    fixture_test.go:9: passing diagnostic\n--- PASS: TestCompleted/passed (0.1s)\n'
                   '=== NAME  TestCompleted\n    fixture_test.go:10: root assertion\n'
                   '--- FAIL: TestCompleted (0.2s)\n')
        timeout = ('=== RUN   TestUnfinished\n    pending_test.go:4: not a completed assertion\n'
                   'panic: test timed out after 10m0s\n\trunning tests:\n\t\tTestUnfinished (1s)\n'
                   '--- FAIL: TestAfterPanic (0.1s)\n    after_test.go:5: interrupted output\n'
                   'FAIL\t' + self.package + '\t600.0s\n')
        parallel = verbose.replace('=== RUN   TestCompleted/failed\n',
                                   '=== RUN   TestCompleted/failed\n=== PAUSE TestCompleted/failed\n=== CONT  TestCompleted/failed\n')
        for log in (buffered, verbose, parallel):
            with self.subTest(log=log):
                known, unknown = REPLAY.failure_records(log + timeout)
                self.assertEqual(known, [{'package': self.package, 'test': 'TestCompleted', 'signature': {
                    'tests': ['TestCompleted', 'TestCompleted/failed'],
                    'messages': ['own assertion: context deadline exceeded', 'root assertion']}}])
                self.assertEqual([row['package'] for row in unknown], [self.package])

    def test_plain_success_skip_and_package_boundaries_do_not_supply_assertions(self):
        for terminal in ('PASS', 'SKIP'):
            log = (f'=== RUN   TestPeer\n    peer_test.go:1: peer diagnostic\n--- {terminal}: TestPeer (0.1s)\n'
                   '=== RUN   TestIncomplete\n    pending_test.go:2: unfinished diagnostic\n'
                   '--- FAIL: TestNoAssertion (0.1s)\npanic: test timed out after 10m0s\n'
                   'FAIL\t' + self.package + '\t600.0s\n')
            self.assertEqual(REPLAY.failures(log), [])
        for summary in ('ok  \t' + self.package + '\t0.1s\n', 'ok   ' + self.package + ' (cached)\n',
                        '?   \t' + self.package + '\t[no test files]\n'):
            earlier = '--- FAIL: TestStale (0.1s)\n    stale_test.go:1: stale assertion\n' + summary
            self.assertEqual(REPLAY.failure_records(earlier), ([], []))
            later = 'panic: test timed out after 10m0s\nFAIL\tagentworkspace/internal/store\t600.0s\n'
            self.assertEqual(REPLAY.failures(earlier + later), [])

    def test_distinct_package_executions_do_not_merge_signatures_or_protocols(self):
        root = 'TestCompleted'
        plain, events = '', []
        for diagnostic in ('first assertion', 'second assertion'):
            plain += f'--- FAIL: {root} (0.1s)\n    fixture_test.go:1: {diagnostic}\nFAIL\t{self.package}\t0.1s\n'
            events += [self.row('start'), self.row('run', root),
                       self.row('output', root, Output=f'    fixture_test.go:1: {diagnostic}\n'),
                       self.row('fail', root), self.row('fail')]
        for log in (plain, self.text(events), plain + self.text(events)):
            known, unknown = REPLAY.failure_records(log)
            self.assertEqual([row['signature']['messages'] for row in known], [['first assertion'], ['second assertion']])
            self.assertEqual(unknown, [])
        later_timeout = self.text([self.row('start'), self.row('output', Output='panic: test timed out after 10m0s\n'), self.row('fail')])
        known, unknown = REPLAY.failure_records(self.text(events) + later_timeout)
        self.assertEqual([row['signature']['messages'] for row in known], [['first assertion'], ['second assertion']])
        self.assertEqual([row['package'] for row in unknown], [self.package])
        unbound = '--- FAIL: TestUnbound (0.1s)\n    unbound_test.go:1: unbound assertion\n'
        known, unknown = REPLAY.failure_records(unbound + self.text(events))
        self.assertEqual([row['test'] for row in known], [root, root])
        self.assertTrue(any(row['package'] is None for row in unknown))

    def test_partial_protocol_streams_cannot_borrow_lifecycle_across_boundaries(self):
        root = 'TestJSON'
        prefix = [self.row('run', root), self.row('output', root, Output='    fixture_test.go:1: JSON assertion\n')]
        suffix = [self.row('fail', root), self.row('fail')]
        plain = ('--- FAIL: TestPlain (0.1s)\n    plain_test.go:2: plain assertion\n'
                 'FAIL\tagentworkspace/internal/store\t0.1s\n')
        known, unknown = REPLAY.failure_records(self.text(prefix) + plain + self.text(suffix))
        self.assertEqual([(row['package'], row['test']) for row in known], [('agentworkspace/internal/store', 'TestPlain')])
        self.assertEqual([row['package'] for row in unknown], [self.package])
        unbound = '--- FAIL: TestUnbound (0.1s)\n    plain_test.go:2: unbound assertion\n'
        known, unknown = REPLAY.failure_records(unbound + self.text(prefix + suffix) + 'FAIL\tagentworkspace/internal/store\t0.1s\n')
        self.assertEqual([row['test'] for row in known], [root])
        self.assertEqual({row['package'] for row in unknown}, {None, 'agentworkspace/internal/store'})

    def test_original_archived_same_package_failures(self):
        directory = os.environ.get('PERIODIC_GO_ARCHIVES')
        if not directory:
            self.skipTest('未提供 PERIODIC_GO_ARCHIVES；跳过原始断言/超时归档验证')
        # Independent judge mappings: run/attempt, ZIP member, raw SHA-256, completed root.
        cases = [
            ('44135947898-2', '0_Go.txt', '8efe8a8b5cbe25df9e6194d95a00cac5544e78bdeaae7df5b71bf26c71e232e9',
             'agentworkspace/internal/httpapi', 'TestMemberMCPCodexMetadataCoordinatorDecision', 1),
            ('44422002958-1', '36_Go Platform 3.txt', 'e49fd5b5bd08193655c7f5df1c6fa1102b40ee820d269b4900fad8cfcab424e6',
             self.package, 'TestInvestigationTaskWorkLaunchSQLite', 4),
            ('44427678293-1', '64_Go Platform 4.txt', 'b27aa19354b6899602a57fe26d030c201e05000a97adf881a93542936e88d70a',
             self.package, 'TestInvestigationOwnerManagementRequiresOriginalUserAdmission', 5),
            ('45767561286-2', '28_Full suite _ Go ordinary and remaining race.txt', '5e75c7f912e3439349914cb553a7fe1e76457d02a5b3769f4416341664946059',
             'agentworkspace/cmd/workspace', 'TestWebDirStartupFailures', 3),
            ('45809258224-1', '27_Full suite _ Go ordinary and remaining race.txt', '5a3cfe8cd56d5818a9fe7b05cb68f6c4d1cfb8ab29d0183f4987b0d1d8d3cae2',
             'agentworkspace/cmd/workspace', 'TestRunProjectExperienceAcceptedAndRetired', 1)]
        for archive_name, member, digest, package, root, count in cases:
            with self.subTest(archive=archive_name), zipfile.ZipFile(Path(directory) / (archive_name + '.zip')) as archive:
                raw = archive.read(member)
                self.assertEqual(hashlib.sha256(raw).hexdigest(), digest)
                known, unknown = REPLAY.failure_records(raw.decode())
                self.assertEqual([(row['package'], row['test']) for row in known], [(package, root)])
                self.assertEqual(len(known[0]['signature']['tests']), count)
                self.assertTrue(known[0]['signature']['messages'])
                self.assertEqual([row['package'] for row in unknown], [package])
                if archive_name == '44427678293-1':
                    self.assertEqual(known[0]['signature'], {
                        'tests': [root, *(root + '/' + suffix for suffix in (
                            'local_user_chat/followup', 'local_user_chat/report', 'member/followup', 'member/report'))],
                        'messages': ['执行准备工作区无效或已变更']})


class ReviewedEvidenceTests(unittest.TestCase):
    def test_plain_named_go_signatures_do_not_borrow_peer_assertions(self):
        log = '--- FAIL: TestFirst (0.1s)\n    first_test.go:1: first assertion\n--- FAIL: TestSecond (0.1s)\n    second_test.go:2: second assertion\nFAIL\tagentworkspace/internal/store\t0.2s\n'
        found = REPLAY.failures(log)
        self.assertEqual([row["signature"]["messages"] for row in found], [["first assertion"], ["second assertion"]])

    def test_completed_named_failures_survive_other_package_timeouts(self):
        store = '--- FAIL: TestTaskWorkInputEffectKeys (0.20s)\n    task_test.go:8: effect key mismatch\nFAIL\tagentworkspace/internal/store\t0.20s\n'
        timeout = 'panic: test timed out after 20m0s\nFAIL\tagentworkspace/cmd/workspace\t1200.0s\n'
        for log in (store + timeout, timeout + store):
            parsed, unresolved = REPLAY.failure_records(log)
            self.assertEqual([(row["package"], row["test"]) for row in parsed], [("agentworkspace/internal/store", "TestTaskWorkInputEffectKeys")])
            self.assertEqual(unresolved[0]["package"], "agentworkspace/cmd/workspace")
        rows = [{"Package": "agentworkspace/internal/store", "Action": "run", "Test": "TestTaskWorkInputEffectKeys"},
                {"Package": "agentworkspace/internal/store", "Action": "fail", "Test": "TestTaskWorkInputEffectKeys"},
                {"Package": "agentworkspace/internal/store", "Action": "fail"},
                {"Package": "agentworkspace/cmd/workspace", "Action": "output", "Output": "panic: test timed out after 20m"},
                {"Package": "agentworkspace/cmd/workspace", "Action": "fail"}]
        self.assertEqual(REPLAY.failures('\n'.join(map(json.dumps, rows)))[0]["test"], "TestTaskWorkInputEffectKeys")

    def test_mixed_jobs_keep_completed_candidates_and_order_real_leaves_by_completion(self):
        api = FakeAPI("periodic")
        store = api.job("Full suite / Go store worker 1", 501, "failure")
        workspace = api.job("Full suite / Go workspace race", 502, "failure")
        store.update(completed_at="2026-11-26T00:10:00Z", steps=[{"number": 3, "name": "Run and verify logical slots", "conclusion": "failure", "started_at": "2026-11-26T00:09:00Z"}])
        workspace.update(completed_at="2026-11-26T00:20:00Z", steps=[{"number": 3, "name": "Race test workspace package", "conclusion": "failure", "started_at": "2026-11-26T00:01:00Z"}])
        api.jobs[1].update(conclusion="failure", completed_at="2026-11-26T00:00:00Z")
        successful = api.job("Full suite / Go benchmarks", 503)
        successful["completed_at"] = "2026-11-26T00:05:00Z"
        api.jobs.extend([workspace, successful, store])
        api.rows[f"repos/{REPO}/actions/runs/{RUN}/attempts/{ATTEMPT}/jobs?per_page=100&page=1"]["total_count"] = len(api.jobs)
        api.rows[f"repos/{REPO}/actions/jobs/501/logs"] = b'--- FAIL: TestTaskWorkInputEffectKeys (0.2s)\n    task_test.go:8: effect key mismatch\nFAIL\tagentworkspace/internal/store\t0.2s\n'
        api.rows[f"repos/{REPO}/actions/jobs/502/logs"] = b'panic: test timed out after 20m\nFAIL\tagentworkspace/cmd/workspace\t1200.0s\n'
        result = {"sha": SHA}
        POLICY.collect_failures(POLICY.Evidence(api, REPO, lambda _: True), api.run, result)
        self.assertEqual(result["first_blocking_event"]["id"], 501)
        self.assertEqual(result["failure_candidates"][0]["sources"][0]["id"], 501)
        self.assertEqual(result["unresolved_failures"][0]["sources"][0]["id"], 502)
        self.assertEqual(result["diagnostic_status"], "partial")
        self.assertEqual(result["failure_candidates"][0]["execution"]["timeout_seconds"], 600)
        api.jobs[-1]["run_attempt"] = 1
        with self.assertRaises(ValueError):
            POLICY.collect_failures(POLICY.Evidence(api, REPO, lambda _: True), api.run, {})

    def test_same_package_collection_retains_candidate_and_unknown_from_one_source(self):
        api = FakeAPI('periodic')
        job = api.job('Full suite / Go workspace race', 501, 'failure')
        job['steps'] = [{'number': 3, 'name': 'Race test workspace package', 'conclusion': 'failure'}]
        api.jobs.append(job)
        api.rows[f'repos/{REPO}/actions/runs/{RUN}/attempts/{ATTEMPT}/jobs?per_page=100&page=1']['total_count'] = len(api.jobs)
        api.rows[f'repos/{REPO}/actions/jobs/501/logs'] = (
            b'--- FAIL: TestCompleted (0.2s)\n    fixture_test.go:1: own assertion: context deadline exceeded\n'
            b'panic: test timed out after 20m0s\nFAIL\tagentworkspace/cmd/workspace\t1200.0s\n')
        result = {'sha': SHA}
        POLICY.collect_failures(POLICY.Evidence(api, REPO, lambda _: True), api.run, result)
        self.assertEqual(result['diagnostic_status'], 'partial')
        self.assertEqual([row['test'] for row in result['failure_candidates']], ['TestCompleted'])
        self.assertEqual([row['package'] for row in result['unresolved_failures']], ['agentworkspace/cmd/workspace'])
        for row in result['failure_candidates'] + result['unresolved_failures']:
            self.assertEqual([source['id'] for source in row['sources']], [501])

    def test_partial_attribution_routes_only_individually_bound_prs(self):
        source = {"id": 501, "run_id": RUN, "run_attempt": ATTEMPT, "log_sha256": "e" * 64}
        item = {"id": "1" * 16, "status": "confirmed", "pr": 17, "good_sha": BASE, "bad_sha": SHA, "commit": SHA,
                "candidate": {"test": "TestTaskWorkInputEffectKeys", "signature": {"messages": ["effect key mismatch"]}, "sources": [source]},
                "evidence": {"artifact": f"periodic-full-localization-{RUN}-{ATTEMPT}", "path": "localization.json", "sha256": "f" * 64}}
        timeout = {"status": "unknown", "pr": 999, "bad_sha": SHA, "commit": None, "sources": [{**source, "id": 502}], "reason": "workspace timeout"}
        result = POLICY.combine_attribution(identity(), [item, timeout])
        self.assertEqual((result["status"], result["coverage"]), ("unknown", "partial"))
        self.assertEqual(result["items"][0]["pr"], 17)
        self.assertNotIn("pr", result["items"][1])
        self.assertNotIn("pr", result)
        same = copy.deepcopy(item); same["id"] = "2" * 16
        result = POLICY.combine_attribution(identity(), [item, same])
        self.assertEqual((result["status"], result["pr"]), ("confirmed", 17))
        different = {**same, "pr": 18, "commit": "d" * 40}
        result = POLICY.combine_attribution(identity(), [item, different])
        self.assertEqual(result["status"], "unknown")
        self.assertEqual(len(result["groups"]), 2)
        forged = {**item, "good_sha": None}
        self.assertNotIn("pr", POLICY.combine_attribution(identity(), [forged])["items"][0])
        diagnostic = copy.deepcopy(item)
        diagnostic['candidate'] = {'kind': 'diagnostic', 'replay_kind': 'go-compile', 'package': 'agentworkspace/internal/store',
                                   'file': 'internal/store/runtime_task_work_resource.go', 'signature': {'message': 'undefined: missing'}, 'sources': [source]}
        self.assertEqual(POLICY.combine_attribution(identity(), [diagnostic])['pr'], 17)

    def test_final_artifact_is_receipt_only_not_raw_api_cache(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary); bundle = root / "initial"; bundle.mkdir()
            (bundle / "receipt.json").write_text(json.dumps(periodic_receipt()))
            with (bundle / "raw-large-log").open("wb") as stream:
                stream.truncate(40 * 1024 * 1024)
            args = ["periodic-full.py", "finish", "--repo", REPO, "--root", str(root), "--bundle", str(bundle), "--output", str(root / "final")]
            env = {"GITHUB_RUN_ID": str(RUN), "GITHUB_RUN_ATTEMPT": str(ATTEMPT)}
            with mock.patch.object(sys, "argv", args), mock.patch.dict(os.environ, env), redirect_stdout(io.StringIO()):
                POLICY.main()
            self.assertEqual([path.name for path in (root / "final").iterdir()], ["receipt.json"])
            self.assertLess((root / "final/receipt.json").stat().st_size, 1024 * 1024)

    def test_client_collection_keeps_two_real_web_signatures_and_an_unknown_peer(self):
        api = FakeAPI("periodic")
        web = api.job("Periodic Full preflight", 133886997987, "failure")
        web["steps"] = [{"number": 8, "name": "Web test, typecheck and build", "conclusion": "failure"}]
        api.jobs.append(web)
        api.rows[f"repos/{REPO}/actions/runs/{RUN}/attempts/{ATTEMPT}/jobs?per_page=100&page=1"]["total_count"] = len(api.jobs)
        log = ' FAIL src/App.test.tsx > IM shell > aborts hidden task list/detail reads, ignores late results and reuses the mounted form on return\nAssertionError: expected "vi.fn()" to be called 2 times, but got 3 times\n FAIL src/RequestWork.test.tsx > uses real scoped author identity for \'我\' / \'user\'\nAssertionError: expected agent to be user\n FAIL src/Other.test.tsx > hangs\nError: Hook timed out in 10000ms\n'
        api.rows[f"repos/{REPO}/actions/jobs/{web['id']}/logs"] = log.encode()
        result = {"sha": SHA}
        POLICY.collect_failures(POLICY.Evidence(api, REPO, lambda _: True), api.run, result)
        self.assertEqual(len(result["failure_candidates"]), 2)
        self.assertTrue(all(row["kind"] == "client" and row["client"] == "web" for row in result["failure_candidates"]))
        self.assertEqual(len(result["unresolved_failures"]), 1)
        self.assertEqual(result["diagnostic_status"], "partial")

    def test_client_receipt_items_replay_and_authenticate_each_signature_separately(self):
        api = FakeAPI('periodic'); job = api.job('Periodic Full preflight', 501, 'failure')
        job['steps'] = [{'name': 'Web test, typecheck and build', 'number': 8, 'conclusion': 'failure'}]
        api.jobs.append(job); api.rows[f'repos/{REPO}/actions/runs/{RUN}/attempts/{ATTEMPT}/jobs?per_page=100&page=1']['total_count'] = len(api.jobs)
        api.rows[f'repos/{REPO}/actions/jobs/501/logs'] = b' FAIL src/App.test.tsx > shell > first\nAssertionError: first mismatch\n FAIL src/RequestWork.test.tsx > second\nAssertionError: second mismatch\n'
        api.rows[f'repos/{REPO}/commits/{SHA}/pulls?per_page=100&page=1'] = [{'number': 17, 'merged_at': '2026-11-26T01:00:00Z', 'merge_commit_sha': SHA, 'base': {'ref': 'master', 'repo': {'full_name': REPO}}}]
        evidence = POLICY.Evidence(api, REPO, lambda _: True)
        needs = full_needs(); needs['preflight']['result'] = 'failure'; needs['full']['result'] = 'skipped'
        initial = POLICY.receipt(identity(), needs, 'run', {'sha': BASE, 'tree': 'd' * 40, 'verified': True, 'run_id': RUN - 1, 'run_attempt': 1})
        POLICY.collect_failures(evidence, api.run, initial)
        calls = []
        class ClientFixture:
            def __init__(self, root, output, **kwargs): output.mkdir(parents=True)
            def close(self): pass
            def __call__(self, sha, candidate):
                calls.append(candidate['test'])
                return {'state': 'pass' if sha == BASE else 'fail', **({'signature': candidate['signature']} if sha == SHA else {})}
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary); bundle = root / 'initial'; bundle.mkdir(); local = root / 'local'; local.mkdir(); final = root / 'final'; final.mkdir()
            (bundle / 'receipt.json').write_text(json.dumps(initial))
            def git(root, *args): return SHA + '\n' + BASE if args[0] == 'rev-list' else TREE if args[-1].startswith(SHA) else 'd' * 40
            worker = REPLAY.worker
            with mock.patch.object(REPLAY, 'git', side_effect=git), mock.patch.object(REPLAY, 'worker', side_effect=lambda name:
                                   SimpleNamespace(ClientProbe=ClientFixture) if name == 'periodic-clients' else worker(name)):
                REPLAY.replay(bundle, root, local)
            self.assertEqual(sorted(calls), ['second'] * 6 + ['shell first'] * 6)
            args = SimpleNamespace(root=root, bundle=bundle, localization=local, output=final)
            with mock.patch.dict(os.environ, {'GITHUB_RUN_ID': str(RUN), 'GITHUB_RUN_ATTEMPT': str(ATTEMPT)}), redirect_stdout(io.StringIO()), \
                 mock.patch.object(evidence, 'verify', return_value=initial['ancestor']) as prior_proof, \
                 mock.patch.object(POLICY, 'verify_localization', side_effect=lambda initial, item, directory, root, **kwargs: item) as checked:
                POLICY.finish(args, api, evidence)
                receipt = json.loads((final / 'receipt.json').read_text())
                self.assertEqual((receipt['attribution']['status'], receipt['attribution']['pr']), ('confirmed', 17))
                self.assertEqual(checked.call_count, 2)
                prior_proof.assert_called_with(RUN - 1, 1)
                self.assertEqual(len({item['id'] for item in receipt['attribution']['items']}), 2)
                prior_proof.return_value = {**initial['ancestor'], 'tree': TREE}
                POLICY.finish(args, api, evidence)
                self.assertFalse(any(item.get('pr') for item in json.loads((final / 'receipt.json').read_text())['attribution']['items']))
                prior_proof.return_value = initial['ancestor']
                envelope = json.loads((local / 'localization.json').read_text()); envelope['items'][1]['candidate']['signature'] = envelope['items'][0]['candidate']['signature']
                (local / 'localization.json').write_text(json.dumps(envelope))
                POLICY.finish(args, api, evidence)
                receipt = json.loads((final / 'receipt.json').read_text())
                self.assertEqual((receipt['attribution']['status'], receipt['attribution']['coverage']), ('unknown', 'partial'))
                self.assertEqual([item.get('pr') for item in receipt['attribution']['items']], [17, None])
        endpoint = f'repos/{REPO}/contents/.github/scripts/ci-plan.py?ref={SHA}'
        original = (SCRIPTS / 'ci-plan.py').read_bytes(); altered = original.replace(b'install = ["npm", "ci"] if', b'install = ["npm", "ci", "--ignore-scripts"] if')
        api.rows[endpoint] = {'sha': hashlib.sha1(b'blob ' + str(len(altered)).encode() + b'\0' + altered).hexdigest(), 'encoding': 'base64', 'content': base64.b64encode(altered).decode()}
        result = {'sha': SHA}; POLICY.collect_failures(evidence, api.run, result)
        self.assertEqual(result['failure_candidates'], [])
        self.assertEqual(result['diagnostic_status'], 'unknown')

    def test_mixed_deterministic_replay_authenticates_own_pr_and_retains_timeout(self):
        api = FakeAPI("periodic")
        store, timeout = api.job("Full suite / Go store worker 1", 501, "failure"), api.job("Full suite / Go workspace race", 502, "failure")
        for job, name in ((store, "Run and verify logical slots"), (timeout, "Race test workspace package")):
            job["steps"] = [{"name": name, "number": 3, "conclusion": "failure"}]
        api.jobs.extend([store, timeout])
        api.rows[f"repos/{REPO}/actions/runs/{RUN}/attempts/{ATTEMPT}/jobs?per_page=100&page=1"]["total_count"] = len(api.jobs)
        api.rows[f"repos/{REPO}/actions/jobs/501/logs"] = b'--- FAIL: TestTaskWorkInputEffectKeys (0.2s)\n    task_test.go:8: effect key mismatch\nFAIL\tagentworkspace/internal/store\t0.2s\n'
        api.rows[f"repos/{REPO}/actions/jobs/502/logs"] = b'panic: test timed out after 20m\nFAIL\tagentworkspace/cmd/workspace\t1200.0s\n'
        api.rows[f"repos/{REPO}/commits/{SHA}/pulls?per_page=100&page=1"] = [{"number": 17, "merged_at": "2026-11-26T01:00:00Z", "merge_commit_sha": SHA,
                                                                           "base": {"ref": "master", "repo": {"full_name": REPO}}}]
        evidence = POLICY.Evidence(api, REPO, lambda _: True)
        needs = full_needs(); needs["full"]["result"] = "failure"
        initial = POLICY.receipt(identity(), needs, "run", {"sha": BASE, "tree": "d" * 40, "verified": True, "run_id": RUN - 1, "run_attempt": 1})
        POLICY.collect_failures(evidence, api.run, initial)
        definition = {"state": "present", "files": {}, "declarations": ["internal/store/fixture_test.go"]}
        def git(root, *args):
            if args[0] == "rev-list": return SHA + "\n" + BASE
            return TREE if args[-1].startswith(SHA) else "d" * 40
        class FixtureProbe:
            def __init__(self, root, output, **kwargs):
                self.output, self.index = output, 0; output.mkdir(parents=True)
            def close(self): pass
            def __call__(self, sha, candidate):
                self.index += 1
                package, test = candidate["package"], candidate["test"]
                def row(action, name=None, **fields):
                    return {"Action": action, "Package": package, **({"Test": name} if name else {}), **fields}
                listing = [row("start"), row("output", Output=test + "\n"), row("pass")]
                action = "pass" if sha == BASE else "fail"
                run = [row("start"), row("run", test)]
                if action == "fail": run.append(row("output", test, Output="task_test.go:8: effect key mismatch\n"))
                run += [row(action, test), row(action)]
                result = {"sha": sha, "tree": TREE if sha == SHA else "d" * 40, "execution": candidate["execution"], "definition": definition}
                flags = ["-race"] if candidate["race"] else []
                for label, events, argv in (("list", listing, ["go", "test", "-json", *flags, "-list", "^" + test + "$"]),
                                             ("run", run, ["go", "test", "-count=1", *flags, "-json", "-timeout=10m", "-run", "^" + test + "$"])):
                    data = ''.join(json.dumps(value) + '\n' for value in events).encode()
                    name = f'{self.index:04d}-{label}.jsonl'; (self.output / name).write_bytes(data)
                    result[label] = {"argv": argv + ["./internal/store"], "log": name, "sha256": hashlib.sha256(data).hexdigest(), "returncode": int(action == "fail" and label == "run")}
                return {**result, "state": action, **({"signature": candidate["signature"]} if action == "fail" else {})}
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary); bundle = root / "initial"; bundle.mkdir(); output = root / "localization"; output.mkdir(); final = root / "final"; final.mkdir()
            (bundle / "receipt.json").write_text(json.dumps(initial))
            loader = POLICY.load
            with mock.patch.object(REPLAY, "git", side_effect=git), mock.patch.object(REPLAY, "definition", return_value=definition), \
                 mock.patch.object(REPLAY, "Probe", FixtureProbe), mock.patch.object(POLICY, "load", side_effect=lambda name, root=None: REPLAY if name == "periodic-replay" else loader(name, root)):
                REPLAY.replay(bundle, root, output)
                args = SimpleNamespace(bundle=bundle, localization=output, root=root, output=final)
                with mock.patch.dict(os.environ, {"GITHUB_RUN_ID": str(RUN), "GITHUB_RUN_ATTEMPT": str(ATTEMPT)}), redirect_stdout(io.StringIO()), \
                     mock.patch.object(evidence, 'verify', return_value=initial['ancestor']):
                    POLICY.finish(args, api, evidence)
                    receipt = json.loads((final / "receipt.json").read_text())
                    self.assertEqual((receipt["status"], receipt["attribution"]["status"], receipt["attribution"]["coverage"]), ("failure", "unknown", "partial"))
                    proven, unknown = receipt["attribution"]["items"]
                    self.assertEqual((proven["pr"], proven["good_sha"], proven["bad_sha"], proven["commit"], proven["sources"][0]["id"]), (17, BASE, SHA, SHA, 501))
                    self.assertEqual(unknown["sources"][0]["id"], 502); self.assertNotIn("pr", unknown)
                    self.assertNotIn("probes", proven)
                    envelope = json.loads((output / "localization.json").read_text())
                    envelope["items"][0]["candidate"]["sources"][0]["id"] = 502
                    (output / "localization.json").write_text(json.dumps(envelope))
                    POLICY.finish(args, api, evidence)
                    self.assertFalse(any(item.get("pr") for item in json.loads((final / "receipt.json").read_text())["attribution"]["items"]))

    def test_native_historical_compile_vet_and_shared_python_entries_are_independent(self):
        workflow = '''jobs:
  httpapi:
    name: Go HTTPAPI ${{ matrix.shard }}
    runs-on: ubuntu-latest
    timeout-minutes: 40
    steps:
      - name: Run and verify HTTPAPI shard
        run: |
          set -euo pipefail
          go test -count=1 -race -json -run "$pattern" ./internal/httpapi
  go:
    name: Go
    runs-on: ubuntu-latest
    timeout-minutes: 25
    steps:
      - run: go vet ./...
      - name: Test Store shard gates
        run: |
          set -euo pipefail
          python3 .github/scripts/test-ci-scheduling.py -v
          python3 .github/scripts/test-store-shard.py -v
      - name: Print fake vet
        run: echo 'go vet ./...'
'''.encode()
        api = FakeAPI()
        api.rows[f'repos/{REPO}/contents/.github/workflows/ci.yml?ref={SHA}'] = {
            'sha': hashlib.sha1(b'blob ' + str(len(workflow)).encode() + b'\0' + workflow).hexdigest(),
            'encoding': 'base64', 'content': base64.b64encode(workflow).decode()}
        jobs = [(501, 'Go HTTPAPI 1', 'Run and verify HTTPAPI shard'), (502, 'Go', 'Run go vet ./...'),
                (503, 'Go', 'Test Store shard gates'), (504, 'Go', 'Print fake vet')]
        for job_id, name, step in jobs:
            job = api.job(name, job_id, 'failure'); job['steps'] = [{'name': step, 'number': 3, 'conclusion': 'failure'}]; api.jobs.append(job)
        api.rows[f'repos/{REPO}/actions/runs/{RUN}/attempts/{ATTEMPT}/jobs?per_page=100&page=1']['total_count'] = len(api.jobs)
        api.rows[f'repos/{REPO}/actions/jobs/501/logs'] = ('\n'.join(json.dumps(row) for row in [
            {'Action': 'build-output', 'ImportPath': 'agentworkspace/internal/store', 'Output': '# agentworkspace/internal/store\ninternal/store/runtime_task_work_resource.go:100:51: undefined: taskWorkFollowupProvenance\n'},
            {'Action': 'build-fail', 'ImportPath': 'agentworkspace/internal/store'},
            {'Action': 'fail', 'Package': 'agentworkspace/internal/httpapi', 'FailedBuild': 'agentworkspace/internal/store'}]) + '\n').encode()
        api.rows[f'repos/{REPO}/actions/jobs/502/logs'] = b'# agentworkspace/internal/platform\ninternal/platform/prepare_task_work.go:100:66: literal copies lock value from base: agentworkspace/internal/platform.memberToolRuntimeRun contains sync/atomic.Bool contains sync/atomic.noCopy\n'
        api.rows[f'repos/{REPO}/actions/jobs/503/logs'] = b'''FAIL: test_remaining_package_exclusion_executes_actual_shell_without_go (__main__.WorkflowTests.test_remaining_package_exclusion_executes_actual_shell_without_go) (case='valid')
----------------------------------------------------------------------
Traceback (most recent call last):
  File "/home/runner/work/workspace/workspace/.github/scripts/test-store-shard.py", line 307, in test_remaining_package_exclusion_executes_actual_shell_without_go
    self.assertEqual(False, True)
AssertionError: False != True :

----------------------------------------------------------------------
Ran 1 test in 0.01s

FAILED (failures=1)
'''
        api.rows[f'repos/{REPO}/actions/jobs/504/logs'] = api.rows[f'repos/{REPO}/actions/jobs/502/logs']
        result = {'sha': SHA}
        POLICY.collect_failures(POLICY.Evidence(api, REPO, lambda _: True), api.run, result)
        self.assertEqual(len(result['failure_candidates']), 3)
        by_kind = {row['replay_kind']: row for row in result['failure_candidates']}
        compile = by_kind['go-compile']
        self.assertEqual((compile['package'], compile['sources'][0]['id']), ('agentworkspace/internal/store', 501))
        self.assertIn('./internal/httpapi', compile['execution']['argv'])
        self.assertEqual(by_kind['go-vet']['sources'][0]['id'], 502)
        self.assertEqual(by_kind['python-unittest']['execution']['argv'], ['python3', '.github/scripts/test-store-shard.py', '-v'])
        self.assertTrue(all(row['recipe']['sha256'] for row in result['failure_candidates']))
        self.assertEqual([row['sources'][0]['id'] for row in result['unresolved_failures']], [504])
        self.assertEqual(result['diagnostic_status'], 'partial')

    def test_current_vet_shared_python_and_shard_compile_reach_diagnostic_module(self):
        for name, step, log, kind in (
            ('Full suite / Go vet and CI gates', 'Run go vet ./...', '# agentworkspace/internal/platform\ninternal/platform/x.go:1: literal copies lock value from base: sync/atomic.Bool contains sync/atomic.noCopy\n', 'go-vet'),
            ('Full suite / Go store worker 1', 'Run and verify logical slots', '# agentworkspace/internal/store\ninternal/store/x.go:1: undefined: missing\nFAIL\tagentworkspace/internal/store\t[build failed]\n', 'go-compile'),
            ('Full suite / Go ordinary and remaining race', 'Race test all other packages', '\n'.join(json.dumps(row) for row in [
                {'Action': 'build-output', 'ImportPath': 'agentworkspace/internal/store', 'Output': '# agentworkspace/internal/store\ninternal/store/x.go:1: undefined: missing\n'},
                {'Action': 'build-fail', 'ImportPath': 'agentworkspace/internal/store'},
                {'Action': 'fail', 'Package': 'agentworkspace/internal/actor', 'FailedBuild': 'agentworkspace/internal/store'}]), 'go-compile'),
            ('Full suite / Go vet and CI gates', 'Test CI scheduling and shard gates', 'FAIL: test_policy (__main__.WorkflowTests.test_policy)\n----------------------------------------------------------------------\nTraceback (most recent call last):\n  File "/home/runner/work/workspace/workspace/.github/scripts/test-store-shard.py", line 4, in test_policy\n    self.assertEqual(1, 2)\nAssertionError: 1 != 2\n\n----------------------------------------------------------------------\nRan 1 test in 0.001s\n\nFAILED (failures=1)\n', 'python-unittest')):
            api = FakeAPI('periodic'); job = api.job(name, 501, 'failure'); job['steps'] = [{'name': step, 'number': 3, 'conclusion': 'failure'}]
            api.jobs.append(job); api.rows[f'repos/{REPO}/actions/runs/{RUN}/attempts/{ATTEMPT}/jobs?per_page=100&page=1']['total_count'] = len(api.jobs)
            api.rows[f'repos/{REPO}/actions/jobs/501/logs'] = log.encode()
            if step == 'Race test all other packages':
                source = (WORKFLOWS / 'ci-full.yml').read_bytes().replace(b'-timeout=14m', b'-timeout=12m30s')
                api.rows[f'repos/{REPO}/contents/.github/workflows/ci-full.yml?ref={SHA}'] = {'sha': hashlib.sha1(b'blob ' + str(len(source)).encode() + b'\0' + source).hexdigest(),
                    'encoding': 'base64', 'content': base64.b64encode(source).decode()}
            result = {'sha': SHA}; POLICY.collect_failures(POLICY.Evidence(api, REPO, lambda _: True), api.run, result)
            with self.subTest(kind=kind):
                self.assertEqual(result['failure_candidates'][0]['replay_kind'], kind)
                self.assertEqual(result['diagnostic_status'], 'available')
                if step == 'Race test all other packages':
                    self.assertEqual(result['failure_candidates'][0]['recipe']['test_timeout'], {'literal': '12m30s', 'seconds': 750})
                    self.assertIn('agentworkspace/internal/actor', result['failure_candidates'][0]['execution']['argv'])

    def test_real_diagnostic_replay_finalization_and_source_tamper(self):
        workflow = '''jobs:
  policy:
    name: CI policy
    runs-on: ubuntu-latest
    timeout-minutes: 25
    steps:
      - name: Test Store shard gates
        run: python3 .github/scripts/test-store-shard.py -v
'''
        with tempfile.TemporaryDirectory(prefix='periodic-policy-fixture-') as temporary:
            root = Path(temporary) / 'repository'; root.mkdir()
            subprocess.run(['git', 'init', '-q', str(root)], check=True)
            def git(*args, data=None):
                return subprocess.check_output(['git', '-C', str(root), *args], input=data).decode().strip()
            def blob(value): return git('hash-object', '-w', '--stdin', data=value.encode())
            def tree(rows): return git('mktree', data=''.join(f'{mode} {kind} {value}\t{name}\n' for mode, kind, value, name in rows).encode())
            workflow_blob = blob(workflow)
            workflows = tree([('100644', 'blob', workflow_blob, 'periodic-full.yml')])
            def commit(expected, parent=None):
                source = 'import unittest\nclass WorkflowTests(unittest.TestCase):\n    def test_regression(self):\n        self.assertEqual(1, ' + str(expected) + ')\nif __name__ == "__main__": unittest.main()\n'
                scripts = tree([('100644', 'blob', blob(source), 'test-store-shard.py')])
                github = tree([('040000', 'tree', scripts, 'scripts'), ('040000', 'tree', workflows, 'workflows')])
                value = tree([('040000', 'tree', github, '.github')])
                raw = 'tree ' + value + '\n' + ('parent ' + parent + '\n' if parent else '') + 'author Fixture <ci@example.invalid> 1 +0000\ncommitter Fixture <ci@example.invalid> 1 +0000\n\nfixture\n'
                return git('hash-object', '-w', '-t', 'commit', '--stdin', data=raw.encode())
            good = commit(1); bad = commit(2, good); git('update-ref', 'refs/heads/master', bad)
            api = FakeAPI('periodic', trigger_sha=bad)
            api.rows[f'repos/{REPO}/contents/.github/workflows/periodic-full.yml?ref={bad}'] = {'sha': workflow_blob, 'encoding': 'base64', 'content': base64.b64encode(workflow.encode()).decode()}
            failed = api.job('CI policy', 501, 'failure'); failed['steps'] = [{'name': 'Test Store shard gates', 'number': 3, 'conclusion': 'failure'}]
            api.jobs.append(failed); api.rows[f'repos/{REPO}/actions/runs/{RUN}/attempts/{ATTEMPT}/jobs?per_page=100&page=1']['total_count'] = len(api.jobs)
            api.rows[f'repos/{REPO}/actions/jobs/501/logs'] = b'FAIL: test_regression (__main__.WorkflowTests.test_regression)\n----------------------------------------------------------------------\nTraceback (most recent call last):\n  File "/home/runner/work/workspace/workspace/.github/scripts/test-store-shard.py", line 4, in test_regression\n    self.assertEqual(1, 2)\nAssertionError: 1 != 2\n\n----------------------------------------------------------------------\nRan 1 test in 0.001s\n\nFAILED (failures=1)\n'
            api.rows[f'repos/{REPO}/commits/{bad}/pulls?per_page=100&page=1'] = [{'number': 17, 'merged_at': '2026-11-26T01:00:00Z', 'merge_commit_sha': bad, 'base': {'ref': 'master', 'repo': {'full_name': REPO}}}]
            actual = {**identity(), 'sha': bad, 'head': bad, 'tree': git('rev-parse', bad + '^{tree}'), 'base': good}
            needs = full_needs(); needs['full']['result'] = 'failure'
            initial = POLICY.receipt(actual, needs, 'run', {'sha': good, 'tree': git('rev-parse', good + '^{tree}'), 'verified': True, 'run_id': RUN - 1, 'run_attempt': 1})
            evidence = POLICY.Evidence(api, REPO, lambda value: value in (good, bad))
            POLICY.collect_failures(evidence, api.run, initial)
            self.assertEqual(initial['failure_candidates'][0]['kind'], 'diagnostic')
            bundle, replayed, final = [Path(temporary) / name for name in ('initial', 'replayed', 'final')]
            for directory in (bundle, replayed, final): directory.mkdir()
            (bundle / 'receipt.json').write_text(json.dumps(initial))
            REPLAY.replay(bundle, root, replayed)
            args = SimpleNamespace(bundle=bundle, localization=replayed, root=root, output=final)
            with mock.patch.dict(os.environ, {'GITHUB_RUN_ID': str(RUN), 'GITHUB_RUN_ATTEMPT': str(ATTEMPT)}), redirect_stdout(io.StringIO()), \
                 mock.patch.object(evidence, 'verify', return_value=initial['ancestor']):
                POLICY.finish(args, api, evidence)
                result = json.loads((final / 'receipt.json').read_text())
                self.assertEqual(result['attribution'].get('pr'), 17, result)
                self.assertEqual(result['attribution']['items'][0]['sources'][0]['id'], 501)
                first = next(replayed.rglob('*-diag-run.jsonl')); first.write_text(first.read_text() + 'tamper\n')
                POLICY.finish(args, api, evidence)
                self.assertNotIn('pr', json.loads((final / 'receipt.json').read_text())['attribution'])

    def test_real_archived_web_names_are_independent_candidates(self):
        archive_dir = os.environ.get('PERIODIC_ARCHIVE_DIR')
        if not archive_dir: self.skipTest('未提供外部只读日志夹具；内嵌签名测试始终执行')
        archive = Path(archive_dir) / '44038474616-1.zip'
        with zipfile.ZipFile(archive) as source:
            log = source.read('21_Web.txt').decode()
        candidates = POLICY.load('periodic-clients').failures('web', log)
        self.assertEqual(len(candidates), 2)
        self.assertIn('aborts hidden task list/detail reads', candidates[0]['test'])
        self.assertEqual(candidates[0]['signature']['message'], 'expected "vi.fn()" to be called 2 times, but got 3 times')
        self.assertIn("uses real scoped author identity", candidates[1]['test'])
        for candidate in candidates:
            result = REPLAY.localize([BASE, SHA], candidate, lambda value, row: {"state": "pass" if value == BASE else "fail", **({"signature": row['signature']} if value == SHA else {})})
            self.assertEqual(result['localization'], 'reproduced')

    def test_original_timeout_proof_and_runtime_argv_do_not_shortcut_slow_pair(self):
        api = FakeAPI("periodic")
        job = api.job("Full suite / Go workspace race", 501, "failure")
        step = {"name": "Race test workspace package"}
        mode = POLICY.go_execution(POLICY.Evidence(api, REPO, lambda _: True), api.run, SHA, job, step)
        self.assertEqual((mode["timeout_seconds"], mode["timeout_arg"], mode["mode"]), (1200, "20m", "race"))
        self.assertTrue(mode["proof"])
        candidate = {"package": "agentworkspace/cmd/workspace", "test": "TestSlow", "race": True,
                     "signature": {"tests": ["TestSlow"], "messages": []}, "execution": mode}
        def fake_git(root, *args):
            if args[:2] == ("worktree", "add"):
                checkout = Path(args[-2]); checkout.mkdir(); (checkout / "go.mod").write_text("module agentworkspace\n")
            return TREE if args[-1] == "HEAD^{tree}" else SHA
        def execute(argv, cwd, label, **kwargs):
            calls.append((argv, kwargs["timeout"]))
            rows = [{"Action": "start", "Package": candidate["package"]}]
            rows += ([{"Action": "output", "Package": candidate["package"], "Output": "TestSlow\n"}] if label == "list" else
                     [{"Action": "run", "Package": candidate["package"], "Test": "TestSlow"}, {"Action": "pass", "Package": candidate["package"], "Test": "TestSlow"}])
            rows.append({"Action": "pass", "Package": candidate["package"]})
            return {"returncode": 0}, '\n'.join(map(json.dumps, rows))
        calls = []
        with tempfile.TemporaryDirectory() as temporary, mock.patch.object(REPLAY, "git", side_effect=fake_git), mock.patch.object(REPLAY, "definition", return_value={}):
            runner = REPLAY.Probe(Path(temporary), Path(temporary) / "output")
            try:
                with mock.patch.object(runner, "execute", side_effect=execute):
                    self.assertEqual(runner(SHA, candidate)["state"], "pass")
            finally:
                runner.close()
        self.assertIn("-timeout=20m", calls[1][0])
        self.assertEqual(calls[0][1], 300)
        self.assertEqual(calls[1][1], 1500)
        endpoint = f"repos/{REPO}/contents/.github/workflows/ci-full.yml?ref={api.run['head_sha']}"
        altered = base64.b64decode(api.rows[endpoint]["content"]).replace(b'-timeout=20m ./cmd/workspace', b'-timeout=20m ./cmd/workspace -timeout=2m')
        api.rows[endpoint] = {"sha": hashlib.sha1(b'blob ' + str(len(altered)).encode() + b'\0' + altered).hexdigest(),
                              "encoding": "base64", "content": base64.b64encode(altered).decode()}
        with self.assertRaises(ValueError):
            POLICY.go_execution(POLICY.Evidence(api, REPO, lambda _: True), api.run, SHA, job, step)

    def test_trusted_unlisted_go_duration_literals_keep_exact_budget(self):
        for duration, seconds in (('15m', 900), ('12m30s', 750), ('1.5m', 90)):
            api = FakeAPI('periodic')
            endpoint = f'repos/{REPO}/contents/.github/workflows/ci-full.yml?ref={SHA}'
            for value in (duration, 'false', '${{ inputs.timeout }}', '15m; echo unsafe', '99h'):
                raw = (WORKFLOWS / 'ci-full.yml').read_bytes().replace(b'-timeout=20m', ('-timeout=' + value).encode())
                api.rows[endpoint] = {'sha': hashlib.sha1(b'blob ' + str(len(raw)).encode() + b'\0' + raw).hexdigest(),
                                      'encoding': 'base64', 'content': base64.b64encode(raw).decode()}
                job = api.job('Full suite / Go workspace race', 501, 'failure')
                step = {'name': 'Race test workspace package'}
                if value != duration:
                    with self.subTest(value=value), self.assertRaises(ValueError): POLICY.go_execution(POLICY.Evidence(api, REPO, lambda _: True), api.run, SHA, job, step)
                else:
                    mode = POLICY.go_execution(POLICY.Evidence(api, REPO, lambda _: True), api.run, SHA, job, step)
                    self.assertEqual((mode['timeout_arg'], mode['timeout_seconds']), (duration, seconds))
        for value in (False, True, 900, '', '0', 'NaN', '-1s', '1e3s', '15m$(x)'):
            with self.subTest(value=value), self.assertRaises(ValueError): REPLAY.duration(value)
        api = FakeAPI('periodic')
        rest = POLICY.go_execution(POLICY.Evidence(api, REPO, lambda _: True), api.run, SHA,
                                   api.job('Full suite / Go ordinary and remaining race', 501, 'failure'), {'name': 'Race test all other packages'})
        self.assertEqual((rest['mode'], rest['timeout_seconds'], rest['timeout_arg']), ('race', 840, '14m'))

    def test_release_minimal_protocol_without_log_or_head_base(self):
        api = FakeAPI("release", event="workflow_dispatch", trigger_sha=BASE)
        receipt = {key: api.receipt[key] for key in ("schema", "kind", "repo", "sha", "tree", "run_id", "run_attempt", "artifact_sha256", "status", "verified")}
        api.set_artifact(receipt, "release-full")
        api.rows.pop(f"repos/{REPO}/actions/jobs/100/logs")
        self.assertEqual(POLICY.Evidence(api, REPO, lambda _: True).verify(RUN, ATTEMPT)["sha"], SHA)

    def test_exact_target_classes_are_not_starved_by_legacy_ancestors(self):
        api = FakeAPI("periodic", trigger_sha=BASE)
        for path in ("ci.yml", "periodic-full.yml", "release-candidate.yml"):
            runs = ([{**api.run, "id": 8000 + i, "head_sha": BASE, "run_attempt": 1} for i in range(50)] if path == "ci.yml" else
                    [api.run] if path == "periodic-full.yml" else [])
            api.rows[f"repos/{REPO}/actions/workflows/{path}/runs?per_page=100&page=1"] = {"total_count": len(runs), "workflow_runs": runs}
        evidence = POLICY.Evidence(api, REPO, lambda _: True)
        def verify(run_id, attempt):
            if run_id == RUN:
                return {**identity(), "verified": True}
            return {**identity(), "sha": BASE, "verified": True}
        with mock.patch.object(evidence, "verify", side_effect=verify):
            found, reasons = evidence.search([SHA, BASE])
        self.assertIn(SHA, found)


class HistoryIntegrationTests(unittest.TestCase):
    def test_sparse_nonmonotonic_and_new_test_raw_proofs_rebuild_through_normal_verifier(self):
        fixtures = load('test-periodic-history')
        for states in (['pass'] * 20 + ['fail'] * 40 + ['pass'] + ['fail'] * 4,
                       ['absent'] * 60 + ['fail'] * 5):
            engine = fixtures.Engine(states)
            report = REPLAY.localize(engine.chain, engine.candidate, engine)
            self.assertEqual(report.get('localization'), 'reproduced', report)
            expected = 61 if states[0] == 'pass' else 60
            self.assertEqual(report['commit'], engine.chain[expected])
            trees = {value: f'{index + 1000000:040x}' for index, value in enumerate(engine.chain)}
            definitions = {sha: source for sha, _, source in engine.bindings.values()}
            initial = {'sha': engine.chain[-1], 'tree': trees[engine.chain[-1]],
                       'ancestor': {'sha': engine.chain[0], 'tree': trees[engine.chain[0]]},
                       'failure_candidates': [engine.candidate]}
            with tempfile.TemporaryDirectory() as temporary:
                directory = Path(temporary)
                for group in report['probes']:
                    for result in group['results']:
                        for label in ('list', 'run'):
                            if label not in result: continue
                            proof = result[label]; data = engine.logs[proof['log']]
                            name = str(len(list(directory.iterdir())) + 10000) + '-' + label + '.jsonl'
                            (directory / name).write_bytes(data)
                            proof['log'] = name
                            command = ['go', 'test', '-json', '-list', '^TestHistory$'] if label == 'list' else [
                                'go', 'test', '-count=1', '-json', '-timeout=10m', '-run', '^TestHistory$']
                            proof['argv'] = command + ['./internal/store']
                def git(root, *args):
                    if args[0] == 'rev-list': return '\n'.join(reversed(engine.chain))
                    return trees[args[-1].split('^')[0]]
                loader = POLICY.load
                with mock.patch.object(REPLAY, 'git', side_effect=git), \
                     mock.patch.object(REPLAY, 'definition', side_effect=lambda root, sha, candidate: definitions[sha]), \
                     mock.patch.object(POLICY, 'load', side_effect=lambda name, root=None: REPLAY if name == 'periodic-replay' else loader(name, root)):
                    POLICY.verify_localization(initial, report, directory, directory)
                    with self.assertRaises(ValueError):
                        POLICY.verify_localization(initial, report, directory, directory, remaining=lambda: 0)
                    for field, value in (('commit', engine.chain[20]), ('scope', 'first-ever-failure'),
                                         ('continuation', {'intervals': [], 'next': None})):
                        if report.get(field) == value: continue
                        forged = copy.deepcopy(report); forged[field] = value
                        with self.subTest(field=field), self.assertRaises(ValueError):
                            POLICY.verify_localization(initial, forged, directory, directory)
                    forged = copy.deepcopy(report)
                    forged['probes'] = [group for group in forged['probes'] if group['sha'] != engine.chain[-2]]
                    with self.assertRaises(ValueError): POLICY.verify_localization(initial, forged, directory, directory)
                    forged = copy.deepcopy(report)
                    forged['probes'][0]['results'][0]['tree'] = 'f' * 40
                    with self.assertRaises(ValueError): POLICY.verify_localization(initial, forged, directory, directory)
                    first = directory / report['probes'][0]['results'][0]['list']['log']
                    first.write_bytes(first.read_bytes() + b'corrupt\n')
                    with self.assertRaises(ValueError): POLICY.verify_localization(initial, report, directory, directory)

    def test_replay_has_no_16_candidate_or_33_commit_cap_and_preserves_budget_continuation(self):
        values = [f'{index + 1:040x}' for index in range(65)]
        candidates = []
        for index in range(20):
            candidate = ReplayTests().candidate()
            candidate['test'] = f'TestCandidate{index}'
            candidate['signature']['tests'] = [candidate['test']]
            candidates.append(candidate)
        initial = {'sha': values[-1], 'tree': TREE, 'ancestor': {'sha': values[0], 'tree': TREE}, 'failure_candidates': candidates}
        for exhaust in (False, True):
            now, calls = [0], []
            class Fixture:
                def __init__(self, *args, **kwargs): pass
                def close(self): pass
                def __call__(self, value, candidate):
                    calls.append((value, candidate['test']))
                    if exhaust: now[0] = 12601
                    return {'sha': value, 'tree': TREE, 'state': 'fail' if value == values[-1] else 'pass', 'signature': candidate['signature']}
            with tempfile.TemporaryDirectory() as temporary:
                directory = Path(temporary); bundle = directory / 'bundle'; bundle.mkdir(); output = directory / 'output'; output.mkdir()
                (bundle / 'receipt.json').write_text(json.dumps(initial))
                def git(root, *args): return '\n'.join(reversed(values)) if args[0] == 'rev-list' else TREE
                with mock.patch.object(REPLAY, 'Probe', Fixture), mock.patch.object(REPLAY, 'git', side_effect=git), \
                     mock.patch.object(REPLAY.time, 'monotonic', side_effect=lambda: now[0]):
                    REPLAY.replay(bundle, directory, output)
                results = json.loads((output / 'localization.json').read_text())['items']
                self.assertEqual(len(results), 20)
                self.assertTrue(all(item['chain'] == values for item in results))
                if exhaust:
                    self.assertEqual(len(calls), 1)
                    self.assertTrue(all(item.get('localization') != 'reproduced' and item['continuation']['next'] for item in results))
                    self.assertTrue(all(item['commit'] is None for item in results))
                else:
                    self.assertTrue(all(item.get('localization') == 'reproduced' for item in results))
                    self.assertEqual(len({test for _, test in calls}), 20)

    def test_repeated_native_shards_coalesce_only_equivalent_compilation_inputs(self):
        api = FakeAPI()
        bodies = []
        configs = [('store', './internal/store', True), ('httpapi', './internal/httpapi', True),
                   ('ordinary', './internal/httpapi', False)]
        for key, target, race in configs:
            flags = '-race ' if race else ''
            bodies.append(f'''  {key}:
    name: Historical {key} ${{{{ matrix.shard }}}}
    runs-on: ubuntu-latest
    timeout-minutes: 40
    steps:
      - name: Compile and run shard
        run: |
          set -euo pipefail
          go test {flags}-list Test {target}
          go test -count=1 {flags}-json -run "$pattern" {target}
''')
        workflow = ('jobs:\n' + ''.join(bodies)).encode()
        api.rows[f'repos/{REPO}/contents/.github/workflows/ci.yml?ref={SHA}'] = {
            'sha': hashlib.sha1(b'blob ' + str(len(workflow)).encode() + b'\0' + workflow).hexdigest(),
            'encoding': 'base64', 'content': base64.b64encode(workflow).decode()}
        for index in range(80):
            key, target, _ = configs[index % 3]
            job_id = 500 + index; job = api.job(f'Historical {key} {index}', job_id, 'failure')
            job['steps'] = [{'name': 'Compile and run shard', 'number': 3, 'conclusion': 'failure'}]
            api.jobs.append(job)
            events = [{'Action': 'build-output', 'ImportPath': 'agentworkspace/internal/store',
                       'Output': '# agentworkspace/internal/store\ninternal/store/x.go:1: undefined: missing\n'},
                      {'Action': 'build-fail', 'ImportPath': 'agentworkspace/internal/store'},
                      {'Action': 'fail', 'Package': 'agentworkspace/' + target[2:], 'FailedBuild': 'agentworkspace/internal/store'}]
            api.rows[f'repos/{REPO}/actions/jobs/{job_id}/logs'] = ('\n'.join(map(json.dumps, events)) + '\n').encode()
        api.rows[f'repos/{REPO}/actions/runs/{RUN}/attempts/{ATTEMPT}/jobs?per_page=100&page=1']['total_count'] = len(api.jobs)
        result = {'sha': SHA}
        POLICY.collect_failures(POLICY.Evidence(api, REPO, lambda _: True), api.run, result)
        self.assertEqual(result['diagnostic_status'], 'available', result['unresolved_failures'])
        self.assertEqual(len(result['failure_candidates']), 3)
        self.assertEqual(sum(len(row['sources']) for row in result['failure_candidates']), 80)
        self.assertEqual({source['id'] for row in result['failure_candidates'] for source in row['sources']}, set(range(500, 580)))
        for candidate in result['failure_candidates']:
            self.assertEqual(candidate['package'], 'agentworkspace/internal/store')
            self.assertEqual(candidate['recipe']['command_status'], 'source-alternatives')
            for source in candidate['sources']:
                self.assertEqual(len(source['diagnostic']['recipes']), 2)
                self.assertTrue(all(len(recipe['alternatives']) == 2 for recipe in source['diagnostic']['recipes']))


class WorkflowTests(unittest.TestCase):
    def test_preflight_npm_cache_is_resolved_before_node_and_clean_children(self):
        preflight = POLICY.block((WORKFLOWS / 'periodic-full.yml').read_text(), 'preflight')
        initializer = re.search(r'^          (.*NPM_CONFIG_CACHE=.*)$', preflight, re.M)
        self.assertIsNotNone(initializer, 'npm 缓存必须在运行时初始化')
        self.assertNotRegex(preflight, r'(?m)^      NPM_CONFIG_CACHE:')
        self.assertLess(initializer.start(), preflight.index('uses: actions/setup-node@'))
        self.assertEqual(preflight.count('npm_config_cache="$NPM_CONFIG_CACHE"'), 2)
        with tempfile.TemporaryDirectory(prefix='periodic cache ') as temporary:
            env_file = Path(temporary) / 'env'
            subprocess.run(['bash', '-euo', 'pipefail', '-c', initializer[1]], check=True,
                           env={'RUNNER_TEMP': temporary, 'GITHUB_ENV': str(env_file)})
            self.assertEqual(env_file.read_text().splitlines(),
                             ['NPM_CONFIG_CACHE=' + str(Path(temporary) / 'periodic-npm-cache')])

    def test_only_enabled_master_requests_share_production_slot(self):
        text = (WORKFLOWS / 'periodic-full.yml').read_text()
        scheduling = load('test-ci-scheduling')
        group = scheduling.expression(text, 'group')
        admission = scheduling.expression(scheduling.job(text, 'resolve'), 'if')
        for event, ref, enabled, admitted in (('schedule', 'refs/heads/master', '', True),
                                             ('workflow_dispatch', 'refs/heads/master', 'false', True),
                                             ('schedule', 'refs/heads/master', 'false', False),
                                             ('workflow_dispatch', 'refs/heads/feature', '', False),
                                             ('workflow_dispatch', 'refs/tags/master', '', False),
                                             ('pull_request', 'refs/heads/master', '', False), ('push', 'refs/heads/master', '', False)):
            values = {'github.event_name': event, 'github.ref': ref, 'vars.PERIODIC_FULL_ENABLED': enabled,
                      'github.run_id': RUN, 'github.run_attempt': ATTEMPT}
            with self.subTest(event=event, ref=ref, enabled=enabled):
                self.assertEqual(bool(scheduling.scalar_expression(admission, values)), admitted)
                actual = scheduling.scalar_expression(group, values)
                self.assertEqual(actual == 'main-master-periodic', admitted)
                if not admitted: self.assertEqual(actual, f'periodic-ignored-{RUN}-{ATTEMPT}')
        self.assertNotIn('PERIODIC_FORCE', text)
        self.assertNotIn('force:', text)
        for name in ('preflight', 'replay'):
            self.assertEqual(scheduling.job(text, name).count('uses: actions/checkout@'), 1)

    def test_periodic_workflow_exists_and_pins_control_target_and_readonly_permissions(self):
        path = WORKFLOWS / "periodic-full.yml"
        self.assertTrue(path.exists(), "periodic-full.yml 尚未实现")
        text = path.read_text()
        for fragment in ["cron: '17 */6 * * *'", "'main-master-periodic'", "cancel-in-progress: false",
                         "contents: read", "actions: read", "pull-requests: read", "persist-credentials: false",
                         "uses: ./.github/workflows/ci-full.yml", "go test -count=1 ./cmd/workspace",
                         'ci-plan.py" web', 'ci-plan.py" bridge', "Periodic Full evidence",
                         "periodic-full-${{ github.run_id }}-${{ github.run_attempt }}", "receipt.json"]:
            self.assertIn(fragment, text)
        self.assertNotIn("gh run", text)
        self.assertNotIn("write-all", text)
        self.assertNotIn("secrets: inherit", text)
        self.assertNotIn("overwrite: true", text)
        self.assertNotIn("environment:", text)
        self.assertEqual(text.count("git/ref/heads/master"), 1)
        replay = re.search(r"^  replay:\n(.*?)(?=^  [\w-]+:|\Z)", text, re.M | re.S)[1]
        self.assertNotRegex(replay, r"GH_TOKEN|GITHUB_TOKEN|github.token|secrets\.")
        self.assertIn("timeout-minutes: 240", replay)
        for tool in ("WEB_NODE", "WEB_NPM", "BRIDGE_NODE", "BRIDGE_NPM"):
            self.assertIn("PERIODIC_" + tool, replay)
        final = re.search(r"^  finalize:\n(.*)", text, re.M | re.S)[1]
        self.assertNotIn("cp -R", final)
        self.assertIn("periodic-full-verification-${{ github.run_id }}-${{ github.run_attempt }}", final)
        for workflow in (text, (WORKFLOWS / "ci.yml").read_text()):
            self.assertNotIn("test-periodic-", workflow)
        scheduling = load('test-ci-scheduling')
        modules = []
        loader = SimpleNamespace(loadTestsFromModule=lambda module: modules.append(module.__name__) or unittest.TestSuite())
        scheduling.load_tests(loader, unittest.TestSuite(), None)
        self.assertCountEqual(modules, ['test_periodic_full', 'test_periodic_history', 'test_periodic_clients', 'test_periodic_diagnostics'])
        self.assertEqual(len(set(modules)), len(modules))
        preflight = re.search(r"^  preflight:\n(.*?)(?=^  [\w-]+:|\Z)", text, re.M | re.S)[1]
        self.assertNotIn("GH_TOKEN", preflight)
        self.assertIn("CHECKOUT_TREE", preflight)


if __name__ == "__main__":
    unittest.main()
