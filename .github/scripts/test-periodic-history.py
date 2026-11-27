#!/usr/bin/env python3
"""Sparse search and raw-log replay fixtures; no Git, network or subprocess calls."""
import copy
import hashlib
import importlib.util
import itertools
import json
from pathlib import Path
import sys
import time
import unittest

sys.dont_write_bytecode = True


def load(name):
    spec = importlib.util.spec_from_file_location(name.replace('-', '_'), Path(__file__).with_name(name + '.py'))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


HISTORY, GO = load('periodic-history'), load('periodic-replay')
CANDIDATE = {'package': 'agentworkspace/internal/store', 'test': 'TestHistory', 'race': False,
             'signature': {'tests': ['TestHistory'], 'messages': ['history regression']},
             'execution': {'mode': 'ordinary', 'timeout_arg': '10m', 'timeout_seconds': 600}}


def chain(size):
    return [f'{index + 1:040x}' for index in range(size)]


def digest(data):
    return hashlib.sha256(data).hexdigest()


class Engine:
    """Independent raw Go lifecycle bytes and checkout/source identity bindings."""
    def __init__(self, states, *, state_at=None, hook=None, candidate=None):
        self.states, self.chain = states, chain(len(states))
        self.candidate = copy.deepcopy(candidate or CANDIDATE)
        self.state_at, self.hook = state_at, hook
        self.calls, self.logs, self.bindings, self.visits = [], {}, {}, {}
        self.positions = {sha: index for index, sha in enumerate(self.chain)}

    def __call__(self, sha, candidate):
        if candidate != self.candidate:
            raise ValueError('candidate mismatch')
        index = self.positions[sha]
        visit = self.visits[index] = self.visits.get(index, 0) + 1
        state = self.state_at(index, visit) if self.state_at else self.states[index]
        nonce = f'probe-{len(self.calls) + 1:05d}'
        self.calls.append(index)
        source = {'state': 'absent' if state == 'absent' else 'present', 'files': {},
                  'declarations': [] if state == 'absent' else ['internal/store/history_test.go']}
        result = {'sha': sha, 'tree': f'{index + 1000000:040x}', 'probe_id': nonce,
                  'execution': candidate['execution'], 'definition': source, 'state': state}
        package, test = candidate['package'], candidate['test']
        def event(action, name=None, **fields):
            return {'Action': action, 'Package': package, **({'Test': name} if name else {}), **fields}
        def artifact(label, events, code):
            data = (''.join(json.dumps(row) + '\n' for row in events)).encode()
            name = nonce + '-' + label + '.jsonl'
            self.logs[name] = data
            return {'log': name, 'sha256': digest(data), 'returncode': code}
        if state == 'unknown':
            result['list'] = artifact('list', [], 1)
            result['reason'] = 'toolchain unavailable or real execution budget exhausted'
        else:
            listing = [event('start'), event('output', Output=test + '\n')] if state != 'absent' else [event('start')]
            result['list'] = artifact('list', listing + [event('pass')], 0)
            if state != 'absent':
                events = [event('start'), event('run', test)]
                if state == 'fail':
                    events.append(event('output', test, Output='    history_test.go:8: history regression\n'))
                    result['signature'] = copy.deepcopy(candidate['signature'])
                events.extend([event(state, test), event(state)])
                result['run'] = artifact('run', events, int(state == 'fail'))
        self.bindings[nonce] = copy.deepcopy((sha, result['tree'], source))
        if self.hook:
            self.hook(index, visit, result)
        return copy.deepcopy(result)


def validate_recording(report, engine):
    """Example coordinator: reconstruct raw states, replay once, reject leftovers."""
    queues, seen = {}, set()
    for group in report['probes']:
        if group['sha'] in queues:
            raise ValueError('duplicate group')
        queues[group['sha']] = list(group['results'])

    def verified(result):
        nonce = result.get('probe_id')
        if nonce in seen or nonce not in engine.bindings:
            raise ValueError('forged or reused execution nonce')
        seen.add(nonce)
        if (result.get('sha'), result.get('tree'), result.get('definition')) != engine.bindings[nonce]:
            raise ValueError('checkout/source identity mismatch')
        text, codes = {'run': ''}, {'run': None}
        for key in ('list', 'run'):
            if key not in result:
                continue
            item = result[key]
            name = item['log']
            if name in seen or name not in engine.logs:
                raise ValueError('reused or forged raw log')
            seen.add(name)
            data = engine.logs[name]
            if item['sha256'] != digest(data):
                raise ValueError('raw digest mismatch')
            text[key], codes[key] = data.decode(), item['returncode']
        assessed = GO.assess(text['list'], codes['list'], text['run'], codes['run'], report['candidate'])
        if any(assessed.get(key) != result.get(key) for key in ('state', 'signature')):
            raise ValueError('claimed state/signature not reconstructed')
        return {**result, **assessed}

    for interrupted in report.get('interruptions', []):
        if verified(interrupted['result'])['state'] != 'unknown':
            raise ValueError('interruption was a completed observation')

    def recorded(sha, candidate):
        if candidate != engine.candidate or not queues.get(sha):
            raise ValueError('missing admissible query')
        return verified(queues[sha].pop(0))

    recomputed = HISTORY.locate(engine.chain, engine.candidate, recorded)
    fields = ('chain', 'candidate', 'original_good_sha', 'original_bad_sha', 'commit',
              'good_sha', 'bad_sha', 'transition', 'scope')
    if (recomputed.get('localization') != 'reproduced' or any(queues.values()) or
            any(recomputed.get(key) != report.get(key) for key in fields)):
        raise ValueError('sparse result cannot be reconstructed or contains extra observations')
    return recomputed


class HistoryTests(unittest.TestCase):
    def assert_episode(self, report, engine, first, transition='regression'):
        self.assertEqual(report.get('localization'), 'reproduced', report.get('reason'))
        self.assertEqual((report['good_sha'], report['commit'], report['bad_sha']),
                         (engine.chain[first - 1], engine.chain[first], engine.chain[-1]))
        self.assertEqual((report['original_good_sha'], report['original_bad_sha']),
                         (engine.chain[0], engine.chain[-1]))
        self.assertEqual(report['transition'], transition)
        self.assertEqual(report['scope'], 'latest-continuous-failing-episode')
        self.assertNotIn('pr', report)
        counts = {group['sha']: len(group['results']) for group in report['probes']}
        for index in {first - 1, first, len(engine.chain) - 1}:
            self.assertEqual(counts[engine.chain[index]], 3)
        self.assertTrue(set(range(first, len(engine.chain))).issubset(engine.calls))
        self.assertIsNone(report['continuation']['next'])

    def test_default_api_and_raw_go_replay(self):
        engine = Engine(['pass', 'fail'])
        report = HISTORY.locate(engine.chain, engine.candidate, engine)
        self.assert_episode(report, engine, 1)
        self.assertEqual(report['status'], 'unknown')
        self.assertEqual(validate_recording(report, engine)['commit'], engine.chain[1])

    def test_33_plus_and_thousands_late_stable_regressions_stay_sparse(self):
        for size, tail in ((33, 3), (65, 7), (1001, 11), (1024, 19), (4097, 7)):
            with self.subTest(size=size, tail=tail):
                first = size - tail
                engine = Engine(['pass'] * first + ['fail'] * tail)
                report = HISTORY.locate(engine.chain, engine.candidate, engine)
                self.assert_episode(report, engine, first)
                self.assertLess(len(engine.calls), tail + 25)
                self.assertTrue(report['continuation']['intervals'])
                validate_recording(report, engine)

    def test_suffix_screen_finds_repair_and_reintroduction_hidden_by_binary_samples(self):
        states = ['pass'] * 33 + ['fail'] * 96
        states[115] = 'pass'
        engine = Engine(states)
        report = HISTORY.locate(engine.chain, engine.candidate, engine)
        self.assert_episode(report, engine, 116)
        self.assertIn(33, engine.calls)
        self.assertIn(112, engine.calls)
        self.assertIn(115, engine.calls)
        self.assertNotEqual(report['commit'], engine.chain[33])
        validate_recording(report, engine)

    def test_all_small_nonmonotonic_histories_find_only_latest_continuous_episode(self):
        for size in range(2, 9):
            for middle in itertools.product(('pass', 'fail'), repeat=size - 2):
                states = ['pass', *middle, 'fail']
                first = max(index for index, state in enumerate(states) if state == 'pass') + 1
                engine = Engine(states)
                with self.subTest(states=states):
                    self.assert_episode(HISTORY.locate(engine.chain, engine.candidate, engine), engine, first)

    def test_missing_original_green_bad_terminal_and_infrastructure_are_unknown(self):
        for values in ([], ['pass'], ['fail', 'fail'], ['unknown', 'fail'],
                       ['pass', 'pass'], ['pass', 'absent'], ['pass', 'unknown']):
            with self.subTest(states=values):
                engine = Engine(values)
                report = HISTORY.locate(engine.chain, engine.candidate, engine)
                self.assertNotEqual(report.get('localization'), 'reproduced')
        engine = Engine(['pass', 'fail'])
        self.assertNotEqual(HISTORY.locate([engine.chain[0]] * 2, engine.candidate, engine).get('localization'), 'reproduced')
        self.assertFalse(engine.calls)

    def test_flaky_repeats_and_changed_signatures_cannot_be_resumed_away(self):
        engine = Engine(['pass'] * 60 + ['fail'] * 5,
                        state_at=lambda index, visit: 'pass' if index < 60 or index == 60 and visit == 2 else 'fail')
        report = HISTORY.locate(engine.chain, engine.candidate, engine)
        self.assertNotEqual(report.get('localization'), 'reproduced')
        calls = len(engine.calls)
        resumed = HISTORY.locate(engine.chain, engine.candidate, engine, prior=report)
        self.assertNotEqual(resumed.get('localization'), 'reproduced')
        self.assertEqual(len(engine.calls), calls)
        bad_signature = Engine(['pass', 'fail'], hook=lambda index, visit, row:
                               row.update(signature={'messages': ['another failure']}) if index else None)
        self.assertNotEqual(HISTORY.locate(bad_signature.chain, bad_signature.candidate, bad_signature).get('localization'), 'reproduced')

    def test_new_test_requires_source_evidence_at_every_commit(self):
        engine = Engine(['absent'] * 60 + ['fail'] * 5)
        report = HISTORY.locate(engine.chain, engine.candidate, engine)
        self.assert_episode(report, engine, 60, 'new-test')
        self.assertEqual(len(report['probes']), len(engine.chain))
        validate_recording(report, engine)
        for definition in ({}, {'state': 'present'}, {'state': 'unknown'}):
            with self.subTest(definition=definition):
                engine = Engine(['absent'] * 60 + ['fail'] * 5,
                                hook=lambda index, visit, row: row.update(definition=definition) if index == 20 else None)
                self.assertNotEqual(HISTORY.locate(engine.chain, engine.candidate, engine).get('localization'), 'reproduced')
        states = ['absent'] * 60 + ['fail'] * 5
        states[20] = 'pass'
        engine = Engine(states)
        self.assertNotEqual(HISTORY.locate(engine.chain, engine.candidate, engine).get('localization'), 'reproduced')
        for states in (['pass', 'absent', 'fail'], ['absent', 'pass', 'fail']):
            engine = Engine(states)
            self.assertNotEqual(HISTORY.locate(engine.chain, engine.candidate, engine).get('localization'), 'reproduced')

    def test_real_time_exhaustion_retains_exact_prefix_and_resumes(self):
        deadline = None
        def remaining():
            nonlocal deadline
            if deadline is None:
                deadline = time.monotonic() + 0.02
            return deadline - time.monotonic()
        def pause(index, visit, result):
            if index == 1003 and visit == 2:
                time.sleep(max(0, deadline - time.monotonic()) + 0.002)
        engine = Engine(['pass'] * 1004 + ['fail'] * 20, hook=pause)
        partial = HISTORY.locate(engine.chain, engine.candidate, engine, remaining=remaining)
        self.assertNotEqual(partial.get('localization'), 'reproduced')
        self.assertLessEqual(remaining(), 0)
        self.assertTrue(partial['probes'])
        self.assertTrue(partial['continuation']['intervals'])
        self.assertIsNotNone(partial['continuation']['next'])
        self.assertFalse(partial['interruptions'])
        prefix = copy.deepcopy(partial)
        completed = {row['probe_id'] for group in partial['probes'] for row in group['results']}
        resumed = HISTORY.locate(engine.chain, engine.candidate, engine, prior=partial)
        self.assertEqual(partial, prefix)
        self.assert_episode(resumed, engine, 1004)
        self.assertTrue(completed.issubset(row['probe_id'] for group in resumed['probes'] for row in group['results']))
        self.assertEqual(engine.visits[0], 1)
        validate_recording(resumed, engine)

    def test_observed_probe_exhaustion_stops_immediately_and_retains_raw_attempt(self):
        engine = Engine(['pass'] * 1000 + ['fail'] * 24,
                        state_at=lambda index, visit: 'unknown' if index == 1019 and visit == 1 else
                        'pass' if index < 1000 else 'fail')
        partial = HISTORY.locate(engine.chain, engine.candidate, engine)
        self.assertNotEqual(partial.get('localization'), 'reproduced')
        self.assertEqual(engine.calls[-1], 1019)
        self.assertEqual(partial['continuation']['next']['index'], 1019)
        self.assertEqual(len(partial['interruptions']), 1)
        resumed = HISTORY.locate(engine.chain, engine.candidate, engine, prior=partial)
        self.assert_episode(resumed, engine, 1000)
        self.assertEqual(len(resumed['interruptions']), 1)
        self.assertEqual(engine.visits[0], 1)
        validate_recording(resumed, engine)

    def test_zero_budget_has_no_execution_and_original_endpoints_are_durable(self):
        engine = Engine(['pass'] * 40 + ['fail'])
        report = HISTORY.locate(engine.chain, engine.candidate, engine, remaining=lambda: 0)
        self.assertFalse(engine.calls)
        self.assertEqual(report['continuation'], {'intervals': [[0, 40]],
                         'next': {'index': 0, 'sha': engine.chain[0], 'observation': 1}})
        self.assert_episode(HISTORY.locate(engine.chain, engine.candidate, engine, prior=report), engine, 40)

    def test_prior_summaries_never_choose_queries_and_extra_prefix_is_rejected(self):
        engine = Engine(['pass'] * 60 + ['fail'] * 5)
        report = HISTORY.locate(engine.chain, engine.candidate, engine)
        prior = copy.deepcopy(report)
        prior.update(commit=engine.chain[1], good_sha=engine.chain[0], scope='invented', localization='invented')
        prior['continuation'] = {'intervals': [], 'next': {'sha': engine.chain[1]}}
        calls = len(engine.calls)
        self.assert_episode(HISTORY.locate(engine.chain, engine.candidate, engine, prior=prior), engine, 60)
        self.assertEqual(len(engine.calls), calls)
        for key in ('chain', 'candidate', 'original_good_sha', 'original_bad_sha'):
            invalid = copy.deepcopy(report)
            invalid[key] = None
            self.assertNotEqual(HISTORY.locate(engine.chain, engine.candidate, engine, prior=invalid).get('localization'), 'reproduced')
        extra = copy.deepcopy(report)
        extra['probes'][0]['results'].append(engine(engine.chain[0], engine.candidate))
        calls = len(engine.calls)
        self.assertNotEqual(HISTORY.locate(engine.chain, engine.candidate, engine, prior=extra).get('localization'), 'reproduced')
        self.assertEqual(len(engine.calls), calls)
        with self.assertRaises(ValueError):
            validate_recording(extra, engine)

    def test_forged_reused_or_extra_evidence_and_claims_fail_raw_reconstruction(self):
        engine = Engine(['pass'] * 60 + ['fail'] * 5)
        report = HISTORY.locate(engine.chain, engine.candidate, engine)
        for field, value in (('state', 'fail'), ('probe_id', 'invented'), ('sha', engine.chain[1]),
                             ('tree', 'f' * 40), ('signature', {'messages': ['forged']})):
            tampered = copy.deepcopy(report)
            tampered['probes'][0]['results'][0][field] = value
            with self.subTest(field=field), self.assertRaises(ValueError):
                validate_recording(tampered, engine)
        for key, value in (('commit', engine.chain[1]), ('good_sha', engine.chain[0]), ('scope', 'first-ever-failure')):
            tampered = copy.deepcopy(report)
            tampered[key] = value
            with self.subTest(summary=key), self.assertRaises(ValueError):
                validate_recording(tampered, engine)
        tampered = copy.deepcopy(report)
        boundary = next(group for group in tampered['probes'] if group['sha'] == engine.chain[60])
        boundary['results'][1] = copy.deepcopy(boundary['results'][0])
        with self.assertRaises(ValueError):
            validate_recording(tampered, engine)
        extra = copy.deepcopy(report)
        extra['probes'].append({'sha': engine.chain[10], 'results': [engine(engine.chain[10], engine.candidate)]})
        with self.assertRaises(ValueError):
            validate_recording(extra, engine)
        name = report['probes'][0]['results'][0]['list']['log']
        engine.logs[name] += b'forged output\n'
        with self.assertRaises(ValueError):
            validate_recording(report, engine)

    def test_reused_log_nonce_and_inconsistent_checkout_never_confirm(self):
        def reused(index, visit, row):
            row['list']['log'] = 'same-log.jsonl'
        for hook in (reused, lambda index, visit, row: row.update(sha='f' * 40),
                     lambda index, visit, row: row.update(tree='f' * 40) if visit == 2 else None):
            engine = Engine(['pass', 'fail'], hook=hook)
            self.assertNotEqual(HISTORY.locate(engine.chain, engine.candidate, engine).get('localization'), 'reproduced')


if __name__ == '__main__':
    unittest.main()
