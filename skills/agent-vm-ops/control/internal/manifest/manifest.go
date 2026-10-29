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
	Format              = 2
	LegacyProductFormat = 3
	ProductFormat       = 4
	Protocol            = 4
	GoToolchain         = "go1.27.3"
	maxBytes            = 2 * 1024 * 1024
)

var stateCapabilityMarkers = []string{
	"legacy-agent-vm-core@1",
	"managed-image-catalog@1",
	"management-operation-audit@1",
	"session-image-binding@1",
}

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

type Schema struct {
	Schema           int64
	SemanticRevision int64
}

type CompatibilityWindow struct {
	SchemaMin   int64
	SchemaMax   int64
	SemanticMin int64
	SemanticMax int64
}

type ActionMetadata struct {
	Request        Schema
	Response       Schema
	RequestWindow  CompatibilityWindow
	ResponseWindow CompatibilityWindow
}

type Metadata struct {
	Version           string
	ReleaseID         string
	SourceRevision    string
	Protocol          int64
	Transport         string
	Actions           map[string]ActionMetadata
	ManifestPresent   bool
	ArtifactsVerified bool
	StateReads        []string
	StateWrites       []string
}

var productLegacyArtifacts = []artifactSpec{
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

var productArtifacts = append(append([]artifactSpec(nil), productLegacyArtifacts...),
	artifactSpec{name: "unit:agent-vm-web.service", path: "systemd/agent-vm-web.service", role: "unit", mode: 0o644},
	artifactSpec{name: "web-config-example", path: "config/agent-vm-web.json.example", role: "support", mode: 0o640},
	artifactSpec{name: "web-assets-manifest", path: "web-assets.manifest.json", role: "support", mode: 0o644},
)

var fixedArtifacts = productLegacyArtifacts

var artifactNamePattern = regexp.MustCompile(`^[A-Za-z0-9_.:@-]{1,120}$`)
var revisionPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
var digestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
var semverPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$`)

// Load validates format-2 identity, compatibility metadata, fixed artifact
// declarations, artifact bytes, modes, and the target ELF header. An empty
// path is reserved for development runs without an installed release bundle.
func Load(path string) error {
	_, err := LoadMetadata(path)
	return err
}

func LoadMetadata(path string) (Metadata, error) {
	if path == "" {
		return Metadata{}, nil
	}
	root, directory, err := readRoot(path)
	if err != nil {
		return Metadata{}, err
	}
	format, _ := integerValue(root["format"])
	var validateErr error
	if format == LegacyProductFormat || format == ProductFormat {
		validateErr = validateProduct(root, directory)
	} else {
		validateErr = validate(root, directory)
	}
	if validateErr != nil {
		return Metadata{}, validateErr
	}
	metadata, err := metadataFromRoot(root)
	if err != nil {
		return Metadata{}, err
	}
	metadata.ManifestPresent = true
	metadata.ArtifactsVerified = true
	return metadata, nil
}

func readRoot(path string) (map[string]any, string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, "", fmt.Errorf("manifest_missing: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, "", fmt.Errorf("manifest_invalid: manifest is not a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, "", fmt.Errorf("manifest_missing: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("manifest_invalid: %w", err)
	}
	if len(data) > maxBytes {
		return nil, "", fmt.Errorf("manifest_invalid: manifest exceeds the size limit")
	}
	var root map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.UseNumber()
	if err := decoder.Decode(&root); err != nil {
		return nil, "", fmt.Errorf("manifest_invalid: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, "", fmt.Errorf("manifest_invalid: trailing JSON")
	}
	if root == nil {
		return nil, "", fmt.Errorf("manifest_invalid: manifest root must be an object")
	}
	return root, filepath.Dir(path), nil
}

func metadataFromRoot(root map[string]any) (Metadata, error) {
	format, _ := integerValue(root["format"])
	releaseID, releaseOK := root["release_id"].(string)
	sourceRevision, sourceOK := root["source_revision"].(string)
	version, _ := root["version"].(string)
	if format == LegacyProductFormat || format == ProductFormat {
		releaseID = sourceRevision
		releaseOK = sourceOK
	}
	view, viewOK := root["client_view"].(map[string]any)
	transport, transportOK := view["transport"].(string)
	rawActions, actionsOK := root["actions"].(map[string]any)
	if !releaseOK || !sourceOK || !viewOK || !transportOK || !actionsOK {
		return Metadata{}, fmt.Errorf("manifest_invalid: compatibility metadata is incomplete")
	}
	actions := make(map[string]ActionMetadata, len(rawActions))
	for action, raw := range rawActions {
		item, ok := raw.(map[string]any)
		if !ok {
			return Metadata{}, fmt.Errorf("manifest_invalid: action metadata is invalid: %s", action)
		}
		request, err := metadataSchema(item["request"])
		if err != nil {
			return Metadata{}, fmt.Errorf("manifest_invalid: %s request: %w", action, err)
		}
		response, err := metadataSchema(item["response"])
		if err != nil {
			return Metadata{}, fmt.Errorf("manifest_invalid: %s response: %w", action, err)
		}
		requestWindow, err := metadataWindow(item["client_to_control"])
		if err != nil {
			return Metadata{}, fmt.Errorf("manifest_invalid: %s client_to_control: %w", action, err)
		}
		responseWindow, err := metadataWindow(item["control_to_client"])
		if err != nil {
			return Metadata{}, fmt.Errorf("manifest_invalid: %s control_to_client: %w", action, err)
		}
		actions[action] = ActionMetadata{Request: request, Response: response, RequestWindow: requestWindow, ResponseWindow: responseWindow}
	}
	metadata := Metadata{Version: version, ReleaseID: releaseID, SourceRevision: sourceRevision, Protocol: Protocol, Transport: transport, Actions: actions}
	if format == ProductFormat {
		reads, writes, err := stateCompatibility(root["state_compatibility"])
		if err != nil {
			return Metadata{}, err
		}
		metadata.StateReads = reads
		metadata.StateWrites = writes
	}
	return metadata, nil
}

func metadataSchema(value any) (Schema, error) {
	object, ok := value.(map[string]any)
	if !ok {
		return Schema{}, fmt.Errorf("schema is not an object")
	}
	schema, schemaOK := integerValue(object["schema"])
	semantic, semanticOK := integerValue(object["semantic_revision"])
	if !schemaOK || !semanticOK {
		return Schema{}, fmt.Errorf("schema values are invalid")
	}
	return Schema{Schema: schema, SemanticRevision: semantic}, nil
}

func metadataWindow(value any) (CompatibilityWindow, error) {
	object, ok := value.(map[string]any)
	if !ok {
		return CompatibilityWindow{}, fmt.Errorf("window is not an object")
	}
	values := make(map[string]int64, 4)
	for _, key := range []string{"schema_min", "schema_max", "semantic_min", "semantic_max"} {
		parsed, valid := integerValue(object[key])
		if !valid {
			return CompatibilityWindow{}, fmt.Errorf("window value is invalid")
		}
		values[key] = parsed
	}
	return CompatibilityWindow{SchemaMin: values["schema_min"], SchemaMax: values["schema_max"], SemanticMin: values["semantic_min"], SemanticMax: values["semantic_max"]}, nil
}

func supportedTransport(value string) bool {
	return value == "legacy-bun" || value == "go-request" || value == "https"
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
	if !ok || !supportedTransport(transport) {
		return fmt.Errorf("manifest_invalid: client_view transport is unsupported")
	}
	if transport == "legacy-bun" {
		return fmt.Errorf("manifest_transport_target_mismatch: Linux Go releases require go-request or https transport")
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

func validateProduct(root map[string]any, directory string) error {
	format, formatOK := integerValue(root["format"])
	if !formatOK || (format != LegacyProductFormat && format != ProductFormat) {
		return fmt.Errorf("manifest_format_unsupported: manifest format must be %d or %d", LegacyProductFormat, ProductFormat)
	}
	expectedKeys := []string{"actions", "artifacts", "client_view", "format", "protocol", "release_id", "source_revision", "target", "toolchain", "version"}
	if format == ProductFormat {
		expectedKeys = append(expectedKeys, "state_compatibility")
	}
	if !sameKeys(root, expectedKeys) {
		return fmt.Errorf("manifest_invalid: product manifest shape is incomplete or contains extra fields")
	}
	if value, ok := integerValue(root["protocol"]); !ok || value != Protocol {
		return fmt.Errorf("manifest_protocol_mismatch: manifest protocol must be %d", Protocol)
	}
	version, ok := root["version"].(string)
	if !ok || !semverPattern.MatchString(version) {
		return fmt.Errorf("manifest_invalid: product version must be valid SemVer 2.0.0")
	}
	source, ok := root["source_revision"].(string)
	releaseID, releaseOK := root["release_id"].(string)
	if !ok || !revisionPattern.MatchString(source) || !releaseOK || releaseID != source {
		return fmt.Errorf("manifest_invalid: source revision is invalid")
	}
	if err := validateProductTarget(root["target"]); err != nil {
		return err
	}
	if err := validateProductToolchain(root["toolchain"]); err != nil {
		return err
	}
	if err := validateOperationMap(root["actions"], "actions"); err != nil {
		return err
	}
	view, ok := root["client_view"].(map[string]any)
	if !ok || !sameKeys(view, []string{"actions", "protocol", "source_revision", "transport", "version"}) {
		return fmt.Errorf("manifest_invalid: product client_view shape is invalid")
	}
	if view["version"] != version || view["source_revision"] != source {
		return fmt.Errorf("manifest_invalid: product client_view identity differs from manifest")
	}
	if value, ok := integerValue(view["protocol"]); !ok || value != Protocol {
		return fmt.Errorf("manifest_invalid: product client_view protocol differs from manifest")
	}
	transport, ok := view["transport"].(string)
	if !ok || !supportedTransport(transport) {
		return fmt.Errorf("manifest_invalid: product client_view transport is unsupported")
	}
	if transport == "legacy-bun" {
		return fmt.Errorf("manifest_transport_target_mismatch: Linux product releases require go-request or https transport")
	}
	if err := validateOperationMap(view["actions"], "client_view.actions"); err != nil {
		return err
	}
	if !reflect.DeepEqual(view["actions"], root["actions"]) {
		return fmt.Errorf("manifest_invalid: product client_view actions differ from manifest actions")
	}
	artifacts, err := validateProductArtifacts(root["artifacts"], format)
	if err != nil {
		return err
	}
	if format == ProductFormat {
		if _, _, err := stateCompatibility(root["state_compatibility"]); err != nil {
			return err
		}
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

func validateProductTarget(value any) error {
	object, ok := value.(map[string]any)
	if !ok || !sameKeys(object, []string{"arch", "os"}) || object["os"] != "linux" || object["arch"] != runtime.GOARCH {
		return fmt.Errorf("manifest_target_mismatch: product target does not match this binary")
	}
	return nil
}

func validateProductToolchain(value any) error {
	object, ok := value.(map[string]any)
	if !ok || !sameKeys(object, []string{"bun", "cgo_enabled", "go"}) || object["bun"] != "1.4.4" || object["go"] != GoToolchain || object["cgo_enabled"] != false {
		return fmt.Errorf("manifest_toolchain_mismatch: product toolchain declaration is invalid")
	}
	return nil
}

func validateProductArtifacts(value any, format int64) ([]checkedArtifact, error) {
	values, ok := value.([]any)
	expectedArtifacts := productLegacyArtifacts
	if format == ProductFormat {
		expectedArtifacts = productArtifacts
	}
	if !ok || len(values) != len(expectedArtifacts) {
		return nil, fmt.Errorf("manifest_artifact_set_invalid: product artifact set is incomplete")
	}
	expected := make(map[string]artifactSpec, len(expectedArtifacts))
	for _, artifact := range expectedArtifacts {
		expected[artifact.name+"\x00"+artifact.path] = artifact
	}
	seenNames := make(map[string]bool, len(values))
	seenPaths := make(map[string]bool, len(values))
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
		if !nameOK || !artifactNamePattern.MatchString(name) || !pathOK || !safeRelativePath(path) || path == "release-manifest.json" || path == "client-release.json" || !roleOK || !validRole || !shaOK || !digestPattern.MatchString(sha) || !modeOK || mode < 0 || mode > 0o777 {
			return nil, fmt.Errorf("manifest_invalid: artifacts[%d] values are invalid", index)
		}
		key := name + "\x00" + path
		want, exists := expected[key]
		if seenNames[name] || seenPaths[path] || seen[key] || !exists || want.role != role || want.mode != mode {
			return nil, fmt.Errorf("manifest_artifact_set_invalid: duplicate product artifact %s", name)
		}
		seenNames[name] = true
		seenPaths[path] = true
		seen[key] = true
		result = append(result, checkedArtifact{artifactSpec: want, sha256: sha})
	}
	if len(seen) != len(expected) {
		return nil, fmt.Errorf("manifest_artifact_set_invalid: product artifact set differs from the fixed allowlist")
	}
	return result, nil
}

func stateCompatibility(value any) ([]string, []string, error) {
	object, ok := value.(map[string]any)
	if !ok || !sameKeys(object, []string{"reads", "writes"}) {
		return nil, nil, fmt.Errorf("manifest_state_compatibility_invalid: state_compatibility shape is invalid")
	}
	parse := func(value any, label string) ([]string, error) {
		values, ok := value.([]any)
		if !ok || len(values) != len(stateCapabilityMarkers) {
			return nil, fmt.Errorf("manifest_state_compatibility_invalid: %s must declare all state capability markers", label)
		}
		result := make([]string, len(values))
		for index, raw := range values {
			item, ok := raw.(string)
			if !ok || item != stateCapabilityMarkers[index] {
				return nil, fmt.Errorf("manifest_state_compatibility_invalid: %s must use the fixed sorted marker set", label)
			}
			result[index] = item
		}
		return result, nil
	}
	reads, err := parse(object["reads"], "state_compatibility.reads")
	if err != nil {
		return nil, nil, err
	}
	writes, err := parse(object["writes"], "state_compatibility.writes")
	if err != nil {
		return nil, nil, err
	}
	return reads, writes, nil
}

// ValidateStateCapabilities rejects activation when the candidate manifest
// cannot preserve every capability already present in the migrated database.
func ValidateStateCapabilities(metadata Metadata, capabilities []string) error {
	if !metadata.ManifestPresent {
		return nil
	}
	requiredReads := metadata.StateReads
	requiredWrites := metadata.StateWrites
	if len(requiredReads) == 0 {
		requiredReads = []string{stateCapabilityMarkers[0]}
		requiredWrites = []string{stateCapabilityMarkers[0]}
	}
	available := make(map[string]struct{}, len(capabilities))
	for _, capability := range capabilities {
		available[capability] = struct{}{}
	}
	readSet := make(map[string]struct{}, len(requiredReads))
	writeSet := make(map[string]struct{}, len(requiredWrites))
	for _, value := range requiredReads {
		readSet[value] = struct{}{}
	}
	for _, value := range requiredWrites {
		writeSet[value] = struct{}{}
	}
	for capability := range available {
		if _, ok := readSet[capability]; !ok {
			return fmt.Errorf("state_compatibility_mismatch: release cannot read database capability %s", capability)
		}
		if _, ok := writeSet[capability]; !ok {
			return fmt.Errorf("state_compatibility_mismatch: release cannot write database capability %s", capability)
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
