"""Batch image conversion with vipsthumbnail (streams, low memory, auto-rotates)."""
import subprocess
import zipfile

import aio

p = aio.params()
vt = aio.which("vipsthumbnail")
fmt = p.get("format", "jpg")
q = int(p.get("quality") or 82)
strip = bool(p.get("strip", True))
files = p["files"]

KEEP = {"jpg": "jpg", "jpeg": "jpg", "png": "png", "webp": "webp", "avif": "avif", "tif": "tif", "tiff": "tif",
        "gif": "gif", "jxl": "jxl"}  # HEIC/HEIF have no safe "keep" -> jpg


def opts(ext):
    o = []
    if ext in ("jpg", "webp", "avif", "jxl"):
        o.append(f"Q={q}")
    if ext == "jpg":
        o += ["optimize_coding", "strip" if strip else ""]
    elif ext == "png":
        o += ["compression=9", "strip" if strip else ""]
    elif strip:
        o.append("strip")
    o = [x for x in o if x]
    # vips >= 8.15 uses keep=none instead of strip; translate if needed
    return "[" + ",".join(o) + "]" if o else ""


made, total_in, total_out = [], 0, 0
for i, src in enumerate(files):
    name = aio.original_name(src)
    src_ext = aio.Path(name).suffix.lstrip(".").lower()
    ext = KEEP.get(src_ext, "jpg") if fmt == "keep" else fmt
    dst = aio.unique(aio.out(f"{aio.stem(src)}.{ext}"))
    mode = p.get("resize", "none")
    if mode == "long":
        size = f"{int(p.get('size') or 2048)}x{int(p.get('size') or 2048)}>"
    elif mode == "width":
        size = f"{int(p.get('size') or 2048)}x100000>"
    elif mode == "percent":
        r = subprocess.run(["vipsheader", "-f", "width", src], capture_output=True, text=True)
        if r.returncode != 0:
            aio.fail(f"{name}: not a readable image ({r.stderr.strip()})")
        w = max(1, int(int(r.stdout.strip()) * float(p.get("percent") or 50) / 100))
        size = f"{w}x100000"
    else:
        size = "100000x100000>"
    aio.status(f"Converting {name} → {ext} ({i + 1}/{len(files)})")
    cmd = [vt, src, "--size", size, "-o", f"{dst}{opts(ext)}"]
    r = subprocess.run(cmd, capture_output=True, text=True)
    if r.returncode != 0 and "strip" in r.stderr:
        cmd[-1] = cmd[-1].replace("strip", "keep=none")
        r = subprocess.run(cmd, capture_output=True, text=True)
    if r.returncode != 0:
        aio.fail(f"{name}: {r.stderr.strip().splitlines()[-1] if r.stderr.strip() else 'conversion failed'}")
    a, b = aio.Path(src).stat().st_size, dst.stat().st_size
    total_in, total_out = total_in + a, total_out + b
    aio.log(f"{name} → {dst.name}: {aio.human(a)} → {aio.human(b)}")
    made.append(dst)
    aio.progress((i + 1) / len(files) * 100)

if p.get("zip") and len(made) > 1:
    zpath = aio.out("images.zip")
    with zipfile.ZipFile(zpath, "w", zipfile.ZIP_STORED) as z:
        for f in made:
            z.write(f, f.name)
            f.unlink()
aio.status(f"{len(made)} image(s): {aio.human(total_in)} → {aio.human(total_out)}")
