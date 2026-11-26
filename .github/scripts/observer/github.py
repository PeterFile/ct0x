"""Bounded read-only REST through gh; credentials never leave the CLI."""
from dataclasses import dataclass
import json
import os
import re
import selectors
import subprocess
import time
from urllib.parse import parse_qs, urlsplit


class Unknown(Exception):
    """No new authoritative evidence; retain state and reconcile later."""


@dataclass
class Reply:
    status: int
    headers: dict
    body: bytes


def run_gh(args, timeout=20, max_bytes=24 * 1024 * 1024):
    env = os.environ.copy()
    env.pop("GH_DEBUG", None)
    env.pop("GH_FORCE_TTY", None)
    env.update(GH_PROMPT_DISABLED="1", GH_PAGER="cat", NO_COLOR="1")
    process = subprocess.Popen(args, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                               stderr=subprocess.DEVNULL, env=env)
    data = bytearray()
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
                data.extend(chunk)
                if len(data) > max_bytes:
                    raise Unknown("API 响应超出安全大小，保持未知")
        process.wait(timeout=max(0.01, deadline - time.monotonic()))
        return process.returncode, bytes(data)
    finally:
        if process.poll() is None:
            process.kill()
        process.wait()
        process.stdout.close()


class GhTransport:
    def __call__(self, path, etag=None):
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
        args.append(path)
        try:
            limit = 128 * 1024 * 1024 if re.search(r"/actions/artifacts/[0-9]+/zip$", path) else 24 * 1024 * 1024
            code, raw = run_gh(args, max_bytes=limit)
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
            if sep and key.lower() in (b"etag", b"link"):
                headers[key.decode().lower()] = value.strip().decode("ascii", errors="replace")
        return Reply(status, headers, body)


class API:
    def __init__(self, store, transport=None):
        self.store = store
        self.transport = transport or GhTransport()

    def request(self, path, etag=None):
        # Retry one demonstrated transient failure; the receiver schedules later retries.
        for attempt in range(2):
            try:
                response = self.transport(path, etag)
            except Unknown:
                if attempt == 0:
                    continue
                raise
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
            raise Unknown(f"API 返回 HTTP {response.status}，保持未知并稍后重试")
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

    def pages(self, path, field=None, max_pages=500):
        result = []
        total = None
        for page in range(1, max_pages + 1):
            endpoint = f"{path}?per_page=100&page={page}"
            data, link = self.document(endpoint)
            if field:
                if not isinstance(data, dict) or type(data.get("total_count")) is not int or not isinstance(data.get(field), list):
                    raise Unknown("API 分页形状无效")
                if total is not None and total != data["total_count"]:
                    raise Unknown("API 分页期间总量变化，稍后重试")
                total = data["total_count"]
                items = data[field]
            else:
                items = data
            if not isinstance(items, list):
                raise Unknown("API 分页形状无效")
            result.extend(items)
            next_links = re.findall(r'<([^>]+)>;\s*rel="next"', link or "")
            if next_links:
                if len(next_links) != 1 or not items:
                    raise Unknown("API 分页链接无效")
                parsed = urlsplit(next_links[0])
                if (parsed.scheme != "https" or parsed.netloc != "api.github.com" or parsed.path != path
                        or parsed.fragment or parse_qs(parsed.query) != {"per_page": ["100"], "page": [str(page + 1)]}):
                    raise Unknown("拒绝跨域、循环或不连续 API 分页")
            elif total is not None:
                if len(result) != total:
                    raise Unknown("API 分页不完整，保持未知")
                return result
            elif len(items) < 100:
                return result
        raise Unknown("API 分页达到安全上限，未宣称完成对账")

    def blob(self, path, limit=8 * 1024 * 1024):
        response = self.request(path)
        if response.status != 200 or len(response.body) > limit:
            raise Unknown("日志或证据下载不可用，等待后补")
        return response.body
