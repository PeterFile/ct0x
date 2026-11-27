#!/usr/bin/env python3
"""Structural release contracts; actionlint supplies the YAML/expression parser."""
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile
import textwrap
import unittest

ROOT = Path(__file__).resolve().parents[2]


class WorkflowTests(unittest.TestCase):
    def workflow(self, name):
        path = ROOT / ".github/workflows" / name
        self.assertTrue(path.is_file(), "缺少可运行的候选工作流：" + name)
        return path.read_text()

    def test_binary_wakes_native_work_independently_and_collector_awaits_full(self):
        text = self.workflow("release-candidate.yml")
        jobs = {match[1]: match[2] for match in re.finditer(r"^  ([a-z][a-z-]*):\n(.*?)(?=^  [a-z][a-z-]*:\n|\Z)", text, re.M | re.S)}
        self.assertIn("uses: ./.github/workflows/ci-full.yml", jobs["full"])
        self.assertIn("policy_sha: ${{ needs.normalize.outputs.workflow_sha }}", jobs["full"])
        dependencies = {name: set(re.search(r"^    needs: \[([^\]]+)\]", block, re.M)[1].replace(" ", "").split(","))
                        for name, block in jobs.items() if re.search(r"^    needs: \[", block, re.M)}
        for name in ("full", "clients", "binary"):
            self.assertEqual(dependencies[name], {"normalize", "claim"})
        self.assertEqual(dependencies["machine"], {"normalize", "claim", "binary", "full-evidence"})
        self.assertIn("needs.full-evidence.result == 'success'", jobs["machine"])
        self.assertIn("needs.binary.result == 'success'", jobs["machine"])
        self.assertNotIn("machine", dependencies["full-evidence"])
        self.assertIn("full", dependencies["full-evidence"])
        self.assertIn("sole observer", text)
        self.assertNotIn("release-machine.py", jobs["machine"])
        self.assertIn("release-evidence.py collect", jobs["machine"])
        self.assertIn("!cancelled()", jobs["verdict"])
        self.assertEqual(dependencies["materials"], {"normalize", "claim", "full", "clients", "binary", "full-evidence", "machine"})
        self.assertEqual(dependencies["verdict"], {"normalize", "claim", "materials"})
        self.assertNotIn("needs.machine.result == 'success'", jobs["verdict"])
        self.assertNotIn("cancel-in-progress: true", text)
        for name in ("full", "clients", "binary", "machine", "full-evidence", "materials", "verdict"):
            self.assertIn("needs.claim.outputs.admitted == 'true'", jobs[name])
            self.assertIn("needs.claim.outputs.attempt == github.run_attempt", jobs[name])
        self.assertIn("group: release-candidate-claim-${{ needs.normalize.outputs.sha }}", jobs["claim"])
        self.assertIn("cancel-in-progress: false", jobs["claim"])
        self.assertEqual(text.count("statuses: write"), 1)
        self.assertNotIn("self-hosted", text)
        self.assertIn("name: Release candidate binary", jobs["binary"])
        self.assertIn("name: Release Full evidence", jobs["full-evidence"])
        for setting in ("GOOS: darwin", "GOARCH: amd64", "CGO_ENABLED: '0'", "-trimpath -buildvcs=true"):
            self.assertIn(setting, jobs["binary"])
        self.assertIn("release-target-${{ github.run_id }}-${{ github.run_attempt }}", jobs["binary"])
        self.assertIn("release-evidence-${{ needs.normalize.outputs.sha }}-${{ github.run_id }}-${{ github.run_attempt }}", jobs["verdict"])

    def test_materials_stage_is_provisional_and_only_named_remote_enforcement_can_emit_evidence(self):
        text = self.workflow("release-candidate.yml")
        jobs = {match[1]: match[2] for match in re.finditer(r"^  ([a-z][a-z-]*):\n(.*?)(?=^  [a-z][a-z-]*:\n|\Z)", text, re.M | re.S)}
        materials, verdict = jobs["materials"], jobs["verdict"]
        self.assertIn("name: Release candidate evidence\n", materials)
        self.assertIn("release-evidence.py materials --output", materials)
        self.assertIn("release-host-proof.py assess", materials)
        self.assertIn('--workflow-sha "$GITHUB_WORKFLOW_SHA"', materials)
        self.assertIn("release-materials-${{ github.run_id }}-${{ github.run_attempt }}", materials)
        self.assertIn('path: ${{ runner.temp }}/release-materials\n', materials)
        self.assertNotIn("evidence.json", materials)
        self.assertIn("name: Enforce remote machine proof\n", verdict)
        self.assertIn("release-host-proof.py enforce", verdict)
        self.assertNotIn("release-evidence.py verdict", verdict)
        self.assertNotIn("continue-on-error", verdict)
        self.assertIn("release-evidence-${{ needs.normalize.outputs.sha }}-${{ github.run_id }}-${{ github.run_attempt }}", verdict)
        self.assertIn("/release-evidence/evidence.json", verdict)

    def test_entries_pin_workflow_before_any_policy_code_and_keep_native_retries(self):
        text = self.workflow("release-candidate.yml")
        self.assertIn("run-name: RC ${{ inputs.sha || github.sha }}", text)
        self.assertIn("tags: ['rc/*']", text)
        self.assertNotIn("allow_whole_attempt_retry", text)
        pin = text.index("name: Authenticate and pin immutable workflow source")
        self.assertLess(text.index("ref: refs/heads/master"), pin)
        self.assertLess(pin, text.index("release-control.py normalize"))
        self.assertLess(pin, text.index("Test release admission"))
        self.assertIn("git merge-base --is-ancestor", text)
        self.assertIn("git -c core.hooksPath=/dev/null checkout --detach", text)
        self.assertIn("name: Release admission ${{ needs.normalize.outputs.sha }}", text)

    def pin_script(self):
        text = self.workflow("release-candidate.yml")
        match = re.search(r"      - name: Authenticate and pin immutable workflow source\n(.*?)(?=      - |\\Z)", text, re.S)
        self.assertIsNotNone(match, "must prove trusted source before executing policy")
        return textwrap.dedent(match[1].split("        run: |\n", 1)[1])

    def test_bootstrap_rejects_untrusted_ref_or_tag_mutation_before_checkout(self):
        script = self.pin_script()
        with tempfile.TemporaryDirectory(prefix="release-workflow-") as temporary:
            root = Path(temporary)
            binary = root / "git"
            binary.write_text("#!" + sys.executable + "\n" + textwrap.dedent('''\
                import os, sys
                from pathlib import Path
                args = sys.argv[1:]
                with open(os.environ["GIT_LOG"], "a") as output:
                    output.write(" ".join(args) + "\\n")
                if args[:2] == ["rev-parse", "--verify"]:
                    print("c" * 40)
                elif args[:2] == ["cat-file", "-t"]:
                    print("commit")
                elif args[:2] == ["merge-base", "--is-ancestor"]:
                    raise SystemExit(int(os.environ.get("ANCESTOR_FAIL", "0")))
                elif args[:2] == ["rev-parse", "HEAD"]:
                    print(os.environ["WORKFLOW_SHA"])
                elif "checkout" in args or args[:2] == ["status", "--porcelain"]:
                    pass
                else:
                    raise SystemExit(92)
                '''))
            binary.chmod(0o700)
            log = root / "git.log"
            env = dict(os.environ, PATH=str(root) + os.pathsep + os.environ["PATH"], GIT_LOG=str(log),
                       WORKFLOW_SHA="a" * 40, EVENT_NAME="workflow_dispatch", EVENT_REF="refs/heads/master",
                       TAG_CREATED="true", TAG_DELETED="false", TAG_FORCED="false", TAG_BEFORE="0" * 40)
            cases = [({}, True), ({"EVENT_NAME": "push", "EVENT_REF": "refs/tags/rc/候选+1"}, True),
                     ({"WORKFLOW_SHA": "A" * 40}, False), ({"WORKFLOW_SHA": "a" * 39}, False),
                     ({"WORKFLOW_SHA": "a" * 40 + "\\n"}, False), ({"ANCESTOR_FAIL": "1"}, False),
                     ({"EVENT_REF": "refs/heads/untrusted"}, False)]
            tag = {"EVENT_NAME": "push", "EVENT_REF": "refs/tags/rc/候选+1"}
            cases += [(tag | change, False) for change in ({"TAG_CREATED": "false"}, {"TAG_DELETED": "true"},
                       {"TAG_FORCED": "true"}, {"TAG_BEFORE": "b" * 40}, {"EVENT_REF": "refs/tags/rc/a/b"})]
            for index, (changes, accepted) in enumerate(cases):
                with self.subTest(changes=changes):
                    env["GIT_LOG"] = str(root / f"git-{index}.log")
                    result = subprocess.run(["bash", "-c", script], cwd=root, env=env | changes, capture_output=True, text=True, timeout=10)
                    self.assertEqual(result.returncode == 0, accepted, result.stderr)
                    output = Path(env["GIT_LOG"]).read_text() if Path(env["GIT_LOG"]).exists() else ""
                    self.assertEqual("checkout --detach" in output, accepted)

    def test_duplicate_or_stale_attempt_only_redirects_after_verifying_owner_without_real_work(self):
        text = self.workflow("release-candidate.yml")
        match = re.search(r"^  duplicate:\n(.*?)(?=^  [a-z][a-z-]*:\n|\\Z)", text, re.M | re.S)
        self.assertIsNotNone(match, "duplicates must not synthesize a successful workflow")
        job = match[1]
        self.assertIn("needs.claim.outputs.admitted != 'true'", job)
        self.assertIn("needs.claim.outputs.attempt != github.run_attempt", job)
        self.assertIn("needs.claim.outputs.owner_run_id", job)
        self.assertIn("ref: ${{ needs.normalize.outputs.workflow_sha }}", job)
        self.assertIn("GITHUB_TOKEN: ${{ github.token }}", job)
        self.assertIn("CANDIDATE_SHA: ${{ needs.normalize.outputs.sha }}", job)
        self.assertIn("CANDIDATE_TREE: ${{ needs.normalize.outputs.tree }}", job)
        self.assertIn("release-duplicate.py", job)
        self.assertNotIn("continue-on-error", job)
        self.assertNotIn("statuses: write", job)
        self.assertNotIn("upload-artifact", job)
        self.assertNotIn("go build", job)
        self.assertNotIn("release-machine.py", job)

    def test_intake_title_scopes_the_existing_single_observer(self):
        text = self.workflow("release-receipt.yml")
        self.assertIn("run-name: RC receipt ${{ fromJSON(inputs.receipt).run_id }} attempt ${{ fromJSON(inputs.receipt).run_attempt }}", text)
        self.assertNotIn("GITHUB_TRIGGERING_ACTOR_ID", text)

    def test_one_master_intake_has_no_user_url_or_command_input(self):
        text = self.workflow("release-receipt.yml")
        self.assertIn("workflow_dispatch:", text)
        self.assertIn("receipt:", text)
        self.assertIn("      machine_signature:\n", text)
        self.assertIn("        required: false\n        default: ''", text)
        self.assertNotIn("${{ inputs.machine_signature }}", text)
        self.assertIn("ref: ${{ github.workflow_sha }}", text)
        self.assertNotIn("${{ inputs.receipt }}", text)
        self.assertNotIn("statuses: write", text)
        self.assertNotIn("environment:", text)
        self.assertIn("release-evidence.py intake", text)


if __name__ == "__main__":
    unittest.main()
