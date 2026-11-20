#!/usr/bin/env python3
"""Local scheduling, routing, batching and fail-closed gate regressions; no product tests."""
from contextlib import redirect_stdout
import importlib.util
import io
import itertools
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile
import textwrap
import unittest
from unittest import mock


SCRIPTS = Path(__file__).resolve().parent
WORKFLOWS = SCRIPTS.parent / "workflows"
SHA = "a" * 40
TREE = "b" * 40
RUN = "42566516008"


def load(name):
    spec = importlib.util.spec_from_file_location(name.replace("-", "_"), SCRIPTS / (name + ".py"))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def pr(action="synchronize", draft=True, **fields):
    return {"action": action, "number": 17,
            "pull_request": {"number": 17, "draft": draft, "head": {"sha": SHA},
                             "base": {"ref": "master", "sha": "c" * 40}, "labels": []}, **fields}


def job(text, name):
    match = re.search(rf"^  {re.escape(name)}:\n(.*?)(?=^  [\w-]+:\n|\Z)", text, re.M | re.S)
    if not match:
        raise AssertionError("missing job: " + name)
    return match.group(1)


def expression(text, key):
    match = re.search(rf"^ +{re.escape(key)}: (?:>-\n +)?(\$\{{\{{.*?\}}\}})", text, re.M | re.S)
    if not match:
        raise AssertionError("missing expression: " + key)
    return " ".join(match.group(1)[3:-2].split())


class RoutingTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.plan = load("ci-plan")

    def test_events_and_draft_full_override(self):
        for action in ["opened", "reopened", "synchronize", "ready_for_review", "converted_to_draft"]:
            for draft in [True, False]:
                with self.subTest(action=action, draft=draft):
                    self.assertEqual(self.plan.mode("pull_request", pr(action, draft)), "feedback" if draft else "full")
        for draft in [True, False]:
            self.assertEqual(self.plan.mode("pull_request", pr("labeled", draft, label={"name": "ci:refresh"})),
                             "feedback" if draft else "full")
        self.assertEqual(self.plan.mode("pull_request", pr("labeled", label={"name": "ci:full"})), "full")
        event = pr()
        event["pull_request"]["labels"] = [{"name": "ci:full"}]
        self.assertEqual(self.plan.mode("pull_request", event), "full")
        self.assertEqual(self.plan.mode("pull_request", pr("labeled", label={"name": "CI:refresh"})), "feedback")
        self.assertEqual(self.plan.mode("pull_request", pr("labeled", label={"name": "CI:FULL"})), "full")
        self.assertEqual(self.plan.mode("push", {"ref": "refs/heads/master"}), "full")
        self.assertEqual(self.plan.mode("merge_group", {"action": "checks_requested"}), "full")

    def test_irrelevant_events_and_real_base_edits(self):
        events = [pr("edited", changes={"title": {"from": "old"}}),
                  pr("edited", changes={"body": {"from": "old"}}),
                  pr("edited", changes={"base": {"sha": {"from": "d" * 40}}}),
                  pr("labeled", label={"name": "bug"}), pr("labeled", False, label={"name": "ci:full"}),
                  pr("unlabeled", label={"name": "ci:full"}), pr("closed")]
        for event in events:
            # An already present override must not make an unrelated event relevant.
            event["pull_request"]["labels"] = [{"name": "ci:full"}]
            with self.subTest(event=event):
                self.assertEqual(self.plan.mode("pull_request", event), "ignore")
        for previous in ("release", "MASTER"):
            self.assertEqual(self.plan.mode("pull_request", pr("edited", changes={"base": {"ref": {"from": previous}}})), "feedback")
        self.assertEqual(self.plan.mode("push", {"ref": "refs/heads/feature"}), "ignore")

    def test_classification_union_and_conservative_fallback(self):
        for paths in [["PRD.md"], ["docs/design.md", "README.md"], ["docs/picture.png"], []]:
            result = self.plan.classify(paths)
            self.assertTrue(result["docs"])
            self.assertFalse(any(result[key] for key in ("go", "web", "bridge", "conservative")))
        for path, expected in [("web/src/App.tsx", "web"), ("extensions/pi-bridge/index.ts", "bridge"),
                               ("internal/store/task.go", "go"), ("cmd/workspace/main_test.go", "go")]:
            result = self.plan.classify([path])
            self.assertTrue(result[expected])
            self.assertFalse(result["conservative"])
        bridge = self.plan.classify(["extensions/pi-bridge/index.ts"])
        self.assertTrue(bridge["go"], "bridge changes also require Go wiring checks")
        both = self.plan.classify(["web/src/a.ts", "extensions/pi-bridge/config.ts", "internal/store/task.go"])
        self.assertTrue(all(both[key] for key in ("go", "web", "bridge")))
        for path in ["go.mod", "go.sum", "Makefile", ".github/workflows/ci.yml", "extensions/unknown/tool.js", ".github/unknown.md", "notes.rst"]:
            result = self.plan.classify([path])
            self.assertTrue(all(result[key] for key in ("go", "web", "bridge", "conservative")))

    def test_heavy_packages_never_get_unbounded_feedback_tests(self):
        for paths in [["internal/store/a.go"], ["internal/platform/a.go", "internal/httpapi/b.go"],
                      ["internal/scheduler/a.go", "internal/scheduler/b.go"], ["internal/scheduler/testdata/input.json"],
                      ["internal/deleted/a.go"], ["Makefile"]]:
            classified = self.plan.classify(paths)
            available = ["agentworkspace/internal/" + part for part in ("store", "platform", "httpapi", "coordination", "scheduler", "provider")]
            packages = self.plan.feedback_packages(classified, available)
            self.assertFalse(set(packages) & {"agentworkspace/internal/store", "agentworkspace/internal/platform", "agentworkspace/internal/httpapi"})
            if paths[0].startswith("internal/scheduler"):
                self.assertEqual(packages, ["agentworkspace/internal/scheduler"])
        bridge = self.plan.classify(["extensions/pi-bridge/index.ts"])
        self.assertIn("agentworkspace/internal/provider", self.plan.feedback_packages(bridge, available))
        renamed = [package.replace("agentworkspace/", "renamed/") for package in available]
        selected = self.plan.feedback_packages(self.plan.classify(["go.mod"]), renamed)
        self.assertEqual(selected, ["renamed/internal/coordination", "renamed/internal/provider", "renamed/internal/scheduler"])

    def test_feedback_runs_bounded_relevant_heavy_regressions_and_bridge_wiring(self):
        available = "\n".join("agentworkspace/internal/" + name for name in ("store", "platform", "httpapi", "scheduler", "provider"))
        for paths, heavy in [(["docs/architecture.md"], []), (["web/src/a.ts"], []),
                             (["extensions/pi-bridge/index.ts"], ["platform"]),
                             (["internal/store/task.go"], ["store"]), (["internal/store/testdata/input.json"], ["store"]),
                             (["internal/platform/a.go", "internal/httpapi/b.go"], ["platform", "httpapi"]),
                             (["Makefile"], ["store", "platform", "httpapi"])]:
            selected = {"mode": "feedback", **self.plan.classify(paths)}
            with self.subTest(paths=paths), mock.patch.object(self.plan, "command") as command:
                command.return_value = subprocess.CompletedProcess([], 0, stdout=available)
                self.plan.feedback(selected)
                calls = [call.args[0] for call in command.call_args_list]
                if selected["docs"]:
                    self.assertEqual(calls, [])
                    continue
                tests = [args for args in calls if args[:2] == ["go", "test"]]
                self.assertEqual(bool(tests), selected["go"])
                for profile in heavy:
                    expected = self.plan.FAST_TESTS[profile]
                    pattern = "^(" + "|".join(expected) + ")$"
                    self.assertIn(["go", "test", "-count=1", "-run", pattern, "./internal/" + profile], tests)
                for args in tests:
                    if any(package.endswith(("/store", "/platform", "/httpapi")) for package in args):
                        self.assertIn("-run", args)
                if selected["bridge"]:
                    self.assertTrue(any("agentworkspace/internal/provider" in args for args in tests))
        with mock.patch.object(self.plan, "command") as command:
            self.plan.feedback({"mode": "full"})
            command.assert_not_called()

    def test_clients_run_before_full_and_selectively_on_drafts(self):
        for mode in ("full", "feedback"):
            for paths in (["docs/a.md"], ["web/src/a.ts"], ["extensions/pi-bridge/index.ts"], ["Makefile"]):
                selected = {"mode": mode, **self.plan.classify(paths)}
                for client, scripts in [("web", ["test", "typecheck", "build"]), ("bridge", ["test", "typecheck"])]:
                    with self.subTest(mode=mode, paths=paths, client=client), mock.patch.object(self.plan, "command") as command:
                        self.plan.clients(selected, client)
                        actual = [call.args[0][-1] for call in command.call_args_list if call.args[0][:2] == ["npm", "run"]]
                        self.assertEqual(actual, scripts if mode == "full" or selected[client] else [])


class GitDiffTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.plan = load("ci-plan")

    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="ci-diff-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.git("init", "-q")

    def git(self, *args, data=None):
        return subprocess.run(["git", "-C", str(self.root), *args], input=data, capture_output=True, check=True).stdout

    def commit_object(self, files, parents=()):
        # Build disposable Git objects, never commit or change a project branch.
        root = {}
        for path, content in files.items():
            node = root
            parts = path.split("/")
            for part in parts[:-1]:
                node = node.setdefault(part, {})
            node[parts[-1]] = self.git("hash-object", "-w", "--stdin", data=content.encode()).strip()

        def tree(node):
            entries = []
            for name, value in sorted(node.items()):
                entry = b"040000 tree " + tree(value) if isinstance(value, dict) else b"100644 blob " + value
                entries.append(entry + b"\t" + os.fsencode(name) + b"\0")
            return self.git("mktree", "-z", data=b"".join(entries)).strip()

        tree_id = tree(root)
        data = b"tree " + tree_id + b"\n" + b"".join(b"parent " + parent.encode() + b"\n" for parent in parents)
        data += b"author CI <ci@example.invalid> 1 +0000\ncommitter CI <ci@example.invalid> 1 +0000\n\nfixture\n"
        sha = self.git("hash-object", "-t", "commit", "-w", "--stdin", data=data).strip().decode()
        return sha, tree_id.decode()

    def fixture(self, old, new):
        base, _ = self.commit_object(old)
        head, _ = self.commit_object(new, [base])
        merge, tree = self.commit_object(new, [base, head])
        self.git("update-ref", "HEAD", merge)
        event = pr()
        event["pull_request"]["head"]["sha"] = head
        return base, head, merge, tree, event

    def test_actual_synthetic_base_not_stale_payload_base(self):
        base, head, merge, tree, event = self.fixture({"docs/a.md": "old"}, {"docs/a.md": "new"})
        result = self.plan.plan("pull_request", event, merge, self.root)
        self.assertEqual((result["sha"], result["tree"], result["base"]), (merge, tree, base))
        self.assertNotEqual(base, event["pull_request"]["base"]["sha"])
        self.assertTrue(result["docs"])
        self.assertEqual(result["bucket"], 1)
        event_file, output, selected_file = (self.root / name for name in ("event.json", "outputs", "plan.json"))
        event_file.write_text(json.dumps(event))
        process = subprocess.run([sys.executable, str(SCRIPTS / "ci-plan.py"), "plan", str(selected_file)], cwd=self.root,
                                 env={**os.environ, "GITHUB_EVENT_PATH": str(event_file), "GITHUB_OUTPUT": str(output),
                                      "GITHUB_EVENT_NAME": "pull_request", "GITHUB_SHA": merge, "GITHUB_RUN_ID": RUN}, capture_output=True, timeout=5)
        self.assertEqual(process.returncode, 0, process.stderr)
        self.assertEqual(json.loads(selected_file.read_text()), result)
        self.assertEqual(set(line.split("=", 1)[0] for line in output.read_text().splitlines()),
                         {"mode", "bucket", "sha", "tree", "base", "head", "matrix", "run_id", "go", "web", "bridge", "conservative"})
        for bad_sha in [head, "f" * 40, merge + "\nowned=true"]:
            with self.assertRaises(ValueError):
                self.plan.plan("pull_request", event, bad_sha, self.root)
        event["pull_request"]["head"]["sha"] = "f" * 40
        with self.assertRaises(ValueError):
            self.plan.plan("pull_request", event, merge, self.root)

    def test_nul_safe_rename_removal_and_over_300_files(self):
        strange = "web/src/$(touch SHOULD_NOT_EXIST)\nquoted'\t.ts"
        old = {"docs/old.md": "same", "extensions/pi-bridge/deleted.ts": "old"}
        new = {strange: "same", **{f"docs/{index}.md": "new" for index in range(351)}}
        base, _, merge, _, event = self.fixture(old, new)
        paths = self.plan.changed_paths(base, merge, self.root)
        self.assertEqual(set(paths), set(old) | set(new))
        result = self.plan.plan("pull_request", event, merge, self.root)
        self.assertTrue(all(result[key] for key in ("web", "bridge", "go")))
        self.assertFalse((self.root / "SHOULD_NOT_EXIST").exists())

    def test_full_product_plan_does_not_add_a_whitespace_style_gate(self):
        _, _, merge, _, event = self.fixture({"web/src/a.ts": "old\n"}, {"web/src/a.ts": "new  \n"})
        event["pull_request"]["draft"] = False
        event_file = self.root / "event.json"
        event_file.write_text(json.dumps(event))
        result = subprocess.run([sys.executable, str(SCRIPTS / "ci-plan.py"), "plan", str(self.root / "plan.json")], cwd=self.root,
                                env={**os.environ, "GITHUB_EVENT_PATH": str(event_file), "GITHUB_OUTPUT": str(self.root / "outputs"),
                                     "GITHUB_EVENT_NAME": "pull_request", "GITHUB_SHA": merge, "GITHUB_RUN_ID": RUN},
                                capture_output=True, timeout=5)
        self.assertEqual(result.returncode, 0, result.stderr)
        selected = json.loads((self.root / "plan.json").read_text())
        self.assertEqual(selected["head"], event["pull_request"]["head"]["sha"])
        for field in ("head", "base", "sha", "tree"):
            self.assertIn(field, result.stdout.decode())

    def test_reject_nonmerge_identity_and_missing_parent(self):
        head, _ = self.commit_object({"PRD.md": "text"})
        self.git("update-ref", "HEAD", head)
        with self.assertRaises(ValueError):
            self.plan.plan("pull_request", pr(), head, self.root)


class AdmissionTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.workflow = (WORKFLOWS / "ci.yml").read_text()

    def test_relevance_is_shared_with_supersession_and_admission_uses_native_queue(self):
        relevant = expression(job(self.workflow, "feedback"), "if")
        group = expression(self.workflow.split("jobs:", 1)[0], "group")
        self.assertIn(relevant, group)
        self.assertIn("format('ignored-{0}', github.run_id)", group)
        self.assertIn("github.event.pull_request.number || github.ref", group)
        self.assertIn("cancel-in-progress: true", self.workflow.split("jobs:", 1)[0])
        full = job(self.workflow, "full")
        for text in ["uses: ./.github/workflows/ci-full.yml", "needs: feedback", "needs.feedback.outputs.mode == 'full'",
                     "queue: max", "needs.feedback.outputs.bucket", "cancel-in-progress: false"]:
            self.assertIn(text, full)
        self.assertNotIn("runs-on:", full)
        self.assertNotIn("concurrency:", (WORKFLOWS / "ci-full.yml").read_text())
        plan = load("ci-plan")
        self.assertEqual([plan.bucket("pull_request", pr(number=n)) for n in range(1, 151)], [n % 2 for n in range(1, 151)])

    def test_clients_and_cmd_are_preflight_not_repeated_after_admission(self):
        feedback = job(self.workflow, "feedback")
        self.assertIn("name: CI preflight and feedback", feedback)
        positions = [feedback.index(text) for text in ["go test -count=1 ./cmd/workspace", "ci-plan.py feedback", "ci-plan.py web", "ci-plan.py bridge"]]
        self.assertEqual(positions, sorted(positions))
        self.assertIn("node-version: '22.x'", feedback)
        self.assertIn("node-version: '24.13.0'", feedback)
        self.assertIn("steps.plan.outputs.mode == 'full' || steps.plan.outputs.web == 'true'", feedback)
        self.assertIn("steps.plan.outputs.mode == 'full' || steps.plan.outputs.bridge == 'true'", feedback)
        self.assertIn("fetch-depth: 2", feedback)
        self.assertIn("ref: ${{ github.sha }}", feedback)
        full = (WORKFLOWS / "ci-full.yml").read_text()
        self.assertNotRegex(full, r"(?m)^  (web|pi-bridge):")
        self.assertNotIn("npm ", full)
        for script in ["test-ci-scheduling.py", "test-store-shard.py"]:
            self.assertEqual(full.count("python3 .github/scripts/" + script + " -v"), 1)
        self.assertNotIn("unittest discover", feedback + full)

    def test_outer_gate_cannot_disappear_with_missing_preflight_outputs(self):
        gate = job(self.workflow, "merge-check")
        self.assertIn('needs: ["feedback", "full"]', gate)
        condition = expression(gate, "if")
        self.assertTrue(condition.startswith("always() && ("))
        self.assertNotIn("needs.", condition, "candidate detection must survive preflight failure or skips")
        candidate = condition[len("always() && ("):-1]
        self.assertEqual(expression(gate, "name"), candidate + " && 'Full CI' || 'Feedback status'")
        self.assertIn(expression(job(self.workflow, "feedback"), "if"), candidate)
        self.assertIn("github.event.pull_request.draft", candidate)
        self.assertIn("contains(github.event.pull_request.labels.*.name, 'ci:full')", candidate)
        self.assertIn("ci-gate.py outer", gate)
        self.assertNotIn("setup-go", gate)
        self.assertNotIn("setup-node", gate)

    def test_trigger_and_security_contract(self):
        for action in ["opened", "reopened", "synchronize", "ready_for_review", "converted_to_draft", "edited", "labeled"]:
            self.assertIn(action, self.workflow.split("permissions:", 1)[0])
        self.assertIn("merge_group:", self.workflow)
        self.assertIn("checks_requested", self.workflow)
        for text in [self.workflow, (WORKFLOWS / "ci-full.yml").read_text()]:
            self.assertNotIn("pull_request_target", text)
            self.assertIn("contents: read", text)
            self.assertEqual(text.count("actions/checkout@"), text.count("persist-credentials: false"))
            self.assertNotIn("secrets:", text)
            self.assertNotIn("AGENT_WORKSPACE_LIVE_SMOKE", text)


class ShardBatchTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.shards = load("ci-shards")
        cls.verifier = load("verify-store-shard")

    def test_exact_81_logical_slots_in_23_bounded_workers_and_generated_matrix(self):
        batches = self.shards.BATCHES
        self.assertEqual({name: len(workers) for name, workers in batches.items()}, {"store": 16, "platform": 5, "httpapi": 2})
        for profile, workers in batches.items():
            flat = list(itertools.chain.from_iterable(workers))
            self.assertEqual(sorted(flat), list(range(self.verifier.PROFILES[profile][0])))
            self.assertTrue(all(0 < len(slots) <= (2 if profile == "platform" else 4) for slots in workers))
        self.assertEqual(sum(len(slots) for workers in batches.values() for slots in workers), 81)
        plan = load("ci-plan")
        matrix = plan.shard_matrix()
        self.assertEqual(matrix, {"include": [{"profile": profile, "worker": worker}
                                              for profile, workers in batches.items() for worker in range(len(workers))]})
        self.assertEqual(matrix["include"][:2], [{"profile": "platform", "worker": 0}, {"profile": "platform", "worker": 1}])
        self.assertIn(4, batches["platform"][0])
        self.assertIn(8, batches["platform"][1])
        workflow = (WORKFLOWS / "ci-full.yml").read_text()
        shards = job(workflow, "go-shards")
        for text in ["matrix: ${{ fromJSON(inputs.matrix) }}", "max-parallel: 7", "fail-fast: false", "timeout-minutes: 25"]:
            self.assertIn(text, shards)
        self.assertIn("merge-multiple: false", job(workflow, "gate"))

    def test_workers_list_once_then_81_real_verified_uncached_logical_runs(self):
        names = [f"TestVector{index}" for index in range(180)]
        calls = []

        def go(command, **kwargs):
            calls.append(command)
            target = command[-1]
            package = "agentworkspace/" + target.removeprefix("./")
            if command[:2] == ["go", "list"]:
                return subprocess.CompletedProcess(command, 0, stdout=package + "\n")
            self.assertEqual(command[:2], ["go", "test"])
            rows = [{"Action": "start", "Package": package}]
            if "-list" in command:
                rows.append({"Action": "output", "Package": package, "Output": "\n".join(names) + f"\nok  \t{package}\t0.001s\n"})
            else:
                pattern = command[command.index("-run") + 1]
                rows.extend({"Action": action, "Package": package, "Test": name}
                            for name in names if re.fullmatch(pattern, name) for action in ("run", "pass"))
            rows.append({"Action": "pass", "Package": package})
            kwargs["stdout"].write("".join(json.dumps(row) + "\n" for row in rows).encode())
            return subprocess.CompletedProcess(command, 0)

        with tempfile.TemporaryDirectory(prefix="ci-worker-") as directory, redirect_stdout(io.StringIO()), \
             mock.patch.object(self.shards, "check_identity"), mock.patch.object(self.shards, "command", side_effect=go):
            root = Path(directory)
            for profile, workers in self.shards.BATCHES.items():
                for worker, slots in enumerate(workers):
                    calls.clear()
                    destination = root / f"{profile}-worker-v1-{worker}"
                    self.shards.run_worker(profile, worker, destination, RUN, SHA, TREE)
                    tests = [command for command in calls if command[:2] == ["go", "test"]]
                    self.assertEqual(len(tests), len(slots) + 1)
                    self.assertEqual(sum("-list" in command for command in tests), 1)
                    for command in tests:
                        for flag in ("-count=1", "-race", "-json"):
                            self.assertIn(flag, command)
                    self.assertEqual({path.name for path in destination.iterdir()}, {f"{profile}-shard-v1-{slot}" for slot in slots})
            result = self.shards.aggregate_workers(root, RUN, SHA, TREE)
            self.assertEqual(sum(summary["total"] for summary in result.values()), 81)
            for profile, summary in result.items():
                self.assertEqual(summary["runs"], {name: 1 for name in names})
                self.assertEqual((summary["run_id"], summary["checkout_sha"], summary["checkout_tree"]), (RUN, SHA, TREE))

    def test_wrong_checkout_and_failed_execution_cannot_produce_evidence(self):
        for actual_sha, actual_tree in [("c" * 40, TREE), (SHA, "c" * 40)]:
            with mock.patch.object(self.shards, "command") as command:
                command.side_effect = [subprocess.CompletedProcess([], 0, stdout=value + "\n") for value in (actual_sha, actual_tree)]
                with self.assertRaises(ValueError):
                    self.shards.check_identity(SHA, TREE)
        with tempfile.TemporaryDirectory(prefix="ci-failed-worker-") as directory, mock.patch.object(self.shards, "check_identity"), \
             mock.patch.object(self.shards, "command") as command, mock.patch.object(self.shards.verifier, "inventory", return_value={"TestVector6": 1}):
            command.side_effect = [subprocess.CompletedProcess([], 0, stdout="agentworkspace/internal/store\n"),
                                   subprocess.CompletedProcess([], 0), subprocess.CalledProcessError(1, ["go", "test"])]
            with self.assertRaises(subprocess.CalledProcessError), redirect_stdout(io.StringIO()):
                self.shards.run_worker("store", 0, Path(directory), RUN, SHA, TREE)
            self.assertEqual(list(Path(directory).iterdir()), [])

    def evidence(self, root):
        compiled = {f"TestVector{index}": 1 for index in range(180)}
        expected = {"run_id": RUN, "checkout_sha": SHA, "checkout_tree": TREE}
        for profile, workers in self.shards.BATCHES.items():
            total, package, summary_name, prefix = self.verifier.PROFILES[profile]
            for worker, slots in enumerate(workers):
                for slot in slots:
                    selected, _ = self.verifier.selection(compiled, slot, profile)
                    summary = {"schema": 1, "slot": slot, "total": total, **expected, "package": package,
                               "compiled": compiled, "selected": selected, "runs": selected, "passes": selected, "skips": {}}
                    path = root / f"{profile}-worker-v1-{worker}" / f"{prefix}{slot}" / summary_name
                    path.parent.mkdir(parents=True, exist_ok=True)
                    path.write_text(json.dumps(summary))

    def test_batched_evidence_uses_existing_aggregate_and_fails_closed(self):
        with tempfile.TemporaryDirectory(prefix="ci-evidence-") as directory:
            root = Path(directory)
            self.evidence(root)
            result = self.shards.aggregate_workers(root, RUN, SHA, TREE)
            self.assertEqual({profile: value["total"] for profile, value in result.items()}, {"store": 64, "platform": 9, "httpapi": 8})
            target = root / "store-worker-v1-0/store-shard-v1-0/store-summary.json"
            original = target.read_text()
            for field, value in [("checkout_sha", "c" * 40), ("checkout_tree", "c" * 40), ("run_id", "999"),
                                 ("package", "other/store"), ("slot", 1), ("runs", {}), ("skips", {"TestVector6": 1})]:
                with self.subTest(field=field):
                    summary = json.loads(original)
                    summary[field] = value
                    target.write_text(json.dumps(summary))
                    with self.assertRaises(ValueError):
                        self.shards.aggregate_workers(root, RUN, SHA, TREE)
                    target.write_text(original)
            # Missing, extra and mis-batched artifacts cannot be hidden by merging downloads.
            for path in [target, target.parent, root / "platform-worker-v1-2"]:
                moved = root.parent / (root.name + "-moved")
                path.rename(moved)
                try:
                    with self.assertRaises(ValueError):
                        self.shards.aggregate_workers(root, RUN, SHA, TREE)
                    path.symlink_to(moved, target_is_directory=moved.is_dir())
                    with self.assertRaises(ValueError):
                        self.shards.aggregate_workers(root, RUN, SHA, TREE)
                    path.unlink()
                finally:
                    moved.rename(path)
            extra = root / "store-worker-v1-16"
            extra.mkdir()
            with self.assertRaises(ValueError):
                self.shards.aggregate_workers(root, RUN, SHA, TREE)
            extra.rmdir()
            duplicate = root / "store-worker-v1-1/store-shard-v1-0"
            duplicate.mkdir()
            (duplicate / target.name).write_text(original)
            with self.assertRaises(ValueError):
                self.shards.aggregate_workers(root, RUN, SHA, TREE)


class GateTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.gate = load("ci-gate")

    def test_actual_gate_cli_rejects_every_missing_or_nonsuccess_result(self):
        full = (WORKFLOWS / "ci-full.yml").read_text()
        gate = job(full, "gate")
        self.assertIn("if: ${{ always() }}", gate)
        needs = json.loads(re.search(r"^    needs: (\[[^\n]+\])$", gate, re.M)[1])
        self.assertEqual(set(needs), set(self.gate.REQUIRED))
        step = re.search(r"- name: Check required results\n(.*?)(?=\n      -)", gate, re.S)
        self.assertIsNotNone(step)
        self.assertIn("toJSON(needs)", step[1])
        command = re.search(r"run: ([^\n]+)", step[1])[1]
        self.assertLess(gate.index("Check required results"), gate.index("download-artifact"))
        success = {name: {"result": "success", "outputs": {}} for name in needs}
        cases = [(success, True)]
        for name in needs:
            cases.append(({key: value for key, value in success.items() if key != name}, False))
            for state in ["failure", "cancelled", "skipped", "", "unknown"]:
                cases.append(({**success, name: {"result": state}}, False))
        cases.extend([({}, False), ([], False), ({**success, "unexpected": {"result": "success"}}, False)])
        for payload, accepted in cases:
            with self.subTest(payload=payload):
                result = subprocess.run(["bash", "-c", command], cwd=SCRIPTS.parent.parent,
                                        env={**os.environ, "NEEDS_JSON": json.dumps(payload)}, capture_output=True, timeout=5)
                self.assertEqual(result.returncode == 0, accepted, result.stderr)
        self.assertNotIn("setup-go", gate)
        self.assertNotRegex(gate, r"\b(?:go test|go vet|npm)\b")

    def test_all_required_result_combinations_fail_closed(self):
        required = sorted(self.gate.REQUIRED)
        for states in itertools.product(("success", "failure", "cancelled", "skipped", "", "unknown"), repeat=len(required)):
            results = {name: {"result": state} for name, state in zip(required, states)}
            try:
                self.gate.check_results(results)
                accepted = True
            except ValueError:
                accepted = False
            self.assertEqual(accepted, all(state == "success" for state in states), states)

    def test_actual_outer_gate_rejects_skipped_preflight_missing_proofs_and_nonfull_events(self):
        gate = job((WORKFLOWS / "ci.yml").read_text(), "merge-check")
        command = re.search(r"run: ([^\n]+)", gate)[1]
        identity = {"run_id": RUN, "sha": SHA, "tree": TREE, "head": "c" * 40, "base": "d" * 40}
        success = {"feedback": {"result": "success", "outputs": {"mode": "full", **identity}},
                   "full": {"result": "success", "outputs": {"verified": "true", **identity}}}
        cases = [(success, pr(draft=False), True)]
        for dependency in ("feedback", "full"):
            for state in ("failure", "cancelled", "skipped", "", "unknown"):
                cases.append(({**success, dependency: {"result": state}}, pr(draft=False), False))
            cases.append(({key: value for key, value in success.items() if key != dependency}, pr(draft=False), False))
            for field in identity:
                broken = json.loads(json.dumps(success))
                broken[dependency]["outputs"][field] = "wrong"
                cases.append((broken, pr(draft=False), False))
        for field in ("verified", "sha", "tree", "run_id"):
            broken = json.loads(json.dumps(success))
            broken["full"]["outputs"].pop(field)
            cases.append((broken, pr(draft=False), False))
        cases.extend([(success, pr(), False), (success, pr("labeled", False, label={"name": "bug"}), False),
                      (success, pr("labeled", label={"name": "CI:FULL"}), True), ({}, pr(draft=False), False)])
        with tempfile.TemporaryDirectory(prefix="ci-outer-gate-") as directory:
            event_file = Path(directory) / "event.json"
            for payload, event, accepted in cases:
                event_file.write_text(json.dumps(event))
                result = subprocess.run(["bash", "-c", command], cwd=SCRIPTS.parent.parent,
                                        env={**os.environ, "NEEDS_JSON": json.dumps(payload), "GITHUB_RUN_ID": RUN,
                                             "GITHUB_SHA": SHA, "GITHUB_EVENT_PATH": str(event_file), "GITHUB_EVENT_NAME": "pull_request"},
                                        capture_output=True, timeout=5)
                self.assertEqual(result.returncode == 0, accepted, result.stderr)

    def test_go_remainder_vet_and_bench_start_without_shard_dependencies(self):
        workflow = (WORKFLOWS / "ci-full.yml").read_text()
        for name in ["go-other", "go-vet", "go-benchmarks"]:
            self.assertNotIn("needs:", job(workflow, name))
        other = job(workflow, "go-other")
        self.assertIn("go test -count=1 ./internal/coordination", other)
        self.assertIn("go test -count=1 -race ./internal/coordination", other)
        self.assertIn('go test -race "${rest[@]}"', other)
        self.assertIn("go vet ./...", job(workflow, "go-vet"))
        self.assertIn("go test -run '^$' -bench . -benchmem ./...", job(workflow, "go-benchmarks"))
        self.assertNotIn("npm ", workflow)

    def test_actual_benchmark_filter_preserves_only_complete_metrics_and_pipefail(self):
        workflow = (WORKFLOWS / "ci-full.yml").read_text()
        block = job(workflow, "go-benchmarks")
        match = re.search(r"- name: Record benchmark metrics\n.*?        run: \|\n(.*?)(?=\n      -)", block, re.S)
        self.assertIsNotNone(match)
        script = textwrap.dedent(match[1])
        fixture = "\n".join(["goos: linux", "sensitive test log", "BenchmarkBad-8 10 5 ns/op 1 B/op 2 allocs/op extra",
                               "BenchmarkMissing-8 10 5 ns/op", "BenchmarkWrong-8 10 5 ns/op 1 bytes 2 allocs/op",
                               "BenchmarkAlloc-8\t100\t1.25 ns/op\t16 B/op\t2 allocs/op",
                               "BenchmarkStream-8 5 2.5e+02 ns/op 1.00 MB/s 0 B/op 0 allocs/op", "PASS"])
        stub = '''go() {
          [[ "$*" == 'test -run ^$ -bench . -benchmem ./...' ]] || return 90
          printf '%s\\n' "$BENCH_OUTPUT"
          return "$BENCH_EXIT"
        }
        readonly -f go
        '''
        with tempfile.TemporaryDirectory(prefix="ci-bench-") as directory:
            for code in (0, 1):
                result = subprocess.run(["bash", "-c", stub + script],
                                        env={**os.environ, "RUNNER_TEMP": directory, "BENCH_OUTPUT": fixture, "BENCH_EXIT": str(code),
                                             "GITHUB_RUN_ID": RUN, "CHECKOUT_SHA": SHA, "CHECKOUT_TREE": TREE, "HEAD_SHA": SHA, "BASE_SHA": SHA},
                                        capture_output=True, timeout=5)
                self.assertEqual(result.returncode == 0, code == 0, result.stderr)
                self.assertEqual((Path(directory) / "go-benchmarks.txt").read_text().splitlines(),
                                 ["BenchmarkAlloc-8 100 1.25 ns/op 16 B/op 2 allocs/op",
                                  "BenchmarkStream-8 5 2.5e+02 ns/op 1.00 MB/s 0 B/op 0 allocs/op"])


if __name__ == "__main__":
    unittest.main()
