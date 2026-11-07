package install

import (
	"context"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/deployd"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPreflightReportsMultipleBlockersWithoutWriting(t *testing.T) {
	root := t.TempDir()
	before, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	report, err := Preflight(context.Background(), Inputs{Config: filepath.Join(root, "missing-config"), Bundle: filepath.Join(root, "missing-bundle"), Materials: filepath.Join(root, "missing-materials")})
	if err == nil || len(report.Blockers) < 2 {
		t.Fatalf("missing blockers: %+v %v", report, err)
	}
	after, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != len(after) {
		t.Fatal("preflight wrote files")
	}
}
func TestProtectedInputRejectsWorldReadAndSymlinks(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "secret.json")
	if err := os.WriteFile(path, []byte(`{}`), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := protectedRead(path); err == nil {
		t.Fatal("public secret accepted")
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	// macOS temporary roots can contain a system symlink; canonicalize for the
	// protected-input contract, which intentionally rejects symlink parents.
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := protectedRead(real); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(filepath.Dir(real), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if _, err := protectedRead(link); err == nil {
		t.Fatal("symlink secret accepted")
	}
}
func TestConfigRejectsInlineSecretsAndUnknownTargets(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "install.json")
	if err := os.WriteFile(path, []byte(`{"schema_version":1,"files":{},"password":"never accepted"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("inline field accepted: %v", err)
	}
}

func TestInstallerMigrationRejectsRedirectedEffects(t *testing.T) {
	defaults := deployd.Defaults().Migration
	good := defaults
	good.RQLiteURL = "https://127.0.0.1:4001"
	if err := validateMigrationLayout(good, defaults); err != nil {
		t.Fatal(err)
	}
	mutations := []func(*deployd.MigrationConfig){
		func(c *deployd.MigrationConfig) { c.LegacyStateDB = "/other/state.sqlite3" },
		func(c *deployd.MigrationConfig) { c.LegacySocket = "/other/app.sock" },
		func(c *deployd.MigrationConfig) { c.LegacyUnit = "other.service" },
		func(c *deployd.MigrationConfig) { c.RQLiteURL = "https://other.invalid:4001" },
		func(c *deployd.MigrationConfig) { c.RQLiteAuthFile = "/other/auth.json" },
		func(c *deployd.MigrationConfig) { c.RQLiteCAFile = "/other/ca.crt" },
		func(c *deployd.MigrationConfig) { c.RecoveryDirectory = "/other/recovery" },
		func(c *deployd.MigrationConfig) { c.RQLiteUnit = "other.service" },
	}
	for i, mutate := range mutations {
		value := good
		mutate(&value)
		if validateMigrationLayout(value, defaults) == nil {
			t.Errorf("accepted redirected migration %d", i)
		}
	}
}
