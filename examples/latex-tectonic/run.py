import zipfile
from pathlib import Path

import aio
import tectonic

p = aio.params()
work = aio.JOB_DIR / "src"
work.mkdir(parents=True, exist_ok=True)

files = p.get("files") or []
source = (p.get("source") or "").strip()
if not files and not source:
    aio.fail("Upload a .tex / .zip file or paste some LaTeX.")

if files:
    src = Path(files[0])
    if src.suffix.lower() == ".zip":
        aio.status("Unpacking project…")
        with zipfile.ZipFile(src) as zf:
            for name in zf.namelist():
                dest = (work / name).resolve()
                if not str(dest).startswith(str(work.resolve())):
                    aio.fail(f"unsafe path in zip: {name}")
            zf.extractall(work)
        main = (p.get("main") or "").strip()
        if main:
            mains = [work / main]
        else:
            mains = [f for f in sorted(work.rglob("*.tex")) if "\\documentclass" in f.read_text(errors="ignore")]
        if not mains or not mains[0].exists():
            aio.fail("No main .tex file found in the zip (set 'Main file in the zip').")
        main_file = mains[0]
        name = aio.stem(src)
    else:
        main_file = work / aio.original_name(src)
        main_file.write_bytes(src.read_bytes())
        name = main_file.stem
else:
    main_file = work / "document.tex"
    main_file.write_text(source, encoding="utf-8")
    name = "document"

exe = tectonic.binary()
aio.title(name + ".pdf")
aio.status("Compiling (the first run downloads the TeX packages it needs)…")
outdir = aio.JOB_DIR / "build"
outdir.mkdir(exist_ok=True)
args = [exe, "-X", "compile", str(main_file), "--outdir", str(outdir)]
if p.get("keep_logs"):
    args.append("--keep-logs")
aio.run(args, cwd=str(main_file.parent))

for f in outdir.iterdir():
    if f.suffix == ".pdf" or (p.get("keep_logs") and f.suffix == ".log"):
        aio.out(f"{name}{f.suffix}").write_bytes(f.read_bytes())
aio.progress(100)
