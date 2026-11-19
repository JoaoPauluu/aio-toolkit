"""Compress PDFs with Ghostscript's pdfwrite device."""
import shutil

import aio

p = aio.params()
files = p["files"]
quality = p.get("quality", "ebook")
gs = aio.which("gs")

total_in = total_out = 0
for i, src in enumerate(files):
    name = aio.original_name(src)
    aio.status(f"Compressing {name} ({i + 1}/{len(files)})")
    aio.progress(i / len(files) * 100)
    dst = aio.unique(aio.out(f"{aio.stem(src)}-compressed.pdf"))
    cmd = [gs, "-sDEVICE=pdfwrite", "-dCompatibilityLevel=1.5", f"-dPDFSETTINGS=/{quality}",
           "-dNOPAUSE", "-dBATCH", "-dQUIET", "-dSAFER", "-dDetectDuplicateImages=true"]
    if p.get("grayscale"):
        cmd += ["-sColorConversionStrategy=Gray", "-dProcessColorModel=/DeviceGray"]
    aio.run(cmd + [f"-sOutputFile={dst}", src])

    before, after = aio.Path(src).stat().st_size, dst.stat().st_size
    if after >= before and not p.get("grayscale"):
        shutil.copyfile(src, dst)
        after = before
        aio.log(f"{name}: already optimal, kept original ({aio.human(before)})")
    else:
        aio.log(f"{name}: {aio.human(before)} → {aio.human(after)} (-{100 - after / before * 100:.0f}%)")
    total_in += before
    total_out += after

saved = 100 - total_out / total_in * 100 if total_in else 0
aio.status(f"{aio.human(total_in)} → {aio.human(total_out)} (saved {saved:.0f}%)")
