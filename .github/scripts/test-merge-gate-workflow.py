#!/usr/bin/env python3
"""Parse the actual YAML and execute its plan/unit/final commands on a real temporary merge."""
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]


def yaml_file(path):
    # Ruby/Psych is available on the development host and the standard Ubuntu runner.
    result = subprocess.run(["ruby", "-rjson", "-ryaml", "-e", "puts JSON.generate(YAML.load_file(ARGV[0]))", str(path)],
                            capture_output=True, text=True, check=True)
    return json.loads(result.stdout)


class WorkflowTests(unittest.TestCase):
    def test_actual_yaml_permissions_intake_and_distinct_pr_concurrency(self):
        validation = yaml_file(ROOT / ".github/workflows/merge-gate.yml")
        publisher = yaml_file(ROOT / ".github/workflows/merge-conflict.yml")
        # YAML 1.1 interprets the plain key 'on' as true; actionlint separately validates Actions YAML.
        intake = validation.get("on", validation.get("true"))
        self.assertIn("workflow_dispatch", intake)
        self.assertIn("labeled", intake["pull_request"]["types"])
        self.assertNotIn("paths", intake["pull_request"])
        self.assertNotIn("branches", intake["pull_request"])
        self.assertEqual(validation["permissions"], {"contents": "read"})
        self.assertEqual(validation["jobs"]["tests"]["strategy"]["max-parallel"], 7)
        self.assertEqual(publisher["jobs"]["resolve"]["name"], "Resolve Merge PR identities")
        self.assertNotIn("checks", publisher["jobs"]["resolve"]["permissions"])
        report = publisher["jobs"]["report"]
        self.assertEqual(report["permissions"]["checks"], "write")
        self.assertEqual(report["permissions"]["actions"], "write")
        self.assertEqual(report["needs"], "resolve")
        group = report["concurrency"]["group"]
        resolved = [group.replace("${{ matrix.source_sha }}", char * 40).replace("${{ github.repository }}", "PeterFile/ct0x") for char in ("a", "b")]
        self.assertNotEqual(*resolved)
        self.assertEqual(group.replace("${{ matrix.pr }}", "7"), group.replace("${{ matrix.pr }}", "8"))
        self.assertFalse(report["concurrency"]["cancel-in-progress"])
        for job in publisher["jobs"].values():
            checkout = job["steps"][0]
            self.assertEqual(checkout["with"]["ref"], "${{ github.workflow_sha }}")
            self.assertIs(checkout["with"]["persist-credentials"], False)

    def test_early_identity_survives_real_go_discovery_failure_in_native_and_controller(self):
        workflow = yaml_file(ROOT / ".github/workflows/merge-gate.yml")
        steps = workflow["jobs"]["plan"]["steps"]
        identity_step = next((step for step in steps if step.get("id") == "identity"), None)
        self.assertIsNotNone(identity_step, "identity must be uploaded before fallible planning")
        identity_index = steps.index(identity_step)
        self.assertEqual(steps[identity_index + 1]["with"]["name"], "merge-identity-${{ github.run_id }}-${{ github.run_attempt }}")
        self.assertLess(identity_index + 1, next(i for i, step in enumerate(steps) if step.get("uses", "").startswith("actions/setup-go@")))
        with tempfile.TemporaryDirectory(prefix="merge missing Go ") as temporary:
            root = Path(temporary)
            def git(*args, input=None):
                return subprocess.run(["git", "-C", str(root), *args], input=input, text=True,
                                      capture_output=True, check=True).stdout.strip()
            git("init", "-q")
            git("config", "user.name", "Fixture")
            git("config", "user.email", "fixture@example.invalid")
            (root / "go.mod").write_text("module fixture\n\ngo 1.26.0\n")
            (root / "code.go").write_text('package fixture\nimport _ "fixture/missing"\n')
            git("add", "--all")
            git("commit", "-qm", "base")
            base = git("rev-parse", "HEAD")
            (root / "README.md").write_text("head\n")
            git("add", "--all")
            git("commit", "-qm", "head")
            head, tree = git("rev-parse", "HEAD"), git("rev-parse", "HEAD^{tree}")
            merge = git("commit-tree", tree, "-p", base, "-p", head, input="merge\n")
            git("reset", "--hard", merge)
            inputs = {"pr": "7", "head": head, "base": base, "sha": merge, "tree": tree, "policy_sha": base,
                      "source_repo_id": "17", "source_repo": "fork/ct0x", "source_ref": "topic", "base_ref": "master"}
            pr = {"number": 7, "head": {"sha": head, "ref": "topic", "repo": {"id": 17, "full_name": "fork/ct0x"}},
                  "base": {"sha": base, "ref": "master", "repo": {"full_name": "PeterFile/ct0x"}}}
            for event_name in ("pull_request", "workflow_dispatch"):
                with self.subTest(event=event_name):
                    scratch = root / event_name
                    scratch.mkdir()
                    event = scratch / "event.json"
                    event.write_text(json.dumps({"inputs": inputs} if event_name == "workflow_dispatch" else {"number": 7, "pull_request": pr}))
                    env = {**os.environ, "GITHUB_EVENT_NAME": event_name, "GITHUB_EVENT_PATH": str(event),
                           "GITHUB_REPOSITORY": "PeterFile/ct0x", "GITHUB_SHA": base if event_name == "workflow_dispatch" else merge,
                           "GITHUB_RUN_ID": "101", "GITHUB_RUN_ATTEMPT": "2", "MERGE_WORKFLOW_SHA": base,
                           "MERGE_POLICY_ROOT": str(ROOT), "RUNNER_TEMP": str(scratch), "GITHUB_OUTPUT": str(scratch / "outputs")}
                    def run(step):
                        return subprocess.run(["bash", "-euo", "pipefail", "-c", step["run"]], cwd=root, env=env,
                                              text=True, capture_output=True)
                    identified = run(identity_step)
                    self.assertEqual(identified.returncode, 0, identified.stderr)
                    identity_path = scratch / "merge-identity/merge-identity.json"
                    value = json.loads(identity_path.read_text())
                    self.assertEqual((value["base"], value["sha"], value["tree"]), (base, merge, tree))
                    failed = run(next(step for step in steps if step.get("id") == "plan"))
                    self.assertNotEqual(failed.returncode, 0)
                    self.assertIn("Merge stderr:", failed.stderr)
                    self.assertIn("fixture/missing", failed.stderr)
                    self.assertEqual(json.loads(identity_path.read_text()), value)
                    self.assertFalse((scratch / "merge-plan.json").exists())
                    identity_path.unlink()
                    env["GITHUB_SHA"] = head
                    if event_name == "workflow_dispatch":
                        event.write_text(json.dumps({"inputs": {**inputs, "sha": head}}))
                    self.assertNotEqual(run(identity_step).returncode, 0)
                    self.assertFalse(identity_path.exists())

    def test_actual_dispatch_steps_empty_docs_receipt_and_skipped_dependency_rejection(self):
        workflow = yaml_file(ROOT / ".github/workflows/merge-gate.yml")
        with tempfile.TemporaryDirectory(prefix="merge workflow ;$ ") as temporary:
            workspace = Path(temporary)
            candidate = workspace / "candidate"
            candidate.mkdir()
            scratch = workspace / "runner"
            scratch.mkdir()
            def git(*args, input=None):
                return subprocess.run(["git", "-C", str(candidate), *args], input=input, text=True,
                                      capture_output=True, check=True).stdout.strip()
            git("init", "-q")
            git("config", "user.name", "Fixture")
            git("config", "user.email", "fixture@example.invalid")
            (candidate / "go.mod").write_text("module fixture\n\ngo 1.26.0\n")
            (candidate / "code.go").write_text("package fixture\n")
            git("add", "--all")
            git("commit", "-qm", "base fixture")
            base = git("rev-parse", "HEAD")
            (candidate / "README.md").write_text("Document fixture\n")
            git("add", "--all")
            git("commit", "-qm", "head fixture")
            head = git("rev-parse", "HEAD")
            tree = git("rev-parse", "HEAD^{tree}")
            merge = git("commit-tree", tree, "-p", base, "-p", head, input="merge fixture\n")
            git("reset", "--hard", merge)
            event = workspace / "event.json"
            event.write_text(json.dumps({"inputs": {"pr": "7", "head": head, "base": base, "sha": merge, "tree": tree,
                                                    "policy_sha": base, "source_repo_id": "17", "source_repo": "fork/ct0x",
                                                    "source_ref": "branch ; $()", "base_ref": "stacked"}}))
            env = {**os.environ, "GITHUB_EVENT_NAME": "workflow_dispatch", "GITHUB_EVENT_PATH": str(event),
                   "GITHUB_REPOSITORY": "PeterFile/ct0x", "GITHUB_SHA": base, "MERGE_WORKFLOW_SHA": base,
                   "GITHUB_RUN_ID": "101", "GITHUB_RUN_ATTEMPT": "2", "GITHUB_OUTPUT": str(workspace / "outputs"),
                   "RUNNER_TEMP": str(scratch), "MERGE_POLICY_ROOT": str(ROOT), "UNIT_INDEX": "0",
                   "NEEDS_JSON": json.dumps({"plan": {"result": "success"}, "tests": {"result": "success"}})}
            def run_step(job, predicate):
                step = next(step for step in workflow["jobs"][job]["steps"] if predicate(step))
                return subprocess.run(["bash", "-euo", "pipefail", "-c", step["run"]], cwd=candidate, env=env,
                                      capture_output=True, text=True)
            planned = run_step("plan", lambda step: step.get("id") == "plan")
            self.assertEqual(planned.returncode, 0, planned.stderr)
            plan_dir = scratch / "merge-plan"
            plan_dir.mkdir()
            shutil.copyfile(scratch / "merge-plan.json", plan_dir / "merge-plan.json")
            plan = json.loads((plan_dir / "merge-plan.json").read_text())
            self.assertEqual(plan["tasks"], [{"unit": "identity-diff", "kind": "identity"}])
            unit = run_step("tests", lambda step: "UNIT_INDEX" in step.get("env", {}))
            self.assertEqual(unit.returncode, 0, unit.stderr)
            unit_dir = scratch / "merge-units/merge-unit-101-2-0"
            unit_dir.mkdir(parents=True)
            shutil.copyfile(scratch / "merge-unit/unit.json", unit_dir / "unit.json")
            final = run_step("evidence", lambda step: "NEEDS_JSON" in step.get("env", {}))
            self.assertEqual(final.returncode, 0, final.stderr)
            receipt = json.loads((scratch / "merge-receipt/merge-gate.json").read_text())
            self.assertEqual((receipt["sha"], receipt["tree"], receipt["base"], receipt["head"]), (merge, tree, base, head))
            self.assertEqual(receipt["source"]["base_ref"], "stacked")
            env["NEEDS_JSON"] = json.dumps({"plan": {"result": "success"}, "tests": {"result": "skipped"}})
            rejected = run_step("evidence", lambda step: "NEEDS_JSON" in step.get("env", {}))
            self.assertNotEqual(rejected.returncode, 0)


if __name__ == "__main__":
    unittest.main()
