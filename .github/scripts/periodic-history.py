#!/usr/bin/env python3
"""Pure sparse localization of the latest continuous failing first-parent episode.

locate(chain, candidate, probe, *, remaining=None, prior=None) takes a caller-verified
[original_good, ..., original_bad] lineage. probe(sha, candidate) supplies genuine
engine evidence; remaining() returns real seconds left. The engine must also bound
individual executions. There are no history-length or probe-count budget limits.

Only a fully screened failing suffix plus three observations of its adjacent
boundary and tip can yield localization='reproduced'. Earlier unsampled history is
NOT classified. New-test additionally requires source absence/presence at EVERY
commit, not just compiled inventory absence. No PR attribution is performed here.

continuation.intervals are inclusive zero-based UNSAMPLED chain intervals; next is
the blocked query (one-based observation), not permission to skip other queries.
Resume by passing this result as prior. Before doing so the caller MUST validate
original SHAs, first-parent lineage, candidate, trees, source definitions and raw
execution evidence externally. Only completed probes are reused, in deterministic
query order; interruptions are retained, never counted as successful observations.
Conflicting completed observations cannot be repaired by appending more samples.

For finalization, verify raw logs with the existing Go/client/diagnostic checker,
including globally unique execution/log nonces. Rerun locate without prior using a
replay callback returning each verified observation once in per-SHA order. Require
reproduced, identical chain/candidate/original endpoints/commit/good_sha/bad_sha/
transition/scope, and consumption of ALL recorded completed observations. Reject
missing, forged, reused or extra evidence; never trust supplied state summaries or
continuation fields. An interrupted result stays unknown until new real evidence.
"""
import subprocess


class _Incomplete(Exception):
    pass


def _check(condition, reason):
    if not condition:
        raise _Incomplete(reason)


def _identities(value):
    if isinstance(value, dict):
        for key, item in value.items():
            if key in ('probe_id', 'log') and isinstance(item, str):
                yield key, item
            else:
                yield from _identities(item)
    elif isinstance(value, list):
        for item in value:
            yield from _identities(item)


def locate(chain, candidate, probe, *, remaining=None, prior=None):
    chain = list(chain)
    evidence = {'status': 'unknown', 'chain': chain, 'candidate': candidate, 'probes': [],
                'original_good_sha': chain[0] if chain else None,
                'original_bad_sha': chain[-1] if chain else None, 'interruptions': []}
    cells, saved, seen, pending = {}, {}, set(), None

    def observe(index, count=1):
        nonlocal pending
        results = cells.setdefault(index, [])
        while len(results) < count:
            pending = {'index': index, 'sha': chain[index], 'observation': len(results) + 1}
            if saved.get(index):
                result = saved[index].pop(0)
            else:
                _check(not any(saved.values()), '续查证据不是确定性查询前缀，或包含额外观察')
                _check(remaining is None or remaining() > 0, '回放真实时间预算已耗尽')
                try:
                    result = probe(chain[index], candidate)
                except (ValueError, KeyError, TypeError, OSError, StopIteration, subprocess.SubprocessError) as error:
                    result = {'state': 'unknown', 'sha': chain[index], 'reason': str(error)}
            if not isinstance(result, dict):
                result = {'state': 'unknown', 'reason': 'probe 结果不是对象'}
            if result.get('state') not in ('pass', 'fail', 'absent'):
                evidence['interruptions'].append({'sha': chain[index], 'result': result})
                raise _Incomplete('缺少执行证明、预算耗尽或基础设施失败：' + str(result.get('reason', '未知')))
            results.append(result)
            pending = None
            _check(result.get('sha', chain[index]) == chain[index], 'probe 提交身份不符')
            _check(result.get('tree') == results[0].get('tree'), '同一提交的 tree 不一致')
            identities = set(_identities(result))
            _check(not identities.intersection(seen), '重复使用了执行或日志身份')
            seen.update(identities)
            state = results[0]['state']
            _check(result['state'] == state and (state != 'fail' or result.get('signature') == candidate['signature']),
                   '观察不稳定或失败签名不一致')
        return results[0]['state']

    try:
        _check(len(chain) >= 2 and all(isinstance(sha, str) and sha for sha in chain) and
               len(set(chain)) == len(chain), '缺少唯一的可信首亲绿点到失败点范围')
        _check(isinstance(candidate, dict) and isinstance(candidate.get('signature'), dict), '缺少候选失败签名')
        if prior is not None:
            _check(isinstance(prior, dict) and all(prior.get(key) == evidence[key] for key in
                   ('chain', 'candidate', 'original_good_sha', 'original_bad_sha')), '续查原始范围或候选不符')
            positions = {sha: index for index, sha in enumerate(chain)}
            _check(isinstance(prior.get('probes'), list) and isinstance(prior.get('interruptions', []), list), '续查证据格式非法')
            for group in prior['probes']:
                _check(isinstance(group, dict) and isinstance(group.get('sha'), str) and group['sha'] in positions,
                       '续查包含范围外的 probe')
                index, results = positions[group['sha']], group.get('results')
                _check(index not in saved and isinstance(results, list) and 1 <= len(results) <= 3 and
                       all(isinstance(row, dict) and row.get('state') in ('pass', 'fail', 'absent') for row in results),
                       '续查存在重复、未完成或额外的观察')
                saved[index] = list(results)
            evidence['interruptions'] = list(prior.get('interruptions', []))
            seen.update(identity for item in evidence['interruptions'] for identity in _identities(item))
        tip = len(chain) - 1
        original = observe(0)
        _check(original in ('pass', 'absent') and observe(tip) == 'fail', '原始绿点或末端失败未复现')
        low, high, distance = 0, tip, 1
        while True:
            index = max(0, tip - distance)
            if observe(index) != 'fail':
                low = index
                break
            high, distance = index, distance * 2
        while high - low > 1:
            middle = (low + high) // 2
            if observe(middle) == 'fail':
                high = middle
            else:
                low = middle
        # Scouting is not proof of monotonicity: establish the latest episode backwards.
        for low in range(tip - 1, low - 1, -1):
            if observe(low) != 'fail':
                high = low + 1
                break
        previous = observe(low)
        _check(previous == original, '原始绿点与相邻边界的测试存在性不一致')
        for index in sorted({low, high, tip}):
            observe(index, 3)
        transition = 'regression'
        if previous == 'absent':
            for index in range(len(chain)):
                expected = 'absent' if index < high else 'fail'
                source = 'absent' if index < high else 'present'
                _check(observe(index) == expected and all(row.get('definition', {}).get('state') == source
                       for row in cells[index]), '缺少逐提交首次引入源码证明，或测试曾经存在')
            transition = 'new-test'
        _check(not any(saved.values()), '存在未消费的额外观察')
        evidence.update(localization='reproduced', scope='latest-continuous-failing-episode', transition=transition,
                        good_sha=chain[low], bad_sha=chain[-1], commit=chain[high])
        evidence['reason'] = '当前连续失败后缀及相邻边界已重复复现，等待合并 PR 核验'
    except _Incomplete as error:
        evidence['reason'] = str(error)
    evidence['probes'] = [{'sha': chain[index], 'results': results} for index, results in cells.items() if results]
    intervals, start = [], None
    for index in range(len(chain) + 1):
        if index < len(chain) and not cells.get(index):
            if start is None:
                start = index
        elif start is not None:
            intervals.append([start, index - 1])
            start = None
    evidence['continuation'] = {'intervals': intervals, 'next': pending}
    return evidence
