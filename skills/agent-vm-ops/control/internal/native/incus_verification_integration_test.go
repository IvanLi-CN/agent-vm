package native

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// This opt-in check creates only disposable verification containers. It must
// run in an explicitly authorized Incus operations environment, not a guest
// granted only ordinary session access.
func TestIncusImageVerificationIntegration(t *testing.T) {
	fingerprints := os.Getenv("AGENT_VM_TEST_IMAGE_FINGERPRINTS")
	if fingerprints == "" {
		t.Skip("set AGENT_VM_TEST_IMAGE_FINGERPRINTS for authorized Incus validation")
	}
	project, pool := os.Getenv("AGENT_VM_TEST_INCUS_PROJECT"), os.Getenv("AGENT_VM_TEST_INCUS_POOL")
	if project == "" || pool == "" {
		t.Fatal("explicit AGENT_VM_TEST_INCUS_PROJECT and AGENT_VM_TEST_INCUS_POOL are required")
	}
	profile := os.Getenv("AGENT_VM_TEST_INCUS_PROFILE")
	if profile == "" {
		t.Fatal("explicit AGENT_VM_TEST_INCUS_PROFILE is required")
	}
	client := NewIncusClient(NewRunner(), project, ClientOptions{Pool: pool, Profile: profile})
	expected := os.Getenv("AGENT_VM_TEST_PRIVILEGED")
	if expected != "true" && expected != "false" {
		t.Fatal("explicit AGENT_VM_TEST_PRIVILEGED=true|false is required")
	}
	for _, fingerprint := range strings.Split(fingerprints, ",") {
		t.Run(fingerprint, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			name, err := verificationInstanceName()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cleanupCancel()
				if err := client.Delete(cleanupCtx, name); err != nil && !IsNotFound(err, name, project) {
					t.Errorf("verification cleanup: %v", err)
				}
			})
			if err := client.InitVerification(ctx, fingerprint, name); err != nil {
				t.Fatal(err)
			}
			config, err := client.Config(ctx, name)
			if err != nil {
				t.Fatal(err)
			}
			settings, ok := config["config"].(map[string]any)
			if !ok || settings["security.privileged"] != expected || settings["security.nesting"] != "true" || settings["boot.autostart"] != "false" {
				t.Fatalf("verification execution mode was not explicitly configured: %#v", settings)
			}
			profiles, ok := config["profiles"].([]any)
			if !ok || len(profiles) != 0 {
				t.Fatalf("verification inherited profiles: %#v", config["profiles"])
			}
			devices, ok := config["expanded_devices"].(map[string]any)
			if !ok || len(devices) != 1 {
				t.Fatalf("verification must have only its own root disk: %#v", devices)
			}
			root, ok := devices["root"].(map[string]any)
			if !ok || root["type"] != "disk" || root["path"] != "/" || root["pool"] != pool || root["source"] != nil {
				t.Fatalf("unexpected verification root disk: %#v", root)
			}
			verification, err := client.VerifyImage(ctx, fingerprint, []string{"bun", "python3", "sshd", "rsync", "docker", "growpart"})
			if err != nil {
				t.Fatal(err)
			}
			if !verification.Runtime || !verification.SyncTool || !verification.GuestReady {
				t.Fatalf("image failed suitability verification: %+v", verification)
			}
		})
	}
}
