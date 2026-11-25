// Package community installs plugins from Git hosting sites or archive URLs
// into a persistent folder, keeps them updated and can roll them back.
package community

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"net/url"
	"path"
	"strings"
)

// Kind is the type of host a plugin comes from.
type Kind string

const (
	GitHub  Kind = "github"
	GitLab  Kind = "gitlab"
	Gitea   Kind = "gitea" // Gitea, Forgejo, Codeberg
	Archive Kind = "archive"
)

// Spec identifies where a plugin comes from.
type Spec struct {
	Kind   Kind   `json:"kind"`
	Scheme string `json:"scheme,omitempty"` // https unless the user typed http://
	Host   string `json:"host,omitempty"`
	Repo   string `json:"repo,omitempty"`   // owner/name (GitLab: group/sub/name)
	Ref    string `json:"ref,omitempty"`    // tag, branch or commit; empty = latest release, else default branch
	Subdir string `json:"subdir,omitempty"` // folder inside the repo holding the plugin(s)
	URL    string `json:"url,omitempty"`    // archive kind: the download URL
}

// Canonical is a stable string form used for identity and display.
func (s Spec) Canonical() string {
	if s.Kind == Archive {
		return s.URL
	}
	c := s.Host + "/" + s.Repo
	if s.Ref != "" {
		c += "@" + s.Ref
	}
	if s.Subdir != "" {
		c += "#" + s.Subdir
	}
	return c
}

// ID is a short stable identifier for the source.
func (s Spec) ID() string {
	c := s.Canonical()
	if s.Kind != Archive {
		c = strings.ToLower(c)
	}
	h := sha1.Sum([]byte(c))
	return hex.EncodeToString(h[:5])
}

// SameRepo reports whether two specs point at the same repository folder,
// regardless of ref (used to match `requires` against installed sources).
func (s Spec) SameRepo(o Spec) bool {
	if s.Kind == Archive || o.Kind == Archive {
		return s.URL == o.URL
	}
	return strings.EqualFold(s.Host, o.Host) && strings.EqualFold(s.Repo, o.Repo) && s.Subdir == o.Subdir
}

func (s Spec) base() string {
	scheme := s.Scheme
	if scheme == "" {
		scheme = "https"
	}
	return scheme + "://" + s.Host
}

// WebURL is a browsable link to the source.
func (s Spec) WebURL() string {
	switch s.Kind {
	case Archive:
		return s.URL
	case GitHub:
		u := s.base() + "/" + s.Repo
		if s.Ref != "" || s.Subdir != "" {
			ref := s.Ref
			if ref == "" {
				ref = "HEAD"
			}
			u += "/tree/" + ref + "/" + s.Subdir
		}
		return strings.TrimSuffix(u, "/")
	case GitLab:
		u := s.base() + "/" + s.Repo
		if s.Ref != "" || s.Subdir != "" {
			ref := s.Ref
			if ref == "" {
				ref = "HEAD"
			}
			u += "/-/tree/" + ref + "/" + s.Subdir
		}
		return strings.TrimSuffix(u, "/")
	default:
		u := s.base() + "/" + s.Repo
		if s.Ref != "" {
			u += "/src/" + s.Ref + "/" + s.Subdir
		}
		return strings.TrimSuffix(u, "/")
	}
}

// CommitURL links to a specific commit (empty for archives).
func (s Spec) CommitURL(commit string) string {
	switch s.Kind {
	case GitHub:
		return s.base() + "/" + s.Repo + "/commit/" + commit
	case GitLab:
		return s.base() + "/" + s.Repo + "/-/commit/" + commit
	case Gitea:
		return s.base() + "/" + s.Repo + "/commit/" + commit
	}
	return ""
}

func isArchivePath(p string) bool {
	p = strings.ToLower(p)
	for _, ext := range []string{".zip", ".tar.gz", ".tgz"} {
		if strings.HasSuffix(p, ext) {
			return true
		}
	}
	return false
}

// ParseSpec understands, for example:
//
//	https://github.com/owner/repo
//	github.com/owner/repo@v1.2.0
//	https://github.com/owner/repo/tree/main/plugins/foo
//	owner/repo                          (GitHub)
//	https://gitlab.com/group/sub/repo/-/tree/main/foo
//	https://codeberg.org/owner/repo/src/branch/main/foo
//	https://example.com/my-plugin.zip   (any .zip / .tar.gz / .tgz)
//
// A trailing "@ref" selects a tag, branch or commit and "#path" a subfolder.
func ParseSpec(raw string) (Spec, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Spec{}, fmt.Errorf("enter a repository or archive URL")
	}
	in := raw
	if !strings.Contains(in, "://") {
		// "owner/repo" shorthand → GitHub
		if parts := strings.Split(strings.SplitN(strings.SplitN(in, "#", 2)[0], "@", 2)[0], "/"); len(parts) == 2 && !strings.Contains(parts[0], ".") {
			in = "github.com/" + in
		}
		in = "https://" + in
	}
	u, err := url.Parse(in)
	if err != nil || u.Host == "" {
		return Spec{}, fmt.Errorf("not a valid URL: %q", raw)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return Spec{}, fmt.Errorf("only http(s) URLs are supported")
	}
	if isArchivePath(u.Path) {
		return Spec{Kind: Archive, URL: u.String()}, nil
	}
	s := Spec{Host: strings.ToLower(u.Host), Subdir: cleanSub(u.Fragment)}
	if u.Scheme == "http" {
		s.Scheme = "http"
	}
	p := strings.Trim(u.Path, "/")
	if i := strings.LastIndex(p, "@"); i >= 0 {
		s.Ref = p[i+1:]
		p = p[:i]
		// "@ref/sub/dir" is allowed too
		if j := strings.Index(s.Ref, "/"); j >= 0 {
			if s.Subdir == "" {
				s.Subdir = cleanSub(s.Ref[j+1:])
			}
			s.Ref = s.Ref[:j]
		}
	}
	seg := strings.Split(p, "/")
	switch {
	case s.Host == "github.com" || s.Host == "www.github.com":
		s.Kind, s.Host = GitHub, "github.com"
		if len(seg) < 2 {
			return Spec{}, fmt.Errorf("GitHub URL needs owner/repo")
		}
		s.Repo = seg[0] + "/" + strings.TrimSuffix(seg[1], ".git")
		if len(seg) >= 4 && (seg[2] == "tree" || seg[2] == "blob") {
			s.Ref = seg[3]
			s.Subdir = cleanSub(strings.Join(seg[4:], "/"))
		} else if len(seg) > 2 {
			return Spec{}, fmt.Errorf("unsupported GitHub URL; use https://github.com/owner/repo or …/tree/<ref>/<folder>")
		}
	case s.Host == "gitlab.com" || strings.HasPrefix(s.Host, "gitlab."):
		s.Kind = GitLab
		repo, rest, _ := strings.Cut(p, "/-/")
		repo = strings.TrimSuffix(repo, ".git")
		if strings.Count(repo, "/") < 1 {
			return Spec{}, fmt.Errorf("GitLab URL needs group/repo")
		}
		s.Repo = repo
		if r := strings.Split(rest, "/"); len(r) >= 2 && (r[0] == "tree" || r[0] == "blob") {
			s.Ref = r[1]
			s.Subdir = cleanSub(strings.Join(r[2:], "/"))
		}
	default:
		// Codeberg and any other host: assume a Gitea/Forgejo API.
		s.Kind = Gitea
		if len(seg) < 2 {
			return Spec{}, fmt.Errorf("repository URL needs owner/repo (or use a direct .zip/.tar.gz link)")
		}
		s.Repo = seg[0] + "/" + strings.TrimSuffix(seg[1], ".git")
		if len(seg) >= 5 && seg[2] == "src" && (seg[3] == "branch" || seg[3] == "tag" || seg[3] == "commit") {
			s.Ref = seg[4]
			s.Subdir = cleanSub(strings.Join(seg[5:], "/"))
		} else if len(seg) > 2 {
			return Spec{}, fmt.Errorf("unsupported repository URL; use https://%s/owner/repo or …/src/branch/<ref>/<folder>", s.Host)
		}
	}
	if s.Ref != "" && !validRef(s.Ref) {
		return Spec{}, fmt.Errorf("invalid ref %q", s.Ref)
	}
	if s.Subdir == ".." || strings.HasPrefix(s.Subdir, "../") {
		return Spec{}, fmt.Errorf("invalid folder %q", s.Subdir)
	}
	return s, nil
}

func cleanSub(p string) string {
	p = strings.Trim(p, "/")
	if p == "" {
		return ""
	}
	c := path.Clean(p)
	if c == "." {
		return ""
	}
	return c
}

func validRef(r string) bool {
	if r == "" || strings.Contains(r, "..") || strings.HasPrefix(r, "-") {
		return false
	}
	for _, c := range r {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("._-+/", c)) {
			return false
		}
	}
	return true
}
