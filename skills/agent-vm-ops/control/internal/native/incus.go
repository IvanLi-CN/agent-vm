package native

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"sort"
	"strconv"
	"strings"
)

type Guest interface {
	Init(context.Context, string, string, map[string]string) error
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
func IsNotFound(err error, instance, project string) bool {
	var commandErr *CommandError
	var exitErr *exec.ExitError
	if !errors.As(err, &commandErr) || !errors.As(commandErr.Cause, &exitErr) || commandErr.Result.Code <= 0 || commandErr.Result.StderrTruncated {
		return false
	}
	diagnostic := strings.ToLower(strings.TrimSpace(string(commandErr.Result.Stderr)))
	if diagnostic == "instance not found" || diagnostic == "error: instance not found" {
		return true
	}
	if instance == "" || project == "" {
		return false
	}
	contextual := fmt.Sprintf("error: failed to fetch instance %q in projectname %q: instance not found", instance, project)
	return diagnostic == strings.ToLower(contextual)
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

func (c *IncusClient) Init(ctx context.Context, image, name string, ownership map[string]string) error {
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
	keys := make([]string, 0, len(ownership))
	for key := range ownership {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		args = append(args, "--config", key+"="+ownership[key])
	}
	return c.run(ctx, args...)
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

func ParseStatus(value map[string]any) (string, error) {
	status, ok := value["status"].(string)
	if !ok || status == "" {
		return "", fmt.Errorf("incus status is missing")
	}
	return status, nil
}

func PortString(value int64) string { return strconv.FormatInt(value, 10) }
