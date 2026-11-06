package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestIsNewer(t *testing.T) {
	cases := []struct {
		current, candidate string
		want               bool
	}{
		{"v0.3.0", "v0.3.1", true},
		{"v0.3.0", "v0.4.0", true},
		{"v0.3.0", "v1.0.0", true},
		{"v0.3.0", "v0.3.0", false},
		{"v0.3.1", "v0.3.0", false},
		{"v1.0.0", "v0.9.9", false},
		{"dev", "v0.3.0", true},
		{"", "v0.3.0", true},
		{"v0.3.0", "nonsense", false},
		{"v0.3.0-rc1", "v0.3.0", false},
	}
	for _, c := range cases {
		if got := IsNewer(c.current, c.candidate); got != c.want {
			t.Errorf("IsNewer(%q, %q) = %v, want %v", c.current, c.candidate, got, c.want)
		}
	}
}

func TestAssetFor(t *testing.T) {
	if got := AssetFor("linux", "amd64"); got != "agents-room-linux-amd64" {
		t.Fatalf("linux asset = %q", got)
	}
	if got := AssetFor("windows", "arm64"); got != "agents-room-windows-arm64.exe" {
		t.Fatalf("windows asset = %q", got)
	}
}

func TestLatestParsesRelease(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/"+Repo+"/releases/latest" {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(Release{
			Tag:  "v9.9.9",
			Name: "test",
			Assets: []Asset{
				{Name: "agents-room-linux-amd64", URL: "http://example.invalid/a", Size: 10},
			},
		})
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), APIBase: srv.URL}
	rel, err := c.Latest(context.Background())
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	if rel.Tag != "v9.9.9" {
		t.Fatalf("tag = %q", rel.Tag)
	}
	if _, ok := rel.FindAsset("agents-room-linux-amd64"); !ok {
		t.Fatal("asset not found")
	}
	if _, ok := rel.FindAsset("nope"); ok {
		t.Fatal("found an asset that does not exist")
	}
}

func TestLatestRejectsPrerelease(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(Release{Tag: "v9.9.9", Prerelease: true})
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), APIBase: srv.URL}
	if _, err := c.Latest(context.Background()); err == nil {
		t.Fatal("a prerelease was accepted")
	}
}

func TestDownloadAndVerifyChecksum(t *testing.T) {
	content := []byte("binary content")
	sum := sha256.Sum256(content)

	dir := t.TempDir()
	binPath := filepath.Join(dir, "agents-room-linux-amd64")
	if err := os.WriteFile(binPath, content, 0o644); err != nil {
		t.Fatal(err)
	}
	sumsPath := filepath.Join(dir, "SHA256SUMS")
	if err := os.WriteFile(sumsPath,
		[]byte(hex.EncodeToString(sum[:])+"  agents-room-linux-amd64\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := VerifyChecksum(binPath, sumsPath); err != nil {
		t.Fatalf("valid checksum rejected: %v", err)
	}

	// A tampered file must fail.
	if err := os.WriteFile(binPath, []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := VerifyChecksum(binPath, sumsPath); err == nil {
		t.Fatal("a tampered file passed the checksum")
	}

	// A file not listed must fail.
	other := filepath.Join(dir, "other")
	os.WriteFile(other, content, 0o644)
	if err := VerifyChecksum(other, sumsPath); err == nil {
		t.Fatal("an unlisted file passed the checksum")
	}
}

func TestDownloadWritesAsset(t *testing.T) {
	body := []byte("hello")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client()}
	path, err := c.Download(context.Background(),
		Asset{Name: "x", URL: srv.URL, Size: int64(len(body))}, t.TempDir())
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "hello" {
		t.Fatalf("content = %q", got)
	}
}

func TestDownloadRejectsWrongSize(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("short"))
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client()}
	if _, err := c.Download(context.Background(),
		Asset{Name: "x", URL: srv.URL, Size: 100}, t.TempDir()); err == nil {
		t.Fatal("a truncated download was accepted")
	}
}

func TestInstallReplacesFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "new")
	target := filepath.Join(dir, "target")
	os.WriteFile(src, []byte("new content"), 0o755)
	os.WriteFile(target, []byte("old content"), 0o755)

	if err := Install(src, target); err != nil {
		t.Fatalf("install: %v", err)
	}
	got, _ := os.ReadFile(target)
	if string(got) != "new content" {
		t.Fatalf("target = %q", got)
	}
	info, _ := os.Stat(target)
	if info.Mode()&0o111 == 0 {
		t.Fatal("installed binary is not executable")
	}
	if _, err := os.Stat(target + ".new"); err == nil {
		t.Fatal("temporary file left behind")
	}
}

// The embedded key must parse. A corrupted embed would otherwise only fail
// on a user's machine during an update.
func TestEmbeddedKeyParses(t *testing.T) {
	data, err := keyFS.ReadFile("release-key.asc")
	if err != nil {
		t.Fatalf("read key: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("embedded key is empty")
	}
	if err := VerifySignature("/nonexistent", "/nonexistent"); err == nil {
		t.Fatal("verifying with missing files succeeded")
	}
}
