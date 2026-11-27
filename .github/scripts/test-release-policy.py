#!/usr/bin/env python3
"""Exercise optional immutable Full policy against distinct disposable Git trees; no real Go."""
from contextlib import contextmanager
import importlib.util
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile
import textwrap
import unittest


SCRIPTS = Path(__file__).resolve().parent
WORKFLOWS = SCRIPTS.parent / "workflows"
FULL = WORKFLOWS / "ci-full.yml"
POLICY_JOBS = {"go-shards", "go-vet", "gate"}
POLICY_SCRIPTS = ("ci-plan.py", "ci-shards.py", "ci-gate.py", "verify-store-shard.py",
                  "test-ci-scheduling.py", "test-store-shard.py")
RUN = "42566516008"


def field(block, key, indent=8):
    match = re.search(rf"^{' ' * indent}{re.escape(key)}: (.*)$", block, re.M)
    if match is None and indent == 8:
        match = re.search(rf"^      - {re.escape(key)}: (.*)$", block, re.M)
    return match[1] if match else None


def workflow_jobs():
    # Deliberately limited to these workflows' step layout; actionlint validates YAML itself.
    text = FULL.read_text(encoding="utf-8")
    anchors, jobs = {}, {}
    for match in re.finditer(r"^  ([\w-]+):\n(.*?)(?=^  [\w-]+:\n|\Z)", text.split("jobs:\n", 1)[1], re.M | re.S):
        steps = []
        for block in re.split(r"(?=^      - )", match[2], flags=re.M)[1:]:
            first, _, rest = block.partition("\n")
            if first.startswith("      - *"):
                name = first.removeprefix("      - *")
                if name not in anchors:
                    raise AssertionError("missing earlier step anchor: " + name)
                block = anchors[name]
            elif first.startswith("      - &"):
                name = first.removeprefix("      - &")
                block = "      - " + rest[8:]
                anchors[name] = block
            steps.append(block)
        jobs[match[1]] = steps
    return jobs


def named(steps, name):
    matches = [block for block in steps if block.startswith("      - name: " + name + "\n")]
    if len(matches) != 1:
        raise AssertionError("expected one step: " + name)
    return matches[0]


def script(block):
    command = field(block, "run")
    if command == "|":
        return textwrap.dedent(block.split("        run: |\n", 1)[1])
    if command is None:
        raise AssertionError("missing shell command")
    return command


class PolicyTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.jobs = workflow_jobs()
        cls.shared = cls.enterClassContext(cls.source_fixture())

    def test_optional_input_and_only_policy_consumers_get_identical_setup(self):
        text = FULL.read_text(encoding="utf-8")
        policy_input = re.search(r"(?m)^      policy_sha:\n((?:        [^\n]+\n)+)", text)
        self.assertIsNotNone(policy_input, "missing optional workflow_call policy_sha input")
        self.assertEqual(policy_input[1], "        type: string\n        required: false\n        default: ''\n")
        setup = [named(self.jobs["go-shards"], name) for name in (
            "Verify immutable policy input", "Checkout immutable CI policy", "Stage verified CI policy")]
        for name, steps in self.jobs.items():
            optional = [block for block in steps if field(block, "if") == "${{ inputs.policy_sha != '' }}"]
            self.assertEqual(optional, setup if name in POLICY_JOBS else [], name)
            target = [block for block in steps if field(block, "ref", 10) == "${{ inputs.sha }}"]
            self.assertEqual(len(target), 1, name)
            self.assertEqual(field(target[0], "persist-credentials", 10), "false")
            self.assertIsNone(field(target[0], "path", 10), "product checkout must stay at workspace root")
            if name in POLICY_JOBS:
                self.assertLess(steps.index(setup[-1]), steps.index(target[0]))
        checkout = setup[1]
        self.assertIn("uses: actions/checkout@v7", checkout)
        self.assertEqual(field(checkout, "ref", 10), "${{ inputs.policy_sha }}")
        self.assertEqual(field(checkout, "path", 10), ".ci-policy")
        self.assertEqual(field(checkout, "sparse-checkout", 10), ".github")
        self.assertEqual(field(checkout, "persist-credentials", 10), "false")
        self.assertIn(field(checkout, "fetch-depth", 10), (None, "1"))
        self.assertIsNone(field(checkout, "repository", 10), "policy must come from the caller repository")
        self.assertIsNone(field(checkout, "token", 10), "use checkout's built-in authentication only")
        for steps in self.jobs.values():
            for block in steps:
                if field(block, "run") and "go " in script(block):
                    self.assertIsNone(field(block, "working-directory"), "never run product Go in policy checkout")

    @classmethod
    @contextmanager
    def source_fixture(cls):
        with tempfile.TemporaryDirectory(prefix="release-policy-source-") as directory:
            root = Path(directory).resolve()
            repo = root / "objects"
            subprocess.run(["git", "init", "-q", str(repo)], check=True, capture_output=True)

            def git(*args, data=None):
                return subprocess.run(["git", "-C", str(repo), *args], input=data,
                                      check=True, capture_output=True).stdout.strip()

            def commit(files):
                # Only disposable objects, never a project commit or branch update.
                nodes = {}
                for filename, content in files.items():
                    node = nodes
                    parts = filename.split("/")
                    for part in parts[:-1]:
                        node = node.setdefault(part, {})
                    node[parts[-1]] = git("hash-object", "-w", "--stdin", data=content.encode())

                def tree(node):
                    entries = []
                    for name, value in sorted(node.items()):
                        entry = b"040000 tree " + tree(value) if isinstance(value, dict) else b"100644 blob " + value
                        entries.append(entry + b"\t" + name.encode() + b"\0")
                    return git("mktree", "-z", data=b"".join(entries))

                tree_id = tree(nodes)
                data = (b"tree " + tree_id + b"\nauthor CI <ci@example.invalid> 1 +0000\n"
                        b"committer CI <ci@example.invalid> 1 +0000\n\nfixture\n")
                return git("hash-object", "-t", "commit", "-w", "--stdin", data=data).decode(), tree_id.decode()

            policy_files = {".github/scripts/" + name: (SCRIPTS / name).read_text() for name in POLICY_SCRIPTS}
            policy_files.update({".github/workflows/" + name: (WORKFLOWS / name).read_text()
                                 for name in ("ci.yml", "ci-full.yml")})
            policy_files["go.mod"] = "module POLICY_MUST_NOT_BE_TESTED\n"
            candidate_files = {"go.mod": "module agentworkspace\n", "product.txt": "historical candidate\n"}
            for name in POLICY_SCRIPTS:
                candidate_files[".github/scripts/" + name] = "raise SystemExit('candidate policy must not execute: " + name + "')\n"
            policy_sha, policy_tree = commit(policy_files)
            sha, tree = commit(candidate_files)
            binary = root / "bin"
            binary.mkdir()
            go = binary / "go"
            go.write_text("#!" + sys.executable + "\n" + textwrap.dedent('''\
                import json, os, re, sys
                from pathlib import Path
                args = sys.argv[1:]
                sha = Path(".git/HEAD").read_text().strip()
                if not re.fullmatch(r"[0-9a-f]{40}", sha):
                    raise SystemExit("fixture checkout must have a detached full SHA")
                with open(os.environ["GO_LOG"], "a") as log:
                    log.write(json.dumps({"cwd": os.getcwd(), "sha": sha, "args": args}) + "\\n")
                package = "agentworkspace/" + args[-1].removeprefix("./")
                if args == ["list", "./..."]:
                    for name in ("internal/store", "internal/platform", "internal/httpapi", "internal/coordination", "cmd/workspace", "internal/other"):
                        print("agentworkspace/" + name)
                elif args[0] == "list":
                    print(package)
                elif "-bench" in args:
                    print("BenchmarkCandidate-8 1 1 ns/op 0 B/op 0 allocs/op")
                elif "-json" in args:
                    names = ["TestVector6"]
                    rows = [{"Action": "start", "Package": package}]
                    if "-list" in args:
                        rows.append({"Action": "output", "Package": package,
                                     "Output": "\\n".join(names) + "\\nok  \\t" + package + "\\t0.001s\\n"})
                    else:
                        pattern = args[args.index("-run") + 1]
                        rows += [{"Action": action, "Package": package, "Test": name}
                                 for name in names if re.fullmatch(pattern, name) for action in ("run", "pass")]
                    rows.append({"Action": "pass", "Package": package})
                    for row in rows:
                        print(json.dumps(row))
                elif args[0] not in ("test", "vet"):
                    raise SystemExit(90)
                '''))
            go.chmod(0o755)
            yield repo, policy_sha, policy_tree, sha, tree, binary

    @contextmanager
    def fixture(self):
        repo, policy_sha, policy_tree, sha, tree, binary = self.shared
        self.assertNotEqual(policy_sha, sha)
        self.assertNotEqual(policy_tree, tree)
        with tempfile.TemporaryDirectory(prefix="release-policy-") as directory:
            root = Path(directory).resolve()
            workspace, temporary = root / "workspace", root / "temp"
            workspace.mkdir()
            temporary.mkdir()
            env = {**os.environ, "GITHUB_WORKSPACE": str(workspace), "RUNNER_TEMP": str(temporary),
                   "GITHUB_ENV": str(root / "env"), "GITHUB_OUTPUT": str(root / "output"),
                   "GITHUB_WORKFLOW_SHA": policy_sha, "GITHUB_RUN_ID": RUN,
                   "GO_LOG": str(root / "go-log"), "PYTHONDONTWRITEBYTECODE": "1"}
            env.pop("CI_POLICY_ROOT", None)
            values = {"inputs.sha": sha, "inputs.tree": tree, "inputs.head": sha, "inputs.base": sha,
                      "inputs.policy_sha": policy_sha, "matrix.profile": "store", "matrix.worker": "0",
                      "toJSON(needs)": json.dumps({name: {"result": "success"} for name in
                                                   ("go-shards", "go-other", "go-workspace", "go-vet", "go-benchmarks")})}
            env["PATH"] = str(binary) + os.pathsep + env["PATH"]
            yield root, repo, workspace, temporary, env, values

    def resolve(self, value, env, values):
        if value == "${{ env.CI_POLICY_ROOT || github.workspace }}":
            return env.get("CI_POLICY_ROOT", env["GITHUB_WORKSPACE"])
        if value.startswith("${{ ") and value.endswith(" }}"):
            key = value[4:-3]
            self.assertIn(key, values, "unhandled expression: " + value)
            return values[key]
        return value

    def run_step(self, block, env, values, prefix="", *, timeout=30):
        step_env = dict(env)
        match = re.search(r"^        env:\n((?:          [^\n]+\n)+)", block, re.M)
        if match:
            for line in match[1].splitlines():
                key, value = line.strip().split(": ", 1)
                step_env[key] = self.resolve(value, env, values)
        cwd = self.resolve(field(block, "working-directory") or env["GITHUB_WORKSPACE"], env, values)
        result = subprocess.run(["bash", "-c", prefix + script(block)], cwd=cwd, env=step_env,
                                text=True, capture_output=True, timeout=timeout)
        if Path(env["GITHUB_ENV"]).exists():
            env.update(line.split("=", 1) for line in Path(env["GITHUB_ENV"]).read_text().splitlines())
        return result

    def checkout(self, block, repo, workspace, env, values):
        target = workspace / (field(block, "path", 10) or ".")
        ref = self.resolve(field(block, "ref", 10), env, values)
        subprocess.run(["git", "clone", "-q", "--shared", "--no-checkout", str(repo), str(target)],
                       check=True, capture_output=True)
        sparse = field(block, "sparse-checkout", 10)
        if sparse:
            subprocess.run(["git", "-C", str(target), "sparse-checkout", "set", sparse], check=True, capture_output=True)
        subprocess.run(["git", "-C", str(target), "checkout", "-q", "--detach", ref], check=True, capture_output=True)
        return target

    def prepare(self, name, repo, workspace, env, values):
        for block in self.jobs[name]:
            if field(block, "if") == "${{ inputs.policy_sha != '' }}" and not values["inputs.policy_sha"]:
                continue
            if field(block, "ref", 10):
                self.checkout(block, repo, workspace, env, values)
                if field(block, "ref", 10) == "${{ inputs.sha }}":
                    return
            elif field(block, "run"):
                result = self.run_step(block, env, values)
                self.assertEqual(result.returncode, 0, result.stderr)
        self.fail("missing target checkout")

    def assert_clean_target(self, workspace, sha, tree):
        result = subprocess.check_output(["git", "-C", str(workspace), "rev-parse", "HEAD", "HEAD^{tree}"], text=True)
        self.assertEqual(result.splitlines(), [sha, tree])
        self.assertEqual(subprocess.check_output(["git", "-C", str(workspace), "status", "--porcelain"], text=True), "")
        self.assertFalse((workspace / ".ci-policy").exists())

    def test_input_rejects_nonlowercase_short_different_and_missing_caller_sha(self):
        block = named(self.jobs["go-shards"], "Verify immutable policy input")
        with self.fixture() as (_, _, _, _, env, values):
            trusted = values["inputs.policy_sha"]
            cases = [(trusted, trusted, True), (trusted.upper(), trusted.upper(), False),
                     (trusted[:39], trusted[:39], False), (trusted + "0", trusted + "0", False),
                     ("g" * 40, "g" * 40, False), ("f" * 40, trusted, False),
                     (trusted + "\n", trusted + "\n", False), (trusted, "", False), (trusted, None, False)]
            for supplied, caller, accepted in cases:
                with self.subTest(supplied=supplied, caller=caller):
                    values["inputs.policy_sha"] = supplied
                    if caller is None:
                        env.pop("GITHUB_WORKFLOW_SHA", None)
                    else:
                        env["GITHUB_WORKFLOW_SHA"] = caller
                    result = self.run_step(block, env, values)
                    self.assertEqual(result.returncode == 0, accepted, result.stderr)

    def test_staging_rejects_different_checkout_missing_source_and_dirty_policy(self):
        steps = self.jobs["go-shards"]
        checkout = named(steps, "Checkout immutable CI policy")
        stage = named(steps, "Stage verified CI policy")
        for problem in ("different-sha", "missing-checkout", "missing-script", "dirty-script"):
            with self.subTest(problem=problem), self.fixture() as (_, repo, workspace, temporary, env, values):
                if problem != "missing-checkout":
                    source = self.checkout(checkout, repo, workspace, env, values)
                    if problem == "different-sha":
                        subprocess.run(["git", "-C", str(source), "checkout", "-q", "--detach", values["inputs.sha"]],
                                       check=True, capture_output=True)
                    elif problem == "missing-script":
                        (source / ".github/scripts/ci-shards.py").unlink()
                    else:
                        with (source / ".github/scripts/ci-shards.py").open("a") as output:
                            output.write("# dirty\n")
                result = self.run_step(stage, env, values)
                self.assertNotEqual(result.returncode, 0, result.stdout)
                self.assertNotIn("CI_POLICY_ROOT", env)
                self.assertFalse((temporary / "ci-policy").exists())

    def test_distinct_policy_plan_and_profile_workers_test_only_candidate_then_aggregate(self):
        with self.fixture() as (root, repo, workspace, temporary, env, values):
            self.prepare("go-shards", repo, workspace, env, values)
            policy = Path(env.get("CI_POLICY_ROOT", workspace)).resolve()
            self.assertEqual(policy, temporary / "ci-policy")
            self.assertNotEqual((policy / ".github/scripts/ci-plan.py").read_bytes(),
                                (workspace / ".github/scripts/ci-plan.py").read_bytes())
            spec = importlib.util.spec_from_file_location("release_policy_plan", policy / ".github/scripts/ci-plan.py")
            plan = importlib.util.module_from_spec(spec)
            spec.loader.exec_module(plan)
            spec = importlib.util.spec_from_file_location("release_policy_shards", policy / ".github/scripts/ci-shards.py")
            shards = importlib.util.module_from_spec(spec)
            spec.loader.exec_module(shards)
            matrix = plan.shard_matrix()["include"]
            expected_workers = {(profile, worker) for profile, batches in shards.BATCHES.items() for worker in range(len(batches))}
            self.assertEqual({(row["profile"], row["worker"]) for row in matrix}, expected_workers)
            self.assertEqual(len(matrix), len(expected_workers))
            compiled = {"TestVector6": 1}
            sampled = {profile: next(worker for worker, slots in enumerate(batches)
                                    if any(shards.verifier.selection(compiled, slot, profile)[0] for slot in slots))
                       for profile, batches in shards.BATCHES.items()}
            self.assertEqual(set(sampled), {"store", "platform", "httpapi"})
            run = named(self.jobs["go-shards"], "Run and verify logical slots")
            workers = temporary / "shard-workers"
            workers.mkdir()
            for row in matrix:
                profile, worker = row["profile"], row["worker"]
                artifact = workers / f"{profile}-worker-v1-{worker}"
                if worker == sampled[profile]:
                    values.update({"matrix.profile": profile, "matrix.worker": str(worker)})
                    scratch = temporary / f"worker-{profile}-{worker}"
                    scratch.mkdir()
                    env["RUNNER_TEMP"] = str(scratch)
                    result = self.run_step(run, env, values)
                    self.assertEqual(result.returncode, 0, result.stderr)
                    (scratch / "shard-evidence").rename(artifact)
                    continue
                # Runner coverage lives in test-ci-scheduling.py; this test exercises policy selection and the full aggregate.
                total, package, summary, prefix = shards.verifier.PROFILES[profile]
                expected = shards.identity(RUN, values["inputs.sha"], values["inputs.tree"], package)
                for slot in shards.BATCHES[profile][worker]:
                    selected, _ = shards.verifier.selection(compiled, slot, profile)
                    value = {"schema": shards.verifier.SCHEMA, "slot": slot, "total": total, **expected,
                             "compiled": compiled, "selected": selected, "runs": selected, "passes": selected, "skips": {}}
                    logical = artifact / f"{prefix}{slot}"
                    logical.mkdir(parents=True)
                    (logical / summary).write_text(json.dumps(value) + "\n")
            self.assertEqual(len(list(workers.glob("*/*/*-summary.json"))), 81)
            env["RUNNER_TEMP"] = str(temporary)
            # The gate consumes the same policy ref and candidate identity, not historical script bytes.
            gate = self.run_step(named(self.jobs["gate"], "Check required results"), env, values)
            self.assertEqual(gate.returncode, 0, gate.stderr)
            aggregate = named(self.jobs["gate"], "Verify all worker shapes and logical coverage")
            # Deterministic filesystem aggregation is bounded by the outer test/CI job.
            result = self.run_step(aggregate, env, values, timeout=None)
            self.assertEqual(result.returncode, 0, result.stderr)
            outputs = dict(line.split("=", 1) for line in (root / "output").read_text().splitlines())
            self.assertEqual(outputs, {"verified": "true", "run_id": RUN, "sha": values["inputs.sha"],
                                       "tree": values["inputs.tree"], "head": values["inputs.sha"], "base": values["inputs.sha"]})
            for key in ("inputs.sha", "inputs.tree"):
                original = values[key]
                values[key] = "f" * 40
                result = self.run_step(run, env, values)
                self.assertNotEqual(result.returncode, 0)
                result = self.run_step(aggregate, env, values, timeout=None)
                self.assertNotEqual(result.returncode, 0)
                values[key] = original
            calls = [json.loads(line) for line in (root / "go-log").read_text().splitlines()]
            self.assertTrue(calls)
            self.assertEqual({row["args"][-1] for row in calls}, {"./internal/" + profile for profile in sampled})
            self.assertTrue(all(row["cwd"] == str(workspace) and row["sha"] == values["inputs.sha"] for row in calls))
            self.assertTrue(all("-count=1" in row["args"] and "-race" in row["args"]
                                for row in calls if row["args"][0] == "test"))
            self.assert_clean_target(workspace, values["inputs.sha"], values["inputs.tree"])

    def test_all_other_go_commands_stay_at_candidate_and_selfchecks_use_policy_cwd(self):
        for name in ("go-other", "go-workspace", "go-vet", "go-benchmarks"):
            with self.subTest(job=name), self.fixture() as (root, repo, workspace, temporary, env, values):
                self.prepare(name, repo, workspace, env, values)
                for block in self.jobs[name]:
                    if field(block, "run") and re.search(r"\bgo (test|vet|list)\b", script(block)):
                        result = self.run_step(block, env, values)
                        self.assertEqual(result.returncode, 0, result.stderr)
                calls = [json.loads(line) for line in (root / "go-log").read_text().splitlines()]
                self.assertTrue(calls)
                self.assertTrue(all(row["cwd"] == str(workspace) and row["sha"] == values["inputs.sha"] for row in calls))
                if name == "go-vet":
                    checks = named(self.jobs[name], "Test CI scheduling and shard gates")
                    self.assertEqual(field(checks, "working-directory"), "${{ env.CI_POLICY_ROOT || github.workspace }}")
                    result = self.run_step(checks, env, values, prefix='python3() { printf "%s:%s\\n" "$PWD" "$*"; };\n')
                    self.assertEqual(result.returncode, 0, result.stderr)
                    self.assertEqual(result.stdout.splitlines(), [str(temporary / "ci-policy") + ":.github/scripts/" + script + " -v"
                                                                  for script in ("test-ci-scheduling.py", "test-store-shard.py")])
                self.assert_clean_target(workspace, values["inputs.sha"], values["inputs.tree"])

    def test_empty_input_keeps_legacy_candidate_scripts_without_policy_checkout(self):
        with self.fixture() as (_, repo, workspace, temporary, env, values):
            values["inputs.policy_sha"] = ""
            env.pop("GITHUB_WORKFLOW_SHA")
            self.prepare("go-shards", repo, workspace, env, values)
            self.assertNotIn("CI_POLICY_ROOT", env)
            self.assertFalse((temporary / "ci-policy").exists())
            result = self.run_step(named(self.jobs["go-shards"], "Run and verify logical slots"), env, values)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("candidate policy must not execute: ci-shards.py", result.stderr)
            self.assert_clean_target(workspace, values["inputs.sha"], values["inputs.tree"])


if __name__ == "__main__":
    unittest.main()
