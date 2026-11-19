"""Extract the audio stream from video files."""
import aio
from aio_media import AUDIO_EXT_FOR_CODEC, ffmpeg, out_for, probe

p = aio.params()
fmt = p.get("format", "original")
ENC = {"mp3": "libmp3lame", "m4a": "aac", "opus": "libopus", "flac": "flac", "wav": "pcm_s16le"}
files = p["files"]
for i, src in enumerate(files):
    info = probe(src)
    if not info.audio:
        aio.fail(f"{aio.original_name(src)} has no audio track")
    aio.status(f"Extracting audio from {aio.original_name(src)} ({i + 1}/{len(files)})")
    args = ["-i", src, "-map", "0:a:0", "-vn", "-sn", "-map_metadata", "0"]
    if fmt == "original":
        ext = AUDIO_EXT_FOR_CODEC.get(info.acodec, "mka")
        args += ["-c:a", "copy"]
        aio.log(f"Audio codec is {info.acodec}; copying into .{ext}")
    else:
        ext = fmt
        args += ["-c:a", ENC[fmt]]
        if fmt in ("mp3", "m4a", "opus"):
            args += ["-b:a", p.get("bitrate", "192k")]
    if ext == "m4a":
        args += ["-movflags", "+faststart"]
    dst = out_for(src, ext)
    ffmpeg(args + [dst], info.duration, i / len(files) * 100, (i + 1) / len(files) * 100)
aio.status(f"Extracted audio from {len(files)} file(s)")
