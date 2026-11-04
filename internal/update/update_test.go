package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestVersions(t *testing.T) {
	cases := []struct {
		cur, tag string
		newer    bool
		dev      bool
	}{
		{"v0.1.0", "v0.2.0", true, false},
		{"v0.2.0", "v0.2.0", false, false},
		{"v0.10.0", "v0.9.9", false, false},
		{"v1.0.0-rc.1", "v1.0.0", true, false},
		{"v1.0.0", "v1.0.1-rc.1", true, false},
		{"0.3.1", "v0.3.2", true, false},
		{"d7caa27", "v0.1.0", false, true},
		{"v0.1.0-3-gabc1234", "v0.2.0", false, true},
		{"v0.1.0-dirty", "v0.2.0", false, true},
		{"dev", "v0.2.0", false, true},
	}
	for _, c := range cases {
		n, err := Newer(c.cur, c.tag)
		if (err == ErrDevBuild) != c.dev || (err == nil && n != c.newer) {
			t.Errorf("Newer(%q, %q) = %v, %v", c.cur, c.tag, n, err)
		}
	}
}

func tarball(t *testing.T, name string, body []byte) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range []struct {
		n string
		b []byte
	}{{"README.md", []byte("hi")}, {name, body}} {
		_ = tw.WriteHeader(&tar.Header{Name: f.n, Mode: 0o755, Size: int64(len(f.b)), Typeflag: tar.TypeReg})
		_, _ = tw.Write(f.b)
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func TestDownloadVerifyReplace(t *testing.T) {
	bin := []byte("#!/bin/sh\necho new\n")
	arch := tarball(t, "baca_0.2.0_x/baca", bin)
	sum := sha256.Sum256(arch)
	name := AssetName("v0.2.0", runtime.GOOS, runtime.GOARCH)
	sums := fmt.Sprintf("%s  %s\n%s  other.tar.gz\n", hex.EncodeToString(sum[:]), name, hex.EncodeToString(sum[:]))
	bad := false
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/" + Repo + "/releases/latest":
			fmt.Fprintf(w, `{"tag_name":"v0.2.0","html_url":"https://github.com/%s/releases/tag/v0.2.0","assets":[{"name":%q,"browser_download_url":"%s/a"},{"name":"checksums.txt","browser_download_url":"%s/sums"}]}`, Repo, name, srv.URL, srv.URL)
		case "/repos/" + Repo + "/releases":
			fmt.Fprint(w, `[{"tag_name":"v0.3.0-rc.1","prerelease":true},{"tag_name":"v0.2.0"},{"tag_name":"v9.0.0","draft":true}]`)
		case "/a":
			if bad {
				w.Write(append(arch, 0))
				return
			}
			w.Write(arch)
		case "/sums":
			fmt.Fprint(w, sums)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	APIBase = srv.URL
	defer func() { APIBase = "https://api.github.com" }()

	c := &Client{Version: "v0.1.0"}
	ctx := context.Background()
	rel, err := c.Latest(ctx, false)
	if err != nil || rel.Tag != "v0.2.0" {
		t.Fatalf("latest: %+v %v", rel, err)
	}
	if pre, err := c.Latest(ctx, true); err != nil || pre.Tag != "v0.3.0-rc.1" {
		t.Fatalf("prerelease: %+v %v", pre, err)
	}
	got, err := c.Download(ctx, rel)
	if err != nil || !bytes.Equal(got, bin) {
		t.Fatalf("download: %q %v", got, err)
	}
	target := filepath.Join(t.TempDir(), "baca")
	_ = os.WriteFile(target, []byte("old"), 0o755)
	if err := Replace(target, got); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(target); !bytes.Equal(b, bin) {
		t.Errorf("target = %q", b)
	}
	if fi, _ := os.Stat(target); fi.Mode().Perm()&0o100 == 0 {
		t.Errorf("not executable: %v", fi.Mode())
	}
	bad = true
	if _, err := c.Download(ctx, rel); err == nil {
		t.Error("tampered archive must be rejected")
	}
}

func TestPrereleaseOrder(t *testing.T) {
	for _, c := range [][2]string{{"rc.9", "rc.10"}, {"alpha", "beta"}, {"rc.1", "rc.1.1"}, {"1", "alpha"}} {
		if !prereleaseLess(c[0], c[1]) || prereleaseLess(c[1], c[0]) {
			t.Errorf("%s < %s", c[0], c[1])
		}
	}
	if newer, err := Newer("v1.0.0-rc.9", "v1.0.0-rc.10"); err != nil || !newer {
		t.Errorf("rc.10 newer than rc.9: %v %v", newer, err)
	}
}

func TestManager(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"tag_name":"v1.2.0","html_url":"https://example.com/r"}`)
	}))
	defer srv.Close()
	old := APIBase
	APIBase = srv.URL
	defer func() { APIBase = old }()

	status := filepath.Join(t.TempDir(), "update.json")
	var told []string
	m := &Manager{Version: "v1.1.0", StatusPath: status, Notify: func(in Info) { told = append(told, in.Latest) }}
	in, err := m.Check(context.Background(), false)
	if err != nil || in.Latest != "v1.2.0" || in.URL != "https://example.com/r" || in.Dev {
		t.Fatalf("%v %+v", err, in)
	}
	m.Check(context.Background(), false) // told once per version
	if len(told) != 1 {
		t.Fatalf("notified %v", told)
	}
	// a new process sees the stored check
	m2 := &Manager{Version: "v1.1.0", StatusPath: status}
	if m2.Info().Latest != "v1.2.0" {
		t.Fatal("status not stored")
	}
	if (&Manager{Version: "dev", StatusPath: status}).Info().Dev != true {
		t.Fatal("dev build")
	}
	if err := (&Manager{Version: "v1.2.0-3-gabc", StatusPath: status}).Install(context.Background()); err == nil {
		t.Fatal("dev build installed")
	}
}
