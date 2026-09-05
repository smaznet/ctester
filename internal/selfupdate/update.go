package selfupdate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const defaultRepo = "smaznet/ctester"

// Options for Update.
type Options struct {
	Repo  string // owner/name, default smaznet/ctester
	Tag   string // release tag, default "latest"
	Force bool   // replace even when checksum matches
}

type ghRelease struct {
	TagName string    `json:"tag_name"`
	Assets  []ghAsset `json:"assets"`
}

type ghAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
}

// Update downloads the release binary for this GOOS/GOARCH and replaces the
// current executable in place. Safe while the process is still running on Unix
// (old inode stays until exit); restart afterward to run the new build.
func Update(opts Options) error {
	if opts.Repo == "" {
		opts.Repo = defaultRepo
	}
	if opts.Tag == "" {
		opts.Tag = "latest"
	}

	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable: %w", err)
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return fmt.Errorf("resolve executable: %w", err)
	}

	assetName := fmt.Sprintf("x-tester-%s-%s", runtime.GOOS, runtime.GOARCH)
	fmt.Printf("fetching %s @ %s (%s)…\n", opts.Repo, opts.Tag, assetName)

	rel, err := fetchRelease(opts.Repo, opts.Tag)
	if err != nil {
		return err
	}

	var binURL, sumURL string
	for _, a := range rel.Assets {
		switch a.Name {
		case assetName:
			binURL = a.BrowserDownloadURL
		case assetName + ".sha256":
			sumURL = a.BrowserDownloadURL
		}
	}
	if binURL == "" {
		return fmt.Errorf("release %q has no asset %q", rel.TagName, assetName)
	}

	wantSum, err := fetchSHA256(sumURL)
	if err != nil {
		return err
	}

	if !opts.Force && wantSum != "" {
		cur, err := fileSHA256(exe)
		if err == nil && cur == wantSum {
			fmt.Printf("already up to date (%s, sha256=%s)\n", rel.TagName, shortHash(wantSum))
			return nil
		}
	}

	tmp, err := os.CreateTemp(filepath.Dir(exe), ".x-tester-update-*")
	if err != nil {
		return fmt.Errorf("temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = os.Remove(tmpPath)
	}()

	gotSum, err := downloadTo(tmp, binURL)
	_ = tmp.Close()
	if err != nil {
		return err
	}
	if wantSum != "" && gotSum != wantSum {
		return fmt.Errorf("checksum mismatch: got %s want %s", gotSum, wantSum)
	}

	info, err := os.Stat(exe)
	if err != nil {
		return err
	}
	if err := os.Chmod(tmpPath, info.Mode().Perm()); err != nil {
		return err
	}

	backup := exe + ".bak"
	_ = os.Remove(backup)
	if err := os.Rename(exe, backup); err != nil {
		return fmt.Errorf("backup current binary: %w", err)
	}
	if err := os.Rename(tmpPath, exe); err != nil {
		_ = os.Rename(backup, exe) // best-effort restore
		return fmt.Errorf("install new binary: %w", err)
	}
	_ = os.Remove(backup)

	fmt.Printf("updated to %s (sha256=%s)\n", rel.TagName, shortHash(gotSum))
	fmt.Println("restart the process (or: systemctl restart x-tester) to use it")
	return nil
}

func fetchRelease(repo, tag string) (*ghRelease, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/releases/tags/%s", repo, tag)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "x-tester-update")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("release tag %q not found on %s", tag, repo)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<10))
		return nil, fmt.Errorf("github api %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var rel ghRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, fmt.Errorf("decode release: %w", err)
	}
	return &rel, nil
}

func fetchSHA256(url string) (string, error) {
	if url == "" {
		return "", nil
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download checksum: %s", resp.Status)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if err != nil {
		return "", err
	}
	// formats: "<hex>  filename" or just "<hex>"
	fields := strings.Fields(string(raw))
	if len(fields) == 0 {
		return "", fmt.Errorf("empty checksum file")
	}
	sum := strings.ToLower(fields[0])
	if len(sum) != 64 {
		return "", fmt.Errorf("bad checksum %q", fields[0])
	}
	return sum, nil
}

func downloadTo(w io.Writer, url string) (string, error) {
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download binary: %s", resp.Status)
	}
	h := sha256.New()
	if _, err := io.Copy(w, io.TeeReader(resp.Body, h)); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func shortHash(sum string) string {
	if len(sum) > 12 {
		return sum[:12]
	}
	return sum
}
