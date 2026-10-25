// Package manifest validates the control artifact's protocol contract before
// the resident service opens the state database or accepts requests.
package manifest

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime"
)

const (
	Format      = 2
	Protocol    = 4
	GoToolchain = "go1.27.3"
)

var actions = map[string]struct{}{
	"acquire": {}, "admin-gc": {}, "admin-metrics": {}, "admin-reconcile": {},
	"admin-session-list": {}, "admin-status": {}, "destroy": {}, "doctor": {},
	"drain-enter": {}, "drain-exit": {}, "drain-status": {}, "port-add": {},
	"port-list": {}, "port-remove": {}, "release": {}, "renew": {}, "resize": {},
	"restart": {}, "ssh-info": {}, "status": {}, "timer-age-gc": {},
	"timer-expire": {}, "timer-reconcile": {},
}

// Load validates a format-2 manifest and the target-specific toolchain claim.
// An empty path means the binary is running from a development checkout.
func Load(path string) error {
	if path == "" {
		return nil
	}
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("manifest_missing: %w", err)
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 2*1024*1024))
	decoder.UseNumber()
	var root map[string]any
	if err := decoder.Decode(&root); err != nil {
		return fmt.Errorf("manifest_invalid: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return fmt.Errorf("manifest_invalid: trailing JSON")
	}
	if integer(root["format"]) != Format || integer(root["protocol"]) != Protocol {
		return fmt.Errorf("manifest_protocol_mismatch: format or protocol is unsupported")
	}
	target, ok := root["target"].(map[string]any)
	if !ok || target["os"] != "linux" || target["arch"] != runtime.GOARCH {
		return fmt.Errorf("manifest_target_mismatch: target does not match this binary")
	}
	toolchain, ok := root["toolchain"].(map[string]any)
	if !ok || toolchain["go"] != GoToolchain || toolchain["cgo_enabled"] != false {
		return fmt.Errorf("manifest_toolchain_mismatch: Go toolchain declaration is invalid")
	}
	declared, ok := root["actions"].(map[string]any)
	if !ok || len(declared) != len(actions) {
		return fmt.Errorf("manifest_action_set_invalid: action set is incomplete")
	}
	for action := range actions {
		if _, ok := declared[action]; !ok {
			return fmt.Errorf("manifest_action_set_invalid: missing action %s", action)
		}
	}
	return nil
}

func integer(value any) int64 {
	number, ok := value.(json.Number)
	if !ok {
		return -1
	}
	value64, err := number.Int64()
	if err != nil {
		return -1
	}
	return value64
}
