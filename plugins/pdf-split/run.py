"""Split PDFs with qpdf."""
import re
import zipfile

import aio

p = aio.params()
files = p["files"]
mode = p.get("mode", "pages")
qpdf = aio.which("qpdf")


def npages(path):
    return int(aio.run([qpdf, "--show-npages", path]).stdout.strip().splitlines()[-1])


def parse_ranges(text, n):
    out = []
    for part in [x.strip() for x in (text or "").split(",") if x.strip()]:
        m = re.fullmatch(r"(\d+|z)\s*(?:-\s*(\d+|z))?", part, re.I)
        if not m:
            aio.fail(f"Invalid range '{part}'")
        a = n if m[1].lower() == "z" else int(m[1])
        b = a if m[2] is None else (n if m[2].lower() == "z" else int(m[2]))
        if not (1 <= a <= n and 1 <= b <= n):
            aio.fail(f"Range '{part}' is outside 1-{n}")
        out.append((a, b))
    if not out:
        aio.fail("Enter at least one page range")
    return out


made = []
for i, src in enumerate(files):
    stem = aio.stem(src)
    n = npages(src)
    aio.status(f"Splitting {aio.original_name(src)} ({n} pages)")
    if mode == "ranges":
        ranges = parse_ranges(p.get("ranges"), n)
    elif mode == "chunks":
        size = max(1, int(p.get("chunk") or 10))
        ranges = [(a, min(a + size - 1, n)) for a in range(1, n + 1, size)]
    else:
        ranges = [(a, a) for a in range(1, n + 1)]
    width = len(str(n))
    for k, (a, b) in enumerate(ranges):
        label = f"p{a:0{width}d}" if a == b else f"p{a:0{width}d}-{b:0{width}d}"
        dst = aio.unique(aio.out(f"{stem}_{label}.pdf"))
        # descending ranges (e.g. 5-1) are valid in qpdf and reverse the order
        aio.run([qpdf, "--warning-exit-0", "--empty", "--pages", src, f"{a}-{b}", "--", dst])
        made.append(dst)
        aio.progress((i + (k + 1) / len(ranges)) / len(files) * 100)

if p.get("zip") and len(made) > 1:
    aio.status("Creating zip")
    name = aio.stem(files[0]) + ("_split.zip" if len(files) == 1 else "_and_more_split.zip")
    zpath = aio.out(name)
    with zipfile.ZipFile(zpath, "w", zipfile.ZIP_STORED) as z:
        for f in made:
            z.write(f, f.name)
            f.unlink()

aio.status(f"Created {len(made)} file(s)")
