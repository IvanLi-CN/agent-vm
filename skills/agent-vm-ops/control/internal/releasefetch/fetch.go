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
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
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
		ID   int64  `json:"id"`
		URL  string `json:"browser_download_url"`
		Size int64  `json:"size"`
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
	response, err := metadata(ctx, client, version)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	var value release
	if err := json.NewDecoder(io.LimitReader(response.Body, 2<<20)).Decode(&value); err != nil {
		return "", validationError("release_metadata_invalid", "release_metadata_lookup", "metadata_json_invalid", "check_release_endpoint")
	}
	if value.TagName != "v"+version || value.Draft || value.Prerelease || !value.Immutable {
		return "", validationError("release_not_immutable", "release_metadata_validation", "release_is_not_published_immutable", "check_version")
	}
	urls := map[string]string{}
	sizes := map[string]int64{}
	for _, asset := range value.Assets {
		if asset.ID > 0 {
			urls[asset.Name] = fmt.Sprintf("https://api.github.com/repos/%s/releases/assets/%d", Repository, asset.ID)
		}
		sizes[asset.Name] = asset.Size
	}
	for _, name := range []string{assetName, "release-provenance.json", "SHA256SUMS"} {
		if urls[name] == "" {
			return "", validationError("release_asset_missing", "release_metadata_validation", "required_asset_missing", "check_release_assets")
		}
		if err := validateAssetURL(urls[name]); err != nil {
			return "", err
		}
	}
	if err := os.MkdirAll(directory, 0o750); err != nil {
		return "", validationError("release_staging_unavailable", "staging_prepare", "staging_directory_unavailable", "check_target_storage")
	}
	var required int64 = 3*256<<20 + 64<<20
	for _, name := range []string{assetName, "release-provenance.json", "SHA256SUMS"} {
		if sizes[name] < 0 || sizes[name] > 256<<20 {
			return "", validationError("release_asset_too_large", "release_metadata_validation", "asset_size_invalid", "inspect_release")
		}
		if sizes[name] > 0 && sizes[name]+64<<20 > required {
			required = sizes[name] + 64<<20
		}
	}
	if err := checkCapacity(directory, required); err != nil {
		return "", err
	}
	files := map[string]string{}
	for _, name := range []string{assetName, "release-provenance.json", "SHA256SUMS"} {
		path := filepath.Join(directory, name)
		if err := download(ctx, client, urls[name], path, readToken()); err != nil {
			return "", err
		}
		files[name] = path
	}
	var proof provenance
	proofFile, err := os.Open(files["release-provenance.json"])
	if err != nil {
		return "", validationError("release_provenance_invalid", "release_validation", "provenance_json_invalid", "inspect_release")
	}
	decoder := json.NewDecoder(io.LimitReader(proofFile, 1<<20))
	err = decoder.Decode(&proof)
	if err == nil {
		var trailing any
		if trailingErr := decoder.Decode(&trailing); trailingErr != io.EOF {
			err = errors.New("trailing data")
		}
	}
	proofFile.Close()
	if err != nil {
		return "", validationError("release_provenance_invalid", "release_validation", "provenance_json_invalid", "inspect_release")
	}
	sumFile, err := os.Open(files["SHA256SUMS"])
	if err != nil {
		return "", validationError("release_checksums_invalid", "release_validation", "checksums_unreadable", "inspect_release")
	}
	sums, err := io.ReadAll(io.LimitReader(sumFile, 1<<20+1))
	sumFile.Close()
	if err != nil || len(sums) > 1<<20 {
		return "", validationError("release_checksums_invalid", "release_validation", "checksums_truncated", "inspect_release")
	}
	checksum := strings.Fields(string(sums))
	checksumFound := false
	for index := 0; index+1 < len(checksum); index += 2 {
		if checksum[index+1] == assetName && checksum[index] == digest(files[assetName]) {
			checksumFound = true
		}
	}
	if !checksumFound {
		return "", validationError("release_checksum_mismatch", "release_validation", "requested_asset_checksum_mismatch", "inspect_release")
	}
	if proof.Version != version || proof.SourceRevision != sourceRevision || proof.Packages[assetName] != digest(files[assetName]) {
		return "", validationError("release_provenance_mismatch", "release_validation", "provenance_does_not_match_candidate", "inspect_release")
	}
	if err := verifyTarget(files[assetName], runtime.GOOS, arch, sourceRevision); err != nil {
		return "", validationError("release_manifest_invalid", "release_validation", "manifest_target_or_source_mismatch", "inspect_release")
	}
	return files[assetName], nil
}

func readToken() string {
	info, err := os.Lstat(credentialFile)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return ""
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || stat.Uid != 0 {
		return ""
	}
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

func metadata(ctx context.Context, client *http.Client, version string) (*http.Response, error) {
	for attempt := 0; attempt < 3; attempt++ {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/repos/"+Repository+"/releases/tags/v"+version, nil)
		if err != nil {
			return nil, validationError("release_network_error", "release_metadata_lookup", "request_creation_failed", "retry_update")
		}
		request.Header.Set("Accept", "application/vnd.github+json")
		request.Header.Set("User-Agent", "agent-vm-deploy")
		if token := readToken(); token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		response, err := client.Do(request)
		if err != nil {
			if attempt == 2 {
				return nil, validationError("release_network_error", "release_metadata_lookup", "transport_error", "retry_update")
			}
			if err := retryDelay(ctx, time.Second*time.Duration(attempt+1)); err != nil {
				return nil, err
			}
			continue
		}
		if response.StatusCode == http.StatusOK {
			return response, nil
		}
		classified := classify(response)
		if !isRetryable(classified) || attempt == 2 {
			response.Body.Close()
			return nil, classified
		}
		response.Body.Close()
		if err := retryDelay(ctx, retryAfter(classified, time.Second*time.Duration(attempt+1))); err != nil {
			return nil, err
		}
	}
	return nil, validationError("release_network_error", "release_metadata_lookup", "retry_exhausted", "retry_update")
}

func validationError(code, phase, reason, next string) error {
	return &Error{Code: code, Phase: phase, Retryable: false, Reason: reason, NextAction: next}
}

func isRetryable(err error) bool {
	value, ok := err.(*Error)
	return ok && value.Retryable
}

func retryAfter(err error, fallback time.Duration) time.Duration {
	value, ok := err.(*Error)
	if !ok || value.RetryAfterSeconds == nil {
		return fallback
	}
	seconds := *value.RetryAfterSeconds
	if seconds > 60 {
		seconds = 60
	}
	return time.Duration(seconds) * time.Second
}

func retryDelay(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func checkCapacity(directory string, required int64) error {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(directory, &stat); err != nil {
		return validationError("staging_capacity_unknown", "staging_prepare", "staging_capacity_unavailable", "check_target_storage")
	}
	available := uint64(stat.Bavail) * uint64(stat.Bsize)
	if required > 0 && available < uint64(required) {
		return validationError("staging_capacity_low", "staging_prepare", "insufficient_staging_capacity", "free_target_storage")
	}
	return nil
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
	} else if parsed, err := http.ParseTime(value); err == nil {
		seconds := int(time.Until(parsed).Seconds())
		if seconds < 0 {
			seconds = 0
		}
		retryAfter = &seconds
	}
	return &Error{Code: code, Phase: "release_metadata_lookup", HTTPStatus: response.StatusCode, Retryable: retryable, Reason: reason, NextAction: next, RequestID: response.Header.Get("X-GitHub-Request-Id"), RetryAfterSeconds: retryAfter}
}

func validateAssetURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Host != "api.github.com" || parsed.User != nil {
		return validationError("release_asset_url_invalid", "release_metadata_validation", "asset_url_outside_github", "inspect_release")
	}
	return nil
}

func download(ctx context.Context, client *http.Client, url, path, token string) error {
	for attempt := 0; attempt < 3; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return validationError("release_asset_request_invalid", "asset_download", "asset_request_creation_failed", "inspect_release")
		}
		req.Header.Set("User-Agent", "agent-vm-deploy")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		res, err := client.Do(req)
		if err != nil {
			if attempt == 2 {
				return validationError("release_network_error", "asset_download", "asset_transport_error", "retry_update")
			}
			if err := retryDelay(ctx, time.Second*time.Duration(attempt+1)); err != nil {
				return err
			}
			continue
		}
		if res.StatusCode != http.StatusOK {
			classified := classify(res)
			classifiedValue, _ := classified.(*Error)
			res.Body.Close()
			if classifiedValue != nil {
				classifiedValue.Phase = "asset_download"
			}
			if !isRetryable(classified) || attempt == 2 {
				return classified
			}
			if err := retryDelay(ctx, retryAfter(classified, time.Second*time.Duration(attempt+1))); err != nil {
				return err
			}
			continue
		}
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
		if err != nil {
			res.Body.Close()
			return validationError("release_staging_unavailable", "asset_download", "staging_file_unavailable", "check_target_storage")
		}
		limited := io.LimitReader(res.Body, 256<<20+1)
		written, copyErr := io.Copy(file, limited)
		closeErr := file.Close()
		res.Body.Close()
		if copyErr != nil || closeErr != nil {
			_ = os.Remove(path)
			if attempt < 2 {
				if err := retryDelay(ctx, time.Second*time.Duration(attempt+1)); err != nil {
					return validationError("release_network_error", "asset_download", "asset_retry_cancelled", "retry_update")
				}
				continue
			}
			return &Error{Code: "release_network_error", Phase: "asset_download", Retryable: true, Reason: "asset_read_failed", NextAction: "retry_update"}
		}
		if written > 256<<20 {
			return validationError("release_asset_too_large", "asset_download", "asset_exceeds_size_limit", "inspect_release")
		}
		return nil
	}
	return validationError("release_network_error", "asset_download", "retry_exhausted", "retry_update")
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
