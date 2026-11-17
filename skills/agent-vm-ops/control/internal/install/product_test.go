package install

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestClientSidecarMustMatchProductIdentity(t *testing.T) {
	root := t.TempDir()
	manifestBytes := []byte(`{"format":3,"version":"1.7.0","source_revision":"candidate","protocol":4,"artifacts":[{"name":"agent_vm.ts","path":"agent_vm.ts","role":"client","sha256":"client"},{"name":"client.ts","path":"client.ts","role":"support","sha256":"dependency"}],"client_view":{"transport":"go-request"}}`)
	sidecar := []byte(`{"format":3,"version":"1.7.0","source_revision":"candidate","protocol":4,"client_artifact":{"name":"agent_vm.ts","path":"agent_vm.ts","role":"client","sha256":"client"},"client_dependencies":[{"name":"client.ts","path":"client.ts","role":"support","sha256":"dependency"}],"client_view":{"transport":"go-request"}}`)
	path := filepath.Join(root, "client-release.json")
	if err := os.WriteFile(path, sidecar, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := verifiedProductSidecar(root, manifestBytes); err != nil {
		t.Fatal(err)
	}
	var altered map[string]any
	if err := json.Unmarshal(sidecar, &altered); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"version", "source_revision", "client_dependencies", "unexpected"} {
		t.Run(field, func(t *testing.T) {
			original, exists := altered[field]
			altered[field] = "tampered"
			bad, err := json.Marshal(altered)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, bad, 0644); err != nil {
				t.Fatal(err)
			}
			if _, err := verifiedProductSidecar(root, manifestBytes); err == nil {
				t.Fatal("mismatched sidecar accepted")
			}
			if exists {
				altered[field] = original
			} else {
				delete(altered, field)
			}
		})
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := verifiedProductSidecar(root, manifestBytes); err == nil {
		t.Fatal("missing client sidecar accepted")
	}
	if sidecar, err := verifiedProductSidecar(root, []byte(`{"artifacts":[]}`)); err != nil || sidecar != nil {
		t.Fatal("native-only package unexpectedly requires client metadata")
	}
}

func TestInstallProductFilesBootstrapFixture(t *testing.T) {
	source := os.Getenv("AGENT_VM_INSTALL_PRODUCT_FIXTURE")
	if source == "" {
		t.Skip("requires an isolated, built bootstrap package")
	}
	if os.Geteuid() != 0 {
		t.Skip("installation writes require root in isolated Linux")
	}
	destination := t.TempDir()
	if err := installProductFiles(source, destination); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"agent_vm.ts", "host-config.ts", "client-release.json", "release-manifest.json", "bin/agent-vm-server", "bin/agent-vm-deploy"} {
		if _, err := os.Stat(filepath.Join(destination, name)); err != nil {
			t.Fatal(name, err)
		}
	}
	for _, name := range []string{"bin/agent-vm-control", "bin/agent-vm-deployd"} {
		if _, err := os.Lstat(filepath.Join(destination, name)); !os.IsNotExist(err) {
			t.Fatal("old binary alias installed", name, err)
		}
	}
}
