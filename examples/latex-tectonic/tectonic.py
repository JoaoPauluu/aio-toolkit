"""Shared by setup.py and run.py: where to get Tectonic."""
import aio

VERSION = "0.17.0"
BASE = f"https://github.com/tectonic-typesetting/tectonic/releases/download/tectonic%40{VERSION}/tectonic-{VERSION}"
URLS = {
    # The container is Alpine Linux, so use the static musl builds.
    "amd64": f"{BASE}-x86_64-unknown-linux-musl.tar.gz",
    "arm64": f"{BASE}-aarch64-unknown-linux-musl.tar.gz",
}


def binary():
    # shared=True would put it on every plugin's PATH; keep it private here.
    return aio.ensure_binary("tectonic", URLS, version=VERSION)
