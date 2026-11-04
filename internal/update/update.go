// Package update checks GitHub releases for a newer baca and installs it:
// download the archive for this OS/arch, verify it against checksums.txt,
// swap the binary in place and re-exec.
package update

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Repo is where releases are published; set at build time with
// -ldflags "-X baca/internal/update.Repo=owner/name".
var Repo = "piciuok/baca"

// APIBase is the GitHub API root; BACA_UPDATE_API points it elsewhere
// (a mirror, or a local server in tests).
var APIBase = func() string {
	if v := os.Getenv("BACA_UPDATE_API"); v != "" {
		return strings.TrimSuffix(v, "/")
	}
	return "https://api.github.com"
}()

// maxDownload caps an archive download.
const maxDownload = 200 << 20

// ErrDevBuild: the running binary was not built from a release tag, so there
// is nothing to compare against.
var ErrDevBuild = errors.New("development build (no release version)")

type Asset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	Size int64  `json:"size"`
}

type Release struct {
	Tag         string    `json:"tag_name"`
	Name        string    `json:"name"`
	URL         string    `json:"html_url"`
	Body        string    `json:"body"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	PublishedAt time.Time `json:"published_at"`
	Assets      []Asset   `json:"assets"`
}

// Version is a parsed vX.Y.Z[-pre].
type Version struct {
	Major, Minor, Patch int
	Pre                 string
}

var semverRe = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)(?:-([0-9A-Za-z.-]+))?$`)

// ParseVersion accepts release tags. `git describe` output past a tag
// ("v0.2.0-3-gabc123") and dirty builds are development builds.
func ParseVersion(s string) (Version, error) {
	m := semverRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil || strings.Contains(s, "dirty") || regexp.MustCompile(`-\d+-g[0-9a-f]+`).MatchString(s) {
		return Version{}, ErrDevBuild
	}
	var v Version
	v.Major, _ = strconv.Atoi(m[1])
	v.Minor, _ = strconv.Atoi(m[2])
	v.Patch, _ = strconv.Atoi(m[3])
	v.Pre = m[4]
	return v, nil
}

// Less orders versions; a pre-release sorts before its release.
func (a Version) Less(b Version) bool {
	if a.Major != b.Major {
		return a.Major < b.Major
	}
	if a.Minor != b.Minor {
		return a.Minor < b.Minor
	}
	if a.Patch != b.Patch {
		return a.Patch < b.Patch
	}
	switch {
	case a.Pre == b.Pre:
		return false
	case a.Pre == "":
		return false
	case b.Pre == "":
		return true
	}
	return prereleaseLess(a.Pre, b.Pre)
}

// prereleaseLess orders pre-release tags the semver way: dot-separated
// identifiers, numeric ones compared as numbers (rc.9 < rc.10), numeric
// before alphanumeric, a shorter list first when it is a prefix.
func prereleaseLess(a, b string) bool {
	x, y := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(x) && i < len(y); i++ {
		if x[i] == y[i] {
			continue
		}
		nx, ex := strconv.Atoi(x[i])
		ny, ey := strconv.Atoi(y[i])
		switch {
		case ex == nil && ey == nil:
			return nx < ny
		case ex == nil:
			return true
		case ey == nil:
			return false
		}
		return x[i] < y[i]
	}
	return len(x) < len(y)
}

// Newer reports whether tag is a newer release than current.
func Newer(current, tag string) (bool, error) {
	cur, err := ParseVersion(current)
	if err != nil {
		return false, err
	}
	t, err := ParseVersion(tag)
	if err != nil {
		return false, fmt.Errorf("release tag %q: not a version", tag)
	}
	return cur.Less(t), nil
}

// Client talks to GitHub; Version goes into the User-Agent.
type Client struct {
	Version string
	HTTP    *http.Client
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 5 * time.Minute}
}

func (c *Client) get(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "baca/"+c.Version)
	if strings.HasPrefix(url, APIBase) {
		req.Header.Set("Accept", "application/vnd.github+json")
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("GET %s: %s %s", url, resp.Status, strings.TrimSpace(string(b)))
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("GET %s: response too large", url)
	}
	return b, nil
}

// Latest returns the newest published release (pre-releases only when
// asked for).
func (c *Client) Latest(ctx context.Context, prerelease bool) (*Release, error) {
	if !prerelease {
		b, err := c.get(ctx, APIBase+"/repos/"+Repo+"/releases/latest", 4<<20)
		if err != nil {
			return nil, err
		}
		var r Release
		return &r, json.Unmarshal(b, &r)
	}
	b, err := c.get(ctx, APIBase+"/repos/"+Repo+"/releases?per_page=20", 8<<20)
	if err != nil {
		return nil, err
	}
	var rs []Release
	if err := json.Unmarshal(b, &rs); err != nil {
		return nil, err
	}
	var best *Release
	var bestV Version
	for i := range rs {
		v, err := ParseVersion(rs[i].Tag)
		if rs[i].Draft || err != nil {
			continue
		}
		if best == nil || bestV.Less(v) {
			best, bestV = &rs[i], v
		}
	}
	if best == nil {
		return nil, errors.New("no releases yet")
	}
	return best, nil
}

// AssetName is the archive for an OS/arch, as the release workflow names it.
func AssetName(tag, goos, goarch string) string {
	return fmt.Sprintf("baca_%s_%s_%s.tar.gz", strings.TrimPrefix(tag, "v"), goos, goarch)
}

func (r *Release) asset(name string) (Asset, bool) {
	for _, a := range r.Assets {
		if a.Name == name {
			return a, true
		}
	}
	return Asset{}, false
}

// Download fetches this platform's binary from the release and verifies it
// against checksums.txt. It returns the binary's bytes.
func (c *Client) Download(ctx context.Context, r *Release) ([]byte, error) {
	name := AssetName(r.Tag, runtime.GOOS, runtime.GOARCH)
	a, ok := r.asset(name)
	if !ok {
		return nil, fmt.Errorf("release %s has no %s", r.Tag, name)
	}
	sums, ok := r.asset("checksums.txt")
	if !ok {
		return nil, fmt.Errorf("release %s has no checksums.txt", r.Tag)
	}
	sumFile, err := c.get(ctx, sums.URL, 1<<20)
	if err != nil {
		return nil, err
	}
	want, err := checksumFor(sumFile, name)
	if err != nil {
		return nil, err
	}
	archive, err := c.get(ctx, a.URL, maxDownload)
	if err != nil {
		return nil, err
	}
	got := sha256.Sum256(archive)
	if hex.EncodeToString(got[:]) != want {
		return nil, fmt.Errorf("%s: checksum mismatch — not installing", name)
	}
	return extractBinary(archive)
}

// checksumFor reads "<sha256>  <file>" lines (sha256sum format).
func checksumFor(sums []byte, name string) (string, error) {
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == name && len(f[0]) == 64 {
			return strings.ToLower(f[0]), nil
		}
	}
	return "", fmt.Errorf("checksums.txt has no entry for %s", name)
}

// extractBinary returns the "baca" file from a .tar.gz.
func extractBinary(archive []byte) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil, errors.New("archive has no baca binary")
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag == tar.TypeReg && filepath.Base(h.Name) == "baca" {
			return io.ReadAll(io.LimitReader(tr, maxDownload))
		}
	}
}

// Replace atomically swaps the file at target for bin (same directory, so
// the rename is atomic; the running process keeps its old inode).
func Replace(target string, bin []byte) error {
	if fi, err := os.Lstat(target); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		// e.g. Homebrew links into its Cellar: writing there corrupts its install
		p, _ := filepath.EvalSymlinks(target)
		return fmt.Errorf("%s links to %s — update baca with the tool that installed it", target, p)
	}
	tmp := target + ".update"
	if err := writeSynced(tmp, bin, 0o755); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, target); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// Install downloads the release's binary for this platform and swaps it in
// at exe.
func (c *Client) Install(ctx context.Context, r *Release, exe string) error {
	bin, err := c.Download(ctx, r)
	if err != nil {
		return err
	}
	return Replace(exe, bin)
}

// Status is what the tray remembers between checks.
type Status struct {
	CheckedAt time.Time `json:"checked_at"`
	Latest    string    `json:"latest,omitempty"`
	URL       string    `json:"url,omitempty"`
	Notified  string    `json:"notified,omitempty"` // version we already told the user about
	Error     string    `json:"error,omitempty"`
	// UpdatedFrom is set right before the restart into a new version, so
	// the new process can say the update is done.
	UpdatedFrom string `json:"updated_from,omitempty"`
}

func LoadStatus(path string) Status {
	var s Status
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &s)
	}
	return s
}

func SaveStatus(path string, s Status) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := writeSynced(path+".tmp", b, 0o600); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}

// writeSynced writes and fsyncs a file, so a rename over the old one never
// leaves an empty file after a power loss.
func writeSynced(path string, b []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(path, mode)
	}
	return err
}
