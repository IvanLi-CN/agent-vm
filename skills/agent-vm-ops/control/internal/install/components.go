package install

import (
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

type componentArtifact struct {
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	Filename string `json:"filename"`
	URL      string `json:"url"`
	SHA256   string `json:"sha256"`
	SHA512   string `json:"sha512"`
}
type component struct {
	Version   string              `json:"version"`
	Artifacts []componentArtifact `json:"artifacts"`
}

func components(root string) (map[string]componentArtifact, error) {
	f, err := materialFile(root, "assets/blue-green/deployment-components.json")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var all struct {
		Caddy  component `json:"caddy"`
		RQLite component `json:"rqlite"`
	}
	if err := json.NewDecoder(io.LimitReader(f, 1<<20)).Decode(&all); err != nil {
		return nil, err
	}
	result := map[string]componentArtifact{}
	for name, c := range map[string]component{"caddy": all.Caddy, "rqlite": all.RQLite} {
		for _, a := range c.Artifacts {
			if a.OS == "linux" && a.Arch == runtime.GOARCH {
				repository := "caddyserver/caddy"
				if name == "rqlite" {
					repository = "rqlite/rqlite"
				}
				if !strings.HasPrefix(a.URL, "https://github.com/"+repository+"/releases/download/") || filepath.Base(a.Filename) != a.Filename || a.Filename == "" {
					return nil, errors.New("invalid pinned component source")
				}
				checksum := a.SHA256
				if name == "caddy" {
					checksum = a.SHA512
				}
				decoded, e := hex.DecodeString(checksum)
				size := 32
				if name == "caddy" {
					size = 64
				}
				if e != nil || len(decoded) != size {
					return nil, errors.New("invalid pinned component digest")
				}
				result[name] = a
			}
		}
	}
	if len(result) != 2 {
		return nil, errors.New("pinned components do not cover this architecture")
	}
	return result, nil
}
func fetchComponent(ctx context.Context, name string, a componentArtifact, root string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", a.URL, nil)
	if err != nil {
		return "", err
	}
	client := &http.Client{Timeout: 10 * time.Minute, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if req.URL.Scheme != "https" || len(via) > 5 {
			return errors.New("unsafe dependency redirect")
		}
		return nil
	}}
	response, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("download %s failed", name)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return "", fmt.Errorf("download %s returned HTTP %d", name, response.StatusCode)
	}
	archive := filepath.Join(root, name+".tar.gz")
	f, err := os.OpenFile(archive, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return "", err
	}
	var checksum hash.Hash = sha256.New()
	expected := a.SHA256
	if name == "caddy" {
		checksum = sha512.New()
		expected = a.SHA512
	}
	n, err := io.Copy(io.MultiWriter(f, checksum), io.LimitReader(response.Body, 256<<20+1))
	closeErr := f.Close()
	if err != nil {
		return "", fmt.Errorf("download %s body: %w", name, err)
	}
	if closeErr != nil {
		return "", closeErr
	}
	if n > 256<<20 || hex.EncodeToString(checksum.Sum(nil)) != expected {
		return "", fmt.Errorf("%s pinned checksum mismatch", name)
	}
	source, err := os.Open(archive)
	if err != nil {
		return "", err
	}
	defer source.Close()
	directory := filepath.Join(root, name)
	if err := ExtractGzip(source, directory, 512<<20); err != nil {
		return "", err
	}
	binary := "caddy"
	if name == "rqlite" {
		binary = filepath.Join(strings.TrimSuffix(a.Filename, ".tar.gz"), "rqlited")
	}
	result := filepath.Join(directory, binary)
	info, err := os.Lstat(result)
	if err != nil || !info.Mode().IsRegular() {
		return "", errors.New("component binary is missing")
	}
	return result, nil
}
