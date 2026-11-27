#!/usr/bin/env python3
"""Read-only native/Full join. Never start a service, provider, model, or release."""
from contextlib import closing, contextmanager, redirect_stdout
import copy
import fcntl
import importlib.util
import io
import os
from pathlib import Path
import runpy
import sqlite3
import sys
from types import SimpleNamespace
from urllib.parse import quote

_spec = importlib.util.spec_from_file_location("release_core", Path(__file__).with_name("release-core.py"))
core = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(core)
machine = core.load_script("release-machine")
evidence = machine.evidence
control = machine.control
proof = core.load_script("release-host-proof")
check = core.check


def raw_file(root, path, limit=32 << 20):
    path = Path(path)
    resolved = path.resolve(strict=True)
    check(not path.is_symlink() and resolved.is_relative_to(root.resolve(strict=True))
          and resolved.is_file() and resolved.stat().st_size <= limit, "缺少、越界或过大的原始私有证据")
    return resolved


def read(root, path):
    return core.decode(raw_file(root, path).read_bytes(), 32 << 20)


def json_lines(root, path):
    return [core.decode(line, 8 << 20) for line in raw_file(root, path).read_bytes().splitlines() if line.strip()]


@contextmanager
def originals(paths, lane):
    """Load only the fixed audited helpers; isolate their historical module names."""
    names = ("audit", "facts", "prepare_fixture", "verify_6b", "export_fixture", "native_exact", "native_preliminary",
             "native_project_stage", "normal_entry", "compare_first_turn")
    previous = {name: sys.modules.pop(name) for name in names if name in sys.modules}
    search, bytecode = sys.path[:], sys.dont_write_bytecode
    env = dict(os.environ)
    try:
        sys.dont_write_bytecode = True
        sys.path[:0] = [str(paths["driver_" + lane].parent), str(paths["audit_6b"].parent), str(paths["normal_entry"].parent)]
        os.environ.clear()
        os.environ.update(machine.native_environment_from(env))
        yield runpy.run_path(str(paths["driver_6a" if lane == "6a" else "verifier_6b" if lane == "6b" else "driver_6c"]))
    except Exception:
        raise ValueError("原始只读审计未通过；不得接纳归一化回执") from None
    finally:
        os.environ.clear()
        os.environ.update(env)
        sys.path[:], sys.dont_write_bytecode = search, bytecode
        for name in names:
            sys.modules.pop(name, None)
        sys.modules.update(previous)


def audit_6a(claim, report, original, binary, intent):
    root = claim / "acceptance-6a-release"
    data, database = root / "data", root / "data/workspace.db"
    check(Path(report["binary"]["path"]).resolve(strict=True) == binary.resolve(strict=True), "6A 没有执行本次下载的实际文件")
    result = copy.deepcopy(report)
    result["binary"] = original["build_identity"](original["GO"], binary, intent["sha"])
    check(report["expected_schema_version"] == read(claim, claim / "execution-config.json")["schema_version"], "6A 原始迁移判据改变")
    with (data / "workspace.lock").open("rb") as lock, closing(sqlite3.connect(database.resolve(strict=True).as_uri() + "?mode=ro", uri=True)) as db:
        fcntl.flock(lock, fcntl.LOCK_SH | fcntl.LOCK_NB)
        db.row_factory = sqlite3.Row
        db.execute("PRAGMA query_only=ON")
        db.execute("BEGIN")
        binding = report["bindings"]
        member = db.execute("SELECT group_id,provider_id,model FROM members WHERE id=?", (binding["member_id"],)).fetchone()
        check(member is not None and all(member[key] == binding[key] for key in member.keys()), "6A 持久成员与原始 provider 绑定不符")
        canary = db.execute("SELECT content FROM messages WHERE id=? AND group_id=?", (report["canary"]["message_id"], binding["group_id"])).fetchone()
        check(canary is not None and canary[0].startswith("验收频道记录测试金丝雀 "), "6A 缺少原始频道 canary")
        nonce = canary[0].removeprefix("验收频道记录测试金丝雀 ")
        check(core.hashed(nonce.encode()) == report["canary"]["sha256"], "6A 原始 canary 改变")
        for label, outcomes in (("positive_turn", {"succeeded"}), ("negative_turn", {"succeeded", "failed"})):
            entry = result[label]
            turn = db.execute("SELECT * FROM turns WHERE id=?", (entry["id"],)).fetchone()
            check(turn is not None and all(turn[key] == entry[key] for key in entry if key in turn.keys()), "6A SQLite Turn 与原始观察不符")
            check(turn["native_session_id"] and turn["native_session_id"] not in {turn["id"], "fixture", "native-session"}, "6A 原生会话不是实际新建会话")
            receipt = core.decode(turn["receipt_json"].encode())
            machine.terminal(receipt, turn["native_session_id"], outcomes)
            check(receipt.get("exit_code") == 0 and receipt.get("logs_truncated") is False
                  and original["redact_receipt"](receipt) == entry["execution_receipt"], "6A 原始终态或输出未结清")
            delivery = db.execute("SELECT state,turn_id,conversation_id FROM deliveries WHERE id=?", (entry["delivery_id"],)).fetchone()
            check(delivery is not None and delivery["state"] == "accepted" and delivery["turn_id"] == turn["id"]
                  and delivery["conversation_id"] == turn["conversation_id"] and turn["accepted_at"]
                  and turn["group_id"] == binding["group_id"] and turn["member_id"] == binding["member_id"], "6A 接纳或群组身份不符")
            trigger = db.execute("SELECT content FROM messages WHERE id=? AND group_id=?", (entry["trigger_message_id"], binding["group_id"])).fetchone()
            check(trigger is not None, "6A 缺少原始触发消息")
            for field in ("stdout_path", "stderr_path"):
                raw_file(root, receipt[field])
            events = original["native_transcript"](data, entry, receipt)
            models = {row["model"] for row in events if row.get("type") == "PLANNER_RESPONSE" and row.get("model")}
            check(models == {binding["model"]}, "6A 原始模型绑定不符")
            reply = db.execute("SELECT sender_id,content FROM messages WHERE id=?", (turn["reply_message_id"],)).fetchone()
            readback = {"delivery_state": delivery["state"], "run_state": turn["state"]}
            if label == "positive_turn":
                entry["transcript_checks"] = original["positive_transcript"](events, nonce, report["canary"]["message_id"], trigger[0])
                check(reply is not None and reply["sender_id"] == binding["member_id"] and nonce in reply["content"], "6A 原始回复没有工具 canary")
                readback["reply_contains_canary"] = True
            else:
                fixture = raw_file(root, root / "synthetic-restricted-file.txt")
                secret = fixture.read_text().strip()
                check(bool(secret), "6A 缺少拒绝 canary")
                entry["transcript_checks"] = original["negative_transcript"](events, secret, fixture)
                check(reply is None or secret not in reply["content"], "6A 原始回复泄漏文件 canary")
            entry["http_readback"] = readback
    ids = [result[name]["id"] for name in ("positive_turn", "negative_turn")]
    result["database"] = original["database_checks"](database, ids)
    health = result["database"]
    check(health["quick_check"] == ["ok"] and health["foreign_key_errors"] == health["resource_claims"] == health["queued_deliveries"] == 0
          and health["migration_versions"] == list(range(1, report["expected_schema_version"] + 1))
          and health["exact_requested_turns"] and not any(health["turn_counts"].get(state, 0) for state in original["ACTIVE"] | {"unknown"}), "6A 原始数据库不健康或未结清")
    result["http_mcp_attestation"] = original["http_attestation"](root, raw_file(root, root / "workspace.stdout.log"), ids[0])
    raw_file(root, root / "workspace.stderr.log")
    check(report["workspace_shutdown"].get("graceful") is True and report["workspace_shutdown"].get("exit_code") == 0
          and report["loopback_closed"] is True and report["passed"] is True and report["errors"] == [], "6A 原始关闭或验收未通过")
    check(result == report, "6A 原始 SQLite、协议、transcript 或 HTTP 审计复算不符")
    return result


def recompute(args, claim, intent, paths, binary_hash):
    """No cached passed/assertion flags can replace any original auditor."""
    source, binary = claim / "source", claim / "workspace"
    check(core.git(source, "rev-parse", "HEAD") == intent["sha"]
          and core.git(source, "rev-parse", "HEAD^{tree}") == intent["tree"]
          and not core.git(source, "status", "--porcelain"), "原始私有源码快照不符")
    for database in (claim / "acceptance-6a-release/data/workspace.db", claim / "acceptance-6b-release/data/workspace.db",
                     *(claim / "acceptance-6c-release" / f"acceptance-{noise:03d}/data/workspace.db" for noise in (0, 200))):
        raw_file(claim, database, 512 << 20)
    # Bound and hash native bytes before handing them to the original auditors.
    normalized = machine.reports(claim, intent, binary_hash)
    recorded_a = read(claim, claim / "acceptance-6a-release/6a-receipt.json")
    recorded_b, recorded_c = (read(claim, claim / (name + "-audit.json")) for name in ("6b", "6c"))
    with originals(paths, "6a") as original:
        audit_6a(claim, recorded_a, original, binary, intent)
    with originals(paths, "6b") as original:
        root = claim / "acceptance-6b-release"
        scope = read(claim, root / "settled-scope.json")
        check(Path(recorded_b["build"]["path"]).resolve(strict=True) == binary.resolve(strict=True), "6B 实际文件不是本次下载二进制")
        output = io.StringIO()
        with redirect_stdout(output):
            status = original["audit"](root, root / "data/workspace.db", scope,
                SimpleNamespace(worktree=source, binary=binary, revision=intent["sha"]))
        check(status == 0 and core.decode(output.getvalue().encode(), 16 << 20) == recorded_b, "6B 原始事实、HTTP、SQLite 或原生协议复算不符")
    with originals(paths, "6c") as original:
        root = claim / "acceptance-6c-release"
        plan = read(claim, root / "plan.json")
        check(Path(plan["binary"]).resolve(strict=True) == binary.resolve(strict=True)
              and plan["binary_sha256"] == binary_hash and plan["revision"] == intent["sha"]
              and plan["helper_sha256"] == core.file_hash(raw_file(root, root / "fixture-helper/seed-artifact")), "6C 原始 prepare、helper 或实际执行文件不符")
        evidence.verify_binary_metadata(raw_file(root, root / "workspace.buildinfo.txt").read_text(), intent)
        original["check_build"](source, binary, args.go, intent["sha"])
        report = original["paired_audit"](root, plan)
        check(report.get("passed") is True and report == recorded_c, "6C 原始 0/200 私有证据复算不符")
    check(machine.reports(claim, intent, binary_hash) == normalized, "原始审计期间 lane 字节改变")
    return normalized


def unknown_guard(claim, expected, scope):
    evidence.keys(expected, {"mode", "proof_sha256"})
    if expected["mode"] == "race":
        raw = raw_file(claim, claim / "unknown-race.jsonl")
        machine.verify_unknown_race(json_lines(claim, raw))
    else:
        check(expected["mode"] == "observed", "unknown guard 模式不符")
        root = claim / "acceptance-6b-release"
        raw = raw_file(claim, root / "b-cancellation.jsonl")
        rows = json_lines(claim, raw)
        check(len(rows) >= 4 and any(row["turn"]["state"] == "unknown" for row in rows), "原始日志没有 unknown 状态观察")
        first, last = rows[0], rows[-1]
        stat = (root / "data/workspace.db").stat()
        identity = [stat.st_dev, stat.st_ino]
        turn = first["turn"]
        check(turn["id"] == scope["b_turn"] and turn["group_id"] == scope["group_id"]
              and turn["state"] == "running" and turn["accepted_at"] and turn["native_session_id"]
              and first["claims"] and len(first["claims"]) == len(set(first["claims"]))
              and all(isinstance(key, str) and key for key in first["claims"]), "unknown 原始接纳、身份或 claim 不符")
        fields = ("id", "group_id", "member_id", "delivery_id", "native_session_id", "accepted_at")
        times = [row["captured_ns"] for row in rows]
        check(times == sorted(set(times)) and all(type(stamp) is int and stamp > 0 for stamp in times)
              and all(row["database"] == identity and all(row["turn"][key] == turn[key] for key in fields) for row in rows), "unknown 数据库、Turn 或时序不符")
        check(any(row["turn"]["state"] == "cancel_requested" for row in rows[1:-1])
              and last["turn"].get("cancel_requested_at")
              and all(row["turn"]["state"] in ("cancel_requested", "unknown") and row["claims"] == first["claims"]
                      and row["turn"]["cancel_requested_at"] == last["turn"]["cancel_requested_at"] for row in rows[1:-1])
              and last["turn"]["state"] == "aborted" and not last["claims"], "unknown 或取消请求期间未保持原始全部 claim")
        machine.terminal(core.decode(last["turn"]["receipt_json"].encode()), turn["native_session_id"], {"aborted"})
    check(core.file_hash(raw) == expected["proof_sha256"], "unknown 原始字节摘要改变")
    return expected


def verify_local(args, api, intent, claim, build, target_fact):
    machine.private_directory(args.work_dir)
    machine.private_directory(claim)
    machine.authorized_host(claim)
    machine.verify_control_files(intent)
    value = read(claim, claim / "receipt.json")
    check(value["machine_id"] == machine.machine_identity(), "原始验收回执不是本机绑定的安装产生")
    complete = read(claim, claim / "complete.json")
    check(core.file_hash(claim / "receipt.json") == complete["receipt_sha256"]
          and read(claim, claim / "intent.json") == intent, "本机完成回执或原始 intent 改变")
    evidence.bind_machine(value, intent | {"artifact_sha256": build["artifact_sha256"]}, target_fact)
    paths, sources, bundle = machine.source_manifest(args.harness_root)
    check(read(claim, claim / "source-manifest.json") == bundle and sources == value["sources"]
          and bundle == evidence.trusted_bundle() and core.hashed(core.encoded(bundle)) == value["source_bundle_sha256"], "原始审计驱动或其实际 helper 依赖改变")
    binary = raw_file(claim, claim / "workspace", 160 << 20)
    actual = evidence.binary_identity(args.go, binary, intent, machine.native_environment())
    machine.identity(actual, intent["sha"], build["artifact_sha256"])
    check(actual == read(claim, claim / "binary-before.json") == read(claim, claim / "binary-after.json"), "实际运行文件的 prepare/pre/post 元数据或摘要不符")
    check(recompute(args, claim, intent, paths, build["artifact_sha256"]) == value["lanes"], "原始 lane 证据与归一化输入不符")
    scope_path = claim / "acceptance-6b-release/settled-scope.json"
    scope = read(claim, scope_path) if value["unknown_guard"]["mode"] == "observed" else {}
    unknown_guard(claim, value["unknown_guard"], scope)
    check(machine.source_manifest(args.harness_root)[1:] == (sources, bundle) and core.file_hash(binary) == actual["sha256"], "复算期间源码或执行文件改变")
    return value


def full_ready(api, intent):
    rows = [job for job in api.jobs(intent["run_id"], intent["run_attempt"]) if job["name"] == "Release Full evidence"]
    check(len(rows) <= 1, "Full producer 重复")
    if not rows:
        return False
    job = rows[0]
    check(job.get("run_id") == intent["run_id"] and job.get("run_attempt") == intent["run_attempt"], "Full producer attempt 不符")
    if job.get("status") != "completed":
        return False
    check(job.get("conclusion") == "success", "Full producer 已失败或取消；不能提交机器验收")
    return True


def prepare_submission(args, api, *, completed=False):
    """Completed mode only rechecks a successful original owner; it cannot dispatch."""
    intent = control.guard(api, args.run_id, args.attempt, completed=completed)
    value = prepare_local(args, api, intent)
    check(control.guard(api, args.run_id, args.attempt, completed=completed) == intent, "复核期间 canonical owner 改变")
    return value


def prepare_local(args, api, intent):
    machine.verify_control_files(intent)
    machine.private_directory(args.work_dir)
    machine.authorized_host(args.work_dir)
    if not full_ready(api, intent):
        return {"status": "waiting", "reason": "full"}
    # Full-first and native-first are both owned actions, not new watchers.
    binding = evidence.full_binding(api, intent)
    try:
        claim = machine.locate_claim(args.work_dir, api.repo, args.run_id, args.attempt)
    except FileNotFoundError:
        return {"status": "waiting", "reason": "native"}
    if not (claim / "complete.json").is_file():
        return {"status": "waiting", "reason": "native"}
    build, _, target_fact = evidence.target(api, intent)
    value = copy.deepcopy(verify_local(args, api, intent, claim, build, target_fact))
    value["full_binding"] = binding
    for lane in value["lanes"]:
        lane["full_binding_sha256"] = core.hashed(core.encoded(binding))
    evidence.validate_machine(value, require_full=True)
    path = claim / "joined-receipt.json"
    reconcile_file(path, value)
    return value


def reconcile_file(path, value):
    if path.exists():
        check(not path.is_symlink() and core.decode(path.read_bytes(), proof.MAX_MATERIALS + core.MAX_RECEIPT) == value, "已存在证据与本次独立复核不符")
        return
    try:
        core.save(path, value)
        path.chmod(0o600)
    except FileExistsError:
        check(not path.is_symlink() and core.decode(path.read_bytes(), proof.MAX_MATERIALS + core.MAX_RECEIPT) == value, "并行复核证据冲突")


def control_root():
    return Path(__file__).resolve().parents[2]


def no_anchor_intent(api, run_id, attempt, root):
    """A terminal exception for one missing-anchor step, never an execution guard."""
    latest, current, original = api.run(run_id), api.run(run_id, attempt), api.run(run_id, 1)
    for run in (latest, current):
        core.native_candidate(run, api.repo)
        check(run.get("run_attempt") == attempt and (run.get("status"), run.get("conclusion")) == ("completed", "failure"),
              "无锚点复核只接受本次完整失败终态")
    core.native_candidate(original, api.repo)
    fields = ("path", "event", "head_branch", "head_sha", "head_commit")
    check(all(latest.get(key) == current.get(key) == original.get(key) for key in fields), "失败终态不是原始不可变 owner")
    check(control.actor_identity(latest.get("actor")) == control.actor_identity(current.get("actor"))
          == control.actor_identity(original.get("actor")), "失败终态改变了原始 actor")
    actor = control.actor_identity(current.get("triggering_actor"))
    check(actor == control.actor_identity(latest.get("triggering_actor")), "终态触发者不符")
    if attempt > 1:
        repo = api.get(api.prefix)
        check(repo.get("full_name") == api.repo, "原生仓库根身份不符")
        core.number(repo.get("id"))
        if actor[0] != core.number(repo.get("owner", {}).get("id")):
            permission = api.get(api.prefix + "/collaborators/" + quote(actor[1], safe="") + "/permission")
            check(control.actor_identity(permission.get("user")) == actor and permission.get("permission") in ("admin", "write"),
                  "原生重跑 actor 没有当前仓库写权限")
    intent = control.read_intent(api, run_id, attempt)
    check(control.canonical_status(api, api.repo, intent["sha"]) == run_id, "失败终态不是 canonical owner")
    machine.verify_control_files(intent)
    jobs = api.jobs(run_id, attempt)
    admission = control.admission_name(intent["sha"])
    if not any(job["name"] == admission for job in jobs):
        admission = "Release admission"
    core.require_jobs(jobs, run_id, attempt, ["Release normalize", admission, "Release clients", "Release candidate binary",
                      "Release Full evidence", "Release machine evidence", evidence.MATERIALS_JOB])
    verdicts = [job for job in jobs if job["name"] == evidence.VERDICT_JOB]
    check(len(verdicts) == 1 and verdicts[0].get("status") == "completed" and verdicts[0].get("conclusion") == "failure",
          "无锚点复核缺少唯一的失败 verdict 作业")
    verdict = verdicts[0]
    raw_jobs = api.pages(f"{api.prefix}/actions/runs/{run_id}/attempts/{attempt}/jobs", "jobs")
    check(all(job["id"] == verdict["id"] or (job.get("status") == "completed" and job.get("conclusion") in ("success", "skipped"))
              for job in raw_jobs), "除机器证明步骤外仍有失败、取消或未完成作业")
    steps = verdict.get("steps", [])
    failed = [step for step in steps if step.get("conclusion") == "failure"]
    check(len(failed) == 1 and failed[0].get("name") == "Enforce remote machine proof"
          and failed[0].get("status") == "completed"
          and sum(step.get("name") == "Enforce remote machine proof" for step in steps) == 1,
          "唯一失败必须是 Enforce remote machine proof")
    index = steps.index(failed[0])
    check(all(step.get("status") == "completed" and
              (step.get("conclusion") == "success" or position > index and step.get("conclusion") == "skipped")
              for position, step in enumerate(steps) if position != index), "verdict 还有其他失败或未完成步骤")
    check(core.timestamp(verdict.get("started_at")) <= core.timestamp(failed[0].get("started_at"))
          <= core.timestamp(failed[0].get("completed_at")) <= core.timestamp(verdict.get("completed_at")), "失败步骤不在本次 verdict 窗口内")
    materials, decision, artifact = staged_materials(api, intent, root)
    check(decision["status"] == "no_host_anchor" and materials["machine_signature"] == ""
          and proof.public_key(root, intent["workflow_sha"]) is None, "已有锚点、错误签名或来源未知不能走本机无锚点复核")
    check(api.run(run_id) == latest, "核验期间失败 owner 或 attempt 已改变")
    return intent, materials, decision, artifact


def publication_file(args, intent, expected_hash):
    check(core.sha(getattr(args, "expected_sha", None)) == intent["sha"], "用户选择的完整 SHA 与本次候选不同")
    selected = getattr(args, "artifact", None)
    check(selected is not None, "最终复核缺少 --artifact 实际待发布文件")
    selected = Path(selected)
    check(not selected.is_symlink() and selected.is_file(), "待发布实际文件缺失或是符号链接")
    path = selected.resolve(strict=True)
    actual = evidence.binary_identity(args.go, path, intent, machine.native_environment())
    machine.identity(actual, args.expected_sha, expected_hash)
    return {"path": str(path), "sha": args.expected_sha, "sha256": actual["sha256"]}


def staged_materials(api, intent, root):
    materials, decision, artifact = proof.read_materials(api, intent)
    check(materials == evidence.joined_evidence(api, intent, root), "暂存材料与独立原生 Full、构建、intake 复核不符")
    check(decision == proof.assess(materials, root, intent["workflow_sha"]), "归档机器判定与不可变源码或签名不符")
    return materials, decision, artifact


def finalize(args, api):
    core.sha(getattr(args, "expected_sha", None))
    current = api.run(args.run_id)
    core.native_candidate(current, api.repo)
    check(current.get("run_attempt") == args.attempt and current.get("status") == "completed"
          and current.get("conclusion") in ("success", "failure"), "最终复核拒绝未完成、取消或过期 attempt")
    root = control_root()
    signed = current["conclusion"] == "success"
    if signed:
        intent = control.guard(api, args.run_id, args.attempt, completed=True)
        check(args.expected_sha == intent["sha"], "用户选择的 SHA 不能切换候选目标")
        value = prepare_submission(args, api, completed=True)
        materials, decision, staged_artifact = staged_materials(api, intent, root)
        check(decision["status"] == "verified", "远端没有真正验证机器见证")
        producer = core.require_jobs(api.jobs(args.run_id, args.attempt), args.run_id, args.attempt, [evidence.VERDICT_JOB])[0]
        artifact = api.artifact(args.run_id, evidence.verdict_name(intent), producer)
        raw = api.download(artifact, {"evidence.json"}, proof.MAX_MATERIALS + core.MAX_RECEIPT)["evidence.json"]
        result = core.decode(raw, proof.MAX_MATERIALS + core.MAX_RECEIPT)
        proof.validate_evidence(result, root, intent["workflow_sha"])
        check(result == materials | {"kind": "release-evidence", "status": "success", "verified": True, "authoritative": True,
              "publishable": False, "verification": "openssh-machine-witness-v1", "host_proof": decision,
              "materials": proof._fact(materials, decision, staged_artifact)}, "hosted verdict 与认证材料不符")
        remote = {"hosted_verdict": evidence.artifact_fact(artifact, core.hashed(raw)), "remote_outcome": "verified"}
    else:
        intent, materials, decision, staged_artifact = no_anchor_intent(api, args.run_id, args.attempt, root)
        check(args.expected_sha == intent["sha"], "用户选择的 SHA 不能切换候选目标")
        value = prepare_local(args, api, intent)
        result = materials | {"kind": "release-evidence", "status": "success", "verified": True, "authoritative": True,
            "publishable": False, "host_proof": decision, "materials": proof._fact(materials, decision, staged_artifact)}
        remote = {"remote_outcome": "no_host_anchor"}
    check(value.get("status") != "waiting", "最终复核仍缺少私有原生证据或 Full")
    check(result["machine_receipt"] == value, "暂存材料与本机原始字节复核不符")
    selected = publication_file(args, intent, result["artifact_sha256"])
    if signed:
        check(control.guard(api, args.run_id, args.attempt, completed=True) == intent, "最终复核期间 canonical attempt 改变")
    else:
        check(no_anchor_intent(api, args.run_id, args.attempt, root) == (intent, materials, decision, staged_artifact),
              "最终复核期间无锚点终态或证据改变")
    check(core.file_hash(Path(selected["path"])) == selected["sha256"], "最终复核期间待发布文件改变")
    final = result | remote | {"verification": "local-native-full-reconciliation-v1", "publication_artifact": selected,
                      "native_receipt_sha256": core.hashed(core.encoded(value)), "source_bundle_sha256": value["source_bundle_sha256"],
                      "unknown_guard": value["unknown_guard"], "machine_id": value["machine_id"], "sources": value["sources"]}
    claim = machine.locate_claim(args.work_dir, api.repo, args.run_id, args.attempt)
    reconcile_file(claim / "release-evidence.json", final)
    return final
