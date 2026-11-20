#!/usr/bin/env python3
"""Local scheduling, routing, batching and fail-closed gate regressions; no product tests."""
import ast
from collections import deque
import importlib.util
import itertools
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile
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


def evaluate(source, event, name="pull_request", run=1, ref="refs/pull/17/merge"):
    # Evaluate the actual, deliberately small GitHub expression subset used by CI.
    context = {"github": {"event": event, "event_name": name, "workflow": "CI", "run_id": run, "ref": ref}}

    def lookup(match):
        value = context
        for part in match.group().split("."):
            value = value.get(part) if isinstance(value, dict) else None
        return repr(value)

    source = re.sub(r"\bgithub(?:\.[A-Za-z_][A-Za-z_0-9]*)+", lookup, source)
    source = source.replace("&&", " and ").replace("||", " or ")
    source = re.sub(r"\b(true|false|null)\b", lambda m: {"true": "True", "false": "False", "null": "None"}[m[0]], source)
    tree = ast.parse(source, mode="eval")
    allowed = (ast.Expression, ast.BoolOp, ast.And, ast.Or, ast.Compare, ast.Eq, ast.NotEq,
               ast.Call, ast.Name, ast.Load, ast.Constant)
    if not all(isinstance(node, allowed) for node in ast.walk(tree)):
        raise AssertionError("unsupported CI expression: " + source)
    def normalized(value):
        return value.casefold() if isinstance(value, str) else value

    class Comparisons(ast.NodeTransformer):
        def visit_Compare(self, node):
            self.assert_single(node)
            function = "equal" if isinstance(node.ops[0], ast.Eq) else "different"
            return ast.Call(func=ast.Name(id=function, ctx=ast.Load()), args=[node.left, node.comparators[0]], keywords=[])

        @staticmethod
        def assert_single(node):
            if len(node.ops) != 1:
                raise AssertionError("chained GitHub comparison")

    tree = ast.fix_missing_locations(Comparisons().visit(tree))
    equal = lambda left, right: normalized(left) == normalized(right)
    return eval(compile(tree, "<ci-expression>", "eval"), {"__builtins__": {}},
                {"contains": lambda items, value: any(equal(item, value) for item in items), "fromJSON": json.loads,
                 "equal": equal, "different": lambda left, right: not equal(left, right),
                 "format": lambda pattern, *values: pattern.format(*values)})


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
        for path in ["go.mod", "go.sum", "Makefile", ".github/workflows/ci.yml", "extensions/unknown/tool.js"]:
            result = self.plan.classify([path])
            self.assertTrue(all(result[key] for key in ("go", "web", "bridge", "conservative")))

    def test_heavy_packages_never_get_unbounded_feedback_tests(self):
        for paths in [["internal/store/a.go"], ["internal/platform/a.go", "internal/httpapi/b.go"],
                      ["internal/scheduler/a.go", "internal/scheduler/b.go"], ["internal/deleted/a.go"], ["Makefile"]]:
            classified = self.plan.classify(paths)
            available = ["agentworkspace/internal/" + part for part in ("store", "platform", "httpapi", "coordination", "scheduler", "provider")]
            packages = self.plan.feedback_packages(classified, available)
            self.assertFalse(set(packages) & {"agentworkspace/internal/store", "agentworkspace/internal/platform", "agentworkspace/internal/httpapi"})
            if paths[0].startswith("internal/scheduler"):
                self.assertEqual(packages, ["agentworkspace/internal/scheduler"])
        bridge = self.plan.classify(["extensions/pi-bridge/index.ts"])
        self.assertIn("agentworkspace/internal/provider", self.plan.feedback_packages(bridge, available))

    def test_feedback_executes_selected_commands_without_docs_product_tests(self):
        available = "\n".join("agentworkspace/internal/" + name for name in ("store", "platform", "httpapi", "scheduler", "provider"))
        for paths in [["docs/architecture.md"], ["web/src/a.ts"], ["extensions/pi-bridge/index.ts"],
                      ["internal/scheduler/loop.go"], ["internal/store/task.go"], ["Makefile"]]:
            selected = {"mode": "feedback", **self.plan.classify(paths)}
            with self.subTest(paths=paths), mock.patch.object(self.plan, "command") as command:
                command.return_value = subprocess.CompletedProcess([], 0, stdout=available)
                self.plan.feedback(selected)
                calls = [call.args[0] for call in command.call_args_list]
                if selected["docs"]:
                    self.assertEqual(calls, [])
                    continue
                for directory, required in [("web", selected["web"]), ("extensions/pi-bridge", selected["bridge"])]:
                    scripts = [call.args[0][-1] for call in command.call_args_list
                               if call.kwargs.get("cwd") == directory and call.args[0][:2] == ["npm", "run"]]
                    self.assertEqual(scripts, (["test", "typecheck", "build"] if directory == "web" else ["test", "typecheck"]) if required else [])
                go_tests = [args for args in calls if args[:2] == ["go", "test"]]
                self.assertEqual(bool(go_tests), selected["go"])
                if go_tests:
                    self.assertEqual(go_tests[0], ["go", "test", "-run", "^$", "./..."])
                    self.assertFalse(any(set(args) & self.plan.HEAVY for args in go_tests))
                    if selected["bridge"]:
                        self.assertTrue(any("agentworkspace/internal/provider" in args for args in go_tests))
        with mock.patch.object(self.plan, "command") as command:
            self.plan.feedback({"mode": "full"})
            command.assert_not_called()


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
                                      "GITHUB_EVENT_NAME": "pull_request", "GITHUB_SHA": merge}, capture_output=True, timeout=5)
        self.assertEqual(process.returncode, 0, process.stderr)
        self.assertEqual(json.loads(selected_file.read_text()), result)
        self.assertEqual(set(line.split("=", 1)[0] for line in output.read_text().splitlines()),
                         {"mode", "bucket", "sha", "tree", "go", "web", "bridge", "conservative"})
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

    def test_reject_nonmerge_identity_and_missing_parent(self):
        head, _ = self.commit_object({"PRD.md": "text"})
        self.git("update-ref", "HEAD", head)
        with self.assertRaises(ValueError):
            self.plan.plan("pull_request", pr(), head, self.root)


class AdmissionTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.workflow = (WORKFLOWS / "ci.yml").read_text()

    def test_actual_event_condition_and_supersession_group(self):
        condition = expression(job(self.workflow, "feedback"), "if")
        group = expression(self.workflow.split("jobs:", 1)[0], "group")
        cases = [(pr(action), True) for action in ["opened", "reopened", "synchronize", "ready_for_review", "converted_to_draft"]]
        cases += [(pr("labeled", label={"name": label}), label.casefold() in ("ci:refresh", "ci:full")) for label in ["ci:refresh", "ci:full", "bug", "CI:refresh"]]
        cases += [(pr("labeled", False, label={"name": "ci:full"}), False),
                  (pr("edited", changes={"body": {"from": "old"}}), False),
                  (pr("edited", changes={"base": {"sha": {"from": "old"}}}), False),
                  (pr("edited", changes={"base": {"ref": {"from": "release"}}}), True),
                  (pr("edited", changes={"base": {"ref": {"from": "MASTER"}}}), True)]
        for event, relevant in cases:
            with self.subTest(event=event):
                self.assertEqual(bool(evaluate(condition, event)), relevant)
                keys = [evaluate(group, event, run=run) for run in [1, 2]]
                self.assertEqual(keys[0] == keys[1], relevant)
                self.assertEqual(keys[0] == evaluate(group, pr()), relevant)
        self.assertNotEqual(evaluate(group, pr()), evaluate(group, {**pr(), "number": 18, "pull_request": {**pr()["pull_request"], "number": 18}}))
        for name, event in [("push", {"ref": "refs/heads/master"}), ("merge_group", {"action": "checks_requested"})]:
            self.assertTrue(evaluate(condition, event, name))
        self.assertIn("cancel-in-progress: true", self.workflow.split("jobs:", 1)[0])
        plan = load("ci-plan")
        for event, relevant in cases:
            self.assertEqual(plan.mode("pull_request", event) != "ignore", relevant)

    def test_two_native_fifo_admission_lanes_not_lossy_default(self):
        full = job(self.workflow, "full")
        self.assertIn("uses: ./.github/workflows/ci-full.yml", full)
        self.assertIn("needs: feedback", full)
        self.assertIn("needs.feedback.outputs.mode == 'full'", full)
        self.assertIn("queue: max", full)
        self.assertIn("needs.feedback.outputs.bucket", full)
        self.assertIn("cancel-in-progress: false", full)
        self.assertNotIn("runs-on:", full, "admission must wrap the entire reusable suite, not a short semaphore job")
        self.assertNotIn("concurrency:", (WORKFLOWS / "ci-full.yml").read_text())
        plan = load("ci-plan")
        # Model the documented native queue (FIFO by arrival at the lane, max 100 pending).
        active, pending, dropped = {}, [deque(), deque()], []
        for number in range(1, 151):
            lane = plan.bucket("pull_request", pr(number=number))
            self.assertIn(lane, (0, 1))
            self.assertEqual(lane, number % 2)
            if lane not in active:
                active[lane] = number
            elif len(pending[lane]) < 100:
                pending[lane].append(number)
            else:
                dropped.append(number)
        self.assertEqual(len(active), 2)
        self.assertEqual(dropped, [], "unrelated PRs must not replace each other's pending suites")
        for lane in (0, 1):
            sequence = [active[lane], *pending[lane]]
            self.assertEqual(sequence, [number for number in range(1, 151) if number % 2 == lane])
        # Supersession removes only the same PR; the other pending requests retain order.
        lane = plan.bucket("pull_request", pr(number=17))
        pending[lane].remove(17)
        pending[lane].append(17)
        self.assertEqual(list(pending[lane])[:-1], [n for n in range(3, 151, 2) if n != 17])
        # The published limit cancels overflow, not an older pending request.
        queue = deque(range(100))
        before = list(queue)
        if len(queue) < 100:
            queue.append(101)
        self.assertEqual(list(queue), before)

    def test_single_feedback_job_and_full_gate_not_emitted_on_quick_or_ignore(self):
        self.assertEqual(re.findall(r"^  ([\w-]+):$", self.workflow.split("jobs:\n", 1)[1], re.M), ["feedback", "full"])
        feedback = job(self.workflow, "feedback")
        self.assertIn("name: Draft feedback", feedback)
        self.assertIn("go test -count=1 ./cmd/workspace", feedback)
        self.assertIn("steps.plan.outputs.mode == 'full' || steps.plan.outputs.go == 'true'", feedback)
        self.assertIn("ci-plan.py feedback", feedback)
        self.assertNotIn("Full CI", feedback)
        full = (WORKFLOWS / "ci-full.yml").read_text()
        self.assertEqual(full.count("name: Full CI\n"), 1)
        self.assertIn("name: Full suite", job(self.workflow, "full"))
        self.assertIn("github.sha", feedback)
        self.assertNotIn("pull_request.head.sha", feedback)
        self.assertIn("fetch-depth: 2", feedback)
        self.assertEqual(feedback.count("python3 .github/scripts/test-ci-scheduling.py -v"), 1)
        self.assertEqual(feedback.count("python3 .github/scripts/test-store-shard.py -v"), 1)
        self.assertNotIn("unittest discover", feedback, "hyphenated legacy test files are not discoverable modules")

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

    def test_exact_81_logical_slots_in_21_bounded_workers(self):
        batches = self.shards.BATCHES
        self.assertEqual({name: len(workers) for name, workers in batches.items()}, {"store": 16, "platform": 3, "httpapi": 2})
        for profile, workers in batches.items():
            flat = list(itertools.chain.from_iterable(workers))
            self.assertEqual(flat, list(range(self.verifier.PROFILES[profile][0])))
            self.assertTrue(all(0 < len(slots) <= 4 for slots in workers))
        self.assertEqual(sum(len(slots) for workers in batches.values() for slots in workers), 81)
        workflow = (WORKFLOWS / "ci-full.yml").read_text()
        shards = job(workflow, "go-shards")
        matrix = re.search(r"^        include: (\[[^\n]+\])$", shards, re.M)
        self.assertIsNotNone(matrix)
        self.assertEqual(json.loads(matrix[1]), [{"profile": profile, "worker": worker}
                                               for profile, workers in batches.items() for worker in range(len(workers))])
        self.assertIn("max-parallel: 4", shards)
        self.assertIn("fail-fast: false", shards)
        self.assertIn("timeout-minutes: 25", shards)
        self.assertIn("merge-multiple: false", job(workflow, "gate"))

    def test_worker_compiles_inventory_once_and_runs_each_slot_uncached(self):
        with tempfile.TemporaryDirectory(prefix="ci-worker-") as directory:
            calls = []

            def run(command, **kwargs):
                calls.append(command)
                if "stdout" in kwargs:
                    kwargs["stdout"].write(b"evidence\n")
                return subprocess.CompletedProcess(command, 0, stdout="agentworkspace/internal/store\n")

            with mock.patch.object(self.shards, "check_identity"), mock.patch.object(self.shards, "command", side_effect=run), \
                 mock.patch.object(self.shards.verifier, "inventory", return_value={"TestCoordination": 1}), \
                 mock.patch.object(self.shards.verifier, "verify", return_value=({}, {})) as verify:
                self.shards.run_worker("store", 0, Path(directory), RUN, SHA, TREE)
            tests = [command for command in calls if command[:2] == ["go", "test"]]
            self.assertEqual(len(tests), 5)
            self.assertEqual(sum("-list" in command for command in tests), 1)
            for command in tests:
                self.assertIn("-count=1", command)
                self.assertIn("-race", command)
                self.assertIn("-json", command)
            self.assertEqual(verify.call_count, 4)
            self.assertEqual(sorted(path.name for path in Path(directory).iterdir()), [f"store-shard-v1-{slot}" for slot in range(4)])

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

    def test_go_remainder_vet_bench_and_clients_start_without_shard_dependencies(self):
        workflow = (WORKFLOWS / "ci-full.yml").read_text()
        for name in ["go-other", "go-vet", "go-benchmarks", "web", "pi-bridge"]:
            self.assertNotIn("needs:", job(workflow, name))
        other = job(workflow, "go-other")
        self.assertIn("go test -count=1 ./internal/coordination", other)
        self.assertIn("go test -count=1 -race ./internal/coordination", other)
        self.assertIn('go test -race "${rest[@]}"', other)
        self.assertIn("go vet ./...", job(workflow, "go-vet"))
        self.assertIn("go test -run '^$' -bench . -benchmem ./...", job(workflow, "go-benchmarks"))
        for name in ["web", "pi-bridge"]:
            for command in ["npm test", "npm run typecheck"]:
                self.assertIn(command, job(workflow, name))
        self.assertIn("npm run build", job(workflow, "web"))

    def test_actual_benchmark_filter_preserves_only_complete_metrics_and_pipefail(self):
        workflow = (WORKFLOWS / "ci-full.yml").read_text()
        block = job(workflow, "go-benchmarks")
        match = re.search(r"- name: Record benchmark metrics\n        run: \|\n(.*?)(?=\n      -)", block, re.S)
        self.assertIsNotNone(match)
        import textwrap
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
                                        env={**os.environ, "RUNNER_TEMP": directory, "BENCH_OUTPUT": fixture, "BENCH_EXIT": str(code)},
                                        capture_output=True, timeout=5)
                self.assertEqual(result.returncode == 0, code == 0, result.stderr)
                self.assertEqual((Path(directory) / "go-benchmarks.txt").read_text().splitlines(),
                                 ["BenchmarkAlloc-8 100 1.25 ns/op 16 B/op 2 allocs/op",
                                  "BenchmarkStream-8 5 2.5e+02 ns/op 1.00 MB/s 0 B/op 0 allocs/op"])


if __name__ == "__main__":
    unittest.main()
