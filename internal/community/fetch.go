package community

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Version is what a spec resolves to at one point in time.
type Version struct {
	Ref    string `json:"ref"`    // tag or branch name (or the commit / "archive")
	Commit string `json:"commit"` // commit SHA, or "sha256:…" for archives
}

// Fetcher talks to hosting APIs. The zero value is not usable; see NewFetcher.
type Fetcher struct {
	Client   *http.Client
	Tokens   map[Kind]string // optional API tokens (private repos, rate limits)
	MaxBytes int64
	TempDir  string // where downloads are written ("" = os.TempDir)
	// GitHubAPI is the GitHub API base (overridable in tests).
	GitHubAPI string
}

func NewFetcher(maxBytes int64) *Fetcher {
	return &Fetcher{
		Client:    &http.Client{Timeout: 10 * time.Minute},
		Tokens:    map[Kind]string{},
		MaxBytes:  maxBytes,
		GitHubAPI: "https://api.github.com",
	}
}

var errNotFound = errors.New("not found")

func (f *Fetcher) apiBase(s Spec) string {
	switch s.Kind {
	case GitHub:
		return strings.TrimRight(f.GitHubAPI, "/") + "/repos/" + s.Repo
	case GitLab:
		return s.base() + "/api/v4/projects/" + url.PathEscape(s.Repo)
	default:
		return s.base() + "/api/v1/repos/" + s.Repo
	}
}

func (f *Fetcher) request(ctx context.Context, s Spec, rawURL, accept string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "aio-toolkit")
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	if tok := f.Tokens[s.Kind]; tok != "" {
		switch s.Kind {
		case GitHub:
			req.Header.Set("Authorization", "Bearer "+tok)
		case GitLab:
			req.Header.Set("PRIVATE-TOKEN", tok)
		case Gitea:
			req.Header.Set("Authorization", "token "+tok)
		}
	}
	res, err := f.Client.Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode == 404 {
		res.Body.Close()
		return nil, errNotFound
	}
	if res.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		res.Body.Close()
		msg := strings.TrimSpace(string(b))
		var j struct{ Message string }
		if json.Unmarshal(b, &j) == nil && j.Message != "" {
			msg = j.Message
		}
		if res.StatusCode == 403 && res.Header.Get("X-RateLimit-Remaining") == "0" {
			msg = "GitHub API rate limit reached; set AIO_GITHUB_TOKEN or try again later"
		}
		return nil, fmt.Errorf("%s: %s %s", req.URL.Host, res.Status, msg)
	}
	return res, nil
}

func (f *Fetcher) getJSON(ctx context.Context, s Spec, u string, v any) error {
	res, err := f.request(ctx, s, u, "application/json")
	if err != nil {
		return err
	}
	defer res.Body.Close()
	return json.NewDecoder(io.LimitReader(res.Body, 4<<20)).Decode(v)
}

// Resolve finds the version a spec currently points at. Without a ref it
// uses the latest release tag, falling back to the default branch.
func (f *Fetcher) Resolve(ctx context.Context, s Spec) (Version, error) {
	if s.Kind == Archive {
		// Archives have no API; hash the content to detect changes.
		tmp, sum, err := f.downloadArchive(ctx, s)
		if err != nil {
			return Version{}, err
		}
		os.Remove(tmp)
		return Version{Ref: "archive", Commit: sum}, nil
	}
	ref := s.Ref
	if ref == "" {
		var err error
		if ref, err = f.latestRelease(ctx, s); err != nil {
			return Version{}, err
		}
	}
	if ref == "" {
		var err error
		if ref, err = f.defaultBranch(ctx, s); err != nil {
			return Version{}, err
		}
	}
	sha, err := f.commit(ctx, s, ref)
	if err != nil {
		return Version{}, err
	}
	return Version{Ref: ref, Commit: sha}, nil
}

func (f *Fetcher) latestRelease(ctx context.Context, s Spec) (string, error) {
	var tag string
	var err error
	switch s.Kind {
	case GitHub, Gitea:
		var rel struct {
			TagName string `json:"tag_name"`
		}
		err = f.getJSON(ctx, s, f.apiBase(s)+"/releases/latest", &rel)
		tag = rel.TagName
	case GitLab:
		var rels []struct {
			TagName string `json:"tag_name"`
		}
		err = f.getJSON(ctx, s, f.apiBase(s)+"/releases?per_page=1&order_by=released_at", &rels)
		if len(rels) > 0 {
			tag = rels[0].TagName
		}
	}
	if errors.Is(err, errNotFound) {
		return "", nil // no releases (a 404 for the repo itself shows up in defaultBranch)
	}
	return tag, err
}

func (f *Fetcher) defaultBranch(ctx context.Context, s Spec) (string, error) {
	var repo struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := f.getJSON(ctx, s, f.apiBase(s), &repo); err != nil {
		if errors.Is(err, errNotFound) {
			return "", fmt.Errorf("repository %s not found (private repos need a token)", s.Repo)
		}
		return "", err
	}
	if repo.DefaultBranch == "" {
		return "", fmt.Errorf("repository %s has no default branch (empty repo?)", s.Repo)
	}
	return repo.DefaultBranch, nil
}

func (f *Fetcher) commit(ctx context.Context, s Spec, ref string) (string, error) {
	var sha string
	var err error
	switch s.Kind {
	case GitHub:
		var res *http.Response
		res, err = f.request(ctx, s, f.apiBase(s)+"/commits/"+url.PathEscape(ref), "application/vnd.github.sha")
		if err == nil {
			b, _ := io.ReadAll(io.LimitReader(res.Body, 128))
			res.Body.Close()
			sha = strings.TrimSpace(string(b))
		}
	case GitLab:
		var c struct {
			ID string `json:"id"`
		}
		err = f.getJSON(ctx, s, f.apiBase(s)+"/repository/commits/"+url.PathEscape(ref), &c)
		sha = c.ID
	case Gitea:
		var cs []struct {
			SHA string `json:"sha"`
		}
		err = f.getJSON(ctx, s, f.apiBase(s)+"/commits?limit=1&stat=false&sha="+url.QueryEscape(ref), &cs)
		if len(cs) > 0 {
			sha = cs[0].SHA
		}
	}
	if errors.Is(err, errNotFound) || err == nil && sha == "" {
		return "", fmt.Errorf("%s: ref %q not found", s.Repo, ref)
	}
	if err != nil {
		return "", err
	}
	if len(sha) < 7 || len(sha) > 64 || strings.ContainsAny(sha, "/ \n") {
		return "", fmt.Errorf("%s: unexpected commit id %q", s.Repo, sha)
	}
	return sha, nil
}

// Download fetches the archive for version v into a temp file and returns
// its path and the version actually downloaded (archives may have changed
// since they were checked). The caller removes the file.
func (f *Fetcher) Download(ctx context.Context, s Spec, v Version) (string, Version, error) {
	if s.Kind == Archive {
		tmp, sum, err := f.downloadArchive(ctx, s)
		return tmp, Version{Ref: "archive", Commit: sum}, err
	}
	var u string
	switch s.Kind {
	case GitHub:
		u = f.apiBase(s) + "/tarball/" + v.Commit
	case GitLab:
		u = f.apiBase(s) + "/repository/archive.tar.gz?sha=" + url.QueryEscape(v.Commit)
	default:
		u = f.apiBase(s) + "/archive/" + v.Commit + ".tar.gz"
	}
	res, err := f.request(ctx, s, u, "")
	if err != nil {
		if errors.Is(err, errNotFound) {
			return "", v, fmt.Errorf("archive for %s@%s not found", s.Repo, short(v.Commit))
		}
		return "", v, err
	}
	defer res.Body.Close()
	tmp, _, err := f.save(res.Body)
	return tmp, v, err
}

func (f *Fetcher) downloadArchive(ctx context.Context, s Spec) (string, string, error) {
	res, err := f.request(ctx, s, s.URL, "")
	if err != nil {
		if errors.Is(err, errNotFound) {
			return "", "", fmt.Errorf("%s: not found", s.URL)
		}
		return "", "", err
	}
	defer res.Body.Close()
	return f.save(res.Body)
}

func (f *Fetcher) save(r io.Reader) (string, string, error) {
	tmp, err := os.CreateTemp(f.TempDir, "download-*.archive")
	if err != nil {
		return "", "", err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(r, f.MaxBytes+1))
	tmp.Close()
	if err == nil && n > f.MaxBytes {
		err = fmt.Errorf("download is larger than %d MB", f.MaxBytes>>20)
	}
	if err != nil {
		os.Remove(tmp.Name())
		return "", "", err
	}
	return tmp.Name(), "sha256:" + hex.EncodeToString(h.Sum(nil))[:16], nil
}

func short(c string) string {
	c = strings.TrimPrefix(c, "sha256:")
	if len(c) > 10 {
		return c[:10]
	}
	return c
}
