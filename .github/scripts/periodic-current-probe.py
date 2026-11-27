"""Reproduce the controlled periodic failure on immutable project history and verify the producing PR."""
import argparse
import copy
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys

GOOD = "9a45793e82ba00b7015ba565f9f90aea2c68694a"
BAD = "18d11a3c0176fe98856aeeef6396e9ddfde0cca0"
PREVIOUS = "7d70dc49ff1f3c3ae3876b2a519059c60d88ec3d"
REPO = "PeterFile/ct0x"
FIXTURE_HASH = "7a045d0d48fd0d74cb0d315c3eb96676861b109602c53d4aa174aac0bf0d208f"
TEST = "TestPeriodicFaultInjection"
PACKAGE = 'agentworkspace/internal/artifactfs'

def ensure(condition, message):
    if not condition:
        raise ValueError(message)

def write(path, value):
    path.write_text(json.dumps(value, indent=2, sort_keys=True) + "\n")

def read(path):
    return json.loads(path.read_bytes())

def module(path):
    spec = importlib.util.spec_from_file_location(path.stem.replace("-", "_"), path)
    result = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(result)
    return result

def sha256(data):
    return hashlib.sha256(data).hexdigest()

def git(root, *args):
    return subprocess.check_output(["git", "-C", str(root), *args], text=True).strip()

def source_hashes(root):
    return {p.name: sha256(p.read_bytes()) for p in sorted((root / ".github/scripts").glob("periodic-*.py"))}

def authorize(args):
    policy = module(args.policy / ".github/scripts/periodic-full.py")
    assert os.environ["GITHUB_REPOSITORY"] == REPO
    assert git(args.policy,"rev-parse","HEAD") == os.environ["FROZEN_POLICY"]
    assert git(args.subject,"rev-parse","HEAD") == os.environ["FIXED_PRODUCT"] == BAD
    api = policy.API(args.output/"api-baseline")
    def ancestor(sha):
        return subprocess.run(["git","-C",str(args.policy),"merge-base","--is-ancestor",sha,BAD]).returncode==0
    ev = policy.Evidence(api, REPO, ancestor)
    proof = ev.verify(45801293592, 1)
    ensure(proof["sha"] == GOOD and proof["verified"] and proof["tree"] == git(args.policy,"rev-parse",GOOD+"^{tree}"),"original Full does not prove input")
    ensure(git(args.policy,"rev-parse",BAD+"^1")==PREVIOUS,"product first parent changed")
    write(args.output/"baseline.json", proof)
    write(args.output/"source.json", {"policy_sha":git(args.policy,"rev-parse","HEAD"), "policy_tree":git(args.policy,"rev-parse","HEAD^{tree}"),
                                     "policy_files":source_hashes(args.policy),"product_sha":BAD,"product_tree":git(args.subject,"rev-parse","HEAD^{tree}"),
                                     "control_sha":os.environ["GITHUB_SHA"],"good_sha":GOOD,"fixture_hash":FIXTURE_HASH})

def fail_and_localize(args):
    replay = module(args.policy / ".github/scripts/periodic-replay.py")
    policy = module(args.policy / ".github/scripts/periodic-full.py")
    setup = read(args.output/"source.json")
    assert source_hashes(args.policy) == setup["policy_files"]
    assert git(args.subject,"rev-parse","HEAD^{tree}") == setup["product_tree"]
    ensure("GH_TOKEN" not in os.environ and "GITHUB_TOKEN" not in os.environ, "credential present during replay")
    fixture = Path(__file__).with_name("periodic-fixture.go.txt").resolve()
    assert sha256(fixture.read_bytes()) == FIXTURE_HASH
    real_go = str(Path(shutil.which("go")).resolve())
    private = args.output/"fixture-home"
    private.mkdir()
    wrapped = args.output/"tools"/"go"
    wrapped.parent.mkdir()
    wrapped.write_text("#!/usr/bin/env python3\nimport json, os, sys, tempfile\nfrom pathlib import Path\n"
                       + "with tempfile.NamedTemporaryFile(mode='w', suffix='.json', delete=False) as f:\n"
                       + "    json.dump({'Replace':{str(Path.cwd()/'internal/artifactfs/benchmark_test.go'):"
                       + repr(str(fixture)) + "}}, f)\n"
                       + "if sys.argv[1] != 'test': raise RuntimeError('unexpected control command')\n"
                       + "os.execv("+repr(real_go)+", ['go','test','-overlay='+f.name,*sys.argv[2:]])\n")
    wrapped.chmod(0o755)
    os.environ["PATH"] = str(wrapped.parent) + os.pathsep + os.environ["PATH"]
    argv = ["go","test","-count=1","-json","-timeout=10m","-run","^"+TEST+"$","./internal/artifactfs"]
    with (args.output/"native-failure.jsonl").open("wb") as log:
        status = subprocess.run(argv, cwd=args.subject, env=replay.child_env(private),stdout=log,stderr=subprocess.STDOUT,timeout=600)
    ensure(status.returncode==1,"fault did not fail with native assertion")
    raw = (args.output/"native-failure.jsonl").read_text()
    found = replay.failures(raw)
    candidate, = [row for row in found if row["test"] == TEST and row["package"] == PACKAGE]
    ensure(candidate["signature"]["messages"] == ["periodic controlled assertion: separate workspace race job is present"],"unexpected signature")
    candidate.update(race=False, execution={"mode":"ordinary","timeout_arg":"10m","timeout_seconds":600,
                      "job_timeout_seconds":1200,"setup_seconds":300,"proof":{"source":"control-only",
                      "argv":argv,"native_failure_sha256":sha256(raw.encode())}})
    bundle = args.output/"initial"
    bundle.mkdir()
    initial = {"kind":"controlled-periodic-input", "sha":BAD,"tree":setup["product_tree"],"ancestor":read(args.output/"baseline.json"),
               "failure_candidates":[candidate]}
    write(bundle/"receipt.json", initial)
    dest = args.output/"localization"
    dest.mkdir()
    replay.replay(bundle,args.policy,dest)
    record=read(dest/"localization.json")
    ensure(record["initial_sha256"]==sha256((bundle/"receipt.json").read_bytes()),"initial evidence changed")
    case,=record["items"]
    verified=policy.verify_localization(initial,case,dest/case["directory"],args.policy)
    ensure(verified.get("localization")=="reproduced" and verified.get("good_sha")==PREVIOUS and verified.get("commit")==BAD,"adjacent failure did not reconstruct")
    altered=copy.deepcopy(case)
    altered["commit"]=GOOD
    try:
        policy.verify_localization(initial,altered,dest/case["directory"],args.policy)
    except ValueError:
        pass
    else:
        raise ValueError("mismatched boundary was accepted")
    write(args.output/"verified.json",verified)
    write(args.output/"probe.json",{"native_failure_sha256":sha256(raw.encode()),"wrapper_sha256":sha256(wrapped.read_bytes()),
                                 "checkouts":len(verified["probes"]),"executions":sum(len(g["results"])for g in verified["probes"]),
                                 "altered_commit_rejected":True})
    shutil.rmtree(private)
    print("Verified controlled failure and adjacent first-parent boundary",PREVIOUS,BAD)

def associate(args):
    policy=module(args.policy/".github/scripts/periodic-full.py")
    verified=read(args.output/"verified.json")
    api=policy.API(args.output/"api-pr")
    evidence=policy.Evidence(api, REPO, lambda sha: subprocess.run(["git","-C",str(args.policy),"merge-base","--is-ancestor",sha,BAD]).returncode==0)
    value=policy.attribute(verified,policy.merged_pulls(api,evidence,verified["commit"]),REPO)
    ensure(value.get("status")=="confirmed" and value.get("pr")==265,"exact target merged PR did not match")
    summary={k:value[k] for k in ("status","pr","commit","good_sha","bad_sha","transition")}
    output={"type":"controlled-periodic-current-policy",**read(args.output/"source.json"),**read(args.output/"probe.json"),
            "run_id":int(os.environ["GITHUB_RUN_ID"]), "run_attempt":int(os.environ["GITHUB_RUN_ATTEMPT"]),
            "attribution":summary,"baseline_url":read(args.output/"baseline.json")["url"]}
    write(args.output/"receipt.json",output)
    print(json.dumps(output,sort_keys=True))

parser=argparse.ArgumentParser()
parser.add_argument("mode",choices=["authorize","replay","associate"])
parser.add_argument("output",type=Path)
parser.add_argument("--policy",type=Path,default=Path("policy"))
parser.add_argument("--subject",type=Path,default=Path("subject"))
args=parser.parse_args()
args.output=args.output.resolve()
args.policy=args.policy.resolve()
args.subject=args.subject.resolve()
args.output.mkdir(parents=True,exist_ok=True)
{"authorize":authorize,"replay":fail_and_localize,"associate":associate}[args.mode](args)
