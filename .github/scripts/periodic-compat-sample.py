"""Check actual periodic diagnostics on the exact release Full workflow and published policy."""
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest

output=Path(sys.argv[1]).resolve()
policy=Path("policy").resolve(); release=Path("release-policy").resolve()
def git(root,*args):
    return subprocess.check_output(["git","-C",str(root),*args],text=True).strip()
def sha256(data):
    return hashlib.sha256(data).hexdigest()

release_sha="5f9b9e691fb3efac3e5e0812ef73c469c88f067e"
assert git(policy,"rev-parse","HEAD")==os.environ["FROZEN_POLICY"]
assert git(release,"rev-parse","HEAD")==release_sha
assert "GH_TOKEN" not in os.environ and "GITHUB_TOKEN" not in os.environ
with tempfile.TemporaryDirectory(prefix="periodic-release-pair-")as folder:
    target=Path(folder)
    shutil.copytree(policy/".github",target/".github",ignore=shutil.ignore_patterns("__pycache__"))
    relative=".github/workflows/ci-full.yml";shutil.copy2(release/relative,target/relative)
    assert git(target,"hash-object",str(target/relative))==git(release,"rev-parse",release_sha+":"+relative)
    source=target/".github/scripts/test-periodic-full.py"
    spec=importlib.util.spec_from_file_location("periodic_cases",source)
    module=importlib.util.module_from_spec(spec);spec.loader.exec_module(module)
    observed=[]; collect=module.POLICY.collect_failures
    def record(evidence,run,result):
        collect(evidence,run,result)
        selected=[]
        for row in result.get("failure_candidates",[]):
            selected.append({"test":row.get("test"),"kind":row.get("replay_kind"),"package":row.get("package"),
                             "source":row.get("recipe",{}).get("path"),"execution":row.get("execution",{}).get("mode")})
        observed.append({"status":result["diagnostic_status"],"selected":selected,
                         "unresolved":[r.get("package") for r in result.get("unresolved_failures",[])]})
    module.POLICY.collect_failures=record
    methods=["test_current_vet_shared_python_and_shard_compile_reach_diagnostic_module",
             "test_mixed_jobs_keep_completed_candidates_and_order_real_leaves_by_completion",
             "test_mixed_deterministic_replay_authenticates_own_pr_and_retains_timeout"]
    suite=unittest.defaultTestLoader.loadTestsFromNames(["ReviewedEvidenceTests."+m for m in methods],module)
    result=unittest.TextTestRunner(verbosity=2).run(suite)
    kinds={r["kind"]for case in observed for r in case["selected"] if r["kind"]}
    mixed=[case for case in observed if any(r["test"]=="TestTaskWorkInputEffectKeys"for r in case["selected"])]
    assert result.testsRun==len(methods)and result.wasSuccessful()and not result.skipped
    assert {"go-vet","go-compile","python-unittest"}<=kinds and mixed
    assert any(case["status"]=="partial" and "agentworkspace/cmd/workspace" in case["unresolved"]for case in mixed)
    recordout={"policy":git(policy,"rev-parse","HEAD"),"release":release_sha,"product":os.environ["FIXED_PRODUCT"],
               "caller_blob":git(policy,"rev-parse","HEAD:.github/workflows/periodic-full.yml"),
               "reusable_blob":git(release,"rev-parse","HEAD:"+relative),
               "policy_file_sha256":sha256((target/".github/scripts/periodic-full.py").read_bytes()),
               "methods":methods,"observed":observed,"tests":result.testsRun,"passed":True}
    (output/"conditional-policy.json").write_text(json.dumps(recordout,indent=2,sort_keys=True)+"\n")
    print(json.dumps({k:recordout[k] for k in ("policy","release","reusable_blob","tests","passed")},sort_keys=True))
