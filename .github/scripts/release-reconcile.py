#!/usr/bin/env python3
"""Recover an exact machine lease from local original evidence; never execute native work."""
import argparse
from contextlib import closing, contextmanager, ExitStack
import fcntl
import importlib.util
import json
import os
from pathlib import Path
import re
import shutil
import sqlite3
import stat
import subprocess
import sys
import tempfile
from types import SimpleNamespace

EXTERNAL = frozenset(("6c-prepare", "6a-execute", "6c-execute", "6b-prepare", "6b-execute"))
STAGES = EXTERNAL | {"reserved", "record-inputs", "install-binary", "binary-before", "clean-snapshot", "seed-helper",
                     "agy-binding", "6c-audit", "unknown-race", "binary-after", "normalize", "complete"}
DATABASES = ("acceptance-6a-release/data", "acceptance-6b-release/data",
             "acceptance-6c-release/acceptance-000/data", "acceptance-6c-release/acceptance-200/data")


class Unproven(ValueError):
    """Only fixed, sanitized descriptions of missing original proof belong here."""


class Busy(ValueError):
    pass


def check(value, message):
    if not value:
        raise Unproven(message)


def identity(repo, run_id, attempt):
    check(isinstance(repo, str) and re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", repo)
          and all(type(value) is int and 0 < value < 2 ** 63 for value in (run_id, attempt)), "仓库、run 或 attempt 非法")
    return {"repo": repo, "run_id": run_id, "run_attempt": attempt}


def exact_owner(value, expected):
    return isinstance(value, dict) and set(value) == set(expected) and all(
        type(value[key]) is type(item) and value[key] == item for key, item in expected.items())


def unique(pairs):
    value = dict(pairs)
    check(len(value) == len(pairs), "本机记录有重复 JSON 字段")
    return value


def private_file(descriptor):
    info = os.fstat(descriptor)
    check(stat.S_ISREG(info.st_mode) and info.st_uid == os.getuid() and stat.S_IMODE(info.st_mode) == 0o600
          and info.st_nlink == 1, "本机记录或执行锁的所有者、权限、类型不符")


def read(path):
    with os.fdopen(os.open(path, os.O_RDONLY | os.O_NOFOLLOW), "rb") as stream:
        private_file(stream.fileno())
        data = stream.read((1 << 20) + 1)
    check(len(data) <= 1 << 20, "本机记录过大")
    return json.loads(data, object_pairs_hook=unique)


def sync_directory(path):
    descriptor = os.open(path, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        os.fsync(descriptor)
    finally:
        os.close(descriptor)


def save(path, value, *, replace=False):
    # Publish only complete, fsynced bytes. A failed publication cannot authorize a command.
    with tempfile.NamedTemporaryFile(dir=path.parent, prefix=".lease-write-", delete=False) as stream:
        temporary = Path(stream.name)
        stream.write((json.dumps(value, sort_keys=True, separators=(",", ":")) + "\n").encode())
        stream.flush()
        os.fsync(stream.fileno())
    try:
        if replace:
            os.replace(temporary, path)
        else:
            try:
                os.link(temporary, path, follow_symlinks=False)
            except FileExistsError:
                check(read(path) == value, "已有本机诊断或结清记录冲突")
    finally:
        temporary.unlink(missing_ok=True)
    sync_directory(path.parent)


@contextmanager
def flock(path, *, create=False, wait=False):
    descriptor = os.open(path, os.O_NOFOLLOW | (os.O_RDWR | os.O_CREAT if create else os.O_RDONLY), 0o600)
    try:
        private_file(descriptor)
        os.set_inheritable(descriptor, False)
        try:
            fcntl.flock(descriptor, fcntl.LOCK_EX | (0 if wait else fcntl.LOCK_NB))
        except BlockingIOError:
            raise Busy("本机登记或原始 worker 仍运行") from None
        yield descriptor
    finally:
        os.close(descriptor)


@contextmanager
def mutex(machine, *, wait=False):
    check(machine.STATE_ROOT == machine.host.registry_root(), "不能切换固定用户机器登记目录")
    machine.private_directory(machine.STATE_ROOT)
    with flock(machine.STATE_ROOT / "lease-mutation.lock", create=True, wait=wait):
        yield


def registered(machine, owner):
    permanent = machine.claim_path(machine.STATE_ROOT, owner["repo"], owner["run_id"], owner["run_attempt"])
    machine.private_directory(permanent)
    value = read(permanent / "claim.json")
    check(isinstance(value, dict) and set(value) == set(owner) | {"claim"}
          and all(type(value[key]) is type(item) and value[key] == item for key, item in owner.items()), "固定认领身份不符")
    claim = Path(value["claim"])
    check(claim.is_absolute() and claim == claim.resolve(strict=True)
          and claim == machine.claim_path(claim.parent.parent, owner["repo"], owner["run_id"], owner["run_attempt"]), "固定认领路径不符")
    machine.authorized_host(claim)
    check(exact_owner(read(claim / "reserved.json"), owner), "原始 reserved.json 认领身份不符")
    return claim, permanent


def sync_reservation(machine, claim):
    bound, permanent = registered(machine, read(claim / "reserved.json"))
    check(bound == claim, "不能同步其他认领")
    active = machine.STATE_ROOT / "machine-active"
    for path in (claim / "reserved.json", permanent / "claim.json", active / "owner.json"):
        with os.fdopen(os.open(path, os.O_RDWR | os.O_NOFOLLOW), "rb") as stream:
            private_file(stream.fileno())
            os.fsync(stream.fileno())
    for path in (claim, claim.parent, claim.parent.parent, permanent, permanent.parent, active, machine.STATE_ROOT):
        sync_directory(path)


class Lease:
    def __init__(self, machine, owner, claim, args, *, intent=None, worker=None):
        self.machine, self.owner, self.claim, self.args = machine, owner, claim, args
        bound, self.permanent = registered(machine, owner)
        check(bound == claim, "worker 不属于固定认领")
        self.active = machine.STATE_ROOT / "machine-active"
        self.archive = self.permanent / "released-machine-active"
        prep_path = claim / "lease-prep.json"
        if intent is not None:
            machine.verify_control_files(intent)
            machine.private_directory(self.active)
            check(exact_owner(read(self.active / "owner.json"), owner), "不能记录其他 owner 的准备阶段")
            info, worker_info = self.active.stat(), os.fstat(worker)
            save(prep_path, owner | {"schema": 1, "kind": "release-native-prep", "claim": str(claim),
                                    "active": [info.st_dev, info.st_ino], "worker": [worker_info.st_dev, worker_info.st_ino], "intent": intent})
        self.prep = read(prep_path)
        check(isinstance(self.prep, dict) and set(self.prep) == set(owner) | {"schema", "kind", "claim", "active", "worker", "intent"}
              and type(self.prep["schema"]) is int and self.prep["schema"] == 1 and self.prep["kind"] == "release-native-prep"
              and all(type(self.prep[key]) is type(item) and self.prep[key] == item for key, item in owner.items())
              and self.prep["claim"] == str(claim), "lease-prep.json 身份或格式不符")
        worker_info = (claim / "worker.lock").stat() if worker is None else os.fstat(worker)
        check(self.prep["worker"] == [worker_info.st_dev, worker_info.st_ino], "worker.lock inode 改变；不能推断原始 worker 已死亡")
        self.intent = self.prep["intent"]
        check(isinstance(self.intent, dict) and all(type(self.intent.get(key)) is type(item) and self.intent.get(key) == item
                                                   for key, item in owner.items()), "准备阶段的控制 intent 身份不符")
        machine.verify_control_files(self.intent)
        self.base = owner | {"schema": 1, "prep_sha256": machine.core.file_hash(prep_path)}
        self.settled = False

    def event(self, name):
        value = read(self.claim / name)
        check(isinstance(value, dict) and all(type(value.get(key)) is type(item) and value.get(key) == item
                                             for key, item in self.base.items()), "阶段或结清记录未绑定原始准备身份")
        return value

    def stage(self, name):
        check(name in STAGES and not self.settled, "阶段非法或本次已结清，禁止重放")
        save(self.claim / "lease-stage.json", self.base | {"stage": name}, replace=True)

    def stage_name(self):
        name = self.event("lease-stage.json")["stage"] if (self.claim / "lease-stage.json").exists() else "reserved"
        check(name in STAGES, "原始准备阶段记录不符")
        return name

    def external(self, phase):
        check(phase in EXTERNAL and not self.settled, "外部阶段非法或本次已结清")
        path = self.claim / ("lease-external-" + phase + ".json")
        check(not path.exists(), "同一外部阶段禁止重放")
        self.stage(phase)
        save(path, self.base | {"stage": phase})

    def phases(self):
        result = set()
        for path in self.claim.glob("lease-external-*.json"):
            phase = self.event(path.name).get("stage")
            check(phase in EXTERNAL and path.name == "lease-external-" + phase + ".json", "外部阶段标记不完整或来源未知")
            result.add(phase)
        return result

    def archived(self):
        if not self.archive.exists():
            return False
        self.machine.private_directory(self.archive)
        check(exact_owner(read(self.archive / "owner.json"), self.owner)
              and [self.archive.stat().st_dev, self.archive.stat().st_ino] == self.prep["active"], "已归档机器锁身份不符")
        self.event("lease-settlement.json")
        return True

    def settle_locked(self, kind, proof):
        # The registry mutex excludes every new reservation during verify-owner + rename.
        self.machine.private_directory(self.active)
        check(exact_owner(read(self.active / "owner.json"), self.owner)
              and [self.active.stat().st_dev, self.active.stat().st_ino] == self.prep["active"], "不能释放其他或未知 owner 的机器锁")
        path = self.claim / "lease-settlement.json"
        if path.exists():
            previous = self.event(path.name)
            prep_kinds = {"pre-native-failed", "pre-native-stopped", "pre-native-dead"}
            check((previous.get("kind") == kind or previous.get("kind") in prep_kinds and kind in prep_kinds)
                  and previous.get("stage") == self.stage_name() and previous.get("proof") == proof, "已有结清事实与本次原始证据不符")
        else:
            save(path, self.base | {"kind": kind, "stage": self.stage_name(), "proof": proof})
        check(not self.archive.exists(), "不能覆盖已有机器锁归档")
        os.rename(self.active, self.archive)
        # Keep the exact old owner directory, avoiding unlink-owner/rmdir crash windows.
        sync_directory(self.permanent)
        sync_directory(self.machine.STATE_ROOT)
        self.settled = True

    def complete_proof(self):
        check((self.claim / "complete.json").is_file(), "complete.json：缺少完整原始 native 完成回执，不能推断外部终止")
        check(self.phases() == EXTERNAL, "外部阶段登记不完整，无法绑定完整 native 终态")
        receipt = read(self.claim / "receipt.json")
        digest = self.machine.core.file_hash(self.claim / "receipt.json")
        check(read(self.claim / "complete.json") == {"receipt_sha256": digest}, "complete.json：原始回执摘要不符")
        finalizer = self.machine.core.load_script("release-finalize")
        try:
            actual = finalizer.verify_local(self.args, None, self.intent, self.claim,
                {"artifact_sha256": receipt["artifact_sha256"]}, receipt["target_artifact"])
        except (ValueError, KeyError, OSError, subprocess.SubprocessError, sqlite3.Error):
            raise Unproven("finalizer.verify_local：缺少通过原始源码、二进制、6A/B/C 协议与审计、unknown guard 的独立复核") from None
        check(isinstance(actual, dict) and actual == receipt, "原始 finalizer 没有返回匹配的完整 native 回执")
        databases = {}
        for relative in DATABASES:
            missing = relative + "/workspace.db：必须无活跃或 unknown Turn、无资源 claim，且原生终态已结清"
            try:
                database = finalizer.raw_file(self.claim, self.claim / relative / "workspace.db", 512 << 20)
                lock = finalizer.raw_file(self.claim, self.claim / relative / "workspace.lock")
                with lock.open("rb") as stream, closing(sqlite3.connect(database.as_uri() + "?mode=ro", uri=True)) as db:
                    fcntl.flock(stream, fcntl.LOCK_SH | fcntl.LOCK_NB)
                    db.execute("PRAGMA query_only=ON")
                    db.execute("BEGIN")
                    check(db.execute("PRAGMA quick_check").fetchall() == [("ok",)]
                          and not db.execute("PRAGMA foreign_key_check").fetchall()
                          and db.execute("SELECT COUNT(*) FROM resource_claims").fetchone()[0] == 0, missing)
                    turns = db.execute("SELECT state,native_session_id,receipt_json FROM turns").fetchmany(1001)
                    check(0 < len(turns) <= 1000, missing)
                    for state, session, raw in turns:
                        check(state in ("succeeded", "failed", "aborted") and isinstance(session, str) and bool(session)
                              and isinstance(raw, str) and len(raw) <= 1 << 20, missing)
                        terminal = json.loads(raw, object_pairs_hook=unique)
                        check(isinstance(terminal, dict), missing)
                        self.machine.terminal(terminal, session, {state})
                    databases[relative] = self.machine.core.file_hash(database)
            except (ValueError, KeyError, OSError, sqlite3.Error):
                raise Unproven(missing) from None
        check(self.machine.core.file_hash(self.claim / "receipt.json") == digest, "复核期间原始完成回执改变")
        return {"receipt_sha256": digest, "databases": databases}

    def complete(self):
        with mutex(self.machine, wait=True):
            self.settle_locked("native-complete", self.complete_proof())


@contextmanager
def execution(machine, args, intent):
    """The long-lived worker flock is never inherited by drivers or services."""
    machine = SimpleNamespace(**machine) if isinstance(machine, dict) else machine
    owner = identity(intent["repo"], args.run_id, args.attempt)
    check(all(type(intent.get(key)) is type(item) and intent.get(key) == item for key, item in owner.items()), "准备 intent 与本次参数不符")
    machine.verify_control_files(intent)
    machine.authorized_host(args.work_dir)
    with ExitStack() as stack:
        with mutex(machine):
            claim = machine.reserve(args.work_dir, owner["repo"], owner["run_id"], owner["run_attempt"])
            sync_reservation(machine, claim)
            worker = stack.enter_context(flock(claim / "worker.lock", create=True))
            os.fsync(worker)
            lease = Lease(machine, owner, claim, args, intent=intent, worker=worker)
            lease.stage("record-inputs")
        try:
            yield lease
        except Exception as error:
            external = lease.phases()
            save(claim / "lease-failure.json", lease.base | {"stage": lease.stage_name(),
                 "category": "external-unknown" if external else "pre-native", "exception": type(error).__name__})
            if not external:
                with mutex(machine, wait=True):
                    lease.settle_locked("pre-native-failed", {"external_phases": []})
            raise
        else:
            if not lease.settled and not lease.phases():
                with mutex(machine, wait=True):
                    lease.settle_locked("pre-native-stopped", {"external_phases": []})


def reconcile(machine, repo, run_id, attempt, *, go):
    owner = identity(repo, run_id, attempt)
    try:
        check(machine.STATE_ROOT == machine.host.registry_root(), "不能切换固定用户机器登记目录")
        claim, _ = registered(machine, owner)
        if (claim / "worker.lock").is_file():
            with flock(claim / "worker.lock"):
                pass  # A live initializing worker is busy even before prep publication.
        check((claim / "lease-prep.json").is_file() and (claim / "worker.lock").is_file(),
              "lease-prep.json/worker.lock 缺失：reserve 与 worker 附着间的崩溃窗口不明确，禁止释放")
        args = SimpleNamespace(work_dir=claim.parent.parent, run_id=run_id, attempt=attempt, go=go, harness_root=None)
        # Authenticate immutable control bytes before opening any writable registry file.
        Lease(machine, owner, claim, args)
        with mutex(machine):
            with flock(claim / "worker.lock") as worker:
                lease = Lease(machine, owner, claim, args, worker=worker)
                if not lease.archived():
                    phases = lease.phases()
                    if phases:
                        lease.settle_locked("native-complete", lease.complete_proof())
                    else:
                        lease.settle_locked("pre-native-dead", {"external_phases": []})
        return owner | {"status": "settled"}
    except Busy:
        return owner | {"status": "busy"}
    except Unproven as error:
        return owner | {"status": "unknown", "missing": [str(error)]}
    except (ValueError, KeyError, OSError, TypeError, sqlite3.Error, subprocess.SubprocessError):
        return owner | {"status": "unknown", "missing": ["固定 registry/claim/owner 或原始私有记录缺失、被更改或不可验证"]}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("reconcile",))
    parser.add_argument("--repo", required=True)
    parser.add_argument("--run-id", type=int, required=True)
    parser.add_argument("--attempt", type=int, required=True)
    parser.add_argument("--go", type=Path, default=Path(shutil.which("go") or "go"))
    args = parser.parse_args()
    check(sys.flags.optimize == 0, "原始验收依赖 assertions，禁止优化模式")
    os.umask(0o077)
    spec = importlib.util.spec_from_file_location("release_core", Path(__file__).with_name("release-core.py"))
    core = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(core)
    result = reconcile(core.load_script("release-machine"), args.repo, args.run_id, args.attempt, go=args.go)
    print(json.dumps(result, ensure_ascii=False, sort_keys=True))
    return {"settled": 0, "busy": 75, "unknown": 1}[result["status"]]


if __name__ == "__main__":
    try:
        sys.exit(main())
    except (ValueError, KeyError, OSError, TypeError, sqlite3.Error, subprocess.SubprocessError):
        print("机器认领未结清；原始身份或终态不可验证，保留未知状态，禁止强制解锁。", file=sys.stderr)
        sys.exit(1)
