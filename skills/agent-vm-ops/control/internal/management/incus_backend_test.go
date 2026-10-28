package management

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/native"
)

type fakeIncusImageClient struct {
	calls        []string
	imported     string
	info         native.ImageInfo
	verification native.ImageVerification
	remoteSize   int64
	queryErr     error
	verifyErr    error
}

func (client *fakeIncusImageClient) CopyImage(_ context.Context, sourceID, fingerprint string) error {
	client.calls = append(client.calls, "copy:"+sourceID+":"+fingerprint)
	return nil
}

func (client *fakeIncusImageClient) QueryRemoteImageSize(_ context.Context, sourceID, fingerprint string) (int64, error) {
	client.calls = append(client.calls, "size:"+sourceID+":"+fingerprint)
	return client.remoteSize, nil
}

func (client *fakeIncusImageClient) ImportImage(_ context.Context, stagingPath, format string, metadata, rootfs []byte) (string, error) {
	client.calls = append(client.calls, "import:"+stagingPath+":"+format+":"+string(metadata)+":"+string(rootfs))
	return client.imported, nil
}

func (client *fakeIncusImageClient) ImportImageFiles(_ context.Context, stagingPath, format, metadataPath, rootfsPath string) (string, error) {
	client.calls = append(client.calls, "import-files:"+stagingPath+":"+format+":"+metadataPath+":"+rootfsPath)
	return client.imported, nil
}

func (client *fakeIncusImageClient) QueryImage(_ context.Context, fingerprint string) (native.ImageInfo, error) {
	client.calls = append(client.calls, "query:"+fingerprint)
	if client.queryErr != nil {
		return native.ImageInfo{}, client.queryErr
	}
	return client.info, nil
}

func (client *fakeIncusImageClient) DeleteImage(_ context.Context, fingerprint string) error {
	client.calls = append(client.calls, "delete:"+fingerprint)
	return nil
}

func (client *fakeIncusImageClient) VerifyImage(_ context.Context, fingerprint string, requiredTools []string) (native.ImageVerification, error) {
	client.calls = append(client.calls, "verify:"+fingerprint+":"+strings.Join(requiredTools, ","))
	if client.verifyErr != nil {
		return native.ImageVerification{}, client.verifyErr
	}
	return client.verification, nil
}

func TestIncusImageBackendImportsTrustedImageAndMapsVerification(t *testing.T) {
	fingerprint := strings.Repeat("a", 64)
	client := &fakeIncusImageClient{
		info:         native.ImageInfo{Fingerprint: fingerprint, Architecture: "x86_64", InstanceType: "container"},
		verification: native.ImageVerification{Runtime: true, SyncTool: true, GuestReady: true},
	}
	backend := NewIncusImageBackend(client, "/var/lib/agent-vm-native/staging", []string{"python3"})

	artifact, err := backend.ImportTrusted(context.Background(), "approved", fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	wantArtifact := ImageArtifact{Fingerprint: fingerprint, Architecture: "x86_64", InstanceType: "container", Runtime: true, SyncTool: true, GuestReady: true}
	if !reflect.DeepEqual(artifact, wantArtifact) {
		t.Fatalf("artifact = %#v, want %#v", artifact, wantArtifact)
	}
	wantCalls := []string{"copy:approved:" + fingerprint, "query:" + fingerprint, "verify:" + fingerprint + ":python3"}
	if !reflect.DeepEqual(client.calls, wantCalls) {
		t.Fatalf("backend calls = %#v, want %#v", client.calls, wantCalls)
	}
}

func TestIncusImageBackendChecksTrustedImageSize(t *testing.T) {
	fingerprint := strings.Repeat("a", 64)
	client := &fakeIncusImageClient{remoteSize: 4096}
	backend := NewIncusImageBackend(client, "/var/lib/agent-vm-native/staging", nil)
	size, err := backend.TrustedImportSize(context.Background(), "approved", fingerprint)
	if err != nil || size != 4096 {
		t.Fatalf("trusted image size = %d, err=%v", size, err)
	}
	if !reflect.DeepEqual(client.calls, []string{"size:approved:" + fingerprint}) {
		t.Fatalf("backend calls = %#v", client.calls)
	}
}

func TestIncusImageBackendImportsLocalImageWithStagingContract(t *testing.T) {
	fingerprint := strings.Repeat("b", 64)
	client := &fakeIncusImageClient{
		imported:     fingerprint,
		info:         native.ImageInfo{Fingerprint: fingerprint, Architecture: "aarch64", InstanceType: "container"},
		verification: native.ImageVerification{Runtime: true, SyncTool: false, GuestReady: true},
	}
	backend := NewIncusImageBackend(client, "/var/lib/agent-vm-native/staging", nil)

	artifact, err := backend.ImportLocal(context.Background(), "split", []byte("metadata"), []byte("rootfs"))
	if err != nil {
		t.Fatal(err)
	}
	if artifact.Fingerprint != fingerprint || artifact.Architecture != "aarch64" || artifact.SyncTool {
		t.Fatalf("artifact = %#v", artifact)
	}
	wantCalls := []string{"import:/var/lib/agent-vm-native/staging:split:metadata:rootfs", "query:" + fingerprint, "verify:" + fingerprint + ":"}
	if !reflect.DeepEqual(client.calls, wantCalls) {
		t.Fatalf("backend calls = %#v, want %#v", client.calls, wantCalls)
	}
}

func TestIncusImageBackendRejectsFingerprintMismatch(t *testing.T) {
	fingerprint := strings.Repeat("c", 64)
	client := &fakeIncusImageClient{
		info: native.ImageInfo{Fingerprint: strings.Repeat("d", 64), Architecture: "x86_64", InstanceType: "container"},
	}
	backend := NewIncusImageBackend(client, "/var/lib/agent-vm-native/staging", nil)
	if _, err := backend.ImportTrusted(context.Background(), "approved", fingerprint); err == nil || !strings.Contains(err.Error(), "fingerprint changed") {
		t.Fatalf("fingerprint mismatch error = %v", err)
	}
}

func TestIncusImageBackendMarksPostImportReadFailuresUncertain(t *testing.T) {
	fingerprint := strings.Repeat("f", 64)
	client := &fakeIncusImageClient{queryErr: errors.New("image query failed")}
	backend := NewIncusImageBackend(client, "/var/lib/agent-vm-native/staging", nil)
	artifact, err := backend.ImportTrusted(context.Background(), "approved", fingerprint)
	if err == nil || !isUncertain(err) {
		t.Fatalf("query failure uncertainty = %v", err)
	}
	if artifact.Fingerprint != fingerprint {
		t.Fatalf("query failure lost imported fingerprint: %#v", artifact)
	}

	client = &fakeIncusImageClient{info: native.ImageInfo{Fingerprint: fingerprint}, verifyErr: errors.New("verification failed")}
	backend = NewIncusImageBackend(client, "/var/lib/agent-vm-native/staging", nil)
	artifact, err = backend.ImportTrusted(context.Background(), "approved", fingerprint)
	if err == nil || !isUncertain(err) || artifact.Fingerprint != fingerprint {
		t.Fatalf("verification failure artifact=%#v err=%v", artifact, err)
	}
}

func TestIncusImageBackendDeletesThroughNativeBoundary(t *testing.T) {
	fingerprint := strings.Repeat("e", 64)
	client := &fakeIncusImageClient{}
	backend := NewIncusImageBackend(client, "/var/lib/agent-vm-native/staging", nil)
	if err := backend.DeleteImage(context.Background(), fingerprint); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(client.calls, []string{"delete:" + fingerprint}) {
		t.Fatalf("backend calls = %#v", client.calls)
	}
}
