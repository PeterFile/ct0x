#!/usr/bin/env python3
"""Offline SSHSIG machine witness assessment and guarded native verdict enforcement."""
import argparse
import base64
import importlib.util
import os
from pathlib import Path
import subprocess
import sys
import tempfile

_spec = importlib.util.spec_from_file_location("release_core", Path(__file__).with_name("release-core.py"))
core = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(core)
control, evidence = core.load_script("release-control"), core.load_script("release-evidence")
check = core.check
KEY_PATH = ".github/release-machine.pub"
NAMESPACE = "agent-workspace-release-machine-v1"
MAX_SIGNATURE = 8192
MAX_MATERIALS = 4 * core.MAX_RECEIPT
SCOPE = ("repo", "sha", "tree", "run_id", "run_attempt", "workflow_sha")
MATERIAL_FIELDS = {"schema", "kind", "status", "verified", "authoritative", "publishable", *SCOPE, "artifact_sha256", "target", "full",
                   "machine", "intake", "full_lanes", "machine_lanes", "machine_receipt", "machine_signature"}


def _scope(value, expected):
    core.scope(value, expected["repo"], expected["sha"], expected["tree"], expected["run_id"], expected["run_attempt"])
    check(type(value.get("schema")) is int, "证据 schema 非法")


def _entry(root, sha, path):
    row = core.git(root, "ls-tree", "-z", core.sha(sha), "--", path)
    if not row:
        return None
    records = row.split("\0")
    check(len(records) == 2 and records[1] == "", "公钥 Git tree 条目不符")
    metadata, name = records[0].split("\t")
    mode, kind, blob = metadata.split(" ")
    check(name == path, "公钥路径越过允许范围")
    return mode, kind, core.sha(blob)


def public_key(control_root, workflow_sha):
    """Only an absent entry in a valid immutable Git tree means unconfigured."""
    root = Path(control_root).resolve(strict=True)
    check(Path(core.git(root, "rev-parse", "--show-toplevel")).resolve() == root, "控制路径不是仓库根目录")
    check(core.git(root, "cat-file", "-t", core.sha(workflow_sha)) == "commit", "控制 SHA 不是确切提交")
    directory = _entry(root, workflow_sha, ".github")
    if directory is None:
        return None
    check(directory[:2] == ("040000", "tree"), "公钥目录不是普通 Git tree")
    row = _entry(root, workflow_sha, KEY_PATH)
    if row is None:
        return None
    check(row[0] in ("100644", "100755") and row[1] == "blob", "公钥不是普通 Git blob")
    size = core.git(root, "cat-file", "-s", row[2])
    check(size.isdecimal() and 0 < int(size) <= 1024, "公钥 blob 长度非法")
    text = core.git(root, "cat-file", "blob", row[2])
    check(text.isascii() and len(text.splitlines()) == 1, "只允许一个 ASCII 公钥")
    fields = text.split(maxsplit=2)
    check(len(fields) >= 2 and fields[0] == "ssh-ed25519", "只允许一个 ed25519 公钥")
    blob = base64.b64decode(fields[1], validate=True)
    check(blob.startswith(b"\0\0\0\x0bssh-ed25519\0\0\0\x20") and len(blob) == 51
          and base64.b64encode(blob).decode() == fields[1], "ed25519 公钥内容非法")
    return fields[0] + " " + fields[1]


def _signature(value):
    check(isinstance(value, str) and value.isascii() and 0 < len(value) <= MAX_SIGNATURE, "机器签名长度或字符非法")
    raw = base64.b64decode(value, validate=True)
    check(base64.b64encode(raw).decode() == value and raw.startswith(b"-----BEGIN SSH SIGNATURE-----\n")
          and raw.endswith(b"-----END SSH SIGNATURE-----\n"), "机器 SSHSIG 格式非法")
    return raw


def _crypto(machine, key, signature=None):
    with tempfile.TemporaryDirectory(prefix="release-sshsig-") as temporary:
        root = Path(temporary)
        env = {"PATH": "/usr/bin:/bin", "HOME": temporary, "LC_ALL": "C", "SSH_ASKPASS_REQUIRE": "never"}
        if signature is None:
            check(bool(os.environ.get("SSH_AUTH_SOCK")), "已配置公钥，但没有可用的签名 agent")
            env["SSH_AUTH_SOCK"] = os.environ["SSH_AUTH_SOCK"]
            public = root / "key.pub"
            public.write_text(key + "\n", encoding="ascii")
            args = ["-Y", "sign", "-U", "-f", str(public), "-n", NAMESPACE]
        else:
            allowed, signed = root / "allowed", root / "signature"
            allowed.write_text(f'release-machine namespaces="{NAMESPACE}" {key}\n', encoding="ascii")
            signed.write_bytes(_signature(signature))
            args = ["-Y", "verify", "-f", str(allowed), "-I", "release-machine", "-n", NAMESPACE, "-s", str(signed)]
        result = subprocess.run(["/usr/bin/ssh-keygen", *args], input=core.encoded(machine), env=env,
                                capture_output=True, timeout=15)
        check(result.returncode == 0, "远端机器签名缺失、签名者不可用或验签失败")
        return result.stdout


def _machine(machine, workflow_sha):
    _scope(machine, machine)
    check(machine.get("workflow_sha") == core.sha(workflow_sha), "机器回执不属于受信任控制 SHA")
    evidence.validate_machine(machine, require_full=True)
    binding = machine["full_binding"]
    _scope(binding, machine)
    check(binding.get("kind") == "release-full-binding" and binding.get("workflow_sha") == workflow_sha
          and binding.get("producer") == "Release Full evidence" and binding.get("artifact_sha256") == machine["artifact_sha256"],
          "机器回执未绑定同次 Full 和二进制")
    for lane in machine["lanes"]:
        _scope(lane, machine)
        check(lane.get("artifact_sha256") == machine["artifact_sha256"]
              and lane.get("full_binding_sha256") == core.hashed(core.encoded(binding)), "原生场景身份或 Full 摘要不符")


def sign_payload(machine_receipt, control_root, workflow_sha):
    """Finalizer-only API: call after original private re-audit and mandatory Full join."""
    _machine(machine_receipt, workflow_sha)
    key = public_key(control_root, workflow_sha)
    if key is None:
        return ""
    signature = base64.b64encode(_crypto(machine_receipt, key)).decode("ascii")
    _signature(signature)
    _crypto(machine_receipt, key, signature)
    return signature


def _materials(value, workflow_sha):
    check(isinstance(value, dict) and set(value) == MATERIAL_FIELDS and len(core.encoded(value)) <= MAX_MATERIALS,
          "候选材料字段或大小不符")
    _scope(value, value)
    check(value["kind"] == "release-materials" and value["status"] == "collected" and value["verified"] is False
          and value["authoritative"] is False and value["publishable"] is False
          and value["workflow_sha"] == core.sha(workflow_sha), "候选材料不是受信任的暂存输入")
    machine = value["machine_receipt"]
    _machine(machine, workflow_sha)
    _scope(machine, value)
    core.digest(value["artifact_sha256"])
    check(machine["artifact_sha256"] == value["artifact_sha256"] and machine["target_artifact"] == value["target"]
          and machine["full_binding"] == value["full"] and machine["lanes"] == value["machine_lanes"], "材料与原始机器回执不符")
    for field in ("target", "machine", "intake"):
        fact = value[field]
        check(set(fact) == {"id", "digest", "receipt_sha256"}, "材料制品字段不符")
        core.number(fact["id"])
        core.exact(fact["digest"], r"sha256:[0-9a-f]{64}", "材料制品摘要非法")
        core.digest(fact["receipt_sha256"])
    check(isinstance(value["full_lanes"], list), "Full 场景字段非法")


def assess(materials, control_root, workflow_sha):
    """Never classify a Git/process error or an unverifiable signature as no anchor."""
    _scope(materials, materials)
    core.sha(workflow_sha)
    decision = {key: materials[key] for key in SCOPE} | {
        "schema": 1, "kind": "release-host-proof", "workflow_sha": workflow_sha,
        "materials_sha256": core.hashed(core.encoded(materials)), "status": "rejected"}
    try:
        _materials(materials, workflow_sha)
        key = public_key(control_root, workflow_sha)
        signature = materials["machine_signature"]
        check(isinstance(signature, str) and signature.isascii() and len(signature) <= MAX_SIGNATURE, "机器签名非法")
        if key is None:
            check(signature == "", "未配置锚点但提交了无法核实的签名")
            decision["status"] = "no_host_anchor"
        else:
            _crypto(materials["machine_receipt"], key, signature)
            decision.update(status="verified", signer_sha256=core.hashed(base64.b64decode(key.split()[1], validate=True)))
    except (ValueError, OSError, KeyError, TypeError, UnicodeError, subprocess.SubprocessError):
        pass
    return decision


def read_materials(api, intent):
    run_id, attempt = intent["run_id"], intent["run_attempt"]
    producer = core.require_jobs(api.jobs(run_id, attempt), run_id, attempt, ["Release candidate evidence"])[0]
    artifact = api.artifact(run_id, f"release-materials-{run_id}-{attempt}", producer)
    files = api.download(artifact, {"receipt.json", "decision.json"}, MAX_MATERIALS + 4096)
    value, decision = core.decode(files["receipt.json"], MAX_MATERIALS), core.decode(files["decision.json"], 4096)
    check(files["receipt.json"] == core.encoded(value) and files["decision.json"] == core.encoded(decision), "暂存回执不是规范原始字节")
    _scope(value, intent)
    _materials(value, intent["workflow_sha"])
    _scope(decision, intent)
    fields = {"schema", "kind", *SCOPE, "materials_sha256", "status"}
    check(decision.get("kind") == "release-host-proof" and decision.get("workflow_sha") == intent["workflow_sha"]
          and decision.get("materials_sha256") == core.hashed(core.encoded(value))
          and decision.get("status") in ("verified", "no_host_anchor", "rejected")
          and set(decision) == (fields | {"signer_sha256"} if decision["status"] == "verified" else fields), "暂存判定身份或字段不符")
    return value, decision, artifact


def _fact(materials, decision, artifact):
    return evidence.artifact_fact(artifact, core.hashed(core.encoded(materials))) | {
        "decision_sha256": core.hashed(core.encoded(decision))}


def enforce(api, intent, control_root):
    check(control.guard(api, intent["run_id"], intent["run_attempt"]) == intent, "当前 owner 来源已改变")
    materials, decision, artifact = read_materials(api, intent)
    check(decision == assess(materials, control_root, intent["workflow_sha"]) and decision["status"] == "verified",
          "远端机器证明未验证；不得产生最终发布证据")
    build, _, target = evidence.target(api, intent)
    check(build["artifact_sha256"] == materials["artifact_sha256"] and target == materials["target"], "候选实际字节不符")
    check(control.guard(api, intent["run_id"], intent["run_attempt"]) == intent, "核验期间 owner 已改变")
    return materials | {"kind": "release-evidence", "status": "success", "verified": True, "authoritative": True,
                        "publishable": False, "verification": "openssh-machine-witness-v1", "host_proof": decision,
                        "materials": _fact(materials, decision, artifact)}


def validate_evidence(value, control_root, workflow_sha):
    check(set(value) == MATERIAL_FIELDS | {"publishable", "verification", "host_proof", "materials"}
          and value.get("kind") == "release-evidence" and value.get("status") == "success" and value.get("verified") is True
          and value.get("authoritative") is True and value.get("publishable") is False
          and value.get("verification") == "openssh-machine-witness-v1", "不是权威远端机器证据")
    materials = {key: value[key] for key in MATERIAL_FIELDS} | {
        "kind": "release-materials", "status": "collected", "verified": False, "authoritative": False}
    decision = assess(materials, control_root, workflow_sha)
    check(decision["status"] == "verified" and value["host_proof"] == decision, "权威远端机器签名不符")
    fact = value["materials"]
    check(set(fact) == {"id", "digest", "receipt_sha256", "decision_sha256"}
          and fact["receipt_sha256"] == core.hashed(core.encoded(materials))
          and fact["decision_sha256"] == core.hashed(core.encoded(decision)), "权威材料摘要不符")
    core.number(fact["id"])
    core.exact(fact["digest"], r"sha256:[0-9a-f]{64}", "权威材料制品摘要非法")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("assess", "enforce"))
    parser.add_argument("--control", type=Path, required=True)
    parser.add_argument("--workflow-sha")
    parser.add_argument("--materials", type=Path)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    if args.command == "assess":
        check(args.materials is not None and args.workflow_sha is not None, "暂存判定需要材料及独立控制 SHA")
        value = core.decode(args.materials.read_bytes(), MAX_MATERIALS)
        core.save(args.output, assess(value, args.control, args.workflow_sha))
    else:
        api = control.AdmissionAPI(os.environ["GITHUB_REPOSITORY"])
        intent = control.guard(api, int(os.environ["GITHUB_RUN_ID"]), int(os.environ["GITHUB_RUN_ATTEMPT"]))
        core.save(args.output / "evidence.json", enforce(api, intent, args.control))


if __name__ == "__main__":
    try:
        main()
    except (ValueError, KeyError, TypeError, OSError, subprocess.SubprocessError):
        print("远端机器证明未放行；不得发布最终证据。", file=sys.stderr)
        sys.exit(1)
