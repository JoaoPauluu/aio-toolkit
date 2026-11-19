"""Convert media between formats with ffmpeg."""
import aio
from aio_media import ffmpeg, out_for, probe

p = aio.params()
ext = p.get("container", "mp4")
VIDEO = {"mp4", "mkv", "webm", "mov"}
is_video_out = ext in VIDEO

ENC = {"h264": "libx264", "h265": "libx265", "vp9": "libvpx-vp9", "av1": "libsvtav1"}
# codecs each container accepts (for "auto" copy decisions) and the default re-encode choice
OK_V = {"mp4": {"h264", "hevc", "av1", "mpeg4"}, "mov": {"h264", "hevc", "prores", "mpeg4"},
        "webm": {"vp8", "vp9", "av1"}, "mkv": None}
DEF_V = {"mp4": "h264", "mov": "h264", "webm": "vp9", "mkv": "h264"}
OK_A = {"mp4": {"aac", "mp3", "alac", "opus", "ac3", "eac3"}, "mov": {"aac", "alac", "mp3", "pcm_s16le"},
        "webm": {"opus", "vorbis"}, "mkv": None}
DEF_A = {"mp4": "aac", "mov": "aac", "webm": "libopus", "mkv": "aac",
         "mp3": "libmp3lame", "m4a": "aac", "opus": "libopus", "ogg": "libvorbis", "flac": "flac", "wav": "pcm_s16le"}

files = p["files"]
for i, src in enumerate(files):
    info = probe(src)
    lo, hi = i / len(files) * 100, (i + 1) / len(files) * 100
    aio.status(f"Converting {aio.original_name(src)} → {ext} ({i + 1}/{len(files)})")
    args = ["-i", src]
    scale = p.get("height", "keep") != "keep" and is_video_out

    if is_video_out:
        if not info.video:
            aio.fail(f"{aio.original_name(src)} has no video stream; pick an audio format")
        args += ["-map", "0:v:0", "-map", "0:a?", "-map_metadata", "0"]
        vc = p.get("vcodec", "auto")
        if vc == "auto":
            ok = OK_V[ext]
            vc = "copy" if not scale and (ok is None or info.vcodec in ok) else DEF_V[ext]
        if vc == "copy":
            if scale:
                aio.fail("Changing resolution requires re-encoding; pick a codec other than Copy")
            args += ["-c:v", "copy"]
        else:
            enc = ENC[vc]
            crf = int(p.get("crf") or 23)
            args += ["-c:v", enc]
            if enc in ("libx264", "libx265"):
                args += ["-crf", crf, "-preset", p.get("preset", "medium"), "-pix_fmt", "yuv420p"]
            elif enc == "libvpx-vp9":
                args += ["-crf", min(crf + 8, 63), "-b:v", "0", "-row-mt", "1",
                         "-deadline", "good", "-cpu-used", "4" if p.get("preset") in ("ultrafast", "veryfast", "fast") else "2"]
            else:  # svt-av1
                presets = {"ultrafast": 12, "veryfast": 10, "fast": 8, "medium": 7, "slow": 5, "veryslow": 3}
                args += ["-crf", min(crf + 8, 63), "-preset", presets.get(p.get("preset"), 7)]
            if enc == "libx265" and ext in ("mp4", "mov"):
                args += ["-tag:v", "hvc1"]  # so Apple players recognise HEVC
        if scale:
            h = int(p["height"])
            if info.height and h >= info.height:
                aio.log(f"Source is {info.height}p; not upscaling")
            else:
                args += ["-vf", f"scale=-2:{h}"]
        if info.audio:
            ok = OK_A[ext]
            if ok is None or info.acodec in ok:
                args += ["-c:a", "copy"]
            else:
                args += ["-c:a", DEF_A[ext], "-b:a", p.get("abitrate", "192k")]
        if ext in ("mp4", "mov"):
            args += ["-movflags", "+faststart"]
        args += ["-map", "0:s?", "-c:s", "copy"] if ext == "mkv" else ["-sn"]
    else:
        if not info.audio:
            aio.fail(f"{aio.original_name(src)} has no audio stream")
        args += ["-map", "0:a:0", "-vn", "-map_metadata", "0", "-c:a", DEF_A[ext]]
        if ext not in ("flac", "wav"):
            args += ["-b:a", p.get("abitrate", "192k")]

    dst = out_for(src, ext)
    ffmpeg(args + [dst], info.duration, lo, hi)
    aio.log(f"{aio.original_name(src)} → {dst.name} ({aio.human(dst.stat().st_size)})")

aio.status(f"Converted {len(files)} file(s) to {ext}")
