#!/usr/bin/env python3
"""Small, dependency-free fixtures; native reporters are exercised when available."""
import copy
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tarfile
import tempfile
import unittest
from unittest.mock import patch

sys.dont_write_bytecode = True
SPEC = importlib.util.spec_from_file_location("periodic_clients", Path(__file__).with_name("periodic-clients.py"))
clients = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(clients)

SHA, TREE = "a" * 40, "b" * 40
WEB_TITLE = "IM shell aborts hidden task list/detail reads, ignores late results and reuses the mounted form on return"
WEB_MESSAGE = 'expected "vi.fn()" to be called 2 times, but got 3 times'
WEB_LOG = f'''2026-11-26T13:01:02.123Z \x1b[41m FAIL \x1b[49m src/App.test.tsx\x1b[2m > \x1b[22mIM shell > {WEB_TITLE.removeprefix("IM shell ")}
2026-11-26T13:01:02.123Z \x1b[31mAssertionError\x1b[39m: {WEB_MESSAGE}
 at src/App.test.tsx:478:30
 FAIL src/RequestWork.test.tsx > uses real scoped author identity for '我' / 'user'
AssertionError: expected 'agent' to be 'user'
'''
NODE_MESSAGE = "Expected values to be strictly equal:\n\n1 !== 2"


def candidate(client="web", test=None):
    return {"kind": "client", "client": client,
            "file": "web/src/App.test.tsx" if client == "web" else "extensions/pi-bridge/tests/example.test.mjs",
            "test": test or (WEB_TITLE if client == "web" else "exact 我 title"),
            "signature": {"name": "AssertionError", "message": WEB_MESSAGE if client == "web" else NODE_MESSAGE}}


def web_report(status="failed", title=WEB_TITLE, **changes):
    names = title.split(" ", 2) if title == WEB_TITLE else [title]
    ancestors, leaf = (names[:2], names[2]) if len(names) == 3 else ([], title)
    row = {"ancestorTitles": ancestors, "title": leaf, "fullName": title, "status": status,
           "duration": 1.2, "failureMessages": ["AssertionError: " + WEB_MESSAGE + "\n    at src/App.test.tsx:478:30"] if status == "failed" else []}
    result = {"numTotalTestSuites": 2, "numPassedTestSuites": 0 if status == "failed" else 2,
              "numFailedTestSuites": 2 if status == "failed" else 0, "numPendingTestSuites": 0,
              "numTotalTests": 1, "numPassedTests": int(status == "passed"), "numFailedTests": int(status == "failed"),
              "numPendingTests": int(status in ("skipped", "pending")), "numTodoTests": int(status == "todo"),
              "success": status != "failed", "snapshot": {"failure": False, "unmatched": 0, "unchecked": 0},
              "testResults": [{"name": "/tmp/replay/web/src/App.test.tsx", "status": "failed" if status == "failed" else "passed",
                               "message": "", "startTime": 100, "endTime": 102, "assertionResults": [row]}]}
    result.update(changes)
    return result


def tap(status="fail", name="exact 我 title", location="tests/example.test.mjs:2:1", extra=""):
    if status == "fail":
        record = f'''not ok 1 - {name}
  ---
  duration_ms: 1.5
  type: 'test'
  location: '{location}'
  failureType: 'testCodeFailure'
  error: |-
    Expected values to be strictly equal:

    1 !== 2

  code: 'ERR_ASSERTION'
  name: 'AssertionError'
  expected: 2
  actual: 1
  operator: 'strictEqual'
  stack: |-
    TestContext.<anonymous> (file:///tmp/replay/extensions/pi-bridge/tests/example.test.mjs:2:40)
  ...'''
    else:
        record = f'''ok 1 - {name}{' # SKIP' if status == 'skip' else ''}
  ---
  duration_ms: 1
  type: 'test'
  ...'''
    return f'''TAP version 13
# Subtest: {name}
{record}
1..1
# tests 1
# suites 0
# pass {int(status == 'pass')}
# fail {int(status == 'fail')}
# cancelled 0
# skipped {int(status == 'skip')}
# todo 0
# duration_ms 2
{extra}'''


def spec_log():
    return f'''✔ passes (1ms)
✖ exact 我 title (1.5ms)
ℹ tests 2
ℹ pass 1
ℹ fail 1
✖ failing tests:

test at tests/example.test.mjs:2:1
✖ exact 我 title (1.5ms)
  AssertionError [ERR_ASSERTION]: Expected values to be strictly equal:

  1 !== 2

      at TestContext.<anonymous> (file:///tmp/replay/tests/example.test.mjs:2:40) {{
    generatedMessage: true,
    code: 'ERR_ASSERTION',
    actual: 1,
    expected: 2,
    operator: 'strictEqual'
  }}
'''


class ParsingTests(unittest.TestCase):
    def test_actual_ansi_web_failures_and_unicode_title(self):
        rows = clients.failures("web", WEB_LOG)
        self.assertEqual(len(rows), 2)
        self.assertEqual(rows[0], candidate())
        self.assertEqual(rows[1]["test"], "uses real scoped author identity for '我' / 'user'")

    def test_unrelated_timeout_does_not_erase_named_web_failure(self):
        rows = clients.failures("web", WEB_LOG + " FAIL src/Other.test.ts > hangs\nError: Hook timed out in 10000ms\n")
        self.assertEqual(len(rows), 2)
        self.assertEqual(rows[0]["signature"], candidate()["signature"])
        self.assertEqual(clients.failures("web", "infra timed out\n" + WEB_LOG), rows)

    def test_no_named_diagnostic_no_candidate(self):
        for text in ("AssertionError: expected 1 to be 2", "FAIL src/App.test.tsx\nSyntaxError: broken",
                     "src/App.test.tsx(10,3): error TS1234: build broke", "No test files found",
                     " FAIL src/App.test.tsx > hangs\nError: Test timed out in 5000ms",
                     " FAIL src/App.test.tsx > fake\nsome unassociated output\nAssertionError: expected 1 to be 2"):
            with self.subTest(text=text):
                self.assertEqual(clients.failures("web", text), [])

    def test_duplicate_names_and_unsafe_paths(self):
        self.assertEqual(len(clients.failures("web", WEB_LOG + WEB_LOG)), 0)
        for path in ("../src/App.test.tsx", "src/../App.test.tsx", "src/a;echo.test.tsx", "/tmp/other/src/App.test.tsx",
                     "/tmp/evil;echo/web/src/App.test.tsx", "/tmp//web/src/App.test.tsx"):
            self.assertEqual(clients.failures("web", f" FAIL {path} > test\nAssertionError: expected 1 to be 2"), [])
        self.assertEqual(clients.failures("other", WEB_LOG), [])
        self.assertEqual(clients.failures("web", WEB_LOG + "x" * clients.MAX_LOG), [])

    def test_web_json_uses_structured_full_name_and_scoped_error(self):
        doc = web_report()
        self.assertEqual(clients.failures("web", json.dumps(doc)), [candidate()])
        row = copy.deepcopy(doc["testResults"][0]["assertionResults"][0])
        row.update(fullName="unknown", ancestorTitles=[], title="unknown", failureMessages=["Error: Test timed out in 5000ms"])
        doc["testResults"][0]["assertionResults"].append(row)
        self.assertEqual(clients.failures("web", json.dumps(doc)), [candidate()])
        doc["testResults"][0]["assertionResults"].append({"title": "unrelated malformed test"})
        self.assertEqual(clients.failures("web", json.dumps(doc)), [candidate()])
        doc["testResults"][0]["assertionResults"].append(copy.deepcopy(doc["testResults"][0]["assertionResults"][0]))
        self.assertEqual(clients.failures("web", json.dumps(doc)), [])

    def test_node_real_tap_and_default_have_same_signature(self):
        self.assertEqual(clients.failures("bridge", tap()), [candidate("bridge")])
        self.assertEqual(clients.failures("bridge", spec_log()), [candidate("bridge")])

    def test_node_requires_linked_exact_file_and_leaf(self):
        for text in (spec_log().replace("test at tests/example.test.mjs:2:1", ""),
                     spec_log().replace("✖ exact 我 title (1.5ms)", "✖ same name (1.5ms)", 1),
                     "▶ exact 我 title\n" + spec_log(),
                     "  ✖ exact 我 title (1.5ms)\n" + spec_log(),
                     tap(location="../tests/example.test.mjs:2:1"),
                     tap().replace("  location: 'tests/example.test.mjs:2:1'\n", ""),
                     tap().replace("type: 'test'", "type: 'suite'"),
                     tap().replace("failureType: 'testCodeFailure'", "failureType: 'hookFailed'")):
            self.assertEqual(clients.failures("bridge", text), [])

    def test_tap_unrelated_timeout_and_truncation_preserve_failure(self):
        prefix = tap().split("1..1\n", 1)[0]
        text = prefix + '''# Subtest: infra timeout
not ok 2 - infra timeout
  ---
  duration_ms: 1000
  type: 'test'
  location: 'tests/infra.test.mjs:1:1'
  failureType: 'testTimeoutFailure'
  error: 'test timed out after 1000ms'
  code: 'ERR_TEST_FAILURE'
  ...
1..2
# tests 2
# suites 0
# pass 0
# fail 2
# cancelled 0
# skipped 0
# todo 0
# duration_ms 1002
'''
        self.assertEqual(clients.failures("bridge", text), [candidate("bridge")])
        self.assertEqual(clients.failures("bridge", prefix + "# Subtest: unfinished\n"), [candidate("bridge")])
        self.assertEqual(clients.assess_tap(text, 1, candidate("bridge"))["state"], "unknown")

    def test_duplicate_node_names_are_ambiguous_even_if_one_passes(self):
        prefix = tap().split("1..1\n", 1)[0]
        duplicate = tap("pass").split("1..1\n", 1)[0].removeprefix("TAP version 13\n").replace("ok 1 -", "ok 2 -")
        self.assertEqual(clients.failures("bridge", prefix + duplicate + "1..2\n"), [])
        self.assertEqual(clients.failures("bridge", tap().replace("  expected: 2", "  expected: {}")), [candidate("bridge")])

    def test_nested_tap_proves_full_name_not_parent_failure(self):
        inner = tap().split("1..1\n", 1)[0].removeprefix("TAP version 13\n")
        text = "TAP version 13\n# Subtest: suite\n" + "\n".join("    " + line for line in inner.splitlines())
        text += '''
    1..1
not ok 1 - suite
  ---
  type: 'suite'
  failureType: 'subtestsFailed'
  error: '1 subtest failed'
  code: 'ERR_TEST_FAILURE'
  ...
1..1
# tests 1
# suites 1
# pass 0
# fail 1
# cancelled 0
# skipped 0
# todo 0
# duration_ms 2
'''
        expected = candidate("bridge", "suite exact 我 title")
        self.assertEqual(clients.failures("bridge", text), [expected])
        self.assertEqual(clients.assess_tap(text, 1, expected), {"state": "fail", "signature": expected["signature"]})


class AssessmentTests(unittest.TestCase):
    def assess_web(self, doc, code=0, stdout=""):
        return clients.assess_web(json.dumps(doc), code, candidate(), stdout)

    def test_web_exact_pass_and_fail(self):
        self.assertEqual(self.assess_web(web_report("passed")), {"state": "pass"})
        self.assertEqual(self.assess_web(web_report(), 1), {"state": "fail", "signature": candidate()["signature"]})

    def test_web_filtered_skips_are_not_target_skips(self):
        doc = web_report("passed")
        doc["testResults"][0]["assertionResults"].append({"ancestorTitles": [], "title": "other", "fullName": "other",
                                                        "status": "skipped", "failureMessages": []})
        doc.update(numTotalTests=2, numPendingTests=1)
        self.assertEqual(self.assess_web(doc), {"state": "pass"})
        for state in ("skipped", "pending", "todo"):
            self.assertEqual(self.assess_web(web_report(state))["state"], "unknown")

    def test_web_rejects_other_execution_duplicates_build_and_unhandled_errors(self):
        for mutation in ("duplicate", "other-file", "other-failure", "no-tests", "bad-count", "build", "snapshot"):
            doc = web_report("passed")
            row = doc["testResults"][0]["assertionResults"][0]
            if mutation == "duplicate":
                doc["testResults"][0]["assertionResults"].append(copy.deepcopy(row))
            elif mutation == "other-file":
                doc["testResults"].append(copy.deepcopy(doc["testResults"][0]))
            elif mutation == "other-failure":
                other = copy.deepcopy(row)
                other.update(title="other", fullName="IM shell other", status="failed", failureMessages=["Error: infra timed out"])
                doc["testResults"][0]["assertionResults"].append(other)
                doc.update(numTotalTests=2, numFailedTests=1, success=False)
            elif mutation == "no-tests":
                doc["testResults"][0]["assertionResults"] = []
                doc.update(numTotalTests=0, numPassedTests=0)
            elif mutation == "bad-count":
                doc["numFailedTests"] = 1
            elif mutation == "build":
                doc["testResults"][0]["message"] = "Build failed"
            else:
                doc["snapshot"]["failure"] = True
            with self.subTest(mutation=mutation):
                self.assertEqual(self.assess_web(doc)["state"], "unknown")
        self.assertEqual(self.assess_web(web_report("passed"), 1)["state"], "unknown")
        self.assertEqual(self.assess_web(web_report("passed"), 0, "Unhandled Error\nError: worker terminated")["state"], "unknown")

    def test_json_duplicate_fields_and_nonfinite_are_unknown(self):
        for text in ('{"testResults":[],"testResults":[]}', '{"testResults":[],"x":NaN}'):
            self.assertEqual(clients.assess_web(text, 0, candidate())["state"], "unknown")

    def test_tap_exact_lifecycle_not_just_exit_status(self):
        self.assertEqual(clients.assess_tap(tap("pass"), 0, candidate("bridge")), {"state": "pass"})
        self.assertEqual(clients.assess_tap(tap(), 1, candidate("bridge")), {"state": "fail", "signature": candidate("bridge")["signature"]})
        for text, code in ((tap("skip"), 0), (tap("pass"), 1), (tap(), 0),
                           (tap("pass").replace("# Subtest: exact 我 title\n", ""), 0),
                           (tap("pass").replace("1..1", "1..0"), 0),
                           (tap("pass").replace("# tests 1", "# tests 0"), 0),
                           (tap("pass", name="other"), 0),
                           (tap("pass").replace("# cancelled 0", "# cancelled 1"), 0),
                           (tap("pass") + "Error: late uncaught exception\n", 0),
                           ("TAP version 13\n1..0\n# tests 0\n# pass 0\n# fail 0\n", 0)):
            self.assertEqual(clients.assess_tap(text, code, candidate("bridge"))["state"], "unknown")


class SafetyTests(unittest.TestCase):
    def test_environment_is_private_allowlist(self):
        with tempfile.TemporaryDirectory() as tmp, patch.dict(os.environ, {"GH_TOKEN": "secret", "NPM_TOKEN": "secret", "NODE_OPTIONS": "--require evil", "PATH": "/evil",
                "npm_config_ignore_scripts": "true", "npm_config_engine_strict": "true", "npm_config_audit": "false", "npm_config_fund": "false"}):
            home = Path(tmp)
            env = clients.child_env(home)
            self.assertEqual(env["HOME"], str(home))
            self.assertEqual(env["PATH"], str(home / "bin") + ":/usr/bin:/bin")
            self.assertEqual(env["npm_config_cache"], str(home / "npm-cache"))
            self.assertFalse({"GH_TOKEN", "NPM_TOKEN", "NODE_OPTIONS"} & env.keys())
            self.assertFalse({"npm_config_ignore_scripts", "npm_config_engine_strict", "npm_config_audit", "npm_config_fund"} & env.keys())

    def test_install_argv_preserves_per_client_ci_semantics(self):
        self.assertEqual(clients.install_argv("/node22", "/npm-cli.js", "web"), ["/node22", "/npm-cli.js", "ci"])
        self.assertEqual(clients.install_argv("/node24", "/npm-cli.js", "bridge"),
                         ["/node24", "/npm-cli.js", "ci", "--ignore-scripts", "--no-audit", "--no-fund"])
        with self.assertRaises(ValueError):
            clients.install_argv("/node", "/npm-cli.js", "other")

    def test_archive_rejects_traversal_symlinks_and_duplicate_files(self):
        for names, kind in ((["web/../../escape"], "file"), (["web/link"], "link"), (["web/a", "web/a"], "file")):
            stream = io.BytesIO()
            with tarfile.open(fileobj=stream, mode="w") as tar:
                for name in names:
                    info = tarfile.TarInfo(name)
                    if kind == "link":
                        info.type, info.linkname = tarfile.SYMTYPE, "/etc/passwd"
                    else:
                        info.size = 1
                    tar.addfile(info, io.BytesIO(b"x") if kind == "file" else None)
            with self.assertRaises(ValueError):
                clients.archive_files(stream.getvalue(), "web")

    def test_exact_argv_escapes_native_regex_without_shell(self):
        item = candidate("bridge", "literal (a)+ [$] 我 .?")
        argv = clients.run_argv("/private/node", item)
        self.assertEqual(argv[-2], r"--test-name-pattern=^literal \(a\)\+ \[\$\] 我 \.\?$")
        self.assertEqual(argv[-1], "tests/example.test.mjs")

    def test_manifest_and_registry_lock_must_match(self):
        package = {"name": "agent-workspace-web", "scripts": {"test": "vitest run"}}
        lock = {"name": package["name"], "lockfileVersion": 3, "packages": {"": {"name": package["name"]}}}
        clients.validate_lock(package, lock, "web")
        for url in ("https://token@registry.npmjs.org/a/-/a.tgz", "https://evil.invalid/a.tgz", "file:../a", "git+https://example/a"):
            broken = copy.deepcopy(lock)
            broken["packages"]["node_modules/a"] = {"resolved": url, "integrity": "sha512-YQ=="}
            with self.assertRaises(ValueError):
                clients.validate_lock(package, broken, "web")


def native_node():
    return os.environ.get("PERIODIC_BRIDGE_NODE") or shutil.which("node")


def native_run(argv, cwd, timeout=15):
    with tempfile.TemporaryDirectory(prefix="native-reporter-") as tmp:
        home = Path(tmp)
        (home / "bin").mkdir()
        (home / "bin/node").symlink_to(Path(argv[0]).resolve())
        for config in ("npmrc", "global-npmrc"):
            (home / config).touch(mode=0o600)
        record, data = clients.process_to_file(argv, cwd, clients.child_env(home), home / "stdout.bin", timeout, clients.MAX_LOG)
        if record.get("state"):
            raise AssertionError(record["reason"] + "\n" + data.decode(errors="replace"))
        return subprocess.CompletedProcess(argv, record["returncode"], data.decode(), "")


class NativeReporterTests(unittest.TestCase):
    def test_native_npm_lifecycle_runs_for_web_but_not_bridge(self):
        for client in ("web", "bridge"):
            with self.subTest(client=client), tempfile.TemporaryDirectory() as temporary:
                prefix = "PERIODIC_" + client.upper()
                node = os.environ.get(prefix + "_NODE")
                if not node:
                    self.skipTest("未指定原生工具链：" + prefix + "_NODE")
                npm = os.environ.get(prefix + "_NPM") or str(Path(node).resolve().parent.parent / "lib/node_modules/npm/bin/npm-cli.js")
                root = Path(temporary).resolve()
                package = {"name": clients.NAMES[client], "scripts": {"test": clients.SCRIPTS[client], "postinstall": "node lifecycle.cjs"}}
                lock = {"name": package["name"], "lockfileVersion": 3, "packages": {"": {"name": package["name"], "hasInstallScript": True}}}
                clients.validate_lock(package, lock, client)
                (root / "package.json").write_text(json.dumps(package))
                (root / "package-lock.json").write_text(json.dumps(lock))
                (root / "lifecycle.cjs").write_text('require("node:fs").writeFileSync("lifecycle.json", JSON.stringify({node: process.version, '
                    'secrets: ["GH_TOKEN", "NPM_TOKEN", "NODE_OPTIONS"].some(key => key in process.env), ignore: process.env.npm_config_ignore_scripts}));')
                with patch.dict(os.environ, {"GH_TOKEN": "fixture-secret", "NPM_TOKEN": "fixture-secret", "NODE_OPTIONS": "--require nonexistent", "npm_config_ignore_scripts": "true"}):
                    # Loading the official npm tree from HDD also needs the opt-in startup budget.
                    completed = native_run(clients.install_argv(str(Path(node).resolve()), str(Path(npm).resolve()), client), root, timeout=300)
                self.assertEqual(completed.returncode, 0, completed.stdout)
                marker = root / "lifecycle.json"
                self.assertEqual(marker.exists(), client == "web", completed.stdout)
                if client == "web":
                    evidence = json.loads(marker.read_text())
                    self.assertTrue(clients.version_ok(client, evidence["node"]))
                    self.assertFalse(evidence["secrets"])
                    self.assertNotEqual(evidence.get("ignore"), "true")
                self.assertEqual(json.loads((root / "package-lock.json").read_text()), lock)

    def test_native_node_tap_and_spec_associate_assertions(self):
        node = native_node()
        if not node:
            self.skipTest("未安装 Node")
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp) / "extensions/pi-bridge"
            (root / "tests").mkdir(parents=True)
            source = root / "tests/example.test.mjs"
            source.write_text('import {test} from "node:test"; import assert from "node:assert/strict";\n'
                              'test("exact 我 title",()=>assert.equal(1,2)); test("other",()=>{});\n')
            signatures = []
            for reporter in ("tap", "spec"):
                completed = native_run([node, "--test", "--test-reporter=" + reporter, "tests/example.test.mjs"], root)
                self.assertEqual(completed.returncode, 1)
                found = clients.failures("bridge", completed.stdout)
                self.assertEqual(found, [candidate("bridge")])
                signatures.append(found[0]["signature"])
            self.assertEqual(signatures[0], signatures[1])

    def test_optional_native_vitest_json_and_default(self):
        cli = os.environ.get("PERIODIC_TEST_VITEST")
        node = os.environ.get("PERIODIC_WEB_NODE")
        if not cli or not node:
            self.skipTest("未指定本地 Node 22/Vitest；普通检查不安装 Web 依赖")
        if not native_run([node, "--version"], Path.cwd()).stdout.startswith("v22."):
            self.skipTest("Web 原生夹具要求与 CI 一致的 Node 22")
        cli = Path(cli).resolve()
        with tempfile.TemporaryDirectory() as tmp:
            web = Path(tmp) / "web"
            (web / "src").mkdir(parents=True)
            modules = web / "node_modules"
            modules.mkdir()
            # Keep Vite's generated caches private; only dependency entries are links.
            for entry in cli.parent.parent.iterdir():
                if not entry.name.startswith("."):
                    (modules / entry.name).symlink_to(entry, target_is_directory=entry.is_dir())
            (web / "package.json").write_text('{"type":"module"}')
            (web / "vite.config.mjs").write_text('export default { cacheDir: "./.cache", test: { environment: "node" } };')
            (web / "src/App.test.tsx").write_text('import {describe,it,expect,vi} from "vitest";\n'
                'describe("IM shell",()=>{it(' + json.dumps(WEB_TITLE.removeprefix("IM shell ")) + ',()=>{const f=vi.fn();f();f();f();expect(f).toHaveBeenCalledTimes(2)});it("other",()=>{})});\n')
            report = Path(tmp) / "report.json"
            # Cold dependency imports on HDD exceeded 90s; keep this opt-in check bounded.
            completed = native_run([node, str(cli), "run", "src/App.test.tsx", "--reporter=default", "--reporter=json",
                                    "--outputFile.json=" + str(report), "--testNamePattern=^" + clients.js_escape(WEB_TITLE) + "$"], web, timeout=300)
            self.assertEqual(completed.returncode, 1, completed.stdout + completed.stderr)
            self.assertEqual(clients.failures("web", completed.stdout + completed.stderr), [candidate()])
            self.assertEqual(clients.assess_web(report.read_text(), 1, candidate()), {"state": "fail", "signature": candidate()["signature"]})


class ReplayTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.repository = self.root / "repository"
        self.repository.mkdir()
        self.output = self.root / "output"

    def probe(self):
        runner = clients.ClientProbe(self.repository, self.output, timeout=15, budget=60)
        self.addCleanup(runner.close)
        return runner

    def snapshot(self, runner, source, missing=False, lock=True, client="bridge"):
        package = clients.PROJECTS[client]
        name = clients.NAMES[client]
        files = {package + "/package.json": json.dumps({"name": name, "type": "module", "scripts": {"test": clients.SCRIPTS[client]}}).encode()}
        if lock:
            files[package + "/package-lock.json"] = json.dumps({"name": name, "lockfileVersion": 3,
                "packages": {"": {"name": name}}}).encode()
        if not missing:
            files[candidate(client)["file"]] = source.encode()
        listing = b"".join(b"100644 blob " + hashlib.sha1(b"blob " + str(len(data)).encode() + b"\0" + data).hexdigest().encode() + b"\t" + name.encode() + b"\0" for name, data in sorted(files.items()))
        stream = io.BytesIO()
        with tarfile.open(fileobj=stream, mode="w") as tar:
            for name, data in files.items():
                entry = tarfile.TarInfo(name)
                entry.size = len(data)
                tar.addfile(entry, io.BytesIO(data))
        original = runner.execute

        def execute(argv, cwd, label, **kwargs):
            if label in ("tree", "inventory", "archive"):
                data = {"tree": (TREE + "\n").encode(), "inventory": listing, "archive": stream.getvalue()}[label]
                return runner.artifact_command(argv, cwd, label, data, 0), data
            return original(argv, cwd, label, **kwargs)
        return patch.object(runner, "execute", side_effect=execute)

    def configure_node(self):
        node = native_node()
        if not node:
            self.skipTest("未安装 Node 24.13.0")
        version = native_run([node, "--version"], self.root, timeout=5).stdout.strip()
        if version != "v24.13.0":
            self.skipTest("端到端 bridge 回放明确要求 Node 24.13.0")
        npm = os.environ.get("PERIODIC_BRIDGE_NPM")
        if not npm:
            npm = str(Path(node).resolve().parent.parent / "lib/node_modules/npm/bin/npm-cli.js")
        return patch.dict(os.environ, {"PERIODIC_BRIDGE_NODE": str(Path(node).resolve()), "PERIODIC_BRIDGE_NPM": npm})

    def test_native_lockfile_install_exact_failure_pass_and_artifact_tamper(self):
        with self.configure_node():
            runner = self.probe()
            logs = set()
            for assertion, expected in (("assert.equal(1,2)", "fail"), ("assert.equal(1,2)", "fail"), ("assert.equal(1,1)", "pass")):
                source = 'import {test} from "node:test"; import assert from "node:assert/strict";\n'
                source += 'test("exact 我 title",()=>{assert.equal(process.env.GH_TOKEN,undefined);assert.equal(process.env.NODE_OPTIONS,undefined);' + assertion + '});test("other",()=>{throw Error("must not run")});\n'
                with self.snapshot(runner, source):
                    result = runner(SHA, candidate("bridge"))
                self.assertEqual(result["state"], expected, result)
                self.assertEqual(clients.verify_probe(result, candidate("bridge"), self.output)["state"], expected)
                self.assertEqual(result["definition"]["state"], "present")
                self.assertIn("--ignore-scripts", result["install"]["argv"])
                self.assertEqual(result["run"]["argv"][-1], "tests/example.test.mjs")
                self.assertNotIn(result["run"]["log"], logs)
                logs.add(result["run"]["log"])
                wrong_lifecycle = tap("pass", name="other").encode()
                changed = copy.deepcopy(result)
                changed["run"] = runner.artifact_command(result["run"]["argv"], result["run"]["cwd"], "run", wrong_lifecycle, 0)
                self.assertEqual(clients.verify_probe(changed, candidate("bridge"), self.output)["state"], "unknown")
                raw = self.output / result["run"]["log"]
                raw.write_bytes(raw.read_bytes() + b"tampered")
                self.assertEqual(clients.verify_probe(result, candidate("bridge"), self.output)["state"], "unknown")

    def test_web_replay_and_verifier_with_mocked_native_json(self):
        runner = self.probe()
        node, npm = "/fixture/node22", "/fixture/npm-cli.js"

        def tools(client, home, result):
            result["toolchain"] = {"node": node, "npm": npm, "node_sha256": "c" * 64, "npm_sha256": "d" * 64,
                "node_version": runner.artifact_command([node, "--version"], home, "node-version", b"v22.21.0\n", 0),
                "npm_version": runner.artifact_command([node, npm, "--version"], home, "npm-version", b"10.9.4\n", 0)}
            return node, npm

        for status, code in (("passed", 0), ("failed", 1)):
            with self.snapshot(runner, 'it("fixture",()=>{});', client="web") as executed, patch.object(runner, "tools", side_effect=tools):
                source_execute = executed.side_effect

                def execute(argv, cwd, label, **kwargs):
                    if label == "install":
                        return runner.artifact_command(argv, cwd, label, b"up to date\n", 0), b"up to date\n"
                    if label == "run":
                        report = Path(next(arg.split("=", 1)[1] for arg in argv if arg.startswith("--outputFile=")))
                        document = web_report(status)
                        document["testResults"][0]["name"] = str(cwd / "src/App.test.tsx")
                        report.write_text(json.dumps(document))
                        return runner.artifact_command(argv, cwd, label, b"JSON report written\n", code), b"JSON report written\n"
                    return source_execute(argv, cwd, label, **kwargs)

                executed.side_effect = execute
                result = runner(SHA, candidate())
            expected = "pass" if code == 0 else "fail"
            self.assertEqual(result["state"], expected, result)
            self.assertEqual(clients.verify_probe(result, candidate(), self.output)["state"], expected)
            changed = copy.deepcopy(result)
            changed["run"]["argv"][-1] = "--passWithNoTests"
            self.assertEqual(clients.verify_probe(changed, candidate(), self.output)["state"], "unknown")
            report = self.output / result["report"]["log"]
            report.write_text(json.dumps(web_report("skipped")))
            changed = copy.deepcopy(result)
            changed["report"].update(sha256=clients.digest(report.read_bytes()), bytes=report.stat().st_size)
            self.assertEqual(clients.verify_probe(changed, candidate(), self.output)["state"], "unknown")

    def test_session_reuses_only_private_per_client_npm_cache(self):
        first, second = self.probe(), self.probe()
        observed, per_client, homes, worktrees = [], {}, set(), set()
        for runner, client in ((first, "web"), (first, "bridge"), (first, "web"), (first, "bridge"), (second, "web")):
            node, npm = "/fixture/node", "/fixture/npm-cli.js"

            def tools(client, home, result):
                version = b"v22.23.5\n" if client == "web" else b"v24.13.0\n"
                result["toolchain"] = {"node": node, "npm": npm, "node_sha256": "c" * 64, "npm_sha256": "d" * 64,
                    "node_version": runner.artifact_command([node, "--version"], home, "node-version", version, 0),
                    "npm_version": runner.artifact_command([node, npm, "--version"], home, "npm-version", b"10.9.9\n", 0)}
                return node, npm

            with self.snapshot(runner, "// fixture", client=client) as executed, patch.object(runner, "tools", side_effect=tools):
                source_execute = executed.side_effect

                def execute(argv, cwd, label, **kwargs):
                    if label == "install":
                        expected = [node, npm, "ci"] + ([] if client == "web" else ["--ignore-scripts", "--no-audit", "--no-fund"])
                        self.assertEqual(argv, expected)
                        self.assertNotIn("npm_config_ignore_scripts", runner.env)
                        self.assertNotIn(runner.env["HOME"], homes)
                        homes.add(runner.env["HOME"])
                        self.assertNotIn(str(cwd), worktrees)
                        worktrees.add(str(cwd))
                        self.assertFalse((cwd / "node_modules").exists())
                        cache = Path(runner.env["npm_config_cache"])
                        self.assertTrue(cache.is_dir())
                        self.assertEqual(cache.stat().st_mode & 0o777, 0o700)
                        self.assertTrue(cache.is_relative_to(Path(runner.home.name).resolve()))
                        self.assertFalse(cache.is_relative_to(Path(runner.env["HOME"])))
                        self.assertTrue(runner.mutex.locked())
                        key = (id(runner), client)
                        if key in per_client:
                            self.assertEqual(cache, per_client[key])
                            self.assertEqual((cache / "fixture-integrity").read_bytes(), b"cached bytes")
                        else:
                            self.assertNotIn(cache, per_client.values())
                            per_client[key] = cache
                            (cache / "fixture-integrity").write_bytes(b"cached bytes")
                        observed.append(cache)
                        (cwd / "node_modules").mkdir()
                        (cwd / "node_modules/not-shared").write_text("fixture")
                        return runner.artifact_command(argv, cwd, label, b"installed\n", 0), b"installed\n"
                    if label == "run":
                        if client == "web":
                            report = Path(next(arg.split("=", 1)[1] for arg in argv if arg.startswith("--outputFile=")))
                            document = web_report("passed")
                            document["testResults"][0]["name"] = str(cwd / "src/App.test.tsx")
                            report.write_text(json.dumps(document))
                            raw = b"JSON report written\n"
                        else:
                            raw = tap("pass").encode()
                        return runner.artifact_command(argv, cwd, label, raw, 0), raw
                    return source_execute(argv, cwd, label, **kwargs)

                executed.side_effect = execute
                result = runner(SHA, candidate(client))
            self.assertEqual(result["state"], "pass", result)
            self.assertFalse(Path(result["checkout"]).exists())
            self.assertEqual(clients.verify_probe(result, candidate(client), self.output), {"state": "pass"})
            changed = copy.deepcopy(result)
            changed["install"]["argv"] = [node, npm, "ci"] + (["--ignore-scripts", "--no-audit", "--no-fund"] if client == "web" else [])
            self.assertEqual(clients.verify_probe(changed, candidate(client), self.output)["state"], "unknown")
        self.assertEqual(len(set(observed)), 3)
        first_cache = observed[0]
        first.close()
        self.assertFalse(first_cache.exists())

    def test_absent_file_is_source_proof_but_missing_lockfile_is_unknown(self):
        runner = self.probe()
        with self.snapshot(runner, "", missing=True):
            result = runner(SHA, candidate("bridge"))
        self.assertEqual(result["state"], "absent", result)
        self.assertEqual(result["definition"]["state"], "absent")
        self.assertEqual(clients.verify_probe(result, candidate("bridge"), self.output), {"state": "absent"})
        shared = set()
        self.assertEqual(clients.verify_probe(result, candidate("bridge"), self.output, seen=shared), {"state": "absent"})
        self.assertEqual(clients.verify_probe(result, candidate("bridge"), self.output, seen=shared)["state"], "unknown")
        with self.snapshot(runner, "", missing=True, lock=False):
            self.assertEqual(runner(SHA, candidate("bridge"))["state"], "unknown")

    def test_verifier_rechecks_trusted_tree_and_inventory_without_checkout(self):
        runner = self.probe()
        with self.snapshot(runner, "", missing=True):
            result = runner(SHA, candidate("bridge"))

        def trusted(argv, cwd, env, path, timeout, limit):
            label = path.stem
            evidence = result["source"][label]
            raw = (self.output / evidence["log"]).read_bytes()
            self.assertNotIn("GH_TOKEN", env)
            self.assertEqual(Path(cwd).resolve(), self.repository.resolve())
            return {"returncode": 0}, raw

        with patch.object(clients, "process_to_file", side_effect=trusted) as execute:
            self.assertEqual(runner.verify_probe(result, candidate("bridge"), self.output), {"state": "absent"})
            self.assertEqual(execute.call_count, 2)
        with patch.object(clients, "process_to_file", return_value=({"returncode": 0}, b"other tree")):
            self.assertEqual(runner.verify_probe(result, candidate("bridge"), self.output)["state"], "unknown")

    def test_toolchain_is_explicit_and_wrong_version_is_unknown(self):
        runner = self.probe()
        source = 'import {test} from "node:test";test("exact 我 title",()=>{});'
        with patch.dict(os.environ, {}, clear=True), self.snapshot(runner, source):
            result = runner(SHA, candidate("bridge"))
        self.assertEqual(result["state"], "unknown")
        self.assertIn("PERIODIC_BRIDGE_NODE", result["reason"])
        self.assertIn("24.13.0", result["reason"])
        npm = self.root / "npm-cli.js"
        npm.write_text("// fixture")
        with patch.dict(os.environ, {"PERIODIC_BRIDGE_NODE": sys.executable, "PERIODIC_BRIDGE_NPM": str(npm)}), self.snapshot(runner, source):
            wrong = runner(SHA, candidate("bridge"))
        self.assertEqual(wrong["state"], "unknown")
        self.assertIn("24.13.0", wrong["reason"])
        self.assertNotIn("install", wrong)

    def test_output_lock_prevents_shared_probe_evidence(self):
        runner = self.probe()
        fd = os.open(self.output / ".periodic-clients.lock", os.O_WRONLY | os.O_CREAT, 0o600)
        self.addCleanup(os.close, fd)
        clients.fcntl.flock(fd, clients.fcntl.LOCK_EX | clients.fcntl.LOCK_NB)
        with patch.object(runner, "execute") as executed:
            result = runner(SHA, candidate("bridge"))
            executed.assert_not_called()
        self.assertEqual(result["state"], "unknown")
        self.assertIn("证据目录", result["reason"])

    def test_budget_output_and_timeout_are_unknown_and_bounded(self):
        runner = self.probe()
        runner.deadline = 0
        self.assertEqual(runner(SHA, candidate("bridge"))["state"], "unknown")
        runner = self.probe()
        with patch.object(clients, "MAX_LOG", 1024):
            record, data = runner.execute([sys.executable, "-c", "print('x'*4096)"], self.root, "run")
        self.assertEqual(record["state"], "unknown")
        self.assertLessEqual((self.output / record["log"]).stat().st_size, 1025)
        runner.timeout = 0.05
        record, _ = runner.execute([sys.executable, "-c", "import time;time.sleep(5)"], self.root, "run")
        self.assertEqual(record["state"], "unknown")
        self.assertTrue(record["timed_out"])
        runner.output_bytes = clients.MAX_TOTAL_OUTPUT
        with patch.object(clients.subprocess, "Popen") as spawn:
            record, _ = runner.execute([sys.executable, "--version"], self.root, "run")
            spawn.assert_not_called()
        self.assertEqual(record["state"], "unknown")

    def test_process_group_is_reaped_after_output_io_error(self):
        runner = self.probe()
        with patch.object(clients.subprocess, "Popen") as spawn, patch.object(clients.os, "killpg") as kill:
            process = spawn.return_value
            process.pid = 123
            with patch.object(Path, "stat", side_effect=OSError("fixture I/O error")), self.assertRaises(OSError):
                runner.execute(["/fixture/node", "--test"], self.root, "run")
            kill.assert_called_once_with(123, clients.signal.SIGKILL)
            process.wait.assert_called_once()

    def test_unsafe_candidate_never_spawns(self):
        runner = self.probe()
        bad = candidate("bridge")
        bad["file"] = "extensions/pi-bridge/tests/../../secret.test.mjs"
        with patch.object(runner, "execute") as execute:
            self.assertEqual(runner(SHA, bad)["state"], "unknown")
            execute.assert_not_called()

    def test_proof_needs_external_bytes_and_exact_command(self):
        runner = self.probe()
        with self.snapshot(runner, "", missing=True):
            result = runner(SHA, candidate("bridge"))
        bad = copy.deepcopy(result)
        bad["state"] = "pass"
        self.assertEqual(clients.verify_probe(bad, candidate("bridge"), self.output)["state"], "unknown")
        bad = copy.deepcopy(result)
        bad["source"]["archive"]["log"] = "../secret"
        self.assertEqual(clients.verify_probe(bad, candidate("bridge"), self.output)["state"], "unknown")
        bad = copy.deepcopy(result)
        bad["source"]["inventory"]["argv"].append("--other-file")
        self.assertEqual(clients.verify_probe(bad, candidate("bridge"), self.output)["state"], "unknown")


if __name__ == "__main__":
    unittest.main()
