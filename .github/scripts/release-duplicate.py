#!/usr/bin/env python3
"""Read-only redirect to a canonical owner's existing authoritative release evidence."""
import importlib.util
import os
from pathlib import Path
import sys

_spec = importlib.util.spec_from_file_location("release_core", Path(__file__).with_name("release-core.py"))
core = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(core)
control, evidence = core.load_script("release-control"), core.load_script("release-evidence")
host = core.load_script("release-host-proof")
check = core.check


def reuse(api, owner, sha, tree):
    core.number(owner)
    core.sha(sha)
    core.sha(tree)
    run = api.run(owner)
    core.native_candidate(run)
    workflow = api.get(api.prefix + "/actions/workflows/release-candidate.yml")
    check(workflow.get("path") == core.WORKFLOW and workflow.get("id") == run.get("workflow_id"), "canonical 工作流身份不符")
    check(run.get("status") == "completed" and run.get("conclusion") == "success", "canonical owner 尚无成功终态")
    check((run["event"] == "workflow_dispatch" and run.get("head_branch") == "master") or
          (run["event"] == "push" and run.get("head_branch", "").startswith("rc/")
           and bool(run["head_branch"][3:]) and "/" not in run["head_branch"][3:]), "canonical 原生入口不符")
    attempt = core.number(run["run_attempt"])
    intent = control.guard(api, owner, attempt, completed=True)
    core.scope(intent, api.repo, sha, tree, owner, attempt)
    source = run.get("head_commit", {}).get("id")
    check(intent.get("workflow_sha") == source and (run["event"] != "workflow_dispatch" or run.get("head_sha") == source),
          "canonical 调用源码不符")
    jobs = api.jobs(owner, attempt)
    verdict, full_job, machine_job, materials_job = core.require_jobs(jobs, owner, attempt,
        ["Release candidate verdict", "Release Full evidence", "Release machine evidence", "Release candidate evidence"])
    artifact = api.artifact(owner, f"release-evidence-{sha}-{owner}-{attempt}", verdict)
    raw = api.download(artifact, {"evidence.json"}, host.MAX_MATERIALS + 4096)["evidence.json"]
    value = core.decode(raw, host.MAX_MATERIALS + 4096)
    core.scope(value, api.repo, sha, tree, owner, attempt)
    check(value.get("kind") == "release-evidence" and value.get("status") == "success" and value.get("verified") is True
          and value.get("authoritative") is True and value.get("publishable") is False
          and value.get("verification") == "openssh-machine-witness-v1"
          and value.get("workflow_sha") == intent["workflow_sha"], "canonical 证据不是已核验的权威结论")
    host.validate_evidence(value, Path(__file__).resolve().parents[2], intent["workflow_sha"])
    staged = api.artifact(owner, f"release-materials-{owner}-{attempt}", materials_job)
    check(all(value["materials"][key] == staged[key] for key in ("id", "digest")), "权威暂存制品不符")
    core.digest(value["artifact_sha256"])
    # Reuse only small receipts. Never download binaries, benchmarks or shard archives again.
    full, full_artifact, full_hash = api.receipt(owner, f"release-full-{owner}-{attempt}", full_job)
    core.scope(full, api.repo, sha, tree, owner, attempt)
    check(full.get("kind") == "release-full" and full.get("status") == "success" and full.get("verified") is True
          and full.get("workflow_sha") == intent["workflow_sha"] and full.get("artifact_sha256") == value["artifact_sha256"]
          and full.get("target_artifact") == value["target"] and full.get("artifacts") == value["full_lanes"], "Full 回执身份不符")
    evidence.verify_reusable(api, intent)
    recorded = core.require_jobs(jobs, owner, attempt, [row["name"] for row in full["jobs"]])
    check(bool(recorded) and {row["id"] for row in recorded} == {row["id"] for row in full["jobs"]}, "Full 原生作业不符")
    binding = {key: intent[key] for key in ("repo", "sha", "tree", "run_id", "run_attempt", "workflow_sha")} | {
        "schema": 1, "kind": "release-full-binding", "artifact_sha256": value["artifact_sha256"],
        **evidence.artifact_fact(full_artifact, full_hash), "job_id": full_job["id"], "producer": full_job["name"]}
    check(value["full"] == binding, "Full run、attempt 或不可变制品不符")
    collected, machine_artifact, machine_hash = api.receipt(owner, f"release-machine-collected-{owner}-{attempt}", machine_job)
    core.scope(collected, api.repo, sha, tree, owner, attempt)
    check(collected.get("kind") == "release-machine-collected"
          and value["machine"] == evidence.artifact_fact(machine_artifact, machine_hash)
          and value["intake"] == collected["intake_artifact"], "机器 collector 制品不符")
    machine = collected["machine"]
    evidence.validate_machine(machine, require_full=True)
    core.scope(machine, api.repo, sha, tree, owner, attempt)
    check(machine["workflow_sha"] == intent["workflow_sha"] and machine["artifact_sha256"] == value["artifact_sha256"]
          and machine["target_artifact"] == value["target"] and machine["full_binding"] == binding
          and machine["lanes"] == value["machine_lanes"] and machine == value["machine_receipt"]
          and collected.get("machine_signature") == value["machine_signature"], "原生场景、签名、来源或 Full 绑定不符")
    check(control.guard(api, owner, attempt, completed=True) == intent, "核验期间 canonical attempt 或来源已改变")
    return {"owner_run_id": owner, "owner_attempt": attempt,
            "evidence_url": f"https://github.com/{api.repo}/actions/runs/{owner}/artifacts/{artifact['id']}"}


def main():
    repo = core.repository(os.environ["GITHUB_REPOSITORY"])
    owner = core.number(int(os.environ["OWNER_RUN_ID"]))
    with open(os.environ["GITHUB_STEP_SUMMARY"], "a", encoding="utf-8") as stream:
        stream.write(f"Canonical owner: https://github.com/{repo}/actions/runs/{owner}\n\n")
        stream.write("重复触发仅引用原 owner；本次不执行候选验收，不产生新发布证据。\n")
    result = reuse(control.AdmissionAPI(repo), owner, os.environ["CANDIDATE_SHA"], os.environ["CANDIDATE_TREE"])
    core.outputs(result)
    with open(os.environ["GITHUB_STEP_SUMMARY"], "a", encoding="utf-8") as stream:
        stream.write(f"已核验的原始证据：{result['evidence_url']}\n")


if __name__ == "__main__":
    try:
        main()
    except (ValueError, KeyError, TypeError, OSError) as error:
        print("重复候选未放行；请查看原 owner：" + str(error), file=sys.stderr)
        sys.exit(1)
