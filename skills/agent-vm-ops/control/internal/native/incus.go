package native

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Guest interface {
	Init(context.Context, string, string) error
	ConfigSet(context.Context, string, string, string) error
	DeviceSet(context.Context, string, string, string, string) error
	DeviceAdd(context.Context, string, string, string, map[string]string) error
	DeviceRemove(context.Context, string, string) error
	Start(context.Context, string) error
	Stop(context.Context, string) error
	Delete(context.Context, string) error
	Status(context.Context, string) (map[string]any, error)
	Config(context.Context, string) (map[string]any, error)
	Exec(context.Context, string, []string, []byte) (Result, error)
}

// HealthProbe is the optional bounded native availability check used by
// operator-facing metrics. Keeping it separate preserves lightweight test and
// fixture Guest implementations without weakening the real Incus client.
type HealthProbe interface {
	Probe(context.Context) error
}

// NetworkInventory exposes addresses already claimed by Incus so lifecycle
// admission can allocate a fixed address without racing unrelated instances.
type NetworkInventory interface {
	NetworkIPs(context.Context) (map[string]struct{}, error)
}

type ImageInfo struct {
	Fingerprint  string
	Architecture string
	InstanceType string
	SizeBytes    int64
	Properties   map[string]string
}

type ImageVerification struct {
	Runtime    bool
	SyncTool   bool
	GuestReady bool
}

// UncertainError marks a native image mutation whose effect cannot be safely
// inferred from the command result. The management journal must reconcile it
// instead of replaying the mutation automatically.
type UncertainError struct {
	Cause error
}

func (err *UncertainError) Error() string {
	if err == nil || err.Cause == nil {
		return "native image outcome is uncertain"
	}
	return err.Cause.Error()
}

func (err *UncertainError) Unwrap() error   { return err.Cause }
func (err *UncertainError) Uncertain() bool { return true }

type IncusClient struct {
	Runner  *Runner
	Project string
	Profile string
	Network string
	Pool    string
}

type ClientOptions struct {
	Profile string
	Network string
	Pool    string
}

// CommandError carries the bounded result of a failed Incus invocation. The
// lifecycle layer can retain this diagnostic without widening the native
// command output limits or exposing an unbounded subprocess buffer.
type CommandError struct {
	Result Result
	Cause  error
}

// IsNotFound reports only an Incus command failure whose diagnostics identify
// a missing instance. Callers use this to finalize a durable destroy intent;
// unrelated ownership or transport failures remain unverified.
func IsNotFound(err error) bool {
	var commandErr *CommandError
	if !errors.As(err, &commandErr) {
		return false
	}
	diagnostic := strings.ToLower(string(commandErr.Result.Stderr) + " " + commandErr.Error())
	for _, marker := range []string{"not found", "does not exist", "404"} {
		if strings.Contains(diagnostic, marker) {
			return true
		}
	}
	return false
}

func (err *CommandError) Error() string {
	if err.Cause == nil {
		return "incus command failed"
	}
	return err.Cause.Error()
}

func (err *CommandError) Unwrap() error { return err.Cause }

func (err *CommandError) Diagnostic() map[string]any {
	return map[string]any{
		"exit_code":        err.Result.Code,
		"stdout":           string(err.Result.Stdout),
		"stderr":           string(err.Result.Stderr),
		"stdout_truncated": err.Result.StdoutTruncated,
		"stderr_truncated": err.Result.StderrTruncated,
	}
}

func NewIncusClient(runner *Runner, project string, options ...ClientOptions) *IncusClient {
	if runner == nil {
		runner = NewRunner()
	}
	client := &IncusClient{Runner: runner, Project: project}
	if len(options) > 0 {
		client.Profile, client.Network, client.Pool = options[0].Profile, options[0].Network, options[0].Pool
	}
	return client
}

func (c *IncusClient) command(ctx context.Context, args ...string) (Result, error) {
	argv := []string{"incus"}
	if c.Project != "" {
		argv = append(argv, "--project", c.Project)
	}
	argv = append(argv, args...)
	return c.Runner.Run(ctx, argv, nil)
}

func (c *IncusClient) run(ctx context.Context, args ...string) error {
	result, err := c.command(ctx, args...)
	return wrapCommandError(result, err)
}

func (c *IncusClient) json(ctx context.Context, args ...string) (map[string]any, error) {
	var result Result
	var err error
	if len(args) > 0 && args[0] == "query" {
		if len(args) != 2 {
			return nil, fmt.Errorf("invalid Incus query arguments")
		}
		endpoint, parseErr := url.Parse(args[1])
		if parseErr != nil {
			return nil, fmt.Errorf("invalid Incus query path: %w", parseErr)
		}
		if c.Project != "" {
			query := endpoint.Query()
			query.Set("project", c.Project)
			endpoint.RawQuery = query.Encode()
		}
		result, err = c.Runner.Run(ctx, []string{"incus", "query", endpoint.String()}, nil)
	} else {
		result, err = c.command(ctx, append(args, "--format", "json")...)
	}
	if err != nil {
		return nil, wrapCommandError(result, err)
	}
	if result.StdoutTruncated {
		return nil, wrapCommandError(result, fmt.Errorf("incus output exceeded diagnostic limit"))
	}
	var value map[string]any
	if err := json.Unmarshal(result.Stdout, &value); err != nil {
		return nil, wrapCommandError(result, fmt.Errorf("invalid incus JSON: %w", err))
	}
	return value, nil
}

func wrapCommandError(result Result, err error) error {
	if err == nil {
		return nil
	}
	var commandErr *CommandError
	if errors.As(err, &commandErr) {
		return err
	}
	return &CommandError{Result: result, Cause: err}
}

func (c *IncusClient) Init(ctx context.Context, image, name string) error {
	// Agent sessions are Incus system containers. The outer control
	// environment may itself be nested, but it must not require KVM/QEMU.
	args := []string{"init", image, name}
	if c.Profile != "" {
		args = append(args, "--profile", c.Profile)
	}
	if c.Network != "" {
		args = append(args, "--network", c.Network)
	}
	if c.Pool != "" {
		args = append(args, "--storage", c.Pool)
	}
	args = append(args, "--config", "boot.autostart=false")
	return c.run(ctx, args...)
}

// InitVerification creates a container without profiles or network devices.
// Image suitability checks must not inherit production mounts, credentials or
// network access from the lifecycle profile.
func (c *IncusClient) InitVerification(ctx context.Context, image, name string) error {
	args := []string{"init", image, name, "--no-profiles"}
	if c.Pool != "" {
		args = append(args, "--storage", c.Pool)
	}
	args = append(args, "--config", "boot.autostart=false")
	return c.run(ctx, args...)
}

func (c *IncusClient) CopyImage(ctx context.Context, sourceID, fingerprint string) error {
	if !incusRemotePattern.MatchString(sourceID) {
		return errors.New("invalid image source ID")
	}
	if !imageFingerprintPattern.MatchString(fingerprint) {
		return errors.New("invalid image fingerprint")
	}
	if err := c.run(ctx, "image", "copy", sourceID+":"+fingerprint, "local:"); err != nil {
		return &UncertainError{Cause: err}
	}
	return nil
}

func (c *IncusClient) QueryRemoteImageSize(ctx context.Context, sourceID, fingerprint string) (int64, error) {
	if !incusRemotePattern.MatchString(sourceID) {
		return 0, errors.New("invalid image source ID")
	}
	if !imageFingerprintPattern.MatchString(fingerprint) {
		return 0, errors.New("invalid image fingerprint")
	}
	value, err := c.json(ctx, "image", "info", sourceID+":"+fingerprint)
	if err != nil {
		return 0, err
	}
	info, err := parseImageInfo(value)
	if err != nil {
		return 0, err
	}
	if info.Fingerprint != fingerprint || info.SizeBytes <= 0 {
		return 0, errors.New("Incus remote image response has no usable size")
	}
	return info.SizeBytes, nil
}

func (c *IncusClient) ImportImage(ctx context.Context, stagingPath, format string, metadata, rootfs []byte) (string, error) {
	if format != "unified" && format != "split" {
		return "", errors.New("invalid image format")
	}
	if format == "unified" && len(rootfs) != 0 {
		return "", errors.New("unified image import cannot include a rootfs")
	}
	if format == "split" && len(rootfs) == 0 {
		return "", errors.New("split image import requires a rootfs")
	}
	work, err := createImageStagingDirectory(stagingPath)
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(work)

	metadataPath := filepath.Join(work, "metadata")
	if format == "unified" {
		metadataPath = filepath.Join(work, "image")
	}
	if err := writePrivateImageFile(metadataPath, metadata); err != nil {
		return "", err
	}
	args := []string{"image", "import", metadataPath}
	if format == "split" {
		rootfsPath := filepath.Join(work, "rootfs")
		if err := writePrivateImageFile(rootfsPath, rootfs); err != nil {
			return "", err
		}
		args = append(args, rootfsPath)
	}
	args = append(args, "local:")
	result, err := c.command(ctx, args...)
	if err != nil {
		return "", &UncertainError{Cause: wrapCommandError(result, err)}
	}
	fingerprint, err := importedFingerprint(result.Stdout)
	if err != nil {
		return "", &UncertainError{Cause: err}
	}
	return fingerprint, nil
}

func (c *IncusClient) ImportImageFiles(ctx context.Context, stagingPath, format, metadataPath, rootfsPath string) (string, error) {
	if format != "unified" && format != "split" {
		return "", errors.New("invalid image format")
	}
	if format == "unified" && rootfsPath != "" {
		return "", errors.New("unified image import cannot include a rootfs")
	}
	if format == "split" && rootfsPath == "" {
		return "", errors.New("split image import requires a rootfs")
	}
	if err := validatePrivateStagedFile(stagingPath, metadataPath); err != nil {
		return "", err
	}
	if format == "split" {
		if err := validatePrivateStagedFile(stagingPath, rootfsPath); err != nil {
			return "", err
		}
	}
	args := []string{"image", "import", metadataPath}
	if format == "split" {
		args = append(args, rootfsPath)
	}
	args = append(args, "local:")
	result, err := c.command(ctx, args...)
	if err != nil {
		return "", &UncertainError{Cause: wrapCommandError(result, err)}
	}
	fingerprint, err := importedFingerprint(result.Stdout)
	if err != nil {
		return "", &UncertainError{Cause: err}
	}
	return fingerprint, nil
}

func (c *IncusClient) QueryImage(ctx context.Context, fingerprint string) (ImageInfo, error) {
	if !imageFingerprintPattern.MatchString(fingerprint) {
		return ImageInfo{}, errors.New("invalid image fingerprint")
	}
	value, err := c.json(ctx, "query", "/1.0/images/"+url.PathEscape(fingerprint))
	if err != nil {
		return ImageInfo{}, err
	}
	return parseImageInfo(value)
}

func (c *IncusClient) DeleteImage(ctx context.Context, fingerprint string) error {
	if !imageFingerprintPattern.MatchString(fingerprint) {
		return errors.New("invalid image fingerprint")
	}
	if err := c.run(ctx, "image", "delete", "local:"+fingerprint); err != nil {
		if IsNotFound(err) {
			return nil
		}
		return &UncertainError{Cause: err}
	}
	return nil
}

func (c *IncusClient) VerifyImage(ctx context.Context, fingerprint string, requiredTools []string) (verification ImageVerification, returnErr error) {
	if !imageFingerprintPattern.MatchString(fingerprint) {
		return ImageVerification{}, errors.New("invalid image fingerprint")
	}
	name, err := verificationInstanceName()
	if err != nil {
		return ImageVerification{}, err
	}
	cleanup := func() error {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		err := c.Delete(cleanupCtx, name)
		if err != nil && IsNotFound(err) {
			return nil
		}
		return err
	}
	defer func() {
		if cleanupErr := cleanup(); cleanupErr != nil {
			if returnErr == nil {
				returnErr = &UncertainError{Cause: cleanupErr}
				return
			}
			returnErr = &UncertainError{Cause: errors.Join(returnErr, cleanupErr)}
		}
	}()
	verifyCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := c.InitVerification(verifyCtx, fingerprint, name); err != nil {
		return ImageVerification{}, &UncertainError{Cause: err}
	}
	for _, limit := range []struct{ key, value string }{
		{key: "limits.cpu", value: "1"},
		{key: "limits.memory", value: "512MiB"},
		{key: "limits.processes", value: "256"},
	} {
		if err := c.ConfigSet(verifyCtx, name, limit.key, limit.value); err != nil {
			return ImageVerification{}, &UncertainError{Cause: err}
		}
	}
	if err := c.DeviceSet(verifyCtx, name, "root", "size", "8GiB"); err != nil {
		return ImageVerification{}, &UncertainError{Cause: err}
	}
	if err := c.Start(verifyCtx, name); err != nil {
		return ImageVerification{}, &UncertainError{Cause: err}
	}
	tools := uniqueVerificationTools(requiredTools)
	probeArgs := []string{"sh", "-c", verificationScript, "agent-vm-image-check"}
	probeArgs = append(probeArgs, tools...)
	result, err := c.Exec(verifyCtx, name, probeArgs, nil)
	if err != nil {
		return ImageVerification{}, &UncertainError{Cause: err}
	}
	seen := map[string]bool{}
	for _, line := range strings.Split(string(result.Stdout), "\n") {
		key, value, found := strings.Cut(strings.TrimSpace(line), "=")
		if !found || (value != "true" && value != "false") {
			continue
		}
		seen[key] = true
		switch key {
		case "runtime":
			verification.Runtime = value == "true"
		case "sync_tool":
			verification.SyncTool = value == "true"
		case "guest_ready":
			verification.GuestReady = value == "true"
		}
	}
	if !seen["runtime"] || !seen["sync_tool"] || !seen["guest_ready"] {
		return ImageVerification{}, &UncertainError{Cause: errors.New("image verification output is incomplete")}
	}
	return verification, nil
}

func (c *IncusClient) ConfigSet(ctx context.Context, name, key, value string) error {
	return c.run(ctx, "config", "set", name, key+"="+value)
}

func (c *IncusClient) DeviceSet(ctx context.Context, name, device, key, value string) error {
	info, err := c.Config(ctx, name)
	if err != nil {
		return err
	}
	action := "override"
	if devices, ok := info["devices"].(map[string]any); ok {
		if _, exists := devices[device]; exists {
			action = "set"
		}
	}
	return c.run(ctx, "config", "device", action, name, device, key+"="+value)
}

func (c *IncusClient) DeviceAdd(ctx context.Context, name, device, deviceType string, properties map[string]string) error {
	args := []string{"config", "device", "add", name, device, deviceType}
	keys := make([]string, 0, len(properties))
	for key := range properties {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value := properties[key]
		args = append(args, key+"="+value)
	}
	return c.run(ctx, args...)
}

func (c *IncusClient) DeviceRemove(ctx context.Context, name, device string) error {
	return c.run(ctx, "config", "device", "remove", name, device)
}

func (c *IncusClient) Start(ctx context.Context, name string) error {
	return c.run(ctx, "start", name)
}
func (c *IncusClient) Stop(ctx context.Context, name string) error {
	return c.run(ctx, "stop", name, "--force")
}
func (c *IncusClient) Delete(ctx context.Context, name string) error {
	return c.run(ctx, "delete", name, "--force")
}

func (c *IncusClient) Probe(ctx context.Context) error {
	return c.run(ctx, "version")
}

func (c *IncusClient) NetworkIPs(ctx context.Context) (map[string]struct{}, error) {
	used := make(map[string]struct{})
	instances, err := c.queryList(ctx, "/1.0/instances?recursion=2", c.Project)
	if err != nil {
		return nil, err
	}
	for _, raw := range instances {
		instance, ok := raw.(map[string]any)
		if !ok {
			return nil, errors.New("incus instance inventory is malformed")
		}
		devices, ok := instance["expanded_devices"].(map[string]any)
		if !ok {
			return nil, errors.New("incus instance network inventory is unavailable")
		}
		for _, rawDevice := range devices {
			device, ok := rawDevice.(map[string]any)
			if !ok {
				return nil, errors.New("incus instance network device is malformed")
			}
			network, _ := device["network"].(string)
			address, _ := device["ipv4.address"].(string)
			if network == c.Network && address != "" {
				used[address] = struct{}{}
			}
		}
	}
	leases, err := c.queryList(ctx, "/1.0/networks/"+url.PathEscape(c.Network)+"/leases", "default")
	if err != nil {
		return nil, err
	}
	for _, raw := range leases {
		lease, ok := raw.(map[string]any)
		if !ok {
			return nil, errors.New("incus network lease inventory is malformed")
		}
		address, _ := lease["address"].(string)
		if address != "" {
			used[address] = struct{}{}
		}
	}
	return used, nil
}

func (c *IncusClient) Status(ctx context.Context, name string) (map[string]any, error) {
	return c.json(ctx, "query", "/1.0/instances/"+name+"/state")
}

func (c *IncusClient) Config(ctx context.Context, name string) (map[string]any, error) {
	return c.json(ctx, "query", "/1.0/instances/"+name)
}

func (c *IncusClient) queryList(ctx context.Context, path, project string) ([]any, error) {
	endpoint, err := url.Parse(path)
	if err != nil {
		return nil, fmt.Errorf("invalid Incus query path: %w", err)
	}
	query := endpoint.Query()
	if project != "" {
		query.Set("project", project)
	}
	endpoint.RawQuery = query.Encode()
	result, runErr := c.Runner.Run(ctx, []string{"incus", "query", endpoint.String()}, nil)
	if runErr != nil {
		return nil, wrapCommandError(result, runErr)
	}
	if result.StdoutTruncated {
		return nil, wrapCommandError(result, errors.New("incus output exceeded diagnostic limit"))
	}
	var value []any
	if err := json.Unmarshal(result.Stdout, &value); err != nil {
		return nil, wrapCommandError(result, fmt.Errorf("invalid Incus JSON: %w", err))
	}
	return value, nil
}

func (c *IncusClient) Exec(ctx context.Context, name string, argv []string, input []byte) (Result, error) {
	args := []string{"exec", name, "--"}
	args = append(args, argv...)
	full := []string{"incus"}
	if c.Project != "" {
		full = append(full, "--project", c.Project)
	}
	full = append(full, args...)
	result, err := c.Runner.Run(ctx, full, input)
	return result, wrapCommandError(result, err)
}

var (
	imageFingerprintPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
	incusRemotePattern      = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)
)

const verificationScript = `set -eu
runtime=false
sync_tool=false
guest_ready=true
missing=false
for tool in "$@"; do
  if command -v "$tool" >/dev/null 2>&1; then
    if [ "$tool" = "bun" ]; then runtime=true; fi
    if [ "$tool" = "rsync" ]; then sync_tool=true; fi
  else
    missing=true
  fi
done
printf 'runtime=%s\n' "$runtime"
printf 'sync_tool=%s\n' "$sync_tool"
if [ "$missing" = true ]; then guest_ready=false; fi
printf 'guest_ready=%s\n' "$guest_ready"`

func createImageStagingDirectory(path string) (string, error) {
	if err := validatePrivateStagingDirectory(path); err != nil {
		return "", err
	}
	return os.MkdirTemp(path, ".agent-vm-image-import-")
}

func validatePrivateStagingDirectory(path string) error {
	if path == "" || !filepath.IsAbs(path) {
		return errors.New("image staging path is not configured")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return errors.New("image staging path is not a private directory")
	}
	return nil
}

func validatePrivateStagedFile(stagingPath, path string) error {
	if err := validatePrivateStagingDirectory(stagingPath); err != nil {
		return err
	}
	if path == "" || !filepath.IsAbs(path) {
		return errors.New("image staging file is not configured")
	}
	relative, err := filepath.Rel(stagingPath, path)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return errors.New("image staging file is outside the private staging directory")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return errors.New("image staging file is not private")
	}
	return nil
}

func writePrivateImageFile(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	if _, err := file.Write(data); err != nil {
		return err
	}
	return file.Chmod(0o600)
}

func importedFingerprint(output []byte) (string, error) {
	for _, field := range strings.Fields(string(output)) {
		if imageFingerprintPattern.MatchString(field) {
			return field, nil
		}
	}
	return "", errors.New("Incus import did not return an image fingerprint")
}

func parseImageInfo(value map[string]any) (ImageInfo, error) {
	fingerprint, _ := value["fingerprint"].(string)
	if !imageFingerprintPattern.MatchString(fingerprint) {
		return ImageInfo{}, errors.New("Incus image response has an invalid fingerprint")
	}
	architecture, _ := value["architecture"].(string)
	if architecture == "" {
		return ImageInfo{}, errors.New("Incus image response has no architecture")
	}
	instanceType, _ := value["type"].(string)
	if instanceType == "" {
		instanceType = "container"
	}
	properties := map[string]string{}
	if raw, ok := value["properties"].(map[string]any); ok {
		for key, item := range raw {
			property, ok := item.(string)
			if !ok {
				return ImageInfo{}, errors.New("Incus image response has malformed properties")
			}
			properties[key] = property
		}
	}
	sizeBytes := int64(0)
	if raw, ok := value["size"]; ok {
		switch parsed := raw.(type) {
		case float64:
			if parsed < 0 || parsed > float64(^uint64(0)>>1) || parsed != float64(int64(parsed)) {
				return ImageInfo{}, errors.New("Incus image response has an invalid size")
			}
			sizeBytes = int64(parsed)
		case string:
			value, err := strconv.ParseInt(parsed, 10, 64)
			if err != nil || value < 0 {
				return ImageInfo{}, errors.New("Incus image response has an invalid size")
			}
			sizeBytes = value
		default:
			return ImageInfo{}, errors.New("Incus image response has an invalid size")
		}
	}
	return ImageInfo{Fingerprint: fingerprint, Architecture: architecture, InstanceType: instanceType, SizeBytes: sizeBytes, Properties: properties}, nil
}

func verificationInstanceName() (string, error) {
	var suffix [6]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", err
	}
	return "agent-vm-image-check-" + hex.EncodeToString(suffix[:]), nil
}

func uniqueVerificationTools(values []string) []string {
	result := []string{"bun", "rsync"}
	seen := map[string]struct{}{"bun": {}, "rsync": {}}
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func ParseStatus(value map[string]any) (string, error) {
	status, ok := value["status"].(string)
	if !ok || status == "" {
		return "", fmt.Errorf("incus status is missing")
	}
	return status, nil
}

func PortString(value int64) string { return strconv.FormatInt(value, 10) }
