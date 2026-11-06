package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"time"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/deployd"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/install"
)

var version = "agent-vm-deploy/dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 || args[0] == "version" || args[0] == "--version" {
		fmt.Println(version)
		return nil
	}
	if args[0] == "install" {
		return runInstall(args[1:])
	}
	if args[0] == "wait-process" {
		return runWaitProcess(args[1:])
	}
	if args[0] == "wait-authority" {
		return runWaitAuthority(args[1:])
	}
	command := args[0]
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	socket := flags.String("socket", "/run/agent-vm-native/deployd.sock", "deployd Unix socket")
	id := flags.String("transaction", "", "deployment transaction ID")
	slot := flags.String("slot", "", "target slot")
	bundle := flags.String("bundle", "", "verified release bundle")
	action := flags.String("action", "", "recovery action")
	timeoutSeconds := flags.Int("timeout", 60, "request timeout in seconds")
	jsonOutput := flags.Bool("json", false, "output JSON")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	_ = jsonOutput
	if command == "migrate" && *timeoutSeconds == 60 {
		*timeoutSeconds = 9000
	}
	if *timeoutSeconds <= 0 {
		return errors.New("timeout must be positive")
	}
	client := deployd.Client{Socket: *socket, Timeout: time.Duration(*timeoutSeconds) * time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*timeoutSeconds)*time.Second)
	defer cancel()
	var method, path string
	var body any
	switch command {
	case "status":
		if *id == "" {
			method, path = "GET", "/v1/status"
		} else {
			method, path = "GET", "/v1/transactions/"+url.PathEscape(*id)
		}
	case "transaction":
		if *id == "" {
			return errors.New("transaction requires --transaction")
		}
		method, path = "GET", "/v1/transactions/"+url.PathEscape(*id)
	case "plan":
		if *slot == "" {
			return errors.New("plan requires --slot")
		}
		method, path, body = "POST", "/v1/plan", map[string]any{"target_slot": *slot, "bundle": *bundle}
	case "stage":
		if *slot == "" || *bundle == "" {
			return errors.New("stage requires --slot and --bundle")
		}
		method, path, body = "POST", "/v1/stage", map[string]any{"target_slot": *slot, "bundle": *bundle}
	case "verify", "activate", "rollback":
		if *id == "" {
			return fmt.Errorf("%s requires --transaction", command)
		}
		method, path, body = "POST", "/v1/"+command, map[string]any{"transaction_id": *id}
	case "recover":
		if *id == "" || *action == "" {
			return errors.New("recover requires --transaction and --action")
		}
		method, path, body = "POST", "/v1/recover", map[string]any{"transaction_id": *id, "action": *action}
	case "migrate":
		method, path, body = "POST", "/v1/migrate", map[string]any{}
	default:
		return fmt.Errorf("unknown deployd command %q", command)
	}
	payload, _, err := client.Call(ctx, method, path, body)
	if err != nil {
		if len(payload) > 0 {
			_, _ = os.Stderr.Write(payload)
			_, _ = os.Stderr.Write([]byte("\n"))
		}
		return err
	}
	_, _ = os.Stdout.Write(payload)
	if len(payload) == 0 || payload[len(payload)-1] != '\n' {
		_, _ = os.Stdout.Write([]byte("\n"))
	}
	return nil
}

func runInstall(args []string) error {
	flags := flag.NewFlagSet("install", flag.ContinueOnError)
	config := flags.String("config", "", "protected installation input JSON")
	bundle := flags.String("bundle", "", "verified product tar archive")
	materials := flags.String("materials", "", "matching verified deployment materials directory")
	apply := flags.Bool("apply", false, "apply installation after preflight")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *config == "" || *bundle == "" || *materials == "" {
		return errors.New("install requires --config, --bundle and --materials; --apply enables changes")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	input := install.Inputs{Config: *config, Bundle: *bundle, Materials: *materials}
	var report install.Report
	var err error
	if *apply {
		report, err = install.Apply(ctx, input)
	} else {
		report, err = install.Preflight(ctx, input)
	}
	if encodeErr := json.NewEncoder(os.Stdout).Encode(report); encodeErr != nil {
		return encodeErr
	}
	return err
}
