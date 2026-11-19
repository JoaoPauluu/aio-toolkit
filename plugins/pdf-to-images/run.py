"""Rasterize PDF pages with Ghostscript."""
import zipfile

import aio

p = aio.params()
gs = aio.which("gs")
qpdf = aio.which("qpdf")
files = p["files"]
fmt = p.get("format", "png")
dpi = int(p.get("custom_dpi") or 200) if p.get("dpi") == "custom" else int(p.get("dpi") or 150)
device = ["-sDEVICE=png16m"] if fmt == "png" else ["-sDEVICE=jpeg", "-dJPEGQ=90"]

made = []
for i, src in enumerate(files):
    name = aio.original_name(src)
    n = int(aio.run([qpdf, "--show-npages", src]).stdout.strip().splitlines()[-1])
    first = int(p.get("first") or 1)
    last = min(int(p.get("last") or n), n)
    if first > last:
        aio.fail(f"{name}: page range {first}-{last} is empty (the PDF has {n} pages)")
    aio.status(f"Rendering {name} pages {first}-{last} at {dpi} dpi ({i + 1}/{len(files)})")
    tmpdir = aio.JOB_DIR / f"r{i}"
    tmpdir.mkdir(exist_ok=True)
    aio.run([gs, "-dSAFER", "-dBATCH", "-dNOPAUSE", "-dQUIET", *device, f"-r{dpi}",
             "-dTextAlphaBits=4", "-dGraphicsAlphaBits=4",
             f"-dFirstPage={first}", f"-dLastPage={last}", f"-sOutputFile={tmpdir}/%04d.{fmt}", src])
    width = max(3, len(str(n)))
    for k, f in enumerate(sorted(tmpdir.iterdir())):
        dst = aio.unique(aio.out(f"{aio.stem(src)}_p{first + k:0{width}d}.{fmt}"))
        f.replace(dst)
        made.append(dst)
    aio.progress((i + 1) / len(files) * 100)

if p.get("zip") and len(made) > 1:
    aio.status("Creating zip")
    zpath = aio.out(f"{aio.stem(files[0])}_{fmt}.zip")
    with zipfile.ZipFile(zpath, "w", zipfile.ZIP_STORED) as z:
        for f in made:
            z.write(f, f.name)
            f.unlink()
aio.status(f"Rendered {len(made)} page(s)")
