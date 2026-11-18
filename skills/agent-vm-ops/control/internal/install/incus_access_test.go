package install

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestIncusProjectAccessUsesProjectAuthorization(t *testing.T) {
	for _, tc := range []struct {
		name                        string
		instances, project, outside int
		wantErr                     bool
	}{
		{"filtered outside collection", http.StatusOK, http.StatusOK, http.StatusForbidden, false},
		{"outside project readable", http.StatusOK, http.StatusOK, http.StatusOK, true},
		{"outside project missing", http.StatusOK, http.StatusOK, http.StatusNotFound, true},
		{"outside authorization unavailable", http.StatusOK, http.StatusOK, http.StatusServiceUnavailable, true},
		{"selected instances denied", http.StatusForbidden, http.StatusOK, http.StatusForbidden, true},
		{"selected project denied", http.StatusOK, http.StatusForbidden, http.StatusForbidden, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Errorf("permission probe attempted %s", r.Method)
					w.WriteHeader(http.StatusMethodNotAllowed)
					return
				}
				switch r.URL.Path {
				case "/1.0/instances":
					if r.URL.Query().Get("project") == "agents" {
						w.WriteHeader(tc.instances)
					} else {
						// A successful empty collection does not grant access to
						// the project; this is the real Incus response at issue.
						w.WriteHeader(http.StatusOK)
					}
					_, _ = w.Write([]byte(`{"metadata":[]}`))
				case "/1.0/projects/agents":
					w.WriteHeader(tc.project)
				case "/1.0/projects/default":
					w.WriteHeader(tc.outside)
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			err := verifyIncusProjectAccess(t.Context(), server.Client(), server.URL, "agents")
			if (err != nil) != tc.wantErr {
				t.Fatalf("permission probe error = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

func TestIncusTrustRequiresExactlyOneProject(t *testing.T) {
	for _, raw := range []string{
		`{"type":"client","restricted":false,"projects":["agents"]}`,
		`{"type":"client","restricted":true,"projects":["agents","another"]}`,
		`{"type":"client","restricted":true,"projects":["another"]}`,
		`{"type":"server","restricted":true,"projects":["agents"]}`,
	} {
		if err := verifyIncusTrust([]byte(raw), "agents"); err == nil {
			t.Errorf("accepted incorrect trust record: %s", raw)
		}
	}
	if err := verifyIncusTrust([]byte(`{"type":"client","restricted":true,"projects":["agents"]}`), "agents"); err != nil {
		t.Fatal(err)
	}
}

func TestNativeIncusInstallerCredentials(t *testing.T) {
	dir := os.Getenv("AGENT_VM_TEST_INCUS_INSTALLER_CERTS")
	if dir == "" {
		t.Skip("requires an isolated Incus server and protected fixture credentials")
	}
	project := os.Getenv("AGENT_VM_TEST_INCUS_INSTALLER_PROJECT")
	cfg := Config{Files: map[string]string{
		"incus_client_certificate": filepath.Join(dir, "client.crt"),
		"incus_client_private_key": filepath.Join(dir, "client.key"),
		"incus_server_certificate": filepath.Join(dir, "server.crt"),
	}}
	if err := checkIncusCredentials(t.Context(), cfg, project); err != nil {
		t.Fatal(err)
	}
	if err := checkIncusCredentials(t.Context(), cfg, project+"-other"); err == nil {
		t.Fatal("accepted same certificate for a different project")
	}
	cfg.Files["incus_server_certificate"] = cfg.Files["incus_client_certificate"]
	if err := checkIncusCredentials(t.Context(), cfg, project); err == nil {
		t.Fatal("accepted a mismatched server certificate pin")
	}
}
