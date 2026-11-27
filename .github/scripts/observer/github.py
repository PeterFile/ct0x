"""Bounded read-only REST through gh; credentials never leave the CLI."""
from dataclasses import dataclass
import io
import json
import os
import re
import selectors
import subprocess
import time
from urllib.parse import parse_qs, urlsplit


class Unknown(Exception):
    """No new authoritative evidence; retain state and reconcile later."""

    def __init__(self, message, status=None, retry_at=0):
        super().__init__(message)
        self.status = status
        self.retry_at = retry_at


@dataclass
class Reply:
    status: int
    headers: dict
    body: bytes


def run_gh(args, timeout=20, max_bytes=24 * 1024 * 1024, consume=None):
    env = os.environ.copy()
    env.pop("GH_DEBUG", None)
    env.pop("GH_FORCE_TTY", None)
    env.update(GH_PROMPT_DISABLED="1", GH_PAGER="cat", NO_COLOR="1")
    process = subprocess.Popen(args, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                               stderr=subprocess.DEVNULL, env=env)
    data = bytearray()
    size = 0
    deadline = time.monotonic() + timeout
    try:
        with selectors.DefaultSelector() as selector:
            selector.register(process.stdout, selectors.EVENT_READ)
            while True:
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    raise TimeoutError
                if not selector.select(remaining):
                    raise TimeoutError
                chunk = os.read(process.stdout.fileno(), 65536)
                if not chunk:
                    break
                size += len(chunk)
                if size > max_bytes:
                    raise Unknown("API 响应超出安全大小，保持未知")
                if consume:
                    consume(chunk)
                else:
                    data.extend(chunk)
        process.wait(timeout=max(0.01, deadline - time.monotonic()))
        return process.returncode, bytes(data)
    finally:
        if process.poll() is None:
            process.kill()
        process.wait()
        process.stdout.close()


class GhTransport:
    def command(self, path, etag=None):
        if not (path == "/user" or path.startswith("/repos/")) or not re.fullmatch(r"/[A-Za-z0-9_./?=&%+:-]+", path):
            raise Unknown("拒绝非预期 API 路径")
        if ".." in path.split("/"):
            raise Unknown("拒绝非预期 API 路径")
        args = ["gh", "api", "--hostname", "github.com", "--method", "GET", "--include",
                "--header", "Accept: application/vnd.github+json",
                "--header", "X-GitHub-Api-Version: 2022-11-28"]
        if etag:
            if len(etag) > 256 or any(ord(c) < 32 or ord(c) > 126 for c in etag):
                raise Unknown("ETag 无效")
            args += ["--header", "If-None-Match: " + etag]
        return args + [path]

    def __call__(self, path, etag=None, max_bytes=None):
        args = self.command(path, etag)
        try:
            limit = 128 * 1024 * 1024 if re.search(r"/actions/artifacts/[0-9]+/zip$", path) else 24 * 1024 * 1024
            code, raw = run_gh(args, timeout=5, max_bytes=max_bytes + 65536 if max_bytes is not None else limit)
        except (OSError, TimeoutError, subprocess.TimeoutExpired):
            raise Unknown("GitHub CLI 不可用或 API 请求超时，保持未知") from None
        head, separator, body = raw.partition(b"\r\n\r\n")
        if not separator:
            head, separator, body = raw.partition(b"\n\n")
        lines = head.splitlines()
        match = re.fullmatch(rb"HTTP/\d+(?:\.\d+)? (\d{3})(?: .*)?", lines[0]) if lines else None
        if not separator or not match or len(head) > 65536:
            raise Unknown("GitHub CLI 未返回完整 HTTP 响应，保持未知")
        status = int(match[1])
        if code and status < 300:
            raise Unknown("GitHub CLI 响应传输不完整，保持未知")
        headers = {}
        for line in lines[1:]:
            key, sep, value = line.partition(b":")
            if sep and key.lower() in (b"etag", b"link", b"retry-after", b"x-ratelimit-remaining", b"x-ratelimit-reset"):
                headers[key.decode().lower()] = value.strip().decode("ascii", errors="replace")
        return Reply(status, headers, body)

    def stream(self, path, consume, timeout, limit):
        header, pending = bytearray(), bytearray()
        status, complete, dropping = None, True, False
        def chunk(data):
            nonlocal status, dropping, complete
            if status is None:
                header.extend(data)
                end = header.find(b"\r\n\r\n")
                width = 4
                if end < 0:
                    end, width = header.find(b"\n\n"), 2
                if end < 0:
                    if len(header) > 65536:
                        raise Unknown("HTTP 响应头超限")
                    return
                if end > 65536:
                    raise Unknown("HTTP 响应头超限")
                match = re.fullmatch(rb"HTTP/\d+(?:\.\d+)? (\d{3})(?: .*)?", header.splitlines()[0])
                if not match:
                    raise Unknown("日志 HTTP 响应头无效")
                status = int(match[1])
                data = bytes(header[end + width:])
                header.clear()
            if status != 200:
                return
            pending.extend(data)
            while b"\n" in pending:
                line, rest = pending.split(b"\n", 1)
                pending[:] = rest
                if not dropping and len(line) <= 65536:
                    consume(line.decode("utf-8", errors="replace"))
                else:
                    complete = False
                dropping = False
            if len(pending) > 65536:
                pending.clear()
                dropping, complete = True, False
        try:
            code, _ = run_gh(self.command(path), timeout=timeout, max_bytes=limit, consume=chunk)
            complete = complete and code == 0
        except (TimeoutError, subprocess.TimeoutExpired, Unknown):
            complete = False
        except OSError:
            raise Unknown("日志下载进程不可用") from None
        if status != 200:
            raise Unknown("日志下载未返回已认证的完整状态头", status=status)
        if complete and pending and not dropping:
            consume(pending.decode("utf-8", errors="replace"))
        return complete


class API:
    def __init__(self, store, transport=None):
        self.store = store
        self.transport = transport or GhTransport()
        self.repo_ids = {}

    def request(self, path, etag=None, limit=None):
        retry_at = self.store.meta_get("api:retry_at", 0)
        if retry_at > time.time():
            raise Unknown("API 限流退避中，保持未知", retry_at=retry_at)
        # Retry one demonstrated transient failure; the receiver schedules later retries.
        for attempt in range(2):
            try:
                response = (self.transport(path, etag, max_bytes=limit) if limit is not None and isinstance(self.transport, GhTransport)
                            else self.transport(path, etag))
            except Unknown:
                if attempt == 0:
                    continue
                raise
            headers = {k.lower(): v for k, v in response.headers.items()}
            if response.status in {403, 429} and ("retry-after" in headers or headers.get("x-ratelimit-remaining") == "0"):
                try:
                    due = max(time.time() + float(headers.get("retry-after", 0)), float(headers.get("x-ratelimit-reset", 0)))
                except (TypeError, ValueError):
                    due = time.time() + 60
                self.store.meta_put("api:retry_at", min(time.time() + 3600, max(time.time() + 1, due)))
            if response.status >= 500 and attempt == 0:
                continue
            return response
        raise Unknown("API 请求失败，保持未知")

    def document(self, path, conditional=True):
        cached = self.store.cache_get(path) if conditional else None
        response = self.request(path, cached[0] if cached else None)
        headers = {k.lower(): v for k, v in response.headers.items()}
        if response.status == 304:
            if not cached:
                raise Unknown("304 缺少持久缓存，保持未知")
            return cached[1], cached[2]
        if response.status != 200:
            raise Unknown(f"API 返回 HTTP {response.status}，保持未知并稍后重试", status=response.status)
        if len(response.body) > 8 * 1024 * 1024:
            raise Unknown("API JSON 超出安全大小，保持未知")
        try:
            data = json.loads(response.body)
        except (ValueError, UnicodeError, RecursionError):
            raise Unknown("API JSON 不完整，保持未知") from None
        if not isinstance(data, (dict, list)):
            raise Unknown("API 数据形状无效，保持未知")
        if conditional:
            self.store.cache_put(path, headers.get("etag"), data, headers.get("link"))
        return data, headers.get("link")

    def get(self, path, conditional=True):
        return self.document(path, conditional)[0]

    def same_repo_path(self, expected, actual):
        if actual == expected:
            return True
        match = re.fullmatch(r"/repos/([^/]+/[^/]+)(/.*)", expected)
        numeric = re.fullmatch(r"/repositories/([1-9][0-9]*)(/.*)", actual)
        if not match or not numeric or match[2] != numeric[2]:
            return False
        repo = match[1]
        if repo not in self.repo_ids:
            item = self.get("/repos/" + repo)
            if (not isinstance(item, dict) or item.get("full_name") != repo or
                    type(item.get("id")) is not int or item["id"] <= 0):
                raise Unknown("数字仓库分页缺少受信仓库身份")
            self.repo_ids[repo] = item["id"]
        return self.repo_ids[repo] == int(numeric[1])

    def page(self, path, field=None, page=1):
        data, link = self.document(f"{path}?per_page=100&page={page}")
        total = data.get("total_count") if field and isinstance(data, dict) else None
        items = data.get(field) if field and isinstance(data, dict) else data
        if not isinstance(items, list) or field and (type(total) is not int or total < 0):
            raise Unknown("API 分页形状无效")
        relations = {}
        if link is not None:
            if not isinstance(link, str) or not link:
                raise Unknown("API 分页链接无效")
            for part in link.split(","):
                match = re.fullmatch(r'\s*<([^<>]+)>;\s*rel="(next|prev|first|last)"\s*', part)
                if not match:
                    raise Unknown("API 分页链接无效")
                try:
                    parsed = urlsplit(match[1])
                    query = parse_qs(parsed.query, keep_blank_values=True, strict_parsing=True)
                    number = query.get("page", [])
                    if len(number) != 1 or not re.fullmatch(r"[1-9][0-9]{0,6}", number[0]):
                        raise ValueError()
                    number = int(number[0])
                except ValueError:
                    raise Unknown("API 分页参数无效") from None
                relation = match[2]
                continuous = ((relation == "next" and number == page + 1) or (relation == "prev" and number == page - 1) or
                              (relation == "first" and number == 1) or (relation == "last" and number >= page))
                if (relation in relations or not continuous or parsed.scheme != "https" or parsed.netloc != "api.github.com"
                        or parsed.fragment or query != {"per_page": ["100"], "page": [str(number)]}
                        or not self.same_repo_path(path, parsed.path)):
                    raise Unknown("拒绝跨域、跨仓库或不连续 API 分页")
                relations[relation] = number
        more = "next" in relations
        if more and not items or not more and relations.get("last", page) != page:
            raise Unknown("API 分页未完整结束")
        if not more and total is not None and total > (page - 1) * 100 + len(items):
            raise Unknown("API 分页不完整或触及过滤上限，保持未知")
        return items, more, total

    def batches(self, path, field=None, max_pages=500):
        total, count = None, 0
        for page in range(1, max_pages + 1):
            items, more, current = self.page(path, field, page)
            if total is not None and current != total:
                raise Unknown("API 分页期间总量变化，稍后重试")
            total = current
            count += len(items)
            if not more and total is not None and count != total:
                raise Unknown("API 分页不完整，保持未知")
            yield items
            if not more and (total is not None or len(items) < 100):
                return
        raise Unknown("API 分页达到安全上限，未宣称完成对账")

    def pages(self, path, field=None, max_pages=500):
        return [item for batch in self.batches(path, field, max_pages) for item in batch]

    def lines(self, path, consume, timeout=1, limit=64 * 1024 * 1024):
        if hasattr(self.transport, "stream"):
            return self.transport.stream(path, consume, timeout, limit)
        response = self.transport(path, None)  # One bounded first-report read, not a retry loop.
        if response.status != 200:
            raise Unknown("日志暂不可用，等待后补", status=response.status)
        stream = io.BytesIO(response.body[:limit])
        complete = len(response.body) <= limit
        for line in stream:
            if len(line) <= 65536:
                consume(line.decode("utf-8", errors="replace"))
            else:
                complete = False
        return complete

    def blob(self, path, limit=8 * 1024 * 1024):
        response = self.request(path, limit=limit)
        if response.status != 200 or len(response.body) > limit:
            raise Unknown("日志或证据下载不可用，等待后补")
        return response.body
