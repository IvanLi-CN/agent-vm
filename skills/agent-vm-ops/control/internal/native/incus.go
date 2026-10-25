package native

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
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

func (c *IncusClient) json(ctx context.Context, args ...string) (map[string]any, error) {
	result, err := c.command(ctx, append(args, "--format", "json")...)
	if err != nil {
		return nil, err
	}
	if result.StdoutTruncated {
		return nil, fmt.Errorf("incus output exceeded diagnostic limit")
	}
	var value map[string]any
	if err := json.Unmarshal(result.Stdout, &value); err != nil {
		return nil, fmt.Errorf("invalid incus JSON: %w", err)
	}
	return value, nil
}

func (c *IncusClient) Init(ctx context.Context, image, name string) error {
	args := []string{"init", image, name, "--vm"}
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
	_, err := c.command(ctx, args...)
	return err
}

func (c *IncusClient) ConfigSet(ctx context.Context, name, key, value string) error {
	_, err := c.command(ctx, "config", "set", name, key+"="+value)
	return err
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
	_, err = c.command(ctx, "config", "device", action, name, device, key+"="+value)
	return err
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
	_, err := c.command(ctx, args...)
	return err
}

func (c *IncusClient) DeviceRemove(ctx context.Context, name, device string) error {
	_, err := c.command(ctx, "config", "device", "remove", name, device)
	return err
}

func (c *IncusClient) Start(ctx context.Context, name string) error {
	_, err := c.command(ctx, "start", name)
	return err
}
func (c *IncusClient) Stop(ctx context.Context, name string) error {
	_, err := c.command(ctx, "stop", name, "--force")
	return err
}
func (c *IncusClient) Delete(ctx context.Context, name string) error {
	_, err := c.command(ctx, "delete", name, "--force")
	return err
}

func (c *IncusClient) Status(ctx context.Context, name string) (map[string]any, error) {
	return c.json(ctx, "query", "/1.0/instances/"+name+"/state")
}

func (c *IncusClient) Config(ctx context.Context, name string) (map[string]any, error) {
	return c.json(ctx, "query", "/1.0/instances/"+name)
}

func (c *IncusClient) Exec(ctx context.Context, name string, argv []string, input []byte) (Result, error) {
	args := []string{"exec", name, "--"}
	args = append(args, argv...)
	full := []string{"incus"}
	if c.Project != "" {
		full = append(full, "--project", c.Project)
	}
	full = append(full, args...)
	return c.Runner.Run(ctx, full, input)
}

func ParseStatus(value map[string]any) (string, error) {
	status, ok := value["status"].(string)
	if !ok || status == "" {
		return "", fmt.Errorf("incus status is missing")
	}
	return status, nil
}

func PortString(value int64) string { return strconv.FormatInt(value, 10) }
