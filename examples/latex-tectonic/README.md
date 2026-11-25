# LaTeX → PDF (example community plugin)

Compiles a `.tex` file, a zipped LaTeX project or pasted LaTeX into a PDF with [Tectonic](https://tectonic-typesetting.github.io/).

It shows how a community plugin can bring its own toolchain: `setup.py` runs right after install/update and calls
`aio.ensure_binary()`, which downloads the static (musl) Tectonic build for the container's architecture into the
plugin's persistent `bin/` folder. TeX packages are fetched on first use and cached in `/data/cache`.

Other plugins can find it with `aio.find("latex.compile")` and run it with `aio.call("latex-tectonic", …)`.

Install: paste `https://github.com/JoaoPauluu/aio-toolkit/tree/main/examples/latex-tectonic` into **Community plugins**.
Copy this folder into its own repository to start a plugin of your own.
