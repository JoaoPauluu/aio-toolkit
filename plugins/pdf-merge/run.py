"""Merge PDFs or assemble a custom page order with qpdf.

params:
  files: [path, ...]                    uploaded PDFs, in upload order
  pages: [{file, page, rotate}, ...]    optional; file = index into files,
                                        page = 1-based, rotate = 0/90/180/270
  output_name: str
"""
import aio

p = aio.params()
files = p.get("files") or []
pages = p.get("pages")
name = (p.get("output_name") or "merged.pdf").strip().replace("/", "_")
if not name.lower().endswith(".pdf"):
    name += ".pdf"

if not files:
    aio.fail("No PDF files uploaded")
qpdf = aio.which("qpdf")


def page_count(path):
    r = aio.run([qpdf, "--show-npages", path])
    return int(r.stdout.strip().splitlines()[-1])


# Build a list of (file_index, page, rotate).
if pages:
    seq = []
    for i, e in enumerate(pages):
        try:
            seq.append((int(e["file"]), int(e["page"]), int(e.get("rotate", 0)) % 360))
        except (KeyError, TypeError, ValueError):
            aio.fail(f"Invalid page entry #{i + 1}")
    counts = {}
    for f, pg, _ in seq:
        if not 0 <= f < len(files):
            aio.fail(f"Page refers to missing file #{f}")
        if f not in counts:
            counts[f] = page_count(files[f])
        if not 1 <= pg <= counts[f]:
            aio.fail(f"{aio.original_name(files[f])} has no page {pg}")
    aio.status(f"Assembling {len(seq)} pages from {len(counts)} file(s)")
else:
    seq = None
    aio.status(f"Merging {len(files)} file(s)")

aio.progress(10)

# qpdf --pages takes "file range" pairs; collapse consecutive pages into ranges.
args = []
if seq is None:
    for f in files:
        args += [f, "1-z"]
else:
    run_file, start, prev = None, None, None
    for f, pg, _ in seq + [(None, None, None)]:
        if run_file is not None and f == run_file and pg == prev + 1:
            prev = pg
            continue
        if run_file is not None:
            args += [files[run_file], f"{start}-{prev}" if start != prev else str(start)]
        run_file, start, prev = f, pg, pg

out = aio.out(name)
tmp = aio.JOB_DIR / "assembled.pdf"
aio.run([qpdf, "--warning-exit-0", "--empty", "--pages", *args, "--", tmp])
aio.progress(70)

rot = {}
if seq:
    for idx, (_, _, r) in enumerate(seq, start=1):
        if r:
            rot.setdefault(r, []).append(str(idx))
if rot:
    aio.status("Applying rotations")
    rargs = [f"--rotate=+{deg}:{','.join(idx)}" for deg, idx in rot.items()]
    aio.run([qpdf, "--warning-exit-0", tmp, *rargs, out])
    tmp.unlink()
else:
    tmp.replace(out)

aio.progress(100)
aio.status(f"Created {name} ({page_count(out)} pages, {aio.human(out.stat().st_size)})")
