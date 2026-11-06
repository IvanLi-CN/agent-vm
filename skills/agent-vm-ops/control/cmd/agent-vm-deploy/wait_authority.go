package main

import (
	"context"
	"errors"
	"flag"
	"time"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/config"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/state"
)

// A running rqlite process may still be opening its store or electing a leader.
// Probe the existing authority without opening a local mirror or changing schema.
func runWaitAuthority(args []string) error {
	flags := flag.NewFlagSet("wait-authority", flag.ContinueOnError)
	slot := flags.String("slot", "", "slot whose protected authority configuration is used")
	timeout := flags.Int("timeout", 55, "authority startup timeout in seconds")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || (*slot != "blue" && *slot != "green") || *timeout <= 0 || *timeout > 55 {
		return errors.New("wait-authority requires --slot blue|green and --timeout 1..55")
	}
	cfg, err := config.Load("/etc/agent-vm-native/slots/" + *slot + "/config.json")
	if err != nil {
		return errors.New("cannot load protected slot authority configuration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*timeout)*time.Second)
	defer cancel()
	return state.WaitForRQLiteReady(ctx, cfg.StateDB, "/etc/agent-vm-native/tls/rqlite/server.crt")
}
