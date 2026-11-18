package install

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"time"
)

const incusLocalHTTPS = "https://127.0.0.1:8443"

func incusCLIConfig(project string) ([]byte, error) {
	return json.Marshal(map[string]any{
		"default-remote": "agent-vm",
		"remotes": map[string]any{
			"agent-vm": map[string]any{"addr": incusLocalHTTPS, "auth_type": "tls", "project": project, "protocol": "incus", "public": false},
		},
	})
}

func checkIncusCredentials(ctx context.Context, cfg Config, project string) error {
	if project == "" || project == "default" {
		return errors.New("restricted Incus credentials require a dedicated non-default project")
	}
	clientCert, err := protectedRead(cfg.Files["incus_client_certificate"])
	if err != nil {
		return err
	}
	clientKey, err := protectedRead(cfg.Files["incus_client_private_key"])
	if err != nil {
		return err
	}
	cert, err := tls.X509KeyPair(clientCert, clientKey)
	if err != nil {
		return errors.New("invalid Incus client key pair")
	}
	fingerprint := sha256.Sum256(cert.Certificate[0])
	trustPath := fmt.Sprintf("/1.0/certificates/%x", fingerprint)
	probeCtx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	trust, err := exec.CommandContext(probeCtx, "incus", "--force-local", "query", trustPath).Output()
	if err != nil {
		return errors.New("Incus client certificate is not verified in the local trust store")
	}
	if err := verifyIncusTrust(trust, project); err != nil {
		return err
	}
	serverCert, err := protectedRead(cfg.Files["incus_server_certificate"])
	if err != nil {
		return err
	}
	pin, rest := pem.Decode(serverCert)
	if pin == nil || pin.Type != "CERTIFICATE" || len(bytes.TrimSpace(rest)) != 0 {
		return errors.New("Incus server pin must contain one PEM certificate")
	}
	if _, err := x509.ParseCertificate(pin.Bytes); err != nil {
		return errors.New("invalid Incus server certificate")
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{
		MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert},
		// Incus supplies a pinned server certificate that can lack an IP SAN.
		InsecureSkipVerify: true,
		VerifyConnection: func(state tls.ConnectionState) error {
			if len(state.PeerCertificates) == 0 {
				return errors.New("missing Incus server certificate")
			}
			leaf := state.PeerCertificates[0]
			now := time.Now()
			if !bytes.Equal(pin.Bytes, leaf.Raw) || now.Before(leaf.NotBefore) || now.After(leaf.NotAfter) {
				return errors.New("Incus server certificate does not match the current pin")
			}
			return nil
		},
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 6 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return verifyIncusProjectAccess(ctx, client, incusLocalHTTPS, project)
}

func verifyIncusTrust(raw []byte, project string) error {
	var record struct {
		Type       string   `json:"type"`
		Restricted bool     `json:"restricted"`
		Projects   []string `json:"projects"`
	}
	if err := json.Unmarshal(raw, &record); err != nil || record.Type != "client" || !record.Restricted || len(record.Projects) != 1 || record.Projects[0] != project {
		return errors.New("Incus client certificate must be restricted to exactly the configured project")
	}
	return nil
}

func verifyIncusProjectAccess(ctx context.Context, client *http.Client, base, project string) error {
	for _, item := range []struct {
		project string
		status  int
	}{{project, http.StatusOK}, {"default", http.StatusForbidden}} {
		query := url.Values{"project": {item.project}}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/1.0/instances?"+query.Encode(), nil)
		if err != nil {
			return err
		}
		response, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("Incus TLS project probe failed: %w", err)
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
		_ = response.Body.Close()
		if response.StatusCode != item.status {
			return fmt.Errorf("Incus scoped access for project %q returned HTTP %d, expected %d", item.project, response.StatusCode, item.status)
		}
	}
	return nil
}
