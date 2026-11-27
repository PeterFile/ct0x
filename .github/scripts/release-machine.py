#!/usr/bin/env python3
"""Operator-owned private HDD bridge. Only `run --execute-native` executes AGY."""
import argparse
import base64
from concurrent.futures import ThreadPoolExecutor
from contextlib import closing
import importlib.util
import json
import os
from pathlib import Path
import platform
import re
import runpy
import shutil
import sqlite3
import subprocess
import sys

_spec = importlib.util.spec_from_file_location("release_core", Path(__file__).with_name("release-core.py"))
core = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(core)
control = core.load_script("release-control")
evidence = core.load_script("release-evidence")
host = core.load_script("release-host")
check = core.check
STATE_ROOT = host.registry_root()
UNKNOWN_TESTS = (
    ("agentworkspace/internal/provider", "TestACPAgyDurableCancellationRequiresSettledPrompt"),
    ("agentworkspace/internal/store", "TestQueueUntrustworthyReceiptsRemainUnknownAndHoldEveryClaim"),
    ("agentworkspace/internal/store", "TestTaskSuccessorPredecessorUnknownBlocksUntilReconciled"),
    ("agentworkspace/internal/scheduler", "TestRunUnknownRetainsCapacityAndResourcesAcrossRecoveryWithoutReplay"),
)


def private_directory(root):
    check(not root.is_symlink() and root.is_dir() and root.stat().st_mode & 0o777 == 0o700, "必须使用已有的非符号链接 0700 私有目录")


def claim_path(root, repo, run_id, attempt):
    core.repository(repo)
    core.number(run_id)
    core.number(attempt)
    return root / "claims" / f"{core.hashed(repo.encode())}-{run_id}-{attempt}"


def reserve(root, repo, run_id, attempt):
    private_directory(root)
    claim = claim_path(root, repo, run_id, attempt)
    STATE_ROOT.mkdir(mode=0o700, parents=True, exist_ok=True)
    private_directory(STATE_ROOT)
    active = STATE_ROOT / "machine-active"
    try:
        active.mkdir(mode=0o700)
    except FileExistsError:
        raise ValueError("机器仍被已认领或未知执行占用；禁止抢占或自动重试") from None
    permanent = claim_path(STATE_ROOT, repo, run_id, attempt)
    if permanent.exists() or claim.exists():
        # This new lock has not started external work. Never remove a prior lock.
        active.rmdir()
        raise ValueError("本次 run/attempt 已有本地认领；禁止重放，先核对私有原生证据")
    claim.parent.mkdir(mode=0o700, exist_ok=True)
    claim.mkdir(mode=0o700)
    permanent.mkdir(mode=0o700, parents=True)
    identity = {"repo": repo, "run_id": run_id, "run_attempt": attempt}
    core.save(active / "owner.json", identity)
    core.save(permanent / "claim.json", identity | {"claim": str(claim.resolve(strict=True))})
    core.save(claim / "reserved.json", identity)
    return claim


def locate_claim(root, repo, run_id, attempt):
    registered = claim_path(STATE_ROOT, repo, run_id, attempt) / "claim.json"
    if registered.exists():
        value = core.decode(registered.read_bytes())
        check(all(value.get(key) == item for key, item in {"repo": repo, "run_id": run_id, "run_attempt": attempt}.items()), "固定机器认领身份不符")
        claim = Path(value["claim"])
    else:
        claim = claim_path(root, repo, run_id, attempt)
    if not claim.exists():
        raise FileNotFoundError("尚无本次原始机器工作")
    private_directory(claim)
    return claim


def authorized_host(root):
    check(platform.system() == "Darwin" and platform.machine() == "x86_64", "必须使用 Darwin x86_64 授权机器")
    host.validate_scope(root)


def machine_identity():
    return core.hashed(core.encoded(host.load_binding()))


def install_binary(path, data, expected):
    check(core.hashed(data) == core.digest(expected), "下载二进制摘要不符")
    with path.open("xb") as output:
        output.write(data)
    path.chmod(0o700)
    check(core.file_hash(path) == expected and path.stat().st_mode & 0o777 == 0o700, "落盘后二进制摘要或执行权限不符")


def identity(build, sha, binary_hash):
    check(build.get("vcs_revision") == sha and build.get("vcs_modified") is False and build.get("sha256") == binary_hash,
          "原始审计返回的提交或实际二进制摘要不符")


def assertions(values, required=()):
    check(isinstance(values, dict) and bool(values) and set(required) <= set(values)
          and all(value is True for value in values.values()), "原始审计缺少正面判据或判据失败")
    return values


def terminal(receipt, session, outcomes):
    check(receipt.get("native_session_id") == session and receipt.get("terminal") == receipt.get("outcome")
          and receipt.get("outcome") in outcomes and receipt.get("delivery_failed") is False
          and all(receipt.get(field) is True for field in ("started", "accepted", "direct_child_reaped", "output_complete")),
          "原生终态未接纳、未结清或与会话不符")


def native_proof(root, turn, session, protocol, transcript, protocol_hash, transcript_hash):
    core.exact(turn, r"[A-Za-z0-9_-]{1,128}", "Turn ID 非法")
    core.exact(session, r"[A-Za-z0-9_-]{1,128}", "原生会话 ID 非法")
    for path, expected in ((protocol, protocol_hash), (transcript, transcript_hash)):
        resolved = path.resolve(strict=True)
        check(resolved.is_file() and resolved.is_relative_to(root.resolve(strict=True))
              and resolved.stat().st_size <= 32 << 20
              and core.file_hash(resolved) == core.digest(expected), "缺少本地原生证据或原始证据摘要不符")
    events = [core.decode(line, 8 << 20) for line in protocol.read_bytes().splitlines() if line.strip()]
    sessions = [event for event in events if event.get("type") == "session_new"]
    check(len(sessions) == 1 and sessions[0].get("session_sha256") == core.hashed(session.encode()), "协议未绑定实际原生 session")
    settled = [event for event in events if event.get("type") == "prompt_terminal"]
    check(len(settled) == 1 and settled[0].get("tools_settled") is True
          and settled[0].get("stop_reason") in ("end_turn", "cancelled"), "缺少原始已结清 prompt_terminal 协议")
    return {"turn_id": turn, "session_id": session, "protocol_sha256": protocol_hash, "transcript_sha256": transcript_hash}


def lane(name, report, checks, proofs, attempt_ids=()):
    assertions(checks)
    return {"name": name, "status": "success", "audit_sha256": core.hashed(core.encoded(report)),
            "assertions_sha256": core.hashed(core.encoded(checks)), "assertion_count": len(checks), "checks": checks,
            "proofs": proofs, "attempt_ids": sorted(set(attempt_ids))}


def normalize_6a(report, root, sha, binary_hash):
    check(report.get("passed") is True and report.get("errors") == [], "6A 原始审计不完整")
    identity(report["binary"], sha, binary_hash)
    checks, proofs = {}, []
    for label, outcomes, required in (("positive_turn", {"succeeded"}, {"canary_absent_from_prompt", "canary_recovered_from_tool"}),
                                      ("negative_turn", {"succeeded", "failed"}, {"pretool_hard_denied", "synthetic_file_canary_absent"})):
        entry = report[label]
        raw = entry["native_evidence"]
        terminal(entry["execution_receipt"], entry["native_session_id"], outcomes)
        selected = {name: entry.get("transcript_checks", {}).get(name) for name in required}
        assertions(selected, required)
        checks.update({label + "_" + key: value for key, value in selected.items()})
        protocol = root / "data" / "runtime" / entry["id"] / "stdout.jsonl"
        proofs.append(native_proof(root, entry["id"], entry["native_session_id"], protocol, Path(raw["transcript"]),
                                   raw["stdout_sha256"], raw["transcript_sha256"]))
    checks.update({key: report.get("http_mcp_attestation", {}).get(key) for key in
                   ("roots_list_changed_202_empty", "bearer_http_mcp_2025_06_18_read_channel")})
    checks.update(loopback_closed=report.get("loopback_closed"), graceful_shutdown=report.get("workspace_shutdown", {}).get("graceful"))
    return lane("6a", report, checks, proofs)


def normalize_6b(report, root, sha, binary_hash):
    check(report.get("passed") is True, "6B 原始审计不完整")
    identity(report["build"], sha, binary_hash)
    checks = assertions(report["checks"], evidence.LANE_CHECKS["6b"])
    check(set(report["native"]) == {"b", "a2", "c", "constraint", "handback", "answer"}, "6B 缺少原始原生场景证据")
    database = (root / "data/workspace.db").resolve(strict=True)
    check(Path(report["database"]).resolve(strict=True) == database, "6B 原始数据库作用域不符")
    with closing(sqlite3.connect(database.as_uri() + "?mode=ro", uri=True)) as db:
        db.row_factory = sqlite3.Row
        db.execute("PRAGMA query_only=ON")
        db.execute("BEGIN")
        turns = list(db.execute("SELECT id,native_session_id,state,receipt_json FROM turns"))
        proofs, attempts = [], []
        for role, native in report["native"].items():
            assertions({key: native.get(key) for key in ("session_bound", "model_matches", "matched_call")})
            matches = []
            for row in turns:
                runtime = root / "data/runtime" / row["id"]
                protocol = runtime / "stdout.jsonl"
                transcript = runtime / "gemini/aw-app/brain" / (row["native_session_id"] or "missing") / ".system_generated/logs/transcript_full.jsonl"
                if protocol.is_file() and transcript.is_file() and core.file_hash(protocol) == native["protocol_sha256"] and core.file_hash(transcript) == native["transcript_sha256"]:
                    matches.append((row, protocol, transcript))
            check(len(matches) == 1, "6B 原生摘要无法唯一绑定持久 Turn")
            row, protocol, transcript = matches[0]
            terminal(core.decode(row["receipt_json"].encode()), row["native_session_id"], {"aborted"} if role == "b" else {"succeeded"})
            check(row["state"] == ("aborted" if role == "b" else "succeeded"), "6B 持久终态不符")
            proofs.append(native_proof(root, row["id"], row["native_session_id"], protocol, transcript, native["protocol_sha256"], native["transcript_sha256"]))
            bound = [item[0] for item in db.execute("SELECT id FROM task_attempts WHERE turn_id=?", (row["id"],))]
            check(bool(bound), "6B 缺少真实任务 attempt 绑定")
            attempts.extend(bound)
    return lane("6b", report, checks, proofs, attempts)


def normalize_6c(report, root, sha, binary_hash):
    check(report.get("passed") is True and report.get("revision") == sha and report.get("binary_sha256") == binary_hash,
          "6C 原始审计提交或实际二进制不符")
    checks = {"pair_" + key: value for key, value in assertions(report["paired"], {
        "distinct_native_session", "distinct_member_id", "distinct_port", "distinct_database_inode", "same_related_initial_context"}).items()}
    check([case.get("noise") for case in report["cases"]] == [0, 200], "6C 必须包含实际 0/200 双夹具")
    proofs = []
    for case in report["cases"]:
        prefix = str(case["noise"]) + "_"
        required = {key.removeprefix(prefix) for key in evidence.LANE_CHECKS["6c"] if key.startswith(prefix)}
        checks.update({str(case["noise"]) + "_" + key: value for key, value in assertions(case["checks"], required).items()})
        protocol = root / f"acceptance-{case['noise']:03d}" / "data/runtime" / case["turn_id"] / "stdout.jsonl"
        proofs.append(native_proof(root, case["turn_id"], case["native_session"], protocol, Path(case["transcript"]),
                                   case["protocol_sha256"], case["transcript_sha256"]))
    return lane("6c", report, checks, proofs)


def verify_unknown_race(events):
    check(not any(event.get("Action") == "fail" for event in events), "unknown race 回归发生实际失败")
    for package, test in UNKNOWN_TESTS:
        matched = [event for event in events if event.get("Package") == package and event.get("Test") == test and event.get("Action") in ("pass", "fail", "skip")]
        started = [event for event in events if event.get("Package") == package and event.get("Test") == test and event.get("Action") == "run"]
        package_pass = [event for event in events if event.get("Package") == package and not event.get("Test") and event.get("Action") == "pass"]
        check(len(started) == len(matched) == len(package_pass) == 1 and matched[0]["Action"] == "pass",
              "缺少 unknown race 的实际测试运行、通过或包终态证据：" + test)
    return True


def native_environment():
    return native_environment_from(os.environ)


def native_environment_from(source):
    # Preserve original CLI configuration references, never injected credentials,
    # user PATH entries, executable hooks, proxies, or language loader settings.
    allowed = ("HOME", "USER", "LOGNAME", "TMPDIR", "LANG", "LC_ALL", "TZ", "XDG_CONFIG_HOME", "XDG_DATA_HOME",
               "GOCACHE", "GOTMPDIR", "GOMODCACHE")
    return {key: source[key] for key in allowed if key in source} | {
        "PATH": "/usr/bin:/bin:/usr/sbin:/sbin", "GOTOOLCHAIN": "local", "GOWORK": "off", "GOPROXY": "off", "GOSUMDB": "off"}


def command(args, log, cwd=None, env=None, timeout=5400):
    with log.open("xb") as output:
        subprocess.run([str(item) for item in args], cwd=cwd, env=env or native_environment(), stdin=subprocess.DEVNULL,
                       stdout=output, stderr=subprocess.STDOUT, timeout=timeout, check=True)


def driver(args, stdout, stderr, timeout=5400):
    with stdout.open("xb") as output, stderr.open("xb") as errors:
        subprocess.run([sys.executable, "-B", *map(str, args)], env=native_environment(), stdin=subprocess.DEVNULL,
                       stdout=output, stderr=errors, timeout=timeout, check=True)


def source_manifest(harness=None):
    if harness is None:
        harness = Path(host.load_binding()["harness"])
    paths, public, private_bundle = host.source_manifest(harness)
    support = {key: value for key, value in private_bundle.items() if key not in public}
    check(public == evidence.TRUSTED_DRIVERS and private_bundle == public | support
          and core.hashed(core.encoded(support)) == evidence.TRUSTED_SUPPORT_SHA256, "原始支持源码与稳定受审依赖不符")
    paths = dict(paths)
    for name in evidence.CONTROL_SOURCES:
        paths[name] = Path(__file__).with_name(name.replace("_", "-") + ".py")
    check(set(paths) == evidence.SOURCES, "原始驱动依赖清单不完整")
    check(all(path.is_file() and not path.is_symlink() for path in paths.values()), "缺少原始私有验收驱动")
    sources = {name: core.file_hash(path) for name, path in paths.items()}
    check(sources == evidence.trusted_sources(), "原始验收安装与稳定受审适配器不符；必须先审查源码变更")
    return paths, sources, {"sources": sources, "support_sha256": core.hashed(core.encoded(support))}


def ensure_active(api, run_id, attempt):
    run = api.run(run_id)
    check(run.get("run_attempt") == attempt and run.get("status") == "in_progress", "候选 owner 已取消、停止或 attempt 过期；不能开始新的外部执行")
    return control.guard(api, run_id, attempt)


def clean_snapshot(api, intent, claim):
    source = claim / "source"
    env = native_environment()
    token = base64.b64encode(("x-access-token:" + api.token).encode()).decode()
    env.update(GIT_CONFIG_NOSYSTEM="1", GIT_CONFIG_GLOBAL=os.devnull, GIT_TERMINAL_PROMPT="0", GIT_CONFIG_COUNT="1",
               GIT_CONFIG_KEY_0="http.https://github.com/.extraheader", GIT_CONFIG_VALUE_0="AUTHORIZATION: basic " + token)
    command(["git", "clone", "--no-checkout", "--single-branch", "--branch", "master", "https://github.com/" + api.repo + ".git", source], claim / "clone.log", env=env)
    remote_master = core.git(source, "rev-parse", "refs/remotes/origin/master")
    command(["git", "-C", source, "merge-base", "--is-ancestor", intent["sha"], remote_master], claim / "ancestry.log")
    check(core.git(source, "rev-parse", intent["sha"] + "^{tree}") == intent["tree"], "本地 target tree 不符")
    command(["git", "-C", source, "-c", "core.hooksPath=/dev/null", "checkout", "--detach", intent["sha"]], claim / "checkout.log")
    # This ref belongs only to this new private clone. Original drivers require
    # origin/master == revision even when authenticated remote master advances.
    command(["git", "-C", source, "update-ref", "refs/remotes/origin/master", intent["sha"], remote_master], claim / "snapshot-ref.log")
    core.save(claim / "snapshot.json", {"sha": intent["sha"], "tree": intent["tree"], "observed_remote_master": remote_master})
    check(not core.git(source, "status", "--porcelain"), "私有候选检出不干净")
    return source


def seed_helper(go, source, seeder_source, claim):
    directory = claim / "fixture-helper"
    directory.mkdir(mode=0o700)
    shutil.copyfile(seeder_source, directory / "main.go")
    shutil.copyfile(source / "go.sum", directory / "go.sum")
    match = re.search(r"(?m)^go ([0-9.]+)$", (source / "go.mod").read_text())
    check(match is not None, "目标 Go 版本缺失")
    (directory / "go.mod").write_text("module agentworkspace/acceptance6b\n\ngo " + match[1] +
                                    "\n\nrequire agentworkspace v0.0.0\nreplace agentworkspace => " + json.dumps(str(source)) + "\n")
    helper = claim / "seed-artifact-6b"
    command([go, "build", "-mod=mod", "-buildvcs=false", "-o", helper, "."], claim / "seeder-build.log", directory,
            dict(native_environment(), CGO_ENABLED="0", GOFLAGS="-p=2"), timeout=900)
    return helper


def reports(claim, intent, binary_hash):
    result = []
    for name, normalize, path in (("6a", normalize_6a, claim / "acceptance-6a-release/6a-receipt.json"),
                                  ("6b", normalize_6b, claim / "6b-audit.json"),
                                  ("6c", normalize_6c, claim / "6c-audit.json")):
        report = core.decode(path.read_bytes(), 16 << 20)
        normalized = normalize(report, claim / f"acceptance-{name}-release", intent["sha"], binary_hash)
        normalized["audit_sha256"] = core.file_hash(path)
        normalized.update({key: intent[key] for key in ("repo", "sha", "tree", "run_id", "run_attempt")})
        normalized.update(schema=1, artifact_sha256=binary_hash, full_binding_sha256=None)
        result.append(normalized)
    return result


def native_run(args, api, intent, claim, binary, paths, lease):
    lease.stage("clean-snapshot")
    source = clean_snapshot(api, intent, claim)
    lease.stage("seed-helper")
    seeder = seed_helper(args.go, source, paths["seed_6b"], claim)
    root_a, root_b, root_c = (claim / f"acceptance-{name}-release" for name in ("6a", "6b", "6c"))
    # Original 6A has an operator-local AGY binding, not an event-supplied command.
    lease.stage("agy-binding")
    original = runpy.run_path(str(paths["driver_6a"]))
    check(Path(original["AGY"]).resolve(strict=True) == args.agy.resolve(strict=True), "6A 原始驱动的 AGY 绑定与本机授权 CLI 不符")
    ensure_active(api, args.run_id, args.attempt)
    # Prepare itself starts services. Persist this boundary before any driver call.
    lease.external("6c-prepare")
    driver([paths["driver_6c"], "prepare", "--binary", binary, "--workspace", source, "--revision", intent["sha"],
            "--go", args.go, "--agy", args.agy, "--root", root_c], claim / "6c-prepare.stdout", claim / "6c-prepare.stderr")
    ensure_active(api, args.run_id, args.attempt)
    # Cover both possibly started lanes, including a crash between submissions.
    lease.external("6a-execute")
    lease.external("6c-execute")
    with ThreadPoolExecutor(max_workers=2) as executor:
        futures = [executor.submit(driver, [paths["driver_6a"], "--binary", binary, "--commit", intent["sha"], "--go", args.go,
                                            "--schema-version", args.schema_version, "--trial-dir", root_a], claim / "6a.stdout", claim / "6a.stderr"),
                   executor.submit(driver, [paths["driver_6c"], "execute", "--root", root_c], claim / "6c.stdout", claim / "6c.stderr")]
        # Wait for both original processes, including on failure; never overlap 6B.
        errors = []
        for future in futures:
            try:
                future.result()
            except Exception as error:
                errors.append(error)
        check(not errors, "6A 或 6C 原始驱动未通过；保留私有输出并停止，不自动重试")
    lease.stage("6c-audit")
    driver([paths["driver_6c"], "audit", "--root", root_c], claim / "6c-audit.json", claim / "6c-audit.stderr")
    ensure_active(api, args.run_id, args.attempt)
    lease.external("6b-prepare")
    driver([paths["prepare_6b"], "--binary", binary, "--root", root_b, "--seeder", seeder], claim / "6b-prepare.stdout", claim / "6b-prepare.stderr")
    lease.external("6b-execute")
    driver([paths["driver_6b"], "--worktree", source, "--binary", binary, "--revision", intent["sha"], "--root", root_b,
            "--seeder", seeder, "--agy", args.agy], claim / "6b-audit.json", claim / "6b.stderr")
    timeline_path = root_b / "b-cancellation.jsonl"
    timeline = [core.decode(line, 2 << 20) for line in timeline_path.read_bytes().splitlines() if line.strip()]
    if any(item["turn"]["state"] == "unknown" for item in timeline):
        check(all(item["claims"] for item in timeline if item["turn"]["state"] == "unknown"), "实际 unknown 没有持锁证据")
        return {"mode": "observed", "proof_sha256": core.file_hash(timeline_path)}
    lease.stage("unknown-race")
    pattern = "^(" + "|".join(test for _, test in UNKNOWN_TESTS) + ")$"
    raw = claim / "unknown-race.jsonl"
    command([args.go, "test", "-count=1", "-race", "-json", "-run", pattern, "./internal/provider", "./internal/store", "./internal/scheduler"],
            raw, source, dict(native_environment(), CGO_ENABLED="1"), timeout=1800)
    events = [core.decode(line, 8 << 20) for line in raw.read_bytes().splitlines() if line.strip()]
    verify_unknown_race(events)
    return {"mode": "race", "proof_sha256": core.file_hash(raw)}


def verify_control_files(intent):
    root = Path(__file__).resolve().parents[2]
    paths = {".github/scripts/" + source.replace("_", "-") + ".py" for source in evidence.CONTROL_SOURCES} | set(control.FULL_PATHS)
    for path in sorted(paths):
        loaded = (root / path).resolve(strict=True)
        check(core.git(root, "hash-object", str(loaded)) == intent["sources"][path], "本地实际加载的桥接或 Full 策略源码不是受信任版本")


def run(args, api):
    check(args.execute_native, "真实验收必须显式传入 --execute-native；该操作可能产生模型费用")
    intent = ensure_active(api, args.run_id, args.attempt)
    verify_control_files(intent)
    authorized_host(args.work_dir)
    paths, sources, bundle = source_manifest(args.harness_root)
    build, data, target_fact = evidence.target(api, intent)
    with core.load_script("release-reconcile").execution(globals(), args, intent) as lease:
        claim = lease.claim
        core.save(claim / "intent.json", intent)
        core.save(claim / "source-manifest.json", bundle)
        core.save(claim / "execution-config.json", {"schema_version": args.schema_version})
        binary = claim / "workspace"
        lease.stage("install-binary")
        install_binary(binary, data, build["artifact_sha256"])
        lease.stage("binary-before")
        before = evidence.binary_identity(args.go, binary, intent, native_environment())
        core.save(claim / "binary-before.json", before)
        unknown = native_run(args, api, intent, claim, binary, paths, lease)
        lease.stage("binary-after")
        after = evidence.binary_identity(args.go, binary, intent, native_environment())
        check(before == after, "实际执行二进制的前后元数据或摘要改变")
        core.save(claim / "binary-after.json", after)
        check(core.file_hash(binary) == build["artifact_sha256"], "验收期间二进制改变")
        _, final_sources, final_bundle = source_manifest(args.harness_root)
        check(sources == final_sources and bundle == final_bundle, "验收期间原始驱动或支持源码改变")
        lease.stage("normalize")
        value = {key: intent[key] for key in ("repo", "sha", "tree", "run_id", "run_attempt", "workflow_sha")} | {
            "schema": 1, "kind": "release-machine", "status": "success", "verified": True,
            "artifact_sha256": build["artifact_sha256"], "target_artifact": target_fact,
            "machine_id": machine_identity(), "source_bundle_sha256": core.hashed(core.encoded(bundle)),
            "sources": sources, "lanes": reports(claim, intent, build["artifact_sha256"]), "unknown_guard": unknown, "full_binding": None}
        evidence.validate_machine(value)
        ensure_active(api, args.run_id, args.attempt)
        lease.stage("complete")
        core.save(claim / "receipt.json", value)
        core.save(claim / "complete.json", {"receipt_sha256": core.file_hash(claim / "receipt.json")})
        lease.complete()
    print("原生验收通过；仅私有保存。使用 submit 子命令递交归一化回执。")


def reconcile_submission(api, intent, value, root):
    candidates = evidence.find_intakes(api, intent, root)
    check(all(item[0]["machine"] == value for item in candidates), "已观察到冲突回执；禁止重新提交")
    if not candidates:
        return {"accepted": None, "status": "unknown"}
    return {"accepted": True, "intake_run_id": min(item[0]["intake"]["run_id"] for item in candidates)}


def submit(args, api):
    private_directory(args.work_dir)
    current = api.run(args.run_id)
    completed = current.get("status") == "completed"
    if completed:
        claim = locate_claim(args.work_dir, api.repo, args.run_id, args.attempt)
        check((claim / "submission-started.json").is_file(), "完成后的 owner 只能对账原始提交，不能新递交")
    prepared = core.load_script("release-finalize").prepare_submission(args, api, completed=completed)
    if prepared.get("status") == "waiting":
        return prepared
    claim = locate_claim(args.work_dir, api.repo, args.run_id, args.attempt)
    value = prepared
    intent = control.guard(api, args.run_id, args.attempt, completed=completed)
    receipt_hash = core.hashed(core.encoded(value))
    result_path = claim / "submission-result.json"
    if result_path.exists():
        check(core.decode((claim / "submission-started.json").read_bytes())["receipt_sha256"] == receipt_hash, "已提交回执摘要改变")
        return core.decode(result_path.read_bytes())
    started = claim / "submission-started.json"
    if not started.exists():
        check(not completed, "完成后的 owner 禁止新递交")
        # No key enrollment or prompt: an absent reviewed anchor yields an empty signature.
        signature = core.load_script("release-host-proof").sign_payload(value, Path(__file__).resolve().parents[2], intent["workflow_sha"])
        evidence.machine_signature(signature)
        ensure_active(api, args.run_id, args.attempt)
        core.save(started, {"receipt_sha256": receipt_hash, "signature_sha256": core.hashed(signature.encode())})
        try:
            response = api.request("POST", f"{api.prefix}/actions/workflows/release-receipt.yml/dispatches",
                                   {"ref": "master", "return_run_details": True,
                                    "inputs": {"receipt": core.encoded(value).decode().strip(), "machine_signature": signature}})
            if response is None:  # Native 204, not fabricated JSON.
                result = {"accepted": True, "response": "204"}
            else:
                check(isinstance(response, dict), "dispatch 返回不是原生 run-details JSON")
                run_id = core.number(response["workflow_run_id"])
                check(response.get("html_url") == f"https://github.com/{api.repo}/actions/runs/{run_id}"
                      and response.get("run_url") == f"https://api.github.com{api.prefix}/actions/runs/{run_id}", "dispatch 返回身份不符")
                result = {"accepted": True, "response": "200", "intake_run_id": run_id}
            core.save(result_path, result)
            return result
        except (OSError, ValueError, KeyError):
            pass  # The durable started marker prohibits another POST.
    check(core.decode(started.read_bytes())["receipt_sha256"] == receipt_hash, "提交中的回执摘要改变")
    result = reconcile_submission(api, intent, value, Path(__file__).resolve().parents[2])
    if result["accepted"] is True:
        core.save(result_path, result)
    return result


def main():
    os.umask(0o077)
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("run", "submit", "finalize-submit", "finalize"))
    parser.add_argument("--repo", required=True)
    parser.add_argument("--run-id", type=int, required=True)
    parser.add_argument("--attempt", type=int, required=True)
    parser.add_argument("--work-dir", type=Path, required=True)
    parser.add_argument("--harness-root", type=Path)
    parser.add_argument("--expected-sha")
    parser.add_argument("--artifact", type=Path)
    parser.add_argument("--go", type=Path, default=Path(shutil.which("go") or "go"))
    parser.add_argument("--agy", type=Path, default=Path(shutil.which("agy") or "agy"))
    parser.add_argument("--schema-version", type=int)
    parser.add_argument("--execute-native", action="store_true")
    args = parser.parse_args()
    check(sys.flags.optimize == 0, "原始验收依赖 Python assertions，禁止优化模式")
    args.work_dir = args.work_dir.absolute()
    if args.command == "run":
        check(type(args.schema_version) is int and args.schema_version > 0, "真实运行需要正式迁移版本")
        args.go, args.agy = args.go.resolve(strict=True), args.agy.resolve(strict=True)
    elif args.command == "finalize":
        core.sha(args.expected_sha)
        check(args.artifact is not None, "最终复核必须指定待发布实际文件 --artifact")
    api = control.AdmissionAPI(args.repo)
    if args.command == "run":
        run(args, api)
    elif args.command == "finalize":
        core.load_script("release-finalize").finalize(args, api)
        print("本机最终权威证明已写入私有 claim；未发布或创建标签。")
    else:
        result = submit(args, api)
        print("等待 Full 或私有原生证据；未提交。" if result.get("status") == "waiting" else
              "复核完成，递交请求已接受；仍需核实原生 intake。" if result.get("accepted") is True else "提交结果未知；只能只读对账，禁止重发。")
        return 0 if result.get("accepted") is True else 75


if __name__ == "__main__":
    try:
        sys.exit(main())
    except (ValueError, KeyError, OSError, subprocess.SubprocessError, sqlite3.Error):
        # Raw errors can contain native text, URLs, or private absolute paths.
        print("本地验收未完成；保留私有目录与机器认领，只能先对账，禁止自动重放。", file=sys.stderr)
        sys.exit(1)
