#!/usr/bin/env python3
"""Operator-bound private harness and account-stable registry; no native execution."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import pwd
import re
import stat
import tempfile

REQUIRED = frozenset(("driver_6a driver_6b observer_6b audit_6b facts_6b prepare_6b seed_6b "
    "driver_6c audit_6c seed_6c legacy_6a runner_6b verifier_6b export_fixture native_exact "
    "native_preliminary native_project_stage normal_entry compare_first_turn").split())
SUPPORT = frozenset("six_b six_c six_b_http six_a_original native_transcript".split())


def _check(value, message="本机绑定字段、权限、路径或原始源码不符"):
    if not value:
        raise ValueError(message)

def _keys(value, names):
    _check(isinstance(value, dict) and set(value) == set(names))


def registry_root():
    return Path(pwd.getpwuid(os.getuid()).pw_dir) / "Library/Application Support/agent-workspace/release-machine"


def _path(value):
    _check(isinstance(value, (str, Path)))
    path = Path(value)
    _check(path.anchor == "/" and ".." not in path.parts
           and not any(part.is_symlink() for part in (path, *path.parents)))
    _check(path.exists())
    return path

def _private(path, directory=False, info=None):
    path = _path(path)
    info = info if info is not None else path.stat()
    _check(info.st_uid == os.getuid() and stat.S_IMODE(info.st_mode) == (0o700 if directory else 0o600)
           and (stat.S_ISDIR(info.st_mode) if directory else stat.S_ISREG(info.st_mode) and info.st_nlink == 1))
    return path


def _registry(create=False):
    root = registry_root()
    home = _path(pwd.getpwuid(os.getuid()).pw_dir)
    _check(home.stat().st_uid == os.getuid() and not home.stat().st_mode & 0o022)
    current = home
    for component in root.relative_to(home).parts:
        current /= component
        if create:
            current.mkdir(mode=0o700, exist_ok=True)
        info = _path(current).stat()
        _check(stat.S_ISDIR(info.st_mode) and info.st_uid == os.getuid() and not info.st_mode & 0o022)
    return _private(root, True)

def _unique(pairs):
    value = dict(pairs)
    _check(len(value) == len(pairs))
    return value

def _read(path):
    with os.fdopen(os.open(_private(path), os.O_RDONLY | os.O_NOFOLLOW), "rb") as stream:
        _private(path, info=os.fstat(stream.fileno()))
        data = stream.read(65537)
    _check(len(data) <= 65536)
    return json.loads(data, object_pairs_hook=_unique)

def _hash(path):
    # Callers have already checked every path component and the configured volume.
    with os.fdopen(os.open(path, os.O_RDONLY | os.O_NOFOLLOW), "rb") as stream:
        info = os.fstat(stream.fileno())
        _check(stat.S_ISREG(info.st_mode) and info.st_uid == os.getuid() and info.st_size <= 8 << 20)
        return hashlib.file_digest(stream, "sha256").hexdigest()

def _scope(path, config):
    path = _path(path)
    _check(path.is_relative_to(Path(config["hdd"]["path"])) and path.stat().st_dev == config["hdd"]["device"])
    return path

def _validate(value):
    _keys(value, ("schema", "hdd", "harness", "sources", "support"))
    _check(type(value["schema"]) is int and value["schema"] == 1)
    _keys(value["hdd"], ("path", "device", "inode"))
    volume = _path(value["hdd"]["path"])
    info = volume.stat()
    _check(volume.is_dir() and all(type(value["hdd"][key]) is int for key in ("device", "inode"))
           and (info.st_dev, info.st_ino) == (value["hdd"]["device"], value["hdd"]["inode"]))
    _check(_scope(value["harness"], value).is_dir())
    _keys(value["sources"], REQUIRED)
    _keys(value["support"], SUPPORT)
    for row in value["sources"].values():
        _keys(row, ("path", "sha256"))
        _check(isinstance(row["sha256"], str) and re.fullmatch(r"[0-9a-f]{64}", row["sha256"])
               and _hash(_scope(row["path"], value)) == row["sha256"])
    for row in value["support"].values():
        _keys(row, ("path", "files"))
        directory = _scope(row["path"], value)
        actual = {path.name for path in directory.iterdir() if path.suffix in (".py", ".go")}
        _check(isinstance(row["files"], dict) and 0 < len(actual) <= 128 and actual == set(row["files"]))
        for name, expected in row["files"].items():
            _check(re.fullmatch(r"[A-Za-z0-9_.-]+\.(py|go)", name)
                   and _hash(_scope(directory / name, value)) == expected)
    return value

def load_binding():
    return _validate(_read(_registry() / "binding.json"))


def bind(manifest):
    value = _validate(_read(manifest))
    root = _registry(create=True)
    # Publish complete bytes once without replacing a competing or previous binding.
    with tempfile.NamedTemporaryFile(dir=root, delete=False) as stream:
        temporary = Path(stream.name)
        stream.write((json.dumps(value, sort_keys=True, separators=(",", ":")) + "\n").encode())
        stream.flush()
        os.fsync(stream.fileno())
    try:
        try:
            os.link(temporary, root / "binding.json", follow_symlinks=False)
        except FileExistsError:
            pass
    finally:
        temporary.unlink()
    descriptor = os.open(root, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        os.fsync(descriptor)
    finally:
        os.close(descriptor)
    _check(load_binding() == value, "已有主机绑定不同；只能先离线对账恢复，禁止切换安装或注册表")
    return value


def validate_scope(path):
    return _private(_scope(path, load_binding()), True)


def source_manifest(harness):
    value = load_binding()
    _check(_path(harness) == Path(value["harness"]))
    paths = {name: Path(row["path"]) for name, row in value["sources"].items()}
    public = {name: row["sha256"] for name, row in value["sources"].items()}
    bundle = public | {f"support:{group}/{name}": digest for group, row in value["support"].items()
                       for name, digest in row["files"].items()}
    return paths, public, bundle


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("bind",))
    parser.add_argument("--manifest", type=Path, required=True)
    try:
        bind(parser.parse_args().manifest)
        print("主机原始安装已绑定并校验；重用固定用户注册表。")
    except (ValueError, OSError, TypeError):
        parser.exit(1, "主机绑定未完成；保留既有状态，先离线核对本机安装与权限。\n")
