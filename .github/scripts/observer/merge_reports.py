"""Read-only intake of the trusted publisher's current source-head Merge gate Check.

Call exclusively for publisher attempts in the existing receiver hot set. Matrix
job names are candidates, never routing authority. Each PR is independently
verified, persisted and delivered; legacy discovery remains once per attempt.
Native merge-gate job failures remain the existing observer path's responsibility.
"""
from datetime import datetime, timezone
import hashlib
import io
import json
import re
from urllib.parse import parse_qs, quote, urlencode, urlsplit
import zipfile
import zlib

from github import Unknown
from store import Key

PUBLISHER = ".github/workflows/merge-conflict.yml"
NATIVE = ".github/workflows/merge-gate.yml"
FAILURE = "merge_gate_failure"
RESULT = "merge_gate_result"


def require(condition, message="合并发布者来源或校验证据不完整，保持未知并重试"):
    if not condition:
        raise Unknown(message)


def mapping(value):
    require(isinstance(value, dict))
    return value


def positive(value):
    return type(value) is int and 0 < value < 2 ** 63


def sha(value):
    return isinstance(value, str) and re.fullmatch(r"[0-9a-f]{40}", value) is not None


def decimal(value):
    require(isinstance(value, str) and re.fullmatch(r"[1-9][0-9]{0,18}", value) is not None)
    require(positive(int(value)))
    return int(value)


def url(value, repo, suffix, check=False):
    require(isinstance(value, str))
    try:
        parsed = urlsplit(value)
    except ValueError:
        raise Unknown("合并发布者证据链接无效") from None
    require(parsed.scheme == "https" and parsed.netloc == "github.com" and
            parsed.path == f"/{repo}/{suffix}" and not parsed.fragment and
            (not parsed.query or check and parsed.query == "check_suite_focus=true"))
    return value


def pages(observer, path, field=None):
    # Keep original query filters and normalize numeric repository Links. This
    # source also works with API versions whose pages() only accepts bare paths.
    endpoint, _, query = path.partition("?")
    filters = parse_qs(query, keep_blank_values=True)
    result, total = [], None
    for page in range(1, 501):
        data, link = observer.api.document(path + ("&" if query else "?") + f"per_page=100&page={page}")
        items = mapping(data).get(field) if field else data
        require(isinstance(items, list), "合并发布者分页形状无效，保持未知")
        if field:
            current = data.get("total_count")
            require(type(current) is int and current >= 0 and (total is None or total == current))
            total = current
        result.extend(items)
        require(len(result) <= 50000, "合并发布者分页达到安全上限，保持未知")
        relations = {}
        if link is not None:
            require(isinstance(link, str) and bool(link))
            for part in link.split(","):
                match = re.fullmatch(r'\s*<([^<>]+)>;\s*rel="(next|prev|first|last)"\s*', part)
                require(match is not None, "合并发布者分页链接无效")
                try:
                    parsed = urlsplit(match[1])
                    params = parse_qs(parsed.query, keep_blank_values=True, strict_parsing=True)
                except ValueError:
                    raise Unknown("合并发布者分页参数无效") from None
                number = params.get("page", [])
                require(len(number) == 1 and re.fullmatch(r"[1-9][0-9]{0,5}", number[0]) is not None)
                number, relation = int(number[0]), match[2]
                require(relation not in relations and parsed.scheme == "https" and parsed.netloc == "api.github.com"
                        and not parsed.fragment and observer.api.same_repo_path(endpoint, parsed.path)
                        and params == dict(filters, per_page=["100"], page=[str(number)]))
                require((relation == "next" and number == page + 1) or
                        (relation == "prev" and number == page - 1) or
                        (relation == "first" and number == 1) or (relation == "last" and number >= page))
                relations[relation] = number
        if "next" not in relations:
            require("last" not in relations or relations["last"] == page)
            require(total is None or len(result) == total, "合并发布者分页不完整，保持未知")
            return result
        require(bool(items) and (total is None or len(result) < total))
    raise Unknown("合并发布者分页未完成，保持未知")


def repo_identity(value):
    value = mapping(value)
    require(positive(value.get("id")) and isinstance(value.get("full_name"), str) and
            re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", value["full_name"]) is not None and
            all(part not in {".", ".."} for part in value["full_name"].split("/")))
    return value["id"], value["full_name"]


def workflow(observer, filename):
    item = mapping(observer.api.get(observer.root + "/actions/workflows/" + filename))
    require(positive(item.get("id")) and item.get("path") == ".github/workflows/" + filename and item.get("state") == "active")
    return item["id"]


def run_identity(observer, run, path, workflow_id, run_id, attempt, successful=True):
    run = mapping(run)
    require(positive(run.get("id")) and run["id"] == run_id and positive(run.get("run_attempt")) and run["run_attempt"] == attempt
            and run.get("path") == path and positive(run.get("workflow_id")) and run["workflow_id"] == workflow_id and sha(run.get("head_sha")))
    require(repo_identity(run.get("repository"))[1] == observer.repo)
    url(run.get("html_url"), observer.repo, f"actions/runs/{run_id}")
    if successful:
        require(run.get("status") == "completed" and run.get("conclusion") == "success", "验证运行尚未明确成功，保持待核实")
    else:
        require(run.get("status") in ("in_progress", "completed"), "发布者尚未开始执行，保持待核实")


def publisher_policy(observer, run, workflow_sha=None, source_head=None):
    repository, head_repository = repo_identity(run.get("repository")), repo_identity(run.get("head_repository"))
    linked = run.get("pull_requests", [])
    require(isinstance(linked, list))
    source_event = run.get("event") == "pull_request_target" and (
        source_head == run["head_sha"] or run.get("head_branch") != "master" or head_repository != repository or
        any(isinstance(item, dict) and isinstance(item.get("head"), dict) and item["head"].get("sha") == run["head_sha"] for item in linked))
    if source_event:
        require(source_head is None or source_head == run["head_sha"], "PR 事件来源与发布的 source head 不符")
        associated = []
        require(0 < len(linked) <= 50000)
        for item in linked:
            item = mapping(item)
            head, base = mapping(item.get("head")), mapping(item.get("base"))
            head_repo, base_repo = mapping(head.get("repo")), mapping(base.get("repo"))
            require(positive(item.get("number")))
            if (head.get("sha") == run["head_sha"] and head.get("ref") == run["head_branch"] and
                    positive(head_repo.get("id")) and head_repo["id"] == head_repository[0] and
                    head_repo.get("url") == f"https://api.github.com/repos/{head_repository[1]}" and
                    base.get("ref") == "master" and sha(base.get("sha")) and
                    positive(base_repo.get("id")) and base_repo["id"] == repository[0] and
                    base_repo.get("url") == f"https://api.github.com/repos/{observer.repo}"):
                associated.append(item)
        require(len(associated) == 1, "发布者没有唯一绑定的 PR/source 与固定 master 工作流身份")
        policy = associated[0]["base"]["sha"]
    else:
        # Preserve legacy base-head receipts; native source-head events use the bound base above.
        require(head_repository == repository and run.get("head_branch") == "master")
        policy = run["head_sha"]
    require(workflow_sha is None or workflow_sha == policy, "发布者固定工作流 SHA 与事件证明不符")
    master = branch(observer, "master")
    if policy != master:
        compared = mapping(observer.api.get(observer.root + f"/compare/{policy}...{master}"))
        require(compared.get("status") in ("ahead", "identical") and
                mapping(compared.get("merge_base_commit")).get("sha") == policy,
                "发布者固定工作流不属于已发布默认分支，保持未知")
    return policy


def publisher_run(observer, run, workflow_sha=None, source_head=None):
    require(positive(run.get("id")) and positive(run.get("run_attempt")))
    identity = workflow(observer, "merge-conflict.yml")
    run_identity(observer, run, PUBLISHER, identity, run["id"], run["run_attempt"], successful=False)
    require(isinstance(run.get("event"), str) and run["event"] in {"pull_request_target", "workflow_run"}
            and isinstance(run.get("head_branch"), str) and bool(run["head_branch"])
            and (run["event"] != "workflow_run" or run["head_branch"] == "master"))
    current = mapping(observer.api.get(observer.root + f"/actions/runs/{run['id']}"))
    require(positive(current.get("run_attempt")) and current["run_attempt"] >= run["run_attempt"])
    run_identity(observer, current, PUBLISHER, identity, run["id"], current["run_attempt"], successful=False)
    if current["run_attempt"] > run["run_attempt"]:
        current = mapping(observer.api.get(observer.root + f"/actions/runs/{run['id']}/attempts/{run['run_attempt']}"))
        run_identity(observer, current, PUBLISHER, identity, run["id"], run["run_attempt"], successful=False)
    require(all(current.get(key) == run.get(key) for key in ("event", "head_sha", "head_branch", "run_started_at")) and
            repo_identity(current.get("repository")) == repo_identity(run["repository"]) and
            repo_identity(current.get("head_repository")) == repo_identity(run.get("head_repository")))
    return publisher_policy(observer, run, workflow_sha, source_head)


def timestamp(value):
    try:
        result = datetime.fromisoformat(value.replace("Z", "+00:00"))
        require(result.tzinfo is not None)
        return result.astimezone(timezone.utc)
    except (ValueError, TypeError, AttributeError):
        raise Unknown("发布作业执行时间不完整，无法核实 attempt 来源") from None


def reporter_origin(observer, run, job, legacy):
    require(positive(job.get("id")) and type(job.get("run_id")) is int and job["run_id"] == run["id"] and
            positive(job.get("run_attempt")) and job["run_attempt"] <= run["run_attempt"] and job.get("head_sha") == run["head_sha"])
    url(job.get("html_url"), observer.repo, f"actions/runs/{run['id']}/job/{job['id']}")
    if job["run_attempt"] < run["run_attempt"]:
        return "inherited"
    if legacy and not any(job.get(key) for key in ("started_at", "completed_at")) and not run.get("run_started_at"):
        return "current"  # Legacy receipts still require exact Check issuer metadata.
    start = timestamp(run.get("run_started_at"))
    finished = timestamp(job["completed_at"]) if job.get("completed_at") else None
    began = timestamp(job["started_at"]) if job.get("started_at") else None
    if finished and finished < start:
        return "inherited"  # GitHub may relabel an inherited job's run_attempt.
    require(began is not None and began >= start and (finished is None or finished >= began) and
            (job.get("status") != "completed" or finished is not None),
            "发布作业没有本 attempt 的可靠执行时间，保持未知")
    return "current"


def live_pr(observer, number, repository):
    item = mapping(observer.api.get(observer.root + f"/pulls/{number}"))
    validate_pr(observer, item, repository)
    require(item["number"] == number)
    return item


def validate_pr(observer, item, repository, allow_closed=False):
    item = mapping(item)
    require(positive(item.get("number")) and item.get("state") in ("open", "closed") and
            (allow_closed or item["state"] == "open" and not item.get("merged", False) and item.get("merged_at") is None),
            "PR 已关闭、合入或来源不明，禁止派给作者")
    url(item.get("html_url"), observer.repo, f"pull/{item['number']}")
    for side in ("head", "base"):
        value = mapping(item.get(side))
        require(sha(value.get("sha")) and isinstance(value.get("ref"), str) and 0 < len(value["ref"]) <= 1024
                and not re.search(r"[\x00-\x20\x7f\ud800-\udfff]", value["ref"]))
        repo_identity(value.get("repo"))
    require(repo_identity(item["base"]["repo"]) == repository and repository[1] == observer.repo)
    require(item.get("merge_commit_sha") is None or sha(item["merge_commit_sha"]))


def source(pr):
    return dict(repo_id=pr["head"]["repo"]["id"], repo=pr["head"]["repo"]["full_name"],
                ref=pr["head"]["ref"], base_ref=pr["base"]["ref"])


def fingerprint(pr):
    return [pr["number"], source(pr), pr["head"]["sha"], pr["base"]["sha"],
            repo_identity(pr["base"]["repo"]), pr.get("merge_commit_sha"), pr.get("mergeable"), pr.get("mergeable_state"), pr.get("draft")]


def unique_json(pairs):
    result = {}
    for key, value in pairs:
        require(key not in result, "合并发布者 summary 含重复字段，保持未知")
        result[key] = value
    return result


def check_metadata(check):
    summary = mapping(mapping(check).get("output")).get("summary")
    require(isinstance(summary, str) and len(summary) <= 128 * 1024)
    try:
        metadata = mapping(json.loads(summary, object_pairs_hook=unique_json))
    except (ValueError, UnicodeError, RecursionError):
        raise Unknown("合并发布者 summary 不是完整、唯一字段的 JSON") from None
    return metadata


def target_details(pr):
    return dict(pr=pr["number"], head=pr["head"]["sha"], head_ref=pr["head"]["ref"],
                head_repo_id=pr["head"]["repo"]["id"], head_repo=pr["head"]["repo"]["full_name"],
                base_ref=pr["base"]["ref"], base=pr["base"]["sha"], draft=pr.get("draft"))


def intersection_target(metadata, pr):
    """Validate the source aggregate, then select only this PR's exact verdict."""
    members, targets = metadata.get("membership"), metadata.get("targets")
    require(isinstance(members, list) and isinstance(targets, list) and 0 < len(members) == len(targets) <= 50000)
    numbers = []
    for member, target in zip(members, targets):
        member, target = mapping(member), mapping(target)
        require(positive(member.get("pr")) and positive(member.get("head_repo_id")) and
                member.get("head") == pr["head"]["sha"] and sha(member.get("base")) and
                type(member.get("draft")) is bool and
                all(isinstance(member.get(key), str) and member[key] for key in ("head_ref", "head_repo", "base_ref")))
        require(set(member) == set(target_details(pr)) and
                all(type(target.get(key)) is type(value) and target.get(key) == value for key, value in member.items()),
                "交集目标与 membership 的 PR/source/base 身份不符")
        require(target.get("result") in ("success", "failure", "pending"))
        numbers.append(member["pr"])
    require(numbers == sorted(set(numbers)) and metadata.get("pr") == numbers[0], "交集成员重复、顺序或代表 PR 不符")
    failed = [row["pr"] for row in targets if row["result"] == "failure"]
    pending = [row["pr"] for row in targets if row["result"] == "pending"]
    result = "failure" if failed else "pending" if pending else "success"
    require(metadata.get("failed_prs") == failed and metadata.get("pending_prs") == pending and metadata.get("result") == result,
            "交集总结果与逐 PR 结果矛盾，保持未知")
    selected = [row for row in targets if row["pr"] == pr["number"]]
    require(len(selected) == 1 and all(type(selected[0].get(key)) is type(value) and selected[0].get(key) == value
                                     for key, value in target_details(pr).items()),
            "交集没有唯一匹配的当前 PR/source/base 身份")
    return selected[0]


def parse_check(observer, run, pr, check):
    check = mapping(check)
    require(positive(check.get("id")) and check.get("name") == "Merge gate" and check.get("head_sha") == pr["head"]["sha"]
            and repo_identity(pr["base"]["repo"])[1] == observer.repo)
    app = mapping(check.get("app"))
    identities = (f"merge-gate:v1:{observer.repo}:pr:{pr['number']}:head:{pr['head']['sha']}",
                  f"merge-gate:v2:{observer.repo}:source:{pr['head']['sha']}")
    require(type(app.get("id")) is int and app["id"] == 15368 and app.get("slug") == "github-actions" and
            check.get("external_id") in identities, "同名 Merge gate 的写入者或 PR/head 身份不符，拒绝归属")
    url(check.get("html_url"), observer.repo, f"runs/{check['id']}", check=True)
    metadata = check_metadata(check)
    intersection = "membership" in metadata
    require(type(metadata.get("schema")) is int and metadata["schema"] == 1 and metadata.get("repo") == observer.repo
            and positive(metadata.get("pr")) and (intersection or metadata["pr"] == pr["number"])
            and metadata.get("head") == pr["head"]["sha"])
    if intersection:
        require(check["external_id"] == identities[1] and metadata.get("reason_code") == "source_head_intersection")
        intersection_target(metadata, pr)
    else:
        require(metadata.get("reason_code") != "source_head_intersection", "交集缺少 membership，保持未知")
    publisher = mapping(metadata.get("publisher"))
    require(publisher.get("workflow") == PUBLISHER)
    source_event = run.get("event") == "pull_request_target" and run["head_sha"] == check["head_sha"]
    if source_event:
        require(sha(publisher.get("workflow_sha")), "原生 PR 事件缺少固定 master 工作流 SHA")
    publisher_policy(observer, run, publisher.get("workflow_sha"), check["head_sha"])
    publisher_run, publisher_attempt = decimal(publisher.get("run_id")), decimal(publisher.get("run_attempt"))
    if (publisher_run, publisher_attempt) != (run["id"], run["run_attempt"]):
        return None
    if source_event:
        require(check.get("status") == "completed", "待定 PR 事件 Check 不能授权终态 reporter")
        latest = mapping(observer.api.get(observer.root + f"/actions/runs/{run['id']}"))
        run_identity(observer, latest, PUBLISHER, run["workflow_id"], run["id"], run["run_attempt"])
        require(all(latest.get(key) == run.get(key) for key in ("event", "head_sha", "head_branch", "run_started_at")) and
                repo_identity(latest.get("repository")) == repo_identity(run["repository"]) and
                repo_identity(latest.get("head_repository")) == repo_identity(run["head_repository"]),
                "当前 PR 事件 publisher 身份已变化，历史终态不可路由")
    if check.get("status") in ("in_progress", "queued"):
        require(check.get("conclusion") is None and metadata.get("result") == "pending")
    else:
        require(check.get("status") == "completed" and check.get("conclusion") in ("success", "failure")
                and metadata.get("result") == check["conclusion"])
        if not intersection and "targets" in metadata:
            require(metadata["targets"] == [target_details(pr)], "终态 Check 没有唯一匹配的当前 PR/source/base 身份")
    return metadata


def current_check(observer, head):
    rows = pages(observer, observer.root + f"/commits/{head}/check-runs?filter=all", "check_runs")
    require(all(isinstance(row, dict) for row in rows))
    checks = [row for row in rows if row.get("name") == "Merge gate"]
    for check in checks:
        app = mapping(check.get("app"))
        require(positive(check.get("id")) and check.get("head_sha") == head and
                type(app.get("id")) is int and app["id"] == 15368 and app.get("slug") == "github-actions",
                "同名 Merge gate 的数值身份、来源或写入者不符")
        url(check.get("html_url"), observer.repo, f"runs/{check['id']}", check=True)
        # Historical v1 remains single-row only; generations must share v2 ownership.
        require(len(checks) == 1 or check.get("external_id") == f"merge-gate:v2:{observer.repo}:source:{head}",
                "多代 Merge gate 没有唯一同源 v2 身份")
    require(len({check["id"] for check in checks}) == len(checks), "Merge gate 代次数值身份重复，保持未知")
    return max(checks, key=lambda check: check["id"]) if checks else None


def discover(observer, run):
    key = f"merge-report:{observer.repo}:{run['id']}:{run['run_attempt']}"
    cached = observer.store.meta_get(key)
    repository = repo_identity(run["repository"])
    if cached is None:
        linked = run.get("pull_requests", [])
        require(isinstance(linked, list))
        if linked:
            numbers = [mapping(item).get("number") for item in linked]
            require(all(positive(number) for number in numbers))
        else:
            prs = pages(observer, observer.root + "/pulls?state=open")
            for pr in prs:
                validate_pr(observer, pr, repository)
            numbers = [pr["number"] for pr in prs]
        require(len(numbers) == len(set(numbers)) and len(numbers) <= 50000)
        cached = {"candidates": numbers}
        observer.store.meta_put(key, cached)
    numbers = [cached["pr"]] if "pr" in cached else cached["candidates"]
    matches = []
    for number in numbers:
        pr = mapping(observer.api.get(observer.root + f"/pulls/{number}"))
        validate_pr(observer, pr, repository, allow_closed=True)
        require(pr["number"] == number)
        if pr["state"] != "open" or pr.get("merged", False) or pr.get("merged_at") is not None:
            continue
        check = current_check(observer, pr["head"]["sha"])
        if check is not None:
            metadata = parse_check(observer, run, pr, check)
            if metadata is not None:
                matches.append((pr, check, metadata))
    if matches and all("membership" in metadata for _, _, metadata in matches):
        # The same source-owned Check can appear under several live PRs. It is
        # one authenticated publication; distinct Checks remain ambiguous.
        require(len({(check["id"], check["head_sha"]) for _, check, _ in matches}) == 1 and
                all(check == matches[0][1] and metadata == matches[0][2] for _, check, metadata in matches),
                "同源交集 Check 或发布快照冲突，保持待核实")
        matches = matches[:1]
    require(len(matches) == 1, "未找到唯一属于本次 publisher run/attempt 的 Merge gate，保持待核实")
    observer.store.meta_put(key, dict(cached, pr=matches[0][0]["number"]))
    return matches[0]


def matrix_check(observer, run, number):
    repository = repo_identity(run["repository"])
    candidate = mapping(observer.api.get(observer.root + f"/pulls/{number}"))
    validate_pr(observer, candidate, repository, allow_closed=True)
    require(candidate["number"] == number)
    target = candidate["head"]["sha"]
    check = current_check(observer, target)
    require(check is not None, "此来源提交没有当前 Merge gate，保持未知")
    claimed = check_metadata(check)
    require(positive(claimed.get("pr")))
    if "membership" in claimed:
        members = claimed["membership"]
        require(isinstance(members, list) and members and all(isinstance(row, dict) and positive(row.get("pr")) for row in members))
        numbers = [row["pr"] for row in members]
        if number in numbers:
            pr = live_pr(observer, number, repository)
        else:
            # The source-owned Check names its representative. This authenticates
            # the snapshot only; delivery still verifies every declared live target.
            require(candidate["state"] == "closed" and claimed["pr"] in numbers, "作业候选不在交集中，不能猜测其他 PR")
            pr = live_pr(observer, claimed["pr"], repository)
    else:
        pr = live_pr(observer, claimed["pr"], repository)
    require(pr["head"]["sha"] == target, "Check 指向的当前 PR 不属于作业候选来源提交")
    metadata = parse_check(observer, run, pr, check)
    require(metadata is not None, "此来源的 Merge gate 不属于本次 publisher run/attempt")
    return pr, check, metadata


def branch(observer, ref):
    item = mapping(observer.api.get(observer.root + "/git/ref/heads/" + quote(ref, safe="/")))
    obj = mapping(item.get("object"))
    require(item.get("ref") == "refs/heads/" + ref and obj.get("type") == "commit" and sha(obj.get("sha")))
    return obj["sha"]


def merge_commit(observer, value):
    item = mapping(observer.api.get(observer.root + "/git/commits/" + value))
    require(item.get("sha") == value and sha(mapping(item.get("tree")).get("sha")) and isinstance(item.get("parents"), list))
    require(all(isinstance(parent, dict) and sha(parent.get("sha")) for parent in item["parents"]))
    return item


def validation(observer, pr, metadata):
    proof = mapping(metadata.get("validation"))
    identity = mapping(proof.get("identity"))
    require(identity.get("repo") == observer.repo and type(identity.get("pr")) is int and identity["pr"] == pr["number"]
            and identity.get("head") == pr["head"]["sha"] and identity.get("base") == pr["base"]["sha"]
            and proof.get("source") == source(pr) and all(sha(identity.get(key)) for key in ("sha", "tree", "base", "head")))
    require(pr.get("mergeable") is True and pr.get("merge_commit_sha") == identity["sha"]
            and branch(observer, pr["base"]["ref"]) == identity["base"])
    commit = merge_commit(observer, identity["sha"])
    require(commit["tree"]["sha"] == identity["tree"] and
            [parent["sha"] for parent in commit["parents"]] == [identity["base"], identity["head"]])
    run_id, attempt = decimal(identity.get("run_id")), decimal(identity.get("run_attempt"))
    workflow_id = workflow(observer, "merge-gate.yml")
    current = observer.api.get(observer.root + f"/actions/runs/{run_id}")
    exact = observer.api.get(observer.root + f"/actions/runs/{run_id}/attempts/{attempt}")
    for native in (current, exact):
        run_identity(observer, native, NATIVE, workflow_id, run_id, attempt)
        require(repo_identity(native["repository"]) == repo_identity(pr["base"]["repo"]))
    require(all(current.get(key) == exact.get(key) for key in ("event", "head_sha", "head_branch", "head_repository")))
    if exact.get("event") == "pull_request":
        require(native_matches(exact, pr, observer.repo) and proof.get("controller_sha") in (None, ""))
    else:
        controller = proof.get("controller_sha")
        require(exact.get("event") == "workflow_dispatch" and sha(controller) and exact.get("head_sha") == controller
                and exact.get("head_branch") == "master" and
                repo_identity(exact.get("head_repository")) == repo_identity(pr["base"]["repo"]))
        master = branch(observer, "master")
        compared = mapping(observer.api.get(observer.root + f"/compare/{controller}...{master}"))
        require(compared.get("status") == ("identical" if controller == master else "ahead") and
                mapping(compared.get("merge_base_commit")).get("sha") == controller)
    return identity, exact


def native_matches(native, pr, repo):
    return (native.get("path") == NATIVE and native.get("event") == "pull_request" and
            repo_identity(native.get("repository")) == repo_identity(pr["base"]["repo"]) and
            native["repository"]["full_name"] == repo and native.get("head_sha") == pr["head"]["sha"] and
            native.get("head_branch") == pr["head"]["ref"] and
            repo_identity(native.get("head_repository")) == repo_identity(pr["head"]["repo"]))


def native_failure(observer, pr):
    rows = observer.store.db.execute(
        "SELECT a.id,a.payload,a.run_id,a.attempt,o.body FROM actions a JOIN observations o "
        "ON o.repo=a.repo AND o.run_id=a.run_id AND o.attempt=a.attempt "
        "WHERE a.repo=? AND a.sha IN (?,?) AND a.kind='first_failure'",
        (observer.repo, pr["head"]["sha"], pr.get("merge_commit_sha"))).fetchall()
    for row in rows:
        payload, observed = json.loads(row["payload"]), json.loads(row["body"])
        if (payload.get("pr") == pr["number"] and payload.get("workflow") == NATIVE and
                payload.get("source_head") == pr["head"]["sha"] and native_matches(observed["run"], pr, observer.repo)):
            return row
    return None


def delivered_native_failure(observer, pr, target, native, jobs):
    """Correlate an authenticated Check target with this author's committed first notice."""
    author = observer.store.author(observer.repo, pr["number"])
    if not author:
        return None

    def matches(value, expected):
        return isinstance(value, dict) and all(type(value.get(key)) is type(item) and value.get(key) == item
                                               for key, item in expected.items())

    run_id, attempt = native["id"], native["run_attempt"]
    binding = observer.store.target(observer.repo, run_id, attempt)
    expected = dict(pr=pr["number"], pr_head=pr["head"]["sha"], source=source(pr),
                    source_head=native["head_sha"], workflow_sha=native["head_sha"], sha=target["merge_sha"], tree=target["tree"])
    if (not matches(binding, expected) or binding.get("provenance") not in ("merge-plan", "merge-identity") or
            not positive(binding.get("artifact_id")) or not isinstance(binding.get("digest"), str) or
            re.fullmatch(r"sha256:[0-9a-f]{64}", binding["digest"]) is None):
        return None
    route = observer.store.meta_get(f"pr-route:{observer.repo}:{run_id}:{attempt}")
    if not matches(route, dict(pr=pr["number"], source_head=native["head_sha"], workflow=NATIVE, event=native["event"], source=source(pr))):
        return None
    rows = observer.store.db.execute(
        "SELECT a.id,a.sha,a.run_id,a.attempt,a.job,a.kind,i.payload,o.body FROM actions a "
        "JOIN outbox b ON b.action_id=a.id JOIN inbox i ON i.receipt_id=b.receipt_id AND i.recipient=b.recipient "
        "JOIN observations o ON o.repo=a.repo AND o.run_id=a.run_id AND o.attempt=a.attempt "
        "WHERE a.repo=? AND a.sha IN (?,?) AND a.run_id=? AND a.attempt=? AND a.kind IN ('first_failure','job_details') "
        "AND b.recipient=? AND b.state IN ('delivered','acked') AND b.delivered_at IS NOT NULL",
        (observer.repo, native["head_sha"], target["merge_sha"], run_id, attempt, author)).fetchall()
    receipts = []
    for row in rows:
        try:
            payload, observed = json.loads(row["payload"]), json.loads(row["body"])
        except (ValueError, TypeError):
            continue
        observed = observed.get("run") if isinstance(observed, dict) else None
        if (not matches(observed, {key: native[key] for key in
                ("id", "run_attempt", "path", "workflow_id", "head_sha", "head_branch", "event", "run_started_at")}) or
                any(not matches(observed.get(key), {field: native[key][field] for field in ("id", "full_name")})
                    for key in ("repository", "head_repository"))):
            continue
        if matches(payload, dict(repo=observer.repo, sha=row["sha"], run_id=run_id, attempt=attempt, action=row["kind"], job=row["job"])):
            receipts.append((row, payload))
    for row, payload in receipts:
        if row["kind"] != "first_failure" or row["sha"] not in (native["head_sha"], target["merge_sha"]):
            continue
        failures = [job for job in jobs if str(job["id"]) == row["job"] and job.get("conclusion") in
                    ("failure", "cancelled", "timed_out", "action_required", "startup_failure")]
        if len(failures) != 1:
            continue
        job = failures[0]
        common = dict(workflow=NATIVE, event=native["event"], workflow_sha=native["head_sha"], source_head=native["head_sha"],
                      pr=pr["number"], work_origin="current", job_id=job["id"], job_url=job["html_url"],
                      status="completed", conclusion=job["conclusion"], started_at=job["started_at"], completed_at=job["completed_at"])
        if not matches(payload, common):
            continue

        def bound(value):
            return (matches(value, dict(common, tested_sha=target["merge_sha"], tested_tree=target["tree"], target_unknown=False)) and
                    value.get("provenance") in ("merge-plan", "merge-identity") and
                    (value.get("provenance") != "merge-identity" or isinstance(binding.get("early_identity"), dict) and
                     value.get("identity_evidence") == binding["early_identity"]))

        if bound(payload):
            return row
        # PR notices can precede the identity artifact. Only a delivered binding
        # of the same canonical first_failure may supply the missing target.
        if matches(payload, dict(tested_sha=None, tested_tree=None, target_unknown=True, provenance="source_only")) and any(
                detail["kind"] == "job_details" and matches(value, dict(update_of=row["id"], detail_kind="target_binding")) and bound(value)
                for detail, value in receipts):
            return row
    return None


def validation_receipt(observer, native, proof=None):
    """Read bounded data only; the publisher's receipt is not executable code."""
    run_id, attempt = native["id"], native["run_attempt"]
    rows = pages(observer, observer.root + f"/actions/runs/{run_id}/artifacts", "artifacts")
    matches = [row for row in rows if mapping(row).get("name") == f"merge-gate-{run_id}-{attempt}"]
    require(len(matches) == 1, "精确验证回执 artifact 缺失或重复")
    artifact = matches[0]
    limit = 16 * 1024 * 1024
    require(positive(artifact.get("id")) and (proof is None or positive(proof.get("artifact_id")) and artifact["id"] == proof["artifact_id"]) and
            artifact.get("expired") is False and type(mapping(artifact.get("workflow_run")).get("id")) is int and
            artifact["workflow_run"]["id"] == run_id and
            type(artifact.get("size_in_bytes")) is int and 0 < artifact["size_in_bytes"] <= limit)
    data = observer.api.blob(observer.root + f"/actions/artifacts/{artifact['id']}/zip", limit=limit)
    require(artifact.get("digest") == "sha256:" + hashlib.sha256(data).hexdigest() and
            (proof is None or artifact["digest"] == proof.get("artifact_digest")),
            "精确验证 artifact 摘要不符")
    try:
        with zipfile.ZipFile(io.BytesIO(data)) as archive:
            entries = archive.infolist()
            require(len(entries) == 1 and entries[0].filename == "merge-gate.json" and
                    not entries[0].is_dir() and 0 < entries[0].file_size <= limit and not entries[0].flag_bits & 1)
            receipt = mapping(json.loads(archive.read(entries[0]), object_pairs_hook=unique_json))
    except (ValueError, UnicodeError, RecursionError, zipfile.BadZipFile, RuntimeError, OSError, EOFError, NotImplementedError, zlib.error):
        raise Unknown("精确验证回执不是有界完整 JSON") from None
    return receipt


def validation_jobs(observer, pr, native):
    """Exclude only REST-authenticated custom Check echoes, never native work."""
    jobs = pages(observer, observer.root + f"/actions/runs/{native['id']}/attempts/{native['run_attempt']}/jobs", "jobs")
    require(all(isinstance(job, dict) for job in jobs))
    counts = {}
    for job in jobs:
        if positive(job.get("id")):
            counts[job["id"]] = counts.get(job["id"], 0) + 1
    result = []
    for job in jobs:
        if job.get("name") != "Merge gate":
            result.append(job)
            continue
        require(job.get("steps") == [] and job.get("labels") == [] and
                all(key in job and job[key] is None for key in ("runner_id", "runner_name", "runner_group_id", "runner_group_name")),
                "额外 Merge gate 作业没有完整的无 runner 空执行证明")
        identity = job.get("id")
        require(positive(identity) and counts[identity] == 1 and positive(job.get("run_id")) and job["run_id"] == native["id"] and
                positive(job.get("run_attempt")) and job["run_attempt"] == native["run_attempt"] and
                job.get("head_sha") == native["head_sha"] == pr["head"]["sha"] and job.get("head_branch") == native["head_branch"],
                "检查回显没有唯一的精确运行和来源身份")
        api_root = f"https://api.github.com/repos/{observer.repo}"
        check_url = f"{api_root}/check-runs/{identity}"
        html_url = f"https://github.com/{observer.repo}/runs/{identity}"
        require(job.get("run_url") == f"{api_root}/actions/runs/{native['id']}" and
                job.get("url") == f"{api_root}/actions/jobs/{identity}" and job.get("check_run_url") == check_url and
                job.get("html_url") == html_url and isinstance(job.get("node_id"), str) and bool(job["node_id"]),
                "检查回显的 API、页面或节点身份不完整")
        check = mapping(observer.api.get(observer.root + f"/check-runs/{identity}"))
        app, suite = mapping(check.get("app")), mapping(check.get("check_suite"))
        require(positive(check.get("id")) and check["id"] == identity and check.get("name") == "Merge gate" and
                check.get("head_sha") == pr["head"]["sha"] and
                check.get("external_id") == f"merge-gate:v2:{observer.repo}:source:{pr['head']['sha']}" and
                type(app.get("id")) is int and app["id"] == 15368 and app.get("slug") == "github-actions" and
                check.get("url") == check_url and check.get("html_url") == html_url and check.get("node_id") == job["node_id"] and
                positive(native.get("check_suite_id")) and positive(suite.get("id")) and suite["id"] == native["check_suite_id"],
                "无法核实检查回显的同源 App、节点或原生 suite 关联")
        metadata = check_metadata(check)
        require(type(metadata.get("schema")) is int and metadata["schema"] == 1 and metadata.get("repo") == observer.repo and
                metadata.get("head") == pr["head"]["sha"])
        members, targets = metadata.get("membership"), metadata.get("targets")
        require(isinstance(members, list) and isinstance(targets, list) and len(members) == len(targets) <= 50000 and
                all(isinstance(row, dict) for row in [*members, *targets]))
        if not members:
            require(positive(metadata.get("pr")) and metadata.get("reason_code") == "source_head_intersection" and
                    metadata.get("result") == "pending" and metadata.get("failed_prs") == [] and metadata.get("pending_prs") == [] and
                    check.get("status") == "in_progress" and check.get("conclusion") is None,
                    "空历史交集必须是无失败或待定目标的 pending 发布投影")
        numbers = [member.get("pr") for member in members]
        require(all(positive(number) for number in numbers) and len(set(numbers)) == len(numbers))
        # An old source-owned publication authenticates its own cohort, not today's PRs or refs.
        for member in members:
            require(member.get("head") == check["head_sha"] and sha(member.get("base")) and type(member.get("draft")) is bool and
                    set(member) == {"pr", "head", "head_ref", "head_repo_id", "head_repo", "base_ref", "base", "draft"})
            repo_identity(dict(id=member.get("head_repo_id"), full_name=member.get("head_repo")))
            require(all(isinstance(member.get(key), str) and 0 < len(member[key]) <= 1024 and
                        not re.search(r"[\x00-\x20\x7f\ud800-\udfff]", member[key]) for key in ("head_ref", "base_ref")))
            require(sum(all(type(row.get(key)) is type(value) and row.get(key) == value for key, value in member.items())
                        for row in targets) == 1, "历史检查回显的 membership/target source 身份不一致")
        publisher = mapping(metadata.get("publisher"))
        require(publisher.get("workflow") == PUBLISHER and sha(publisher.get("workflow_sha")))
        run_id, attempt = decimal(publisher.get("run_id")), decimal(publisher.get("run_attempt"))
        issuer = mapping(observer.api.get(observer.root + f"/actions/runs/{run_id}"))
        require(positive(issuer.get("id")) and issuer["id"] == run_id and
                positive(issuer.get("run_attempt")) and issuer["run_attempt"] >= attempt)
        if issuer["run_attempt"] > attempt:
            issuer = mapping(observer.api.get(observer.root + f"/actions/runs/{run_id}/attempts/{attempt}"))
        require(issuer.get("id") == run_id and issuer.get("run_attempt") == attempt)
        publisher_run(observer, issuer, publisher["workflow_sha"], check["head_sha"])
        # This authenticates classification only, not the Check's current verdict.
    return result


def verify_evidence(observer, pr, native, proof):
    receipt = validation_receipt(observer, native, proof)
    identity = proof["identity"]
    require(type(receipt.get("schema")) is int and receipt["schema"] == 1 and receipt.get("result") == "success" and
            all(type(receipt.get(key)) is type(value) and receipt.get(key) == value for key, value in identity.items()) and
            receipt.get("source") == source(pr) and receipt.get("controller_sha", "") == proof.get("controller_sha", "") and
            isinstance(proof.get("policy_digest"), str) and re.fullmatch(r"[0-9a-f]{64}", proof["policy_digest"]) is not None and
            receipt.get("policy_digest") == proof["policy_digest"], "验证回执与此 PR/run/attempt/合并树不符")
    tasks, evidence = receipt.get("tasks"), receipt.get("evidence")
    require(isinstance(tasks, list) and 0 < len(tasks) <= 256 and isinstance(evidence, list) and len(evidence) == len(tasks))
    units = [mapping(task).get("unit") for task in tasks]
    require(all(isinstance(unit, str) and re.fullmatch(r"[A-Za-z0-9_.-]{1,128}", unit) is not None for unit in units) and
            len(set(units)) == len(units))
    observed = []
    for row in evidence:
        row = mapping(row)
        require(row.get("result") == "success" and row.get("source") == source(pr) and
                all(type(row.get(key)) is type(value) and row.get(key) == value for key, value in identity.items()),
                "逐任务证据不属于此 PR/run/attempt/合并树")
        observed.append(row.get("unit"))
    require(all(isinstance(unit, str) for unit in observed) and sorted(observed) == sorted(units))
    jobs = validation_jobs(observer, pr, native)
    expected = ["Merge plan", "Merge evidence", *["Merge tests / " + unit for unit in units]]
    require(len(jobs) == len(expected) and all(isinstance(job, dict) and isinstance(job.get("name"), str) for job in jobs) and
            sorted(job["name"] for job in jobs) == sorted(expected) and len({job.get("id") for job in jobs if positive(job.get("id"))}) == len(jobs))
    for job in jobs:
        require(job.get("status") == "completed" and job.get("conclusion") == "success" and
                reporter_origin(observer, native, job, False) == "current", "精确验证作业未在本 attempt 全部成功执行")


def latest_authority(observer, pr, target, native, members):
    authority = mapping(target.get("authority"))
    require(positive(authority.get("id")) and positive(authority.get("run_attempt")) and
            all(type(authority.get(key)) is type(native.get(key)) and authority.get(key) == native.get(key)
                for key in ("id", "run_attempt", "status", "conclusion", "event")), "逐 PR 验证权威身份不符")
    candidates = []
    for event in ("pull_request", "workflow_dispatch"):
        query = dict(event=event)
        if event == "pull_request":
            query["head_sha"] = pr["head"]["sha"]
        rows = pages(observer, observer.root + "/actions/workflows/merge-gate.yml/runs?" + urlencode(query), "workflow_runs")
        for row in rows:
            row = mapping(row)
            if row.get("path") != NATIVE or row.get("event") != event:
                continue
            require(positive(row.get("id")) and positive(row.get("run_attempt")))
            if row["id"] < native["id"]:
                continue  # Older runs cannot replace the already verified authority.
            if event == "workflow_dispatch":
                title = f"Merge validation PR {pr['number']} head {pr['head']['sha']} base {pr['base']['sha']}"
                if row.get("display_title") != title:
                    continue
            else:
                if not native_matches(row, pr, observer.repo):
                    continue
                if re.fullmatch(r"Merge metadata ignored PR [1-9][0-9]* head " + pr["head"]["sha"] + r" action (edited|labeled)", row.get("display_title", "")):
                    continue
                title = re.fullmatch(r"Merge validation PR ([1-9][0-9]*)", row.get("display_title", ""))
                linked = row.get("pull_requests", [])
                require(isinstance(linked, list))
                numbers = {mapping(item).get("number") for item in linked}
                require(all(positive(number) for number in numbers))
                owner = int(title[1]) if title else next(iter(numbers)) if len(numbers) == 1 else None
                if owner is None and len(members) > 1:
                    require(row.get("status") == "completed" and row.get("conclusion") == "success",
                            "共享 source 的最新原生运行没有唯一 PR 归属")
                    receipt = validation_receipt(observer, row)
                    owners = [member for member in members if member["pr"] == receipt.get("pr")]
                    require(len(owners) == 1 and positive(receipt.get("pr")))
                    member = owners[0]
                    require(receipt.get("repo") == observer.repo and receipt.get("head") == member["head"] and
                            receipt.get("base") == member["base"] and receipt.get("run_id") == str(row["id"]) and
                            receipt.get("run_attempt") == str(row["run_attempt"]) and receipt.get("source") == dict(
                                repo_id=member["head_repo_id"], repo=member["head_repo"], ref=member["head_ref"], base_ref=member["base_ref"]),
                            "共享 source 的回执没有唯一 PR/source/base 归属")
                    owner = receipt["pr"]
                if owner is not None and owner != pr["number"]:
                    continue
            candidates.append(row)
    require(candidates and max(row["id"] for row in candidates) == native["id"] and
            all(all(row.get(key) == native.get(key) for key in ("run_attempt", "status", "conclusion", "event", "head_sha"))
                for row in candidates if row["id"] == native["id"]),
            "存在较新或身份不明的验证运行，旧交集不可消费")


def terminal_validation_failure(observer, pr, target, members):
    authority = mapping(target.get("authority"))
    require(positive(authority.get("id")) and positive(authority.get("run_attempt")))
    run_id, attempt = authority["id"], authority["run_attempt"]
    current = mapping(observer.api.get(observer.root + f"/actions/runs/{run_id}"))
    exact = mapping(observer.api.get(observer.root + f"/actions/runs/{run_id}/attempts/{attempt}"))
    workflow_id = workflow(observer, "merge-gate.yml")
    for native in (current, exact):
        run_identity(observer, native, NATIVE, workflow_id, run_id, attempt, successful=False)
        require(native.get("status") == "completed" and native.get("conclusion") in
                ("failure", "cancelled", "timed_out", "action_required", "startup_failure"))
        require(repo_identity(native["repository"]) == repo_identity(pr["base"]["repo"]))
        if native.get("event") == "pull_request":
            require(native_matches(native, pr, observer.repo))
        else:
            require(native.get("event") == "workflow_dispatch" and native.get("head_branch") == "master" and
                    repo_identity(native.get("head_repository")) == repo_identity(pr["base"]["repo"]) and
                    native.get("display_title") == f"Merge validation PR {pr['number']} head {pr['head']['sha']} base {pr['base']['sha']}")
            master = branch(observer, "master")
            compared = mapping(observer.api.get(observer.root + f"/compare/{native['head_sha']}...{master}"))
            require(compared.get("status") in ("ahead", "identical") and
                    mapping(compared.get("merge_base_commit")).get("sha") == native["head_sha"])
    require(all(current.get(key) == exact.get(key) for key in ("event", "head_sha", "head_branch", "head_repository", "status", "conclusion")))
    require(pr.get("mergeable") is True and sha(target.get("merge_sha")) and
            target["merge_sha"] == pr.get("merge_commit_sha") and sha(target.get("tree")))
    commit = merge_commit(observer, target["merge_sha"])
    require(commit["tree"]["sha"] == target["tree"] and
            [parent["sha"] for parent in commit["parents"]] == [pr["base"]["sha"], pr["head"]["sha"]])
    latest_authority(observer, pr, target, exact, members)
    jobs = validation_jobs(observer, pr, exact)
    require(jobs and all(mapping(job).get("status") == "completed" and reporter_origin(observer, exact, job, False) == "current"
                         for job in jobs) and any(job.get("conclusion") in ("failure", "cancelled", "timed_out", "action_required", "startup_failure") for job in jobs),
            "没有属于此 PR 精确 attempt 的真实失败作业")
    return exact, jobs


def payload_for(observer, run, job, pr, check, metadata):
    require("membership" in metadata or check["status"] == "completed", "同一 source SHA 对应多个开放目标，保留未知并等待唯一目标" if
            metadata.get("reason_code") == "shared_head_multiple_targets" else "Merge gate 仍在等待，不能提前判断终态")
    identity = exact = None
    failure_kind = None
    previous = None if "membership" in metadata else native_failure(observer, pr)
    target = intersection_target(metadata, pr) if "membership" in metadata else metadata
    conclusion = target["result"]
    require(conclusion in ("success", "failure"), "此 PR 的精确目标仍在等待，不能借用兄弟目标终态")
    if conclusion == "success":
        if "membership" in metadata:
            require(target.get("verification") is True and target.get("reason_code") == "exact_target_verified")
        identity, exact = validation(observer, pr, target)
        if "membership" in metadata:
            require(target.get("merge_sha") == identity["sha"] and target.get("tree") == identity["tree"])
            latest_authority(observer, pr, target, exact, metadata["membership"])
            verify_evidence(observer, pr, exact, target["validation"])
    else:
        conflict = pr.get("mergeable") is False and pr.get("mergeable_state") == "dirty"
        if "membership" in metadata:
            require(branch(observer, pr["base"]["ref"]) == pr["base"]["sha"])
        if conflict and pr.get("merge_commit_sha") is not None:
            commit = merge_commit(observer, pr["merge_commit_sha"])
            base = branch(observer, pr["base"]["ref"])
            conflict = [parent["sha"] for parent in commit["parents"]] != [base, pr["head"]["sha"]]
        if "membership" in metadata:
            require(target.get("verification") is False)
            if target.get("reason_code") == "confirmed_conflict":
                require(conflict and target.get("merge_sha") is None and target.get("tree") is None and "authority" not in target,
                        "此 PR 冲突尚无可独立核实的终态证据")
            else:
                require(target.get("reason_code") == "terminal_validation_unsuccessful" and not conflict,
                        "此 PR 失败尚无可独立核实的终态证据")
                exact, jobs = terminal_validation_failure(observer, pr, target, metadata["membership"])
                previous = delivered_native_failure(observer, pr, target, exact, jobs)
        failure_kind = "merge_conflict" if conflict else "validation_evidence"
    prior_reports = []
    if "membership" not in metadata:
        prior_reports = observer.store.db.execute(
            "SELECT payload FROM actions WHERE repo=? AND sha=? AND kind=? AND (run_id!=? OR attempt!=?)",
            (observer.repo, pr["head"]["sha"], FAILURE, run["id"], run["run_attempt"])).fetchall()
    prior = any(json.loads(row[0]).get("pr") == pr["number"] and json.loads(row[0]).get("check_id") == check["id"] for row in prior_reports)
    investigation = conclusion == "failure" and previous is None and not prior
    message = ("已确认合并冲突；没有可声称已测试的合并树" if failure_kind == "merge_conflict" else
               "校验证据失败；此通知不声称具体测试断言失败" if failure_kind else "最终必需检查已通过；不代表发布或验收通过")
    if "membership" in metadata and conclusion == "success":
        aggregate = {"success": "通过", "failure": "失败", None: "等待"}[check.get("conclusion")]
        message = f"此 PR 的精确合并目标已验证通过；共享 source 必需检查总结果为{aggregate}；不代表发布或验收通过"
    if previous is not None or prior:
        message += "；仅更新必需检查状态，沿用既有调查，不重复派工"
    payload = dict(workflow=PUBLISHER, workflow_sha=metadata["publisher"].get("workflow_sha") or run["head_sha"],
                   event=run["event"], pr=pr["number"], pr_url=pr["html_url"],
                   source_head=pr["head"]["sha"], tested_sha=identity["sha"] if identity else None,
                   tested_tree=identity["tree"] if identity else None, status="completed", conclusion=conclusion,
                   check_id=check["id"], check_sha=check["head_sha"], check_name="Merge gate", check_url=check["html_url"], check_conclusion=check.get("conclusion"),
                   reporter_job_id=job["id"], reporter_job_name=job["name"], reporter_job_url=job["html_url"], reporter_job_conclusion=job["conclusion"],
                   failure_kind=failure_kind, investigation=investigation, tests=[], logs_pending=False, message=message)
    if previous is not None:
        payload.update(native_failure_run_id=previous["run_id"], native_failure_attempt=previous["attempt"], native_failure_action_id=previous["id"])
    if identity:
        payload.update(validation_run_id=exact["id"], validation_attempt=exact["run_attempt"], validation_event=exact["event"])
    return payload


def publish_target(observer, run, job, pr, check, metadata, deliver):
    require(parse_check(observer, run, pr, check) == metadata, "目标不属于此已认证 publisher Check")
    author = observer.store.author(observer.repo, pr["number"])
    payload = payload_for(observer, run, job, pr, check, metadata)
    current = live_pr(observer, pr["number"], repo_identity(run["repository"]))
    require(fingerprint(current) == fingerprint(pr), "PR 在校验期间变化，禁止把旧 head 归给当前作者")
    final_check = observer.api.get(observer.root + f"/check-runs/{check['id']}")
    require(parse_check(observer, run, pr, final_check) == metadata and final_check.get("id") == check["id"] and
            all(final_check.get(key) == check.get(key) for key in ("head_sha", "external_id", "status", "conclusion")),
            "Merge gate 在校验期间变化，保持未知")
    if "membership" in metadata:
        require(branch(observer, pr["base"]["ref"]) == pr["base"]["sha"], "目标 base 在校验期间变化")
        target = intersection_target(metadata, pr)
        if "authority" in target:
            authority = mapping(target["authority"])
            native = mapping(observer.api.get(observer.root + f"/actions/runs/{authority['id']}"))
            latest_authority(observer, pr, target, native, metadata["membership"])
    latest = current_check(observer, pr["head"]["sha"])
    require(latest is not None and latest["id"] == check["id"] and parse_check(observer, run, pr, latest) == metadata and
            all(latest.get(key) == final_check.get(key) for key in ("head_sha", "external_id", "status", "conclusion")),
            "Merge gate 当前代次在校验期间变化，禁止投递历史结果")
    require(author == observer.store.author(observer.repo, pr["number"]), "校验期间作者指派变化，重新核实首报回执后再投递")
    check_key = f"@merge-check-{check['id']}" + (f"-pr-{pr['number']}" if "membership" in metadata else "")
    key = Key(observer.repo, pr["head"]["sha"], run["id"], run["run_attempt"], check_key)
    binding_key = f"merge-report-target:{observer.repo}:{run['id']}:{run['run_attempt']}:{pr['number']}:{check['id']}"
    binding = {key: payload[key] for key in ("source_head", "workflow_sha", "check_sha", "tested_sha", "tested_tree", "reporter_job_id")}
    old = observer.store.meta_get(binding_key)
    require(old is None or old == binding, "此 PR/Check 已绑定其他来源或测试树，保持未知")
    if old is None:
        observer.store.meta_put(binding_key, binding)
    payload = observer.store.event(key, payload)
    kind = FAILURE if payload["investigation"] else RESULT
    recipients = [observer.coordinator]
    if author:
        recipients.append(author)
    action = observer.store.publish(key, kind, payload, recipients)
    require(isinstance(action, str), "合并发布者待办未唯一持久绑定")
    if deliver:
        observer.store.deliver_actions({action: pr["number"]}, observer.coordinator)
    if author != observer.store.author(observer.repo, pr["number"]):
        author = None
    if deliver and author:
        receipt = observer.store.db.execute("SELECT state FROM outbox WHERE action_id=? AND recipient=?", (action, author)).fetchone()
        if receipt is None or receipt[0] not in ("delivered", "acked"):
            author = None
    return dict(state="route_pending" if author is None else "completed" if deliver else "queued",
                pr=pr["number"], check_id=check["id"], reporter_job_id=job["id"], action_id=action)


def open_source_targets(observer, head):
    rows = pages(observer, observer.root + "/pulls?state=open")
    result = {}
    for row in rows:
        row = mapping(row)
        source_head, base = row.get("head"), row.get("base")
        require(isinstance(source_head, dict) and isinstance(base, dict))
        repository = base.get("repo")
        if row.get("state") != "open" or source_head.get("sha") != head or not isinstance(repository, dict) or repository.get("full_name") != observer.repo:
            continue
        require(positive(row.get("number")) and row["number"] not in result, "开放 source 目标重复或身份不完整")
        result[row["number"]] = row
    return result


def publish_intersection(observer, run, job, check, metadata, reporters, deliver):
    """A reporter authenticates a source Check, not a one-PR delivery limit."""
    live = open_source_targets(observer, check["head_sha"])
    members = metadata["membership"]
    numbers = {member["pr"] for member in members}
    repository = repo_identity(run["repository"])
    scope = f"merge-report:{run['id']}:{run['run_attempt']}:check:{check['id']}"
    results = []
    available = {row["id"]: row for number, row in reporters.items() if number in numbers}
    available[job["id"]] = job
    for member in members:
        number = member["pr"]
        target_scope = scope + f":pr:{number}"
        state_key = f"merge-report-check:{observer.repo}:{run['id']}:{run['run_attempt']}:{check['id']}:{number}"
        try:
            require(number in live, "交集目标已离开当前开放 source，保持待核实")
            validate_pr(observer, live[number], repository)
            require(target_details(live[number]) == member, "开放 source 目标快照与已认证 membership 不符")
            pr = live_pr(observer, number, repository)
            preferred = reporters.get(number, job)
            binding_key = f"merge-report-target:{observer.repo}:{run['id']}:{run['run_attempt']}:{number}:{check['id']}"
            bound = observer.store.meta_get(binding_key)
            reporter = available.get(bound["reporter_job_id"]) if bound else preferred
            require(reporter is not None, "目标原有 reporter 未在本 attempt 核实，禁止重新绑定")
            result = publish_target(observer, run, reporter, pr, check, metadata, deliver)
            observer.store.problem(target_scope, "本地作者指派缺失或冲突；保留目标待补路由" if result["state"] == "route_pending" else None)
        except (Unknown, ValueError, KeyError, TypeError) as error:
            reason = str(error) if isinstance(error, (Unknown, ValueError)) else "交集目标身份或证据形状不完整，保持未知"
            result = dict(observer.store.meta_get(state_key, {}), state="waiting", pr=number, reason=reason)
            observer.store.problem(target_scope, reason)
        observer.store.meta_put(state_key, result)
        results.append(result)
    try:
        current = open_source_targets(observer, check["head_sha"])
        complete = (set(current) == numbers and
                    all(target_details(current[member["pr"]]) == member for member in members) and
                    all(result["state"] in ("completed", "queued") for result in results))
    except (Unknown, ValueError, KeyError, TypeError):
        complete = False
    return dict(state=("completed" if deliver else "queued") if complete else "waiting", check_id=check["id"],
                reporter_job_id=job["id"], targets=results, intersection=True,
                reason=None if complete else "共享 source 成员、目标证据或作者路由尚未全部核实，保留独立重试")


def publish_report(observer, run, job, number, deliver, reporters=None):
    pr, check, metadata = discover(observer, run) if number is None else matrix_check(observer, run, number)
    if "membership" in metadata:
        return publish_intersection(observer, run, job, check, metadata, reporters or {number: job}, deliver)
    return publish_target(observer, run, job, pr, check, metadata, deliver)


def reconcile_publisher(observer, run, jobs, deliver=True):
    """Independently reconcile matrix PRs; False never rolls back healthy siblings.

    Persist waiting separately from completed/queued/route_pending for each source.
    Keep pending/unknown/route-pending sources hot. Inherited jobs never create
    actions for the viewing attempt; reconcile their original attempt separately.
    FAILURE/RESULT remain Check actions, not native job or release-ready signals.
    """
    if observer.lock is None:
        raise RuntimeError("合并发布者对账必须持有单接收进程锁")
    run = mapping(run)
    scope = f"merge-report:{run.get('id')}:{run.get('run_attempt')}"
    try:
        publisher_run(observer, run)
        require(isinstance(jobs, list) and len(jobs) <= 10000)
    except (Unknown, ValueError) as error:
        observer.store.problem(scope, str(error))
        return False
    observer.store.problem(scope)
    groups, seen, conflicting_ids = {}, {}, set()
    healthy = True
    for index, job in enumerate(jobs):
        observer.store.problem(scope + f":job:{index}")
        try:
            job = mapping(job)
            name = job.get("name")
            if name == "Resolve Merge PR identities":
                continue  # Resolution is not a PR Check result or routing grant.
            require(isinstance(name, str))
            match = re.fullmatch(r"Trusted Merge report / PR ([1-9][0-9]{0,18})", name)
            require(match is not None or name == "Trusted Merge report", "发布作业名不是规范 PR candidate")
            number = decimal(match[1]) if match else None
            groups.setdefault(number, []).append(job)
            identity = job.get("id")
            require(positive(identity))
            if identity in seen and seen[identity] != job:
                conflicting_ids.add(identity)
            seen[identity] = job
        except (Unknown, ValueError) as error:
            observer.store.problem(scope + f":job:{index}", str(error))
            healthy = False
    if not groups:
        observer.store.problem(scope, "仅有解析作业或缺少可核实的 PR reporter，不能判断所有 PR 检查通过")
        return False
    reporters = {}
    for number, rows in groups.items():
        source_scope = scope + (f":pr:{number}" if number is not None else ":legacy")
        state_key = f"merge-report-source:{observer.repo}:{run['id']}:{run['run_attempt']}:{number or 'legacy'}"
        try:
            require(number is not None or len(groups) == 1, "旧单 reporter 与矩阵混合，旧来源保持未知")
            unique = {}
            for job in rows:
                require(positive(job.get("id")) and job["id"] not in conflicting_ids)
                unique[job["id"]] = job
            current = [job for job in unique.values() if reporter_origin(observer, run, job, number is None) == "current"]
            if not current:
                observer.store.meta_put(state_key, dict(state="inherited"))
                observer.store.problem(source_scope)
                continue
            require(len(current) == 1, "同一 PR 存在多个本 attempt reporter，拒绝重复归属")
            job = current[0]
            require(job.get("status") == "completed" and job.get("conclusion") == "success", "此 PR reporter 尚未明确成功，保留独立未知")
            previous = observer.store.meta_get(state_key, {})
            require(previous.get("reporter_job_id", job["id"]) == job["id"], "此 PR attempt 的 reporter 身份变化，保持未知")
            reporters[number] = job
        except (Unknown, ValueError) as error:
            previous = observer.store.meta_get(state_key, {})
            observer.store.meta_put(state_key, dict(previous, state="waiting", reason=str(error)))
            observer.store.problem(source_scope, str(error))
            healthy = False
    for number, job in reporters.items():
        source_scope = scope + (f":pr:{number}" if number is not None else ":legacy")
        state_key = f"merge-report-source:{observer.repo}:{run['id']}:{run['run_attempt']}:{number or 'legacy'}"
        try:
            result = publish_report(observer, run, job, number, deliver, reporters)
            observer.store.meta_put(state_key, result)
            pending = result["state"] in ("waiting", "route_pending")
            reason = result.get("reason") or "本地作者指派缺失或冲突；仅交协调者，保留同源待补路由"
            observer.store.problem(source_scope, reason if pending else None)
            healthy = healthy and not pending
        except (Unknown, ValueError) as error:
            previous = observer.store.meta_get(state_key, {})
            observer.store.meta_put(state_key, dict(previous, state="waiting", reason=str(error)))
            observer.store.problem(source_scope, str(error))
            healthy = False
    return healthy
