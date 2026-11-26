#!/usr/bin/env python3
"""Build-byte identity, independent Full proof, and owner-authenticated machine intake."""
import argparse
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import time

import importlib.util
_spec = importlib.util.spec_from_file_location("release_core", Path(__file__).with_name("release-core.py"))
core = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(core)
control = core.load_script("release-control")
check = core.check
SOURCES = {"driver_6a", "driver_6b", "observer_6b", "audit_6b", "facts_6b", "prepare_6b", "seed_6b", "driver_6c", "audit_6c", "release_machine"}


def keys(value, expected):
    check(isinstance(value, dict) and set(value) == set(expected), "回执字段不符或含未授权内容")


def validate_machine(value):
    keys(value, {"schema", "kind", "repo", "sha", "tree", "run_id", "run_attempt", "status", "verified", "workflow_sha",
                 "artifact_sha256", "target_artifact", "machine_id", "source_bundle_sha256", "lanes", "unknown_guard", "sources"})
    core.scope(value, value["repo"], value["sha"], value["tree"], value["run_id"], value["run_attempt"])
    check(value["kind"] == "release-machine" and value["status"] == "success" and value["verified"] is True, "机器验收未通过")
    core.sha(value["workflow_sha"])
    for field in ("artifact_sha256", "machine_id", "source_bundle_sha256"):
        core.digest(value[field])
    artifact = value["target_artifact"]
    keys(artifact, {"id", "digest", "receipt_sha256"})
    core.number(artifact["id"])
    core.exact(artifact["digest"], r"sha256:[0-9a-f]{64}", "原生制品摘要非法")
    core.digest(artifact["receipt_sha256"])
    keys(value["sources"], SOURCES)
    for item in value["sources"].values():
        core.digest(item)
    keys(value["unknown_guard"], {"mode", "proof_sha256"})
    check(value["unknown_guard"]["mode"] in ("observed", "race"), "unknown 判据缺少实际观察或同提交 race 回归")
    core.digest(value["unknown_guard"]["proof_sha256"])
    lanes = value["lanes"]
    check(isinstance(lanes, list) and [lane.get("name") for lane in lanes] == ["6a", "6b", "6c"], "必须包含三个完整的原生验收场景")
    turns, sessions = set(), set()
    for lane in lanes:
        keys(lane, {"name", "status", "audit_sha256", "assertions_sha256", "assertion_count", "proofs", "attempt_ids"})
        check(lane["status"] == "success", "原生场景未通过")
        core.number(lane["assertion_count"])
        core.digest(lane["audit_sha256"])
        core.digest(lane["assertions_sha256"])
        check(isinstance(lane["proofs"], list) and len(lane["proofs"]) == (6 if lane["name"] == "6b" else 2), "原生 Turn 证据数量不符")
        check(isinstance(lane["attempt_ids"], list) and (bool(lane["attempt_ids"]) == (lane["name"] == "6b")), "任务 attempt 证据缺失")
        for attempt_id in lane["attempt_ids"]:
            core.exact(attempt_id, r"[A-Za-z0-9_-]{1,128}", "任务 attempt ID 非法")
        for proof in lane["proofs"]:
            keys(proof, {"turn_id", "session_id", "protocol_sha256", "transcript_sha256"})
            turn = core.exact(proof["turn_id"], r"[A-Za-z0-9_-]{1,128}", "Turn ID 非法")
            session = core.exact(proof["session_id"], r"[A-Za-z0-9_-]{1,128}", "原生会话 ID 非法")
            check(turn not in turns and session not in sessions, "原生 Turn 或会话重复")
            turns.add(turn)
            sessions.add(session)
            core.digest(proof["protocol_sha256"])
            core.digest(proof["transcript_sha256"])
    check(len(core.encoded(value)) <= core.MAX_RECEIPT, "机器回执过大")


def verify_target(receipt, binary, intent):
    core.scope(receipt, intent["repo"], intent["sha"], intent["tree"], intent["run_id"], intent["run_attempt"])
    check(receipt.get("kind") == "release-target" and receipt.get("workflow_sha") == intent["workflow_sha"]
          and receipt.get("artifact_sha256") == core.hashed(binary) and bool(binary), "目标二进制字节、构建来源不符")
    check(receipt.get("goos") == "darwin" and receipt.get("goarch") == "amd64" and receipt.get("cgo_enabled") is False
          and receipt.get("vcs_modified") is False and receipt.get("trimpath") is True, "目标二进制构建配置不符")


def full_outputs(needs, intent):
    full = needs.get("full", {})
    check(full.get("result") == "success", "Full 没有成功完成")
    expected = {"verified": "true", "run_id": str(intent["run_id"]), **{key: intent[key] for key in ("sha", "tree", "head", "base")}}
    check(all(full.get("outputs", {}).get(key) == value for key, value in expected.items()), "Full 实际输出与目标身份不符")


def intake_actor(event, ref, actor_id, triggering_actor_id, attempt):
    owner = event.get("repository", {}).get("owner", {}).get("id")
    core.number(owner)
    check(ref == "refs/heads/master" and actor_id == triggering_actor_id == event.get("sender", {}).get("id") == owner
          and attempt == 1, "回执仅接受仓库所有者数字 ID 从 master 新触发；禁止转交或局部重跑")


def bind_machine(value, intent, target_artifact):
    validate_machine(value)
    core.scope(value, intent["repo"], intent["sha"], intent["tree"], intent["run_id"], intent["run_attempt"])
    check(value["workflow_sha"] == intent["workflow_sha"] and value["artifact_sha256"] == intent["artifact_sha256"]
          and value["target_artifact"] == target_artifact, "机器证据没有绑定同一次构建字节及原生 artifact")


def artifact_fact(artifact, receipt_hash=None):
    result = {"id": artifact["id"], "digest": artifact["digest"]}
    if receipt_hash:
        result["receipt_sha256"] = receipt_hash
    return result


def target(api, intent):
    run_id, attempt = intent["run_id"], intent["run_attempt"]
    job = core.require_jobs(api.jobs(run_id, attempt), run_id, attempt, ["Release candidate binary"])[0]
    artifact = api.artifact(run_id, f"release-target-{run_id}-{attempt}", job)
    files = api.download(artifact, {"receipt.json", "workspace"})
    receipt = core.decode(files["receipt.json"])
    verify_target(receipt, files["workspace"], intent)
    return receipt, files["workspace"], artifact_fact(artifact, core.hashed(files["receipt.json"]))


def build_receipt(api, intent, candidate, binary, output):
    check(core.git(candidate, "rev-parse", "HEAD") == intent["sha"]
          and core.git(candidate, "rev-parse", "HEAD^{tree}") == intent["tree"]
          and not core.git(candidate, "status", "--porcelain", "--untracked-files=all"), "候选检出不干净或身份不符")
    metadata = subprocess.check_output(["go", "version", "-m", str(binary)], text=True, timeout=30)
    check("\tpath\tagentworkspace/cmd/workspace\n" in metadata, "不是 workspace 二进制")
    settings = {}
    for line in metadata.splitlines():
        if line.startswith("\tbuild\t"):
            name, _, value = line[len("\tbuild\t"):].partition("=")
            settings[name] = value
    expected = {"vcs.revision": intent["sha"], "vcs.modified": "false", "vcs": "git", "GOOS": "darwin",
                "GOARCH": "amd64", "CGO_ENABLED": "0", "-trimpath": "true"}
    check(all(settings.get(key) == value for key, value in expected.items()) and "-overlay" not in settings, "二进制 Go 构建身份不符")
    receipt = {key: intent[key] for key in ("repo", "sha", "tree", "run_id", "run_attempt", "workflow_sha")}
    receipt.update(schema=1, kind="release-target", artifact_sha256=core.file_hash(binary), goos="darwin", goarch="amd64",
                   cgo_enabled=False, vcs_modified=False, trimpath=True)
    core.save(output / "receipt.json", receipt)


def verify_reusable(api, intent):
    check(intent.get("policy_sha") == intent["workflow_sha"] and intent.get("policy") == "caller-policy-v1", "Full 缺少确切调用者策略身份")
    run = api.run(intent["run_id"], intent["run_attempt"])
    references = [item for item in run.get("referenced_workflows", [])
                  if item.get("path", "").split("@")[0] == api.repo + "/.github/workflows/ci-full.yml"]
    check(len(references) == 1 and references[0].get("sha") == intent["workflow_sha"], "实际调用的 reusable Full 源码 SHA 不符或不可核实")


def full_evidence(api, intent, needs):
    full_outputs(needs, intent)
    verify_reusable(api, intent)
    run_id, attempt = intent["run_id"], intent["run_attempt"]
    matrix = core.load_script("ci-plan").shard_matrix()
    names = [f"Release Full / Go {item['profile']} worker {item['worker']}" for item in matrix["include"]]
    names += ["Release Full / " + name for name in ("Go ordinary and remaining race", "Go workspace race", "Go vet and CI gates", "Go benchmarks", "Full suite evidence")]
    names += ["Release clients", "Release candidate binary", "Release normalize", "Release admission"]
    jobs = core.require_jobs(api.jobs(run_id, attempt), run_id, attempt, names)
    for job in jobs:
        if job["name"] in names[:len(matrix["include"])] or job["name"] in ("Release Full / Go vet and CI gates", "Release Full / Full suite evidence"):
            for name in ("Verify immutable policy input", "Checkout immutable CI policy", "Stage verified CI policy"):
                steps = [step for step in job.get("steps", []) if step["name"] == name]
                check(len(steps) == 1 and steps[0].get("status") == "completed" and steps[0].get("conclusion") == "success",
                      "本次 Full 没有实际执行受信任的不可变 CI 策略步骤：" + name)
    build, _, target_fact = target(api, intent)  # Independent download and byte hash, not a needs string.
    shards = core.load_script("ci-shards")
    artifacts = []
    for item in matrix["include"]:
        profile, worker = item["profile"], item["worker"]
        producer = next(job for job in jobs if job["name"] == f"Release Full / Go {profile} worker {worker}")
        artifact = api.artifact(run_id, f"{profile}-worker-v1-{worker}", producer)
        _, _, summary, prefix = shards.verifier.PROFILES[profile]
        expected = {f"{prefix}{slot}/{summary}" for slot in shards.BATCHES[profile][worker]}
        files = api.download(artifact, expected, 16 << 20)
        artifacts.append(artifact_fact(artifact) | {"job_id": producer["id"], "run_attempt": attempt,
                                                   "receipt_sha256s": [core.hashed(files[name]) for name in sorted(files)]})
    benchmark = next(job for job in jobs if job["name"] == "Release Full / Go benchmarks")
    artifact = api.artifact(run_id, "go-benchmarks", benchmark)
    files = api.download(artifact, {"go-benchmarks.txt"}, 2 << 20)
    artifacts.append(artifact_fact(artifact) | {"job_id": benchmark["id"], "run_attempt": attempt,
                                               "receipt_sha256s": [core.hashed(files["go-benchmarks.txt"])]})
    return {key: intent[key] for key in ("repo", "sha", "tree", "run_id", "run_attempt", "workflow_sha")} | {
        "schema": 1, "kind": "release-full", "status": "success", "verified": True,
        "artifact_sha256": build["artifact_sha256"], "target_artifact": target_fact, "artifacts": artifacts,
        "jobs": [{key: job[key] for key in ("id", "name", "run_attempt", "conclusion")} for job in jobs]}


def intake_source(api, intent, intake_sha):
    core.sha(intake_sha)
    master = core.sha(api.get(api.prefix + "/commits/master")["sha"])
    comparison = api.get(api.prefix + f"/compare/{intake_sha}...{master}")
    check(comparison.get("status") in ("ahead", "identical") and comparison.get("merge_base_commit", {}).get("sha") == intake_sha,
          "intake 调用源码不可从当前远端 master 到达")
    # Master can advance after the collector checks out. REST blob identity does
    # not depend on the collector having fetched that future commit locally.
    for path in (core.INTAKE, ".github/scripts/release-core.py", ".github/scripts/release-control.py", ".github/scripts/release-evidence.py"):
        blob = api.get(api.prefix + "/contents/" + path + "?ref=" + intake_sha)
        check(blob.get("sha") == intent["sources"][path], "intake 控制源码与候选认领不符")


def intake(api, event, value, intake_run, intake_attempt, root):
    native_intake = api.run(intake_run)
    intake_actor(event, os.environ["GITHUB_REF"], int(os.environ["GITHUB_ACTOR_ID"]),
                 native_intake.get("triggering_actor", {}).get("id"), intake_attempt)
    validate_machine(value)
    owner = api.run(value["run_id"])
    check(owner.get("run_attempt") == value["run_attempt"] and owner.get("status") == "in_progress", "canonical owner 已停止或机器回执 attempt 过期")
    intent = control.guard(api, value["run_id"], value["run_attempt"])
    build, _, target_fact = target(api, intent)
    bind_machine(value, intent | {"artifact_sha256": build["artifact_sha256"]}, target_fact)
    intake_sha = core.sha(os.environ["GITHUB_WORKFLOW_SHA"])
    intake_source(api, intent, intake_sha)
    return {key: value[key] for key in ("repo", "sha", "tree", "run_id", "run_attempt")} | {
        "schema": 1, "kind": "release-machine-intake", "machine": value,
        "intake": {"run_id": intake_run, "run_attempt": intake_attempt, "actor_id": event["sender"]["id"], "workflow_sha": intake_sha}}


def verify_intake(api, native, wrapper, intent, target_fact, root):
    owner_id = api.get(api.prefix)["owner"]["id"]
    check(native.get("path") == core.INTAKE and native.get("event") == "workflow_dispatch" and native.get("head_branch") == "master"
          and native.get("status") == "completed" and native.get("conclusion") == "success" and native.get("run_attempt") == 1
          and native.get("actor", {}).get("id") == native.get("triggering_actor", {}).get("id") == owner_id,
          "机器证据不是 owner 从 master 发出的受信任 intake")
    core.scope(wrapper, api.repo, intent["sha"], intent["tree"], intent["run_id"], intent["run_attempt"])
    check(wrapper.get("kind") == "release-machine-intake" and wrapper.get("intake") == {
        "run_id": native["id"], "run_attempt": 1, "actor_id": owner_id, "workflow_sha": native["head_sha"]}, "intake 原生身份不符")
    intake_source(api, intent, native["head_sha"])
    bind_machine(wrapper["machine"], intent, target_fact)


def collect(api, intent, root, seconds):
    build, _, target_fact = target(api, intent)
    intent = intent | {"artifact_sha256": build["artifact_sha256"]}
    deadline = time.monotonic() + seconds
    while True:
        control.guard(api, intent["run_id"], intent["run_attempt"])
        candidates = []
        for run in api.pages(f"{api.prefix}/actions/workflows/release-receipt.yml/runs?event=workflow_dispatch", "workflow_runs"):
            if run.get("status") != "completed" or run.get("conclusion") != "success":
                continue
            name = f"release-machine-{intent['run_id']}-{intent['run_attempt']}-{run['id']}"
            if not any(artifact["name"] == name for artifact in api.artifacts(run["id"])):
                continue
            native = api.run(run["id"], 1)
            producer = core.require_jobs(api.jobs(run["id"], 1), run["id"], 1, ["Release receipt intake"])[0]
            wrapper, artifact, receipt_hash = api.receipt(run["id"], name, producer)
            verify_intake(api, native, wrapper, intent, target_fact, root)
            candidates.append((wrapper, artifact, receipt_hash))
        if candidates:
            hashes = {core.hashed(core.encoded(item[0]["machine"])) for item in candidates}
            check(len(hashes) == 1, "收到冲突的原生机器回执")
            wrapper, artifact, receipt_hash = min(candidates, key=lambda item: item[1]["id"])
            return {key: intent[key] for key in ("repo", "sha", "tree", "run_id", "run_attempt")} | {
                "schema": 1, "kind": "release-machine-collected", "intake_run_id": wrapper["intake"]["run_id"],
                "intake_artifact": artifact_fact(artifact, receipt_hash), "machine": wrapper["machine"]}
        check(time.monotonic() < deadline, "在限定时间内没有收到可核实的本地机器验收；禁止放行")
        time.sleep(min(45, max(0, deadline - time.monotonic())))


def verdict(api, intent, needs, root):
    check(all(needs.get(name, {}).get("result") == "success" for name in ("full", "clients", "binary", "full-evidence", "machine")), "Full 或机器证据未全部成功；禁止产生已验证证据")
    run_id, attempt = intent["run_id"], intent["run_attempt"]
    jobs = core.require_jobs(api.jobs(run_id, attempt), run_id, attempt, ["Release Full evidence", "Release machine evidence", "Release clients", "Release candidate binary"])
    build, _, target_fact = target(api, intent)
    full, full_artifact, full_hash = api.receipt(run_id, f"release-full-{run_id}-{attempt}", jobs[0])
    core.scope(full, api.repo, intent["sha"], intent["tree"], run_id, attempt)
    check(full.get("kind") == "release-full" and full.get("status") == "success" and full.get("verified") is True
          and full.get("artifact_sha256") == build["artifact_sha256"] and full.get("target_artifact") == target_fact, "Full 回执与本次构建不符")
    verify_reusable(api, intent)
    # Verify every recorded native Full job again, not just reusable output strings.
    recorded = core.require_jobs(api.jobs(run_id, attempt), run_id, attempt, [job["name"] for job in full["jobs"]])
    check({job["id"] for job in recorded} == {job["id"] for job in full["jobs"]}, "Full 原生 job ID 已改变")
    machine, machine_artifact, machine_hash = api.receipt(run_id, f"release-machine-collected-{run_id}-{attempt}", jobs[1])
    core.scope(machine, api.repo, intent["sha"], intent["tree"], run_id, attempt)
    bind_machine(machine["machine"], intent | {"artifact_sha256": build["artifact_sha256"]}, target_fact)
    native = api.run(machine["intake_run_id"], 1)
    producer = core.require_jobs(api.jobs(native["id"], 1), native["id"], 1, ["Release receipt intake"])[0]
    wrapper, intake_artifact, intake_hash = api.receipt(native["id"], f"release-machine-{run_id}-{attempt}-{native['id']}", producer)
    verify_intake(api, native, wrapper, intent | {"artifact_sha256": build["artifact_sha256"]}, target_fact, root)
    check(machine["intake_artifact"] == artifact_fact(intake_artifact, intake_hash)
          and machine["machine"] == wrapper["machine"], "机器 collector 与原生 intake 证据不符")
    return {key: intent[key] for key in ("repo", "sha", "tree", "run_id", "run_attempt", "workflow_sha")} | {
        "schema": 1, "kind": "release-evidence", "status": "success", "verified": True,
        "artifact_sha256": build["artifact_sha256"], "target": target_fact,
        "full": artifact_fact(full_artifact, full_hash), "machine": artifact_fact(machine_artifact, machine_hash),
        "intake": artifact_fact(intake_artifact, intake_hash), "full_lanes": full["artifacts"], "machine_lanes": wrapper["machine"]["lanes"]}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("binary", "full", "collect", "intake", "verdict"))
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--candidate", type=Path)
    parser.add_argument("--binary", type=Path)
    parser.add_argument("--wait-seconds", type=int, default=6600)
    args = parser.parse_args()
    api = control.AdmissionAPI(os.environ["GITHUB_REPOSITORY"])
    run_id, attempt = int(os.environ["GITHUB_RUN_ID"]), int(os.environ["GITHUB_RUN_ATTEMPT"])
    if args.command == "intake":
        event = core.decode(Path(os.environ["GITHUB_EVENT_PATH"]).read_bytes(), 8 << 20)
        value = core.decode(event["inputs"]["receipt"].encode())
        result = intake(api, event, value, run_id, attempt, Path.cwd())
        core.outputs({"owner_run_id": value["run_id"], "owner_attempt": value["run_attempt"]})
    else:
        intent = control.guard(api, run_id, attempt)
        if args.command == "binary":
            build_receipt(api, intent, args.candidate, args.binary, args.output)
            return
        if args.command == "full":
            result = full_evidence(api, intent, core.decode(os.environ["NEEDS_JSON"].encode(), 1 << 20))
        elif args.command == "collect":
            check(0 <= args.wait_seconds <= 6600, "collector 等待超限")
            result = collect(api, intent, Path.cwd(), args.wait_seconds)
        else:
            result = verdict(api, intent, core.decode(os.environ["NEEDS_JSON"].encode(), 1 << 20), Path.cwd())
    core.save(args.output / ("evidence.json" if args.command == "verdict" else "receipt.json"), result)


if __name__ == "__main__":
    try:
        main()
    except (ValueError, KeyError, OSError, subprocess.SubprocessError) as error:
        print("候选证据失败：" + str(error), file=sys.stderr)
        sys.exit(1)
