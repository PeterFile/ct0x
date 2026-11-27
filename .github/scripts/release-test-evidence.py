#!/usr/bin/env python3
"""Evidence tests never contact GitHub, AGY, or a model."""
import copy
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import tempfile
import unittest
from types import SimpleNamespace
from unittest.mock import patch

HERE = Path(__file__).resolve().parent
SHA, TREE, BINARY = "a" * 40, "b" * 40, b"one-built-binary"
REPO, RUN, ATTEMPT = "owner/project", 10, 2


def load(name):
    path = HERE / (name + ".py")
    assert path.is_file(), "缺少证据行为：" + name
    spec = importlib.util.spec_from_file_location(name.replace("-", "_"), path)
    result = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(result)
    return result


def machine_receipt():
    proof = {"turn_id": "turn-1", "session_id": "session-1", "protocol_sha256": "d" * 64, "transcript_sha256": "e" * 64}
    lanes = [{"name": name, "status": "success", "audit_sha256": "c" * 64, "assertions_sha256": "f" * 64,
              "assertion_count": 3, "proofs": [proof | {"turn_id": f"{name}-turn-{index}", "session_id": f"{name}-session-{index}"}
                                              for index in range(2 if name != "6b" else 6)],
              "attempt_ids": [f"attempt-{i}" for i in range(6)] if name == "6b" else []} for name in ("6a", "6b", "6c")]
    evidence = load("release-evidence")
    scope = {"schema": 1, "repo": REPO, "sha": SHA, "tree": TREE, "run_id": RUN,
             "run_attempt": ATTEMPT, "artifact_sha256": hashlib.sha256(BINARY).hexdigest()}
    for lane in lanes:
        lane.update(scope)
        lane["full_binding_sha256"] = None
        lane["checks"] = {key: True for key in evidence.LANE_CHECKS[lane["name"]]}
        lane["assertion_count"] = len(lane["checks"])
        lane["assertions_sha256"] = evidence.core.hashed(evidence.core.encoded(lane["checks"]))
    sources = evidence.trusted_sources()
    return {"schema": 1, "kind": "release-machine", "repo": REPO, "sha": SHA, "tree": TREE,
            "run_id": RUN, "run_attempt": ATTEMPT, "status": "success", "verified": True,
            "workflow_sha": SHA, "artifact_sha256": hashlib.sha256(BINARY).hexdigest(),
            "target_artifact": {"id": 30, "digest": "sha256:" + "1" * 64, "receipt_sha256": "2" * 64},
            "machine_id": evidence.core.hashed(os.uname().nodename.encode()), "source_bundle_sha256": evidence.core.hashed(evidence.core.encoded(evidence.trusted_bundle())), "lanes": lanes,
            "unknown_guard": {"mode": "observed", "proof_sha256": "5" * 64}, "full_binding": None,
            "sources": sources}


class ReceiptTests(unittest.TestCase):
    def setUp(self):
        self.evidence = load("release-evidence")
        self.assertTrue(hasattr(self.evidence, "LANE_CHECKS"), "缺少逐 lane 独立身份和判据验证")
        self.receipt = machine_receipt()

    def test_lane_identity_and_proof_maps_are_independently_validated(self):
        for key, changed in [("sha", "c" * 40), ("tree", "c" * 40), ("run_id", RUN + 1),
                             ("run_attempt", ATTEMPT + 1), ("artifact_sha256", "c" * 64)]:
            value = copy.deepcopy(self.receipt)
            value["lanes"][1][key] = changed
            with self.subTest(key=key), self.assertRaises(ValueError):
                self.evidence.validate_machine(value)
        for change in (lambda r: r["lanes"][0]["checks"].pop(next(iter(r["lanes"][0]["checks"]))),
                       lambda r: r["lanes"][0]["checks"].update(loopback_closed=False),
                       lambda r: r["lanes"][0].update(assertions_sha256="0" * 64),
                       lambda r: r.update(source_bundle_sha256="0" * 64),
                       lambda r: r["sources"].pop("seed_6c")):
            value = copy.deepcopy(self.receipt)
            change(value)
            with self.assertRaises(ValueError):
                self.evidence.validate_machine(value)

    def test_all_three_native_lanes_have_ids_digests_and_assertions(self):
        self.evidence.validate_machine(self.receipt)
        for mutate in [lambda r: r["lanes"].pop(), lambda r: r["lanes"][0].update(status="cancelled"),
                       lambda r: r["lanes"][2]["proofs"].pop(), lambda r: r.update(verified=False),
                       lambda r: r.update(run_attempt=True), lambda r: r.update(schema=True),
                       lambda r: r["lanes"][0].update(schema=True), lambda r: r["lanes"][0].update(assertion_count=0),
                       lambda r: r["lanes"][0]["proofs"][0].update(transcript_sha256=""),
                       lambda r: r["lanes"][0]["proofs"][0].update(session_id="../private"),
                       lambda r: r.update(unknown_guard={"mode": "skipped", "proof_sha256": "5" * 64})]:
            value = copy.deepcopy(self.receipt)
            mutate(value)
            with self.subTest(value=value), self.assertRaises(ValueError):
                self.evidence.validate_machine(value)

    def test_public_receipt_rejects_raw_prompts_paths_urls_and_extra_fields(self):
        for place, key in [((), "prompt"), (("lanes", 0), "private_path"), (("lanes", 0, "proofs", 0), "transcript"), (("sources",), "arbitrary")]:
            value = copy.deepcopy(self.receipt)
            cursor = value
            for part in place:
                cursor = cursor[part]
            cursor[key] = "/private/person/transcript raw prompt https://example.invalid"
            with self.subTest(place=place), self.assertRaises(ValueError):
                self.evidence.validate_machine(value)

    def test_six_native_workers_require_distinct_durable_attempt_bindings(self):
        for attempts in (["one-attempt"], [f"attempt-{i}" for i in range(5)], ["duplicate"] * 6):
            value = copy.deepcopy(self.receipt)
            value["lanes"][1]["attempt_ids"] = attempts
            with self.subTest(attempts=attempts), self.assertRaises(ValueError):
                self.evidence.validate_machine(value)

    def test_self_consistent_but_untrusted_driver_or_control_manifest_is_rejected(self):
        for name in ("driver_6a", "seed_6c", "release_machine"):
            value = copy.deepcopy(self.receipt)
            value["sources"][name] = "7" * 64
            value["source_bundle_sha256"] = self.evidence.core.hashed(self.evidence.core.encoded(value["sources"]))
            with self.subTest(name=name), self.assertRaises(ValueError):
                self.evidence.validate_machine(value)

    def test_unjoined_shape_correct_receipt_is_only_input_not_accepted_intake(self):
        self.evidence.validate_machine(self.receipt)
        with self.assertRaisesRegex(ValueError, "Full"):
            self.evidence.validate_machine(self.receipt, require_full=True)

    def test_target_receipt_hashes_real_binary_and_metadata(self):
        receipt = {"schema": 1, "kind": "release-target", "repo": REPO, "sha": SHA, "tree": TREE,
                   "run_id": RUN, "run_attempt": ATTEMPT, "workflow_sha": SHA,
                   "artifact_sha256": hashlib.sha256(BINARY).hexdigest(), "goos": "darwin", "goarch": "amd64",
                   "cgo_enabled": False, "vcs_modified": False, "trimpath": True}
        intent = receipt | {"kind": "release-intent"}
        self.evidence.verify_target(receipt, BINARY, intent)
        for replacement in [BINARY + b"changed", b""]:
            with self.assertRaises(ValueError):
                self.evidence.verify_target(receipt, replacement, intent)
        for change in [{"run_attempt": 1}, {"sha": TREE}, {"vcs_modified": True}, {"goarch": "arm64"}]:
            with self.assertRaises(ValueError):
                self.evidence.verify_target(receipt | change, BINARY, intent)

    def test_full_outputs_must_match_the_current_attempt_and_target(self):
        intent = {"repo": REPO, "sha": SHA, "tree": TREE, "head": SHA, "base": TREE, "run_id": RUN, "run_attempt": ATTEMPT}
        needs = {"full": {"result": "success", "outputs": {"verified": "true", "run_id": str(RUN), "sha": SHA,
                                                               "tree": TREE, "head": SHA, "base": TREE}}}
        self.evidence.full_outputs(needs, intent)
        for key, value in [("verified", "false"), ("run_id", "11"), ("sha", TREE), ("base", SHA)]:
            changed = copy.deepcopy(needs)
            changed["full"]["outputs"][key] = value
            with self.assertRaises(ValueError):
                self.evidence.full_outputs(changed, intent)

    def test_intake_reads_numeric_triggering_actor_from_native_run(self):
        class API:
            def run(self, run_id):
                return {"actor": {"id": 43}, "triggering_actor": {"id": 43}}
        event = {"sender": {"id": 42}, "repository": {"owner": {"id": 42}}}
        import os
        from unittest.mock import patch
        with patch.dict(os.environ, {"GITHUB_REF": "refs/heads/master", "GITHUB_ACTOR_ID": "42"}), self.assertRaisesRegex(ValueError, "数字 ID"):
            try:
                self.evidence.intake(API(), event, self.receipt, 99, 1, HERE)
            except KeyError:
                self.fail("intake must use native run.triggering_actor.id, not an invented environment variable")

    def test_owner_intake_rejects_wrong_numeric_actor_and_stale_attempt(self):
        event = {"sender": {"id": 42}, "repository": {"owner": {"id": 42}}, "inputs": {"receipt": json.dumps(self.receipt)}}
        self.evidence.intake_actor(event, "refs/heads/master", 42, 42, 1)
        for args in [(43, 42, 1), (42, 43, 1), (42, 42, 2)]:
            with self.assertRaises(ValueError):
                self.evidence.intake_actor(event, "refs/heads/master", *args)
        with self.assertRaises(ValueError):
            self.evidence.bind_machine(self.receipt, self.receipt | {"run_attempt": 3}, self.receipt["target_artifact"])
        with self.assertRaises(ValueError):
            self.evidence.bind_machine(self.receipt, self.receipt, self.receipt["target_artifact"] | {"id": 31})


class EndToEndTests(unittest.TestCase):
    def setUp(self):
        import io
        import zipfile
        self.evidence = load("release-evidence")
        core = self.evidence.core
        fixtures = load("test-release-control")
        class API(fixtures.NativeAPI):
            def __init__(self):
                super().__init__()
                self.items, self.archives, self.runs = {}, {}, {}
                self.source_changed = False
                for name in ("release-evidence", "release-finalize", "release-host", "release-host-proof"):
                    self.blobs[".github/scripts/" + name + ".py"] = hashlib.sha1(name.encode()).hexdigest()
                self.trees = {tree: self.blobs.copy() for tree in self.trees}
                self.intent["sources"] = self.blobs.copy()
                self.job_rows[1].update(status="completed", conclusion="success", completed_at="2026-11-26T15:56:54Z")
                self.second_run = self.native | {"run_attempt": ATTEMPT, "run_started_at": "2026-11-26T16:00:00Z",
                    "referenced_workflows": [{"path": REPO + "/.github/workflows/ci-full.yml@refs/heads/master", "sha": fixtures.MASTER}]}
                self.second_jobs = [row | {"id": row["id"] + 100, "run_attempt": ATTEMPT,
                    "started_at": "2026-11-26T16:00:01Z", "completed_at": "2026-11-26T16:00:04Z"} for row in self.job_rows]
                self.intent.update(run_attempt=ATTEMPT, allow_retry=False)
                self.runs[RUN] = self.second_run
                # Intake arrives from a later master whose identical policy is not fetched locally.
                self.master = "f" * 40
                self.commits[self.master] = {"sha": self.master, "tree": {"sha": "3" * 40}, "parents": [{"sha": fixtures.MASTER}]}
                self.reachable[self.master] = {self.master, fixtures.MASTER, SHA, self.old}
                self.trees["3" * 40] = self.blobs.copy()
                self.runs[99] = self.native | {"id": 99, "path": core.INTAKE, "head_sha": self.master,
                    "head_commit": {"id": self.master, "tree_id": "3" * 40}, "status": "completed", "conclusion": "success",
                    "display_title": "RC receipt 10 attempt 2", "created_at": "2026-11-26T16:01:00Z",
                    "run_started_at": "2026-11-26T16:01:00Z"}
                self.intake_jobs = [{"id": 999, "name": "Release receipt intake", "run_id": 99, "run_attempt": 1,
                    "started_at": "2026-11-26T16:01:00Z", "completed_at": "2026-11-26T16:01:10Z",
                    "status": "completed", "conclusion": "success"}]
            def request(self, method, path, data=None, binary=False):
                if method == "GET":
                    if "/actions/artifacts/" in path:
                        return self.archives[int(path.split("/artifacts/")[1].split("/")[0])]
                    if "/actions/runs/" in path and "/artifacts?" in path:
                        run_id = int(path.split("/actions/runs/")[1].split("/")[0])
                        return {"artifacts": copy.deepcopy(self.items.get(run_id, []))}
                    if path in (self.prefix + "/actions/runs/10", self.prefix + "/actions/runs/10/attempts/2"):
                        return copy.deepcopy(self.runs[RUN])
                    if path in (self.prefix + "/actions/runs/99", self.prefix + "/actions/runs/99/attempts/1"):
                        return copy.deepcopy(self.runs[99])
                    if "/actions/runs/99/attempts/1/jobs?" in path:
                        return {"jobs": copy.deepcopy(self.intake_jobs)}
                    if "release-receipt.yml/runs?" in path:
                        return {"total_count": 1, "workflow_runs": [copy.deepcopy(self.runs[99])]}
                return super().request(method, path, data, binary)
            def tree_response(self, identity, recursive):
                response = super().tree_response(identity, recursive)
                if self.source_changed:
                    for row in response["tree"]:
                        if row["path"] == "release-receipt.yml":
                            row["sha"] = "8" * 40
                return response
            def upload(self, run_id, name, files, artifact_id=None, time=None):
                artifact_id = artifact_id or max([999, *self.archives]) + 1
                stream = io.BytesIO()
                with zipfile.ZipFile(stream, "w") as archive:
                    for filename, body in files.items():
                        archive.writestr(zipfile.ZipInfo(filename, (2026, 11, 26, 16, 0, 0)), body)
                raw = stream.getvalue()
                if time is None:
                    time = "2026-11-26T16:01:05Z" if run_id == 99 else "2026-11-26T16:00:12Z"
                    for prefix, at in (("release-full-", "2026-11-26T16:00:25Z"),
                                       ("release-machine-collected-", "2026-11-26T16:01:15Z"),
                                       ("release-materials-", "2026-11-26T16:01:25Z"),
                                       ("release-evidence-", "2026-11-26T16:01:35Z")):
                        if name.startswith(prefix):
                            time = at
                artifact = {"id": artifact_id, "name": name, "digest": "sha256:" + core.hashed(raw), "expired": False,
                    "workflow_run": {"id": run_id}, "created_at": time, "updated_at": time}
                self.items.setdefault(run_id, []).append(artifact)
                self.archives[artifact_id] = raw
                return artifact
        self.api = API()
        self.intent = self.api.intent
        for attempt, time in ((1, "2026-11-26T15:56:51Z"), (ATTEMPT, "2026-11-26T16:00:03Z")):
            self.api.upload(RUN, f"release-intent-{RUN}-{attempt}", {"receipt.json": core.encoded(self.intent | {"run_attempt": attempt})}, time=time)
        matrix = core.load_script("ci-plan").shard_matrix()
        names = [f"Release Full / Go {item['profile']} worker {item['worker']}" for item in matrix["include"]]
        names += ["Release Full / " + name for name in ("Go ordinary and remaining race", "Go workspace race", "Go vet and CI gates", "Go benchmarks", "Full suite evidence")]
        names += ["Release clients", "Release candidate binary", "Release Full evidence", "Release machine evidence", "Release candidate evidence"]
        for index, name in enumerate(names):
            job = {"id": index + 200, "name": name, "run_id": RUN, "run_attempt": ATTEMPT,
                "started_at": "2026-11-26T16:00:07Z", "completed_at": "2026-11-26T16:00:20Z",
                "status": "completed", "conclusion": "success"}
            windows = {"Release Full evidence": ("16:00:21", "16:00:30"),
                       "Release machine evidence": ("16:01:11", "16:01:20"),
                       "Release candidate evidence": ("16:01:21", "16:01:30")}
            if name in windows:
                job["started_at"], job["completed_at"] = ("2026-11-26T" + time + "Z" for time in windows[name])
            if " worker " in name or name in ("Release Full / Go vet and CI gates", "Release Full / Full suite evidence"):
                job["steps"] = [{"name": step, "status": "completed", "conclusion": "success",
                    "started_at": "2026-11-26T16:00:08Z", "completed_at": "2026-11-26T16:00:09Z"} for step in (
                        "Verify immutable policy input", "Checkout immutable CI policy", "Stage verified CI policy")]
            self.api.second_jobs.append(job)
        build = {key: self.intent[key] for key in ("repo", "sha", "tree", "run_id", "run_attempt", "workflow_sha")}
        build.update(schema=1, kind="release-target", artifact_sha256=core.hashed(BINARY), goos="darwin", goarch="amd64", cgo_enabled=False, vcs_modified=False, trimpath=True)
        binary_artifact = self.api.upload(RUN, f"release-target-{RUN}-{ATTEMPT}", {"receipt.json": core.encoded(build), "workspace": BINARY}, 30)
        shards = core.load_script("ci-shards")
        for item in matrix["include"]:
            profile, worker = item["profile"], item["worker"]
            total, package, summary, prefix = shards.verifier.PROFILES[profile]
            compiled = {"TestFixture" + str(i): 1 for i in range(8)}
            files = {}
            for slot in shards.BATCHES[profile][worker]:
                selected = dict(shards.verifier.selection(compiled, slot, profile)[0])
                files[f"{prefix}{slot}/{summary}"] = core.encoded({"schema": 1, "slot": slot, "total": total,
                    "run_id": str(RUN), "checkout_sha": SHA, "checkout_tree": TREE, "package": package,
                    "compiled": compiled, "selected": selected, "runs": selected, "passes": selected, "skips": {}})
            self.api.upload(RUN, f"{profile}-worker-v1-{worker}", files)
        self.api.upload(RUN, "go-benchmarks", {"go-benchmarks.txt": b"BenchmarkFixture 1 1 ns/op 0 B/op 0 allocs/op\n"})
        self.needs = {name: {"result": "success"} for name in ("full", "clients", "binary", "full-evidence", "machine")}
        self.needs["full"]["outputs"] = {"verified": "true", "run_id": str(RUN), **{key: self.intent[key] for key in ("sha", "tree", "head", "base")}}
        # Native Full is prepared by each test, after exercising any mutations.
        machine = machine_receipt()
        machine["workflow_sha"] = self.intent["workflow_sha"]
        machine["target_artifact"] = self.evidence.artifact_fact(binary_artifact, core.hashed(core.encoded(build)))
        self.wrapper = {key: self.intent[key] for key in ("repo", "sha", "tree", "run_id", "run_attempt")} | {
            "schema": 1, "kind": "release-machine-intake", "machine": machine, **self.evidence.AWAITING,
            "machine_signature": "", "intake": {"run_id": 99, "run_attempt": 1, "actor_id": 42, "workflow_sha": self.api.master}}

    def ready(self):
        core = self.evidence.core
        full = self.evidence.full_evidence(self.api, self.intent, self.needs)
        self.api.upload(RUN, f"release-full-{RUN}-{ATTEMPT}", {"receipt.json": core.encoded(full)})
        machine = self.wrapper["machine"]
        machine["full_binding"] = self.evidence.full_binding(self.api, self.intent)
        for lane in machine["lanes"]:
            lane["full_binding_sha256"] = core.hashed(core.encoded(machine["full_binding"]))
        self.api.upload(99, f"release-machine-{RUN}-{ATTEMPT}-99", {"receipt.json": core.encoded(self.wrapper)})
        return full

    def test_complete_native_attempt_produces_verdict_with_receipt_hashes(self):
        core = self.evidence.core
        self.ready()
        try:
            collected = self.evidence.collect(self.api, self.intent, HERE, 0)
        except Exception as error:
            self.fail("independent native intake must be collectible after master advances: " + str(error))
        self.api.upload(RUN, f"release-machine-collected-{RUN}-{ATTEMPT}", {"receipt.json": core.encoded(collected)})
        verdict = self.evidence.verdict(self.api, self.intent, self.needs, HERE)
        self.assertEqual(verdict["kind"], "release-materials")
        self.assertEqual(verdict["status"], "collected")
        self.assertFalse(verdict["verified"] or verdict["authoritative"])
        proof = load("release-host-proof")
        proof._materials(verdict, self.intent["workflow_sha"])
        self.assertEqual(verdict["artifact_sha256"], core.hashed(BINARY))
        self.assertEqual(len(verdict["machine_lanes"]), 3)
        self.assertEqual(verdict["machine_receipt"], self.wrapper["machine"])
        self.assertEqual(verdict["machine_signature"], "")
        self.assertEqual((verdict["full"]["run_id"], verdict["full"]["run_attempt"]), (RUN, ATTEMPT))
        self.assertTrue(all("receipt_sha256" in verdict[name] for name in ("target", "full", "machine", "intake")))
        self.assertEqual(sum(len(item["receipt_sha256s"]) for item in verdict["full_lanes"]), 82)

    def test_partial_rerun_or_failed_native_full_job_never_produces_full_receipt(self):
        job = self.api.second_jobs[2]
        for changes in [{"run_attempt": 1}, {"conclusion": "failure"}, {"conclusion": "cancelled"}]:
            original = dict(job)
            job.update(changes)
            with self.assertRaises(ValueError):
                self.evidence.full_evidence(self.api, self.intent, self.needs)
            job.clear()
            job.update(original)

    def test_durable_owner_does_not_depend_on_expired_intent_artifact(self):
        control = self.evidence.control
        self.api.items[RUN] = [row for row in self.api.items[RUN] if row["name"] != f"release-intent-{RUN}-1"]
        try:
            self.assertEqual(control.canonical_status(self.api, REPO, SHA), RUN)
        except ValueError as error:
            self.fail("durable owner must survive artifact retention: " + str(error))

    def test_owner_must_be_native_master_dispatch(self):
        self.api.native["head_branch"] = "other"
        self.api.runs[RUN]["head_branch"] = "other"
        with self.assertRaises(ValueError):
            self.evidence.control.canonical_status(self.api, REPO, SHA)

    def test_full_requires_actual_trusted_policy_steps(self):
        for job in self.api.second_jobs:
            for step in job.get("steps", []):
                for changes in ({"conclusion": "skipped"}, {"started_at": "2026-11-26T15:59:00Z"}):
                    original = dict(step)
                    step.update(changes)
                    with self.subTest(job=job["name"], step=step["name"], changes=changes), self.assertRaises(ValueError):
                        self.evidence.full_evidence(self.api, self.intent, self.needs)
                    step.clear()
                    step.update(original)

    def test_product_target_can_differ_from_native_immutable_reviewed_policy(self):
        self.assertNotEqual(self.intent["sha"], self.intent["workflow_sha"])
        for path in self.evidence.control.FULL_PATHS:
            self.api.trees[TREE][path] = "7" * 40
        self.assertEqual(self.evidence.control.guard(self.api, RUN, ATTEMPT), self.intent)
        self.evidence.full_evidence(self.api, self.intent, self.needs)

    def test_shards_are_semantically_reaggregated_not_just_rehashed(self):
        import io
        import zipfile
        artifact = next(row for row in self.api.items[RUN] if row["name"] == "store-worker-v1-0")
        original = self.api.archives[artifact["id"]]
        with zipfile.ZipFile(io.BytesIO(original)) as archive:
            files = {name: archive.read(name) for name in archive.namelist()}
        name = sorted(files)[0]
        summary = self.evidence.core.decode(files[name])
        for changes in ({"checkout_sha": "d" * 40}, {"run_id": "999"}, {"compiled": {"TestUncovered": 1}}, {"unexpected": True}):
            files[name] = self.evidence.core.encoded(summary | changes)
            stream = io.BytesIO()
            with zipfile.ZipFile(stream, "w") as archive:
                for entry, data in files.items():
                    archive.writestr(entry, data)
            raw = stream.getvalue()
            self.api.archives[artifact["id"]] = raw
            artifact["digest"] = "sha256:" + self.evidence.core.hashed(raw)
            with self.subTest(changes=changes), self.assertRaises(ValueError):
                self.evidence.full_evidence(self.api, self.intent, self.needs)

    def test_legacy_job_timestamps_relabelled_as_current_attempt_are_rejected(self):
        job = self.api.second_jobs[2]
        job.update(started_at="2026-11-26T15:56:48Z", completed_at="2026-11-26T15:56:52Z")
        with self.assertRaises(ValueError):
            self.evidence.full_evidence(self.api, self.intent, self.needs)

    def test_artifact_creation_and_update_must_both_be_in_producer_window(self):
        artifact = next(row for row in self.api.items[RUN] if row["name"] == "store-worker-v1-0")
        for field, time in (("created_at", "2026-11-26T15:59:00Z"), ("updated_at", "2026-11-26T16:01:00Z")):
            original = artifact[field]
            artifact[field] = time
            with self.subTest(field=field), self.assertRaises(ValueError):
                self.evidence.full_evidence(self.api, self.intent, self.needs)
            artifact[field] = original

    def test_guard_rejects_cancelled_owner_or_newer_attempt(self):
        for change in [{"status": "completed", "conclusion": "cancelled"}, {"run_attempt": 3}]:
            original = dict(self.api.runs[RUN])
            self.api.runs[RUN].update(change)
            with self.subTest(change=change), self.assertRaises(ValueError):
                self.evidence.control.guard(self.api, RUN, ATTEMPT)
            self.api.runs[RUN] = original

    def test_changed_intake_source_and_foreign_numeric_owner_fail_closed(self):
        intent = self.intent | {"artifact_sha256": hashlib.sha256(BINARY).hexdigest()}
        self.api.source_changed = True
        with self.assertRaises(ValueError):
            try:
                self.evidence.verify_intake(self.api, self.api.runs[99], self.wrapper, intent, self.wrapper["machine"]["target_artifact"], HERE)
            except self.evidence.subprocess.SubprocessError:
                self.fail("intake must compare authenticated source blobs, not depend on a stale local fetch")
        self.api.source_changed = False
        self.api._objects.clear()
        self.api.runs[99]["triggering_actor"] = {"id": 43}
        with self.assertRaises(ValueError):
            self.evidence.verify_intake(self.api, self.api.runs[99], self.wrapper, intent, self.wrapper["machine"]["target_artifact"], HERE)
class FullBindingTests(unittest.TestCase):
    setUp = EndToEndTests.setUp
    ready = EndToEndTests.ready

    def test_full_join_rehashes_native_shards_jobs_and_binary_not_just_output_strings(self):
        self.ready()
        binding = self.evidence.full_binding(self.api, self.intent)
        producer = next(job for job in self.api.second_jobs if job["name"] == "Release Full evidence")
        self.assertEqual((binding["run_id"], binding["run_attempt"], binding["job_id"]), (RUN, ATTEMPT, producer["id"]))
        job = self.api.second_jobs[2]
        for changes in ({"run_attempt": 1}, {"name": "Full suite / Go store worker 0"}, {"conclusion": "cancelled"}):
            original = dict(job)
            job.update(changes)
            with self.subTest(changes=changes), self.assertRaises(ValueError):
                self.evidence.full_binding(self.api, self.intent)
            job.clear()
            job.update(original)
        artifact = next(row for row in self.api.items[RUN] if row["name"] == "store-worker-v1-0")
        self.api.archives[artifact["id"]] += b"changed raw archive"
        with self.assertRaises(ValueError):
            self.evidence.full_binding(self.api, self.intent)


class CollectorTests(unittest.TestCase):
    def test_only_exact_run_name_within_current_attempt_window_reaches_payloads(self):
        evidence = load("release-evidence")
        self.assertTrue(hasattr(evidence, "find_intakes"), "缺少按 owner/attempt 有界检索")
        intent = {"repo": REPO, "sha": SHA, "tree": TREE, "run_id": RUN, "run_attempt": ATTEMPT}
        requests, payloads = [], []
        class API:
            repo, prefix = REPO, "/repos/" + REPO
            def get(self, path):
                if path == self.prefix:
                    return {"owner": {"id": 42, "login": "owner"}}
                requests.append(path)
                return {"workflow_runs": [
                    {"id": i, "display_title": title, "created_at": created,
                     "actor": {"id": actor}, "triggering_actor": {"id": actor},
                     "head_branch": "master", "event": "workflow_dispatch", "run_attempt": 1,
                     "status": "completed", "conclusion": "success"}
                    for i, title, created, actor in ((90, "old release", "2026-11-26T00:00:00Z", 42),
                        (91, "RC receipt 10 attempt 2", "2026-11-25T00:00:00Z", 42),
                        (92, "RC receipt 10 attempt 2", "2026-11-26T00:00:01Z", 43),
                        (93, "RC receipt 10 attempt 2", "2026-11-26T00:00:01Z", 42))]}
            def run(self, run_id, attempt=None):
                if run_id == RUN:
                    return {"run_started_at": "2026-11-26T00:00:00Z", "run_attempt": ATTEMPT}
                return {"id": run_id}
            def jobs(self, run_id, attempt):
                return [{"id": 7, "name": "Release receipt intake", "run_id": run_id,
                         "run_attempt": attempt, "status": "completed", "conclusion": "success"}]
            def receipt(self, run_id, name, producer):
                payloads.append(run_id)
                return {}, {}, "a" * 64
        with patch.object(evidence, "verify_intake"), patch.object(evidence, "target", return_value=({"artifact_sha256": "a" * 64}, b"", {})):
            self.assertEqual(len(evidence.find_intakes(API(), intent, HERE)), 1)
        self.assertEqual(payloads, [93])
        self.assertEqual(len(requests), 1)
        self.assertIn("branch=master", requests[0])
        self.assertIn("actor=owner", requests[0])
        self.assertIn("created=", requests[0])
        self.assertIn("per_page=100", requests[0])

    def test_intake_new_master_is_verified_via_native_tree_not_local_git_object(self):
        evidence = load("release-evidence")
        fixtures = load("test-release-control")
        api = fixtures.NativeAPI()
        with patch.object(evidence.core, "git", side_effect=AssertionError("new master not fetched")):
            evidence.intake_source(api, api.intent, fixtures.MASTER)
            api._objects.clear()
            api.trees["1" * 40][evidence.core.INTAKE] = "d" * 40
            with self.assertRaises(ValueError):
                evidence.intake_source(api, api.intent, fixtures.MASTER)
            api._objects.clear()
            api.reachable[fixtures.MASTER].remove(SHA)
            with self.assertRaises(ValueError):
                evidence.intake_source(api, api.intent, SHA)
        requests = [path for _, path in api.calls]
        self.assertIn(api.prefix + "/git/commits/" + fixtures.MASTER, requests)
        self.assertIn(api.prefix + "/git/trees/" + "1" * 40, requests)
        self.assertNotIn(api.prefix + "/git/trees/" + "1" * 40 + "?recursive=1", requests)

    def test_native_actor_not_environment_actor_is_authoritative(self):
        evidence = load("release-evidence")
        event = {"sender": {"id": 42}, "repository": {"owner": {"id": 42}}}
        api = SimpleNamespace(run=lambda _: {"actor": {"id": 43}, "triggering_actor": {"id": 42}})
        with patch.dict(__import__("os").environ, {"GITHUB_REF": "refs/heads/master", "GITHUB_ACTOR_ID": "42"}), \
             self.assertRaisesRegex(ValueError, "数字 ID"):
            evidence.intake(api, event, {}, 99, 1, HERE)


class IdleCollectorTests(unittest.TestCase):
    setUp = EndToEndTests.setUp
    ready = EndToEndTests.ready

    def test_idle_poll_does_not_repeat_deep_guard_or_full_download(self):
        self.ready()
        finder = self.evidence.find_intakes
        polls = []
        def find(*args):
            polls.append(True)
            return finder(*args) if len(polls) == 3 else []
        with patch.object(self.evidence, "find_intakes", side_effect=find), \
             patch.object(self.evidence.control, "guard", wraps=self.evidence.control.guard) as guard, \
             patch.object(self.evidence, "full_binding", wraps=self.evidence.full_binding) as full, \
             patch.object(self.evidence.time, "sleep"):
            result = self.evidence.collect(self.api, self.intent, HERE, 30)
        self.assertEqual(result["machine"], self.wrapper["machine"])
        self.assertEqual(guard.call_count, 2, "deep guard is only initial and on matching receipt arrival")
        self.assertEqual(full.call_count, 1)

    def test_stopped_owner_never_accepts_receipt_after_initial_guard(self):
        self.ready()
        def stop(*args):
            self.api.runs[RUN].update(status="completed", conclusion="cancelled")
            return []
        with patch.object(self.evidence, "find_intakes", side_effect=stop), patch.object(self.evidence.time, "sleep"), self.assertRaises(ValueError):
            self.evidence.collect(self.api, self.intent, HERE, 30)

    def test_signature_is_separate_bounded_ascii_and_replay_scope_is_rechecked(self):
        self.ready()
        native = self.api.runs[99]
        intent = self.intent | {"artifact_sha256": self.wrapper["machine"]["artifact_sha256"]}
        self.wrapper["machine_signature"] = "YWJj"
        self.evidence.verify_intake(self.api, native, self.wrapper, intent, self.wrapper["machine"]["target_artifact"], HERE)
        for signature in (True, "密钥", "A" * 16385, "raw\nprivate"):
            self.wrapper["machine_signature"] = signature
            with self.subTest(signature=repr(signature)[:20]), self.assertRaises(ValueError):
                self.evidence.verify_intake(self.api, native, self.wrapper, intent, self.wrapper["machine"]["target_artifact"], HERE)
        self.wrapper["machine_signature"] = ""
        for changes in ({"display_title": "RC receipt 10 attempt 1"}, {"head_sha": SHA}, {"run_attempt": 2},
                        {"created_at": "2026-11-26T15:59:00Z"}, {"created_at": "2026-11-26T16:00:22Z"}):
            with self.subTest(changes=changes), self.assertRaises(ValueError):
                self.evidence.verify_intake(self.api, native | changes, self.wrapper, intent, self.wrapper["machine"]["target_artifact"], HERE)


if __name__ == "__main__":
    unittest.main()
