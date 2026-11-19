// Package shares stores uploaded files behind unguessable public links.
//
// Each share lives in <dir>/<id>/ with a meta.json and a files/ folder.
package shares

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"aio/internal/fsutil"
)

type File struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

// Share is the persisted meta.json.
type Share struct {
	ID      string    `json:"id"`
	Created time.Time `json:"created"`
	Expires time.Time `json:"expires"` // zero: never
	Files   []File    `json:"files"`
	Note    string    `json:"note,omitempty"`
	Salt    string    `json:"salt,omitempty"`
	PwHash  string    `json:"pwhash,omitempty"`
}

// View is the admin-facing representation (no password material).
type View struct {
	ID          string     `json:"id"`
	Created     time.Time  `json:"created"`
	Expires     *time.Time `json:"expires,omitempty"`
	Files       []File     `json:"files"`
	Size        int64      `json:"size"`
	Note        string     `json:"note,omitempty"`
	HasPassword bool       `json:"has_password"`
}

func (s *Share) HasPassword() bool { return s.PwHash != "" }

func (s *Share) Expired(now time.Time) bool { return !s.Expires.IsZero() && !now.Before(s.Expires) }

func (s *Share) TotalSize() (n int64) {
	for _, f := range s.Files {
		n += f.Size
	}
	return
}

func (s *Share) View() View {
	v := View{ID: s.ID, Created: s.Created, Files: s.Files, Size: s.TotalSize(), Note: s.Note, HasPassword: s.HasPassword()}
	if !s.Expires.IsZero() {
		e := s.Expires
		v.Expires = &e
	}
	return v
}

// CheckPassword compares pw against the stored salted hash in constant time.
func (s *Share) CheckPassword(pw string) bool {
	if !s.HasPassword() {
		return true
	}
	return subtle.ConstantTimeCompare([]byte(hashPassword(s.Salt, pw)), []byte(s.PwHash)) == 1
}

func hashPassword(salt, pw string) string {
	h := sha256.Sum256([]byte(salt + pw))
	return hex.EncodeToString(h[:])
}

// Durations accepted for the "expires" field.
var Expiries = map[string]time.Duration{
	"1h": time.Hour, "1d": 24 * time.Hour, "7d": 7 * 24 * time.Hour, "30d": 30 * 24 * time.Hour, "never": 0,
}

var idRe = regexp.MustCompile(`^[A-Za-z0-9_-]{22}$`)

// ValidID reports whether id has the shape of a share id.
func ValidID(id string) bool { return idRe.MatchString(id) }

type Manager struct {
	dir    string
	secret []byte
}

// NewManager uses dataDir/shares for storage and dataDir/secret as the cookie HMAC key.
func NewManager(dataDir string) (*Manager, error) {
	m := &Manager{dir: filepath.Join(dataDir, "shares")}
	if err := os.MkdirAll(m.dir, 0o755); err != nil {
		return nil, err
	}
	secretPath := filepath.Join(dataDir, "secret")
	b, err := os.ReadFile(secretPath)
	if err != nil || len(b) < 32 {
		b = make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return nil, err
		}
		if err := os.WriteFile(secretPath, b, 0o600); err != nil {
			return nil, fmt.Errorf("write secret: %w", err)
		}
	}
	m.secret = b
	return m, nil
}

// Token is the cookie value proving the password for share id was entered.
func (m *Manager) Token(id string) string {
	mac := hmac.New(sha256.New, m.secret)
	mac.Write([]byte("share:" + id))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (m *Manager) CheckToken(id, tok string) bool {
	return hmac.Equal([]byte(tok), []byte(m.Token(id)))
}

func CookieName(id string) string { return "aio_s_" + id }

func newID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// FilePath returns the on-disk path of a file in the share, or "" if it isn't part of it.
func (m *Manager) FilePath(s *Share, name string) string {
	for _, f := range s.Files {
		if f.Name == name {
			return filepath.Join(m.dir, s.ID, "files", name)
		}
	}
	return ""
}

// Create reads a multipart body (file parts plus "expires", "password" and
// "note" fields) and stores it as a new share.
func (m *Manager) Create(mr *multipart.Reader) (*Share, error) {
	id, err := newID()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(m.dir, id)
	filesDir := filepath.Join(dir, "files")
	if err := os.MkdirAll(filesDir, 0o755); err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			os.RemoveAll(dir)
		}
	}()

	s := &Share{ID: id, Created: time.Now().UTC(), Files: []File{}}
	expires, password := "7d", ""
	used := map[string]bool{}
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("upload failed: %w", err)
		}
		if part.FileName() != "" {
			name := uniqueName(fsutil.SanitizeFilename(part.FileName()), used)
			n, err := save(filepath.Join(filesDir, name), part)
			if err != nil {
				return nil, fmt.Errorf("upload failed: %w", err)
			}
			s.Files = append(s.Files, File{Name: name, Size: n})
			continue
		}
		b, err := io.ReadAll(io.LimitReader(part, 64<<10))
		if err != nil {
			return nil, err
		}
		v := string(b)
		switch part.FormName() {
		case "expires":
			expires = strings.TrimSpace(v)
		case "password":
			password = v
		case "note":
			s.Note = strings.TrimSpace(v)
		}
	}
	if len(s.Files) == 0 {
		return nil, errors.New("no files uploaded")
	}
	d, valid := Expiries[expires]
	if !valid {
		return nil, fmt.Errorf("invalid expires %q (use 1h, 1d, 7d, 30d or never)", expires)
	}
	if d > 0 {
		s.Expires = s.Created.Add(d)
	}
	if len(s.Note) > 2000 {
		s.Note = s.Note[:2000]
	}
	if password != "" {
		salt := make([]byte, 16)
		if _, err := rand.Read(salt); err != nil {
			return nil, err
		}
		s.Salt = hex.EncodeToString(salt)
		s.PwHash = hashPassword(s.Salt, password)
	}
	if err := writeMeta(dir, s); err != nil {
		return nil, err
	}
	ok = true
	return s, nil
}

func uniqueName(name string, used map[string]bool) string {
	cand := name
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	for i := 2; used[strings.ToLower(cand)]; i++ {
		cand = fmt.Sprintf("%s (%d)%s", base, i, ext)
	}
	used[strings.ToLower(cand)] = true
	return cand
}

func save(dst string, r io.Reader) (int64, error) {
	f, err := os.Create(dst)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(f, r)
	if err != nil {
		f.Close()
		return n, err
	}
	return n, f.Close()
}

func writeMeta(dir string, s *Share) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, "meta.json.tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "meta.json"))
}

func (m *Manager) load(id string) (*Share, error) {
	b, err := os.ReadFile(filepath.Join(m.dir, id, "meta.json"))
	if err != nil {
		return nil, err
	}
	var s Share
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	s.ID = id
	return &s, nil
}

// Get returns the share, or nil if it doesn't exist or has expired.
func (m *Manager) Get(id string) *Share {
	if !ValidID(id) {
		return nil
	}
	s, err := m.load(id)
	if err != nil || s.Expired(time.Now()) {
		return nil
	}
	return s
}

// List returns all live shares, newest first.
func (m *Manager) List() []*Share {
	ents, _ := os.ReadDir(m.dir)
	now := time.Now()
	out := []*Share{}
	for _, e := range ents {
		if !e.IsDir() || !ValidID(e.Name()) {
			continue
		}
		if s, err := m.load(e.Name()); err == nil && !s.Expired(now) {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out
}

// Delete removes a share; it reports false if it didn't exist.
func (m *Manager) Delete(id string) bool {
	if !ValidID(id) {
		return false
	}
	dir := filepath.Join(m.dir, id)
	if _, err := os.Stat(dir); err != nil {
		return false
	}
	return os.RemoveAll(dir) == nil
}

// Cleanup deletes expired shares and abandoned partial uploads.
func (m *Manager) Cleanup() {
	ents, _ := os.ReadDir(m.dir)
	now := time.Now()
	for _, e := range ents {
		if !e.IsDir() || !ValidID(e.Name()) {
			continue
		}
		s, err := m.load(e.Name())
		if err != nil {
			// no meta.json: an upload in progress, or one that died mid-way
			if info, ierr := e.Info(); ierr == nil && now.Sub(info.ModTime()) > 24*time.Hour {
				os.RemoveAll(filepath.Join(m.dir, e.Name()))
			}
			continue
		}
		if s.Expired(now) {
			if err := os.RemoveAll(filepath.Join(m.dir, s.ID)); err == nil {
				log.Printf("share %s expired, removed", s.ID)
			}
		}
	}
}

// RunCleanup calls Cleanup now and then every interval until ctx is done.
func (m *Manager) RunCleanup(ctx context.Context, interval time.Duration) {
	m.Cleanup()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.Cleanup()
		}
	}
}
