//go:build linux

package native

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRecoveryCgroupRequiresOnlyAuthorityProcess(t *testing.T) {
	for _, mode := range []string{"only-self", "other-process", "no-process", "child-group", "symlink"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			value := "42\n"
			if mode == "other-process" {
				value += "43\n"
			}
			if mode == "no-process" {
				value = ""
			}
			if err := os.WriteFile(filepath.Join(dir, "cgroup.procs"), []byte(value), 0o600); err != nil {
				t.Fatal(err)
			}
			if mode == "child-group" {
				if err := os.Mkdir(filepath.Join(dir, "child"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "symlink" {
				if err := os.Symlink("cgroup.procs", filepath.Join(dir, "link")); err != nil {
					t.Fatal(err)
				}
			}
			err := checkRecoveryCgroup(dir, 42)
			if (err == nil) != (mode == "only-self") {
				t.Fatalf("checkRecoveryCgroup: %v", err)
			}
		})
	}
}

func TestRecoveryServiceBoundaryRequiresExactIdentity(t *testing.T) {
	valid := "MainPID=42\nControlGroup=/system.slice/agent-vm-native.service\nDelegate=no\nKillMode=control-group\n"
	for _, tc := range []struct {
		name, output string
		ok           bool
	}{
		{"valid", valid, true},
		{"wrong-pid", strings.Replace(valid, "MainPID=42", "MainPID=43", 1), false},
		{"delegated", strings.Replace(valid, "Delegate=no", "Delegate=yes", 1), false},
		{"killmode", strings.Replace(valid, "KillMode=control-group", "KillMode=process", 1), false},
		{"wrong-unit", strings.Replace(valid, "agent-vm-native.service", "other.service", 1), false},
		{"duplicate", valid + "MainPID=42\n", false},
		{"missing", strings.Replace(valid, "Delegate=no\n", "", 1), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			group, err := recoveryServiceGroup([]byte(tc.output), 42)
			if (err == nil) != tc.ok {
				t.Fatalf("group=%q, err=%v", group, err)
			}
		})
	}
	group := "/system.slice/agent-vm-native.service"
	if !recoveryCgroupMatches([]byte("0::"+group+"\n"), group) || recoveryCgroupMatches([]byte("0::/other\n"), group) || recoveryCgroupMatches([]byte("0::"+group+"\n1:cpu:/other\n"), group) {
		t.Fatal("control process cgroup matching was not strict")
	}
}

func TestRecoveryOperationsRequireEmptyValidList(t *testing.T) {
	for _, tc := range []struct {
		input string
		ok    bool
	}{
		{"[]", true}, {"  []\n", true}, {"null", false}, {"{}", false}, {"[{}]", false}, {"[", false}, {"", false},
	} {
		if err := recoveryOperationsClear([]byte(tc.input)); (err == nil) != tc.ok {
			t.Errorf("input=%q err=%v", tc.input, err)
		}
	}
}

func TestRecoverySlotBoundaryRejectsUnverifiedPeers(t *testing.T) {
	group := `/system.slice/system-agent\x2dvm\x2dnative.slice/agent-vm-native@green.service`
	valid := "MainPID=42\nControlGroup=" + group + "\nDelegate=no\nKillMode=control-group\n"
	for _, tc := range []struct {
		name, output string
		ok           bool
	}{
		{"slot", valid, true},
		{"inactive", "MainPID=0\nControlGroup=\nDelegate=no\nKillMode=control-group\n", true},
		{"dead-pid-with-group", strings.Replace(valid, "MainPID=42", "MainPID=0", 1), false},
		{"unexpected-group", strings.Replace(valid, "@green.service", "@blue.service", 1), false},
		{"delegated", strings.Replace(valid, "Delegate=no", "Delegate=yes", 1), false},
		{"wrong-kill-mode", strings.Replace(valid, "KillMode=control-group", "KillMode=process", 1), false},
		{"missing", strings.Replace(valid, "Delegate=no\n", "", 1), false},
		{"duplicate", valid + "MainPID=42\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := recoveryUnitGroup([]byte(tc.output), group)
			if (err == nil) != tc.ok {
				t.Fatalf("unexpected boundary validation: %v", err)
			}
		})
	}
}

func TestInactiveRecoveryCgroupRequiresNoSurvivingProcesses(t *testing.T) {
	for _, mode := range []string{"absent", "empty", "process", "child", "symlink", "missing-procs"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "unit")
			if mode != "absent" {
				if err := os.Mkdir(dir, 0700); err != nil {
					t.Fatal(err)
				}
				if mode != "missing-procs" {
					procs := ""
					if mode == "process" {
						procs = "42\n"
					}
					if err := os.WriteFile(filepath.Join(dir, "cgroup.procs"), []byte(procs), 0600); err != nil {
						t.Fatal(err)
					}
				}
				if mode == "child" {
					if err := os.Mkdir(filepath.Join(dir, "child"), 0700); err != nil {
						t.Fatal(err)
					}
				}
				if mode == "symlink" {
					link := filepath.Join(root, "link")
					if err := os.Symlink(dir, link); err != nil {
						t.Fatal(err)
					}
					dir = link
				}
			}
			err := checkRecoveryCgroup(dir, 0)
			if (err == nil) != (mode == "absent" || mode == "empty") {
				t.Fatalf("unexpected recovery proof: %v", err)
			}
		})
	}
}
