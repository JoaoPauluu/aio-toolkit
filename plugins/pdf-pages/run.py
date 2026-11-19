"""Rotate or delete selected pages with qpdf."""
import re

import aio

p = aio.params()
qpdf = aio.which("qpdf")
action, which = p.get("action", "rotate90"), p.get("which", "all")


def selected(n):
    if which == "all":
        return set(range(1, n + 1))
    if which == "odd":
        return set(range(1, n + 1, 2))
    if which == "even":
        return set(range(2, n + 1, 2))
    pages = set()
    for part in [x.strip() for x in (p.get("ranges") or "").split(",") if x.strip()]:
        m = re.fullmatch(r"(\d+|z)\s*(?:-\s*(\d+|z))?", part, re.I)
        if not m:
            aio.fail(f"Invalid range '{part}'")
        a = n if m[1].lower() == "z" else int(m[1])
        b = a if m[2] is None else (n if m[2].lower() == "z" else int(m[2]))
        if not (1 <= a <= n and 1 <= b <= n):
            aio.fail(f"Range '{part}' is outside 1-{n}")
        pages.update(range(min(a, b), max(a, b) + 1))
    if not pages:
        aio.fail("Enter at least one page range")
    return pages


def to_ranges(pages):
    out, run = [], []
    for pg in sorted(pages):
        if run and pg == run[-1] + 1:
            run.append(pg)
        else:
            if run:
                out.append(f"{run[0]}-{run[-1]}" if len(run) > 1 else str(run[0]))
            run = [pg]
    if run:
        out.append(f"{run[0]}-{run[-1]}" if len(run) > 1 else str(run[0]))
    return ",".join(out)


files = p["files"]
for i, src in enumerate(files):
    name = aio.original_name(src)
    n = int(aio.run([qpdf, "--show-npages", src]).stdout.strip().splitlines()[-1])
    sel = selected(n)
    if action == "delete":
        keep = set(range(1, n + 1)) - sel
        if not keep:
            aio.fail(f"{name}: that would delete every page")
        aio.status(f"{name}: removing {len(sel)} of {n} pages")
        dst = aio.unique(aio.out(f"{aio.stem(src)}-edited.pdf"))
        aio.run([qpdf, "--warning-exit-0", "--empty", "--pages", src, to_ranges(keep), "--", dst])
    else:
        deg = action.removeprefix("rotate")
        aio.status(f"{name}: rotating {len(sel)} of {n} pages by {deg}°")
        dst = aio.unique(aio.out(f"{aio.stem(src)}-rotated.pdf"))
        aio.run([qpdf, "--warning-exit-0", src, f"--rotate=+{deg}:{to_ranges(sel)}", dst])
    aio.progress((i + 1) / len(files) * 100)
aio.status(f"Processed {len(files)} file(s)")
