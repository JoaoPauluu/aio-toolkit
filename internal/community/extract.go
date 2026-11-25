package community

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Extract unpacks a .zip or .tar.gz archive into dest (which must not
// exist), refusing entries that would land outside it, links that point
// outside it, special files, and archives that unpack to more than maxBytes.
// If everything sits inside one top-level folder (as in GitHub tarballs),
// that folder becomes dest.
func Extract(archive, dest string, maxBytes int64) error {
	raw := dest + ".raw"
	os.RemoveAll(raw)
	if err := os.MkdirAll(raw, 0o755); err != nil {
		return err
	}
	defer os.RemoveAll(raw)

	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	br := bufio.NewReader(f)
	magic, _ := br.Peek(4)
	x := &extractor{root: raw, max: maxBytes}
	switch {
	case len(magic) >= 4 && string(magic[:4]) == "PK\x03\x04":
		fi, err := f.Stat()
		if err != nil {
			return err
		}
		zr, err := zip.NewReader(f, fi.Size())
		if err != nil {
			return fmt.Errorf("bad zip: %w", err)
		}
		err = x.zip(zr)
		if err != nil {
			return err
		}
	case len(magic) >= 2 && magic[0] == 0x1f && magic[1] == 0x8b:
		gz, err := gzip.NewReader(br)
		if err != nil {
			return fmt.Errorf("bad gzip: %w", err)
		}
		if err := x.tar(tar.NewReader(gz)); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported archive (expected .zip or .tar.gz)")
	}
	if err := x.finishLinks(); err != nil {
		return err
	}

	top := raw
	entries, err := os.ReadDir(raw)
	if err != nil {
		return err
	}
	visible := entries[:0]
	for _, e := range entries {
		if e.Name() != "pax_global_header" {
			visible = append(visible, e)
		}
	}
	if len(visible) == 1 && visible[0].IsDir() {
		top = filepath.Join(raw, visible[0].Name())
	}
	return os.Rename(top, dest)
}

type extractor struct {
	root  string
	max   int64
	total int64
	links [][2]string // deferred symlinks: path, target
}

// target maps an archive entry name to a path inside root.
func (x *extractor) target(name string) (string, error) {
	name = strings.ReplaceAll(name, "\\", "/")
	if strings.HasPrefix(name, "/") || filepath.VolumeName(name) != "" {
		return "", fmt.Errorf("archive entry %q has an absolute path", name)
	}
	clean := path.Clean(name)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("archive entry %q escapes the archive", name)
	}
	if clean == "." {
		return "", nil
	}
	return filepath.Join(x.root, filepath.FromSlash(clean)), nil
}

func (x *extractor) file(dst string, r io.Reader, mode fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	perm := fs.FileMode(0o644)
	if mode&0o111 != 0 {
		perm = 0o755
	}
	w, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	n, err := io.Copy(w, io.LimitReader(r, x.max-x.total+1))
	w.Close()
	x.total += n
	if err != nil {
		return err
	}
	if x.total > x.max {
		return fmt.Errorf("plugin unpacks to more than %d MB", x.max>>20)
	}
	return nil
}

func (x *extractor) link(dst, target string) error {
	if filepath.IsAbs(target) || strings.HasPrefix(target, "/") {
		return fmt.Errorf("symlink %q points to an absolute path", target)
	}
	resolved := filepath.Join(filepath.Dir(dst), filepath.FromSlash(target))
	if rel, err := filepath.Rel(x.root, resolved); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("symlink %q points outside the plugin", target)
	}
	// Created after all files so no later entry can be written through it.
	x.links = append(x.links, [2]string{dst, target})
	return nil
}

func (x *extractor) finishLinks() error {
	// A link inside a linked folder could resolve differently than the
	// lexical check assumed, so links may not live under other links.
	set := map[string]bool{}
	for _, l := range x.links {
		set[l[0]] = true
	}
	for _, l := range x.links {
		for d := filepath.Dir(l[0]); len(d) > len(x.root); d = filepath.Dir(d) {
			if set[d] {
				return fmt.Errorf("symlink %q is inside another symlink", l[0])
			}
		}
	}
	for _, l := range x.links {
		if err := os.MkdirAll(filepath.Dir(l[0]), 0o755); err != nil {
			return err
		}
		os.Remove(l[0])
		if err := os.Symlink(l[1], l[0]); err != nil {
			return err
		}
	}
	return nil
}

func (x *extractor) tar(tr *tar.Reader) error {
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("bad tar: %w", err)
		}
		dst, err := x.target(h.Name)
		if err != nil {
			return err
		}
		if dst == "" {
			continue
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dst, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := x.file(dst, tr, fs.FileMode(h.Mode)); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := x.link(dst, h.Linkname); err != nil {
				return err
			}
		case tar.TypeXGlobalHeader, tar.TypeXHeader:
			// metadata only
		default:
			return fmt.Errorf("archive entry %q has an unsupported type (hard links and devices are not allowed)", h.Name)
		}
	}
}

func (x *extractor) zip(zr *zip.Reader) error {
	for _, zf := range zr.File {
		dst, err := x.target(zf.Name)
		if err != nil {
			return err
		}
		if dst == "" {
			continue
		}
		mode := zf.Mode()
		switch {
		case mode.IsDir():
			if err := os.MkdirAll(dst, 0o755); err != nil {
				return err
			}
		case mode&fs.ModeSymlink != 0:
			rc, err := zf.Open()
			if err != nil {
				return err
			}
			b, _ := io.ReadAll(io.LimitReader(rc, 4096))
			rc.Close()
			if err := x.link(dst, string(b)); err != nil {
				return err
			}
		case mode.IsRegular():
			rc, err := zf.Open()
			if err != nil {
				return err
			}
			err = x.file(dst, rc, mode)
			rc.Close()
			if err != nil {
				return err
			}
		default:
			return fmt.Errorf("archive entry %q has an unsupported type", zf.Name)
		}
	}
	return nil
}
