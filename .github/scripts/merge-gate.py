#!/usr/bin/env python3
"""Inspect an exact merge, select reverse Go dependencies, and verify terminal evidence."""
import argparse
from collections import Counter
import fnmatch
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile

HERE = Path(__file__).resolve().parent
IDENTITY = ("repo", "pr", "head", "base", "sha", "tree", "run_id", "run_attempt")
SHA = r"[0-9a-f]{40}"
POLICY_PATHS = (
    ".github/workflows/merge-gate.yml", ".github/scripts/merge-gate.py",
    ".github/scripts/merge-gate-manifest.json", ".github/scripts/merge-gate-migrations.json",
    ".github/scripts/merge-gate-migration.py", "internal/store/merge_gate_migration_test.go",
    ".github/scripts/ci-plan.py", ".github/scripts/ci-shards.py", ".github/scripts/verify-store-shard.py",
)


def module(name):
    spec = importlib.util.spec_from_file_location(name.replace("-", "_"), HERE / (name + ".py"))
    loaded = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(loaded)
    return loaded


routing = module("ci-plan")
shards = module("ci-shards")
verifier = shards.verifier
check = verifier.check


def command(argv, root, **kwargs):
    print("Merge:", json.dumps([str(arg) for arg in argv]), file=sys.stderr, flush=True)
    return subprocess.run(argv, cwd=root, check=True, **kwargs)


def git(root, *args):
    return routing.git(root, *args).decode().strip()


def native_identity(root, event, context):
    check(context["GITHUB_EVENT_NAME"] in ("pull_request", "workflow_dispatch"), "只接受原生 PR 或可信控制器事件")
    dispatched = context["GITHUB_EVENT_NAME"] == "workflow_dispatch"
    inputs = event.get("inputs", {})
    sha = inputs["sha"] if dispatched else context["GITHUB_SHA"]
    check(re.fullmatch(SHA, sha) is not None, "合并 SHA 非法")
    check(git(root, "rev-parse", "HEAD") == sha, "checkout 不是触发提交")
    parents = git(root, "show", "-s", "--format=%P", sha).split()
    pr = ({"number": int(inputs["pr"]), "head": {"sha": inputs["head"]},
           "base": {"repo": {"full_name": context["GITHUB_REPOSITORY"]}}} if dispatched else event["pull_request"])
    check(len(parents) == 2 and parents[1] == pr["head"]["sha"], "不是此 PR head 的真实双亲合并提交")
    if dispatched:
        check(parents[0] == inputs["base"] and git(root, "rev-parse", sha + "^{tree}") == inputs["tree"], "控制器输入与真实合并不符")
        check(context.get("MERGE_WORKFLOW_SHA", context.get("GITHUB_WORKFLOW_SHA")) == inputs["policy_sha"], "控制器必须使用指定的受信默认分支工作流")
    repo = context["GITHUB_REPOSITORY"]
    check(re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", repo) is not None, "仓库身份非法")
    check(pr["base"]["repo"]["full_name"] == repo, "PR 目标仓库不符")
    number = pr["number"] if dispatched else event["number"]
    check(type(number) is int and number > 0 and number == pr["number"], "PR 身份非法")
    for name in ("GITHUB_RUN_ID", "GITHUB_RUN_ATTEMPT"):
        check(re.fullmatch(r"[1-9][0-9]{0,63}", context[name]) is not None, "运行/尝试身份非法")
    # A shallow root is not sufficient evidence of real merge parents.
    for parent in parents:
        git(root, "cat-file", "-e", parent + "^{commit}")
    command(["git", "diff", "--no-ext-diff", "--check", parents[0], sha, "--"], root)
    result = dict(zip(IDENTITY, (repo, number, parents[1], parents[0], sha,
                                git(root, "rev-parse", sha + "^{tree}"),
                                context["GITHUB_RUN_ID"], context["GITHUB_RUN_ATTEMPT"])))
    result["source"] = ({"repo_id": int(inputs["source_repo_id"]), "repo": inputs["source_repo"],
                         "ref": inputs["source_ref"], "base_ref": inputs["base_ref"]} if dispatched else
                        {"repo_id": pr["head"]["repo"]["id"], "repo": pr["head"]["repo"]["full_name"],
                         "ref": pr["head"]["ref"], "base_ref": pr["base"]["ref"]})
    return result


def json_stream(text):
    decoder = json.JSONDecoder()
    while text.strip():
        text = text.lstrip()
        value, index = decoder.raw_decode(text)
        yield value
        text = text[index:]


def graph(root):
    """Union ordinary and race test variants, including external/test-only imports."""
    root = Path(root).resolve()
    rows = []
    for race in (False, True):
        argv = ["go", "list", "-deps", "-test", "-json"] + (["-race"] if race else []) + ["./..."]
        output = command(argv, root, capture_output=True, text=True).stdout
        variant = list(json_stream(output))
        check(variant, "Go 依赖图为空")
        check(all(not row.get("Error") and not row.get("DepsErrors") and not row.get("Incomplete") for row in variant),
              "Go 依赖图不完整")
        rows.extend((race, row) for row in variant)
    packages, reverse, files, ignored = {}, {}, {}, set()
    modes = {race: {"reverse": {}, "files": {}, "inputs": {}} for race in (False, True)}

    def canonical(name):
        return name.split(" [", 1)[0]

    for race, row in rows:
        if not row.get("Module", {}).get("Main") or not row.get("Dir"):
            continue
        package = row.get("ForTest") or canonical(row["ImportPath"])
        if package.endswith(".test"):
            continue
        check(re.fullmatch(r"[A-Za-z0-9._~+-]+(?:/[A-Za-z0-9._~+-]+)*", package) is not None,
              "Go 包路径非法")
        directory = Path(row["Dir"]).resolve()
        check(directory == root or root in directory.parents, "主模块目录越界")
        relative = directory.relative_to(root).as_posix()
        packages[package] = relative
        for dependency in row.get("Imports", []):
            dependency = row.get("ImportMap", {}).get(dependency, dependency)
            reverse.setdefault(canonical(dependency), set()).add(package)
            modes[race]["reverse"].setdefault(canonical(dependency), set()).add(package)
            modes[race]["inputs"].setdefault(package, set()).add("import:" + canonical(dependency))
        for key in ("GoFiles", "CgoFiles", "TestGoFiles", "XTestGoFiles", "EmbedFiles", "TestEmbedFiles", "XTestEmbedFiles"):
            for filename in row.get(key, []):
                path = (directory / filename).relative_to(root).as_posix()
                files.setdefault(path, set()).add(package)
                modes[race]["files"].setdefault(path, set()).add(package)
                modes[race]["inputs"].setdefault(package, set()).add("file:" + path)
        for filename in row.get("IgnoredGoFiles", []):
            ignored.add((directory / filename).relative_to(root).as_posix())
    check(packages, "主模块包为空")
    return packages, reverse, files, ignored, modes


def matches(path, prefix):
    return path.startswith(prefix) if prefix.endswith("/") else fnmatch.fnmatchcase(path, prefix)


def select(root, base, sha, manifest):
    root = Path(root).resolve()
    check(re.fullmatch(SHA, sha) is not None and git(root, "rev-parse", "HEAD") == sha, "依赖图必须来自 --sha 的实际 checkout")
    paths = routing.changed_paths(base, sha, root)
    deleted = set(os.fsdecode(path) for path in routing.git(root, "diff", "--no-renames", "--name-only", "--diff-filter=D", "-z", base, sha, "--").split(b"\0") if path)
    packages, reverse, files, ignored, modes = graph(root)
    seeds, conservative, all_go = set(), False, False
    web = bridge = ci = False
    for path in paths:
        owners = files.get(path, set())
        if owners:
            seeds.update(owners)
            if path in ignored:
                all_go = True
            continue
        if path in deleted and (path.startswith("docs/") or "/" not in path):
            # Candidate embed metadata has forgotten deleted assets; never classify these as opaque docs.
            all_go = True
            continue
        # Only opaque Markdown in docs/root is exempt; package fixtures are not.
        if path.endswith(".md") and (path.startswith("docs/") or "/" not in path):
            if not any(part in ("testdata", "fixtures") for part in Path(path).parts):
                continue
        if path.startswith(".github/"):
            ci = True
        elif path.startswith("web/"):
            web = True
        elif path.startswith("extensions/pi-bridge/"):
            bridge = True
            seeds.update(package for package, directory in packages.items()
                         if directory in ("internal/provider", "internal/platform"))
        elif path in ("go.mod", "go.sum", "go.work", "go.work.sum"):
            all_go = True
        else:
            candidates = [(directory.count("/") + (directory != "."), package)
                          for package, directory in packages.items()
                          if directory != "." and (path == directory or path.startswith(directory + "/"))]
            if candidates:
                depth = max(item[0] for item in candidates)
                seeds.update(package for level, package in candidates if level == depth)
                if path.endswith(".go"):
                    # Deleted/ignored/uncompiled source can hide imports absent from the live graph.
                    all_go = True
            elif path.startswith(("internal/", "cmd/")) or path.endswith(".go"):
                all_go = True
            else:
                conservative = all_go = web = bridge = ci = True
    impacted = set(packages) if all_go else set(seeds)
    pending = list(impacted)
    while pending:
        for dependent in reverse.get(pending.pop(), set()):
            if dependent in packages and dependent not in impacted:
                impacted.add(dependent)
                pending.append(dependent)
    def closure(mode):
        selected = set()
        for path in paths:
            selected.update(modes[mode]["files"].get(path, set()))
        queue = list(selected)
        while queue:
            for dependent in modes[mode]["reverse"].get(queue.pop(), set()):
                if dependent in packages and dependent not in selected:
                    selected.add(dependent)
                    queue.append(dependent)
        return selected

    race_full = closure(True) - closure(False)
    race_full.update(package for package in impacted if modes[True]["inputs"].get(package, set()) != modes[False]["inputs"].get(package, set()))
    for path in paths:
        if path in modes[True]["files"] and path not in modes[False]["files"]:
            race_full.update(closure(True))
    race, ordinary = {}, {}
    for rule in manifest["rules"]:
        triggered = conservative or any(matches(path, prefix) for path in paths for prefix in rule["paths"])
        for key, target in (("tests", race), ("ordinary", ordinary)):
            for package, names in rule.get(key, {}).items():
                if triggered or package in impacted:
                    target.setdefault(package, set()).update(names)
    impacted.update(ordinary)
    unresolved = []
    required = {package: race.get(package, set()) | ordinary.get(package, set()) for package in race.keys() | ordinary.keys()}
    for package, names in required.items():
        directory = root / packages[package] if package in packages else None
        source_names = set()
        if directory is not None:
            for source in directory.glob("*_test.go"):
                source_names.update(re.findall(r"(?m)^func (Test\w+)\(t \*testing\.T\)", source.read_text(encoding="utf-8")))
        if package == "agentworkspace/internal/store":
            source_names.update(re.findall(r"(?m)^func (Test\w+)\(t \*testing\.T\)", module("merge-gate-migration").TEMPLATE))
        unresolved.extend(package + ":" + name for name in names - source_names)
    return {"paths": paths, "packages": sorted(impacted), "race": {p: sorted(n) for p, n in sorted(race.items())},
            "ordinary_required": {p: sorted(n) for p, n in sorted(ordinary.items())},
            "unresolved": sorted(unresolved), "conservative": conservative, "all_race": all_go,
            "race_full": sorted(race_full),
            "web": web, "bridge": bridge, "ci": ci}


def tasks(selected):
    check(not selected["unresolved"], "必需回归测试缺失：" + ", ".join(selected["unresolved"]))
    result = [{"unit": "identity-diff", "kind": "identity"}]
    for race in (False, True):
        packages = selected["packages"] if not race or selected["all_race"] else sorted(set(selected["race"]) | set(selected.get("race_full", [])))
        for package in packages:
            profile = next((p for p in shards.BATCHES if package == "agentworkspace/internal/" + p), None)
            names = selected["race"].get(package) if race and not selected["all_race"] and package not in selected.get("race_full", []) else None
            buckets = 8 if race and names is None and package == "agentworkspace/cmd/workspace" else None
            workers = range(len(shards.BATCHES[profile])) if profile and names is None else range(buckets) if buckets else [None]
            # Keep the historical slow-slot admission order and the fixed BATCHES unchanged.
            if profile == "store" and names is None:
                workers = sorted(workers, key=lambda w: not shards.SLOW_STORE_SLOTS.intersection(shards.BATCHES[profile][w]))
            for worker in workers:
                unit = ("race-" if race else "ordinary-") + package.replace("/", "-")
                if worker is not None:
                    unit += "-" + str(worker)
                result.append({"unit": unit, "kind": "go", "package": package, "race": race,
                               "profile": profile, "worker": worker if not buckets else None,
                               "bucket": worker if buckets else None, "buckets": buckets, "names": names,
                               "required": (selected["race"] if race else selected.get("ordinary_required", {})).get(package, [])})
    if selected["packages"]:
        result.append({"unit": "vet", "kind": "vet", "packages": selected["packages"]})
    for client in ("web", "bridge", "ci"):
        if selected[client]:
            result.append({"unit": client, "kind": client})
    check(len({row["unit"] for row in result}) == len(result) <= 256, "任务重复或超过原生矩阵上限")
    return result


def finalize(plan, results, evidence):
    check(set(results) == {"plan", "tests"} and all(row.get("result") == "success" for row in results.values()),
          "计划和全部测试任务必须明确成功；取消/跳过不算成功")
    expected = {task["unit"] for task in plan["tasks"]}
    check(len(evidence) == len(expected) and {row.get("unit") for row in evidence} == expected, "任务证据缺失、重复或多余")
    by_unit = {row["unit"]: row for row in evidence}
    groups = {}
    for row in evidence:
        check(row.get("result") == "success" and all(row.get(field) == plan[field] for field in IDENTITY) and
              row.get("source", {}) == plan.get("source", {}), "任务终态或运行/PR/合并/来源身份不符")
    for task in plan["tasks"]:
        if task["kind"] != "go":
            continue
        detail = by_unit[task["unit"]]["detail"]
        compiled = verifier.counter(detail["compiled"])
        check(all(count == 1 for count in compiled.values()), "编译清单重复")
        check(not set(task.get("required", [])) - compiled.keys(), "必需测试未实际编译")
        if detail.get("compile_only"):
            check(not compiled and task.get("names") is None and task.get("worker") is None and task.get("bucket") is None, "有测试的任务不能伪装空包")
            continue
        selected, runs, passes = (verifier.counter(detail[key]) for key in ("selected", "runs", "passes"))
        skips = detail["skips"]
        check(isinstance(skips, dict) and all(type(count) is int and count == 1 for count in skips.values()), "跳过证据非法")
        check(not {name.split("/", 1)[0] for name in skips} & set(task.get("required", []) + (task.get("names") or [])), "必需测试不能跳过")
        top_skips = Counter({name: count for name, count in skips.items() if "/" not in name})
        check(runs == selected == passes + top_skips, "任务实际 run/pass/skip 覆盖不符")
        profile, worker = task.get("profile"), task.get("worker")
        if worker is not None:
            expected_names = sum((verifier.selection(compiled, slot, profile)[0] for slot in shards.BATCHES[profile][worker]), Counter())
        elif task.get("bucket") is not None:
            expected_names = bucket_selection(compiled, task["bucket"], task["buckets"])
        else:
            expected_names = Counter({name: 1 for name in task["names"]}) if task.get("names") is not None else compiled
        check(selected == expected_names, "任务过滤器不属于实际选定测试")
        if worker is not None or task.get("bucket") is not None:
            key = (task["package"], task["race"])
            group = groups.setdefault(key, {"compiled": compiled, "selected": Counter()})
            check(group["compiled"] == compiled and not group["selected"].keys() & selected.keys(), "跨桶清单不一致/重复覆盖")
            group["selected"].update(selected)
    check(all(group["compiled"] == group["selected"] for group in groups.values()), "逻辑槽/独立桶并集不等于完整编译清单")
    return {**{field: plan[field] for field in IDENTITY}, "schema": 1, "result": "success",
            "source": plan.get("source", {}), "policy_digest": plan.get("policy_digest"), "controller_sha": plan.get("controller_sha", ""),
            "tasks": plan["tasks"], "evidence": evidence}


def policy_digest(root):
    digest = hashlib.sha256()
    for path in POLICY_PATHS:
        digest.update(path.encode() + b"\0" + (Path(root) / path).read_bytes() + b"\0")
    return digest.hexdigest()


def inventory(root, package, race, directory, build_flags):
    metadata = json.loads(command(["go", "list", *build_flags, "-json", *(["-race"] if race else []), package], root,
                                  capture_output=True, text=True).stdout)
    check(not metadata.get("Incomplete") and not metadata.get("Error"), "测试包元数据不完整")
    if not metadata.get("TestGoFiles") and not metadata.get("XTestGoFiles"):
        return Counter()
    output = Path(directory) / "inventory.json"
    with output.open("wb") as stream:
        command(["go", "test", *build_flags, "-count=1", *(["-race"] if race else []), "-json", "-list", "^(Test|Example|Fuzz)", package], root, stdout=stream)
    # The shared verifier deliberately rejects empty inventories; packages without tests are valid.
    names = Counter()
    for row in verifier.events(output, package):
        check(row["Action"] == "output" and not row.get("Test"), "清单事件非法")
        for name in row["Output"].splitlines():
            if name.startswith(("Test", "Example", "Fuzz")):
                verifier.valid_name(name)
                names[name] += 1
    check(all(count == 1 for count in names.values()), "测试清单重复")
    return names


def bucket_selection(compiled, bucket, total):
    check(type(bucket) is int and type(total) is int and total == 8 and bucket in range(total), "独立桶配置非法")
    return Counter({name: count for name, count in compiled.items()
                    if int.from_bytes(hashlib.sha256(name.encode()).digest()[:8], "big") % total == bucket})


def verify_suite(path, package, selected, required, allow_skips):
    skips = Counter()
    normalized = Path(str(path) + ".verified")
    with path.open(encoding="utf-8") as source, normalized.open("w", encoding="utf-8") as output:
        for line in source:
            row = verifier.decode(line)
            if row.get("Action") == "skip":
                name = row.get("Test", "")
                check(allow_skips and name and name.split("/", 1)[0] not in required, "必需测试跳过或未执行，不能作为通过证据")
                skips[name] += 1
                row["Action"] = "pass"
            output.write(json.dumps(row) + "\n")
    runs, passes = verifier.verify(normalized, package, selected)
    top_skips = Counter({name: count for name, count in skips.items() if "/" not in name})
    return runs, passes - top_skips, skips


def run_vet(root, packages):
    check(packages and all(re.fullmatch(r"[A-Za-z0-9._~+-]+(?:/[A-Za-z0-9._~+-]+)*", p) for p in packages), "vet 包清单非法")
    command(["go", "vet", *packages], root)


def ci_tests(root, manifest):
    directory = Path(root) / ".github/scripts"
    paths = sorted(directory.glob("test-*.py"))
    names = {path.name for path in paths}
    required = set(manifest["ci_tests"])
    check(required <= names, "必需 CI 自测缺失：" + ", ".join(sorted(required - names)))
    for prefix, tests in manifest.get("ci_modules", {}).items():
        if list(directory.glob(prefix)):
            check(any(any(fnmatch.fnmatchcase(name, pattern) for pattern in tests) for name in names), "CI 模块缺少已登记自测：" + prefix)
    check(all(path.is_file() and not path.is_symlink() for path in paths), "CI 自测文件非法")
    return paths


def run_go(root, task):
    if task["package"] == "agentworkspace/internal/store":
        with module("merge-gate-migration").generated(root, HERE / "merge-gate-migrations.json") as flags:
            return _run_go(root, task, flags)
    return _run_go(root, task, [])


def _run_go(root, task, build_flags):
    package, race = task["package"], task["race"]
    check(re.fullmatch(r"[A-Za-z0-9._~+-]+(?:/[A-Za-z0-9._~+-]+)*", package) is not None, "包路径非法")
    with tempfile.TemporaryDirectory(prefix="merge-tests-") as temporary:
        compiled = inventory(root, package, race, temporary, build_flags)
        check(not set(task.get("required", [])) - compiled.keys(), "必需名称不在实际编译测试清单中")
        if not compiled:
            check(task["names"] is None and task["worker"] is None, "必需测试/分片不能使用空清单")
            command(["go", "test", *build_flags, "-count=1", *(["-race"] if race else []), "-run", "^$", package], root)
            return {"compiled": {}, "selected": {}, "compile_only": True}
        if task["names"] is not None:
            selected = Counter({name: 1 for name in task["names"]})
            check(selected and not selected.keys() - compiled.keys(), "必需名称不在实际编译测试清单中")
            profile = task["profile"]
            groups = shards.execution_subsets(selected, profile, 6 if profile == "store" else None)
        elif task.get("bucket") is not None:
            groups = [bucket_selection(compiled, task["bucket"], task["buckets"])]
        elif task["worker"] is not None:
            profile, worker = task["profile"], task["worker"]
            check(profile in shards.BATCHES and type(worker) is int and worker in range(len(shards.BATCHES[profile])), "分片配置非法")
            groups = [subset for slot in shards.BATCHES[profile][worker]
                      for subset in shards.execution_subsets(verifier.selection(compiled, slot, profile)[0], profile, slot)]
        else:
            groups = [compiled]
        observed_runs, observed_passes, observed_skips = Counter(), Counter(), Counter()
        for index, selected in enumerate(groups):
            output = Path(temporary) / (str(index) + ".json")
            pattern = verifier.run_pattern(selected)
            argv = ["go", "test", *build_flags, "-count=1", *(["-race"] if race else []), "-json", "-run", pattern, package]
            try:
                with output.open("wb") as stream:
                    command(argv, root, stdout=stream, env={**os.environ, "TMPDIR": temporary,
                                                               "MERGE_GATE_MIGRATIONS_AUTHORITY": str(HERE / "merge-gate-migrations.json")})
            finally:
                if output.exists():
                    shards.log_events(output, sys.stdout)
            runs, passes, skips = verify_suite(output, package, selected, set(task.get("required", [])) | set(task["names"] or []), task["names"] is None)
            observed_runs.update(runs)
            observed_passes.update(passes)
            observed_skips.update(skips)
        return {"compiled": dict(compiled), "selected": dict(sum(groups, Counter())),
                "runs": dict(observed_runs), "passes": dict(observed_passes), "skips": dict(observed_skips)}


def run_task(root, plan, index, event, context):
    actual = native_identity(root, event, context)
    check(all(plan[field] == actual[field] for field in IDENTITY) and plan.get("source") == actual["source"], "计划与实际 checkout/run/来源不符")
    check(type(index) is int and index in range(len(plan["tasks"])), "任务编号非法")
    task = plan["tasks"][index]
    detail = {}
    if task["kind"] == "go":
        detail = run_go(root, task)
    elif task["kind"] in ("web", "bridge"):
        routing.clients({"mode": "feedback", task["kind"]: True}, task["kind"])
    elif task["kind"] == "vet":
        run_vet(root, task["packages"])
        detail = {"packages": task["packages"]}
    elif task["kind"] == "ci":
        for script in ci_tests(root, read_json(HERE / "merge-gate-manifest.json")):
            command([sys.executable, "-B", str(script), "-v"], root)
    else:
        check(task["kind"] == "identity", "未知任务类型")
    return {**actual, "unit": task["unit"], "result": "success", "detail": detail}


def read_json(path):
    return verifier.decode(Path(path).read_text(encoding="utf-8"))


def load_manifest(path):
    value = read_json(path)
    check(value.get("schema") == 1 and isinstance(value.get("rules"), list) and value["rules"], "回归登记 schema/rules 非法")
    required = {"test-ci-scheduling.py", "test-store-shard.py", "test-merge-gate.py", "test-merge-gate-publish.py", "test-merge-gate-workflow.py"}
    check(isinstance(value.get("ci_tests"), list) and required <= set(value["ci_tests"]), "必需 CI 自测登记不完整")
    for rule in value["rules"]:
        check(isinstance(rule.get("paths"), list) and rule["paths"] and all(isinstance(p, str) and p and not p.startswith("/") and ".." not in p.split("/") for p in rule["paths"]), "回归路径登记非法")
        for key in ("tests", "ordinary"):
            check(isinstance(rule.get(key, {}), dict), "回归包登记非法")
            for package, names in rule.get(key, {}).items():
                check(isinstance(package, str) and isinstance(names, list) and names and len(names) == len(set(names)), "必需回归名称登记非法")
                for name in names:
                    verifier.valid_name(name)
    return value


def write_json(path, value):
    Path(path).parent.mkdir(parents=True, exist_ok=True)
    Path(path).write_text(json.dumps(value, sort_keys=True, ensure_ascii=True) + "\n", encoding="utf-8")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    subs = parser.add_subparsers(dest="command", required=True)
    inspect = subs.add_parser("inspect", help="只读列出路径、包和普通/race 必需测试，不运行测试")
    inspect.add_argument("--root", type=Path, default=Path.cwd())
    inspect.add_argument("--base", required=True)
    inspect.add_argument("--sha", required=True)
    inspect.add_argument("--manifest", type=Path, default=HERE / "merge-gate-manifest.json")
    plan_parser = subs.add_parser("plan")
    plan_parser.add_argument("output", type=Path)
    run = subs.add_parser("run")
    run.add_argument("plan", type=Path)
    run.add_argument("index", type=int)
    run.add_argument("output", type=Path)
    final = subs.add_parser("final")
    final.add_argument("plan", type=Path)
    final.add_argument("evidence", type=Path)
    final.add_argument("output", type=Path)
    args = parser.parse_args()
    if args.command == "inspect":
        selected = select(args.root, args.base, args.sha, load_manifest(args.manifest))
        selected["tasks"] = tasks(selected) if not selected["unresolved"] else []
        print(json.dumps(selected, sort_keys=True, ensure_ascii=True, indent=2))
        check(not selected["unresolved"], "必需测试未解析，已拒绝生成可执行计划")
        return
    root = Path.cwd()
    event = read_json(os.environ["GITHUB_EVENT_PATH"])
    actual = native_identity(root, event, os.environ)
    if args.command == "plan":
        selected = select(root, actual["base"], actual["sha"], load_manifest(HERE / "merge-gate-manifest.json"))
        plan = {**actual, **selected, "tasks": tasks(selected), "policy_digest": policy_digest(HERE.parents[1]),
                "controller_sha": os.environ.get("MERGE_WORKFLOW_SHA", "") if os.environ["GITHUB_EVENT_NAME"] == "workflow_dispatch" else ""}
        write_json(args.output, plan)
        matrix = {"include": [{"index": index, "unit": task["unit"], "kind": task["kind"]}
                              for index, task in enumerate(plan["tasks"])]}
        with open(os.environ["GITHUB_OUTPUT"], "a", encoding="utf-8") as output:
            output.write("matrix=" + json.dumps(matrix, ensure_ascii=True) + "\n")
    else:
        plan = read_json(args.plan)
        check(all(plan[field] == actual[field] for field in IDENTITY), "计划身份不匹配")
        if args.command == "run":
            write_json(args.output, run_task(root, plan, args.index, event, os.environ))
        else:
            evidence = [read_json(path) for path in sorted(args.evidence.glob("*/unit.json"))]
            write_json(args.output, finalize(plan, json.loads(os.environ["NEEDS_JSON"]), evidence))


if __name__ == "__main__":
    try:
        main()
    except (ValueError, KeyError, TypeError, OSError, UnicodeError, subprocess.CalledProcessError) as error:
        print(f"Merge 校验失败：{error}", file=sys.stderr)
        sys.exit(1)
