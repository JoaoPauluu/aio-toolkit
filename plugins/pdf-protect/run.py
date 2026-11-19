"""Decrypt or encrypt PDFs with qpdf."""
import secrets
import subprocess

import aio

p = aio.params()
qpdf = aio.which("qpdf")
files = p["files"]
mode = p.get("mode", "unlock")

if mode == "protect":
    user = p.get("user_password") or ""
    if not user:
        aio.fail("Enter a password to open the PDF")
    owner = p.get("owner_password") or secrets.token_urlsafe(18)
    if not p.get("owner_password"):
        aio.log("No owner password given; a random one was generated (permissions cannot be changed later).")

for i, src in enumerate(files):
    name = aio.original_name(src)
    aio.progress(i / len(files) * 100)
    if mode == "unlock":
        aio.status(f"Unlocking {name}")
        dst = aio.unique(aio.out(f"{aio.stem(src)}-unlocked.pdf"))
        r = subprocess.run([qpdf, "--warning-exit-0", f"--password={p.get('password') or ''}", "--decrypt", src, dst],
                           capture_output=True, text=True)
        if r.returncode != 0:
            if "invalid password" in r.stderr.lower():
                aio.fail(f"{name}: wrong password" if p.get("password") else f"{name} needs a password to open")
            aio.fail(f"{name}: {r.stderr.strip()}")
    else:
        aio.status(f"Encrypting {name}")
        dst = aio.unique(aio.out(f"{aio.stem(src)}-protected.pdf"))
        aio.run([qpdf, "--warning-exit-0", "--encrypt", user, owner, "256",
                 f"--print={'full' if p.get('allow_print') else 'none'}",
                 f"--modify={'all' if p.get('allow_edit') else 'none'}",
                 f"--extract={'y' if p.get('allow_copy') else 'n'}", "--", src, dst])
aio.status(f"{'Unlocked' if mode == 'unlock' else 'Protected'} {len(files)} file(s)")
