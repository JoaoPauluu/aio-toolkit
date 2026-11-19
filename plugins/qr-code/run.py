"""Generate a QR code (qrcode + Pillow are installed on first use)."""
import re

import aio

aio.ensure_packages("qrcode", "qrcode[pil]")
import qrcode  # noqa: E402
import qrcode.image.svg  # noqa: E402

p = aio.params()


def esc(s):  # Wi-Fi QR format escaping
    return re.sub(r'([\;,":])', r"\\\1", s or "")


if p.get("kind") == "wifi":
    if not p.get("ssid"):
        aio.fail("Enter the network name")
    sec = p.get("security", "WPA")
    data = f"WIFI:T:{sec};S:{esc(p['ssid'])};" + (f"P:{esc(p.get('wifi_password'))};" if sec != "nopass" else "") + ";"
else:
    data = (p.get("text") or "").strip()
    if not data:
        aio.fail("Enter the text or link to encode")

ecc = {"L": qrcode.constants.ERROR_CORRECT_L, "M": qrcode.constants.ERROR_CORRECT_M,
       "Q": qrcode.constants.ERROR_CORRECT_Q, "H": qrcode.constants.ERROR_CORRECT_H}[p.get("ecc", "M")]
fmt = p.get("format", "png")
qr = qrcode.QRCode(error_correction=ecc, box_size=int(p.get("box_size") or 12), border=int(p.get("border") or 0))
try:
    qr.add_data(data)
    qr.make(fit=True)
except Exception as e:  # data too long for a QR code
    aio.fail(str(e))

name = re.sub(r"[^\w.-]+", "_", p.get("filename") or "qrcode").strip("_") or "qrcode"
dst = aio.unique(aio.out(f"{name}.{fmt}"))
img = qr.make_image(image_factory=qrcode.image.svg.SvgPathImage) if fmt == "svg" else qr.make_image()
img.save(str(dst))
aio.status(f"QR version {qr.version} ({qr.modules_count}×{qr.modules_count}), {len(data)} chars")
