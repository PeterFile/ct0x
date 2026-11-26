"""Read-only rc tag intake in the existing locked observer process.

The first complete, durable snapshot is a non-replaying baseline. Later ref/object
identities survive retries and restarts. A tag created and deleted entirely
between successful snapshots cannot be recovered by this polling source.
"""
import re
from urllib.parse import parse_qs, quote, urlsplit

from github import Unknown
from store import Key, now

RELEASE = ".github/workflows/release-candidate.yml"
ACTION = "release_tag_forward"
PREFIX = "refs/tags/rc/"


def require(condition, message="候选标签证据不完整，保持未知并稍后重试"):
    if not condition:
        raise Unknown(message)


def sha(value):
    return isinstance(value, str) and re.fullmatch(r"[0-9a-f]{40}", value) is not None


def object_id(value):
    require(isinstance(value, dict) and sha(value.get("sha")) and isinstance(value.get("type"), str) and
            value["type"] in {"tag", "commit", "tree", "blob"})
    return value["sha"], value["type"]


def valid_ref(value):
    require(isinstance(value, str) and value.startswith(PREFIX) and len(value) <= 1024)
    require(not re.search(r"[\x00-\x20\x7f\ud800-\udfff~^:?*\[\\]", value) and ".." not in value and "@{" not in value)
    require(all(part and not part.startswith(".") and not part.endswith((".lock", ".")) for part in value.split("/")))
    return "/" not in value[len(PREFIX):]


def next_page(link, endpoint, page):
    if link is None:
        return None
    require(isinstance(link, str) and bool(link), "候选标签分页不完整，保持未知")
    links = {}
    for part in link.split(","):
        match = re.fullmatch(r'\s*<([^<>]+)>;\s*rel="(next|prev|first|last)"\s*', part)
        require(match is not None, "候选标签分页链接无效，保持未知")
        try:
            parsed = urlsplit(match[1])
            query = parse_qs(parsed.query, strict_parsing=True, keep_blank_values=True)
        except ValueError:
            raise Unknown("候选标签分页参数无效，保持未知") from None
        require(parsed.scheme == "https" and parsed.netloc == "api.github.com" and
                parsed.path == endpoint and not parsed.fragment and set(query) == {"page"} and
                len(query["page"]) == 1 and re.fullmatch(r"[1-9][0-9]{0,5}", query["page"][0]) is not None,
                "拒绝非预期候选标签分页，保持未知")
        relation, number = match[2], int(query["page"][0])
        require(relation not in links)
        require((relation == "next" and number == page + 1) or
                (relation == "prev" and number == page - 1) or
                (relation == "first" and number == 1) or
                (relation == "last" and number >= page), "候选标签分页不连续，保持未知")
        links[relation] = (number, parsed.path + "?" + parsed.query)
    require("next" in links or "last" not in links or links["last"][0] == page,
            "候选标签分页尚未完成，保持未知")
    return links["next"][1] if "next" in links else None


def snapshot(observer):
    endpoint = observer.root + "/git/matching-refs/tags/rc/"
    path, refs, seen = endpoint, {}, set()
    for page in range(1, 501):
        items, link = observer.api.document(path)
        require(isinstance(items, list), "候选标签列表形状无效，保持未知")
        for item in items:
            require(isinstance(item, dict))
            single_level = valid_ref(item.get("ref"))
            identity = object_id(item.get("object"))
            require(item["ref"] not in seen, "候选标签分页重叠或 ref 变化，保持未知")
            seen.add(item["ref"])
            require(len(seen) <= 50000, "候选标签数量达到安全上限，保持未知")
            if single_level:
                refs[item["ref"]] = identity
        path = next_page(link, endpoint, page)
        if path is None:
            return refs
        require(bool(items), "候选标签分页为空但仍有下一页，保持未知")
    raise Unknown("候选标签分页达到安全上限，保持未知")


def schema(store):
    with store.db:
        store.db.execute("CREATE TABLE IF NOT EXISTS rc_ref_baselines (repo TEXT PRIMARY KEY, completed_at TEXT NOT NULL)")
        store.db.execute("""CREATE TABLE IF NOT EXISTS rc_ref_candidates (
            repo TEXT NOT NULL, ref TEXT NOT NULL, tag_object TEXT NOT NULL,
            object_type TEXT NOT NULL, state TEXT NOT NULL, sha TEXT, reason TEXT,
            updated_at TEXT NOT NULL, PRIMARY KEY(repo,ref,tag_object))""")


def remember(store, repo, refs):
    # Never commit seen identities separately from their retryable candidate rows.
    with store.db:
        store.db.execute("BEGIN IMMEDIATE")
        baseline = store.db.execute("SELECT 1 FROM rc_ref_baselines WHERE repo=?", (repo,)).fetchone() is None
        for ref, (obj, kind) in refs.items():
            old = store.db.execute("SELECT object_type FROM rc_ref_candidates WHERE repo=? AND ref=? AND tag_object=?",
                                   (repo, ref, obj)).fetchone()
            require(old is None or old[0] == kind, "同一候选标签 object 类型冲突，保持未知")
            store.db.execute("INSERT OR IGNORE INTO rc_ref_candidates VALUES(?,?,?,?,?,NULL,NULL,?)",
                             (repo, ref, obj, kind, "baseline" if baseline else "pending", now()))
        if baseline:
            store.db.execute("INSERT INTO rc_ref_baselines VALUES(?,?)", (repo, now()))
    return baseline


def state(store, row, value, reason=None, target=None):
    with store.db:
        result = store.db.execute("UPDATE rc_ref_candidates SET state=?,reason=?,sha=COALESCE(sha,?),updated_at=? "
                                  "WHERE repo=? AND ref=? AND tag_object=? AND (? IS NULL OR sha IS NULL OR sha=?)",
                                  (value, reason, target, now(), row["repo"], row["ref"], row["tag_object"], target, target))
        require(result.rowcount == 1, "候选标签已绑定的目标 SHA 冲突，保持未知")


def current_ref(observer, row):
    item = observer.api.get(observer.root + "/git/ref/" + quote(row["ref"][5:], safe="/"))
    require(isinstance(item, dict) and item.get("ref") == row["ref"] and
            object_id(item.get("object")) == (row["tag_object"], row["object_type"]),
            "当前候选标签 ref 或原始 object 不匹配，保持未知")


def peel(observer, row):
    obj, kind, visited = row["tag_object"], row["object_type"], set()
    for depth in range(17):
        require(obj not in visited, "候选标签 peel 出现循环，保持未知")
        if kind == "commit":
            return obj
        require(kind == "tag" and depth < 16, "候选标签 peel 类型或深度超出安全范围，保持未知")
        visited.add(obj)
        item = observer.api.get(observer.root + "/git/tags/" + obj)
        require(isinstance(item, dict) and item.get("sha") == obj)
        obj, kind = object_id(item.get("object"))
    raise Unknown("候选标签 peel 未完成，保持未知")


def master_sha(observer):
    item = observer.api.get(observer.root + "/git/ref/heads/master")
    require(isinstance(item, dict) and item.get("ref") == "refs/heads/master")
    obj, kind = object_id(item.get("object"))
    require(kind == "commit")
    return obj


def on_master(observer, target, master):
    item = observer.api.get(observer.root + f"/compare/{target}...{master}")
    require(isinstance(item, dict) and isinstance(item.get("base_commit"), dict) and
            item["base_commit"].get("sha") == target and isinstance(item.get("merge_base_commit"), dict))
    merge_base = item["merge_base_commit"].get("sha")
    ahead, behind, status = item.get("ahead_by"), item.get("behind_by"), item.get("status")
    require(sha(merge_base) and type(ahead) is int and type(behind) is int and ahead >= 0 and behind >= 0)
    if status == "identical":
        require(target == master == merge_base and ahead == behind == 0)
        return True
    require(target != master)
    if status == "ahead":
        require(merge_base == target and ahead > 0 and behind == 0)
        return True
    if status == "behind":
        require(merge_base == master and ahead == 0 and behind > 0)
        return False
    require(status == "diverged" and merge_base not in {target, master} and ahead > 0 and behind > 0)
    return False


def native_absence(observer, target):
    commit = observer.api.get(observer.root + "/git/commits/" + target)
    require(isinstance(commit, dict) and commit.get("sha") == target and isinstance(commit.get("tree"), dict)
            and sha(commit["tree"].get("sha")))
    tree = root_tree = commit["tree"]["sha"]
    parts, walked = RELEASE.split("/"), []
    for index, component in enumerate(parts):
        item = observer.api.get(observer.root + "/git/trees/" + tree)
        require(isinstance(item, dict) and item.get("sha") == tree and item.get("truncated") is False
                and isinstance(item.get("tree"), list), "候选标签 tree 身份或完整性无法核实，保持未知")
        entries = {}
        for child in item["tree"]:
            require(isinstance(child, dict) and isinstance(child.get("path"), str))
            name = child["path"]
            require(name and name not in {".", ".."} and "/" not in name and name not in entries
                    and not re.search(r"[\x00\ud800-\udfff]", name))
            _, kind = object_id(child)
            require(isinstance(child.get("mode"), str) and
                    child["mode"] in {"tree": {"040000"}, "blob": {"100644", "100755", "120000"},
                                      "commit": {"160000"}}.get(kind, set()))
            entries[name] = child
        child = entries.get(component)
        walked.append({"tree": tree, "entry": component, "object": child["sha"] if child else None})
        if child is None:
            return dict(verified=True, sha=target, tree=root_tree, path=RELEASE,
                        missing_path="/".join(parts[:index + 1]), trees=walked)
        if index == len(parts) - 1:
            require(child["type"] == "blob" and child["mode"] in {"100644", "100755"},
                    "原生候选工作流路径不是可核实的普通文件，保持未知")
            return None
        require(child["type"] == "tree", "原生候选工作流父路径不是 tree，保持未知")
        tree = child["sha"]
    raise Unknown("原生候选工作流路径未完成核实，保持未知")


def inspect_candidate(observer, row):
    current_ref(observer, row)
    target = peel(observer, row)
    master = master_sha(observer)
    if not on_master(observer, target, master):
        return "outside_master", target, None
    proof = native_absence(observer, target)
    if proof is None:
        return "native_workflow", target, None
    # Revalidate mutable refs after immutable object/tree evidence, before fanout.
    current_ref(observer, row)
    require(master_sha(observer) == master, "master 在候选标签核实期间变化，保持未知并重试")
    payload = dict(tag=row["ref"], tag_object=row["tag_object"], tag_object_type=row["object_type"],
                   source_head=target, tested_sha=None, pr=None, workflow=RELEASE,
                   master_sha=master, native_workflow_absent=True, native_workflow_absence=proof,
                   title="旧提交候选标签待转发",
                   message="目标已核实缺少原生候选工作流；请发布消费者重新核实后续办，观察器不执行发布")
    return "queued", target, payload


def source_delivery(store, key, coordinator, deliver):
    # Store.deliver's run scope cannot distinguish NULL-run tag sources. Keep the
    # exact job/ref/object namespace and recipient in this local transaction.
    with store.db:
        store.db.execute("BEGIN IMMEDIATE")
        rows = store.db.execute(
            "SELECT o.receipt_id,o.state,a.payload FROM outbox o JOIN actions a ON a.id=o.action_id "
            "WHERE a.repo=? AND a.sha=? AND a.run_id IS NULL AND a.attempt IS NULL "
            "AND a.job=? AND a.kind=? AND o.recipient=?",
            (key.repo, key.sha, key.job, ACTION, coordinator)).fetchall()
        require(len(rows) == 1, "候选标签持久待办未唯一绑定，保持未知")
        row = rows[0]
        if row["state"] != "pending":
            return True
        if not deliver:
            return False
        stamp = now()
        store.db.execute("INSERT OR IGNORE INTO inbox(receipt_id,recipient,payload,delivered_at,acked_at) VALUES(?,?,?,?,NULL)",
                         (row["receipt_id"], coordinator, row["payload"], stamp))
        store.db.execute("UPDATE outbox SET state='delivered',delivered_at=? WHERE receipt_id=? AND state='pending'",
                         (stamp, row["receipt_id"]))
    return True


def reconcile_tags(observer, deliver=True):
    """Return health; persist unknowns and only deliver individually verified tags.

    Call once per reconcile under the existing Observer receiver lock, even if
    workflow run discovery failed. No run identity is fabricated for tag intake.
    """
    if observer.lock is None:
        raise RuntimeError("候选标签对账必须持有单接收进程锁")
    store = observer.store
    schema(store)
    try:
        identity = observer.api.get("/user", conditional=False)
        require(isinstance(identity, dict) and type(identity.get("id")) is int and identity["id"] > 0
                and isinstance(identity.get("login"), str) and bool(identity["login"].strip()),
                "缺少已认证身份，候选标签保持未知")
        refs = snapshot(observer)
        baseline = remember(store, observer.repo, refs)
        store.problem("rc-refs")
    except Unknown as error:
        store.problem("rc-refs", str(error))
        return False
    if baseline:
        return True
    healthy = True
    rows = store.db.execute("SELECT * FROM rc_ref_candidates WHERE repo=? AND state NOT IN ('baseline','forwarded') "
                            "ORDER BY ref,tag_object", (observer.repo,)).fetchall()
    for row in rows:
        scope = f"rc-tag:{row['ref']}:{row['tag_object']}"
        try:
            if refs.get(row["ref"]) != (row["tag_object"], row["object_type"]):
                state(store, row, "not_current", "当前完整 refs 快照不含此 ref/object；保留记录并等待恢复")
            else:
                outcome, target, payload = inspect_candidate(observer, row)
                if payload is not None:
                    key = Key(observer.repo, target, None, None, f"@tag:{row['ref']}:{row['tag_object']}")
                    # Bind the peeled target before publication, including the
                    # crash window before the final candidate-state update.
                    state(store, row, "pending", target=target)
                    store.publish(key, ACTION, payload, [observer.coordinator])
                    if source_delivery(store, key, observer.coordinator, deliver):
                        outcome = "forwarded"
                state(store, row, outcome, target=target)
            store.problem(scope)
        except Unknown as error:
            state(store, row, "unknown", str(error))
            store.problem(scope, str(error))
            healthy = False
    return healthy
