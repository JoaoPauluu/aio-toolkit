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
