#!/usr/bin/env python3
"""Local scheduling, routing, batching and fail-closed gate regressions; no product tests."""
from collections import Counter
from concurrent.futures import ThreadPoolExecutor
from contextlib import redirect_stderr, redirect_stdout
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
import threading
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


def step(text, name):
    match = re.search(rf"^      - name: {re.escape(name)}\n(.*?)(?=^      - |\Z)", text, re.M | re.S)
    if not match:
        raise AssertionError("missing step: " + name)
    return match.group(1)


def scalar_expression(text, values):
    # Evaluate only the routing/status subset used by these workflows.
    for key, value in sorted(values.items(), key=lambda item: len(item[0]), reverse=True):
        text = text.replace(key, repr(value))
    text = re.sub(r"!(?!=)", " not ", text.replace("&&", " and ").replace("||", " or "))
    functions = {"contains": lambda items, value: value.casefold() in [item.casefold() for item in items],
                 "fromJSON": json.loads, "format": lambda pattern, *args: pattern.format(*args)}
    return eval(text, {"__builtins__": {}, "null": None, **functions}, {})


def native_expression(text, event_name, event, run_id=RUN):
    pull_request = event.get("pull_request", {})
    # Actions string comparisons and contains are case-insensitive.
    return scalar_expression(text, {
        "github.event_name": event_name,
        "github.event.action": event.get("action"),
        "github.event.label.name": event.get("label", {}).get("name", "").casefold(),
        "github.event.pull_request.labels.*.name": [label["name"] for label in pull_request.get("labels", [])],
        "github.event.pull_request.number": pull_request.get("number"),
        "github.event.pull_request.draft": pull_request.get("draft"),
        "github.event.changes.base.ref.from": event.get("changes", {}).get("base", {}).get("ref", {}).get("from"),
        "github.ref": event.get("ref", "refs/pull/17/merge"),
        "github.workflow": "CI", "github.run_id": run_id,
    })


class RoutingTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.plan = load("ci-plan")

    def test_relevant_draft_and_ready_prs_default_to_feedback_with_explicit_full(self):
        relevant = [(action, {}) for action in ["opened", "reopened", "synchronize", "ready_for_review", "converted_to_draft"]]
        relevant.extend([("edited", {"changes": {"base": {"ref": {"from": "release"}}}}),
                         ("labeled", {"label": {"name": "ci:refresh"}}), ("labeled", {"label": {"name": "CI:REFRESH"}})])
        for draft in (True, False):
            for action, fields in relevant:
                for labels, expected in (([], "feedback"), ([{"name": "CI:FULL"}], "full")):
                    event = pr(action, draft, **fields)
                    event["pull_request"]["labels"] = labels
                    with self.subTest(action=action, draft=draft, fields=fields, labels=labels):
                        self.assertEqual(self.plan.mode("pull_request", event), expected)
            for label in ("ci:full", "CI:FULL"):
                self.assertEqual(self.plan.mode("pull_request", pr("labeled", draft, label={"name": label})), "full")
        self.assertEqual(self.plan.mode("push", {"ref": "refs/heads/master"}), "full")
        self.assertEqual(self.plan.mode("merge_group", {"action": "checks_requested"}), "ignore")

    def test_irrelevant_events_and_real_base_edits(self):
        events = [pr("edited", changes={"title": {"from": "old"}}),
                  pr("edited", changes={"body": {"from": "old"}}),
                  pr("edited", changes={"base": {"sha": {"from": "d" * 40}}}),
                  pr("labeled", label={"name": "bug"}), pr("labeled", False, label={"name": "bug"}),
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
            self.assertFalse(any(result[key] for key in ("go", "web", "bridge", "ci", "conservative")))
        for path, expected in [("web/src/App.tsx", "web"), ("extensions/pi-bridge/index.ts", "bridge"),
                               ("internal/store/task.go", "go"), ("cmd/workspace/main_test.go", "go")]:
            result = self.plan.classify([path])
            self.assertTrue(result[expected])
            self.assertFalse(result["conservative"])
        bridge = self.plan.classify(["extensions/pi-bridge/index.ts"])
        self.assertTrue(bridge["go"], "bridge changes also require Go wiring checks")
        both = self.plan.classify(["web/src/a.ts", "extensions/pi-bridge/config.ts", "internal/store/task.go"])
        self.assertTrue(all(both[key] for key in ("go", "web", "bridge")))
        for path in ["go.mod", "go.sum", "Makefile", "extensions/unknown/tool.js", ".github/unknown.md", "notes.rst",
                     ".github/workflows/other.yml", ".github/scripts/ci-new.py", ".github/scripts/ci-plan.py/extra"]:
            result = self.plan.classify([path])
            self.assertTrue(all(result[key] for key in ("go", "web", "bridge", "ci", "conservative")))

    def test_ci_policy_allowlist_is_exact_and_unions_with_product_paths(self):
        policies = {
            ".github/workflows/ci.yml", ".github/workflows/ci-full.yml",
            ".github/scripts/ci-plan.py", ".github/scripts/ci-shards.py", ".github/scripts/ci-gate.py",
            ".github/scripts/test-ci-scheduling.py", ".github/scripts/test-store-shard.py", ".github/scripts/verify-store-shard.py",
        }
        self.assertEqual(self.plan.CI_PATHS, policies)
        for policy in policies:
            with self.subTest(policy=policy):
                selected = self.plan.classify(["docs/design.md", policy])
                self.assertTrue(selected["ci"])
                self.assertFalse(any(selected[key] for key in ("docs", "go", "web", "bridge", "conservative")))
                for products in (["internal/scheduler/task.go"], ["internal/store/testdata/input.json"],
                                 ["web/src/App.tsx"], ["extensions/pi-bridge/index.ts"],
                                 ["internal/provider/provider.go", "web/src/App.tsx"], ["go.mod"]):
                    expected = {**self.plan.classify(products), "ci": True}
                    self.assertEqual(self.plan.classify([policy, *products]), expected)
                    self.assertEqual(self.plan.classify([*products, policy]), expected)

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
                             ([".github/workflows/ci.yml"], []),
                             ([".github/scripts/ci-plan.py", "internal/scheduler/task.go"], []),
                             ([".github/workflows/ci-full.yml", "internal/store/task.go"], ["store"]),
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
            for paths in (["docs/a.md"], ["web/src/a.ts"], ["extensions/pi-bridge/index.ts"], ["Makefile"],
                          [".github/workflows/ci.yml"], [".github/scripts/ci-plan.py", "internal/scheduler/task.go"],
                          [".github/scripts/ci-shards.py", "web/src/a.ts"],
                          [".github/scripts/ci-gate.py", "extensions/pi-bridge/index.ts"]):
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
        ancestor, _ = self.commit_object({"docs/a.md": "old\n"})
        head, _ = self.commit_object({"docs/a.md": "new\n"}, [ancestor])
        base, _ = self.commit_object({"docs/a.md": "old\n", "web/src/base.ts": "base only\n"}, [ancestor])
        merge_tree = self.git("merge-tree", "--write-tree", base, head).decode().strip()
        merge, tree = self.commit_object({"docs/a.md": "new\n", "web/src/base.ts": "base only\n"}, [base, head])
        self.assertEqual(tree, merge_tree)
        self.git("checkout", "--quiet", "--detach", merge)
        self.assertEqual((self.root / "web/src/base.ts").read_text(), "base only\n")
        event = pr()
        event["pull_request"]["head"]["sha"] = head
        event["pull_request"]["base"]["sha"] = ancestor
        result = self.plan.plan("pull_request", event, merge, self.root)
        self.assertEqual((result["sha"], result["tree"], result["base"]), (merge, tree, base))
        self.assertNotEqual(base, event["pull_request"]["base"]["sha"])
        self.assertTrue(result["docs"])
        self.assertFalse(result["web"], "base-only changes must not expand PR feedback")
        self.assertEqual(result["head"], head)
        self.assertNotIn("bucket", result)
        event_file, output, selected_file = (self.root / name for name in ("event.json", "outputs", "plan.json"))
        event_file.write_text(json.dumps(event))
        process = subprocess.run([sys.executable, str(SCRIPTS / "ci-plan.py"), "plan", str(selected_file)], cwd=self.root,
                                 env={**os.environ, "GITHUB_EVENT_PATH": str(event_file), "GITHUB_OUTPUT": str(output),
                                      "GITHUB_EVENT_NAME": "pull_request", "GITHUB_SHA": merge, "GITHUB_RUN_ID": RUN}, capture_output=True, timeout=5)
        self.assertEqual(process.returncode, 0, process.stderr)
        self.assertEqual(json.loads(selected_file.read_text()), result)
        self.assertEqual(set(line.split("=", 1)[0] for line in output.read_text().splitlines()),
                         {"mode", "sha", "tree", "base", "head", "matrix", "run_id", "go", "web", "bridge", "ci", "conservative"})
        for bad_sha in [head, "f" * 40, merge + "\nowned=true"]:
            with self.assertRaises(ValueError):
                self.plan.plan("pull_request", event, bad_sha, self.root)
        event["pull_request"]["head"]["sha"] = "f" * 40
        with self.assertRaises(ValueError):
            self.plan.plan("pull_request", event, merge, self.root)

    def test_stacked_ci_backport_and_go_diff_do_not_expand_product_selection(self):
        _, _, merge, _, event = self.fixture({".github/workflows/ci.yml": "old"},
                                             {".github/workflows/ci.yml": "new", "internal/scheduler/task.go": "new"})
        for draft, labels, expected_mode in ((True, [], "feedback"), (False, [], "feedback"),
                                              (True, [{"name": "ci:full"}], "full"), (False, [{"name": "ci:full"}], "full")):
            event["pull_request"].update(draft=draft, labels=labels)
            selected = self.plan.plan("pull_request", event, merge, self.root)
            self.assertEqual(selected["mode"], expected_mode)
            self.assertTrue(selected["ci"] and selected["go"])
            self.assertFalse(selected["web"] or selected["bridge"] or selected["conservative"])
            self.assertEqual(selected["directories"], ["internal/scheduler"])

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
        event["pull_request"].update(draft=False, labels=[{"name": "ci:full"}])
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

    def test_master_push_plan_preserves_pushed_commit_identity_and_rejects_other_events(self):
        base, _, pushed, tree, _ = self.fixture({"PRD.md": "old"}, {"PRD.md": "new"})
        selected = self.plan.plan("push", {"ref": "refs/heads/master"}, pushed, self.root)
        self.assertEqual({key: selected[key] for key in ("mode", "sha", "tree", "head", "base")},
                         {"mode": "full", "sha": pushed, "tree": tree, "head": pushed, "base": base})
        self.assertNotIn("bucket", selected)
        for name, event in [("push", {"ref": "refs/heads/feature"}),
                            ("merge_group", {"action": "checks_requested", "merge_group": {"head_sha": pushed}})]:
            with self.subTest(event_name=name), self.assertRaises(ValueError):
                self.plan.plan(name, event, pushed, self.root)

    def test_reject_nonmerge_identity_and_missing_parent(self):
        head, _ = self.commit_object({"PRD.md": "text"})
        self.git("update-ref", "HEAD", head)
        with self.assertRaises(ValueError):
            self.plan.plan("pull_request", pr(), head, self.root)


class AdmissionTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.workflow = (WORKFLOWS / "ci.yml").read_text()

    def test_relevance_is_shared_with_supersession_without_full_admission_queue(self):
        relevant = expression(job(self.workflow, "feedback"), "if")
        concurrency = self.workflow.split("jobs:", 1)[0]
        group = expression(concurrency, "group")
        cancel = expression(concurrency, "cancel-in-progress")
        self.assertIn(relevant, group)
        full = job(self.workflow, "full")
        for text in ["uses: ./.github/workflows/ci-full.yml", "needs: feedback", "needs.feedback.outputs.mode == 'full'"]:
            self.assertIn(text, full)
        self.assertNotIn("runs-on:", full)
        self.assertNotIn("concurrency:", full)
        self.assertNotIn("concurrency:", (WORKFLOWS / "ci-full.yml").read_text())
        self.assertNotIn("queue:", self.workflow)
        self.assertNotIn("bucket", self.workflow)
        self.assertFalse(hasattr(load("ci-plan"), "bucket"))
        events = [("push", {"ref": "refs/heads/master"}, "full"),
                  ("push", {"ref": "refs/heads/feature"}, "ignore"),
                  ("merge_group", {"action": "checks_requested"}, "ignore"),
                  ("workflow_dispatch", {}, "ignore"), ("issues", {"action": "opened"}, "ignore")]
        for draft in (True, False):
            for action in ("opened", "reopened", "synchronize", "ready_for_review", "converted_to_draft"):
                events.append(("pull_request", pr(action, draft), "feedback"))
                event = pr(action, draft)
                event["pull_request"]["labels"] = [{"name": "CI:FULL"}]
                events.append(("pull_request", event, "full"))
            for label, expected in (("ci:refresh", "feedback"), ("CI:REFRESH", "feedback"),
                                    ("ci:full", "full"), ("CI:FULL", "full"), ("bug", "ignore")):
                events.append(("pull_request", pr("labeled", draft, label={"name": label}), expected))
            events.append(("pull_request", pr("edited", draft, changes={"base": {"ref": {"from": "release"}}}), "feedback"))
            for fields in ({"title": {"from": "old"}}, {"body": {"from": "old"}}, {"base": {"sha": {"from": SHA}}}):
                event = pr("edited", draft, changes=fields)
                event["pull_request"]["labels"] = [{"name": "ci:full"}]
                events.append(("pull_request", event, "ignore"))
            event = pr("labeled", draft, label={"name": "bug"})
            event["pull_request"]["labels"] = [{"name": "ci:full"}]
            events.append(("pull_request", event, "ignore"))
        outer = job(self.workflow, "merge-check")
        condition = expression(outer, "if")
        candidate = condition[len("!cancelled() && ("):-1]
        name = expression(outer, "name")
        for event_name, event, tier in events:
            with self.subTest(event_name=event_name, event=event):
                self.assertEqual(native_expression(relevant, event_name, event), tier != "ignore")
                self.assertEqual(native_expression(candidate, event_name, event), tier == "full")
                group1 = native_expression(group, event_name, event)
                group2 = native_expression(group, event_name, event, run_id="999")
                if tier == "ignore":
                    self.assertNotEqual(group1, group2)
                    self.assertNotEqual(group1, native_expression(group, "pull_request", pr()))
                    self.assertNotEqual(group1, native_expression(group, "push", {"ref": "refs/heads/master"}))
                else:
                    self.assertEqual(group1, group2)
                    self.assertEqual(native_expression(cancel, event_name, event), event_name == "pull_request")
                values = {candidate: tier == "full", "needs.feedback.result": "success", "needs.full.result": "success"}
                self.assertEqual(scalar_expression(name, values), "Full CI" if tier == "full" else "Feedback status")
        pr_group = native_expression(group, "pull_request", pr())
        for event_name, event, tier in events:
            if event_name == "pull_request" and tier != "ignore":
                self.assertEqual(native_expression(group, event_name, event), pr_group)
        other = pr()
        other["pull_request"]["number"] = 18
        self.assertNotEqual(native_expression(group, "pull_request", other), pr_group)
        self.assertNotEqual(native_expression(group, "push", {"ref": "refs/heads/master"}), pr_group)

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

    def test_ci_policy_feedback_runs_both_explicit_modules_and_fails_on_either(self):
        feedback = job(self.workflow, "feedback")
        block = step(feedback, "Test CI scheduling and shard gates")
        condition = expression(block, "if")
        self.assertEqual(condition, "steps.plan.outputs.mode == 'feedback' && steps.plan.outputs.ci == 'true'")
        ordinary = expression(step(feedback, "Ordinary test cmd package"), "if")
        plan = load("ci-plan")
        for mode in ("feedback", "full"):
            for paths, ci, go in [(["docs/a.md"], False, False), (["internal/scheduler/task.go"], False, True),
                                  ([".github/workflows/ci.yml"], True, False),
                                  ([".github/scripts/ci-shards.py", "internal/scheduler/task.go"], True, True),
                                  ([".github/scripts/ci-plan.py", "web/src/a.ts"], True, False),
                                  ([".github/scripts/ci-gate.py", "extensions/pi-bridge/index.ts"], True, True),
                                  ([".github/scripts/unknown.py"], True, True)]:
                selected = {"mode": mode, **plan.classify(paths)}
                values = {"steps.plan.outputs." + key: json.dumps(value) if type(value) is bool else value
                          for key, value in selected.items()}
                with self.subTest(mode=mode, paths=paths):
                    self.assertEqual(scalar_expression(condition, values), mode == "feedback" and ci)
                    self.assertEqual(scalar_expression(ordinary, values), mode == "full" or go)
        script = textwrap.dedent(re.search(r"        run: \|\n(.*)", block, re.S)[1])
        commands = [".github/scripts/test-ci-scheduling.py -v", ".github/scripts/test-store-shard.py -v"]
        stub = '''python3() {
          printf '%s\\n' "$*"
          [[ "$*" != "$CI_TEST_FAIL" ]]
        }
        readonly -f python3
        '''
        for failure in ("", *commands):
            result = subprocess.run(["bash", "-c", stub + script],
                                    env={**os.environ, "CI_TEST_FAIL": failure}, text=True, capture_output=True, timeout=5)
            self.assertEqual(result.returncode == 0, not failure, result.stderr)
            self.assertEqual(result.stdout.splitlines(), commands[:1] if failure == commands[0] else commands)
        self.assertNotIn("unittest discover", block)

    def test_outer_gate_cannot_disappear_with_missing_preflight_outputs(self):
        gate = job(self.workflow, "merge-check")
        self.assertIn('needs: ["feedback", "full"]', gate)
        condition = expression(gate, "if")
        self.assertTrue(condition.startswith("!cancelled() && ("))
        self.assertNotIn("needs.", condition, "candidate detection must survive preflight failure or skips")
        candidate = condition[len("!cancelled() && ("):-1]
        self.assertTrue(expression(gate, "name").startswith(candidate + " && "))
        self.assertIn(expression(job(self.workflow, "feedback"), "if"), candidate)
        self.assertNotIn("github.event.pull_request.draft", candidate)
        self.assertIn("contains(github.event.pull_request.labels.*.name, 'ci:full')", candidate)
        self.assertIn("ci-gate.py outer", gate)
        self.assertNotIn("setup-go", gate)
        self.assertNotIn("setup-node", gate)

    def test_cancelled_workflows_skip_gates_but_live_failures_keep_a_failing_required_check(self):
        outer = job(self.workflow, "merge-check")
        condition = expression(outer, "if")
        name = expression(outer, "name")
        candidate = condition[len("!cancelled() && ("):-1]
        evidence = expression(job((WORKFLOWS / "ci-full.yml").read_text(), "gate"), "if")
        self.assertEqual(evidence, "!cancelled()")
        cases = [("success", "success", "Full CI"), ("failure", "skipped", "Full CI"),
                 ("success", "failure", "Full CI"), ("skipped", "skipped", "Full CI"), ("", "", "Full CI"),
                 ("cancelled", "skipped", "Feedback status"), ("cancelled", "success", "Feedback status"),
                 ("success", "cancelled", "Feedback status"), ("failure", "cancelled", "Feedback status"),
                 ("success", "skipped", "Feedback status")]
        for feedback, full, expected_name in cases:
            for cancelled, is_candidate in itertools.product((False, True), repeat=2):
                values = {candidate: is_candidate, "cancelled()": cancelled,
                          "needs.feedback.result": feedback, "needs.full.result": full}
                with self.subTest(feedback=feedback, full=full, cancelled=cancelled, candidate=is_candidate):
                    self.assertEqual(scalar_expression(condition, values), not cancelled and is_candidate)
                    self.assertEqual(scalar_expression(evidence, values), not cancelled)
                    self.assertEqual(scalar_expression(name, values), expected_name if is_candidate else "Feedback status")
        self.assertNotIn("always()", outer)

    def test_trigger_and_security_contract(self):
        for action in ["opened", "reopened", "synchronize", "ready_for_review", "converted_to_draft", "edited", "labeled"]:
            self.assertIn(action, self.workflow.split("permissions:", 1)[0])
        triggers = self.workflow.split("permissions:", 1)[0]
        def trigger_keys(text):
            active = "\n".join(line.split("#", 1)[0].rstrip() for line in text.splitlines())
            block = re.search(r"^on:\n(.*?)(?=^\S|\Z)", active, re.M | re.S)[1]
            return set(re.findall(r"^  ([a-z_]+):", block, re.M))

        self.assertEqual(trigger_keys(triggers), {"pull_request"})
        rollback = "  # push: {branches: [master]}"
        self.assertEqual(triggers.splitlines().count(rollback), 1)
        restored = triggers.replace(rollback, rollback.replace("# ", "", 1))
        self.assertEqual(trigger_keys(restored), {"pull_request", "push"})
        self.assertIn("  push: {branches: [master]}", restored)
        pr_trigger = triggers.split("  pull_request:", 1)[1].split(rollback, 1)[0]
        self.assertNotIn("merge_group:", triggers)
        self.assertNotIn("branches:", pr_trigger)
        self.assertNotRegex(triggers, r"(?m)^ +paths(?:-ignore)?:")
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

    def test_exact_81_logical_slots_in_24_bounded_workers_and_generated_matrix(self):
        batches = self.shards.BATCHES
        self.assertEqual({name: len(workers) for name, workers in batches.items()}, {"store": 16, "platform": 6, "httpapi": 2})
        self.assertEqual(batches["platform"], ((4,), (8, 0), (1,), (3, 5), (6, 7), (2,)))
        self.assertEqual(batches["store"], tuple(tuple(range(start, start + 4)) for start in range(0, 64, 4)))
        self.assertEqual(batches["httpapi"], ((0, 1, 2, 3), (4, 5, 6, 7)))
        for profile, workers in batches.items():
            flat = list(itertools.chain.from_iterable(workers))
            self.assertEqual(sorted(flat), list(range(self.verifier.PROFILES[profile][0])))
            self.assertTrue(all(0 < len(slots) <= (2 if profile == "platform" else 4) for slots in workers))
        self.assertEqual(sum(len(slots) for workers in batches.values() for slots in workers), 81)
        plan = load("ci-plan")
        with mock.patch.object(os, "sched_getaffinity", side_effect=AssertionError("matrix must not inspect host CPUs"), create=True):
            matrix = plan.shard_matrix()
        self.assertCountEqual(matrix["include"], [{"profile": profile, "worker": worker}
                                                 for profile, workers in batches.items() for worker in range(len(workers))])
        expected_order = {"platform": list(range(6)),
                          "store": [1, 2, 9, 12, 14, 15, 0, 3, 4, 5, 6, 7, 8, 10, 11, 13],
                          "httpapi": list(range(2))}
        self.assertEqual(matrix, {"include": [{"profile": profile, "worker": worker}
                                              for profile, order in expected_order.items() for worker in order]})
        self.assertEqual(len(matrix["include"]), 24)
        self.assertEqual(matrix["include"][:2], [{"profile": "platform", "worker": 0}, {"profile": "platform", "worker": 1}])
        self.assertIn(4, batches["platform"][0])
        self.assertIn(8, batches["platform"][1])
        workflow = (WORKFLOWS / "ci-full.yml").read_text()
        shards = job(workflow, "go-shards")
        for text in ["matrix: ${{ fromJSON(inputs.matrix) }}", "max-parallel: 7", "fail-fast: false", "timeout-minutes: 40"]:
            self.assertIn(text, shards)
        self.assertIn("merge-multiple: false", job(workflow, "gate"))

    def slot_names(self, profile, slot, count):
        names = []
        for index in itertools.count():
            name = f"TestVector{index}"
            if self.verifier.selection({name: 1}, slot, profile)[0]:
                names.append(name)
                if len(names) == count:
                    return list(reversed(names))

    def worker_go(self, names, mutate=None):
        def go(args, **kwargs):
            package = "agentworkspace/" + args[-1].removeprefix("./")
            if args[:2] == ["go", "list"]:
                return subprocess.CompletedProcess(args, 0, stdout=package + "\n")
            self.assertEqual(args[:2], ["go", "test"])
            rows = [{"Action": "start", "Package": package}]
            if "-list" in args:
                rows.append({"Action": "output", "Package": package,
                             "Output": "\n".join(names) + f"\nok  \t{package}\t0.001s\n"})
            else:
                pattern = args[args.index("-run") + 1]
                for name in names:
                    if re.fullmatch(pattern, name):
                        rows.extend([{"Action": "run", "Package": package, "Test": name},
                                     {"Action": "pass", "Package": package, "Test": name}])
            rows.append({"Action": "pass", "Package": package})
            if mutate and "-run" in args:
                mutate(rows)
            kwargs["stdout"].write("".join(json.dumps(row) + "\n" for row in rows).encode())
            return subprocess.CompletedProcess(args, 0)
        return go

    def test_platform_45_and_48_roots_have_balanced_deterministic_verified_subsets(self):
        names = self.slot_names("platform", 1, 45) + self.slot_names("platform", 2, 48)
        compiled = Counter(names)
        for worker, slot, sizes in [(2, 1, [23, 22]), (5, 2, [24, 24])]:
            selected, _ = self.verifier.selection(compiled, slot, "platform")
            ordered = sorted(selected)
            expected_subsets = [Counter({name: 1 for name in ordered[index::2]}) for index in range(2)]
            # Reversing the compiled-list order must not change subprocess selections.
            for inventory in (names, list(reversed(names))):
                with self.subTest(slot=slot, first=inventory[0]), tempfile.TemporaryDirectory(prefix="ci-platform-") as directory, \
                     redirect_stdout(io.StringIO()), mock.patch.object(self.shards, "check_identity"), \
                     mock.patch.object(self.shards, "command", side_effect=self.worker_go(inventory)) as command, \
                     mock.patch.object(self.shards.verifier, "verify", wraps=self.shards.verifier.verify) as verify:
                    self.shards.run_worker("platform", worker, directory, RUN, SHA, TREE)
                    calls = [call.args[0] for call in command.call_args_list]
                    self.assertEqual(sum("-list" in args for args in calls), 1)
                    runs = [args for args in calls if "-run" in args]
                    self.assertEqual(len(runs), 2)
                    actual_patterns = [args[args.index("-run") + 1] for args in runs]
                    run_calls = [call for call in command.call_args_list if "-run" in call.args[0]]
                    self.assertEqual(len({call.kwargs["stdout"].name for call in run_calls}), 2)
                    self.assertEqual(len({call.kwargs["env"]["TMPDIR"] for call in run_calls}), 1)
                    self.assertEqual(actual_patterns, ["^(" + "|".join(sorted(subset)) + ")$" for subset in expected_subsets])
                    self.assertEqual([len(subset) for subset in expected_subsets], sizes)
                    for args in runs:
                        self.assertEqual(args, ["go", "test", "-count=1", "-race", "-json", "-run",
                                                args[args.index("-run") + 1], "./internal/platform"])
                    self.assertEqual([call.args[2] for call in verify.call_args_list], expected_subsets)
                    self.assertTrue(all(call.args[1] == "agentworkspace/internal/platform" for call in verify.call_args_list))
                    self.assertFalse(expected_subsets[0].keys() & expected_subsets[1].keys())
                    self.assertEqual(sum(expected_subsets, Counter()), selected)
                    artifact = Path(directory) / f"platform-shard-v1-{slot}"
                    self.assertEqual(list(Path(directory).iterdir()), [artifact])
                    self.assertEqual([path.name for path in artifact.iterdir()], ["platform-summary.json"])
                    summary = json.loads((artifact / "platform-summary.json").read_text())
                    self.assertEqual(summary, {"schema": 1, "slot": slot, "total": 9, "run_id": RUN,
                                              "checkout_sha": SHA, "checkout_tree": TREE,
                                              "package": "agentworkspace/internal/platform", "compiled": compiled,
                                              "selected": selected, "runs": selected, "passes": selected, "skips": {}})

    def test_platform_subset_boundaries_and_exact_union(self):
        for count in (0, 1, 24, 25, 45, 48, 49, 73):
            selected = Counter({f"TestVector{index}": 1 for index in reversed(range(count))})
            groups = max(1, (count + 23) // 24)
            ordered = sorted(selected)
            expected = [Counter({name: selected[name] for name in ordered[index::groups]}) for index in range(groups)]
            with self.subTest(count=count):
                subsets = self.shards.execution_subsets(selected, "platform")
                self.assertEqual(subsets, expected)
                self.assertTrue(all(len(subset) <= 24 for subset in subsets))
                self.assertLessEqual(max(map(len, subsets)) - min(map(len, subsets)), 1)
                self.assertEqual(sum(subsets, Counter()), selected)
                for first, second in itertools.combinations(subsets, 2):
                    self.assertFalse(first.keys() & second.keys())

    def test_missing_or_overlapping_subsets_cannot_create_logical_receipt(self):
        names = self.slot_names("platform", 1, 45)
        ordered = sorted(names)
        first, second = [Counter({name: 1 for name in ordered[index::2]}) for index in range(2)]
        for subsets in ([first], [first, first, second]):
            with self.subTest(sizes=list(map(len, subsets))), tempfile.TemporaryDirectory(prefix="ci-subset-union-") as directory, \
                 redirect_stdout(io.StringIO()), mock.patch.object(self.shards, "check_identity"), \
                 mock.patch.object(self.shards, "command", side_effect=self.worker_go(names)), \
                 mock.patch.object(self.shards, "execution_subsets", return_value=subsets):
                with self.assertRaises(ValueError):
                    self.shards.run_worker("platform", 2, directory, RUN, SHA, TREE)
                self.assertEqual(list(Path(directory).iterdir()), [])

    def test_shared_run_pattern_preserves_escaping_and_byte_limit(self):
        self.assertEqual(self.verifier.run_pattern({}), "^$")
        self.assertEqual(self.verifier.run_pattern({"TestA.B": 1, "FuzzBytes": 1}), r"^(FuzzBytes|TestA\.B)$")
        boundary = "Test" + "A" * (self.verifier.MAX_PATTERN_BYTES - 8)
        self.assertEqual(len(self.verifier.run_pattern({boundary: 1}).encode("utf-8")), self.verifier.MAX_PATTERN_BYTES)
        for name in (boundary + "A", "Test" + "界" * 20000, "TestRoot/child"):
            with self.subTest(name=name[:20]), self.assertRaises(ValueError):
                self.verifier.run_pattern({name: 1})

    def test_platform_small_and_empty_slots_and_other_profiles_keep_canonical_runs(self):
        for profile, worker, slot, count in [("platform", 2, 1, 1), ("platform", 2, 1, 24),
                                             ("platform", 2, 1, 0), ("store", 0, 0, 48), ("httpapi", 0, 0, 48)]:
            names = self.slot_names(profile, slot, count) if count else self.slot_names(profile, 0, 1)
            compiled = Counter(names)
            with self.subTest(profile=profile, count=count), tempfile.TemporaryDirectory(prefix="ci-canonical-") as directory, \
                 redirect_stdout(io.StringIO()), mock.patch.object(self.shards, "check_identity"), \
                 mock.patch.object(self.shards, "command", side_effect=self.worker_go(names)) as command, \
                 mock.patch.object(self.shards, "worker_parallelism", return_value=1), \
                 mock.patch.object(self.shards.verifier, "verify", wraps=self.shards.verifier.verify) as verify:
                self.shards.run_worker(profile, worker, directory, RUN, SHA, TREE)
                runs = [call.args[0] for call in command.call_args_list if "-run" in call.args[0]]
                expected = [self.verifier.selection(compiled, logical, profile) for logical in self.shards.BATCHES[profile][worker]]
                self.assertEqual(runs, [["go", "test", "-count=1", "-race", "-json", "-run", pattern, "./internal/" + profile]
                                        for _, pattern in expected])
                self.assertEqual([call.args[2] for call in verify.call_args_list], [selected for selected, _ in expected])
                total, package, summary_name, prefix = self.verifier.PROFILES[profile]
                for logical, (selected, _) in zip(self.shards.BATCHES[profile][worker], expected):
                    summary = json.loads((Path(directory) / f"{prefix}{logical}" / summary_name).read_text())
                    self.assertEqual(summary, {"schema": 1, "slot": logical, "total": total, "run_id": RUN,
                                              "checkout_sha": SHA, "checkout_tree": TREE, "package": package,
                                              "compiled": compiled, "selected": selected,
                                              "runs": selected, "passes": selected, "skips": {}})

    def test_invalid_second_platform_subset_cannot_create_logical_receipt(self):
        names = self.slot_names("platform", 1, 45)
        for problem in ("missing", "duplicate", "fail", "skip", "child-fail", "child-skip", "unfinished-child",
                        "missing-start", "missing-package-pass", "package-fail", "package-skip", "foreign-package", "extra-lifecycle"):
            invocation = itertools.count(1)

            def mutate(rows):
                if next(invocation) != 2:
                    return
                if problem == "missing":
                    del rows[1:3]
                elif problem == "duplicate":
                    rows[1:1] = rows[1:3]
                elif problem in ("fail", "skip"):
                    rows[2]["Action"] = problem
                elif problem in ("child-fail", "child-skip", "unfinished-child"):
                    child = {**rows[1], "Test": rows[1]["Test"] + "/child"}
                    rows[2:2] = [child] if problem == "unfinished-child" else [child, {**child, "Action": problem[6:]}]
                elif problem == "missing-start":
                    del rows[0]
                elif problem == "missing-package-pass":
                    rows.pop()
                elif problem in ("package-fail", "package-skip"):
                    rows[-1]["Action"] = problem[8:]
                elif problem == "foreign-package":
                    rows[0]["Package"] = "other/platform"
                else:
                    rows.extend([rows[0], rows[-1]])

            with self.subTest(problem=problem), tempfile.TemporaryDirectory(prefix="ci-invalid-subset-") as directory, \
                 redirect_stdout(io.StringIO()), mock.patch.object(self.shards, "check_identity"), \
                 mock.patch.object(self.shards, "command", side_effect=self.worker_go(names, mutate)):
                with self.assertRaises(ValueError):
                    self.shards.run_worker("platform", 2, directory, RUN, SHA, TREE)
                self.assertEqual(list(Path(directory).iterdir()), [])

    def test_slow_store_slot_subsets_preserve_exact_disjoint_union(self):
        for count in (0, 1, 5, 6, 18):
            selected = Counter({f"TestStoreVector{index}": 1 for index in reversed(range(count))})
            limit = self.shards.STORE_SLOW_RUN_LIMIT
            groups = max(1, (count + limit - 1) // limit)
            ordered = sorted(selected)
            expected = [Counter({name: selected[name] for name in ordered[index::groups]}) for index in range(groups)]
            with self.subTest(count=count):
                subsets = self.shards.execution_subsets(selected, "store", 62)
                self.assertEqual(subsets, expected)
                self.assertTrue(all(len(subset) <= limit for subset in subsets))
                self.assertLessEqual(max(map(len, subsets)) - min(map(len, subsets)), 1)
                self.assertEqual(sum(subsets, Counter()), selected)
                for first, second in itertools.combinations(subsets, 2):
                    self.assertFalse(first.keys() & second.keys())
                self.assertEqual(self.shards.execution_subsets(selected, "store", 0), [selected])

        slot60 = self.slot_names("store", 60, 1)
        slot61 = self.slot_names("store", 61, 1)
        slot62 = self.slot_names("store", 62, 18)
        slot63 = self.slot_names("store", 63, 1)
        names = slot60 + slot61 + slot62 + slot63
        compiled = Counter(names)
        with tempfile.TemporaryDirectory(prefix="ci-slow-store-") as directory, \
             redirect_stdout(io.StringIO()), mock.patch.object(self.shards, "check_identity"), \
             mock.patch.object(self.shards, "command", side_effect=self.worker_go(names)) as command, \
             mock.patch.object(self.shards, "worker_parallelism", return_value=1):
            self.shards.run_worker("store", 15, directory, RUN, SHA, TREE)
            runs = [call.args[0] for call in command.call_args_list if "-run" in call.args[0]]
            expected_patterns = []
            for logical in self.shards.BATCHES["store"][15]:
                selected, _ = self.verifier.selection(compiled, logical, "store")
                for subset in self.shards.execution_subsets(selected, "store", logical):
                    expected_patterns.append(self.verifier.run_pattern(subset))
            self.assertEqual(len(runs), 7)
            self.assertEqual(runs, [["go", "test", "-count=1", "-race", "-json", "-run", pattern, "./internal/store"]
                                    for pattern in expected_patterns])
            for logical in self.shards.BATCHES["store"][15]:
                selected, _ = self.verifier.selection(compiled, logical, "store")
                summary = json.loads((Path(directory) / f"store-shard-v1-{logical}" / "store-summary.json").read_text())
                self.assertEqual(summary, {"schema": 1, "slot": logical, "total": 64, "run_id": RUN,
                                          "checkout_sha": SHA, "checkout_tree": TREE, "package": "agentworkspace/internal/store",
                                          "compiled": compiled, "selected": selected,
                                          "runs": selected, "passes": selected, "skips": {}})

        ordered62 = sorted(slot62)
        first, second = [Counter({name: 1 for name in ordered62[index::2]}) for index in range(2)]
        for subsets in ([first], [first, first, second]):
            with self.subTest(bad_sizes=list(map(len, subsets))), tempfile.TemporaryDirectory(prefix="ci-slow-store-bad-") as directory, \
                 redirect_stdout(io.StringIO()), mock.patch.object(self.shards, "check_identity"), \
                 mock.patch.object(self.shards, "command", side_effect=self.worker_go(slot62)), \
                 mock.patch.object(self.shards, "worker_parallelism", return_value=1), \
                 mock.patch.object(self.shards, "execution_subsets", return_value=subsets):
                with self.assertRaises(ValueError):
                    self.shards.run_worker("store", 15, directory, RUN, SHA, TREE)
                self.assertEqual(list(Path(directory).iterdir()), [])

    def test_store_pairing_requires_available_cpus_and_excludes_entire_slow_groups(self):
        slow_slots = {6, 8, 38, 50, 51, 57, 62}
        self.assertEqual(self.shards.SLOW_STORE_SLOTS, slow_slots)
        for cpus in (set(), {0}, {2, 4, 6}, {2, 4, 6, 8}, set(range(8))):
            with mock.patch.object(self.shards.sys, "platform", "linux"), \
                 mock.patch.object(os, "sched_getaffinity", return_value=cpus, create=True):
                for profile, workers in self.shards.BATCHES.items():
                    for slots in workers:
                        with self.subTest(profile=profile, slots=slots, cpus=len(cpus)):
                            expected = 2 if profile == "store" and len(cpus) >= 4 and not slow_slots.intersection(slots) else 1
                            self.assertEqual(self.shards.worker_parallelism(profile, slots), expected)
        for error in (AttributeError, NotImplementedError, OSError):
            with self.subTest(error=error), mock.patch.object(self.shards.sys, "platform", "linux"), \
                 mock.patch.object(os, "sched_getaffinity", side_effect=error, create=True):
                self.assertEqual(self.shards.worker_parallelism("store", (44, 45, 46, 47)), 1)
        for platform in ("darwin", "win32"):
            with self.subTest(platform=platform), mock.patch.object(self.shards.sys, "platform", platform), \
                 mock.patch.object(os, "sched_getaffinity", return_value=set(range(8)), create=True) as affinity:
                self.assertEqual(self.shards.worker_parallelism("store", (44, 45, 46, 47)), 1)
                affinity.assert_not_called()

    def test_logical_slots_have_private_events_and_tmpdir_without_environment_mutation(self):
        names = [self.slot_names("store", slot, 1)[0] for slot in (44, 45, 46, 47)]
        before = dict(os.environ)
        with tempfile.TemporaryDirectory(prefix="ci-private-slot-") as directory, redirect_stdout(io.StringIO()), \
             mock.patch.object(self.shards, "check_identity"), \
             mock.patch.object(self.shards.sys, "platform", "linux"), \
             mock.patch.object(os, "sched_getaffinity", return_value=set(range(4)), create=True), \
             mock.patch.object(self.shards, "command", side_effect=self.worker_go(names)) as command:
            self.shards.run_worker("store", 11, directory, RUN, SHA, TREE)
            runs = [call for call in command.call_args_list if "-run" in call.args[0]]
            self.assertEqual(len(runs), 4)
            self.assertTrue(all("env" in call.kwargs and "TMPDIR" in call.kwargs["env"] for call in runs))
            self.assertEqual(len({call.kwargs["stdout"].name for call in runs}), 4)
            self.assertEqual(len({call.kwargs["env"]["TMPDIR"] for call in runs}), 4)
            self.assertEqual(len({id(call.kwargs["env"]) for call in runs}), 4)
        self.assertEqual(dict(os.environ), before)

    def test_store_concurrency_is_bounded_to_two_real_verified_slots(self):
        names = [self.slot_names("store", slot, 1)[0] for slot in (44, 45, 46, 47)]
        base_go = self.worker_go(names)
        barrier, lock = threading.Barrier(2, timeout=5), threading.Lock()
        active, peak, completed = 0, 0, []

        def go(args, **kwargs):
            nonlocal active, peak
            if "-run" not in args:
                return base_go(args, **kwargs)
            with lock:
                active += 1
                peak = max(peak, active)
            try:
                barrier.wait()
                self.assertTrue(Path(kwargs["env"]["TMPDIR"]).is_dir())
                self.assertTrue(Path(kwargs["stdout"].name).is_file())
                return base_go(args, **kwargs)
            finally:
                with lock:
                    active -= 1
                    completed.append(args)

        output = io.StringIO()
        with tempfile.TemporaryDirectory(prefix="ci-paired-store-") as directory, redirect_stdout(output), \
             mock.patch.object(self.shards, "check_identity"), \
             mock.patch.object(self.shards.sys, "platform", "linux"), \
             mock.patch.object(os, "sched_getaffinity", return_value=set(range(4)), create=True), \
             mock.patch.object(self.shards.subprocess, "run", side_effect=go) as command, \
             mock.patch.object(self.shards.verifier, "verify", wraps=self.shards.verifier.verify) as verify:
            self.shards.run_worker("store", 11, directory, RUN, SHA, TREE)
            self.assertEqual((peak, active, len(completed)), (2, 0, 4))
            self.assertEqual(sum("-list" in call.args[0] for call in command.call_args_list), 1)
            self.assertEqual(verify.call_count, 4)
            self.assertEqual(sum((call.args[2] for call in verify.call_args_list), Counter()), Counter(names))
            for slot in (44, 45, 46, 47):
                selected, _ = self.verifier.selection(Counter(names), slot, "store")
                summary = json.loads((Path(directory) / f"store-shard-v1-{slot}/store-summary.json").read_text())
                self.assertEqual(summary, {"schema": 1, "slot": slot, "total": 64, "run_id": RUN,
                                          "checkout_sha": SHA, "checkout_tree": TREE,
                                          "package": "agentworkspace/internal/store", "compiled": Counter(names),
                                          "selected": selected, "runs": selected, "passes": selected, "skips": {}})
        rows = [json.loads(line) for line in output.getvalue().splitlines() if line.startswith("{")]
        self.assertEqual(Counter(row["Test"] for row in rows if row["Action"] == "pass" and "Test" in row), Counter(names))
        # Each replay is a complete, contiguous package lifecycle, not merged task output.
        self.assertEqual([row["Action"] for row in rows], ["start", "run", "pass", "pass"] * 4)

    def test_store_error_joins_all_tasks_and_cannot_publish_worker_evidence(self):
        names = [self.slot_names("store", slot, 1)[0] for slot in (44, 45, 46, 47)]
        for problem in ("subprocess", "coverage"):
            failed, release = threading.Event(), threading.Event()
            completed = []

            def mutate(rows):
                if rows[1]["Test"] == names[0] and problem == "coverage":
                    del rows[1:3]

            base_go = self.worker_go(names, mutate)

            def go(args, **kwargs):
                if "-run" not in args:
                    return base_go(args, **kwargs)
                pattern = args[args.index("-run") + 1]
                failing = re.fullmatch(pattern, names[0]) is not None
                try:
                    if not failing:
                        self.assertTrue(release.wait(5))
                    result = base_go(args, **kwargs)
                    if failing:
                        failed.set()
                        if problem == "subprocess":
                            raise subprocess.CalledProcessError(1, args)
                    return result
                finally:
                    completed.append(args)

            diagnostics = io.StringIO()
            with self.subTest(problem=problem), tempfile.TemporaryDirectory(prefix="ci-failed-pair-") as directory, \
                 redirect_stdout(io.StringIO()), redirect_stderr(diagnostics), \
                 mock.patch.object(self.shards, "check_identity"), \
                 mock.patch.object(self.shards.sys, "platform", "linux"), \
                 mock.patch.object(os, "sched_getaffinity", return_value=set(range(4)), create=True), \
                 mock.patch.object(self.shards, "command", side_effect=go):
                root = Path(directory) / "workers"
                self.evidence(root)
                destination = root / "store-worker-v1-11"
                destination.rename(Path(directory) / "previous-worker")
                with ThreadPoolExecutor(max_workers=1) as executor:
                    future = executor.submit(self.shards.run_worker, "store", 11, destination, RUN, SHA, TREE)
                    try:
                        self.assertTrue(failed.wait(5))
                        self.assertFalse(future.done(), "worker returned while sibling subprocesses were blocked")
                    finally:
                        release.set()
                    with self.assertRaises(subprocess.CalledProcessError if problem == "subprocess" else ValueError):
                        future.result(timeout=5)
                self.assertEqual(len(completed), 4)
                self.assertEqual(list(destination.iterdir()), [])
                with self.assertRaises(ValueError):
                    self.shards.aggregate_workers(root, RUN, SHA, TREE)
            if problem == "subprocess":
                self.assertIn('"Action": "start"', diagnostics.getvalue())
                self.assertIn(names[0], diagnostics.getvalue())

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
                for name in names:
                    if re.fullmatch(pattern, name):
                        rows.extend([{"Action": "run", "Package": package, "Test": name},
                                     {"Action": "pass", "Package": package, "Test": name,
                                      "Elapsed": {"TestVector0": 303, "TestVector1": 122}.get(name, 0.001)}])
            rows.append({"Action": "pass", "Package": package})
            kwargs["stdout"].write("".join(json.dumps(row) + "\n" for row in rows).encode())
            return subprocess.CompletedProcess(command, 0)

        output = io.StringIO()
        with tempfile.TemporaryDirectory(prefix="ci-worker-") as directory, redirect_stdout(output), \
             mock.patch.object(self.shards, "check_identity"), mock.patch.object(self.shards, "command", side_effect=go):
            root = Path(directory)
            for profile, workers in self.shards.BATCHES.items():
                for worker, slots in enumerate(workers):
                    calls.clear()
                    destination = root / f"{profile}-worker-v1-{worker}"
                    self.shards.run_worker(profile, worker, destination, RUN, SHA, TREE)
                    tests = [command for command in calls if command[:2] == ["go", "test"]]
                    expected_runs = sum(max(1, (len(self.verifier.selection(Counter(names), slot, profile)[0]) + 23) // 24)
                                        if profile == "platform" else 1 for slot in slots)
                    self.assertEqual(len(tests), expected_runs + 1)
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
        receipts = [json.loads(line) for line in output.getvalue().splitlines() if line.startswith("{")]
        passes = [row for row in receipts if row["Action"] == "pass" and row.get("Test")]
        self.assertEqual(len(passes), 3 * len(names))
        for name, elapsed in (("TestVector0", 303), ("TestVector1", 122)):
            self.assertEqual([row["Elapsed"] for row in passes if row["Test"] == name], [elapsed] * 3)

    def test_recorded_json_replay_uses_bounded_reads(self):
        payload = (json.dumps({"Action": "pass", "Test": "TestSlow", "Elapsed": 303}) + "\n") * 4096
        source, destination = io.StringIO(payload), io.StringIO()
        read = source.read

        def bounded_read(length=-1):
            self.assertGreater(length, 0)
            self.assertLessEqual(length, 64 * 1024)
            return read(length)

        path = mock.Mock()
        path.open.return_value = source
        with mock.patch.object(source, "read", side_effect=bounded_read) as reads:
            self.shards.log_events(path, destination)
        self.assertEqual(destination.getvalue(), payload)
        self.assertGreater(reads.call_count, 2)

    def test_wrong_checkout_and_failed_execution_cannot_produce_evidence(self):
        for actual_sha, actual_tree in [("c" * 40, TREE), (SHA, "c" * 40)]:
            with mock.patch.object(self.shards, "command") as command:
                command.side_effect = [subprocess.CompletedProcess([], 0, stdout=value + "\n") for value in (actual_sha, actual_tree)]
                with self.assertRaises(ValueError):
                    self.shards.check_identity(SHA, TREE)
        failure = json.dumps({"Action": "output", "Test": "TestVector6", "Output": "failure diagnostic\n"}) + "\n"

        def failed_go(args, **kwargs):
            if args[:2] == ["go", "list"]:
                return subprocess.CompletedProcess(args, 0, stdout="agentworkspace/internal/store\n")
            if "-list" in args:
                return subprocess.CompletedProcess(args, 0)
            kwargs["stdout"].write(failure.encode())
            raise subprocess.CalledProcessError(1, args)

        diagnostics = io.StringIO()
        with tempfile.TemporaryDirectory(prefix="ci-failed-worker-") as directory, mock.patch.object(self.shards, "check_identity"), \
             mock.patch.object(self.shards, "command", side_effect=failed_go), \
             mock.patch.object(self.shards, "worker_parallelism", return_value=1), \
             mock.patch.object(self.shards.verifier, "inventory", return_value={"TestVector6": 1}):
            with self.assertRaises(subprocess.CalledProcessError), redirect_stdout(io.StringIO()), redirect_stderr(diagnostics):
                self.shards.run_worker("store", 0, Path(directory), RUN, SHA, TREE)
            self.assertEqual(list(Path(directory).iterdir()), [])
        self.assertEqual(diagnostics.getvalue(), failure)

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
        self.assertIn("if: ${{ !cancelled() }}", gate)
        needs = json.loads(re.search(r"^    needs: (\[[^\n]+\])$", gate, re.M)[1])
        self.assertEqual(self.gate.REQUIRED, {"go-shards", "go-other", "go-workspace", "go-vet", "go-benchmarks"})
        self.assertEqual(set(needs), set(self.gate.REQUIRED))
        self.assertEqual(len(needs), len(self.gate.REQUIRED))
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
        full_pr = pr(draft=False)
        full_pr["pull_request"]["labels"] = [{"name": "CI:FULL"}]
        cases = [(success, "pull_request", full_pr, True),
                 (success, "push", {"ref": "refs/heads/master"}, True),
                 (success, "push", {"ref": "refs/heads/feature"}, False),
                 (success, "merge_group", {"action": "checks_requested"}, False)]
        for draft in (True, False):
            cases.append((success, "pull_request", pr(draft=draft), False))
            for label, accepted in (("ci:full", True), ("CI:FULL", True), ("ci:refresh", False), ("CI:REFRESH", False), ("bug", False)):
                cases.append((success, "pull_request", pr("labeled", draft, label={"name": label}), accepted))
            refresh = pr("labeled", draft, label={"name": "CI:REFRESH"})
            refresh["pull_request"]["labels"] = [{"name": "CI:FULL"}]
            cases.append((success, "pull_request", refresh, True))
            for event in (pr("edited", draft, changes={"title": {"from": "old"}}), pr("labeled", draft, label={"name": "bug"})):
                event["pull_request"]["labels"] = [{"name": "ci:full"}]
                cases.append((success, "pull_request", event, False))
        for dependency in ("feedback", "full"):
            for state in ("failure", "cancelled", "skipped", "", "unknown"):
                for event_name, event in (("pull_request", full_pr), ("push", {"ref": "refs/heads/master"})):
                    cases.append(({**success, dependency: {"result": state}}, event_name, event, False))
            cases.append(({key: value for key, value in success.items() if key != dependency}, "pull_request", full_pr, False))
            for field in identity:
                broken = json.loads(json.dumps(success))
                broken[dependency]["outputs"][field] = "wrong"
                cases.append((broken, "pull_request", full_pr, False))
        for field in ("verified", *identity):
            broken = json.loads(json.dumps(success))
            broken["full"]["outputs"].pop(field)
            cases.append((broken, "pull_request", full_pr, False))
        feedback_only = json.loads(json.dumps(success))
        feedback_only["feedback"]["outputs"]["mode"] = "feedback"
        cases.extend([(feedback_only, "pull_request", full_pr, False), ({}, "pull_request", full_pr, False)])
        with tempfile.TemporaryDirectory(prefix="ci-outer-gate-") as directory:
            event_file = Path(directory) / "event.json"
            for payload, event_name, event, accepted in cases:
                with self.subTest(payload=payload, event_name=event_name, event=event):
                    event_file.write_text(json.dumps(event))
                    result = subprocess.run(["bash", "-c", command], cwd=SCRIPTS.parent.parent,
                                            env={**os.environ, "NEEDS_JSON": json.dumps(payload), "GITHUB_RUN_ID": RUN,
                                                 "GITHUB_SHA": SHA, "GITHUB_EVENT_PATH": str(event_file), "GITHUB_EVENT_NAME": event_name},
                                            capture_output=True, timeout=5)
                    self.assertEqual(result.returncode == 0, accepted, result.stderr)

    def test_workspace_race_job_is_independent_and_runs_one_exact_uncached_package(self):
        workflow = (WORKFLOWS / "ci-full.yml").read_text()
        workspace = job(workflow, "go-workspace")
        self.assertLess(workflow.index("\n  go-other:"), workflow.index("\n  go-workspace:"))
        self.assertLess(workflow.index("\n  go-workspace:"), workflow.index("\n  go-vet:"))
        for text in ["    name: Go workspace race\n", "    runs-on: ubuntu-latest\n", "    timeout-minutes: 25\n"]:
            self.assertIn(text, workspace)
        self.assertNotRegex(workspace, r"(?m)^ +(?:needs|if|continue-on-error|strategy):")
        self.assertEqual(re.findall(r"^      - uses: ([^\n]+)", workspace, re.M),
                         ["actions/checkout@v7", "actions/setup-go@v7"])
        self.assertIn("      - uses: actions/checkout@v7\n        with:\n"
                      "          ref: ${{ inputs.sha }}\n          persist-credentials: false\n", workspace)
        self.assertIn("      - uses: actions/setup-go@v7\n        with:\n"
                      "          go-version-file: go.mod\n          cache-dependency-path: go.sum\n", workspace)
        self.assertEqual(len(re.findall(r"^      - ", workspace, re.M)), 3)
        self.assertEqual(len(re.findall(r"^        run:", workspace, re.M)), 1)
        race = step(workspace, "Race test workspace package")
        self.assertIn("        env:\n          CHECKOUT_SHA: ${{ inputs.sha }}\n"
                      "          CHECKOUT_TREE: ${{ inputs.tree }}\n", race)
        script = textwrap.dedent(re.search(r"        run: \|\n(.*)", race, re.S)[1])
        self.assertEqual(script.splitlines()[0], "set -euo pipefail")
        command = "go test -count=1 -race -timeout=20m ./cmd/workspace"
        self.assertEqual([line.strip() for line in workspace.splitlines() if line.strip().startswith("go test")], [command])
        self.assertEqual(workflow.count(command), 1)
        self.assertEqual(script.rstrip().splitlines()[-1], command)
        self.assertNotIn("GOFLAGS", workspace)
        self.assertIn("    timeout-minutes: 25\n", job(workflow, "go-other"))

    def test_workspace_race_actual_step_checks_sha_and_tree_before_go_and_propagates_failure(self):
        workspace = job((WORKFLOWS / "ci-full.yml").read_text(), "go-workspace")
        race = step(workspace, "Race test workspace package")
        script = textwrap.dedent(re.search(r"        run: \|\n(.*)", race, re.S)[1])
        stub = '''git() {
          case "$*" in
            'rev-parse HEAD') printf '%s\\n' "$STUB_SHA" ;;
            'rev-parse HEAD^{tree}') printf '%s\\n' "$STUB_TREE" ;;
            *) return 90 ;;
          esac
        }
        go() {
          printf 'go:%s\\n' "$*"
          [[ "$*" == 'test -count=1 -race -timeout=20m ./cmd/workspace' ]] || return 90
          return "$STUB_GO_EXIT"
        }
        readonly -f git go
        '''
        for actual_sha, actual_tree, code in [(SHA, TREE, 0), ("c" * 40, TREE, 0), (SHA, "c" * 40, 0),
                                               ("", TREE, 0), (SHA, "", 0), (SHA, TREE, 1)]:
            with self.subTest(sha=actual_sha, tree=actual_tree, go_exit=code):
                identity_matches = actual_sha == SHA and actual_tree == TREE
                result = subprocess.run(["/bin/bash", "-c", stub + script],
                                        env={**os.environ, "PATH": "", "CHECKOUT_SHA": SHA, "CHECKOUT_TREE": TREE,
                                             "STUB_SHA": actual_sha, "STUB_TREE": actual_tree, "STUB_GO_EXIT": str(code)},
                                        text=True, capture_output=True, timeout=5)
                self.assertEqual(result.returncode == 0, identity_matches and code == 0, result.stderr)
                self.assertEqual(result.stdout.splitlines(),
                                 ["go:test -count=1 -race -timeout=20m ./cmd/workspace"] if identity_matches else [])

    def test_go_remainder_vet_and_bench_start_without_shard_dependencies(self):
        workflow = (WORKFLOWS / "ci-full.yml").read_text()
        for name in ["go-other", "go-workspace", "go-vet", "go-benchmarks"]:
            self.assertNotIn("needs:", job(workflow, name))
        other = job(workflow, "go-other")
        commands = ["go test -count=1 ./internal/coordination", "go test -count=1 -race ./internal/coordination",
                    'GOFLAGS="${GOFLAGS:+$GOFLAGS }-timeout=14m" go test -race "${rest[@]}"']
        positions = [other.index(command) for command in commands]
        self.assertEqual(positions, sorted(positions))
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


def load_tests(loader, tests, pattern):
    # Keep the periodic policy suite in both existing explicit CI test entry points.
    tests.addTests(loader.loadTestsFromModule(load("test-periodic-full")))
    return tests


if __name__ == "__main__":
    unittest.main()
