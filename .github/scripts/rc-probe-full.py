#!/usr/bin/env python3
"""Read-only test dispatch for one frozen product and the unmodified reusable Full."""
import argparse
import importlib.util
import json
import os
from pathlib import Path
import re
import sys

ROOT = Path(__file__).resolve().parents[2]
REPO = "PeterFile/ct0x"
BRANCH = "codex/ci-release-probe-20261126"
PRODUCT = "18d11a3c0176fe98856aeeef6396e9ddfde0cca0"
PRODUCT_TREE = "6df8bf3ccb986f8f4088dc310e0257b3f6285d9a"
PRODUCT_BASE = "7d70dc49ff1f3c3ae3876b2a519059c60d88ec3d"
PREFIX = "Probe Full / "


def load(name):
    spec = importlib.util.spec_from_file_location(name.replace("-", "_"), ROOT / ".github/scripts" / (name + ".py"))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


core, control, planner = load("release-core"), load("release-control"), load("ci-plan")
check = core.check


class ReadOnly(core.GitHub):
    def request(self, method, path, data=None, binary=False):
        check(method == "GET" and data is None, "Probe permits only native read requests")
        return super().request(method, path, binary=binary)


def context(api):
    env = os.environ
    check(env["GITHUB_REPOSITORY"] == REPO and env["GITHUB_REF"] == "refs/heads/" + BRANCH
          and env["GITHUB_EVENT_NAME"] == "workflow_dispatch", "Not the approved test repository/ref/event")
    event = core.decode(Path(env["GITHUB_EVENT_PATH"]).read_bytes(), 8 << 20)
    check(event["repository"]["full_name"] == REPO and event["inputs"]["sha"] == PRODUCT, "Not the frozen product input")
    workflow_sha = core.sha(env["GITHUB_WORKFLOW_SHA"])
    check(core.git(ROOT, "rev-parse", "HEAD") == workflow_sha, "Checkout is not the native immutable workflow SHA")
    run_id, attempt = core.number(int(env["GITHUB_RUN_ID"])), core.number(int(env["GITHUB_RUN_ATTEMPT"]))
    run = api.run(run_id, attempt)
    workflow = api.get(api.prefix + "/actions/workflows/ci.yml")
    latest = api.run(run_id)
    check(latest["run_attempt"] == attempt and run["status"] == latest["status"] == "in_progress"
          and run["event"] == "workflow_dispatch" and run["head_branch"] == BRANCH
          and run["path"] == workflow["path"] == ".github/workflows/ci.yml" and run["workflow_id"] == workflow["id"]
          and run["head_sha"] == workflow_sha and run["head_commit"]["id"] == workflow_sha,
          "Native workflow source or attempt does not match the approved test")
    commit = control.commit_identity(api, workflow_sha)
    check(commit["tree"]["sha"] == run["head_commit"]["tree_id"]
          == core.git(ROOT, "rev-parse", workflow_sha + "^{tree}"), "Native controller tree mismatch")
    policy = core.sha(core.git(ROOT, "show", workflow_sha + ":.github/rc-probe-policy.txt"))
    previous = core.sha(core.git(ROOT, "show", workflow_sha + ":.github/rc-probe-previous.txt"))
    check(previous != policy, "Controller parent pins must be distinct")
    check([row["sha"] for row in commit["parents"]] == [previous, policy]
          == core.git(ROOT, "rev-parse", workflow_sha + "^@").split(),
          "Controller parents must be the previous experiment head then the reviewed policy")
    changed = core.git(ROOT, "diff-tree", "--no-commit-id", "--name-only", "--no-renames", "-r", "-z", policy, workflow_sha, "--")
    paths = changed.split("\0")
    check(paths[-1] == "" and all(re.fullmatch(r"\.github/(?:workflows/ci\.yml|rc-probe-[a-z0-9-]+\.txt|scripts/rc-probe-[a-z0-9-]+\.py)", path)
                                        for path in paths[:-1]), "Controller changes exceed test workflow/probe overlays")
    parent = control.commit_identity(api, policy)
    check(parent["tree"]["sha"] == core.git(ROOT, "rev-parse", policy + "^{tree}"), "Reviewed policy was not fetched exactly")
    current, reviewed = control.source_blobs(api, commit), control.source_blobs(api, parent)
    sources = {}
    for path in (*control.FULL_PATHS, ".github/scripts/release-core.py", ".github/scripts/release-control.py"):
        blob = core.sha(reviewed[path])
        check(current[path] == blob == core.git(ROOT, "rev-parse", policy + ":" + path)
              == core.git(ROOT, "rev-parse", workflow_sha + ":" + path)
              == core.git(ROOT, "hash-object", "--no-filters", str(ROOT / path)), "Original policy blob changed: " + path)
        sources[path] = blob
    return {"repo": REPO, "run_id": run_id, "run_attempt": attempt, "workflow_sha": workflow_sha,
            "policy_source_sha": policy, "sources": sources}


def plan(api):
    identity = context(api)
    # This tests the real resolver. The native dispatch remains on the test ref.
    simulated = {"repository": {"full_name": REPO}, "inputs": {"sha": PRODUCT}}
    target = control.resolve_target(api, REPO, "workflow_dispatch", simulated, "refs/heads/master")
    check(all(target[key] == value for key, value in {"sha": PRODUCT, "tree": PRODUCT_TREE, "head": PRODUCT, "base": PRODUCT_BASE}.items()),
          "Native frozen product SHA/tree/parent mismatch")
    check(core.git(ROOT, "rev-parse", PRODUCT + "^{tree}") == PRODUCT_TREE
          and core.git(ROOT, "rev-parse", PRODUCT + "^") == PRODUCT_BASE, "Local frozen product objects mismatch")
    matrix = planner.shard_matrix()
    shards = load("ci-shards")
    slots = sum(len(batch) for batches in shards.BATCHES.values() for batch in batches)
    check(slots == 81, "The original Full must cover all 81 logical slots")
    return {"schema": 1, "kind": "full-probe-plan", "test_only": True,
            "resolution": "test-master-dispatch-simulation", "actual_ref": "refs/heads/" + BRANCH,
            **identity, **target, "matrix": matrix, "logical_slots": slots, "workers": len(matrix["include"])}


def required_jobs(selected):
    return [PREFIX + f"Go {row['profile']} worker {row['worker']}" for row in selected["matrix"]["include"]] + [
        PREFIX + name for name in ("Go ordinary and remaining race", "Go workspace race", "Go vet and CI gates", "Go benchmarks", "Full suite evidence")]


def job_records(jobs):
    result = []
    for job in jobs:
        if not job["name"].startswith(PREFIX):
            continue
        row = {key: job.get(key) for key in ("id", "name", "run_id", "run_attempt", "status", "conclusion", "started_at", "completed_at", "html_url")}
        if row["started_at"] and row["completed_at"]:
            row["duration_seconds"] = (core.timestamp(row["completed_at"]) - core.timestamp(row["started_at"])).total_seconds()
        result.append(row)
    return result


def verify(api, needs, output):
    expected = plan(api)
    run_id, attempt = expected["run_id"], expected["run_attempt"]
    jobs = api.jobs(run_id, attempt)
    records = job_records(jobs)
    core.save(output / "jobs.json", records)
    core.save(output / "timings.json", {"basis": "native job started_at to completed_at; excludes queue before job start", "jobs": records})
    producer = core.require_jobs(jobs, run_id, attempt, ["Prepare Full probe"])[0]
    selected, artifact, receipt_hash = api.receipt(run_id, f"probe-full-plan-{run_id}-{attempt}", producer)
    check(selected.keys() == expected.keys() and all(selected[key] == expected[key] for key in expected if key != "master_sha"),
          "Native prepare plan does not match the frozen product/current policy")
    check(needs["full-prepare"]["result"] == needs["probe-full"]["result"] == "success", "Prepare or actual reusable Full failed")
    check(all(needs["full-prepare"]["outputs"][key] == expected[key] for key in ("sha", "tree", "head", "base"))
          and json.loads(needs["full-prepare"]["outputs"]["matrix"]) == expected["matrix"], "Native prepare outputs mismatch")
    outputs = needs["probe-full"]["outputs"]
    check(all(outputs.get(key) == value for key, value in {"verified": "true", "run_id": str(run_id),
          **{key: expected[key] for key in ("sha", "tree", "head", "base")}}.items()), "Actual Full output identity/verification mismatch")
    names = required_jobs(expected)
    check({row["name"] for row in records} == set(names), "Missing or additional native Full jobs")
    full_jobs = core.require_jobs(jobs, run_id, attempt, names)
    gate = full_jobs[-1]
    evidence = []
    workers = expected["matrix"]["include"]
    for row, job in zip(workers, full_jobs):
        item = api.artifact(run_id, f"{row['profile']}-worker-v1-{row['worker']}", job)
        evidence.append({key: item[key] for key in ("id", "name", "digest", "created_at", "updated_at")})
    benchmarks = api.artifact(run_id, "go-benchmarks", full_jobs[-2])
    evidence.append({key: benchmarks[key] for key in ("id", "name", "digest", "created_at", "updated_at")})
    core.save(output / "artifacts.json", evidence)
    # Recheck source and latest native attempt before emitting the success receipt.
    check(all(expected[key] == value for key, value in context(api).items()), "Native source/attempt changed during verification")
    gate_url = gate.get("html_url") or f"https://github.com/{REPO}/actions/runs/{run_id}/job/{gate['id']}"
    result = {"schema": 1, "kind": "full-probe-evidence", "status": "success", "test_only": True,
              "native_rc": False, "production_6abc": False, "release": False, "full_verified": True,
              "plan": selected, "full_outputs": outputs, "gate_job_id": gate["id"], "gate_job_url": gate_url,
              "run_url": f"https://github.com/{REPO}/actions/runs/{run_id}", "artifacts": evidence,
              "plan_artifact": {"id": artifact["id"], "digest": artifact["digest"], "receipt_sha256": receipt_hash}}
    core.save(output / "receipt.json", result)
    summary = os.environ.get("GITHUB_STEP_SUMMARY")
    if summary:
        with open(summary, "a", encoding="utf-8") as stream:
            stream.write(f"Test-only Full passed for `{PRODUCT}`. [Native Full gate]({gate_url}). Not a native RC, production 6A/B/C, or release.\n")



def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("prepare", "verify"))
    parser.add_argument("--output", required=True, type=Path)
    args = parser.parse_args()
    api = ReadOnly(REPO)
    try:
        if args.command == "prepare":
            selected = plan(api)
            core.save(args.output / "receipt.json", selected)
            core.outputs({key: selected[key] for key in ("sha", "tree", "head", "base", "matrix")})
        else:
            verify(api, core.decode(os.environ["NEEDS_JSON"].encode(), 1 << 20), args.output)
    except (ValueError, KeyError, TypeError, OSError) as error:
        core.save(args.output / "failure.json", {"schema": 1, "kind": "full-probe-failure", "test_only": True, "reason": str(error)})
        raise


if __name__ == "__main__":
    try:
        main()
    except (ValueError, KeyError, TypeError, OSError) as error:
        print("Full probe failed: " + str(error), file=sys.stderr)
        sys.exit(1)
