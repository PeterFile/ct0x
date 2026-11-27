"""Read-only first-owner proof and local reconciliation of ancillary release failures."""
import json
import re

from github import Unknown
from merge_reports import pages, positive, sha, timestamp
from store import encode

RELEASE = ".github/workflows/release-candidate.yml"
DUPLICATE = "Release duplicate verdict"
CONTEXT = "release-candidate/first-owner-v1"
CREATOR = {"login": "github-actions[bot]", "id": 41898282, "type": "Bot"}
PROVENANCE = "native-release-owner-v1"
DIAGNOSTIC = "重复候选未放行；请查看原 owner：canonical owner 尚无成功终态"
PRODUCTS = {"Release Full", "Release clients", "Release candidate binary", "Release machine evidence",
            "Release Full evidence", "Release candidate evidence", "Release candidate verdict"}
FULL_PATHS = {".github/workflows/ci-full.yml"} | {".github/scripts/" + name for name in (
    "ci-plan.py", "ci-shards.py", "ci-gate.py", "verify-store-shard.py", "test-ci-scheduling.py", "test-store-shard.py")}
REQUIRED = FULL_PATHS | {RELEASE, ".github/workflows/release-receipt.yml",
                         ".github/scripts/release-core.py", ".github/scripts/release-control.py"}
PROOF_IDS = ("run_id", "run_attempt", "owner_run_id", "owner_attempt", "status_id", "status_creator_id", "admission_job_id", "duplicate_job_id", "claim_job_id", "intent_artifact_id")
PROOF_SHAS = ("candidate_sha", "candidate_tree", "workflow_sha", "owner_workflow_sha", "owner_source_tree", "source_tree")
PROOF_FIELDS = ("schema", "provenance", "admitted", "owner_url", "owner_status", "owner_conclusion", "status_created_at", "intent_digest") + PROOF_IDS + PROOF_SHAS
RUN_FIELDS = ("id", "run_attempt", "workflow_id", "path", "head_sha", "head_commit", "event", "head_branch", "run_started_at")
JOB_FIELDS = ("id", "run_id", "run_attempt", "head_sha", "name", "status", "conclusion", "started_at", "completed_at", "html_url")


class APIUnavailable(Unknown):
    """A read failed, as opposed to a contradiction in returned evidence."""


class ProofAPI:
    def __init__(self, api):
        self.api = api

    def __getattr__(self, name):
        method = getattr(self.api, name)
        if not callable(method):
            return method
        def read(*args, **kwargs):
            try:
                return method(*args, **kwargs)
            except Unknown as error:
                raise APIUnavailable(str(error), status=error.status, retry_at=error.retry_at) from error
        return read


def cache_key(observer, run):
    return f"release-duplicate-proof:{observer.repo}:{run['id']}:{run['run_attempt']}"


def repository_identity(run):
    return [{k: (run.get(side) or {}).get(k) for k in ("id", "full_name")} for side in ("repository", "head_repository")]


def same_job(left, right):
    return all(left.get(k) == right.get(k) for k in JOB_FIELDS)


def matches_snapshot(snapshot, run, jobs):
    return (isinstance(snapshot, dict) and isinstance(snapshot.get("run"), dict) and isinstance(snapshot.get("jobs"), list) and
            isinstance(snapshot.get("proof"), dict) and all(snapshot["run"].get(k) == run.get(k) for k in RUN_FIELDS) and
            repository_identity(snapshot["run"]) == repository_identity(run) and all(any(same_job(j, old) for old in snapshot["jobs"]) for j in jobs))


def require(condition, message="重复发布的原生 owner、来源或作业证据未核实；保留普通失败"):
    if not condition:
        raise Unknown(message)


def native_source(observer, run, workflow_id, target, sources=None):
    """Bind published controller blobs to the native commit, not an artifact assertion."""
    require(run.get("path") == RELEASE and run.get("workflow_id") == workflow_id and positive(run.get("id")) and
            run.get("repository", {}).get("full_name") == observer.repo and
            run.get("head_repository", {}).get("full_name") == observer.repo)
    head = run.get("head_commit") or {}
    control = head.get("id")
    require(sha(control) and sha(run.get("head_sha")))
    if run.get("event") == "workflow_dispatch":
        require(run.get("head_branch") == "master" and run["head_sha"] == control)
    else:
        from rc_refs import valid_ref
        require(run.get("event") == "push" and valid_ref("refs/tags/" + run.get("head_branch", "")) and control == target["sha"])
        raw, seen = run["head_sha"], set()
        while raw != control:
            require(raw not in seen and len(seen) < 8)
            seen.add(raw)
            tag = observer.api.get(observer.root + "/git/tags/" + raw)
            require(tag.get("sha") == raw and isinstance(tag.get("object"), dict))
            obj = tag["object"]
            require(sha(obj.get("sha")) and obj.get("type") in {"tag", "commit"})
            raw = obj["sha"]
            require(obj["type"] != "commit" or raw == control)
    commit = observer.api.get(observer.root + "/git/commits/" + control)
    tree = commit.get("tree", {}).get("sha")
    require(commit.get("sha") == control and sha(tree) and head.get("tree_id") == tree)
    observer.contains(control, observer.master_sha())
    candidate = observer.api.get(observer.root + "/git/commits/" + target["sha"])
    require(candidate.get("sha") == target["sha"] and candidate.get("tree", {}).get("sha") == target["tree"])
    observer.contains(target["sha"], observer.master_sha())
    rows = observer.api.get(observer.root + "/git/trees/" + tree + "?recursive=1")
    require(rows.get("sha") == tree and rows.get("truncated") is False and isinstance(rows.get("tree"), list))
    blobs, seen = {}, set()
    for row in rows["tree"]:
        path = row.get("path")
        require(isinstance(path, str) and path not in seen)
        seen.add(path)
        if path in REQUIRED or path.startswith(".github/scripts/release-"):
            require(row.get("type") == "blob" and row.get("mode") in {"100644", "100755"} and sha(row.get("sha")))
            blobs[path] = row["sha"]
    require(REQUIRED <= blobs.keys() and (sources is None or sources == blobs))
    return control, tree, candidate


def current_job(observer, run, jobs, name, conclusion):
    matches = [j for j in jobs if j.get("name") == name]
    require(len(matches) == 1)
    job = matches[0]
    require(positive(job.get("id")) and type(job.get("run_id")) is int and job["run_id"] == run["id"] and
            type(job.get("run_attempt")) is int and job["run_attempt"] == run["run_attempt"] and
            job.get("status") == "completed" and job.get("conclusion") == conclusion and
            job.get("started_at") is not None and observer.origin(run, job) == "current")
    return job


def compatible_history(observer, run, duplicate_job_id):
    rows = observer.store.db.execute("SELECT job,kind,payload FROM actions WHERE repo=? AND run_id=? AND attempt=?",
                                    (observer.repo, run["id"], run["run_attempt"])).fetchall()
    for row in rows:
        if row["kind"] not in {"first_failure", "terminal", "job_details"}:
            return False
        if row["kind"] == "job_details" and row["job"] not in {str(duplicate_job_id), "@target"}:
            return False
        saved = json.loads(row["payload"])
        if saved.get("conclusion") != "failure" or saved.get("source_head") != run["head_sha"] or saved.get("workflow") != RELEASE:
            return False
        if row["kind"] == "first_failure" and (row["job"] != str(duplicate_job_id) or saved.get("job_name") != DUPLICATE):
            return False
    return True


def verify(observer, context, supplied):
    run = context["run"]
    require(run["path"] == RELEASE and (run.get("status") == "completed" and run.get("conclusion") == "failure" or
            run.get("status") in {"queued", "in_progress", "waiting", "pending", "requested"} and run.get("conclusion") is None))
    observer.validate_run(run, run["id"], run["run_attempt"])
    native = observer.api.get(observer.root + f"/actions/runs/{run['id']}/attempts/{run['run_attempt']}")
    require(all(native.get(k) == run.get(k) for k in RUN_FIELDS + ("status", "conclusion")) and repository_identity(native) == repository_identity(run))
    jobs = observer.api.pages(observer.root + f"/actions/runs/{run['id']}/attempts/{run['run_attempt']}/jobs", "jobs")
    require(len({j.get("id") for j in jobs}) == len(jobs) and
            all(any(same_job(j, native) for native in jobs) for j in supplied))
    intent = observer.receipt(run, "release-intent", jobs)
    require(isinstance(intent.get("sources"), dict))
    control, tree, candidate = native_source(observer, run, run["workflow_id"], intent, intent.get("sources"))
    require(intent.get("workflow_sha") == control == intent.get("policy_sha") and intent.get("policy") == "caller-policy-v1" and
            intent.get("head") == intent["sha"] and intent.get("base") == (candidate.get("parents") or [{"sha": intent["sha"]}])[0]["sha"])
    observer.contains(intent.get("master_sha"), observer.master_sha())
    observer.contains(intent["sha"], intent["master_sha"])
    observer.contains(control, intent["master_sha"])
    claim = current_job(observer, run, jobs, "Release admission " + intent["sha"], "success")
    duplicate = current_job(observer, run, jobs, DUPLICATE, "failure")
    names = [j.get("name") for j in jobs]
    require(len(names) == len(set(names)) and set(names) == PRODUCTS | {"Release normalize", claim["name"], DUPLICATE})
    for job in jobs:
        require(job.get("run_id") == run["id"] and type(job.get("run_attempt")) is int and job["run_attempt"] == run["run_attempt"] and
                job.get("head_sha") == run["head_sha"] and positive(job.get("id")) and
                job.get("html_url") == f"https://github.com/{observer.repo}/actions/runs/{run['id']}/job/{job['id']}")
        if job["name"] in PRODUCTS:
            require(job.get("status") == "completed" and job.get("conclusion") == "skipped")
    for attempt in range(1, run["run_attempt"]):
        previous = observer.api.pages(observer.root + f"/actions/runs/{run['id']}/attempts/{attempt}/jobs", "jobs")
        require(bool(previous) and all(j.get("name") in {"Release normalize", claim["name"], DUPLICATE} or
                j.get("status") == "completed" and j.get("conclusion") == "skipped" for j in previous))
    require(compatible_history(observer, run, duplicate["id"]))
    statuses = observer.api.pages(observer.root + "/commits/" + intent["sha"] + "/statuses")
    statuses = [s for s in statuses if s.get("context") == CONTEXT]
    require(bool(statuses) and all(positive(s.get("id")) for s in statuses) and len({s["id"] for s in statuses}) == len(statuses))
    statuses.sort(key=lambda s: s["id"])
    owner_id, admission, owner_control, owner_tree = None, None, None, None
    for status in statuses:
        require(all(status.get("creator", {}).get(k) == v for k, v in CREATOR.items()) and
                (status.get("state"), status.get("description")) in {
                    ("success", "首个 owner 已登记；尚未验证候选"), ("pending", "release-owner-v1")})
        match = re.fullmatch(re.escape(f"https://github.com/{observer.repo}/actions/runs/") + r"([1-9][0-9]*)", status.get("target_url", ""))
        require(match is not None and positive(int(match[1])))
        identity = int(match[1])
        require(identity != run["id"] and owner_id in {None, identity})
        owner_id = identity
        original = observer.api.get(observer.root + f"/actions/runs/{owner_id}/attempts/1")
        require(original.get("run_attempt") == 1 and original.get("id") == owner_id)
        owner_control, owner_tree, _ = native_source(observer, original, run["workflow_id"], intent)
        owner_jobs = pages(observer, observer.root + f"/actions/runs/{owner_id}/attempts/1/jobs", "jobs")
        current_job(observer, original, owner_jobs, "Release normalize", "success")
        matches = [j for j in owner_jobs if j.get("name") == "Release admission " + intent["sha"]]
        require(len(matches) == 1)
        admission = matches[0]
        require(positive(admission.get("id")) and admission.get("run_id") == owner_id and admission.get("run_attempt") == 1 and
                observer.origin(original, admission) == "current")
        started, created = timestamp(admission.get("started_at")), timestamp(status.get("created_at"))
        require(started <= created <= timestamp(claim["completed_at"]))
        require((admission.get("status"), admission.get("conclusion")) in {("in_progress", None), ("completed", "success")} and
                (created <= timestamp(admission["completed_at"]) if admission.get("completed_at") else admission["status"] == "in_progress"))
    owner = observer.api.get(observer.root + f"/actions/runs/{owner_id}")
    require(owner.get("id") == owner_id and positive(owner.get("run_attempt")) and repository_identity(owner) == repository_identity(original) and all(
        owner.get(k) == original.get(k) for k in ("workflow_id", "path", "event", "head_sha", "head_commit", "head_branch")))
    require(owner.get("status") in {"queued", "in_progress", "waiting", "pending", "requested", "completed"} and
            (owner.get("conclusion") is None if owner["status"] != "completed" else owner.get("conclusion") in {"success", "failure", "cancelled", "timed_out", "action_required", "startup_failure", "neutral", "skipped", "stale"}))
    if owner.get("conclusion") == "success":
        previous = observer.store.meta_get(cache_key(observer, run))
        require(matches_snapshot(previous, run, jobs) and previous["proof"].get("owner_run_id") == owner_id,
                "owner 已成功，且没有此前已核实的 duplicate fail-closed；保留普通失败")
    seen, abnormal = 0, False
    def line(value):
        nonlocal seen, abnormal
        value = re.sub(r"^\d{4}-\d\d-\d\dT[0-9:.]+Z ", "", value).strip()
        if value == DIAGNOSTIC:
            seen += 1
        elif re.search(r"Traceback|\bError:|::error::|##\[error\]|panic:|fatal:", value, re.I):
            abnormal |= value != "##[error]Process completed with exit code 1."
    require(observer.api.lines(observer.root + f"/actions/jobs/{duplicate['id']}/logs", line, timeout=1) and seen == 1 and not abnormal,
            "重复 verdict 不是明确的 canonical 未成功 fail-closed；保留普通失败")
    target = observer.store.target(observer.repo, run["id"], run["run_attempt"])
    context["base"].update(tested_sha=intent["sha"], tested_tree=intent["tree"], workflow_sha=control, target_unknown=False, provenance="release-intent")
    proof = dict(schema=1, provenance=PROVENANCE, admitted=False, run_id=run["id"], run_attempt=run["run_attempt"],
                candidate_sha=intent["sha"], candidate_tree=intent["tree"], workflow_sha=control, status_creator_id=CREATOR["id"],
                owner_run_id=owner_id, owner_attempt=owner["run_attempt"],
                owner_url=f"https://github.com/{observer.repo}/actions/runs/{owner_id}", owner_status=owner["status"], owner_conclusion=owner.get("conclusion"),
                status_id=statuses[0]["id"], status_created_at=statuses[0]["created_at"], admission_job_id=admission["id"],
                owner_workflow_sha=owner_control, owner_source_tree=owner_tree, source_tree=tree, duplicate_job_id=duplicate["id"],
                claim_job_id=claim["id"], intent_artifact_id=target["artifact_id"], intent_digest=target["digest"])
    snapshot = dict(run=run, jobs=jobs, proof=proof)
    observer.store.meta_put(cache_key(observer, run), snapshot)
    context["release_duplicate_snapshot"] = snapshot
    return proof


def classify(observer, context, jobs):
    if context["run"]["path"] != RELEASE or not any(j.get("name") == DUPLICATE and j.get("conclusion") == "failure" for j in jobs):
        return
    if "release_duplicate_checked" in context:
        if not context.get("release_duplicate") or matches_snapshot(context.get("release_duplicate_snapshot"), context["run"], jobs):
            return
        for key in ("release_duplicate", "release_duplicate_pending", "release_duplicate_snapshot"):
            context.pop(key, None)
    context["release_duplicate_checked"] = True
    scope = f"release-duplicate:{context['run']['id']}:{context['run']['run_attempt']}"
    api, observer.api = observer.api, ProofAPI(observer.api)
    try:
        context["release_duplicate"] = verify(observer, context, jobs)
        observer.store.problem(scope)
        run = context["run"]
        for kind in ("target", "attribution", "origin", "conclusion"):
            observer.store.problem(f"{kind}:{run['id']}:{run['run_attempt']}")
        observer.store.problem(f"logs:{run['id']}:{run['run_attempt']}:{context['release_duplicate']['duplicate_job_id']}")
    except (Unknown, ValueError, KeyError, TypeError, AttributeError) as error:
        observer.store.problem(scope, str(error) if isinstance(error, Unknown) else "重复发布证明形状无效；保留普通失败")
        snapshot = observer.store.meta_get(cache_key(observer, context["run"]))
        if (isinstance(error, APIUnavailable) and matches_snapshot(snapshot, context["run"], jobs) and
                compatible_history(observer, context["run"], snapshot["proof"]["duplicate_job_id"])):
            target = observer.store.target(observer.repo, context["run"]["id"], context["run"]["run_attempt"])
            context["base"].update(tested_sha=target["sha"], tested_tree=target["tree"], workflow_sha=target["workflow_sha"], target_unknown=False, provenance="release-intent")
            context.update(release_duplicate=snapshot["proof"], release_duplicate_snapshot=snapshot, release_duplicate_pending=True)
    finally:
        observer.api = api


def reconcile_terminal(store, key, payload, recipients):
    """Keep audit rows and receipt IDs, retire only this proven duplicate's local work."""
    action_id = store.publish(key, "terminal", payload, recipients)
    with store.db:
        raw = store.db.execute("SELECT payload FROM actions WHERE id=?", (action_id,)).fetchone()[0]
        value = encode(dict(json.loads(raw), **payload))
        changed = value != raw
        if changed:
            store.db.execute("UPDATE actions SET payload=? WHERE id=?", (value, action_id))
            store.db.execute("UPDATE inbox SET payload=? WHERE receipt_id IN (SELECT receipt_id FROM outbox WHERE action_id=?)", (value, action_id))
        rows = store.db.execute("SELECT o.receipt_id FROM actions a JOIN outbox o ON o.action_id=a.id WHERE a.repo=? AND a.run_id=? AND a.attempt=? AND a.kind IN ('first_failure','job_details')",
                                (key.repo, key.run_id, key.attempt)).fetchall()
        for row in rows:
            store.db.execute("INSERT OR IGNORE INTO meta VALUES(?,?)", ("release-duplicate-superseded:" + row[0], encode(action_id)))
        if store.db.execute("SELECT 1 FROM sqlite_master WHERE type='table' AND name='codex_receiver_receipts'").fetchone():
            store.db.executemany("UPDATE codex_receiver_receipts SET state='information',message=NULL WHERE receipt_id=? AND state IN ('prepared','invalid','information') AND attempting_at IS NULL", [(r[0],) for r in rows])
            if changed:
                columns = {r[1] for r in store.db.execute("PRAGMA table_info(codex_receiver_receipts)")}
                version = ",projection_version=1" if "projection_version" in columns else ""
                store.db.execute("UPDATE codex_receiver_receipts SET state='information',message=NULL" + version + " WHERE action_id=? AND state IN ('prepared','invalid','information') AND attempting_at IS NULL", (action_id,))
    return action_id


def public_proof(payload):
    """An arbitrary investigation=false flag is never informational authority."""
    proof = payload.get("release_duplicate")
    if not isinstance(proof, dict):
        return None
    valid = (payload.get("action") == "terminal" and payload.get("job") == "@terminal" and payload.get("workflow") == RELEASE and
             payload.get("conclusion") == "failure" and payload.get("target_unknown") is False and payload.get("provenance") == "release-intent" and
             sha(payload.get("tested_sha")) and sha(payload.get("tested_tree")) and sha(payload.get("workflow_sha")) and
             type(proof.get("schema")) is int and proof["schema"] == 1 and proof.get("admitted") is False and
             proof.get("provenance") == PROVENANCE and all(positive(proof.get(k)) for k in PROOF_IDS) and
             proof["run_id"] == payload.get("run_id") and proof["run_attempt"] == payload.get("attempt") and proof["status_creator_id"] == CREATOR["id"] and
             all(proof.get(k) == payload.get(p) for k, p in (("candidate_sha", "tested_sha"), ("candidate_tree", "tested_tree"), ("workflow_sha", "workflow_sha"))) and
             proof["owner_run_id"] != payload.get("run_id") and all(sha(proof.get(k)) for k in PROOF_SHAS) and
             isinstance(payload.get("failed_jobs"), list) and all(isinstance(j, dict) for j in payload["failed_jobs"]) and
             [j.get("job_id") for j in payload["failed_jobs"]] == [proof["duplicate_job_id"]] and
             proof.get("owner_url") == f"https://github.com/{payload.get('repo')}/actions/runs/{proof['owner_run_id']}" and
             isinstance(proof.get("intent_digest"), str) and re.fullmatch(r"sha256:[0-9a-f]{64}", proof["intent_digest"]) and
             isinstance(proof.get("owner_status"), str) and proof["owner_status"] in {"queued", "in_progress", "waiting", "pending", "requested", "completed"} and
             (proof.get("owner_conclusion") is None if proof["owner_status"] != "completed" else
              isinstance(proof.get("owner_conclusion"), str) and proof["owner_conclusion"] in {"success", "failure", "cancelled", "timed_out", "action_required", "startup_failure", "neutral", "skipped", "stale"}))
    if not valid:
        return None
    try:
        timestamp(proof.get("status_created_at"))
    except Unknown:
        return None
    return {k: proof.get(k) for k in PROOF_FIELDS}
