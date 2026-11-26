"""Read-only GitHub Actions experiment using an explicit, hashed Go test overlay."""
import argparse
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys

GOOD = '9a45793e82ba00b7015ba565f9f90aea2c68694a'
BAD = '18d11a3c0176fe98856aeeef6396e9ddfde0cca0'
EXPECTED_PARENT = '7d70dc49ff1f3c3ae3876b2a519059c60d88ec3d'
REPO = 'PeterFile/ct0x'
PACKAGE = 'agentworkspace/internal/artifactfs'
TEST = 'TestPeriodicFaultInjection'
SIGNATURE = 'periodic controlled assertion: separate workspace race job is present'
FIXTURE = '''package artifactfs

import (
    "bytes"
    "os"
    "testing"
)

func TestPeriodicFaultInjection(t *testing.T) {
    data, err := os.ReadFile("../../.github/workflows/ci-full.yml")
    if err != nil { t.Fatal(err) }
    if bytes.Contains(data, []byte("  go-workspace:")) {
        t.Fatal("periodic controlled assertion: separate workspace race job is present")
    }
}
'''

def module(path):
    spec = importlib.util.spec_from_file_location(path.stem.replace('-', '_'), path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod

ROOT = Path.cwd()
POLICY = module(ROOT / '.github/scripts/periodic-full.py')
REPLAY = module(ROOT / '.github/scripts/periodic-replay.py')


def write(path, value):
    path.write_text(json.dumps(value, indent=2, sort_keys=True) + '\n')


def require(value, msg):
    if not value:
        raise ValueError(msg)


def controls():
    return {key: hashlib.sha256((ROOT / path).read_bytes()).hexdigest() for key, path in {
        'policy_sha256':'.github/scripts/periodic-full.py', 'replay_sha256':'.github/scripts/periodic-replay.py'}.items()}


def prove(args):
    api = POLICY.API(args.output / 'api-good')
    evidence = POLICY.Evidence(api, REPO, lambda value: subprocess.run(['git','merge-base','--is-ancestor',value,BAD]).returncode == 0)
    proof = evidence.verify(45801293592, 1)
    require(proof['sha'] == GOOD and proof['verified'] is True, 'missing original green Full evidence')
    require(REPLAY.git(ROOT, 'rev-parse', BAD+'^1') == EXPECTED_PARENT, 'target first parent differs')
    write(args.output / 'baseline.json', proof)


def replay(args):
    source = args.output / 'fault_test.go'
    source.write_text(FIXTURE)
    executable = str(Path(shutil.which('go')).resolve())
    wrapper = args.output / 'bin' / 'go'
    wrapper.parent.mkdir(exist_ok=True)
    wrapper.write_text('#!/usr/bin/env python3\n' + 'import json, os, sys, tempfile\nfrom pathlib import Path\n' +
                       f'real = {executable!r}\nfixture = {str(source)!r}\n' +
                       'assert sys.argv[1] == "test"\nwith tempfile.NamedTemporaryFile(mode="w", suffix=".json", delete=False) as fd:\n' +
                       '    json.dump({"Replace": {str(Path.cwd()/"internal/artifactfs/benchmark_test.go"): fixture}}, fd)\n' +
                       'os.execv(real, [real, "test", "-overlay=" + fd.name, *sys.argv[2:]])\n')
    wrapper.chmod(0o755)
    chain = [GOOD, *REPLAY.git(ROOT, 'rev-list', '--first-parent', '--reverse', GOOD + '..' + BAD).splitlines()]
    require(chain[-2:] == [EXPECTED_PARENT, BAD], 'unexpected master history')
    os.environ['PATH'] = str(wrapper.parent) + os.pathsep + os.environ['PATH']
    candidate = {'package': PACKAGE, 'test': TEST, 'race': False, 'signature': {'tests': [TEST], 'messages':[SIGNATURE]}}
    raw = args.output/'probes'
    runner = REPLAY.Probe(ROOT, raw, timeout=150, budget=1200)
    try:
        result = REPLAY.localize(chain, candidate, runner)
    finally:
        runner.close()
    write(args.output/'localization.json', result)
    require(result.get('localization')=='reproduced' and result.get('commit')==BAD, 'did not reproduce unique transition')
    require(result.get('good_sha')==EXPECTED_PARENT and result.get('bad_sha')==BAD, 'localized pair differs')
    write(args.output/'control.json', {'experiment':'test-overlay', 'source':'controlled assertion on actual master history', 'fault_sha256':hashlib.sha256(source.read_bytes()).hexdigest(), 'wrapper_sha256':hashlib.sha256(wrapper.read_bytes()).hexdigest(), 'target':BAD, 'good':GOOD, **controls()})


def associate(args):
    api = POLICY.API(args.output/'api-pr')
    result = json.loads((args.output/'localization.json').read_text()); proof = json.loads((args.output/'baseline.json').read_text())
    pulls=api.get(f'repos/{REPO}/commits/{result["commit"]}/pulls?per_page=100&page=1')
    require(len(pulls)<100, 'incomplete PR pagination')
    result = POLICY.attribute(result, pulls, REPO)
    require(result.get('status')=='confirmed' and result.get('pr')==265, 'exact master PR not authenticated')
    output={**json.loads((args.output/'control.json').read_text()), 'type':'controlled-fault-injection', 'run_id':int(os.environ['GITHUB_RUN_ID']), 'run_attempt':int(os.environ['GITHUB_RUN_ATTEMPT']), 'control_sha':os.environ['GITHUB_SHA'], 'proven_baseline':proof, 'attribution':result}
    write(args.output/'experiment.json', output)
    print(json.dumps({k:result[k] for k in ('pr','commit','good_sha','bad_sha','status')},sort_keys=True)); print('Verified real GitHub Full baseline',proof['url'],'and',len(result['chain']),'first-parent commits under the published test overlay.')


parser = argparse.ArgumentParser()
parser.add_argument('mode', choices=['prove','replay','associate'])
parser.add_argument('output', type=Path)
args=parser.parse_args(); args.output=args.output.resolve(); args.output.mkdir(parents=True,exist_ok=True)
require(os.environ.get('FIXED_SHA')==BAD, 'fixed SHA mismatch')
{'prove':prove,'replay':replay,'associate':associate}[args.mode](args)
