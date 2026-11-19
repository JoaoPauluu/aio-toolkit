// Package fsutil holds small filesystem helpers shared by the job runner and file shares.
package fsutil

import (
	"path/filepath"
	"regexp"
	"strings"
)

var unsafeChars = regexp.MustCompile(`[^\p{L}\p{N}._ ()\[\]+-]+`)

// SanitizeFilename reduces a client-supplied name to a safe base name.
func SanitizeFilename(name string) string {
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	name = strings.TrimSpace(unsafeChars.ReplaceAllString(name, "_"))
	if name == "" || name == "." || name == ".." {
		name = "file"
	}
	if len(name) > 180 {
		ext := filepath.Ext(name)
		if len(ext) > 20 {
			ext = ""
		}
		name = name[:180-len(ext)] + ext
	}
	return name
}
