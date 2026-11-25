"""Tiny helper for AIO Toolkit plugins written in Python.

The server puts this folder on PYTHONPATH, so a plugin can simply:

    import aio
    p = aio.params()
    aio.status("Working…")
    aio.progress(50)
    out = aio.out("result.pdf")

Everything here is a thin wrapper over the stdout protocol and env vars, so
plugins in other languages can do the same with plain prints.
"""
from __future__ import annotations

import json
import os
import shutil
import subprocess
import sys
from pathlib import Path

IN = Path(os.environ.get("AIO_IN", "."))
OUT = Path(os.environ.get("AIO_OUT", "."))
JOB_DIR = Path(os.environ.get("AIO_JOB_DIR", "."))
DATA = Path(os.environ.get("AIO_DATA", "."))
DOWNLOADS = Path(os.environ.get("AIO_DOWNLOADS", "."))

_params = None


def params() -> dict:
    """Parsed params.json (file inputs are lists of absolute paths)."""
    global _params
    if _params is None:
        with open(os.environ["AIO_PARAMS"], encoding="utf-8") as f:
            _params = json.load(f)
    return _params


def _emit(line: str) -> None:
    sys.stdout.write(line + "\n")
    sys.stdout.flush()


def progress(pct: float) -> None:
    _emit(f"::progress {max(0.0, min(100.0, float(pct))):.1f}")


def status(msg: str) -> None:
    _emit("::status " + " ".join(str(msg).split()))


def title(msg: str) -> None:
    """Set the job title shown in the Jobs panel."""
    _emit("::title " + " ".join(str(msg).split()))


def log(msg: str) -> None:
    _emit(str(msg))


def fail(msg: str, code: int = 1):
    """Report an error message to the UI and exit."""
    _emit("::error " + " ".join(str(msg).split()))
    sys.exit(code)


def out(name: str) -> Path:
    """Path for an output file (becomes a download in the UI)."""
    p = OUT / name
    p.parent.mkdir(parents=True, exist_ok=True)
    return p


def original_name(path: str | Path) -> str:
    """Uploaded files are stored as NNN_<name>; return <name>."""
    name = Path(path).name
    head, sep, rest = name.partition("_")
    return rest if sep and head.isdigit() else name


def stem(path: str | Path) -> str:
    return Path(original_name(path)).stem


def unique(path: Path) -> Path:
    """Return path, or path with ' (n)' appended if it already exists."""
    if not path.exists():
        return path
    i = 2
    while True:
        cand = path.with_name(f"{path.stem} ({i}){path.suffix}")
        if not cand.exists():
            return cand
        i += 1


def run(cmd: list, **kw) -> subprocess.CompletedProcess:
    """Run a command, streaming its output into the job log. Fails the job on error."""
    log("$ " + " ".join(str(c) for c in cmd))
    proc = subprocess.Popen([str(c) for c in cmd], stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                            text=True, errors="replace", **kw)
    lines = []
    for line in proc.stdout:
        line = line.rstrip()
        lines.append(line)
        log(line)
    proc.wait()
    if proc.returncode != 0:
        tail = next((l for l in reversed(lines) if l.strip()), "")
        fail(f"{Path(str(cmd[0])).name} failed (exit {proc.returncode}){': ' + tail if tail else ''}")
    return subprocess.CompletedProcess(cmd, proc.returncode, "\n".join(lines), None)


def which(name: str) -> str:
    p = shutil.which(name)
    if not p:
        fail(f"'{name}' is not installed in this container")
    return p


def human(n: float) -> str:
    for unit in ("B", "KB", "MB", "GB"):
        if n < 1024 or unit == "GB":
            return f"{n:.1f} {unit}" if unit != "B" else f"{int(n)} B"
        n /= 1024
    return str(n)


def ensure_packages(name: str, *pkgs: str) -> None:
    """Make pip packages importable, installing them on first use into
    $AIO_DATA/venvs/<name> (persists across container updates)."""
    import glob
    import site
    venv = DATA / "venvs" / name
    marker = venv / ".installed"
    want = "\n".join(sorted(pkgs))
    if not marker.exists() or marker.read_text() != want:
        status(f"Installing {', '.join(pkgs)} (first run only)…")
        if not (venv / "bin" / "pip").exists():
            run([sys.executable, "-m", "venv", venv])
        run([venv / "bin" / "pip", "install", "--no-cache-dir", "--disable-pip-version-check", "-q", *pkgs])
        marker.write_text(want)
    for sp in glob.glob(str(venv / "lib" / "python*" / "site-packages")):
        site.addsitedir(sp)


# --------------------------------------------------------------------------
# Talking to the server and to other plugins
# --------------------------------------------------------------------------

API = os.environ.get("AIO_API", "http://127.0.0.1:8080").rstrip("/")
PLUGIN_ID = os.environ.get("AIO_PLUGIN_ID", "")
ARCH = os.environ.get("AIO_ARCH", "")
TOOLS = Path(os.environ.get("AIO_TOOLS", str(DATA / "tools" / "bin")))


def plugin_data() -> Path:
    """Persistent folder for this plugin ($AIO_DATA/plugins/<id>); survives
    updates and restarts. Its bin/ subfolder is on PATH for this plugin."""
    p = Path(os.environ.get("AIO_PLUGIN_DATA", str(DATA / "plugins" / (PLUGIN_ID or "unknown"))))
    p.mkdir(parents=True, exist_ok=True)
    return p


def _headers(extra: dict | None = None) -> dict:
    h = {"User-Agent": "aio-plugin/" + (PLUGIN_ID or "?")}
    tok = os.environ.get("AIO_JOB_TOKEN")
    if tok:
        h["X-AIO-Job-Token"] = tok
    if extra:
        h.update(extra)
    return h


def api(method: str, path: str, body=None, timeout: float = 60, raw: bool = False):
    """Call the AIO HTTP API. body may be a dict (sent as JSON) or bytes.
    Returns parsed JSON (or bytes with raw=True). Raises RuntimeError on HTTP errors."""
    import urllib.error
    import urllib.request
    headers = _headers()
    data = None
    if isinstance(body, (dict, list)):
        data = json.dumps(body).encode()
        headers["Content-Type"] = "application/json"
    elif isinstance(body, bytes):
        data = body
    req = urllib.request.Request(API + path, data=data, method=method, headers=headers)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as res:
            b = res.read()
    except urllib.error.HTTPError as e:
        b = e.read()
        try:
            msg = json.loads(b).get("error") or b.decode(errors="replace")
        except Exception:
            msg = b.decode(errors="replace")
        raise RuntimeError(f"{method} {path}: HTTP {e.code}: {msg}") from None
    if raw:
        return b
    return json.loads(b) if b else None


def find(capability: str) -> list[dict]:
    """Plugins that declare `provides: [capability]` (e.g. "latex.compile")."""
    from urllib.parse import quote
    return api("GET", "/api/plugins?provides=" + quote(capability))["plugins"]


class CallResult(dict):
    """The finished job (dict) plus `.files`: local paths of downloaded outputs."""
    files: list[Path]


def call(plugin: str, params: dict | None = None, files: dict | None = None,
         wait: str = "2h", out_dir: str | Path | None = None, title: str | None = None) -> CallResult:
    """Run another plugin and wait for it, e.g.

        res = aio.call("pdf-merge", files={"files": [a, b]})
        merged = res.files[0]

    `params` are the target's inputs (as in its form / GET /api/plugins/<id>);
    `files` maps a file input to a list of local paths. Called from a job, the
    child skips the job queue and is canceled with this job. Outputs are
    downloaded into out_dir (default: a folder in the job's scratch space).
    Raises RuntimeError if the job fails.
    """
    import uuid
    from urllib.parse import quote
    params = dict(params or {})
    if title:
        params["__title"] = title
    path = f"/api/plugins/{quote(plugin)}/run?wait={quote(wait)}"
    if files:
        boundary = "aio" + uuid.uuid4().hex
        parts = []
        for k, v in params.items():
            if isinstance(v, (dict, list)):
                v = json.dumps(v)
            elif isinstance(v, bool):
                v = "true" if v else "false"
            parts.append(f'--{boundary}\r\nContent-Disposition: form-data; name="{k}"\r\n\r\n{v}\r\n'.encode())
        for k, paths in files.items():
            for fp in paths:
                fp = Path(fp)
                parts.append((f'--{boundary}\r\nContent-Disposition: form-data; name="{k}"; '
                              f'filename="{original_name(fp)}"\r\nContent-Type: application/octet-stream\r\n\r\n').encode()
                             + fp.read_bytes() + b"\r\n")
        parts.append(f"--{boundary}--\r\n".encode())
        import urllib.request
        import urllib.error
        req = urllib.request.Request(API + path, data=b"".join(parts), method="POST",
                                     headers=_headers({"Content-Type": f"multipart/form-data; boundary={boundary}"}))
        try:
            with urllib.request.urlopen(req, timeout=None) as res:
                job = json.loads(res.read())
        except urllib.error.HTTPError as e:
            raise RuntimeError(f"{plugin}: HTTP {e.code}: {e.read().decode(errors='replace')}") from None
    else:
        job = api("POST", path, params, timeout=None)
    while job.get("state") not in ("done", "failed", "canceled"):
        job = api("GET", f"/api/jobs/{job['id']}?wait=10m", timeout=None)
    if job["state"] != "done":
        tail = "\n".join((job.get("logs") or [])[-5:])
        raise RuntimeError(f"{plugin} {job['state']}: {job.get('error') or tail}")
    dest = Path(out_dir) if out_dir else JOB_DIR / "calls" / job["id"]
    dest.mkdir(parents=True, exist_ok=True)
    res = CallResult(job)
    res.files = []
    for f in job.get("files", []):
        target = dest / f["name"]
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_bytes(api("GET", f["url"], timeout=None, raw=True))
        res.files.append(target)
    return res


def emit(topic: str, data: dict | None = None) -> None:
    """Publish a custom event, e.g. aio.emit("my-plugin.finished", {"n": 3}).
    Other plugins can react with `events: [{on: my-plugin.finished}]`."""
    api("POST", "/api/events", {"topic": topic, "data": data or {}})


def event() -> dict | None:
    """The event that started this job (for `events:` hooks), else None."""
    p = os.environ.get("AIO_EVENT")
    if not p:
        return None
    with open(p, encoding="utf-8") as f:
        return json.load(f)


def service_url(plugin: str | None = None) -> str:
    """Base URL of a plugin's background service (default: this plugin's)."""
    return f"{API}/s/{plugin or PLUGIN_ID}/"


def ensure_binary(name: str, urls: dict, sha256: dict | None = None, member: str | None = None,
                  shared: bool = False, version: str = "") -> Path:
    """Download a prebuilt tool once and return its path, e.g.

        tectonic = aio.ensure_binary("tectonic", {
            "amd64": "https://…/tectonic-0.15.0-x86_64-unknown-linux-musl.tar.gz",
            "arm64": "https://…/tectonic-0.15.0-aarch64-unknown-linux-musl.tar.gz",
        }, version="0.15.0")

    `urls` maps AIO_ARCH (amd64, arm64) to a URL of the binary itself or a
    .tar.gz/.tgz/.tar.xz/.zip holding it (`member` = path inside the archive,
    default: the first file named `name`). The container is Alpine (musl), so
    pick static or *-linux-musl builds. With shared=True it goes to
    $AIO_TOOLS (on every plugin's PATH), else to this plugin's bin/.
    Re-downloaded only when `version` changes.
    """
    import hashlib
    import tarfile
    import tempfile
    import urllib.request
    import zipfile
    bindir = TOOLS if shared else plugin_data() / "bin"
    bindir.mkdir(parents=True, exist_ok=True)
    target = bindir / name
    marker = bindir / f".{name}.version"
    if target.exists() and (not version or (marker.exists() and marker.read_text() == version)):
        return target
    arch = ARCH or {"x86_64": "amd64", "aarch64": "arm64"}.get(os.uname().machine, os.uname().machine)
    url = urls.get(arch)
    if not url:
        fail(f"{name}: no download for architecture {arch}")
    status(f"Downloading {name} (first run only)…")
    log(f"downloading {url}")
    with tempfile.TemporaryDirectory(dir=str(bindir)) as tmp:
        dl = Path(tmp) / "download"
        with urllib.request.urlopen(urllib.request.Request(url, headers={"User-Agent": "aio-toolkit"}), timeout=600) as res, open(dl, "wb") as f:
            shutil.copyfileobj(res, f)
        want = (sha256 or {}).get(arch)
        if want:
            got = hashlib.sha256(dl.read_bytes()).hexdigest()
            if got.lower() != want.lower():
                fail(f"{name}: checksum mismatch (got {got})")
        src = dl
        low = url.lower().split("?")[0]
        if low.endswith((".tar.gz", ".tgz", ".tar.xz", ".tar.bz2", ".tar")):
            with tarfile.open(dl) as tf:
                cands = [m for m in tf.getmembers() if m.isfile() and
                         (m.name == member if member else Path(m.name).name == name)]
                if not cands:
                    fail(f"{name}: not found in archive")
                f = tf.extractfile(cands[0])
                src = Path(tmp) / "bin"
                src.write_bytes(f.read())
        elif low.endswith(".zip"):
            with zipfile.ZipFile(dl) as zf:
                cands = [n for n in zf.namelist() if (n == member if member else Path(n).name == name)]
                if not cands:
                    fail(f"{name}: not found in archive")
                src = Path(tmp) / "bin"
                src.write_bytes(zf.read(cands[0]))
        src.chmod(0o755)
        os.replace(src, target)
    if version:
        marker.write_text(version)
    return target
