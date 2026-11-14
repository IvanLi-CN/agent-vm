package releasefetch

import (
	"archive/tar"
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
	"runtime"
	"strconv"
	"strings"
	"time"
)

const Repository = "IvanLi-CN/agent-vm"
const credentialFile = "/etc/agent-vm-native/release-fetch.env"

type Error struct {
	Code              string `json:"code"`
	Phase             string `json:"phase"`
	HTTPStatus        int    `json:"http_status,omitempty"`
	Retryable         bool   `json:"retryable"`
	Reason            string `json:"reason"`
	NextAction        string `json:"next_action"`
	RequestID         string `json:"request_id,omitempty"`
	RetryAfterSeconds *int   `json:"retry_after_seconds"`
}

func (e *Error) Error() string { b, _ := json.Marshal(e); return string(b) }

type release struct {
	TagName    string `json:"tag_name"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
	Immutable  bool   `json:"immutable"`
	Assets     []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

type provenance struct {
	Version        string            `json:"version"`
	SourceRevision string            `json:"source_revision"`
	Packages       map[string]string `json:"packages"`
}

func Fetch(ctx context.Context, version, sourceRevision, directory string) (string, error) {
	if version == "" || strings.HasPrefix(version, "v") || strings.ContainsAny(version, " /\\") {
		return "", errors.New("version must be a release SemVer without v")
	}
	if len(sourceRevision) != 40 {
		return "", errors.New("source revision must be a 40-character SHA")
	}
	if _, err := hex.DecodeString(sourceRevision); err != nil {
		return "", errors.New("source revision must be hexadecimal")
	}
	arch := runtime.GOARCH
	if runtime.GOOS != "linux" || (arch != "amd64" && arch != "arm64") {
		return "", fmt.Errorf("unsupported deployment target %s/%s", runtime.GOOS, arch)
	}
	assetName := fmt.Sprintf("agent-vm-%s-linux-%s.tar", version, arch)
	client := &http.Client{}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/repos/"+Repository+"/releases/tags/v"+version, nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "agent-vm-deploy")
	if token := readToken(); token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := client.Do(request)
	if err != nil {
		return "", &Error{Code: "release_network_error", Phase: "release_metadata_lookup", Retryable: true, Reason: "transport_error", NextAction: "retry_update"}
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", classify(response)
	}
	var value release
	if err := json.NewDecoder(io.LimitReader(response.Body, 2<<20)).Decode(&value); err != nil {
		return "", fmt.Errorf("release metadata invalid: %w", err)
	}
	if value.TagName != "v"+version || value.Draft || value.Prerelease || !value.Immutable {
		return "", errors.New("release is not a published immutable candidate")
	}
	urls := map[string]string{}
	for _, asset := range value.Assets {
		urls[asset.Name] = asset.URL
	}
	for _, name := range []string{assetName, "release-provenance.json", "SHA256SUMS"} {
		if urls[name] == "" {
			return "", fmt.Errorf("release asset missing: %s", name)
		}
	}
	if err := os.MkdirAll(directory, 0o750); err != nil {
		return "", err
	}
	files := map[string]string{}
	for _, name := range []string{assetName, "release-provenance.json", "SHA256SUMS"} {
		path := filepath.Join(directory, name)
		if err := download(ctx, client, urls[name], path); err != nil {
			return "", err
		}
		files[name] = path
	}
	var proof provenance
	proofFile, err := os.Open(files["release-provenance.json"])
	if err != nil {
		return "", err
	}
	err = json.NewDecoder(proofFile).Decode(&proof)
	proofFile.Close()
	if err != nil {
		return "", fmt.Errorf("release provenance invalid: %w", err)
	}
	sums, err := os.ReadFile(files["SHA256SUMS"])
	if err != nil {
		return "", fmt.Errorf("release checksums unreadable: %w", err)
	}
	checksum := strings.Fields(string(sums))
	checksumFound := false
	for index := 0; index+1 < len(checksum); index += 2 {
		if checksum[index+1] == assetName && checksum[index] == digest(files[assetName]) {
			checksumFound = true
		}
	}
	if !checksumFound {
		return "", errors.New("release checksum does not match requested asset")
	}
	if proof.Version != version || proof.SourceRevision != sourceRevision || proof.Packages[assetName] != digest(files[assetName]) {
		return "", errors.New("release provenance does not match requested candidate")
	}
	if err := verifyTarget(files[assetName], runtime.GOOS, arch, sourceRevision); err != nil {
		return "", err
	}
	return files[assetName], nil
}

func readToken() string {
	b, err := os.ReadFile(credentialFile)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "GITHUB_TOKEN=") {
			return strings.TrimSpace(strings.TrimPrefix(line, "GITHUB_TOKEN="))
		}
	}
	return ""
}

func classify(response *http.Response) error {
	code, reason, next, retryable := "release_access_denied", "access_denied", "configure_release_read_credential", false
	if response.StatusCode == http.StatusNotFound {
		code, reason, next = "release_not_found", "release_not_found", "check_version"
	}
	if response.StatusCode == http.StatusTooManyRequests || response.StatusCode == http.StatusForbidden && response.Header.Get("X-RateLimit-Remaining") == "0" {
		code, reason, next, retryable = "release_rate_limited", "api_rate_limited", "wait_or_configure_release_read_credential", true
	}
	if response.StatusCode >= 500 {
		code, reason, next, retryable = "release_network_error", "upstream_server_error", "retry_update", true
	}
	var retryAfter *int
	value := response.Header.Get("Retry-After")
	if value == "" && response.Header.Get("X-RateLimit-Reset") != "" {
		if epoch, err := strconv.ParseInt(response.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			seconds := int(time.Until(time.Unix(epoch, 0)).Seconds())
			if seconds < 0 {
				seconds = 0
			}
			retryAfter = &seconds
		}
	} else if seconds, err := strconv.Atoi(value); err == nil {
		retryAfter = &seconds
	}
	return &Error{Code: code, Phase: "release_metadata_lookup", HTTPStatus: response.StatusCode, Retryable: retryable, Reason: reason, NextAction: next, RequestID: response.Header.Get("X-GitHub-Request-Id"), RetryAfterSeconds: retryAfter}
}

func download(ctx context.Context, client *http.Client, url, path string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "agent-vm-deploy")
	res, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("asset download failed: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("asset download returned HTTP %d", res.StatusCode)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(file, io.LimitReader(res.Body, 256<<20))
	closeErr := file.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}
func digest(path string) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()
	sum := sha256.New()
	if _, err := io.Copy(sum, file); err != nil {
		return ""
	}
	return hex.EncodeToString(sum.Sum(nil))
}
func verifyTarget(path, wantOS, wantArch, source string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	reader := tar.NewReader(file)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if header.Name != "release-manifest.json" {
			continue
		}
		var manifest struct {
			SourceRevision string `json:"source_revision"`
			Target         struct {
				OS   string `json:"os"`
				Arch string `json:"arch"`
			} `json:"target"`
		}
		if err := json.NewDecoder(io.LimitReader(reader, 1<<20)).Decode(&manifest); err != nil {
			return err
		}
		if manifest.SourceRevision != source || manifest.Target.OS != wantOS || manifest.Target.Arch != wantArch {
			return errors.New("release manifest target or source mismatch")
		}
		return nil
	}
	return errors.New("release manifest missing")
}
