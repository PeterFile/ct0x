#!/usr/bin/env python3
"""Build-byte identity, independent Full proof, and owner-authenticated machine intake."""
import argparse
import base64
from datetime import datetime, timedelta
from urllib.parse import urlencode
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import time
import tempfile

import importlib.util
_spec = importlib.util.spec_from_file_location("release_core", Path(__file__).with_name("release-core.py"))
core = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(core)
control = core.load_script("release-control")
check = core.check
SOURCES = {"driver_6a", "driver_6b", "observer_6b", "audit_6b", "facts_6b", "prepare_6b", "seed_6b", "driver_6c", "audit_6c",
           "seed_6c", "legacy_6a", "runner_6b", "verifier_6b", "export_fixture", "native_exact", "native_preliminary",
           "native_project_stage", "normal_entry", "compare_first_turn", "release_core", "release_control",
           "release_evidence", "release_machine", "release_finalize", "release_host", "release_host_proof", "release_reconcile"}
CONTROL_SOURCES = SOURCES - {"driver_6a", "driver_6b", "observer_6b", "audit_6b", "facts_6b", "prepare_6b", "seed_6b", "driver_6c", "audit_6c",
                             "seed_6c", "legacy_6a", "runner_6b", "verifier_6b", "export_fixture", "native_exact", "native_preliminary",
                             "native_project_stage", "normal_entry", "compare_first_turn"}
# Digest of the coordinator-reviewed, path-free auxiliary source map.
TRUSTED_SUPPORT_SHA256 = "d525a58030db89bc6ff4a9f06c6d72bda605b701d9ce846263a847dd87c6d768"
# Sanitized hashes of the existing audited HDD adapters, not event-supplied code.
TRUSTED_DRIVERS = {
    "audit_6b": "31922563c99cce3ee736abd04ca6dc80702c8ff5969a497e836118e0d815cc1d",
    "audit_6c": "fe8c64ec52788e6f13691de56ce84c6058fc1595b17ac4e2126b79f351cc3216",
    "compare_first_turn": "af7c099dbf6fa6e8f8444445d2bc446a9d56124404f37f2bc7188ed6fb3d25a1",
    "driver_6a": "05b1ab778721269302e40f4c867c43f1140688cd06b907f07a4d3c3e2872521e",
    "driver_6b": "fd22f61c7367c9b41429673f7c3e017b00d32176e36aea3c3c7c8616bc222f53",
    "driver_6c": "e0619e0828f1f7a095884d27eaac821913e906fae1edf0895270111abf396ba8",
    "export_fixture": "585b6b26cca438412d294fbed62065b4f99f5baf4f148a8cfe67651c61ad2622",
    "facts_6b": "49517cb47d88360adfe21dde521832f2481dbc7c0602b491214723060e54e4aa",
    "legacy_6a": "266bd42604ada2e4a3cf3d05d7a60852a8920d49b9629490ffa11bfcb1b211d0",
    "native_exact": "221993b0d52e6f6283c31f3017c10c50d5dc1f9997c9edbd37c751aaa3c29353",
    "native_preliminary": "13793398e9878bb82ba8529592c868e5f1abad9659ffda850b5deb6733dc9f55",
    "native_project_stage": "6bb9d71417ad056f9177c8c78323db4b1a8fb8ebaf89a764409ce00d5b49b736",
    "normal_entry": "eb60da67e37db63f86157a48fc3404cbf6dadced1c6aae42982db67ed3038944",
    "observer_6b": "79fb79b287fde16e8937d2961292623f926ee8560fe9d16ff172fd5d6b20bb45",
    "prepare_6b": "ffc01bbfba60771aed019691ecf79f1fe990fde10ff29483f7eaf3a75e773861",
    "runner_6b": "5fafeddcb50610ac06197fe0c187426772ccbf0012b1b83538c821430dfc5ea6",
    "seed_6b": "6b1a5f7fd374e0cbf472ccfa389a4bfb68921705cbe039f30ef83efa690a17ee",
    "seed_6c": "4cce11a855ada1d844f8706837407ab8d86e976835b97e95e8f66b358d4b8b5a",
    "verifier_6b": "e46ec19b4875ff050221d7fedaf3149e0947b1a3dfeddd229dbcef64af434d51",
}
LANE_CHECKS = {
    "6a": {"positive_turn_canary_absent_from_prompt", "positive_turn_canary_recovered_from_tool",
           "negative_turn_pretool_hard_denied", "negative_turn_synthetic_file_canary_absent",
           "roots_list_changed_202_empty", "bearer_http_mcp_2025_06_18_read_channel", "loopback_closed", "graceful_shutdown"},
    "6b": {"same_cancelled_turn_and_database", "accepted_and_locked_before_change", "lock_held_while_cancel_requested",
           "trusted_cancellation_terminal", "same_trial_database_and_final_b_receipt",
           "a1_accepted_submission_review_and_blob_preserved", "new_a2_authority_bound_to_a1_exact_accepted_outcome",
           "a2_new_formal_attempt_and_distinct_accepted_early_b_attempt", "constraint_ack_from_original_a2_owner",
           "exact_immutable_b_pin_and_its_aged_a1_evidence", "c_independently_completed_with_no_impacts",
           "out_of_scope_handback_to_f", "answer_sent_and_explicitly_acknowledged_by_original_a2_owner",
           "authentic_early_b_and_a2_agy_tool_transcripts", "final_clean_binary_and_healthy_shutdowns",
           "exact_f_answer_replay", "one_durable_f_answer_and_wake_after_restart"},
    "6c": {"pair_" + name for name in ("distinct_native_session", "distinct_member_id", "distinct_port", "distinct_database_inode",
                                      "same_related_initial_context")} | {
        str(noise) + "_" + name for noise in (0, 200) for name in (
            "exact_fresh_user_input", "prompt_bound_to_fresh_member", "current_thread_constraint",
            "initial_context_sources_match_fixture", "no_preexisting_summary", "zero_unrelated_prompt", "zero_unrelated_summary",
            "zero_unrelated_tool_returns", "zero_unrelated_vendor_system_input", "no_draft_or_sibling", "no_pin_in_initial_prompt",
            "exact_experience_origin", "looked_up_active_and_retired_experiences", "read_original_constraint", "read_old_plan_source",
            "read_retirement_source", "read_exact_accepted_artifact_after_lookup", "final_has_citations_and_artifact_content",
            "final_matches_vendor_transcript", "raw_and_public_terminal_receipts", "matching_native_session_and_protocol",
            "matching_observed_agy_model", "same_member_turn_and_delivery", "exact_single_normal_message",
            "source_messages_aged_three_months", "durable_exact_delivery_grant", "one_turn_one_delivery_no_new_task",
            "http_matches_persisted_final", "known_case_body", "draft_is_distinct", "service_exited_zero",
            "healthy_database_and_no_unsettled_claims")},
}
AWAITING = {"status": "awaiting-local-native-verification", "verified": False, "authoritative": False, "publishable": False}
VERDICT_JOB = "Release candidate verdict"
MATERIALS_JOB = "Release candidate evidence"
MAX_SIGNATURE = 8192
MAX_MATERIALS = 4 * core.MAX_RECEIPT


def intake_name(intent):
    return f"release-machine-{intent['run_id']}-{intent['run_attempt']}"


def intake_title(intent):
    return f"RC receipt {intent['run_id']} attempt {intent['run_attempt']}"


def verdict_name(intent):
    return f"release-evidence-{intent['sha']}-{intent['run_id']}-{intent['run_attempt']}"


def materials_name(intent):
    return f"release-materials-{intent['run_id']}-{intent['run_attempt']}"


def machine_signature(value):
    check(isinstance(value, str) and len(value) <= MAX_SIGNATURE and value.isascii(), "机器签名必须是有界 ASCII")
    base64.b64decode(value, validate=True)
    return value


def trusted_sources():
    controls = {name: core.file_hash(Path(__file__).with_name(name.replace("_", "-") + ".py"))
                for name in CONTROL_SOURCES}
    return TRUSTED_DRIVERS | controls


def trusted_bundle():
    return {"sources": trusted_sources(), "support_sha256": TRUSTED_SUPPORT_SHA256}


def keys(value, expected):
    check(isinstance(value, dict) and set(value) == set(expected), "回执字段不符或含未授权内容")


def validate_machine(value, require_full=False):
    keys(value, {"schema", "kind", "repo", "sha", "tree", "run_id", "run_attempt", "status", "verified", "workflow_sha",
                 "artifact_sha256", "target_artifact", "machine_id", "source_bundle_sha256", "lanes", "unknown_guard", "sources", "full_binding"})
    check(type(value["schema"]) is int, "机器回执 schema 非法")
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
    check(core.hashed(core.encoded(trusted_bundle())) == value["source_bundle_sha256"]
          and value["sources"] == trusted_sources(), "原始驱动清单、稳定受审适配器或控制源码摘要不符")
    keys(value["unknown_guard"], {"mode", "proof_sha256"})
    check(value["unknown_guard"]["mode"] in ("observed", "race"), "unknown 判据缺少实际观察或同提交 race 回归")
    core.digest(value["unknown_guard"]["proof_sha256"])
    binding = value["full_binding"]
    check(binding is not None or not require_full, "机器输入尚未在原始机器上绑定实际 Full；不是可接纳证据")
    if binding is not None:
        keys(binding, {"schema", "kind", "repo", "sha", "tree", "run_id", "run_attempt", "workflow_sha", "artifact_sha256",
                       "id", "digest", "receipt_sha256", "job_id", "producer"})
        check(type(binding["schema"]) is int, "Full 绑定 schema 非法")
        core.scope(binding, value["repo"], value["sha"], value["tree"], value["run_id"], value["run_attempt"])
        check(binding["kind"] == "release-full-binding" and binding["producer"] == "Release Full evidence"
              and binding["workflow_sha"] == value["workflow_sha"] and binding["artifact_sha256"] == value["artifact_sha256"], "Full 绑定来源或实际二进制不符")
        core.number(binding["id"])
        core.number(binding["job_id"])
        core.exact(binding["digest"], r"sha256:[0-9a-f]{64}", "Full ZIP 摘要非法")
        core.digest(binding["receipt_sha256"])
    lanes = value["lanes"]
    check(isinstance(lanes, list) and [lane.get("name") for lane in lanes] == ["6a", "6b", "6c"], "必须包含三个完整的原生验收场景")
    turns, sessions = set(), set()
    for lane in lanes:
        keys(lane, {"name", "status", "schema", "repo", "sha", "tree", "artifact_sha256", "run_id", "run_attempt",
                    "audit_sha256", "assertions_sha256", "assertion_count", "checks", "proofs", "attempt_ids", "full_binding_sha256"})
        check(type(lane["schema"]) is int, "场景 schema 非法")
        core.scope(lane, value["repo"], value["sha"], value["tree"], value["run_id"], value["run_attempt"])
        check(lane["artifact_sha256"] == value["artifact_sha256"]
              and lane["full_binding_sha256"] == (core.hashed(core.encoded(binding)) if binding is not None else None), "场景二进制或 Full 绑定不符")
        keys(lane["checks"], LANE_CHECKS[lane["name"]])
        check(all(item is True for item in lane["checks"].values())
              and lane["assertion_count"] == len(lane["checks"])
              and lane["assertions_sha256"] == core.hashed(core.encoded(lane["checks"])), "场景正面判据不完整或摘要不符")
        check(lane["status"] == "success", "原生场景未通过")
        core.number(lane["assertion_count"])
        core.digest(lane["audit_sha256"])
        core.digest(lane["assertions_sha256"])
        check(isinstance(lane["proofs"], list) and len(lane["proofs"]) == (6 if lane["name"] == "6b" else 2), "原生 Turn 证据数量不符")
        check(isinstance(lane["attempt_ids"], list) and (bool(lane["attempt_ids"]) == (lane["name"] == "6b")), "任务 attempt 证据缺失")
        for attempt_id in lane["attempt_ids"]:
            core.exact(attempt_id, r"[A-Za-z0-9_-]{1,128}", "任务 attempt ID 非法")
        check(len(lane["attempt_ids"]) == len(set(lane["attempt_ids"]))
              and (lane["name"] != "6b" or len(lane["attempt_ids"]) >= 6), "6B 缺少六个原生场景的独立持久 attempt")
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
    for value in (owner, actor_id, triggering_actor_id, event.get("sender", {}).get("id")):
        core.number(value)
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


def verify_binary_metadata(metadata, intent):
    check("\tpath\tagentworkspace/cmd/workspace\n" in metadata, "不是 workspace 二进制")
    settings = {}
    for line in metadata.splitlines():
        if line.startswith("\tbuild\t"):
            name, _, value = line[len("\tbuild\t"):].partition("=")
            settings[name] = value
    expected = {"vcs.revision": intent["sha"], "vcs.modified": "false", "vcs": "git", "GOOS": "darwin",
                "GOARCH": "amd64", "CGO_ENABLED": "0", "-trimpath": "true"}
    check(all(settings.get(key) == value for key, value in expected.items()) and "-overlay" not in settings, "二进制 Go 构建身份不符")


def binary_identity(go, binary, intent, env=None):
    metadata = subprocess.check_output([str(go), "version", "-m", str(binary)], text=True, timeout=30, env=env)
    verify_binary_metadata(metadata, intent)
    return {"path": str(binary), "package": "agentworkspace/cmd/workspace", "vcs_revision": intent["sha"],
            "vcs_modified": False, "sha256": core.file_hash(binary)}


def build_receipt(api, intent, candidate, binary, output):
    check(core.git(candidate, "rev-parse", "HEAD") == intent["sha"]
          and core.git(candidate, "rev-parse", "HEAD^{tree}") == intent["tree"]
          and not core.git(candidate, "status", "--porcelain", "--untracked-files=all"), "候选检出不干净或身份不符")
    binary_identity("go", binary, intent)
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
    if needs is not None:
        full_outputs(needs, intent)
    verify_reusable(api, intent)
    run_id, attempt = intent["run_id"], intent["run_attempt"]
    matrix = core.load_script("ci-plan").shard_matrix()
    names = [f"Release Full / Go {item['profile']} worker {item['worker']}" for item in matrix["include"]]
    names += ["Release Full / " + name for name in ("Go ordinary and remaining race", "Go workspace race", "Go vet and CI gates", "Go benchmarks", "Full suite evidence")]
    current_jobs = api.jobs(run_id, attempt)
    admission = control.admission_name(intent["sha"])
    if not any(job["name"] == admission for job in current_jobs):
        admission = "Release admission"
    names += ["Release clients", "Release candidate binary", "Release normalize", admission]
    jobs = core.require_jobs(current_jobs, run_id, attempt, names)
    for job in jobs:
        if job["name"] in names[:len(matrix["include"])] or job["name"] in ("Release Full / Go vet and CI gates", "Release Full / Full suite evidence"):
            for name in ("Verify immutable policy input", "Checkout immutable CI policy", "Stage verified CI policy"):
                steps = [step for step in job.get("steps", []) if step["name"] == name]
                check(len(steps) == 1 and steps[0].get("status") == "completed" and steps[0].get("conclusion") == "success",
                      "本次 Full 没有实际执行受信任的不可变 CI 策略步骤：" + name)
                check(core.timestamp(job.get("started_at")) <= core.timestamp(steps[0].get("started_at"))
                      <= core.timestamp(steps[0].get("completed_at")) <= core.timestamp(job.get("completed_at")),
                      "不可变策略步骤不在本次原生作业窗口内")
    build, _, target_fact = target(api, intent)  # Independent download and byte hash, not a needs string.
    shards = core.load_script("ci-shards")
    artifacts = []
    with tempfile.TemporaryDirectory(prefix="release-full-") as temporary:
        directory = Path(temporary)
        for item in matrix["include"]:
            profile, worker = item["profile"], item["worker"]
            producer = next(job for job in jobs if job["name"] == f"Release Full / Go {profile} worker {worker}")
            name = f"{profile}-worker-v1-{worker}"
            artifact = api.artifact(run_id, name, producer)
            _, _, summary, prefix = shards.verifier.PROFILES[profile]
            expected = {f"{prefix}{slot}/{summary}" for slot in shards.BATCHES[profile][worker]}
            files = api.download(artifact, expected, 16 << 20)
            check(set(files) == expected, "Full 原始 shard 文件形状不符")
            for filename, data in files.items():
                path = directory / name / filename
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_bytes(data)
            artifacts.append(artifact_fact(artifact) | {"job_id": producer["id"], "run_attempt": attempt,
                                                       "receipt_sha256s": [core.hashed(files[name]) for name in sorted(files)]})
        shards.aggregate_workers(directory, str(run_id), intent["sha"], intent["tree"])
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
    # Resolve the native commit to bounded policy subtrees, never local future refs.
    master = core.sha(api.get(api.prefix + "/commits/master")["sha"])
    control.reachable(api, core.sha(intake_sha), master)
    commit = control.commit_identity(api, intake_sha)
    check(0 < len(intent["sources"]) <= 128, "intake 控制源码清单超限")
    check(control.source_blobs(api, commit) == intent["sources"], "intake 控制源码与候选认领不符")


def intake(api, event, value, intake_run, intake_attempt, root):
    native_intake = api.run(intake_run)
    intake_actor(event, os.environ["GITHUB_REF"], native_intake.get("actor", {}).get("id"),
                 native_intake.get("triggering_actor", {}).get("id"), intake_attempt)
    check(native_intake.get("id") == intake_run and native_intake.get("path") == core.INTAKE
          and native_intake.get("event") == "workflow_dispatch" and native_intake.get("head_branch") == "master"
          and native_intake.get("run_attempt") == intake_attempt and native_intake.get("status") == "in_progress"
          and native_intake.get("conclusion") is None, "回执入口不是本次原生 master dispatch")
    signature = machine_signature(event.get("inputs", {}).get("machine_signature", ""))
    validate_machine(value, require_full=True)
    check(native_intake.get("display_title") == intake_title(value), "intake 原生标题与回执目标不符")
    intent = control.guard(api, value["run_id"], value["run_attempt"])
    build, _, target_fact = target(api, intent)
    bind_machine(value, intent | {"artifact_sha256": build["artifact_sha256"]}, target_fact)
    intake_sha = core.sha(native_intake.get("head_sha"))
    check(os.environ["GITHUB_WORKFLOW_SHA"] == intake_sha, "intake 实际源码 SHA 与原生 run 不符")
    intake_source(api, intent, intake_sha)
    check(value["full_binding"] == full_binding(api, intent), "本机提交未绑定实际同 attempt Full")
    return {key: value[key] for key in ("repo", "sha", "tree", "run_id", "run_attempt")} | {
        "schema": 1, "kind": "release-machine-intake", "machine": value, "machine_signature": signature, **AWAITING,
        "intake": {"run_id": intake_run, "run_attempt": intake_attempt, "actor_id": event["sender"]["id"], "workflow_sha": intake_sha}}


def verify_intake(api, native, wrapper, intent, target_fact, root):
    owner_id = api.get(api.prefix)["owner"]["id"]
    for value in (owner_id, native.get("actor", {}).get("id"), native.get("triggering_actor", {}).get("id")):
        core.number(value)
    check(native.get("path") == core.INTAKE and native.get("event") == "workflow_dispatch" and native.get("head_branch") == "master"
          and native.get("status") == "completed" and native.get("conclusion") == "success" and native.get("run_attempt") == 1
          and native.get("actor", {}).get("id") == native.get("triggering_actor", {}).get("id") == owner_id,
          "机器证据不是 owner 从 master 发出的受信任 intake")
    check(native.get("display_title") == intake_title(intent), "intake 原生标题与目标 attempt 不符")
    run_id, attempt = intent["run_id"], intent["run_attempt"]
    start = core.timestamp(api.run(run_id, attempt).get("run_started_at"))
    created = core.timestamp(native.get("created_at"))
    producer = core.require_jobs(api.jobs(run_id, attempt), run_id, attempt, ["Release Full evidence"])[0]
    check(start <= created <= start + timedelta(hours=12) and core.timestamp(producer.get("completed_at")) <= created,
          "intake 不在本次 attempt 窗口内，或早于实际 Full 终态")
    keys(wrapper, {"schema", "kind", "repo", "sha", "tree", "run_id", "run_attempt", "machine", "intake", "machine_signature", *AWAITING})
    machine_signature(wrapper["machine_signature"])
    core.scope(wrapper, api.repo, intent["sha"], intent["tree"], intent["run_id"], intent["run_attempt"])
    check(wrapper.get("kind") == "release-machine-intake" and wrapper.get("intake") == {
        "run_id": native["id"], "run_attempt": 1, "actor_id": owner_id, "workflow_sha": native["head_sha"]}, "intake 原生身份不符")
    check(all(wrapper.get(key) == value for key, value in AWAITING.items()), "intake 不能冒充最终证据")
    intake_source(api, intent, native["head_sha"])
    validate_machine(wrapper["machine"], require_full=True)
    bind_machine(wrapper["machine"], intent, target_fact)


def timestamp(value):
    check(isinstance(value, str), "缺少实际 attempt 开始时间")
    parsed = datetime.fromisoformat(value.replace("Z", "+00:00"))
    check(parsed.tzinfo is not None, "原生时间缺少时区")
    return parsed


def find_intakes(api, intent, root, target_fact=None):
    attempt = api.run(intent["run_id"], intent["run_attempt"])
    start = timestamp(attempt.get("run_started_at"))
    end = start + timedelta(hours=12)
    owner = api.get(api.prefix)["owner"]
    core.number(owner["id"])
    query = urlencode({"event": "workflow_dispatch", "branch": "master", "actor": owner["login"],
                       "created": start.isoformat() + ".." + end.isoformat(), "per_page": 100})
    page = api.get(f"{api.prefix}/actions/workflows/release-receipt.yml/runs?" + query)
    rows = page["workflow_runs"]
    check(isinstance(rows, list) and len(rows) < 100 and page.get("total_count", len(rows)) < 100,
          "intake 检索窗口饱和；拒绝不完整对账")
    candidates = []
    for run in rows:
        # Intentional run-name first; never open every historical artifact/payload.
        if run.get("display_title") != intake_title(intent):
            continue
        if not (start <= timestamp(run.get("created_at")) <= end
                and run.get("head_branch") == "master" and run.get("event") == "workflow_dispatch"
                and run.get("actor", {}).get("id") == run.get("triggering_actor", {}).get("id") == owner["id"]):
            continue
        if run.get("status") != "completed" or run.get("conclusion") != "success" or run.get("run_attempt") != 1:
            continue
        if target_fact is None:
            build, _, target_fact = target(api, intent)
            intent = intent | {"artifact_sha256": build["artifact_sha256"]}
        native = api.run(run["id"], 1)
        producer = core.require_jobs(api.jobs(run["id"], 1), run["id"], 1, ["Release receipt intake"])[0]
        wrapper, artifact, receipt_hash = api.receipt(run["id"], intake_name(intent) + f"-{run['id']}", producer)
        verify_intake(api, native, wrapper, intent, target_fact, root)
        candidates.append((wrapper, artifact, receipt_hash))
    return candidates


def collect(api, intent, root, seconds):
    build, _, target_fact = target(api, intent)
    intent = intent | {"artifact_sha256": build["artifact_sha256"]}
    binding = full_binding(api, intent)
    expected = {key: intent[key] for key in intent if key != "artifact_sha256"}
    check(control.guard(api, intent["run_id"], intent["run_attempt"]) == expected, "collector 初始 owner 身份改变")
    deadline = time.monotonic() + seconds
    while True:
        control.require_run_state(api, api.run(intent["run_id"]), intent["run_attempt"])
        candidates = find_intakes(api, intent, root, target_fact)
        if candidates:
            check(control.guard(api, intent["run_id"], intent["run_attempt"]) == expected, "收到回执时 canonical owner 改变")
            hashes = {core.hashed(core.encoded(item[0]["machine"])) for item in candidates}
            check(len(hashes) == 1 and all(item[0]["machine"]["full_binding"] == binding for item in candidates), "收到冲突或未绑定实际 Full 的机器回执")
            wrapper, artifact, receipt_hash = min(candidates, key=lambda item: item[1]["id"])
            return {key: intent[key] for key in ("repo", "sha", "tree", "run_id", "run_attempt")} | {
                "schema": 1, "kind": "release-machine-collected", "intake_run_id": wrapper["intake"]["run_id"], **AWAITING,
                "intake_artifact": artifact_fact(artifact, receipt_hash), "machine": wrapper["machine"],
                "machine_signature": wrapper["machine_signature"]}
        if time.monotonic() >= deadline:
            control.guard(api, intent["run_id"], intent["run_attempt"])
            raise ValueError("在限定时间内没有收到可核实的本地机器验收；禁止放行")
        time.sleep(min(45, max(0, deadline - time.monotonic())))


def full_binding(api, intent):
    run_id, attempt = intent["run_id"], intent["run_attempt"]
    producer = core.require_jobs(api.jobs(run_id, attempt), run_id, attempt, ["Release Full evidence"])[0]
    full, artifact, receipt_hash = api.receipt(run_id, f"release-full-{run_id}-{attempt}", producer)
    core.scope(full, api.repo, intent["sha"], intent["tree"], run_id, attempt)
    check(full == full_evidence(api, intent, None), "Full 回执与原生作业、原始 shard 或构建字节不符")
    return {key: intent[key] for key in ("repo", "sha", "tree", "run_id", "run_attempt", "workflow_sha")} | {
        "schema": 1, "kind": "release-full-binding", "artifact_sha256": full["artifact_sha256"],
        **artifact_fact(artifact, receipt_hash), "job_id": producer["id"], "producer": producer["name"]}


def materials(api, intent, needs, root):
    check(all(needs.get(name, {}).get("result") == "success" for name in ("full", "clients", "binary", "full-evidence", "machine")), "Full 或机器证据未全部成功；禁止产生暂存材料")
    return joined_evidence(api, intent, root)


# Compatibility alias: this can only produce provisional staging materials.
verdict = materials


def joined_evidence(api, intent, root):
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
    binding = full_binding(api, intent)
    machine, machine_artifact, machine_hash = api.receipt(run_id, f"release-machine-collected-{run_id}-{attempt}", jobs[1])
    core.scope(machine, api.repo, intent["sha"], intent["tree"], run_id, attempt)
    check(machine.get("kind") == "release-machine-collected" and all(machine.get(key) == value for key, value in AWAITING.items()), "collector 不能冒充最终证据")
    validate_machine(machine["machine"], require_full=True)
    bind_machine(machine["machine"], intent | {"artifact_sha256": build["artifact_sha256"]}, target_fact)
    check(machine["machine"]["full_binding"] == binding, "机器未在同次原生 Full 成功后复算并提交")
    native = api.run(machine["intake_run_id"], 1)
    producer = core.require_jobs(api.jobs(native["id"], 1), native["id"], 1, ["Release receipt intake"])[0]
    wrapper, intake_artifact, intake_hash = api.receipt(native["id"], f"release-machine-{run_id}-{attempt}-{native['id']}", producer)
    verify_intake(api, native, wrapper, intent | {"artifact_sha256": build["artifact_sha256"]}, target_fact, root)
    check(machine["intake_artifact"] == artifact_fact(intake_artifact, intake_hash)
          and machine["machine"] == wrapper["machine"]
          and machine["machine_signature"] == wrapper["machine_signature"], "机器 collector 与原生 intake 证据不符")
    return {key: intent[key] for key in ("repo", "sha", "tree", "run_id", "run_attempt", "workflow_sha")} | {
        "schema": 1, "kind": "release-materials", "status": "collected", "verified": False, "authoritative": False, "publishable": False,
        "artifact_sha256": build["artifact_sha256"], "target": target_fact,
        "full": binding, "machine": artifact_fact(machine_artifact, machine_hash),
        "intake": artifact_fact(intake_artifact, intake_hash), "full_lanes": full["artifacts"], "machine_lanes": wrapper["machine"]["lanes"],
        "machine_receipt": wrapper["machine"], "machine_signature": wrapper["machine_signature"]}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("binary", "full", "collect", "intake", "materials", "verdict"))
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
    # Both names are staging only; cryptographic enforcement has a separate producer.
    core.save(args.output / "receipt.json", result)


if __name__ == "__main__":
    try:
        main()
    except (ValueError, KeyError, OSError, subprocess.SubprocessError) as error:
        print("候选证据失败：" + str(error), file=sys.stderr)
        sys.exit(1)
