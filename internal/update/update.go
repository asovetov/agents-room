// Package update checks GitHub for a newer release and installs it.
//
// Two rules shape this package:
//
//  1. Nothing is installed without the operator saying yes. The default is a
//     check that prints what it found and exits.
//  2. The checksum is verified against the embedded release key before
//     anything replaces the running binary. A checksum alone proves the
//     download is intact, not that it is the file the maintainer published.
package update

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
)

//go:embed release-key.asc
var keyFS embed.FS

// Repo is the GitHub repository releases come from.
const Repo = "asovetov/agents-room"

// Release is one GitHub release.
type Release struct {
	Tag        string  `json:"tag_name"`
	Name       string  `json:"name"`
	Body       string  `json:"body"`
	Prerelease bool    `json:"prerelease"`
	Draft      bool    `json:"draft"`
	Assets     []Asset `json:"assets"`
}

// Asset is one file attached to a release.
type Asset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	Size int64  `json:"size"`
}

// AssetFor returns the asset name for this platform.
func AssetFor(goos, goarch string) string {
	name := fmt.Sprintf("agents-room-%s-%s", goos, goarch)
	if goos == "windows" {
		name += ".exe"
	}
	return name
}

// Client talks to the GitHub API.
type Client struct {
	HTTP    *http.Client
	APIBase string
	Token   string // optional; raises the rate limit
}

// NewClient returns a client with a sane timeout.
func NewClient() *Client {
	return &Client{
		HTTP:    &http.Client{Timeout: 30 * time.Second},
		APIBase: "https://api.github.com",
		Token:   os.Getenv("GITHUB_TOKEN"),
	}
}

// Latest returns the newest published, non-prerelease release.
func (c *Client) Latest(ctx context.Context) (*Release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.APIBase+"/repos/"+Repo+"/releases/latest", nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("reach GitHub: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("GitHub returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	var rel Release
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, fmt.Errorf("decode release: %w", err)
	}
	if rel.Draft || rel.Prerelease {
		return nil, errors.New("the latest release is a draft or prerelease")
	}
	return &rel, nil
}

// IsNewer reports whether candidate is a higher version than current.
//
// Both are expected as vMAJOR.MINOR.PATCH. A current version of "dev" or an
// unparseable value is always older, so a locally built binary can be told
// there is a release.
func IsNewer(current, candidate string) bool {
	cur, okCur := parseVersion(current)
	cand, okCand := parseVersion(candidate)
	if !okCand {
		return false
	}
	if !okCur {
		return true
	}
	for i := 0; i < 3; i++ {
		if cand[i] != cur[i] {
			return cand[i] > cur[i]
		}
	}
	return false
}

func parseVersion(v string) ([3]int, bool) {
	var out [3]int
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n := 0
		if p == "" {
			return out, false
		}
		for _, r := range p {
			if r < '0' || r > '9' {
				return out, false
			}
			n = n*10 + int(r-'0')
		}
		out[i] = n
	}
	return out, true
}

// Download fetches an asset into dir and returns its path.
func (c *Client) Download(ctx context.Context, asset Asset, dir string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, asset.URL, nil)
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("download %s: %w", asset.Name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download %s: %s", asset.Name, resp.Status)
	}

	path := filepath.Join(dir, asset.Name)
	f, err := os.Create(path)
	if err != nil {
		return "", fmt.Errorf("create %s: %w", path, err)
	}
	defer f.Close()

	written, err := io.Copy(f, io.LimitReader(resp.Body, 1<<30))
	if err != nil {
		return "", fmt.Errorf("write %s: %w", asset.Name, err)
	}
	if asset.Size > 0 && written != asset.Size {
		return "", fmt.Errorf("download %s: got %d bytes, release lists %d", asset.Name, written, asset.Size)
	}
	return path, nil
}

// FindAsset returns the asset with the given name.
func (r *Release) FindAsset(name string) (Asset, bool) {
	for _, a := range r.Assets {
		if a.Name == name {
			return a, true
		}
	}
	return Asset{}, false
}

// VerifyChecksum checks that file matches the line for its name in a
// SHA256SUMS file.
func VerifyChecksum(file, sumsPath string) error {
	data, err := os.ReadFile(sumsPath)
	if err != nil {
		return fmt.Errorf("read checksums: %w", err)
	}
	name := filepath.Base(file)
	want := ""
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		if strings.TrimPrefix(fields[1], "*") == name {
			want = fields[0]
			break
		}
	}
	if want == "" {
		return fmt.Errorf("%s is not listed in SHA256SUMS", name)
	}

	f, err := os.Open(file)
	if err != nil {
		return fmt.Errorf("open %s: %w", file, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("hash %s: %w", file, err)
	}
	got := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(got, want) {
		return fmt.Errorf("checksum mismatch for %s: got %s, want %s", name, got, want)
	}
	return nil
}

// VerifySignature checks an armored detached signature over the checksum
// file against the embedded release key.
func VerifySignature(sumsPath, sigPath string) error {
	keyData, err := keyFS.ReadFile("release-key.asc")
	if err != nil {
		return fmt.Errorf("read embedded key: %w", err)
	}
	keyring, err := openpgp.ReadArmoredKeyRing(strings.NewReader(string(keyData)))
	if err != nil {
		return fmt.Errorf("parse embedded key: %w", err)
	}

	sig, err := os.Open(sigPath)
	if err != nil {
		return fmt.Errorf("open signature: %w", err)
	}
	defer sig.Close()
	sums, err := os.Open(sumsPath)
	if err != nil {
		return fmt.Errorf("open checksums: %w", err)
	}
	defer sums.Close()

	signer, err := openpgp.CheckArmoredDetachedSignature(keyring, sums, sig, nil)
	if err != nil {
		return fmt.Errorf("signature does not verify against the release key: %w", err)
	}
	if signer == nil || len(signer.Identities) == 0 {
		return errors.New("signature verified but names no signer")
	}
	return nil
}

// Install replaces the binary at target with src.
//
// The new file is written next to the target and renamed over it, so a
// failure never leaves a half-written binary in place. On Windows a running
// executable cannot be replaced, which surfaces as a clear error.
func Install(src, target string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open new binary: %w", err)
	}
	defer in.Close()

	tmp := target + ".new"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return fmt.Errorf("copy new binary: %w", err)
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("close %s: %w", tmp, err)
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("make executable: %w", err)
	}
	if err := os.Rename(tmp, target); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("replace %s: %w (on Windows, run from a different shell than the one being updated)", target, err)
	}
	return nil
}

// Platform returns the current GOOS/GOARCH.
func Platform() (string, string) { return runtime.GOOS, runtime.GOARCH }
