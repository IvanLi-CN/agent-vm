package releasefetch

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type releaseFixtureTransport func(*http.Request) (*http.Response, error)

func (f releaseFixtureTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestPrepareLocalAndDownloadUseSameReleaseIdentity(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	protected, err := os.MkdirTemp(home, ".agent-vm-release-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(protected) })
	protected, err = filepath.EvalSymlinks(protected)
	if err != nil {
		t.Fatal(err)
	}
	version, revision := "1.6.0", strings.Repeat("a", 40)
	archive := func(arch string) []byte {
		var buffer bytes.Buffer
		writer := tar.NewWriter(&buffer)
		for name, value := range map[string]string{
			"release-manifest.json": fmt.Sprintf(`{"source_revision":%q,"version":%q,"target":{"os":"linux","arch":%q}}`, revision, version, arch),
			"bin/agent-vm-server":   "published server",
		} {
			if err := writer.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(value))}); err != nil {
				t.Fatal(err)
			}
			if _, err := writer.Write([]byte(value)); err != nil {
				t.Fatal(err)
			}
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		return buffer.Bytes()
	}
	bundle := filepath.Join(protected, "release.tar")
	server := filepath.Join(protected, "server")
	bundleBytes := archive("amd64")
	write := func(path string, data []byte) {
		t.Helper()
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(bundle, bundleBytes)
	write(server, []byte("published server"))
	name := "agent-vm-native-1.6.0-linux-amd64.tar"
	candidateDownloads := 0
	mirror := &http.Client{Transport: releaseFixtureTransport(func(request *http.Request) (*http.Response, error) {
		var body []byte
		sum := fmt.Sprintf("%x", sha256.Sum256(bundleBytes))
		switch filepath.Base(request.URL.Path) {
		case "v1.6.0":
			body = []byte(fmt.Sprintf(`{"tag_name":"v1.6.0","immutable":true,"assets":[{"name":%q,"id":1},{"name":"release-provenance.json","id":2},{"name":"SHA256SUMS","id":3}]}`, name))
		case "1":
			candidateDownloads++
			body = bundleBytes
		case "2":
			body = []byte(fmt.Sprintf(`{"version":%q,"source_revision":%q,"packages":{%q:%q}}`, version, revision, name, sum))
		case "3":
			body = []byte(fmt.Sprintf("%s  %s\n", sum, name))
		default:
			t.Fatalf("unexpected release request %s", request.URL.Path)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(body))}, nil
	})}
	run := func(file, binary string) error {
		staged, err := prepare(t.Context(), mirror, "linux", "amd64", version, revision, t.TempDir(), file, binary)
		if err == nil && digest(staged) != fmt.Sprintf("%x", sha256.Sum256(bundleBytes)) {
			t.Fatal("unexpected staged byte identity")
		}
		return err
	}
	if err := run("", ""); err != nil {
		t.Fatal(err)
	}
	if err := run(bundle, server); err != nil || candidateDownloads != 1 {
		t.Fatalf("local input = %v, bundle downloads = %d", err, candidateDownloads)
	}
	write(server, []byte("different server"))
	if err := run(bundle, server); err == nil {
		t.Fatal("different server was accepted")
	}
	link := filepath.Join(protected, "symlink.tar")
	if err := os.Symlink(bundle, link); err != nil {
		t.Fatal(err)
	}
	if err := run(link, ""); err == nil {
		t.Fatal("symlink input was accepted")
	}
	write(bundle, []byte("modified"))
	if err := run(bundle, ""); err == nil {
		t.Fatal("modified bundle was accepted")
	}
	bundleBytes = archive("arm64")
	write(bundle, bundleBytes)
	if err := run(bundle, ""); err == nil {
		t.Fatal("wrong architecture was accepted even with matching release checksums")
	}
}
