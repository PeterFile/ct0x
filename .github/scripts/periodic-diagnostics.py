#!/usr/bin/env python3
"""Conservative compile/vet/unittest diagnostics, using periodic-replay primitives.

failure_records(phase, log, context) returns (candidates, unresolved); failures
returns only candidates. Phases: compile, vet, unittest. The coordinator MUST
bind log to the native failed job/step and authenticate context from immutable
workflow/delegated source, not from commands or step names printed by a test.

Required context: argv (reviewed effective original argv, including GOFLAGS),
job, step, source_root (original absolute working directory), timeout_seconds
(original process/Go ceiling), job_timeout_seconds, proof ([{sha,path,blob}]).
Optional requires_full=True retains the full original unittest selection.
Otherwise unittest selects Class.method; Go always retains original packages.
Unknown flags, environment-dependent commands and unsupported outputs stay unknown.

DiagnosticProbe(root, output, replay=None, timeout=None, budget=12600) composes
replay.Probe, git and child_env. verify_probe(result, candidate, directory,
replay=None, root=..., seen=None) rechecks source and reconstructs state from
bounded raw artifacts, never trusting claimed state/signature. The coordinator
must also compare the candidate to its authenticated initial candidate list.
"""
import ast
from collections import Counter
import hashlib
import importlib.util
import json
import math
from pathlib import Path
import re
import subprocess
import tempfile
import time


_REPLAY = None
SCRIPTS = ('.github/scripts/test-ci-scheduling.py', '.github/scripts/test-store-shard.py')
KINDS = {'compile': 'go-compile', 'vet': 'go-vet', 'unittest': 'python-unittest'}
IDENT = r'[A-Za-z_][A-Za-z0-9_]*'
METHOD = r'test_[A-Za-z0-9_]+'
INFRA = re.compile(r'timed? out|timeout|network|connection (?:refused|reset)|no such host|temporary failure|TLS handshake|signal: killed|fatal error:|panic:', re.I)
COMPILE = re.compile(r'(?:undefined: [A-Za-z_][A-Za-z0-9_.]*|cannot use .+|.+ has no field or method .+|syntax error: .+|cannot refer to unexported name .+)\Z', re.S)
COPYLOCKS = re.compile(r'\b(?:copies lock value|passes lock by value|copies lock)\b')
HOOK_PREFIX = 'AW_DIAGNOSTIC_EVENT='
# Native unittest does not report successful subtests. Keep the approved native
# argv and report actual lifecycle callbacks at exit, not test-printed statuses.
_HOOK_BODY = '''import atexit
import json
import unittest
_events = []
def _wrap(name):
    original = getattr(unittest.TextTestResult, name)
    def record(self, test, *args):
        event = {"test": test.id(), "event": name}
        if name == "addSubTest":
            event.update(description=str(args[0]), state="pass" if args[1] is None else "fail")
        if len(_events) < 4096 and len(json.dumps(event)) <= 4096:
            _events.append(event)
        elif not _events or _events[-1] != {"event": "overflow"}:
            _events.append({"event": "overflow"})
        return original(self, test, *args)
    setattr(unittest.TextTestResult, name, record)
for _name in ("startTest", "stopTest", "addSuccess", "addFailure", "addError", "addSkip", "addExpectedFailure", "addUnexpectedSuccess", "addSubTest"):
    _wrap(_name)
def _flush():
    for event in _events:
        print("AW_DIAGNOSTIC_EVENT=" + json.dumps(event, sort_keys=True), flush=True)
atexit.register(_flush)
'''
# Consume the enable flag so Python subprocesses retain original unittest behavior.
SUBTEST_HOOK = 'import os\nif os.environ.pop("PERIODIC_DIAGNOSTIC_HOOK", None) == "1":\n' + '\n'.join('    ' + line for line in _HOOK_BODY.splitlines()) + '\n'


def normal_replay(replay=None):
    global _REPLAY
    if replay is not None:
        return replay
    if _REPLAY is None:
        spec = importlib.util.spec_from_file_location('periodic_replay_diagnostics', Path(__file__).with_name('periodic-replay.py'))
        _REPLAY = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(_REPLAY)
    return _REPLAY


def check(condition, message):
    if not condition:
        raise ValueError(message)


def unknown(reason):
    return {'state': 'unknown', 'reason': reason}


def package(value):
    if not isinstance(value, str):
        return None
    pattern = normal_replay().PACKAGE_RE
    variant = re.fullmatch(r'(' + pattern + r')(?: \[(' + pattern + r')\.test\])?', value)
    if variant and (not variant[2] or variant[1] in (variant[2], variant[2] + '_test')):
        return variant[2] or variant[1]
    return None


def go_args(argv):
    """Small reviewed flag vocabulary; never forward exec, overlays or tool paths."""
    check(argv[:2] in (['go', 'test'], ['go', 'build'], ['go', 'vet']), '不是支持的原始 Go 命令')
    flags, build, targets, index = [], [], [], 2
    while index < len(argv):
        value = argv[index]
        if value in ('-list', '-run') and argv[1] == 'test':
            check(index + 1 < len(argv) and len(argv[index + 1]) <= 512, '原始测试选择器缺失')
            index += 2
            continue
        if value in ('-race', '-trimpath', '-mod=readonly', '-buildvcs=false') or re.fullmatch(r'-tags=[A-Za-z0-9_,.]+', value):
            flags.append(value)
            build.append(value)
        elif argv[1] == 'test' and (value in ('-count=1', '-json') or re.fullmatch(r'-timeout=[1-9][0-9]*(?:s|m|h)', value)):
            flags.append(value)
        elif argv[1] == 'vet' and value in ('-copylocks', '-copylocks=true'):
            flags.append(value)
        else:
            target = 'agentworkspace/' + value[2:] if value.startswith('./') else value
            check(value == './...' or package(target) == target, '原始 Go 参数或包不受支持')
            targets.append(value)
        index += 1
    check(bool(targets), '缺少原始 Go 包选择')
    return flags, build, targets


def execution(phase, context):
    check(phase in KINDS and isinstance(context, dict), '缺少可信原始命令上下文')
    value = {key: context[key] for key in ('argv', 'job', 'step', 'source_root', 'timeout_seconds', 'job_timeout_seconds', 'proof')}
    for key in ('job', 'step', 'source_root'):
        check(isinstance(value[key], str) and 0 < len(value[key]) <= 4096 and not re.search(r'[\x00-\x1f]', value[key]), '原生步骤或工作目录非法')
    root = Path(value['source_root'])
    check(root.is_absolute() and '..' not in root.parts, '原始工作目录必须为绝对路径')
    check(type(value['timeout_seconds']) is int and type(value['job_timeout_seconds']) is int and
          0 < value['timeout_seconds'] <= value['job_timeout_seconds'], '原始命令预算未证明')
    argv = value['argv']
    check(isinstance(argv, list) and 2 <= len(argv) <= 64 and all(isinstance(arg, str) and 0 < len(arg) <= 2048 and not re.search(r'[\x00-\x1f]', arg) for arg in argv), '原始 argv 非法')
    proof = value['proof']
    check(isinstance(proof, list) and 1 <= len(proof) <= 8, '原始源码证明缺失')
    for item in proof:
        check(isinstance(item, dict) and all(isinstance(item.get(key), str) and re.fullmatch(normal_replay().SHA_RE, item[key]) for key in ('sha', 'blob')) and
              isinstance(item.get('path'), str) and re.fullmatch(r'\.github/(?:workflows/[A-Za-z0-9_-]+\.yml|scripts/[A-Za-z0-9_-]+\.py)', item['path']), '原始源码 blob 证明非法')
    check(any(item['path'].startswith('.github/workflows/') for item in proof), '缺少原始工作流证明')
    if 'requires_full' in context:
        check(type(context['requires_full']) is bool, '完整命令要求类型非法')
        value['requires_full'] = context['requires_full']
    if phase == 'unittest':
        args = argv[1:]
        if args[0] == '-B':
            args = args[1:]
        check(args and argv[0] == 'python3' and args[0] in SCRIPTS, '不是允许的原始 unittest 脚本')
        check(args[1:] == ['-v'] or len(args) == 3 and re.fullmatch(IDENT + r'\.' + METHOD, args[1]) and args[2] == '-v', '原始 unittest 选择器不受支持')
    else:
        check(argv[0] == 'go' and argv[1] in (('test', 'build') if phase == 'compile' else ('vet',)), '原始命令与诊断类型不符')
        go_args(argv)
    value['proof'] = [{key: item[key] for key in ('sha', 'path', 'blob')} for item in proof]
    return json.loads(json.dumps(value))


def cleaned(text):
    return '\n'.join(re.sub(r'^##\[error\]', '', re.sub(r'\x1b\[[0-9;]*m', '', normal_replay().clean_line(line))) for line in text.splitlines())


def owned_file(value, owner, mode):
    if value.startswith(mode['source_root'].rstrip('/') + '/'):
        value = value[len(mode['source_root'].rstrip('/')) + 1:]
    value = value.removeprefix('./')
    if re.fullmatch(r'[A-Za-z][A-Za-z0-9_]*\.go', value) and owner:
        value = owner.removeprefix('agentworkspace/') + '/' + value
    match = re.fullmatch(r'(cmd/workspace|internal/[A-Za-z][A-Za-z0-9_]*)/([A-Za-z][A-Za-z0-9_]*\.go)', value)
    if not match:
        return None
    actual = 'agentworkspace/' + match[1]
    if owner and actual != owner:
        return None
    return actual, value


def named(phase, mode, owner, file, message, test=None, subtest=''):
    signature = {'file': file, 'message': message}
    row = {'kind': 'diagnostic', 'replay_kind': KINDS[phase], 'file': file, 'execution': mode, 'signature': signature}
    if test:
        row['test'] = test
        signature.update(test=test, subtest=subtest)
    else:
        row['package'] = owner
        signature['package'] = owner
    return row


def go_records(phase, text, mode):
    groups, failed, terminals, raw, issues = {}, set(), [], [], []
    for line in text.splitlines():
        if line.startswith('{'):
            try:
                row = json.loads(line, object_pairs_hook=normal_replay().unique_pairs)
                check(isinstance(row, dict), 'Go JSON 不是对象')
                action = row.get('Action')
                if action == 'build-output':
                    check(package(row.get('ImportPath')) and isinstance(row.get('Output'), str), 'Go build-output 包身份非法')
                    groups.setdefault(row['ImportPath'], []).append(row['Output'])
                elif action == 'build-fail':
                    check(package(row.get('ImportPath')), 'Go build-fail 包身份非法')
                    failed.add(row['ImportPath'])
                elif action == 'skip':
                    issues.append({'reason': 'Go 报告包含跳过，不能证明诊断'})
                elif action == 'fail':
                    if row.get('Test'):
                        issues.append({'reason': '命名测试失败不是编译诊断'})
                    else:
                        terminals.append(row)
            except (ValueError, TypeError) as error:
                issues.append({'reason': str(error)})
        else:
            raw.append(line)
    found = []
    streams = [(None, '\n'.join(raw))] + [(package(key), ''.join(values)) for key, values in groups.items() if key in failed]
    if set(groups) != failed:
        issues.append({'reason': 'Go 编译输出与 build-fail 终止事件不完整'})
    for bound, stream in streams:
        owner, local, conflicted, blocked, consumed = bound, [], False, False, 0
        lines = stream.splitlines()
        for index, line in enumerate(lines):
            if index < consumed:
                continue
            if line.startswith('# '):
                header = line[2:]
                if header.startswith('[') and header.endswith(']'):
                    header = header[1:-1]
                actual = package(header)
                blocked = actual is None
                if blocked or bound and actual != bound:
                    conflicted = bound is not None
                    issues.append({'reason': 'Go 包头与编译归属冲突或不受支持'})
                owner = actual
                continue
            match = re.fullmatch(r'(?:vet: )?([^\s:]+\.go):[0-9]+(?::[0-9]+)?:\s*(.+)', line)
            if match:
                path, message = owned_file(match[1], owner, mode), match[2].strip()
                consumed = index + 1
                while consumed < len(lines) and re.match(r'(?:\t|    )\S', lines[consumed]):
                    message += '\n' + lines[consumed].rstrip()
                    consumed += 1
                supported = COMPILE.fullmatch(message) or phase == 'vet' and COPYLOCKS.search(message)
                if not path or not supported or blocked or len(message) > 4096 or phase == 'compile' and not owner:
                    issues.append({'reason': 'Go 文件归属或诊断签名不受支持'})
                elif phase == 'compile' and mode['argv'][1] == 'build' and path[1].endswith('_test.go'):
                    issues.append({'reason': 'go build 不编译测试文件'})
                else:
                    local.append(named(phase, mode, path[0], path[1], message))
            elif re.match(r'go: (?!downloading )|panic:|fatal(?: error)?:|[Ee][Rr][Rr][Oo][Rr]\b|curl:|signal:|.*(?:\.go:[0-9]+:|connection refused|could not resolve host|network unreachable)', line):
                issues.append({'reason': '另有未分类的 Go 或基础设施错误'})
        if not conflicted:
            found.extend(local)
    unique = {json.dumps(row['signature'], sort_keys=True): row for row in found}
    owners = {row['package'] for row in found}
    for row in terminals:
        if package(row.get('FailedBuild')) not in owners and package(row.get('Package')) not in owners:
            issues.append({'package': row.get('Package'), 'reason': '另有未绑定已知诊断的 Go 包失败'})
    return [unique[key] for key in sorted(unique)], issues


def subtest_name(value):
    if not value:
        return ''
    check(len(value) <= 512 and not re.search(r'[\x00-\x1f]', value), '子用例名称非法')
    call = ast.parse('f' + value, mode='eval').body
    check(isinstance(call, ast.Call) and isinstance(call.func, ast.Name) and call.func.id == 'f' and not call.args and call.keywords, '子用例参数不受支持')
    keys = [item.arg for item in call.keywords]
    check(all(key and re.fullmatch(IDENT, key) for key in keys) and len(keys) == len(set(keys)), '子用例键有歧义')
    for item in call.keywords:
        check(type(ast.literal_eval(item.value)) in (str, int, float, bool, type(None)), '子用例参数不是简单字面值')
    return value


def assertion(value):
    check(isinstance(value, str), 'AssertionError 签名类型非法')
    value = value.rstrip()
    check(len(value) <= 4096 and re.match(r'^AssertionError(?:: |$)', value) and not INFRA.search(value), '不是支持的稳定 AssertionError')
    lines = value.splitlines()
    lines[0] = re.sub(r'\s+:$', '', lines[0])
    return '\n'.join(lines)


def python_records(text, mode):
    script = next(arg for arg in mode['argv'] if arg in SCRIPTS)
    found, issues, counts = [], [], Counter()
    complete = re.search(r'(?m)^FAILED \(failures=[1-9][0-9]*(?:, errors=[0-9]+)?\)$', text)
    for header in re.finditer(r'(?m)^(FAIL|ERROR): (.+)$', text):
        try:
            match = re.fullmatch(r'(' + METHOD + r') \(__main__\.(' + IDENT + r')\.(' + METHOD + r')\)(?: (.+))?', header[2])
            check(header[1] == 'FAIL' and match and match[1] == match[3], 'unittest 失败身份不完整或不是断言')
            test, subtest = match[2] + '.' + match[1], subtest_name(match[4] or '')
            counts[(test, subtest)] += 1
            args = mode['argv']
            selected = [arg for arg in args if re.fullmatch(IDENT + r'\.' + METHOD, arg)]
            check(not selected or selected == [test], '失败不属于原始 unittest 选择')
            tail = text[header.end() + 1:]
            check(re.match(r'-{10,}\nTraceback \(most recent call last\):\n', tail), '缺少原生 unittest traceback')
            body = re.split(r'\n(?:={10,}|-{10,})\n', tail, maxsplit=1)[0]
            frames = re.findall(r'(?m)^  File "([^"]+)", line [0-9]+, in ([A-Za-z0-9_<>]+)$', body)
            expected = (script, mode['source_root'].rstrip('/') + '/' + script)
            check(any(path in expected and method == match[1] for path, method in frames), 'traceback 未绑定原始脚本和方法')
            check(all(not any(path.endswith('/' + allowed) or path == allowed for allowed in SCRIPTS) or path in expected for path, _ in frames), 'traceback 脚本来源冲突')
            errors = list(re.finditer(r'(?m)^AssertionError(?:: .*|)$', body))
            check(len(errors) == 1 and complete, '断言或原生失败终止记录缺失')
            found.append(named('unittest', mode, None, script, assertion(body[errors[0].start():]), test, subtest))
        except (ValueError, SyntaxError, TypeError) as error:
            issues.append({'reason': str(error)})
    if any(count > 1 for count in counts.values()):
        issues.append({'reason': '同名 unittest 或子用例失败重复，身份有歧义'})
    known = [row for row in found if counts[(row['test'], row['signature']['subtest'])] == 1]
    return sorted(known, key=lambda row: json.dumps(row['signature'], sort_keys=True)), issues


def failure_records(phase, log, context=None):
    try:
        mode = execution(phase, context)
        check(isinstance(log, str) and len(log.encode()) <= normal_replay().MAX_LOG, '原始诊断日志超过上限')
        text = cleaned(log)
        known, issues = python_records(text, mode) if phase == 'unittest' else go_records(phase, text, mode)
        if not known and not issues:
            issues.append({'reason': '没有可证明的已知诊断'})
        return known, issues
    except (ValueError, KeyError, TypeError, SyntaxError) as error:
        return [], [{'reason': '原始诊断上下文不可用：' + str(error)}]


def failures(phase, log, context=None):
    return failure_records(phase, log, context)[0]


def validate(candidate):
    check(isinstance(candidate, dict) and candidate.get('kind') == 'diagnostic', '不是诊断候选')
    phase = next((key for key, value in KINDS.items() if value == candidate.get('replay_kind')), None)
    mode = execution(phase, candidate.get('execution'))
    signature = candidate.get('signature')
    check(isinstance(signature, dict) and isinstance(candidate.get('file'), str) and candidate['file'] == signature.get('file'), '候选文件与签名不符')
    if phase == 'unittest':
        check(candidate['file'] in SCRIPTS and candidate['file'] in mode['argv'] and isinstance(candidate.get('test'), str) and re.fullmatch(IDENT + r'\.' + METHOD, candidate['test']), 'unittest 候选身份非法')
        expected = named(phase, mode, None, candidate['file'], assertion(signature['message']), candidate['test'], subtest_name(signature['subtest']))
    else:
        path = owned_file(candidate['file'], candidate.get('package'), mode)
        check(path and package(candidate.get('package')) == path[0] and path[1] == candidate['file'], '候选 Go 文件归属非法')
        message = signature.get('message')
        check(isinstance(message, str) and len(message) <= 4096 and (COMPILE.fullmatch(message) or phase == 'vet' and COPYLOCKS.search(message)), '候选 Go 签名非法')
        expected = named(phase, mode, path[0], path[1], message)
    check(expected['signature'] == signature, '候选签名含有未证明或非规范字段')
    return phase, mode


def commands(candidate):
    phase, mode = validate(candidate)
    argv = mode['argv']
    if phase == 'unittest':
        selection = [arg for arg in argv[1:] if arg != '-B'] if mode.get('requires_full') else [candidate['file'], candidate['test'], '-v']
        return {'run': ['python3', '-B', *selection]}
    flags, build, targets = go_args(argv)
    listing = ['go', 'list', '-deps', *(['-test'] if argv[1] != 'build' else []), '-json', *build, *targets]
    run = ['go', argv[1], *flags, *(['-c', '-o', '.periodic-diagnostic-binaries/'] if argv[1] == 'test' else []), *targets]
    return {'list': listing, 'run': run}


def source(root, sha, candidate, replay):
    for proof in candidate['execution']['proof']:
        check(replay.git(root, 'rev-parse', proof['sha'] + ':' + proof['path']) == proof['blob'], '原始命令源码 blob 不符')
    path = candidate['file']
    entry = replay.git(root, 'ls-tree', sha, '--', path)
    match = re.fullmatch(r'100(?:644|755) blob (' + replay.SHA_RE + r')\t' + re.escape(path), entry)
    check(match, '原始候选源码不存在、不是普通文件或为链接')
    text = replay.git(root, 'show', sha + ':' + path)
    check(len(text.encode()) <= replay.MAX_LOG, '候选源码超过上限')
    definition = {'state': 'present', 'files': {path: match[1]}}
    if candidate['replay_kind'] == 'python-unittest':
        cls, method = candidate['test'].split('.')
        classes = [node for node in ast.parse(text).body if isinstance(node, ast.ClassDef) and node.name == cls]
        check(len(classes) == 1 and not classes[0].decorator_list, '原始 unittest 类不存在、有歧义或有装饰器')
        methods = [node for node in classes[0].body if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef)) and node.name == method]
        check(len(methods) == 1 and isinstance(methods[0], ast.FunctionDef) and not methods[0].decorator_list, '原始 unittest 方法不存在、有歧义或可能跳过')
        check(any(ast.unparse(base) in ('unittest.TestCase', 'TestCase') for base in classes[0].bases), '原始类不是已知 TestCase')
        definition['method_sha256'] = hashlib.sha256(ast.dump(methods[0]).encode()).hexdigest()
    return definition


def inventory(text, candidate, checkout):
    decoder, offset, values = json.JSONDecoder(object_pairs_hook=normal_replay().unique_pairs), 0, []
    text = re.sub(r'(?m)^go: downloading [^\s]+ [^\s]+\n', '', text)
    while offset < len(text):
        if text[offset].isspace():
            offset += 1
            continue
        row, offset = decoder.raw_decode(text, offset)
        check(isinstance(row, dict) and not row.get('Error') and not row.get('DepsErrors'), 'Go 源码清单包含包错误')
        values.append(row)
    owner = candidate['package']
    directory, filename = candidate['file'].rsplit('/', 1)
    owned = [row for row in values if package(row.get('ImportPath')) == owner and
             row.get('Dir') == checkout + '/' + directory and isinstance(row.get('Module'), dict) and
             row['Module'].get('Path') == 'agentworkspace' and row['Module'].get('Main') is True]
    for row in owned:
        check(all(isinstance(row.get(key, []), list) and all(isinstance(name, str) for name in row.get(key, [])) for key in ('GoFiles', 'CgoFiles')), 'Go 文件清单类型非法')
    check(any(filename in row.get('GoFiles', []) + row.get('CgoFiles', []) for row in owned), '诊断文件未参与原始包图构建')
    if candidate['execution']['argv'][1] == 'test':
        check(any(not row.get('DepOnly') and (row.get('TestGoFiles') or row.get('XTestGoFiles')) for row in values), '原始选择没有可编译测试')


def python_proof(text, code, candidate):
    lines, events = [], []
    for line in text.splitlines():
        if line.startswith(HOOK_PREFIX):
            events.append(json.loads(line[len(HOOK_PREFIX):], object_pairs_hook=normal_replay().unique_pairs))
        else:
            lines.append(line)
    active, starts, successes = set(), Counter(), Counter()
    allowed = {'startTest', 'stopTest', 'addSuccess', 'addFailure', 'addError', 'addSubTest'}
    for row in events:
        check(isinstance(row, dict) and row.get('event') in allowed and isinstance(row.get('test'), str), 'unittest 事件缺失、溢出、跳过或不受支持')
        name, event = row['test'], row['event']
        check(re.fullmatch(r'__main__\.' + IDENT + r'\.' + METHOD, name), 'unittest 事件身份非法')
        if event == 'startTest':
            check(name not in active, 'unittest 重复开始')
            active.add(name)
            starts[name] += 1
        else:
            check(name in active, 'unittest 事件缺少活动方法')
            if event == 'stopTest':
                active.remove(name)
            elif event == 'addSuccess':
                successes[name] += 1
            elif code == 0:
                check(event == 'addSubTest' and row.get('state') == 'pass', '通过报告包含失败事件')
    target = '__main__.' + candidate['test']
    check(not active and starts.get(target) == 1 and all(count == 1 for count in starts.values()), '同名 unittest 生命周期不完整')
    check(candidate['execution'].get('requires_full') or list(starts) == [target], '执行了非原始选择的方法')
    native = '\n'.join(lines)
    summaries = re.findall(r'(?m)^Ran ([1-9][0-9]*) tests? in [0-9.]+s$', native)
    check(summaries == [str(sum(starts.values()))], '原生 unittest 总数与实际执行事件不符')
    subtest = candidate['signature']['subtest']
    if subtest:
        method = candidate['test'].split('.')[1]
        description = f'{method} (__main__.{candidate["test"]}) {subtest}'
        matching = [row for row in events if row['event'] == 'addSubTest' and row['test'] == target and row.get('description') == description]
        check(len(matching) == 1 and matching[0].get('state') == ('pass' if code == 0 else 'fail'), '缺少同名子用例实际结果事件')
    elif code == 1:
        check(sum(row['event'] == 'addFailure' and row['test'] == target for row in events) == 1, '缺少同名断言失败事件')
    if code == 0:
        marker = re.escape(candidate['test'].split('.')[1]) + r' \(__main__\.' + re.escape(candidate['test']) + r'\) \.\.\. ok'
        check(successes == starts and len(re.findall(r'(?m)^' + marker + '$', native)) == 1 and re.search(r'\nOK\s*\Z', native) and
              not re.search(r'(?i)skipped|expected failure|unexpected success|^FAIL:|^ERROR:', native, re.M), '同名 unittest 未完整通过或发生跳过')
    return native


def assess(candidate, text, code, checkout):
    phase, mode = validate(candidate)
    check(type(code) is int and code in (0, 1), '诊断进程返回码异常')
    if phase == 'unittest':
        text = python_proof(text, code, candidate)
    else:
        check(not re.search(r'no test files|no tests to run|^\s*--- SKIP:', text, re.M), 'Go 回放没有测试或发生跳过')
        for line in text.splitlines():
            if line.startswith('{'):
                row = json.loads(line, object_pairs_hook=normal_replay().unique_pairs)
                check(isinstance(row, dict) and row.get('Action') != 'skip', 'Go 回放包含非法事件或跳过')
    if code == 1:
        known, issues = failure_records(phase, text, {**mode, 'source_root': checkout})
        check(sum(row['signature'] == candidate['signature'] for row in known) == 1, '原始失败签名未复现或有歧义')
        return {'state': 'fail', 'signature': candidate['signature'], 'unresolved': issues}
    if phase != 'unittest':
        remaining = re.sub(r'(?m)^go: downloading [^\s]+ [^\s]+\n?', '', text)
        check(not remaining.strip(), '编译或 vet 成功包含无关输出、无测试或未分类诊断')
    return {'state': 'pass'}


class DiagnosticProbe:
    def __init__(self, root, output, replay=None, timeout=None, budget=12600):
        self.replay = normal_replay(replay)
        self.worker = self.replay.Probe(root, output, timeout=timeout, budget=budget)

    def close(self):
        self.worker.close()

    def __call__(self, sha, candidate):
        details = {'sha': sha}
        try:
            phase, mode = validate(candidate)
            check(isinstance(sha, str) and re.fullmatch(self.replay.SHA_RE, sha), '诊断回放 SHA 非法')
            check(time.monotonic() < self.worker.deadline, '诊断回放总预算已耗尽')
            details.update(tree=self.replay.git(self.worker.root, 'rev-parse', sha + '^{tree}'), execution=mode,
                           definition=source(self.worker.root, sha, candidate, self.replay))
            self.worker.index += 1
            with tempfile.TemporaryDirectory(prefix='periodic-diagnostic-') as temporary:
                checkout = Path(temporary).resolve() / 'source'
                # A local shared-object clone has its own config, never the source
                # repository's authenticated config or a linked worktree .git file.
                self.replay.git(self.worker.root, 'clone', '--quiet', '--shared', '--no-checkout', '--', str(self.worker.root.resolve()), str(checkout))
                self.replay.git(checkout, 'remote', 'remove', 'origin')
                self.replay.git(checkout, 'checkout', '--quiet', '--detach', sha)
                check(self.replay.git(checkout, 'rev-parse', 'HEAD') == sha, '诊断 checkout 身份不符')
                details['checkout'] = str(checkout)
                if phase == 'compile' and mode['argv'][1] == 'test':
                    (checkout / '.periodic-diagnostic-binaries').mkdir()
                previous = self.worker.env
                self.worker.env = {**self.replay.child_env(Path(self.worker.home.name)), 'PYTHONDONTWRITEBYTECODE': '1', 'PYTHONNOUSERSITE': '1'}
                try:
                    if phase == 'unittest':
                        hook = Path(temporary) / 'hook'
                        hook.mkdir()
                        (hook / 'sitecustomize.py').write_text(SUBTEST_HOOK)
                        self.worker.env.update(PYTHONPATH=str(hook), PERIODIC_DIAGNOSTIC_HOOK='1')
                        details['subtest_hook_sha256'] = hashlib.sha256(SUBTEST_HOOK.encode()).hexdigest()
                    for label, argv in commands(candidate).items():
                        proof, text = self.worker.execute(argv, checkout, 'diag-' + label, timeout=mode['timeout_seconds'])
                        details[label] = proof
                        check(not proof.get('state') and not proof.get('timed_out'), '诊断回放超过时间或日志上限')
                        if label == 'list':
                            check(type(proof.get('returncode')) is int and proof['returncode'] == 0, 'Go 原始包图列举失败')
                            inventory(text, candidate, str(checkout))
                    return {**details, **assess(candidate, text, proof.get('returncode'), str(checkout))}
                finally:
                    self.worker.env = previous
        except (ValueError, KeyError, TypeError, SyntaxError, OSError, subprocess.SubprocessError) as error:
            return {**details, **unknown('诊断回放无法证明：' + str(error))}

    def verify_probe(self, result, candidate, directory, seen=None):
        return verify_probe(result, candidate, directory, replay=self.replay, root=self.worker.root, seen=seen)


def verify_probe(result, candidate, directory, replay=None, root=None, seen=None):
    try:
        replay = normal_replay(replay)
        phase, mode = validate(candidate)
        check(root is not None and isinstance(result, dict), '缺少可信仓库或回放记录')
        sha = result.get('sha')
        check(isinstance(sha, str) and re.fullmatch(replay.SHA_RE, sha), '回放提交非法')
        check(result.get('tree') == replay.git(root, 'rev-parse', sha + '^{tree}') and result.get('execution') == mode, '回放 tree 或原始执行模式不符')
        check(result.get('definition') == source(root, sha, candidate, replay), '回放源码不能从可信提交重建')
        checkout = result.get('checkout')
        check(isinstance(checkout, str) and re.fullmatch(r'/[^\x00-\x1f]*/periodic-diagnostic-[A-Za-z0-9_-]+/source', checkout) and '..' not in Path(checkout).parts, '回放工作目录非法')
        if phase == 'unittest':
            check(result.get('subtest_hook_sha256') == hashlib.sha256(SUBTEST_HOOK.encode()).hexdigest(), '子用例事件钩子不符')
        directory = Path(directory)
        check(not directory.is_symlink(), '回放证据目录为链接')
        seen = set() if seen is None else seen
        for label, argv in commands(candidate).items():
            proof = result[label]
            check(isinstance(proof, dict), '诊断执行证据不是对象')
            name = proof.get('log')
            check(isinstance(name, str) and re.fullmatch(r'[0-9]{4,}-diag-' + label + r'\.jsonl', name) and name not in seen, '诊断证据路径非法或重复使用')
            path = directory / name
            check(not path.is_symlink() and path.is_file() and path.stat().st_size <= replay.MAX_LOG, '诊断证据超过上限或为链接')
            with path.open('rb') as stream:
                data = stream.read(replay.MAX_LOG + 1)
            check(len(data) <= replay.MAX_LOG and proof.get('sha256') == hashlib.sha256(data).hexdigest(), '诊断证据摘要不符')
            check(proof.get('argv') == argv and not proof.get('state') and not proof.get('timed_out'), '诊断 argv、超时或执行状态不符')
            limit = proof.get('limit_seconds')
            check(type(limit) in (int, float) and math.isfinite(limit) and 0 < limit <= mode['timeout_seconds'], '诊断进程预算超过原始上限')
            code, text = proof.get('returncode'), data.decode('utf-8')
            check(type(code) is int and code in (0, 1), '诊断进程返回码非法')
            seen.add(name)
            if label == 'list':
                check(code == 0, 'Go 源码清单失败')
                inventory(text, candidate, checkout)
        return assess(candidate, text, code, checkout)
    except (ValueError, KeyError, TypeError, SyntaxError, OSError, subprocess.SubprocessError) as error:
        return unknown('诊断原始证据不可验证：' + str(error))
