package management

import (
	"context"
	"errors"
	"fmt"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/native"
)

type incusImageClient interface {
	CopyImage(context.Context, string, string) error
	QueryRemoteImageSize(context.Context, string, string) (int64, error)
	ImportImage(context.Context, string, string, []byte, []byte) (string, error)
	ImportImageFiles(context.Context, string, string, string, string) (string, error)
	QueryImage(context.Context, string) (native.ImageInfo, error)
	DeleteImage(context.Context, string) error
	VerifyImage(context.Context, string, []string) (native.ImageVerification, error)
}

// IncusImageBackend is the only management adapter allowed to call the
// native image boundary. It deliberately receives an interface so durable
// operation tests do not need an Incus daemon or a web context.
type IncusImageBackend struct {
	Client        incusImageClient
	StagingPath   string
	RequiredTools []string
}

func NewIncusImageBackend(client incusImageClient, stagingPath string, requiredTools []string) *IncusImageBackend {
	return &IncusImageBackend{Client: client, StagingPath: stagingPath, RequiredTools: append([]string(nil), requiredTools...)}
}

func (backend *IncusImageBackend) ImportTrusted(ctx context.Context, sourceID, expectedFingerprint string) (ImageArtifact, error) {
	if backend == nil || backend.Client == nil {
		return ImageArtifact{}, errors.New("authority_unavailable: image backend is not configured")
	}
	if err := backend.Client.CopyImage(ctx, sourceID, expectedFingerprint); err != nil {
		return ImageArtifact{}, err
	}
	return backend.artifact(ctx, expectedFingerprint)
}

func (backend *IncusImageBackend) TrustedImportSize(ctx context.Context, sourceID, expectedFingerprint string) (int64, error) {
	if backend == nil || backend.Client == nil {
		return 0, errors.New("authority_unavailable: image backend is not configured")
	}
	size, err := backend.Client.QueryRemoteImageSize(ctx, sourceID, expectedFingerprint)
	if err != nil {
		return 0, uncertainBackendError(err)
	}
	return size, nil
}

func (backend *IncusImageBackend) ImportLocal(ctx context.Context, format string, metadata, rootfs []byte) (ImageArtifact, error) {
	if backend == nil || backend.Client == nil {
		return ImageArtifact{}, errors.New("authority_unavailable: image backend is not configured")
	}
	fingerprint, err := backend.Client.ImportImage(ctx, backend.StagingPath, format, metadata, rootfs)
	if err != nil {
		return ImageArtifact{}, err
	}
	return backend.artifact(ctx, fingerprint)
}

func (backend *IncusImageBackend) ImportLocalFiles(ctx context.Context, format, metadataPath, rootfsPath string) (ImageArtifact, error) {
	if backend == nil || backend.Client == nil {
		return ImageArtifact{}, errors.New("authority_unavailable: image backend is not configured")
	}
	fingerprint, err := backend.Client.ImportImageFiles(ctx, backend.StagingPath, format, metadataPath, rootfsPath)
	if err != nil {
		return ImageArtifact{}, err
	}
	return backend.artifact(ctx, fingerprint)
}

func (backend *IncusImageBackend) DeleteImage(ctx context.Context, fingerprint string) error {
	if backend == nil || backend.Client == nil {
		return errors.New("authority_unavailable: image backend is not configured")
	}
	return backend.Client.DeleteImage(ctx, fingerprint)
}

func (backend *IncusImageBackend) artifact(ctx context.Context, fingerprint string) (ImageArtifact, error) {
	info, err := backend.Client.QueryImage(ctx, fingerprint)
	if err != nil {
		return ImageArtifact{}, uncertainBackendError(err)
	}
	if info.Fingerprint != fingerprint {
		return ImageArtifact{}, fmt.Errorf("image fingerprint changed during import")
	}
	verification, err := backend.Client.VerifyImage(ctx, fingerprint, backend.RequiredTools)
	if err != nil {
		return ImageArtifact{}, uncertainBackendError(err)
	}
	return ImageArtifact{
		Fingerprint:  info.Fingerprint,
		Architecture: info.Architecture,
		InstanceType: info.InstanceType,
		Runtime:      verification.Runtime,
		SyncTool:     verification.SyncTool,
		GuestReady:   verification.GuestReady,
	}, nil
}

func uncertainBackendError(err error) error {
	if err == nil || isUncertain(err) {
		return err
	}
	return &native.UncertainError{Cause: err}
}
