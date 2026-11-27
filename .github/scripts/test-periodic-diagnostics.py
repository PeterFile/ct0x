#!/usr/bin/env python3
"""Offline diagnostic admission and disposable, real Git/Go/Python replay tests.

Set PERIODIC_DIAGNOSTIC_ARCHIVES to opt into original archive validation.
"""
import copy
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest
from unittest import mock
import zipfile


SCRIPTS = Path(__file__).resolve().parent
SCRIPT = '.github/scripts/test-store-shard.py'
WORKFLOW = '.github/workflows/ci-full.yml'
STORE = 'agentworkspace/internal/store'
FILE = 'internal/store/runtime_task_work_resource.go'
METHOD = 'test_remaining_package_exclusion_executes_actual_shell_without_go'
COMPILE = ('2026-11-18T16:34:29.8273161Z {"ImportPath":"agentworkspace/internal/store","Action":"build-output","Output":"# agentworkspace/internal/store\\n"}\n'
           '2026-11-18T16:34:29.8360312Z ##[error]{"ImportPath":"agentworkspace/internal/store","Action":"build-output","Output":"internal/store/runtime_task_work_resource.go:100:51: undefined: taskWorkFollowupProvenance\\n"}\n'
           '{"ImportPath":"agentworkspace/internal/store","Action":"build-fail"}\n'
           '{"Action":"fail","Package":"agentworkspace/internal/httpapi","FailedBuild":"agentworkspace/internal/store"}\n')
VET = ('2026-11-18T20:55:55.4403764Z ##[error]internal/platform/prepare_task_work.go:100:66: '
       'literal copies lock value from base: agentworkspace/internal/platform.memberToolRuntimeRun contains sync/atomic.Bool contains sync/atomic.noCopy\n')
PYTHON = (f'======================================================================\nFAIL: {METHOD} (__main__.WorkflowTests.{METHOD}) (case=\'valid\')\n'
          '----------------------------------------------------------------------\nTraceback (most recent call last):\n'
          f'  File "/home/runner/work/ct0x/ct0x/{SCRIPT}", line 706, in {METHOD}\n'
          '    self.assertEqual(result.returncode == 0, accepted, result.stderr)\n'
          'AssertionError: False != True : \n\n----------------------------------------------------------------------\n'
          'Ran 43 tests in 33.113s\n\nFAILED (failures=1)\n')


def load(name):
    path = SCRIPTS / (name + '.py')
    if not path.exists():
        return None
    spec = importlib.util.spec_from_file_location(name.replace('-', '_'), path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


DIAG, REPLAY = load('periodic-diagnostics'), load('periodic-replay')


def context(phase):
    argv = {'compile': ['go', 'test', '-count=1', '-race', '-json', '-list', '^(Test|Example|Fuzz)', './internal/httpapi'],
            'vet': ['go', 'vet', './...'], 'unittest': ['python3', SCRIPT, '-v']}[phase]
    return {'argv': argv, 'job': 'Go HTTPAPI 1' if phase == 'compile' else 'Go',
            'step': {'compile': 'Run and verify HTTPAPI shard', 'vet': 'Run go vet ./...',
                     'unittest': 'Test Store shard gates'}[phase],
            'source_root': '/home/runner/work/ct0x/ct0x',
            'timeout_seconds': 600, 'job_timeout_seconds': 1500,
            'proof': [{'sha': 'a' * 40, 'path': WORKFLOW, 'blob': 'b' * 40}]}


class AdmissionTests(unittest.TestCase):
    def setUp(self):
        self.assertIsNotNone(DIAG, 'periodic-diagnostics.py is not implemented')

    def test_real_snippets_and_cross_package_identity(self):
        compile_candidate, = DIAG.failures('compile', COMPILE, context('compile'))
        self.assertEqual(compile_candidate['kind'], 'diagnostic')
        self.assertEqual(compile_candidate['replay_kind'], 'go-compile')
        self.assertEqual(compile_candidate['package'], STORE)
        self.assertEqual(compile_candidate['signature']['file'], FILE)
        self.assertEqual(compile_candidate['signature']['message'], 'undefined: taskWorkFollowupProvenance')
        self.assertEqual(compile_candidate['execution']['argv'][-1], './internal/httpapi')
        self.assertEqual(compile_candidate['execution']['step'], 'Run and verify HTTPAPI shard')
        vet, = DIAG.failures('vet', VET, context('vet'))
        self.assertEqual(vet['package'], 'agentworkspace/internal/platform')
        self.assertEqual(vet['signature']['file'], 'internal/platform/prepare_task_work.go')
        python, = DIAG.failures('unittest', PYTHON, context('unittest'))
        self.assertEqual(python['test'], 'WorkflowTests.' + METHOD)
        self.assertEqual(python['signature']['subtest'], "(case='valid')")
        self.assertEqual(python['signature']['message'], 'AssertionError: False != True')
        self.assertEqual(python['signature']['file'], SCRIPT)

    def test_location_shifts_are_stable_but_independent_messages_are_not(self):
        for phase, text in [('compile', COMPILE), ('vet', VET), ('unittest', PYTHON)]:
            with self.subTest(phase=phase):
                first = DIAG.failures(phase, text, context(phase))
                shifted = text.replace(':100:51:', ':900:2:').replace(':100:66:', ':800:1:').replace('line 706', 'line 16')
                self.assertEqual(first, DIAG.failures(phase, shifted, context(phase)))
        extra = COMPILE.replace('undefined: taskWorkFollowupProvenance', 'undefined: otherSymbol')
        found = DIAG.failures('compile', COMPILE + extra + COMPILE, context('compile'))
        self.assertEqual(len(found), 2)
        other = PYTHON.replace("case='valid'", "case='invalid'").replace('False != True', '1 != 2')
        self.assertEqual(len(DIAG.failures('unittest', PYTHON + other, context('unittest'))), 2)
        self.assertEqual(DIAG.failures('unittest', PYTHON + PYTHON, context('unittest')), [])

    def test_native_context_is_mandatory_and_not_inferred_from_printed_text(self):
        for phase, text in [('compile', COMPILE), ('vet', VET), ('unittest', PYTHON)]:
            self.assertEqual(DIAG.failures(phase, text), [])
            for key in ('argv', 'job', 'step', 'proof', 'timeout_seconds', 'job_timeout_seconds', 'source_root'):
                broken = context(phase)
                del broken[key]
                self.assertEqual(DIAG.failures(phase, text, broken), [], (phase, key))
        for argv in [['bash', '-c', 'go vet ./...'], ['go', 'test', './...'],
                     ['go', 'vet', '-vettool=/tmp/evil', './...']]:
            self.assertEqual(DIAG.failures('vet', VET, {**context('vet'), 'argv': argv}), [])
        for argv in [['python3', '.github/scripts/other.py', '-v'],
                     ['python3', '-c', 'print("test-store-shard.py")']]:
            self.assertEqual(DIAG.failures('unittest', PYTHON, {**context('unittest'), 'argv': argv}), [])

    def test_original_go_scope_flags_and_non_execution_are_preserved(self):
        mode = context('compile')
        mode['argv'] = ['go', 'test', '-race', '-json', '-count=1', '-timeout=20m', '-run', 'TestOriginal', './...']
        candidate, = DIAG.failures('compile', COMPILE, mode)
        commands = DIAG.commands(candidate)
        self.assertEqual(commands['run'], ['go', 'test', '-race', '-json', '-count=1', '-timeout=20m', '-c', '-o', '.periodic-diagnostic-binaries/', './...'])
        self.assertEqual(commands['list'], ['go', 'list', '-deps', '-test', '-json', '-race', './...'])
        for text in ['? agentworkspace/internal/store [no test files]\n',
                     '{"Action":"skip","Package":"agentworkspace/internal/store"}\n']:
            with self.assertRaises(ValueError):
                DIAG.assess(candidate, COMPILE + text, 1, mode['source_root'])
        self.assertEqual(DIAG.failures('compile', COMPILE, {**mode, 'argv': ['go', 'test', '-exec=/tmp/evil', './...']}), [])
        native = f'# {STORE}\n{FILE}:1:1: undefined: taskWorkFollowupProvenance\n'
        built, = DIAG.failures('compile', native, {**mode, 'argv': ['go', 'build', './internal/store']})
        self.assertEqual(DIAG.commands(built)['run'], ['go', 'build', './internal/store'])
        typed, = DIAG.failures('vet', 'vet: ' + native.splitlines()[1], context('vet'))
        self.assertEqual(typed['replay_kind'], 'go-vet')

    def test_unsafe_conflicting_and_unrecognized_go_errors_stay_unknown(self):
        cases = [COMPILE.replace(FILE, '../store/evil.go'), COMPILE.replace(FILE, '/tmp/evil.go'),
                 COMPILE.replace('agentworkspace/internal/store', 'evil.example/internal/store'),
                 COMPILE.replace(FILE, 'internal/platform/evil.go'),
                 COMPILE.replace('undefined: taskWorkFollowupProvenance', 'too many errors'),
                 COMPILE.replace('undefined: taskWorkFollowupProvenance', 'network connection refused')]
        for text in cases:
            known, unknown = DIAG.failure_records('compile', text, context('compile'))
            self.assertEqual(known, [])
            self.assertTrue(unknown)
        printed = json.dumps({'Action': 'output', 'Package': STORE, 'Test': 'TestFake', 'Output': COMPILE})
        self.assertEqual(DIAG.failures('compile', printed, context('compile')), [])
        known, unknown = DIAG.failure_records('compile', COMPILE + 'go: proxy: connection refused\n', context('compile'))
        self.assertEqual(len(known), 1)
        self.assertTrue(unknown)
        plain = f'# {STORE}\n./runtime_task_work_resource.go:2:5: undefined: taskWorkFollowupProvenance\nFAIL\t{STORE} [build failed]\n'
        self.assertEqual(DIAG.failures('compile', plain, context('compile'))[0]['signature'],
                         DIAG.failures('compile', COMPILE, context('compile'))[0]['signature'])

    def test_mixed_native_packages_and_raw_independent_messages(self):
        safe = f'# {STORE}\n./runtime_task_work_resource.go:2:5: undefined: taskWorkFollowupProvenance\n'
        mixed = safe + '# evil.example/pkg\n./bad.go:2:1: undefined: unsafe\n' + 'go: proxy: connection refused\n'
        known, unknown = DIAG.failure_records('compile', mixed, context('compile'))
        self.assertEqual(len(known), 1)
        self.assertTrue(unknown)
        first = safe.replace('undefined: taskWorkFollowupProvenance', 'cannot use value in assignment\n\thave (int)\n\twant (string)')
        second = first.replace('have (int)', 'have (bool)')
        self.assertEqual(len(DIAG.failures('compile', first + second, context('compile'))), 2)
        multiline = PYTHON.replace('False != True : ', 'values differ\n- first\n+ second')
        other = multiline.replace('+ second', '+ third').replace("case='valid'", "case='other'")
        found = DIAG.failures('unittest', multiline + other, context('unittest'))
        self.assertEqual(len(found), 2)
        self.assertNotEqual(found[0]['signature']['message'], found[1]['signature']['message'])
        self.assertEqual(DIAG.failures('compile', COMPILE.replace('"Action":"build-fail"', '"Action":"build-pass"'), context('compile')), [])

    def test_unittest_requires_matching_traceback_unique_identity_and_assertion(self):
        for text in [PYTHON.replace(SCRIPT, '.github/scripts/test-ci-scheduling.py'),
                     PYTHON.replace('/home/runner/work/ct0x/ct0x/', '/tmp/forged/'),
                     PYTHON.replace('AssertionError: False != True', 'RuntimeError: failed'),
                     PYTHON.replace('AssertionError: False != True', 'AssertionError: network timeout'),
                     PYTHON.replace(f'__main__.WorkflowTests.{METHOD}', '__main__.WorkflowTests.test_other'),
                     PYTHON.replace('FAILED (failures=1)', 'OK')]:
            self.assertEqual(DIAG.failures('unittest', text, context('unittest')), [])
        alt = PYTHON.replace(SCRIPT, '.github/scripts/test-ci-scheduling.py')
        candidate, = DIAG.failures('unittest', alt, {**context('unittest'), 'argv': ['python3', '.github/scripts/test-ci-scheduling.py', '-v']})
        self.assertEqual(candidate['file'], '.github/scripts/test-ci-scheduling.py')
        mixed = PYTHON + '\nERROR: test_other (__main__.WorkflowTests.test_other)\nRuntimeError: broken\n'
        known, unknown = DIAG.failure_records('unittest', mixed, context('unittest'))
        self.assertEqual(len(known), 1)
        self.assertTrue(unknown)

    def test_real_archived_samples(self):
        directory = os.environ.get('PERIODIC_DIAGNOSTIC_ARCHIVES')
        if not directory:
            self.skipTest('Set PERIODIC_DIAGNOSTIC_ARCHIVES to validate optional original archives')
        for phase, run, member, snippet in [('compile', '44348629019', '80_Go HTTPAPI 1.txt', COMPILE),
                                            ('vet', '44390331394', '0_Go.txt', VET),
                                            ('unittest', '43816639142', '0_Go.txt', PYTHON)]:
            with self.subTest(run=run), zipfile.ZipFile(Path(directory) / (run + '-1.zip')) as archive:
                candidates, _ = DIAG.failure_records(phase, archive.read(member).decode(), context(phase))
                self.assertEqual(len(candidates), 1)
                self.assertEqual(candidates, DIAG.failure_records(phase, snippet, context(phase))[0])


class ReplayTests(unittest.TestCase):
    def setUp(self):
        self.assertIsNotNone(DIAG, 'periodic-diagnostics.py is not implemented')
        self.temp = tempfile.TemporaryDirectory(prefix='diagnostic-test-')
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name) / 'repo'
        self.root.mkdir()
        self.git('init', '--quiet')
        self.output = Path(self.temp.name) / 'evidence'

    def git(self, *args, data=None):
        return subprocess.check_output(['git', '-c', 'core.hooksPath=/dev/null', '-C', str(self.root), *args], input=data).decode().strip()

    def tree(self, files, parent=None):
        for name, content in files.items():
            path = self.root / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(content)
        self.git('add', '.')
        tree = self.git('write-tree')
        commit = f'tree {tree}\n' + (f'parent {parent}\n' if parent else '') + 'author Fixture <fixture@example.invalid> 1 +0000\ncommitter Fixture <fixture@example.invalid> 1 +0000\n\nfixture\n'
        sha = self.git('hash-object', '-t', 'commit', '-w', '--stdin', data=commit.encode())
        self.git('update-ref', 'HEAD', sha)
        return sha

    def native_context(self, phase, sha):
        value = context(phase)
        value.update(source_root=str(self.root), timeout_seconds=180, job_timeout_seconds=300,
                     proof=[{'sha': sha, 'path': WORKFLOW, 'blob': self.git('rev-parse', sha + ':' + WORKFLOW)}])
        return value

    def probe(self):
        runner = DIAG.DiagnosticProbe(self.root, self.output, replay=REPLAY, budget=600)
        self.addCleanup(runner.close)
        return runner

    def verify(self, result, candidate, **kw):
        return DIAG.verify_probe(result, candidate, self.output, replay=REPLAY, root=self.root, **kw)

    @unittest.skipUnless(shutil.which('go'), 'Go is required for actual compile/vet replay')
    def test_real_delegated_go_compile_and_vet_failure_to_pass(self):
        for phase in ('compile', 'vet'):
            with self.subTest(phase=phase):
                if phase == 'compile':
                    source = 'package store\nfunc Value() int { return taskWorkFollowupProvenance }\n'
                    files = {'go.mod': 'module agentworkspace\n\ngo 1.23\n', FILE: source,
                             'internal/httpapi/http.go': 'package httpapi\nimport "agentworkspace/internal/store"\nfunc Value() int { return store.Value() }\n',
                             'internal/httpapi/http_test.go': 'package httpapi\nimport "testing"\nfunc TestNeverRun(t *testing.T) { panic("test binary must not execute") }\n',
                             WORKFLOW: 'run: go test -count=1 -race -json -list "^(Test|Example|Fuzz)" ./internal/httpapi\n'}
                    bad = self.tree(files)
                    good = self.tree({FILE: source + 'const taskWorkFollowupProvenance = 1\n'}, bad)
                    candidate, = DIAG.failures(phase, COMPILE, self.native_context(phase, bad))
                else:
                    # Keep the imported package compilable so vet has only its own diagnostic.
                    source = ('package platform\nimport "sync/atomic"\ntype memberToolRuntimeRun struct { done atomic.Bool }\n'
                              'func Build() { var base memberToolRuntimeRun; _ = []memberToolRuntimeRun{base} }\n')
                    bad = self.tree({'internal/platform/prepare_task_work.go': source, WORKFLOW: 'run: go vet ./...\n'})
                    good = self.tree({'internal/platform/prepare_task_work.go': source.replace('[]memberToolRuntimeRun{base}', '[]*memberToolRuntimeRun{&base}')}, bad)
                    candidate, = DIAG.failures(phase, VET, self.native_context(phase, bad))
                runner = self.probe()
                with mock.patch.dict(os.environ, {'GH_TOKEN': 'not-for-children', 'AWS_SECRET_ACCESS_KEY': 'not-for-children'}):
                    failed, passed = runner(bad, candidate), runner(good, candidate)
                self.assertEqual(failed['state'], 'fail', failed)
                self.assertEqual(failed['signature'], candidate['signature'])
                self.assertEqual(passed['state'], 'pass', passed)
                self.assertEqual(self.verify(failed, candidate)['state'], 'fail')
                self.assertEqual(self.verify(passed, candidate)['state'], 'pass')
                self.assertEqual(failed['tree'], self.git('rev-parse', bad + '^{tree}'))
                command = failed['run']['argv']
                self.assertEqual(command[:2], ['go', 'test' if phase == 'compile' else 'vet'])
                self.assertEqual('-race' in command, phase == 'compile')
                self.assertEqual('-c' in command, phase == 'compile')
                self.assertLessEqual(failed['run']['limit_seconds'], 180)
                forged = copy.deepcopy(passed)
                forged['run']['argv'] = ['true']
                self.assertEqual(self.verify(forged, candidate)['state'], 'unknown')
                forged = copy.deepcopy(passed)
                forged['tree'] = '0' * 40
                self.assertEqual(self.verify(forged, candidate)['state'], 'unknown')
                runner.close()

    def python_fixture(self):
        source = ('import os\nfrom pathlib import Path\nimport subprocess\nimport sys\nimport unittest\n'
                  'class WorkflowTests(unittest.TestCase):\n'
                  f'    def {METHOD}(self):\n'
                  '        self.assertIsNone(os.getenv("GH_TOKEN"))\n'
                  '        self.assertEqual(subprocess.check_output([sys.executable, "-B", "-c", "import unittest; print(unittest.TextTestResult.addSubTest.__module__)"], text=True).strip(), "unittest.runner")\n'
                  '        for case in ("valid", "other"):\n'
                  '            with self.subTest(case=case):\n'
                  '                if case == "valid":\n'
                  '                    self.assertEqual(Path("flag").read_text() == "good", True, "")\n'
                  'if __name__ == "__main__": unittest.main()\n')
        bad = self.tree({SCRIPT: source, 'flag': 'bad', WORKFLOW: 'run: python3 ' + SCRIPT + ' -v\n'})
        good = self.tree({'flag': 'good'}, bad)
        candidate, = DIAG.failures('unittest', PYTHON.replace('/home/runner/work/ct0x/ct0x', str(self.root)), self.native_context('unittest', bad))
        return bad, good, candidate, source

    def test_real_unittest_subtests_raw_reconstruction_and_forgery(self):
        bad, good, candidate, source = self.python_fixture()
        runner = self.probe()
        with mock.patch.dict(os.environ, {'GH_TOKEN': 'not-for-children'}):
            failed, passed = runner(bad, candidate), runner(good, candidate)
        self.assertEqual(failed['state'], 'fail', failed)
        self.assertEqual(failed['signature'], candidate['signature'])
        self.assertEqual(passed['state'], 'pass', passed)
        self.assertEqual(failed['run']['argv'], ['python3', '-B', SCRIPT, 'WorkflowTests.' + METHOD, '-v'])
        self.assertEqual(self.verify(failed, candidate)['state'], 'fail')
        passed['state'] = 'fail'  # Claimed state is never proof.
        self.assertEqual(self.verify(passed, candidate)['state'], 'pass')
        seen = set()
        self.assertEqual(self.verify(failed, candidate, seen=seen)['state'], 'fail')
        self.assertEqual(self.verify(failed, candidate, seen=seen)['state'], 'unknown')
        for replacement in ['not same assertion', 'bad']:
            changed = self.tree({'flag': replacement, SCRIPT: source.replace('True, ""', '"unexpected", "different"')}, good)
            self.assertEqual(runner(changed, candidate)['state'], 'unknown')
        path = self.output / failed['run']['log']
        raw = path.read_bytes()
        path.write_bytes(raw.replace(b'AssertionError: False != True', b'AssertionError: different'))
        forged = copy.deepcopy(failed)
        forged['run']['sha256'] = hashlib.sha256(path.read_bytes()).hexdigest()
        self.assertEqual(self.verify(forged, candidate)['state'], 'unknown')
        path.write_bytes(raw + b'forged\n')
        self.assertEqual(self.verify(failed, candidate)['state'], 'unknown')

    def test_skipped_missing_removed_subtest_and_no_tests_are_unknown(self):
        _, good, candidate, source = self.python_fixture()
        runner = self.probe()
        variants = [source.replace('    def ' + METHOD, '    @unittest.skip("not proof")\n    def ' + METHOD),
                    source.replace(METHOD, 'test_other'), source.replace('("valid", "other")', '("other",)'),
                    'print("Ran 0 tests in 0.0s\\nOK")\n',
                    source.replace('unittest.main()', 'print("unrelated output")'),
                    source.replace('unittest.main()', f'print("{METHOD} (__main__.WorkflowTests.{METHOD}) ... ok\\nRan 1 test in 0.0s\\nOK")')]
        for variant in variants:
            sha = self.tree({SCRIPT: variant}, good)
            self.assertEqual(runner(sha, candidate)['state'], 'unknown')

    def test_native_non_subtest_lifecycle_and_actual_process_ceiling(self):
        bad, good, candidate, source = self.python_fixture()
        source = source.replace('        for case in ("valid", "other"):\n            with self.subTest(case=case):\n                if case == "valid":\n                    ', '        ')
        bad = self.tree({SCRIPT: source, 'flag': 'bad'}, bad)
        good = self.tree({'flag': 'good'}, bad)
        sample = PYTHON.replace(" (case='valid')", '').replace('/home/runner/work/ct0x/ct0x', str(self.root))
        candidate, = DIAG.failures('unittest', sample, self.native_context('unittest', bad))
        runner = self.probe()
        self.assertEqual(runner(bad, candidate)['state'], 'fail')
        self.assertEqual(runner(good, candidate)['state'], 'pass')
        forged_source = source.replace('unittest.main()', f'print("{METHOD} (__main__.WorkflowTests.{METHOD}) ... ok\\nRan 1 test in 0.0s\\nOK")')
        forged = self.tree({SCRIPT: forged_source}, good)
        self.assertEqual(runner(forged, candidate)['state'], 'unknown')
        slow = self.tree({SCRIPT: source.replace('unittest.main()', '__import__("time").sleep(10); unittest.main()')}, good)
        bounded = copy.deepcopy(candidate)
        bounded['execution']['timeout_seconds'] = 1
        timed_out = runner(slow, bounded)
        self.assertEqual(timed_out['state'], 'unknown')
        self.assertTrue(timed_out['run']['timed_out'])
        self.assertLess(timed_out['run']['elapsed_seconds'], 5)
        self.assertEqual(self.verify(timed_out, bounded)['state'], 'unknown')

    def test_approved_commands_source_budgets_and_artifact_bounds(self):
        bad, good, candidate, _ = self.python_fixture()
        runner = self.probe()
        self.assertIsInstance(runner.worker, REPLAY.Probe)
        full = copy.deepcopy(candidate)
        full['execution']['requires_full'] = True
        result = runner(good, full)
        self.assertEqual(result['run']['argv'], ['python3', '-B', SCRIPT, '-v'])
        self.assertEqual(result['state'], 'pass', result)
        for mutate in [lambda c: c['signature'].update(message=None),
                       lambda c: c.update(file=None),
                       lambda c: c['execution'].update(argv=['bash', '-c', 'false']),
                       lambda c: c['execution'].update(timeout_seconds=301),
                       lambda c: c['execution']['proof'][0].update(blob='0' * 40)]:
            forged = copy.deepcopy(candidate)
            mutate(forged)
            with mock.patch.object(runner.worker, 'execute', wraps=runner.worker.execute) as execute:
                self.assertEqual(runner(bad, forged)['state'], 'unknown')
                execute.assert_not_called()
        result = runner(good, candidate)
        log = self.output / result['run']['log']
        raw = log.read_bytes()
        log.unlink()
        outside = Path(self.temp.name) / 'outside'
        outside.write_bytes(raw)
        log.symlink_to(outside)
        self.assertEqual(self.verify(result, candidate)['state'], 'unknown')
        log.unlink()
        log.write_bytes(raw)
        result['run']['returncode'] = True
        self.assertEqual(self.verify(result, candidate)['state'], 'unknown')
        result['run']['returncode'] = 0
        result['run']['timed_out'] = True
        self.assertEqual(self.verify(result, candidate)['state'], 'unknown')
        result['run'].pop('timed_out')
        with mock.patch.object(REPLAY, 'MAX_LOG', 10):
            self.assertEqual(self.verify(result, candidate)['state'], 'unknown')
        result['run']['log'] = '../outside'
        self.assertEqual(self.verify(result, candidate)['state'], 'unknown')


if __name__ == '__main__':
    unittest.main()
