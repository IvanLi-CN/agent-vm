// Package manifest validates the immutable control artifact contract before
// the resident service opens the state database or accepts requests.
package manifest

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strings"
)

const (
	Format      = 2
	Protocol    = 4
	GoToolchain = "go1.27.3"
	maxBytes    = 2 * 1024 * 1024
)

var actionNames = []string{
	"acquire", "admin-gc", "admin-metrics", "admin-reconcile", "admin-session-list", "admin-status",
	"destroy", "doctor", "drain-enter", "drain-exit", "drain-status", "port-add", "port-list",
	"port-remove", "release", "renew", "resize", "restart", "ssh-info", "status", "timer-age-gc",
	"timer-expire", "timer-reconcile",
}

var retiredUnits = []string{
	"agent-vm-native-age-gc.service",
	"agent-vm-native-age-gc.timer",
	"agent-vm-native-expire@.service",
	"agent-vm-native-reconcile.service",
}

type artifactSpec struct {
	name string
	path string
	role string
	mode int64
}

var fixedArtifacts = []artifactSpec{
	{name: "agent_vm.ts", path: "agent_vm.ts", role: "client", mode: 0o755},
	{name: "byte-fs.ts", path: "byte-fs.ts", role: "support", mode: 0o644},
	{name: "client-io.ts", path: "client-io.ts", role: "support", mode: 0o644},
	{name: "client-release.ts", path: "client-release.ts", role: "support", mode: 0o644},
	{name: "client.ts", path: "client.ts", role: "support", mode: 0o644},
	{name: "guest-source.ts", path: "guest-source.ts", role: "support", mode: 0o644},
	{name: "sync-native.ts", path: "sync-native.ts", role: "support", mode: 0o644},
	{name: "sync_worktree.ts", path: "sync_worktree.ts", role: "support", mode: 0o755},
	{name: "agent_vm_control_bridge.ts", path: "agent_vm_control_bridge.ts", role: "control", mode: 0o755},
	{name: "agent-vm-control", path: "bin/agent-vm-control", role: "control", mode: 0o755},
	{name: "unit:agent-vm-native.service", path: "systemd/agent-vm-native.service", role: "unit", mode: 0o644},
}

var artifactNamePattern = regexp.MustCompile(`^[A-Za-z0-9_.:@-]{1,120}$`)
var revisionPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
var digestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Load validates format-2 identity, compatibility metadata, fixed artifact
// declarations, artifact bytes, modes, and the target ELF header. An empty
// path is reserved for development runs without an installed release bundle.
func Load(path string) error {
	if path == "" {
		return nil
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("manifest_missing: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("manifest_invalid: manifest is not a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("manifest_missing: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return fmt.Errorf("manifest_invalid: %w", err)
	}
	if len(data) > maxBytes {
		return fmt.Errorf("manifest_invalid: manifest exceeds the size limit")
	}
	var root map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.UseNumber()
	if err := decoder.Decode(&root); err != nil {
		return fmt.Errorf("manifest_invalid: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return fmt.Errorf("manifest_invalid: trailing JSON")
	}
	if root == nil {
		return fmt.Errorf("manifest_invalid: manifest root must be an object")
	}
	return validate(root, filepath.Dir(path))
}

func validate(root map[string]any, directory string) error {
	if !sameKeys(root, []string{"actions", "artifacts", "client_view", "format", "protocol", "release_id", "retired_units", "source_revision", "target", "toolchain"}) {
		return fmt.Errorf("manifest_invalid: manifest shape is incomplete or contains extra fields")
	}
	if value, ok := integerValue(root["format"]); !ok || value != Format {
		return fmt.Errorf("manifest_format_unsupported: manifest format must be %d", Format)
	}
	if value, ok := integerValue(root["protocol"]); !ok || value != Protocol {
		return fmt.Errorf("manifest_protocol_mismatch: manifest protocol must be %d", Protocol)
	}
	source, ok := root["source_revision"].(string)
	if !ok || !revisionPattern.MatchString(source) || root["release_id"] != source {
		return fmt.Errorf("manifest_invalid: release identity is invalid")
	}
	if err := validateTarget(root["target"]); err != nil {
		return err
	}
	if err := validateToolchain(root["toolchain"]); err != nil {
		return err
	}
	if err := validateRetiredUnits(root["retired_units"]); err != nil {
		return err
	}
	if err := validateOperationMap(root["actions"], "actions"); err != nil {
		return err
	}
	view, ok := root["client_view"].(map[string]any)
	if !ok || !sameKeys(view, []string{"actions", "protocol", "release_id", "source_revision", "transport"}) {
		return fmt.Errorf("manifest_invalid: client_view shape is invalid")
	}
	if view["release_id"] != source || view["source_revision"] != source {
		return fmt.Errorf("manifest_invalid: client_view identity differs from the manifest")
	}
	if value, ok := integerValue(view["protocol"]); !ok || value != Protocol {
		return fmt.Errorf("manifest_invalid: client_view protocol differs from the manifest")
	}
	transport, ok := view["transport"].(string)
	if !ok || (transport != "legacy-bun" && transport != "go-request") {
		return fmt.Errorf("manifest_invalid: client_view transport is unsupported")
	}
	if err := validateOperationMap(view["actions"], "client_view.actions"); err != nil {
		return err
	}
	if !reflect.DeepEqual(view["actions"], root["actions"]) {
		return fmt.Errorf("manifest_invalid: client_view actions differ from manifest actions")
	}
	artifacts, err := validateArtifacts(root["artifacts"])
	if err != nil {
		return err
	}
	for _, artifact := range artifacts {
		path := filepath.Join(directory, filepath.FromSlash(artifact.path))
		info, err := os.Lstat(path)
		if err != nil {
			return fmt.Errorf("artifact_missing: %s: %w", artifact.path, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("artifact_unverified: artifact is not a regular file: %s", artifact.path)
		}
		if int64(info.Mode().Perm()) != artifact.mode {
			return fmt.Errorf("artifact_mode_mismatch: artifact mode differs: %s", artifact.path)
		}
		digest, err := fileDigest(path)
		if err != nil {
			return fmt.Errorf("artifact_unverified: cannot read %s: %w", artifact.path, err)
		}
		if digest != artifact.sha256 {
			return fmt.Errorf("artifact_checksum_mismatch: artifact checksum differs: %s", artifact.path)
		}
		if artifact.path == "bin/agent-vm-control" {
			if err := verifyELF(path); err != nil {
				return err
			}
		}
	}
	return nil
}

type checkedArtifact struct {
	artifactSpec
	sha256 string
}

func validateArtifacts(value any) ([]checkedArtifact, error) {
	values, ok := value.([]any)
	if !ok || len(values) != len(fixedArtifacts) {
		return nil, fmt.Errorf("manifest_artifact_set_invalid: artifact set is incomplete")
	}
	expected := make(map[string]artifactSpec, len(fixedArtifacts))
	for _, artifact := range fixedArtifacts {
		expected[artifact.name+"\x00"+artifact.path] = artifact
	}
	seen := make(map[string]bool, len(values))
	result := make([]checkedArtifact, 0, len(values))
	for index, raw := range values {
		item, ok := raw.(map[string]any)
		if !ok || !sameKeys(item, []string{"mode", "name", "path", "role", "sha256"}) {
			return nil, fmt.Errorf("manifest_invalid: artifacts[%d] shape is invalid", index)
		}
		name, nameOK := item["name"].(string)
		path, pathOK := item["path"].(string)
		role, roleOK := item["role"].(string)
		sha, shaOK := item["sha256"].(string)
		mode, modeOK := integerValue(item["mode"])
		validRole := role == "client" || role == "control" || role == "support" || role == "unit"
		if !nameOK || !artifactNamePattern.MatchString(name) || !pathOK || !safeRelativePath(path) || !roleOK || !validRole || !shaOK || !digestPattern.MatchString(sha) || !modeOK || mode < 0 || mode > 0o777 {
			return nil, fmt.Errorf("manifest_invalid: artifacts[%d] values are invalid", index)
		}
		key := name + "\x00" + path
		if seen[key] {
			return nil, fmt.Errorf("manifest_artifact_set_invalid: duplicate artifact %s", name)
		}
		seen[key] = true
		want, exists := expected[key]
		if !exists || want.role != role || want.mode != mode {
			return nil, fmt.Errorf("manifest_artifact_set_invalid: artifact %s is not in the fixed allowlist", name)
		}
		result = append(result, checkedArtifact{artifactSpec: want, sha256: sha})
	}
	if len(seen) != len(expected) {
		return nil, fmt.Errorf("manifest_artifact_set_invalid: artifact set differs from the fixed allowlist")
	}
	return result, nil
}

func validateOperationMap(value any, label string) error {
	object, ok := value.(map[string]any)
	if !ok || !sameKeys(object, actionNames) {
		return fmt.Errorf("manifest_action_set_invalid: %s differs from the fixed control surface", label)
	}
	for _, action := range actionNames {
		item, ok := object[action].(map[string]any)
		if !ok || !sameKeys(item, []string{"client_to_control", "control_to_client", "request", "response"}) {
			return fmt.Errorf("manifest_invalid: %s.%s shape is invalid", label, action)
		}
		request, err := validateSchema(item["request"], label+"."+action+".request")
		if err != nil {
			return err
		}
		response, err := validateSchema(item["response"], label+"."+action+".response")
		if err != nil {
			return err
		}
		requestWindow, err := validateWindow(item["client_to_control"], label+"."+action+".client_to_control")
		if err != nil {
			return err
		}
		responseWindow, err := validateWindow(item["control_to_client"], label+"."+action+".control_to_client")
		if err != nil {
			return err
		}
		if request["schema"].(int64) < requestWindow["schema_min"].(int64) || request["schema"].(int64) > requestWindow["schema_max"].(int64) || request["semantic_revision"].(int64) < requestWindow["semantic_min"].(int64) || request["semantic_revision"].(int64) > requestWindow["semantic_max"].(int64) {
			return fmt.Errorf("manifest_invalid: %s.%s request is outside its compatibility window", label, action)
		}
		if response["schema"].(int64) < responseWindow["schema_min"].(int64) || response["schema"].(int64) > responseWindow["schema_max"].(int64) || response["semantic_revision"].(int64) < responseWindow["semantic_min"].(int64) || response["semantic_revision"].(int64) > responseWindow["semantic_max"].(int64) {
			return fmt.Errorf("manifest_invalid: %s.%s response is outside its compatibility window", label, action)
		}
	}
	return nil
}

func validateSchema(value any, label string) (map[string]any, error) {
	object, ok := value.(map[string]any)
	if !ok || !sameKeys(object, []string{"schema", "semantic_revision"}) {
		return nil, fmt.Errorf("manifest_invalid: %s shape is invalid", label)
	}
	result := make(map[string]any, 2)
	for _, key := range []string{"schema", "semantic_revision"} {
		value, ok := integerValue(object[key])
		if !ok || value < 1 {
			return nil, fmt.Errorf("manifest_invalid: %s.%s is invalid", label, key)
		}
		result[key] = value
	}
	return result, nil
}

func validateWindow(value any, label string) (map[string]any, error) {
	object, ok := value.(map[string]any)
	if !ok || !sameKeys(object, []string{"schema_min", "schema_max", "semantic_min", "semantic_max"}) {
		return nil, fmt.Errorf("manifest_invalid: %s shape is invalid", label)
	}
	result := make(map[string]any, 4)
	for _, key := range []string{"schema_min", "schema_max", "semantic_min", "semantic_max"} {
		value, ok := integerValue(object[key])
		if !ok || value < 1 {
			return nil, fmt.Errorf("manifest_invalid: %s.%s is invalid", label, key)
		}
		result[key] = value
	}
	if result["schema_min"].(int64) > result["schema_max"].(int64) || result["semantic_min"].(int64) > result["semantic_max"].(int64) {
		return nil, fmt.Errorf("manifest_invalid: %s bounds are inverted", label)
	}
	return result, nil
}

func validateTarget(value any) error {
	object, ok := value.(map[string]any)
	if !ok || !sameKeys(object, []string{"arch", "os"}) || object["os"] != "linux" || object["arch"] != runtime.GOARCH {
		return fmt.Errorf("manifest_target_mismatch: target does not match this binary")
	}
	return nil
}

func validateToolchain(value any) error {
	object, ok := value.(map[string]any)
	if !ok || !sameKeys(object, []string{"cgo_enabled", "go"}) || object["go"] != GoToolchain || object["cgo_enabled"] != false {
		return fmt.Errorf("manifest_toolchain_mismatch: Go toolchain declaration is invalid")
	}
	return nil
}

func validateRetiredUnits(value any) error {
	values, ok := value.([]any)
	if !ok || len(values) != len(retiredUnits) {
		return fmt.Errorf("manifest_invalid: retired unit set is invalid")
	}
	for index, expected := range retiredUnits {
		actual, ok := values[index].(string)
		if !ok || actual != expected {
			return fmt.Errorf("manifest_invalid: retired unit set is invalid")
		}
	}
	return nil
}

func integerValue(value any) (int64, bool) {
	number, ok := value.(json.Number)
	if !ok {
		return 0, false
	}
	parsed, err := number.Int64()
	return parsed, err == nil
}

func safeRelativePath(value string) bool {
	if value == "" || strings.HasPrefix(value, "/") || strings.Contains(value, "\\") {
		return false
	}
	for _, part := range strings.Split(value, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

func sameKeys(value map[string]any, expected []string) bool {
	actual := make([]string, 0, len(value))
	for key := range value {
		actual = append(actual, key)
	}
	want := append([]string(nil), expected...)
	sort.Strings(actual)
	sort.Strings(want)
	return reflect.DeepEqual(actual, want)
}

func fileDigest(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func verifyELF(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("binary_unverified: cannot open Go control artifact")
	}
	defer file.Close()
	header := make([]byte, 20)
	if _, err := io.ReadFull(file, header); err != nil {
		return fmt.Errorf("binary_format_mismatch: Go control artifact is not ELF64")
	}
	if header[0] != 0x7f || string(header[1:4]) != "ELF" || header[4] != 2 || header[5] != 1 {
		return fmt.Errorf("binary_format_mismatch: Go control artifact must be little-endian ELF64")
	}
	machine := binary.LittleEndian.Uint16(header[18:20])
	expected := uint16(62)
	if runtime.GOARCH == "arm64" {
		expected = 183
	}
	if machine != expected {
		return fmt.Errorf("binary_arch_mismatch: Go control artifact architecture differs from this binary")
	}
	return nil
}
