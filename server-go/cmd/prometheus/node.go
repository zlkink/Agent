package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"prometheus-go/internal/config"
)

const nodeVersion = "22.23.4"

type nodeTarget struct {
	urls   []string
	sha256 string
	format string
	strip  int
	check  string
}

func nodeTargetFor() (nodeTarget, bool) {
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "windows/amd64":
		return nodeTarget{
			urls: []string{
				"https://nodejs.org/dist/v" + nodeVersion + "/node-v" + nodeVersion + "-win-x64.zip",
				"https://cdn.npmmirror.com/binaries/node/v" + nodeVersion + "/node-v" + nodeVersion + "-win-x64.zip",
			},
			sha256: "cbc74b5e0982c9dbb41525fd8fb5dcd32fbee237863f7829072f4639a1ce5f20",
			format: "zip", strip: 1, check: "node.exe",
		}, true
	case "windows/arm64":
		return nodeTarget{
			urls: []string{
				"https://nodejs.org/dist/v" + nodeVersion + "/node-v" + nodeVersion + "-win-arm64.zip",
				"https://cdn.npmmirror.com/binaries/node/v" + nodeVersion + "/node-v" + nodeVersion + "-win-arm64.zip",
			},
			sha256: "a8b8cd79cbbb3062435f449591d98dd63730bbbf30ce2d7cc3a6376c236cb111",
			format: "zip", strip: 1, check: "node.exe",
		}, true
	case "linux/amd64":
		return nodeTarget{
			urls: []string{
				"https://nodejs.org/dist/v" + nodeVersion + "/node-v" + nodeVersion + "-linux-x64.tar.gz",
				"https://cdn.npmmirror.com/binaries/node/v" + nodeVersion + "/node-v" + nodeVersion + "-linux-x64.tar.gz",
			},
			sha256: "b8492284c76a54baf58d61668dd0e41ee0fc98adabf9503d30f05d08f55b4406",
			format: "tar.gz", strip: 1, check: "bin/node",
		}, true
	case "linux/arm64":
		return nodeTarget{
			urls: []string{
				"https://nodejs.org/dist/v" + nodeVersion + "/node-v" + nodeVersion + "-linux-arm64.tar.gz",
				"https://cdn.npmmirror.com/binaries/node/v" + nodeVersion + "/node-v" + nodeVersion + "-linux-arm64.tar.gz",
			},
			sha256: "f7a1e486dbc8b286847815b477d4fb5e9fb963ed4489c3e1588853d685f589f9",
			format: "tar.gz", strip: 1, check: "bin/node",
		}, true
	case "darwin/amd64":
		return nodeTarget{
			urls: []string{
				"https://nodejs.org/dist/v" + nodeVersion + "/node-v" + nodeVersion + "-darwin-x64.tar.gz",
				"https://cdn.npmmirror.com/binaries/node/v" + nodeVersion + "/node-v" + nodeVersion + "-darwin-x64.tar.gz",
			},
			sha256: "efcc2b0db6f94bf53eddb038fe2b370277a042bd29e4f19b8f8e23598795b819",
			format: "tar.gz", strip: 1, check: "bin/node",
		}, true
	case "darwin/arm64":
		return nodeTarget{
			urls: []string{
				"https://nodejs.org/dist/v" + nodeVersion + "/node-v" + nodeVersion + "-darwin-arm64.tar.gz",
				"https://cdn.npmmirror.com/binaries/node/v" + nodeVersion + "/node-v" + nodeVersion + "-darwin-arm64.tar.gz",
			},
			sha256: "c9e1dbc748b33ed2d14f42445191ecaf14c56259c7a9a26d0042c49ec2b66428",
			format: "tar.gz", strip: 1, check: "bin/node",
		}, true
	}
	return nodeTarget{}, false
}

func nodeRoot() string {
	return filepath.Join(config.Home(), "go-poc", "node", nodeVersion)
}

func installedNode() string {
	t, ok := nodeTargetFor()
	if !ok {
		return ""
	}
	p := filepath.Join(nodeRoot(), filepath.FromSlash(t.check))
	if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
		return p
	}
	return ""
}

func eligibleNode(path string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--version").Output()
	if err != nil {
		return false
	}
	v := strings.TrimSpace(string(out))
	v = strings.TrimPrefix(v, "v")
	parts := strings.Split(v, ".")
	if len(parts) < 2 {
		return false
	}
	major, _ := strconv.Atoi(parts[0])
	minor, _ := strconv.Atoi(parts[1])
	return major > 22 || (major == 22 && minor >= 19)
}

// ensureNode returns a usable Node >= 22.19, downloading an official build
// into ~/.prometheus/go-poc/node when the machine has none.
func ensureNode(ctx context.Context) (string, error) {
	if p, err := exec.LookPath("node"); err == nil && eligibleNode(p) {
		return p, nil
	}
	if p := installedNode(); p != "" && eligibleNode(p) {
		return p, nil
	}
	t, ok := nodeTargetFor()
	if !ok {
		return "", fmt.Errorf("no Node build is known for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	if err := os.MkdirAll(nodeRoot(), 0o755); err != nil {
		return "", err
	}
	tmp := filepath.Join(os.TempDir(), "prometheus-node-"+nodeVersion+"."+t.format)
	var last error
	for _, url := range t.urls {
		if err := download(ctx, url, tmp); err == nil {
			last = nil
			break
		} else {
			last = err
		}
	}
	if last != nil {
		return "", last
	}
	defer os.Remove(tmp)
	sum, err := fileSHA256(tmp)
	if err != nil {
		return "", err
	}
	if sum != t.sha256 {
		return "", fmt.Errorf("Node download checksum mismatch: %s", sum)
	}
	switch t.format {
	case "zip":
		if err := extractZip(tmp, nodeRoot(), t.strip); err != nil {
			return "", err
		}
	case "tar.gz":
		if err := extractTarGz(tmp, nodeRoot(), t.strip); err != nil {
			return "", err
		}
	}
	p := filepath.Join(nodeRoot(), filepath.FromSlash(t.check))
	if runtime.GOOS != "windows" {
		_ = os.Chmod(p, 0o755)
	}
	if _, err := os.Stat(p); err != nil {
		return "", fmt.Errorf("Node unpacked but %s is missing", p)
	}
	return p, nil
}

func download(ctx context.Context, url, dest string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 10 * time.Minute}
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: HTTP %d", url, res.StatusCode)
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, res.Body)
	cerr := f.Close()
	if err != nil {
		return err
	}
	return cerr
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

func extractZip(file, dest string, strip int) error {
	zr, err := zip.OpenReader(file)
	if err != nil {
		return err
	}
	defer zr.Close()
	for _, f := range zr.File {
		rel, ok := strippedPath(f.Name, strip)
		if !ok {
			continue
		}
		target := filepath.Join(dest, filepath.FromSlash(rel))
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, f.Mode())
		if err != nil {
			rc.Close()
			return err
		}
		_, err = io.Copy(out, rc)
		rc.Close()
		out.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func extractTarGz(file, dest string, strip int) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		rel, ok := strippedPath(hdr.Name, strip)
		if !ok {
			continue
		}
		target := filepath.Join(dest, filepath.FromSlash(rel))
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if hdr.Typeflag == tar.TypeSymlink {
				_ = os.Remove(target)
				_ = os.Symlink(hdr.Linkname, target)
				continue
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(hdr.Mode))
			if err != nil {
				return err
			}
			_, err = io.Copy(out, tr)
			out.Close()
			if err != nil {
				return err
			}
		}
	}
}

func strippedPath(name string, strip int) (string, bool) {
	name = strings.ReplaceAll(name, "\\", "/")
	parts := strings.Split(strings.TrimPrefix(name, "./"), "/")
	if len(parts) <= strip {
		return "", false
	}
	return strings.Join(parts[strip:], "/"), true
}
