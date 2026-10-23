#!/usr/bin/env python3
"""Mount the Body's default root as a local directory (FUSE, via fusepy).

agy's file tools (view_file, write_to_file, list_dir, grep_search, edits ...)
read and write the disk inside the agy process, so the only way to point them
at the user's computer is a filesystem: the service runs agy with its
workspace on this mount, and every file operation becomes a Body MCP call
(stat_path, list_directory, read_file / write_file with encoding "base64",
create_directory, delete_path, move_path).

usage: komputer-fs.py <mountpoint> <config.json>
config: {"url": ..., "headers": {...}, "ttl": 2.0, "negativeTtl": 1.0, "debug": false}
("debug" logs every Body call, without file contents, to stderr.)
The mount goes away when stdin closes (the service exited) or on SIGTERM.
"""
import base64
import errno
import json
import os
import stat
import subprocess
import sys
import threading
import time
import urllib.request
from datetime import datetime

try:
    from fusepy import FUSE, FuseOSError, Operations  # Debian / Ubuntu python3-fusepy
except ImportError:  # pip install fusepy
    from fuse import FUSE, FuseOSError, Operations


class Mcp:
    """Streamable HTTP MCP client: initialize once, then tools/call."""

    def __init__(self, url, headers):
        self.url = url
        self.headers = dict(headers or {})
        self.session = None
        self.protocol = None
        self.lock = threading.Lock()
        self.ready = False
        self.seq = 0

    def _post(self, msg):
        data = json.dumps(msg).encode()
        headers = {**self.headers, "content-type": "application/json", "accept": "application/json, text/event-stream"}
        if self.session:
            headers["mcp-session-id"] = self.session
        if self.protocol:
            headers["mcp-protocol-version"] = self.protocol
        req = urllib.request.Request(self.url, data=data, headers=headers, method="POST")
        with urllib.request.urlopen(req, timeout=60) as res:
            sid = res.headers.get("mcp-session-id")
            if sid:
                self.session = sid
            raw = res.read().decode("utf-8", "replace")
            ctype = res.headers.get("content-type", "")
        if "id" not in msg:
            return None
        blocks = [raw]
        if "text/event-stream" in ctype:
            blocks = ["\n".join(l[5:].strip() for l in b.splitlines() if l.startswith("data:")) for b in raw.replace("\r\n", "\n").split("\n\n")]
        for b in blocks:
            if not b.strip():
                continue
            try:
                parsed = json.loads(b)
            except ValueError:
                continue
            for m in parsed if isinstance(parsed, list) else [parsed]:
                if isinstance(m, dict) and m.get("id") == msg["id"]:
                    return m
        raise OSError(errno.EIO, "MCP reply without a matching response")

    def _next(self):
        with self.lock:
            self.seq += 1
            return self.seq

    def _ensure(self):
        with self.lock:
            if self.ready:
                return
            r = self._post({"jsonrpc": "2.0", "id": 0, "method": "initialize",
                            "params": {"protocolVersion": "2025-06-18", "capabilities": {}, "clientInfo": {"name": "komputer-fs", "version": "1"}}})
            if r.get("error"):
                raise OSError(errno.EIO, r["error"].get("message", "initialize failed"))
            self.protocol = (r.get("result") or {}).get("protocolVersion")
            self._post({"jsonrpc": "2.0", "method": "notifications/initialized"})
            self.ready = True

    def call(self, name, args):
        self._ensure()
        r = self._post({"jsonrpc": "2.0", "id": self._next(), "method": "tools/call", "params": {"name": name, "arguments": args}})
        if r.get("error"):
            raise OSError(errno.EIO, r["error"].get("message", name))
        result = r.get("result") or {}
        text = "".join(c.get("text", "") for c in result.get("content", []) if c.get("type") == "text")
        if result.get("isError"):
            raise ToolError(text)
        try:
            return json.loads(text)
        except ValueError:
            return text


class ToolError(Exception):
    pass


def errno_of(message):
    m = str(message).lower()
    if "enoent" in m or "no such file" in m or "not found" in m:
        return errno.ENOENT
    if "eexist" in m or "exists" in m:
        return errno.EEXIST
    if "enotempty" in m or "not empty" in m:
        return errno.ENOTEMPTY
    if "eisdir" in m or "is a directory" in m:
        return errno.EISDIR
    if "enotdir" in m or "not a directory" in m:
        return errno.ENOTDIR
    if "outside" in m or "eacces" in m or "eperm" in m or "permission" in m or "climb" in m:
        return errno.EACCES
    if "limit is" in m or "too large" in m:
        return errno.EFBIG
    return errno.EIO


def mtime_of(iso):
    try:
        return datetime.fromisoformat(str(iso).replace("Z", "+00:00")).timestamp()
    except (TypeError, ValueError):
        return time.time()


class Handle:
    def __init__(self, path):
        self.path = path
        self.buf = None  # whole content once a write needs it
        self.window = (0, b"")  # read-ahead for read-only use
        self.dirty = False
        self.lock = threading.Lock()


class KomputerFS(Operations):
    READ_AHEAD = 512 * 1024

    def __init__(self, mcp, ttl=2.0, negative_ttl=1.0, debug=False):
        self.mcp = mcp
        self.debug = debug
        self.ttl = ttl
        self.negative_ttl = negative_ttl
        self.attrs = {}
        self.dirs = {}
        self.handles = {}
        self.next_fh = 1
        self.mutex = threading.Lock()
        self.uid = os.getuid()
        self.gid = os.getgid()

    def init(self, path):
        print("mounted", flush=True)

    # -- helpers
    @staticmethod
    def rel(path):
        return path.lstrip("/") or "."

    def call(self, name, args):
        if self.debug:
            shown = {k: v for k, v in args.items() if k != "content"}
            print(f"komputer-fs: {name} {json.dumps(shown)}", file=sys.stderr, flush=True)
        try:
            return self.mcp.call(name, args)
        except ToolError as e:
            raise FuseOSError(errno_of(e))
        except OSError as e:
            raise FuseOSError(e.errno if isinstance(e.errno, int) else errno.EIO)
        except Exception:
            raise FuseOSError(errno.EIO)

    def to_attr(self, info):
        kind = info.get("type")
        mode = info.get("mode")
        if kind == "dir":
            st_mode = stat.S_IFDIR | (mode if isinstance(mode, int) else 0o755)
            nlink = 2
        else:
            st_mode = stat.S_IFREG | (mode if isinstance(mode, int) else 0o644)
            nlink = 1
        t = mtime_of(info.get("modified"))
        size = info.get("size") or 0
        return {"st_mode": st_mode, "st_nlink": nlink, "st_size": size, "st_uid": self.uid, "st_gid": self.gid,
                "st_mtime": t, "st_ctime": t, "st_atime": t, "st_blocks": (size + 511) // 512, "st_blksize": 4096}

    def remember(self, path, attr):
        with self.mutex:
            self.attrs[path] = (time.monotonic() + (self.ttl if attr else self.negative_ttl), attr)

    def forget(self, *paths):
        with self.mutex:
            for p in paths:
                self.attrs.pop(p, None)
                self.dirs.pop(p, None)
                parent = os.path.dirname(p) or "/"
                self.dirs.pop(parent, None)

    def open_handle(self, path):
        with self.mutex:
            for h in self.handles.values():
                if h.path == path and h.buf is not None:
                    return h
        return None

    # -- metadata
    def getattr(self, path, fh=None):
        h = self.handles.get(fh) if fh else self.open_handle(path)
        with self.mutex:
            hit = self.attrs.get(path)
        if hit and hit[0] > time.monotonic():
            attr = hit[1]
            if attr is None:
                raise FuseOSError(errno.ENOENT)
        else:
            if path == "/":
                attr = self.to_attr({"type": "dir"})
            else:
                try:
                    attr = self.to_attr(self.call("stat_path", {"path": self.rel(path)}))
                except FuseOSError as e:
                    if e.errno == errno.ENOENT:
                        self.remember(path, None)
                    raise
            self.remember(path, attr)
        if h is not None and h.buf is not None:
            attr = {**attr, "st_size": len(h.buf)}
        return attr

    def readdir(self, path, fh):
        with self.mutex:
            hit = self.dirs.get(path)
        if hit and hit[0] > time.monotonic():
            return [".", ".."] + hit[1]
        r = self.call("list_directory", {"path": self.rel(path), "limit": 100000})
        names = []
        for e in r.get("entries", []):
            names.append(e["name"])
            child = (path.rstrip("/") + "/" + e["name"])
            self.remember(child, self.to_attr({**e, "type": "dir" if e.get("type") == "dir" else "file"}))
        with self.mutex:
            self.dirs[path] = (time.monotonic() + self.ttl, names)
        return [".", ".."] + names

    def statfs(self, path):
        return {"f_bsize": 4096, "f_frsize": 4096, "f_blocks": 1 << 30, "f_bfree": 1 << 29, "f_bavail": 1 << 29,
                "f_files": 1 << 24, "f_ffree": 1 << 23, "f_favail": 1 << 23, "f_namemax": 255}

    def access(self, path, amode):
        self.getattr(path)
        return 0

    def chmod(self, path, mode):
        return 0

    def chown(self, path, uid, gid):
        return 0

    def utimens(self, path, times=None):
        return 0

    def readlink(self, path):
        raise FuseOSError(errno.ENOSYS)

    def symlink(self, target, source):
        raise FuseOSError(errno.ENOSYS)

    def link(self, target, source):
        raise FuseOSError(errno.ENOSYS)

    # -- directories and names
    def mkdir(self, path, mode):
        self.call("create_directory", {"path": self.rel(path)})
        self.forget(path)

    def rmdir(self, path):
        if self.call("list_directory", {"path": self.rel(path), "limit": 1}).get("total", 0):
            raise FuseOSError(errno.ENOTEMPTY)
        self.call("delete_path", {"path": self.rel(path), "recursive": True})
        self.forget(path)

    def unlink(self, path):
        self.call("delete_path", {"path": self.rel(path)})
        self.forget(path)

    def rename(self, old, new):
        self.call("move_path", {"from": self.rel(old), "to": self.rel(new), "overwrite": True})
        self.forget(old, new)

    # -- file contents
    def _new_fh(self, h):
        with self.mutex:
            fh = self.next_fh
            self.next_fh += 1
            self.handles[fh] = h
        return fh

    def _load(self, h):
        if h.buf is None:
            r = self.call("read_file", {"path": self.rel(h.path), "encoding": "base64"})
            h.buf = bytearray(base64.b64decode(r.get("content", "")))
            if r.get("size", len(h.buf)) > len(h.buf):
                raise FuseOSError(errno.EFBIG)

    def _flush(self, h):
        if not h.dirty:
            return
        self.call("write_file", {"path": self.rel(h.path), "content": base64.b64encode(bytes(h.buf)).decode(), "encoding": "base64", "overwrite": True})
        h.dirty = False
        self.forget(h.path)

    def create(self, path, mode, fi=None):
        self.call("write_file", {"path": self.rel(path), "content": "", "encoding": "base64", "overwrite": True})
        self.forget(path)
        h = Handle(path)
        h.buf = bytearray()
        return self._new_fh(h)

    def open(self, path, flags):
        h = Handle(path)
        if flags & os.O_TRUNC:
            h.buf = bytearray()
            h.dirty = True
        return self._new_fh(h)

    def read(self, path, size, offset, fh):
        h = self.handles.get(fh) or Handle(path)
        with h.lock:
            if h.buf is not None:
                return bytes(h.buf[offset:offset + size])
            start, data = h.window
            if start <= offset and offset + size <= start + len(data):
                return data[offset - start:offset - start + size]
            r = self.call("read_file", {"path": self.rel(path), "encoding": "base64", "byteOffset": offset, "byteLength": max(size, self.READ_AHEAD)})
            data = base64.b64decode(r.get("content", ""))
            h.window = (offset, data)
            return data[:size]

    def write(self, path, data, offset, fh):
        h = self.handles.get(fh)
        if h is None:
            raise FuseOSError(errno.EBADF)
        with h.lock:
            self._load(h)
            end = offset + len(data)
            if end > len(h.buf):
                h.buf.extend(b"\0" * (end - len(h.buf)))
            h.buf[offset:end] = data
            h.dirty = True
        return len(data)

    def truncate(self, path, length, fh=None):
        h = self.handles.get(fh) if fh else None
        temp = h is None
        if temp:
            h = Handle(path)
        with h.lock:
            if length == 0:
                h.buf = bytearray()
            else:
                self._load(h)
                del h.buf[length:]
                if len(h.buf) < length:
                    h.buf.extend(b"\0" * (length - len(h.buf)))
            h.dirty = True
            if temp:
                self._flush(h)

    def flush(self, path, fh):
        h = self.handles.get(fh)
        if h:
            with h.lock:
                self._flush(h)
        return 0

    def fsync(self, path, datasync, fh):
        return self.flush(path, fh)

    def release(self, path, fh):
        with self.mutex:
            h = self.handles.pop(fh, None)
        if h:
            with h.lock:
                self._flush(h)
        return 0


def main():
    if len(sys.argv) != 3:
        print(__doc__, file=sys.stderr)
        sys.exit(2)
    mountpoint, config_file = sys.argv[1], sys.argv[2]
    with open(config_file) as f:
        cfg = json.load(f)
    mcp = Mcp(cfg["url"], cfg.get("headers"))
    top = mcp.call("stat_path", {"path": "."})  # fail fast when the Body is unreachable
    if not isinstance(top, dict) or top.get("type") != "dir":
        print(f"komputer-fs: the Body root is not a directory: {json.dumps(top)[:200]}", file=sys.stderr)
        sys.exit(1)

    def unmount():
        for tool in ("fusermount3", "fusermount"):
            try:
                subprocess.run([tool, "-u", "-z", mountpoint], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=False)
                return
            except FileNotFoundError:
                continue

    def watch_stdin():
        # Raw reads: a daemon thread blocked in a buffered read aborts interpreter shutdown.
        try:
            while os.read(0, 4096):
                pass
        except OSError:
            pass
        unmount()

    threading.Thread(target=watch_stdin, daemon=True).start()
    fs = KomputerFS(mcp, float(cfg.get("ttl", 2.0)), float(cfg.get("negativeTtl", 1.0)), bool(cfg.get("debug")))
    FUSE(fs, mountpoint, foreground=True, nothreads=False, fsname="komputer", subtype="komputer", default_permissions=False, auto_cache=False)


if __name__ == "__main__":
    main()
