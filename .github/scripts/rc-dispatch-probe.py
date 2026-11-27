#!/usr/bin/env python3
"""Two-member dispatch experiment. Artifact registration is NOT production status provenance."""
import argparse
from dataclasses import dataclass
import importlib.util
import json
import os
from pathlib import Path
import re
import struct
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

sys.dont_write_bytecode = True
REPO = "PeterFile/ct0x"
SHA = "18d11a3c0176fe98856aeeef6396e9ddfde0cca0"
TREE = "6df8bf3ccb986f8f4088dc310e0257b3f6285d9a"
BRANCH = "codex/ci-release-probe-20261126"
WORKFLOW_ID = 401975626
ADMISSION = "RC dispatch admission"
PROOF = "RC dispatch owner proof"
QUEUE_STATUSES = ("queued", "waiting", "pending", "requested")
BUILD_RUN, BUILD_JOB, BUILD_ARTIFACT = 45858696170, 140043726219, 15136299634
BUILD_CONTROLLER = "5d47236aff14966dbb83bc58fb673847535136bd"
ZIP_SHA = "6b29482a5532652f504d129be230157f12e9b7afef71d2971b39d641e41b5c18"
BINARY_SHA = "b82914bad4c7c5a33fd90caeb486f7430ea69419911613a26997e8beeabc8140"
LEGACY_SHA = "b9c531b8cf7a0e6cc89fac73576dae330840c67e866e143cb2d8aa01a76c0204"


def check(condition, message):
    if not condition:
        raise ValueError(message)


def git(root, *args):
    return subprocess.check_output(["git", "-C", str(root), *args], stderr=subprocess.PIPE, text=True, timeout=30).strip()


def outputs(values):
    with open(os.environ["GITHUB_OUTPUT"], "a", encoding="utf-8") as stream:
        for key, value in values.items():
            text = str(value).lower() if type(value) is bool else str(value)
            check("\n" not in text and "\r" not in text, "拒绝多行步骤输出")
            stream.write(f"{key}={text}\n")


def save(path, value):
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("x", encoding="utf-8") as stream:
        json.dump(value, stream, sort_keys=True, indent=2)
        stream.write("\n")
    print(json.dumps(value, sort_keys=True), flush=True)


def pin(controller):
    env = os.environ
    check(env["GITHUB_REPOSITORY"] == REPO and env["GITHUB_SERVER_URL"] == "https://github.com"
          and env["GITHUB_EVENT_NAME"] == "workflow_dispatch" and env["GITHUB_REF"] == "refs/heads/" + BRANCH
          and env["CONTROLLER_REF"] == REPO + "/.github/workflows/ci.yml@refs/heads/" + BRANCH,
          "仅允许已注册 ci.yml 的实验分支 dispatch")
    identity = env["CONTROLLER_SHA"]
    check(re.fullmatch(r"[0-9a-f]{40}", identity) and identity == env["GITHUB_SHA"]
          and env["TARGET_SHA"] == SHA and env["GITHUB_RUN_ATTEMPT"] == "1", "控制器、产品 SHA 或 attempt 不符")
    check(git(controller, "rev-parse", "HEAD") == identity and not git(controller, "status", "--porcelain", "--untracked-files=all"),
          "控制器检出不干净或不是不可变 workflow SHA")
    raw = (controller / ".github/rc-probe-policy.txt").read_bytes()
    check(re.fullmatch(rb"[0-9a-fA-F]{40}\n?", raw) is not None, "策略文件必须只含一个完整十六进制提交 SHA")
    return identity, git(controller, "rev-parse", "HEAD^{tree}"), raw.decode().rstrip("\n").lower()


def load_policy(policy):
    spec = importlib.util.spec_from_file_location("dispatch_probe_evidence", policy / ".github/scripts/release-evidence.py")
    evidence = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(evidence)
    return evidence.core, evidence.control, evidence


@dataclass(frozen=True)
class Scope:
    controller: str
    controller_tree: str
    policy: str
    policy_tree: str
    run_id: int

    @property
    def owner_name(self):
        return f"rc-dispatch-owner-{SHA}-{self.controller}"

    def title(self, run_id):
        return f"RC dispatch probe / {SHA} / {self.controller} / {run_id}-1"

    def native(self, core, run):
        core.number(run.get("id"))
        check(run.get("run_attempt") == 1 and type(run["run_attempt"]) is int
              and run.get("workflow_id") == WORKFLOW_ID and run.get("path") == ".github/workflows/ci.yml"
              and run.get("event") == "workflow_dispatch" and run.get("head_branch") == BRANCH
              and run.get("head_sha") == self.controller and run.get("display_title") == self.title(run["id"]),
              "实验原生 run、标题或不可变控制器不符")
        check(all(run.get(key, {}).get("full_name") == REPO for key in ("repository", "head_repository")), "实验原生仓库不符")
        check(core.number(run["repository"]["id"]) == core.number(run["head_repository"]["id"]), "拒绝 fork 实验")
        check(run.get("status") in ("queued", "waiting", "pending", "requested", "in_progress", "completed")
              and run.get("conclusion") in (None, "success", "failure", "cancelled", "timed_out", "skipped", "neutral", "action_required", "stale", "startup_failure"),
              "原生 run 状态未知")
        check((run["status"] == "completed") == (run.get("conclusion") is not None), "原生 run 终态不完整")
        return run

    def record(self, run, job):
        return {"schema": 1, "kind": "rc-dispatch-probe-owner", "storage": "actions-artifact-adapter",
                "repo": REPO, "sha": SHA, "tree": TREE, "controller_sha": self.controller,
                "controller_tree": self.controller_tree, "policy_sha": self.policy, "policy_tree": self.policy_tree,
                "workflow_id": WORKFLOW_ID, "run_id": run["id"], "run_attempt": 1, "job_id": job["id"],
                "winner_marker": f"dispatch-probe/{SHA}/{self.controller}/{run['id']}/1",
                "authoritative": False, "publishable": False}


class ReadAPI:
    """Fixed public GitHub API host, GET only, bounded pages/bytes/requests/time. Never discover credentials."""
    def __init__(self, core, token):
        check(bool(token), "缺少本作业只读 GH_TOKEN")
        self.core, self.repo, self.prefix = core, REPO, "/repos/" + REPO
        self.token = token
        self.opener = urllib.request.build_opener(core.SafeRedirect())
        self.started, self.requests = time.monotonic(), 0

    def request(self, method, path, data=None, binary=False):
        suffix = urllib.parse.urlsplit(path)
        route = suffix.path.removeprefix(self.prefix)
        check(method == "GET" and data is None and suffix.path.startswith(self.prefix + "/")
              and not suffix.scheme and not suffix.netloc and not suffix.fragment
              and re.fullmatch(r"/(?:actions/(?:runs/[1-9][0-9]*(?:/attempts/1)?(?:/jobs)?|jobs/[1-9][0-9]*|artifacts(?:/[1-9][0-9]*(?:/zip)?)?|workflows/401975626/runs)|git/commits/[0-9a-f]{40}|commits/master|compare/[0-9a-f]{40}\.\.\.[0-9a-f]{40})", route),
              "探针只允许固定仓库的指定 GET 端点")
        limit = 160 << 20 if binary else 2 << 20
        for retry in range(2):
            self.requests += 1
            check(self.requests <= 96 and time.monotonic() - self.started < 240, "只读请求预算已耗尽")
            request = urllib.request.Request("https://api.github.com" + path, method="GET", headers={
                "Authorization": "Bearer " + self.token, "Accept": "application/vnd.github+json",
                "X-GitHub-Api-Version": "2022-11-28", "User-Agent": "rc-dispatch-acceptance-probe"})
            try:
                with self.opener.open(request, timeout=15) as response:
                    raw = response.read(limit + 1)
                check(0 < len(raw) <= limit, "只读 API 响应为空或超限")
                return raw if binary else self.core.decode(raw, limit)
            except (urllib.error.URLError, TimeoutError) as error:
                transient = not isinstance(error, urllib.error.HTTPError) or error.code >= 500
                if retry == 0 and transient:
                    time.sleep(1)
                    continue
                raise OSError("只读 GitHub 请求失败；来源未知，禁止新认领") from None

    def get(self, path):
        return self.request("GET", path)

    def items(self, path, key):
        value = self.get(path + ("&" if "?" in path else "?") + "per_page=100&page=1")
        rows = value.get(key)
        check(isinstance(rows, list) and type(value.get("total_count")) is int
              and len(rows) == value["total_count"] <= 100, "拒绝截断或不完整的原生列表")
        return rows

    def run(self, run_id, attempt=None):
        self.core.number(run_id)
        check(attempt in (None, 1), "本实验禁止重跑 attempt")
        row = self.get(f"{self.prefix}/actions/runs/{run_id}" + ("/attempts/1" if attempt else ""))
        check(row.get("id") == run_id, "原生 run ID 不符")
        return row

    def jobs(self, run_id, attempt=1):
        check(attempt == 1, "本实验只接受首 attempt 作业")
        rows = self.items(f"{self.prefix}/actions/runs/{self.core.number(run_id)}/attempts/1/jobs", "jobs")
        check(all(row.get("run_id") == run_id and type(row.get("run_attempt")) is int
                  and row["run_attempt"] == 1 for row in rows), "原生 job run/attempt 不符")
        return rows


def job_named(core, jobs, name):
    rows = [job for job in jobs if job.get("name") == name]
    check(len(rows) == 1, "原生作业缺失或重复：" + name)
    core.number(rows[0].get("id"))
    return rows[0]


def artifact_files(api, artifact, producer, run, names, limit):
    core = api.core
    core.number(artifact.get("id"))
    owner = artifact.get("workflow_run", {})
    check(artifact.get("expired") is False and re.fullmatch(r"sha256:[0-9a-f]{64}", artifact.get("digest", ""))
          and owner.get("id") == run["id"] and owner.get("head_sha") == run["head_sha"]
          and owner.get("head_branch") == run["head_branch"]
          and owner.get("repository_id") == run["repository"]["id"]
          and owner.get("head_repository_id") == run["head_repository"]["id"]
          and 0 < core.number(artifact.get("size_in_bytes")) <= limit, "artifact 已过期或原生绑定不符")
    check(producer.get("run_id") == run["id"] and type(producer.get("run_attempt")) is int and producer["run_attempt"] == 1
          and producer.get("head_sha") == run["head_sha"]
          and producer.get("status") == "completed" and producer.get("conclusion") == "success", "artifact 生产作业没有原生成功证据")
    check(core.timestamp(producer.get("started_at")) <= core.timestamp(artifact.get("created_at"))
          <= core.timestamp(artifact.get("updated_at")) <= core.timestamp(producer.get("completed_at")), "artifact 不在生产作业窗口内")
    raw = api.request("GET", f"{api.prefix}/actions/artifacts/{artifact['id']}/zip", binary=True)
    check(len(raw) <= limit, "下载 ZIP 超限")
    return core.unpack(raw, artifact["digest"], names, limit)


class NotVisible(ValueError):
    """Unknown admission state is not permission to register a new owner."""


class UnresolvedOwner(NotVisible):
    """Observed execution or registration cannot become a fresh claim on a later miss."""


@dataclass(frozen=True)
class Owner:
    run: dict
    job: dict
    artifact: dict
    record: dict



def job_evidence(job):
    steps = job.get("steps", [])
    check(isinstance(steps, list) and all(isinstance(step, dict) for step in steps), "原生 steps 形状不可核实")
    executed = [step for step in steps if step.get("status") in ("in_progress", "completed")
                or step.get("conclusion") is not None]
    runner = job.get("runner_id")
    first = None
    if type(runner) is int and runner > 0:
        first = "runner_id"
    elif job.get("runner_name"):
        first = "runner_name"
    elif job.get("status") not in QUEUE_STATUSES:
        first = "job.status"
    elif job.get("conclusion") is not None:
        first = "job.conclusion"
    elif executed:
        first = "step.status"
    elif any(step.get("status") not in QUEUE_STATUSES for step in steps):
        first = "step.status:unknown"
    elif runner is not None and (type(runner) is not int or runner < 0):
        first = "runner_id:unknown"
    # Queue records can carry started_at before any runner or step executes.
    return {"job_id": job.get("id"), "status": job.get("status"), "runner_id": runner,
            "step_execution_count": len(executed), "first_evidence": first}


def discover(api, scope, role):
    query = urllib.parse.urlencode({"event": "workflow_dispatch", "branch": BRANCH, "head_sha": scope.controller})
    rows = api.items(f"{api.prefix}/actions/workflows/{WORKFLOW_ID}/runs?{query}", "workflow_runs")
    prefix = f"RC dispatch probe / {SHA} / {scope.controller} / "
    cohort = [row for row in rows if row.get("display_title", "").startswith(prefix)]
    check(len(cohort) <= 2, "本控制器的同产品 cohort 超过两次；需新控制器，禁止覆盖认领")
    if len(cohort) != 2 or scope.run_id not in {row.get("id") for row in cohort}:
        raise NotVisible("必须看到完整的双成员原生 cohort；请背靠背触发同一控制器")
    for row in cohort:
        scope.native(api.core, row)
    native = {row["id"]: scope.native(api.core, api.run(row["id"])) for row in cohort}
    jobs = {run_id: api.jobs(run_id) for run_id in native}
    current = native[scope.run_id]
    current_job = job_named(api.core, jobs[scope.run_id], role)
    check(current.get("status") == "in_progress" and current.get("conclusion") is None
          and current_job.get("status") == "in_progress" and current_job.get("head_sha") == scope.controller,
          "当前作业或 run 已停止，禁止继续")
    api.core.timestamp(current_job.get("started_at"))
    artifacts = api.items(api.prefix + "/actions/artifacts?" + urllib.parse.urlencode({"name": scope.owner_name}), "artifacts")
    check(len(artifacts) <= 1 and all(row.get("name") == scope.owner_name for row in artifacts), "owner artifact 名称重复或原生筛选不符")
    peers = {run_id: {"run_id": run_id, "run_status": run["status"],
                      "jobs": [job_evidence(job) for job in jobs[run_id]]}
             for run_id, run in native.items() if run_id != scope.run_id}
    for peer in peers.values():
        print(json.dumps(peer, sort_keys=True), flush=True)
    if artifacts:
        artifact = artifacts[0]
        owner_id = artifact.get("workflow_run", {}).get("id")
        check(owner_id in native, "owner artifact 不属于已核实的双成员 cohort")
        producer = job_named(api.core, jobs[owner_id], ADMISSION)
        if producer.get("status") != "completed":
            raise UnresolvedOwner("owner 上传后的原生 admission 终态尚不可核实")
        files = artifact_files(api, artifact, producer, native[owner_id], {"owner.json"}, 256 << 10)
        record = api.core.decode(files["owner.json"])
        # Canonical bytes distinguish JSON true/1 and 1.0/1 in native identity fields.
        check(isinstance(record, dict) and api.core.encoded(record) == api.core.encoded(scope.record(native[owner_id], producer)),
              "owner 回执不是同一产品、策略、控制器和原生 job")
        return current, current_job, Owner(native[owner_id], producer, artifact, record)
    for other_id, peer in peers.items():
        admissions = [job for job in jobs[other_id] if job.get("name") == ADMISSION]
        if peer["run_status"] == "completed" or any(job["first_evidence"] is not None for job in peer["jobs"]):
            raise UnresolvedOwner("先前成员已执行、终止或证据未知但 owner artifact 不可核实；禁止因过期、取消或断连重新认领")
        if len(admissions) > 1 or (not admissions and peer["run_status"] not in QUEUE_STATUSES):
            raise NotVisible("另一成员的未开始 admission 尚不可核实")
    # ci.yml serializes admission jobs. The sole active admission may run before an earlier queued request.
    return current, current_job, None


def reconcile(api, scope, role):
    # Exactly one visibility reconciliation. A missing prior registration never becomes a successful owner.
    unresolved = None
    try:
        first = discover(api, scope, role)
        if first[2] is not None:
            return first
    except NotVisible as error:
        if isinstance(error, UnresolvedOwner):
            unresolved = error
    time.sleep(3)
    second = discover(api, scope, role)
    if unresolved is not None and second[2] is None:
        raise unresolved
    return second


class OwnerRegister:
    """Only adapts admit/canonical_status storage. These pending rows are NOT GitHub product statuses."""
    def __init__(self, core, scope, current, job, owner):
        self.core, self.scope, self.current, self.job, self.owner = core, scope, current, job, owner
        self.pending = False

    def statuses_for(self, sha):
        check(sha == SHA, "实验 owner 查询越界")
        run, job = (self.owner.run, self.owner.job) if self.owner else (self.current, self.job)
        if not self.owner and not self.pending:
            return []
        return [{"id": job["id"], "context": self.core.CONTEXT, "state": "pending", "description": "release-owner-v1",
                 "creator": self.core.BOT, "target_url": f"https://github.com/{REPO}/actions/runs/{run['id']}"}]

    def check_owner(self, sha, row):
        check(row == self.statuses_for(sha)[0], "实验内存镜像或原生 owner 不符")
        return self.owner.run["id"] if self.owner else self.current["id"]

    def post_claim(self, sha, body):
        check(sha == SHA and self.owner is None and not self.pending
              and body.get("target_url") == f"https://github.com/{REPO}/actions/runs/{self.current['id']}", "拒绝重复或越界实验认领")
        self.pending = True



def resolve(control, api):
    # Deliberate normalized master TEST context. Never claim the native experiment ran on master.
    event = {"repository": {"full_name": REPO}, "inputs": {"sha": SHA}}
    target = control.resolve_target(api, REPO, "workflow_dispatch", event, "refs/heads/master")
    check((target["sha"], target["tree"]) == (SHA, TREE), "实时 master 没有确切实验产品或树")
    print(json.dumps({"resolution_context": "normalized-master-test-context", **target}, sort_keys=True), flush=True)
    return target


def rejection(label, function):
    try:
        function()
    except ValueError:
        print(json.dumps({"negative_proof": label, "result": "rejected"}), flush=True)
        return label
    raise ValueError("实际生产检查错误放行：" + label)


def binary_proof(api, evidence):
    core = evidence.core
    native = api.run(BUILD_RUN, 1)
    check(type(native.get("run_attempt")) is int and native["run_attempt"] == 1 and native.get("workflow_id") == WORKFLOW_ID
          and native.get("path") == ".github/workflows/ci.yml" and native.get("event") == "workflow_dispatch"
          and native.get("head_branch") == BRANCH and native.get("head_sha") == BUILD_CONTROLLER
          and native.get("display_title") == f"Release probe / build / {SHA} / {BUILD_RUN}-1 / Pi"
          and native.get("status") == "completed" and native.get("conclusion") == "success"
          and all(native.get(key, {}).get("full_name") == REPO for key in ("repository", "head_repository")), "原始构建原生 run 不符")
    check(core.number(native["repository"]["id"]) == core.number(native["head_repository"]["id"]), "原始构建不是本仓库")
    job = api.get(f"{api.prefix}/actions/jobs/{BUILD_JOB}")
    check(job.get("id") == BUILD_JOB and job.get("name") == "build", "原始构建 job 不符")
    artifact = api.get(f"{api.prefix}/actions/artifacts/{BUILD_ARTIFACT}")
    check(artifact.get("id") == BUILD_ARTIFACT and artifact.get("name") == f"release-probe-binary-{BUILD_RUN}-1"
          and artifact.get("digest") == "sha256:" + ZIP_SHA, "原始构建 ZIP 身份不符")
    files = artifact_files(api, artifact, job, native, {"workspace-darwin-amd64", "provenance.json"}, 160 << 20)
    binary, provenance = files["workspace-darwin-amd64"], core.decode(files["provenance.json"])
    check(core.hashed(binary) == BINARY_SHA and len(binary) >= 32
          and struct.unpack_from("<II", binary) == (0xfeedfacf, 0x01000007)
          and struct.unpack_from("<I", binary, 12)[0] == 2, "真实 Darwin x86_64 字节不符")
    expected_keys = {"schema", "mode", "controller", "workflow_ref", "workflow_sha", "target_sha", "target_tree",
                     "origin_master", "run_id", "run_attempt", "go_version", "build_settings", "binary", "sha256"}
    check(isinstance(provenance, dict) and set(provenance) == expected_keys, "旧 provenance 形状不符；禁止输出额外内容")
    settings = {"vcs": "git", "vcs.revision": SHA, "vcs.modified": "false", "GOOS": "darwin", "GOARCH": "amd64", "CGO_ENABLED": "0", "-trimpath": "true"}
    check(type(provenance["schema"]) is int and provenance["schema"] == 1 and provenance["mode"] == "build"
          and provenance["controller"] == "Pi" and provenance["workflow_sha"] == BUILD_CONTROLLER
          and provenance["workflow_ref"] == REPO + "/.github/workflows/ci.yml@refs/heads/" + BRANCH
          and provenance["target_sha"] == SHA and provenance["target_tree"] == TREE
          and provenance["run_id"] == str(BUILD_RUN) and provenance["run_attempt"] == "1"
          and provenance["binary"] == "workspace-darwin-amd64" and provenance["sha256"] == BINARY_SHA
          and provenance["build_settings"] == settings, "旧 provenance 与真实构建不符")
    core.sha(provenance["origin_master"])
    core.exact(provenance["go_version"], r"go[0-9]+\.[0-9]+\.[0-9]+", "Go 版本元数据非法")
    # Transport normalization only. Both production functions and the archived bytes stay untouched.
    intent = {"repo": REPO, "sha": SHA, "tree": TREE, "run_id": BUILD_RUN, "run_attempt": 1, "workflow_sha": BUILD_CONTROLLER}
    receipt = {**intent, "schema": 1, "kind": "release-target", "artifact_sha256": BINARY_SHA,
               "goos": "darwin", "goarch": "amd64", "cgo_enabled": False, "vcs_modified": False, "trimpath": True}
    evidence.verify_target(receipt.copy(), binary, intent.copy())
    negative = []
    for field, value in (("sha", BUILD_CONTROLLER), ("artifact_sha256", LEGACY_SHA), ("tree", "f" * 40), ("run_attempt", 2)):
        negative.append(rejection("real-binary-wrong-" + field,
            lambda field=field, value=value: evidence.verify_target(receipt | {field: value}, binary, intent.copy())))
    negative.append(rejection("same-sha-mutated-bytes", lambda: evidence.verify_target(receipt.copy(), binary + b"\0", intent.copy())))
    return {"verifier": "release-evidence.verify_target", "accepted": "pinned-real-crosshost-bytes", "negative": negative,
            "build_run_id": BUILD_RUN, "build_attempt": 1, "build_job_id": BUILD_JOB, "artifact_id": BUILD_ARTIFACT,
            "zip_sha256": ZIP_SHA, "binary_sha256": core.hashed(binary), "provenance_sha256": core.hashed(files["provenance.json"])}


def bootstrap(controller, policy):
    identity, controller_tree, policy_sha = pin(controller)
    check(git(policy, "rev-parse", "HEAD") == policy_sha and not git(policy, "status", "--porcelain", "--untracked-files=all"),
          "受审策略检出不干净或不是固定提交")
    core, control, evidence = load_policy(policy)
    scope = Scope(identity, controller_tree, policy_sha, git(policy, "rev-parse", "HEAD^{tree}"), core.number(int(os.environ["GITHUB_RUN_ID"])))
    for value in (scope.controller, scope.controller_tree, scope.policy, scope.policy_tree):
        core.sha(value)
    api = ReadAPI(core, os.environ.get("GH_TOKEN"))
    for sha, tree in ((scope.controller, scope.controller_tree), (scope.policy, scope.policy_tree)):
        commit = api.get(api.prefix + "/git/commits/" + sha)
        check(commit.get("sha") == sha and commit.get("tree", {}).get("sha") == tree, "当前仓库的原生控制/策略提交与检出不符")
    return scope, api, control, evidence


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("pin", "admit", "prove"))
    parser.add_argument("--controller", type=Path, default=Path("controller"))
    parser.add_argument("--policy", type=Path, default=Path("policy"))
    args = parser.parse_args()
    if args.command == "pin":
        _, controller_tree, policy_sha = pin(args.controller)
        outputs({"policy_sha": policy_sha})
        print(json.dumps({"policy_sha": policy_sha, "controller_tree": controller_tree}), flush=True)
        return
    scope, api, control, evidence = bootstrap(args.controller, args.policy)
    role = ADMISSION if args.command == "admit" else PROOF
    current, job, owner = reconcile(api, scope, role)
    out = Path(os.environ["RUNNER_TEMP"]) / "rc-dispatch-probe"
    if args.command == "admit":
        target = resolve(control, api)
        register = OwnerRegister(api.core, scope, current, job, owner)
        result = control.admit(register, REPO, SHA, scope.run_id, 1)
        check(control.canonical_status(register, REPO, SHA) == result["owner_run_id"], "实际 canonical_status 没有复核实验 owner")
        record = owner.record if owner else scope.record(current, job)
        if result["admitted"]:
            check(owner is None and register.pending, "禁止覆盖持久 owner")
            save(out / "owner/owner.json", record)
        decision = {"kind": "dispatch-probe-decision", "storage": "actions-artifact-adapter", **result,
                    "owner_job_id": record["job_id"], "winner_marker": record["winner_marker"],
                    "sha": SHA, "tree": TREE, "controller_sha": scope.controller, "policy_sha": scope.policy,
                    "policy_tree": scope.policy_tree, "normalized_target": target,
                    "native_owner_run_status": owner.run["status"] if owner else "in_progress",
                    "native_owner_run_conclusion": owner.run.get("conclusion") if owner else None,
                    "owner_work": "requires-native-admission-job-success" if result["admitted"] else "skipped-duplicate",
                    "production_authoritative": False, "publishable": False}
        save(out / "decision.json", decision)
        outputs({"admitted": result["admitted"], "owner_run_id": result["owner_run_id"], "owner_job_id": record["job_id"]})
        return
    check(owner is not None and owner.run["id"] == scope.run_id, "没有本次已成功上传并完成 admission 的持久 owner；禁止 owner work")
    negative = [rejection("production-native-context", lambda: api.core.native_candidate(current, REPO)),
                rejection("production-authenticated-owner-guard", lambda: control.guard(api, scope.run_id, 1))]
    target = resolve(control, api)
    proof = binary_proof(api, evidence)
    intent = {**target, "repo": REPO, "run_id": scope.run_id, "run_attempt": 1, "workflow_sha": scope.controller}
    negative.append(rejection("missing-Full", lambda: evidence.full_outputs({}, intent)))
    negative.append(rejection("missing-native-6A-6B-6C-machine", lambda: evidence.validate_machine({})))
    negative.append(rejection("production-gate-missing-Full-and-machine", lambda: evidence.verdict(api, intent, {}, args.policy)))
    check(not git(args.policy, "status", "--porcelain", "--untracked-files=all"), "探针不得修改生产策略源码")
    save(out / "proof.json", {"kind": "dispatch-probe-proof", "result": "probe-checks-passed", "owner": owner.record,
         "native_owner_artifact_id": owner.artifact["id"], "native_owner_job_id": owner.job["id"],
         "policy_verifier_sha256": api.core.file_hash(args.policy / ".github/scripts/release-evidence.py"),
         "normalized_master_test_target": target, "binary": proof, "negative": negative,
         "real_Full": False, "real_native_6ABC": False, "real_tag": False,
         "production_authoritative": False, "publishable": False})


if __name__ == "__main__":
    try:
        main()
    except (ValueError, KeyError, OSError, subprocess.SubprocessError) as error:
        print("dispatch 探针停止，未放行：" + str(error), file=sys.stderr)
        sys.exit(1)
