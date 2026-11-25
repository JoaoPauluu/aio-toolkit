import aio
import tectonic

path = tectonic.binary()
aio.run([path, "--version"])
aio.log(f"Tectonic ready at {path}. TeX packages are downloaded on first use and cached in /data/cache.")
