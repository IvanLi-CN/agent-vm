// Package management contains the browser-independent management authority.
// It owns image identity, durable administrator operations, and the checks
// that must happen before a native lifecycle action is delegated.
package management

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/protocol"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/state"
)

var fingerprintPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

type Service struct {
	Store   *state.Store
	Clock   func() time.Time
	Options Options
}

type Options struct {
	TrustedImageSourceIDs  []string
	SupportedArchitectures []string
	UploadMaxBytes         int64
	UploadMinimumFreeBytes int64
	AvailableStagingBytes  func() (int64, error)
	Clock                  func() time.Time
	Backend                ImageBackend
	Actions                ActionBackend
}

type ImageBackend interface {
	ImportTrusted(context.Context, string, string) (ImageArtifact, error)
	ImportLocal(context.Context, string, []byte, []byte) (ImageArtifact, error)
	DeleteImage(context.Context, string) error
}

type ImageArtifact struct {
	Fingerprint  string
	Architecture string
	InstanceType string
	Runtime      bool
	SyncTool     bool
	GuestReady   bool
}

type ActionBackend interface {
	ExecuteVMAction(context.Context, string, string, string, int64, map[string]any) (map[string]any, error)
	ExecuteMaintenance(context.Context, string, string, map[string]any) (map[string]any, error)
}

type ImportPart struct {
	ExpectedSHA256 string
	ExpectedSize   int64
}

type ImportSource struct {
	Kind                string
	Format              string
	SourceID            string
	ExpectedFingerprint string
	ExpectedSHA256      string
	ExpectedSize        int64
	Metadata            ImportPart
	Rootfs              ImportPart
}

type ImportRequest struct {
	OperationID   string
	Actor         string
	Reason        string
	TargetVersion string
	VersionLabel  string
	Source        ImportSource
}

type VMActionRequest struct {
	OperationID        string
	Actor              string
	Reason             string
	TaskID             string
	TargetVersion      string
	Action             string
	ExpectedGeneration int64
	Parameters         map[string]any
}

type MaintenanceRequest struct {
	OperationID   string
	Actor         string
	Reason        string
	TargetVersion string
	Action        string
	Parameters    map[string]any
}

func New(store *state.Store, options Options) *Service {
	clock := options.Clock
	if clock == nil {
		clock = time.Now
	}
	return &Service{Store: store, Clock: clock, Options: options}
}

func (s *Service) Initialize(ctx context.Context, legacyFingerprint string) error {
	if err := s.Store.EnsureManagementSchema(ctx); err != nil {
		return err
	}
	if legacyFingerprint != "" {
		return s.Store.SeedLegacyImage(ctx, legacyFingerprint)
	}
	return nil
}

func (s *Service) ListImages(ctx context.Context, filter state.ImageFilter) ([]state.ManagedImage, string, error) {
	images, err := s.Store.ListManagedImages(ctx, filter)
	if err != nil {
		return nil, "", err
	}
	version, err := s.Store.ImageSelectionVersion(ctx)
	return images, version, err
}

func (s *Service) ImageCatalogVersion(ctx context.Context) (string, error) {
	return s.Store.ImageCatalogVersion(ctx)
}

func (s *Service) GetImage(ctx context.Context, fingerprint string) (*state.ManagedImage, error) {
	if !fingerprintPattern.MatchString(fingerprint) {
		return nil, errors.New("invalid_request: fingerprint must be lowercase SHA-256")
	}
	return s.Store.GetManagedImage(ctx, fingerprint)
}

func (s *Service) GetOperation(ctx context.Context, operationID string) (*state.ManagementOperation, error) {
	return s.Store.GetManagementOperation(ctx, operationID)
}

func (s *Service) ListOperations(ctx context.Context, limit int) ([]state.ManagementOperation, error) {
	return s.Store.ListManagementOperations(ctx, limit)
}

func (s *Service) ListAudit(ctx context.Context, limit int) ([]state.ManagementAudit, error) {
	return s.Store.ListManagementAudit(ctx, limit)
}

func (s *Service) Capabilities(ctx context.Context) ([]state.ManagementCapability, error) {
	return s.Store.Capabilities(ctx)
}

func (s *Service) existingOperation(ctx context.Context, operationID, digest string) (*state.ManagementOperation, error) {
	if operationID == "" {
		return nil, nil
	}
	existing, err := s.Store.GetManagementOperation(ctx, operationID)
	if err != nil || existing == nil {
		return existing, err
	}
	if stringValue(existing.Request["_request_digest"]) != digest {
		return nil, errors.New("operation_conflict: operation ID belongs to a different request")
	}
	return existing, nil
}

func (s *Service) StartImageImport(ctx context.Context, request ImportRequest) (*state.ManagementOperation, error) {
	if err := s.ValidateImport(request); err != nil {
		return nil, err
	}
	requestMap := importRequestMap(request)
	digest, err := protocol.Digest(protocol.Request(requestMap))
	if err != nil {
		return nil, errors.New("invalid_request: import request cannot be canonicalized")
	}
	if existing, err := s.existingOperation(ctx, request.OperationID, digest); err != nil || existing != nil {
		return existing, err
	}
	catalogVersion, err := s.Store.ImageCatalogVersion(ctx)
	if err != nil {
		return nil, err
	}
	if request.TargetVersion != catalogVersion {
		return nil, fmt.Errorf("stale_target: expected %s, current %s", request.TargetVersion, catalogVersion)
	}
	operation := state.ManagementOperation{
		OperationID: request.OperationID,
		Action:      "image_import",
		TargetKind:  "image_catalog",
		TargetID:    "image-catalog",
		Actor:       request.Actor,
		Reason:      request.Reason,
		Phase:       "awaiting_upload",
		Status:      "awaiting_upload",
		Request:     map[string]any{"_request_digest": digest, "request": requestMap},
	}
	if request.Source.Kind == "trusted_remote" {
		operation.Phase, operation.Status = "queued", "queued"
	}
	existing, err := s.Store.ClaimManagementOperation(ctx, operation)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		if stringValue(existing.Request["_request_digest"]) != digest {
			return nil, errors.New("operation_conflict: operation ID belongs to a different request")
		}
		return existing, nil
	}
	if err := s.audit(ctx, operation, "accepted", operation.Phase, nil); err != nil {
		return nil, err
	}
	return &operation, nil
}

func (s *Service) ValidateImport(request ImportRequest) error {
	if request.OperationID == "" || request.Actor == "" || strings.TrimSpace(request.Reason) == "" {
		return errors.New("invalid_request: operation_id, actor and reason are required")
	}
	if len(request.Reason) > 512 {
		return errors.New("invalid_request: reason exceeds 512 characters")
	}
	if request.TargetVersion == "" || request.VersionLabel == "" {
		return errors.New("invalid_request: target_version and version_label are required")
	}
	source := request.Source
	switch source.Kind {
	case "trusted_remote":
		if source.SourceID == "" || !s.trustedSource(source.SourceID) {
			return errors.New("source_not_allowed: image source is not configured")
		}
		if !fingerprintPattern.MatchString(source.ExpectedFingerprint) {
			return errors.New("invalid_request: expected_fingerprint must be lowercase SHA-256")
		}
	case "local_upload":
		if source.Format != "unified" && source.Format != "split" {
			return errors.New("invalid_request: unsupported local image format")
		}
		if source.Format == "unified" {
			if err := validatePart(source.ExpectedSHA256, source.ExpectedSize, s.Options.UploadMaxBytes); err != nil {
				return err
			}
			if err := s.checkStagingCapacity(source.ExpectedSize); err != nil {
				return err
			}
		} else {
			if err := validatePart(source.Metadata.ExpectedSHA256, source.Metadata.ExpectedSize, s.Options.UploadMaxBytes); err != nil {
				return err
			}
			if err := validatePart(source.Rootfs.ExpectedSHA256, source.Rootfs.ExpectedSize, s.Options.UploadMaxBytes); err != nil {
				return err
			}
			if source.Metadata.ExpectedSize > math.MaxInt64-source.Rootfs.ExpectedSize {
				return errors.New("upload_too_large: declared image size overflows")
			}
			if s.Options.UploadMaxBytes > 0 && source.Metadata.ExpectedSize+source.Rootfs.ExpectedSize > s.Options.UploadMaxBytes {
				return errors.New("upload_too_large: declared image exceeds the configured limit")
			}
			if err := s.checkStagingCapacity(source.Metadata.ExpectedSize + source.Rootfs.ExpectedSize); err != nil {
				return err
			}
		}
	default:
		return errors.New("invalid_request: unsupported image source kind")
	}
	return nil
}

func validatePart(expectedSHA string, expectedSize, maximum int64) error {
	if !fingerprintPattern.MatchString(expectedSHA) {
		return errors.New("invalid_request: expected SHA-256 must be lowercase")
	}
	if expectedSize < 0 {
		return errors.New("invalid_request: expected size must not be negative")
	}
	if maximum > 0 && expectedSize > maximum {
		return errors.New("upload_too_large: declared image exceeds the configured limit")
	}
	return nil
}

func (s *Service) checkStagingCapacity(size int64) error {
	if s.Options.AvailableStagingBytes == nil || s.Options.UploadMinimumFreeBytes <= 0 {
		return nil
	}
	available, err := s.Options.AvailableStagingBytes()
	if err != nil {
		return errors.New("staging_capacity_low: staging capacity could not be verified")
	}
	if available < 0 || size > math.MaxInt64-s.Options.UploadMinimumFreeBytes || available < size+s.Options.UploadMinimumFreeBytes {
		return errors.New("staging_capacity_low: staging reserve is insufficient")
	}
	return nil
}

func (s *Service) UploadImageImport(ctx context.Context, operationID string, metadata, rootfs []byte) (*state.ManagementOperation, error) {
	operation, err := s.Store.GetManagementOperation(ctx, operationID)
	if err != nil {
		return nil, err
	}
	if operation == nil {
		return nil, errors.New("not_found: import operation does not exist")
	}
	if operation.Action != "image_import" {
		return nil, errors.New("invalid_request: operation is not an image import")
	}
	if operation.Status != "awaiting_upload" {
		if operation.Status == "running" || operation.Status == "succeeded" || operation.Status == "failed" || operation.Status == "outcome_unknown" {
			return operation, nil
		}
		return nil, errors.New("operation_conflict: image upload has already been received")
	}
	request, err := importRequestFromOperation(operation)
	if err != nil {
		return nil, err
	}
	if request.Source.Kind != "local_upload" {
		return nil, errors.New("invalid_request: trusted imports do not accept upload content")
	}
	if request.Source.Format == "unified" {
		if len(rootfs) != 0 || int64(len(metadata)) != request.Source.ExpectedSize {
			return s.failImport(ctx, operation, "upload", "invalid_request", "uploaded bytes do not match the declared size", false)
		}
		if digestBytes(metadata) != request.Source.ExpectedSHA256 {
			return s.failImport(ctx, operation, "integrity_check", "fingerprint_mismatch", "uploaded bytes do not match the declared digest", false)
		}
	} else {
		if int64(len(metadata)) != request.Source.Metadata.ExpectedSize || int64(len(rootfs)) != request.Source.Rootfs.ExpectedSize {
			return s.failImport(ctx, operation, "upload", "invalid_request", "uploaded parts do not match the declared sizes", false)
		}
		if digestBytes(metadata) != request.Source.Metadata.ExpectedSHA256 || digestBytes(rootfs) != request.Source.Rootfs.ExpectedSHA256 {
			return s.failImport(ctx, operation, "integrity_check", "fingerprint_mismatch", "uploaded parts do not match the declared digests", false)
		}
	}
	if s.Options.Backend == nil {
		return s.failImport(ctx, operation, "image_import", "internal_error", "no image backend is configured", true)
	}
	running, claimed, err := s.Store.TransitionManagementOperation(ctx, operationID, "awaiting_upload", "image_import", "running")
	if err != nil {
		return nil, err
	}
	if !claimed {
		return running, nil
	}
	operation = running
	artifact, err := s.Options.Backend.ImportLocal(ctx, request.Source.Format, metadata, rootfs)
	if err != nil {
		status := "failed"
		if isUncertain(err) {
			status = "outcome_unknown"
		}
		return s.finishOperation(ctx, operation, "reconciliation", status, nil, safeError(err))
	}
	return s.finishImport(ctx, operation, request, artifact)
}

func (s *Service) ProcessTrustedImport(ctx context.Context, operationID string) (*state.ManagementOperation, error) {
	operation, err := s.Store.GetManagementOperation(ctx, operationID)
	if err != nil {
		return nil, err
	}
	if operation == nil {
		return nil, errors.New("not_found: import operation does not exist")
	}
	if operation.Action != "image_import" {
		return nil, errors.New("invalid_request: operation is not an image import")
	}
	if operation.Status != "queued" {
		if operation.Status == "running" || operation.Status == "succeeded" || operation.Status == "failed" || operation.Status == "outcome_unknown" {
			return operation, nil
		}
		return nil, errors.New("operation_conflict: import is not queued")
	}
	request, err := importRequestFromOperation(operation)
	if err != nil {
		return nil, err
	}
	if s.Options.Backend == nil {
		return s.failImport(ctx, operation, "image_import", "internal_error", "no image backend is configured", true)
	}
	running, claimed, err := s.Store.TransitionManagementOperation(ctx, operationID, "queued", "image_import", "running")
	if err != nil {
		return nil, err
	}
	if !claimed {
		return running, nil
	}
	operation = running
	artifact, err := s.Options.Backend.ImportTrusted(ctx, request.Source.SourceID, request.Source.ExpectedFingerprint)
	if err != nil {
		status := "failed"
		if isUncertain(err) {
			status = "outcome_unknown"
		}
		return s.finishOperation(ctx, operation, "reconciliation", status, nil, safeError(err))
	}
	return s.finishImport(ctx, operation, request, artifact)
}

func (s *Service) finishImport(ctx context.Context, operation *state.ManagementOperation, request ImportRequest, artifact ImageArtifact) (*state.ManagementOperation, error) {
	findings := []string{}
	if artifact.Fingerprint != request.Source.ExpectedFingerprint && request.Source.Kind == "trusted_remote" {
		findings = append(findings, "fingerprint_mismatch")
	}
	if request.Source.Kind == "local_upload" && !fingerprintPattern.MatchString(artifact.Fingerprint) {
		findings = append(findings, "fingerprint_mismatch")
	}
	if artifact.InstanceType != "container" {
		findings = append(findings, "unsupported_instance_type")
	}
	if !s.supportedArchitecture(artifact.Architecture) {
		findings = append(findings, "unsupported_architecture")
	}
	if !artifact.Runtime {
		findings = append(findings, "missing_runtime")
	}
	if !artifact.SyncTool {
		findings = append(findings, "missing_sync_tool")
	}
	if !artifact.GuestReady {
		findings = append(findings, "guest_not_ready")
	}
	integrity := "passed"
	suitability := "passed"
	for _, finding := range findings {
		if finding == "fingerprint_mismatch" {
			integrity = "failed"
		} else {
			suitability = "failed"
		}
	}
	if len(findings) > 0 {
		return s.failImportWithImage(ctx, operation, request, artifact, integrity, suitability, findings)
	}
	image := state.ManagedImage{
		Fingerprint: artifact.Fingerprint, VersionLabel: stringPointer(request.VersionLabel), Architecture: artifact.Architecture,
		InstanceType: artifact.InstanceType, SourceKind: request.Source.Kind, SourceID: stringPointerIfNonEmpty(request.Source.SourceID),
		State: "available", Integrity: integrity, Suitability: suitability, Findings: findings, DefaultCompatibility: "none",
	}
	if err := s.Store.InsertManagedImage(ctx, image); err != nil {
		return s.failImport(ctx, operation, "image_import", "internal_error", err.Error(), true)
	}
	return s.finishOperation(ctx, operation, "completed", "succeeded", map[string]any{"fingerprint": artifact.Fingerprint, "state": "available"}, nil)
}

func (s *Service) failImportWithImage(ctx context.Context, operation *state.ManagementOperation, request ImportRequest, artifact ImageArtifact, integrity, suitability string, findings []string) (*state.ManagementOperation, error) {
	if fingerprintPattern.MatchString(artifact.Fingerprint) {
		_ = s.Store.InsertManagedImage(ctx, state.ManagedImage{Fingerprint: artifact.Fingerprint, VersionLabel: stringPointer(request.VersionLabel), Architecture: artifact.Architecture, InstanceType: artifact.InstanceType, SourceKind: request.Source.Kind, SourceID: stringPointerIfNonEmpty(request.Source.SourceID), State: "failed", Integrity: integrity, Suitability: suitability, Findings: findings, DefaultCompatibility: "none"})
	}
	return s.finishOperation(ctx, operation, "completed", "failed", nil, map[string]any{"code": "image_not_applicable", "phase": "suitability_check", "findings": findings})
}

func (s *Service) failImport(ctx context.Context, operation *state.ManagementOperation, phase, code, message string, retryable bool) (*state.ManagementOperation, error) {
	_ = message
	return s.finishOperation(ctx, operation, phase, "failed", nil, map[string]any{"code": safeCode(code), "retryable": retryable})
}

func (s *Service) finishOperation(ctx context.Context, operation *state.ManagementOperation, phase, status string, result, failure map[string]any) (*state.ManagementOperation, error) {
	now := s.Clock().UTC().Format(time.RFC3339Nano)
	if err := s.Store.UpdateManagementOperation(ctx, operation.OperationID, func(value *state.ManagementOperation) error {
		value.Phase, value.Status, value.Result, value.Error, value.UpdatedAt = phase, status, result, failure, now
		return nil
	}); err != nil {
		return nil, err
	}
	updated, err := s.Store.GetManagementOperation(ctx, operation.OperationID)
	if err != nil {
		return nil, err
	}
	outcome := status
	if outcome != "succeeded" && outcome != "outcome_unknown" {
		outcome = "failed"
	}
	if auditErr := s.audit(ctx, *updated, outcome, phase, failure); auditErr != nil {
		return nil, auditErr
	}
	return updated, nil
}

func (s *Service) SelectDefault(ctx context.Context, operationID, actor, reason, fingerprint, targetVersion string) (*state.ManagementOperation, error) {
	return s.runImageMutation(ctx, operationID, actor, reason, "default_image_select", fingerprint, targetVersion, func() (map[string]any, error) {
		version, err := s.Store.SelectManagedImage(ctx, fingerprint, targetVersion)
		if err != nil {
			return nil, err
		}
		return map[string]any{"fingerprint": fingerprint, "selection_version": version}, nil
	})
}

func (s *Service) RetireImage(ctx context.Context, operationID, actor, reason, fingerprint, targetVersion string) (*state.ManagementOperation, error) {
	return s.runImageMutation(ctx, operationID, actor, reason, "image_retire", fingerprint, targetVersion, func() (map[string]any, error) {
		unlock := s.Store.LockImageSelection()
		defer unlock()
		if err := s.requireImageVersion(ctx, fingerprint, targetVersion); err != nil {
			return nil, err
		}
		if err := s.Store.RetireManagedImage(ctx, fingerprint); err != nil {
			return nil, err
		}
		return map[string]any{"fingerprint": fingerprint, "state": "retired"}, nil
	})
}

func (s *Service) DeleteImage(ctx context.Context, operationID, actor, reason, fingerprint, targetVersion string) (*state.ManagementOperation, error) {
	return s.runImageMutation(ctx, operationID, actor, reason, "image_delete", fingerprint, targetVersion, func() (map[string]any, error) {
		unlock := s.Store.LockImageSelection()
		defer unlock()
		if err := s.requireImageVersion(ctx, fingerprint, targetVersion); err != nil {
			return nil, err
		}
		image, err := s.Store.GetManagedImage(ctx, fingerprint)
		if err != nil {
			return nil, err
		}
		if image == nil {
			return nil, errors.New("not_found: managed image does not exist")
		}
		if image.IsDefault {
			return nil, errors.New("default_image_required: select another default before deleting this image")
		}
		if image.References.Total() > 0 {
			return nil, fmt.Errorf("image_referenced: image has %d references", image.References.Total())
		}
		if s.Options.Backend != nil {
			if err := s.Options.Backend.DeleteImage(ctx, fingerprint); err != nil {
				return nil, err
			}
		}
		if err := s.Store.DeleteManagedImage(ctx, fingerprint); err != nil {
			return nil, nativeStateUncertainError{cause: err}
		}
		return map[string]any{"fingerprint": fingerprint, "state": "deleted"}, nil
	})
}

type nativeStateUncertainError struct {
	cause error
}

func (err nativeStateUncertainError) Error() string {
	return "native image deletion completed but catalog state is uncertain"
}

func (err nativeStateUncertainError) Unwrap() error   { return err.cause }
func (err nativeStateUncertainError) Uncertain() bool { return true }

func (s *Service) requireImageVersion(ctx context.Context, fingerprint, expected string) error {
	if expected == "" {
		return errors.New("invalid_request: target_version is required")
	}
	image, err := s.Store.GetManagedImage(ctx, fingerprint)
	if err != nil {
		return err
	}
	if image == nil {
		return errors.New("not_found: managed image does not exist")
	}
	current := "image:" + image.Fingerprint + ":revision-" + strconv.FormatInt(image.Revision, 10)
	if expected != current {
		return fmt.Errorf("stale_target: expected %s, current %s", expected, current)
	}
	return nil
}

func (s *Service) runImageMutation(ctx context.Context, operationID, actor, reason, action, targetID, targetVersion string, mutate func() (map[string]any, error)) (*state.ManagementOperation, error) {
	if operationID == "" || actor == "" || strings.TrimSpace(reason) == "" {
		return nil, errors.New("invalid_request: operation, actor and reason are required")
	}
	req := map[string]any{"action": action, "target_id": targetID, "target_version": targetVersion, "actor": actor, "reason": reason}
	digest, _ := protocol.Digest(protocol.Request(req))
	operation := state.ManagementOperation{OperationID: operationID, Action: action, TargetKind: "image", TargetID: targetID, Actor: actor, Reason: reason, Phase: "reference_check", Status: "queued", Request: map[string]any{"_request_digest": digest, "request": req}}
	existing, err := s.Store.ClaimManagementOperation(ctx, operation)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		if stringValue(existing.Request["_request_digest"]) != digest {
			return nil, errors.New("operation_conflict: operation ID belongs to a different request")
		}
		return existing, nil
	}
	if err := s.audit(ctx, operation, "accepted", operation.Phase, nil); err != nil {
		return nil, err
	}
	result, mutateErr := mutate()
	if mutateErr != nil {
		status := "failed"
		phase := "reference_check"
		if isUncertain(mutateErr) {
			status, phase = "outcome_unknown", "reconciliation"
		}
		return s.finishOperation(ctx, &operation, phase, status, nil, safeError(mutateErr))
	}
	return s.finishOperation(ctx, &operation, "completed", "succeeded", result, nil)
}

func (s *Service) ExecuteVMAction(ctx context.Context, request VMActionRequest) (*state.ManagementOperation, error) {
	actionRequest := map[string]any{"action": request.Action, "task_id": request.TaskID, "target_version": request.TargetVersion, "generation": request.ExpectedGeneration, "parameters": request.Parameters, "actor": request.Actor, "reason": request.Reason, "target_kind": "vm", "target_id": request.TaskID}
	digest, digestErr := protocol.Digest(protocol.Request(actionRequest))
	if digestErr != nil {
		return nil, errors.New("invalid_request: action request cannot be canonicalized")
	}
	if existing, err := s.existingOperation(ctx, request.OperationID, digest); err != nil || existing != nil {
		return existing, err
	}
	if s.Options.Actions == nil {
		return nil, errors.New("authority_unavailable: action backend is not configured")
	}
	task, err := s.Store.GetTask(ctx, map[string]any{"task_id": request.TaskID})
	if err != nil {
		return nil, err
	}
	if task == nil {
		return nil, errors.New("not_found: VM does not exist")
	}
	if task.Generation != request.ExpectedGeneration {
		return nil, errors.New("stale_target: VM generation changed")
	}
	currentVersion := "vm:" + task.TaskID + ":" + task.UpdatedAt
	if request.TargetVersion != currentVersion {
		return nil, errors.New("stale_target: VM resource version changed")
	}
	if !contains([]string{"restart", "resize", "release", "destroy", "port_add", "port_remove"}, request.Action) {
		return nil, errors.New("unsupported_action: VM action is not allowed")
	}
	operation, err := s.claimAction(ctx, request.OperationID, request.Actor, request.Reason, request.Action, "vm", request.TaskID, actionRequest)
	if err != nil || operation != nil && operation.Status != "queued" {
		return operation, err
	}
	if err := s.Store.UpdateManagementOperation(ctx, request.OperationID, func(value *state.ManagementOperation) error {
		value.Phase, value.Status, value.UpdatedAt = "native_action", "running", s.Clock().UTC().Format(time.RFC3339Nano)
		return nil
	}); err != nil {
		return nil, err
	}
	result, actionErr := s.Options.Actions.ExecuteVMAction(ctx, request.OperationID, request.Action, request.TaskID, request.ExpectedGeneration, request.Parameters)
	if actionErr != nil {
		status := "failed"
		if isUncertain(actionErr) {
			status = "outcome_unknown"
		}
		return s.finishOperation(ctx, operation, "reconciliation", status, nil, safeError(actionErr))
	}
	return s.finishOperation(ctx, operation, "completed", "succeeded", result, nil)
}

func (s *Service) ExecuteMaintenance(ctx context.Context, request MaintenanceRequest) (*state.ManagementOperation, error) {
	actionRequest := map[string]any{"action": request.Action, "target_version": request.TargetVersion, "parameters": request.Parameters, "actor": request.Actor, "reason": request.Reason, "target_kind": "system", "target_id": "system"}
	digest, digestErr := protocol.Digest(protocol.Request(actionRequest))
	if digestErr != nil {
		return nil, errors.New("invalid_request: maintenance request cannot be canonicalized")
	}
	if existing, err := s.existingOperation(ctx, request.OperationID, digest); err != nil || existing != nil {
		return existing, err
	}
	if s.Options.Actions == nil {
		return nil, errors.New("authority_unavailable: action backend is not configured")
	}
	if request.TargetVersion == "" {
		return nil, errors.New("invalid_request: target_version is required")
	}
	if !contains([]string{"drain_enter", "drain_exit", "reconcile", "gc"}, request.Action) {
		return nil, errors.New("unsupported_action: maintenance action is not allowed")
	}
	operation, err := s.claimAction(ctx, request.OperationID, request.Actor, request.Reason, request.Action, "system", "system", actionRequest)
	if err != nil || operation != nil && operation.Status != "queued" {
		return operation, err
	}
	if err := s.Store.UpdateManagementOperation(ctx, request.OperationID, func(value *state.ManagementOperation) error {
		value.Phase, value.Status, value.UpdatedAt = "native_action", "running", s.Clock().UTC().Format(time.RFC3339Nano)
		return nil
	}); err != nil {
		return nil, err
	}
	result, actionErr := s.Options.Actions.ExecuteMaintenance(ctx, request.OperationID, request.Action, request.Parameters)
	if actionErr != nil {
		status := "failed"
		if isUncertain(actionErr) {
			status = "outcome_unknown"
		}
		return s.finishOperation(ctx, operation, "reconciliation", status, nil, safeError(actionErr))
	}
	return s.finishOperation(ctx, operation, "completed", "succeeded", result, nil)
}

func (s *Service) claimAction(ctx context.Context, operationID, actor, reason, action, targetKind, targetID string, request map[string]any) (*state.ManagementOperation, error) {
	if operationID == "" || actor == "" || strings.TrimSpace(reason) == "" {
		return nil, errors.New("invalid_request: operation, actor and reason are required")
	}
	request["actor"], request["reason"] = actor, reason
	request["action"], request["target_kind"], request["target_id"] = action, targetKind, targetID
	digest, _ := protocol.Digest(protocol.Request(request))
	operation := state.ManagementOperation{OperationID: operationID, Action: action, TargetKind: targetKind, TargetID: targetID, Actor: actor, Reason: reason, Phase: "native_action", Status: "queued", Request: map[string]any{"_request_digest": digest, "request": request}}
	existing, err := s.Store.ClaimManagementOperation(ctx, operation)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		if stringValue(existing.Request["_request_digest"]) != digest {
			return nil, errors.New("operation_conflict: operation ID belongs to a different request")
		}
		return existing, nil
	}
	if err := s.audit(ctx, operation, "accepted", operation.Phase, nil); err != nil {
		return nil, err
	}
	return &operation, nil
}

func (s *Service) audit(ctx context.Context, operation state.ManagementOperation, outcome, phase string, detail map[string]any) error {
	return s.Store.RecordManagementAudit(ctx, state.ManagementAudit{EventID: auditEventID(operation.OperationID, phase, outcome), OperationID: stringPointer(operation.OperationID), Action: operation.Action, Actor: operation.Actor, Reason: operation.Reason, Target: map[string]any{"kind": operation.TargetKind, "id": operation.TargetID}, Outcome: outcome, Phase: phase, Detail: detail, OccurredAt: s.Clock().UTC().Format(time.RFC3339Nano)})
}

func auditEventID(operationID, phase, outcome string) string {
	digest := sha256.Sum256([]byte(operationID + "\x00" + phase + "\x00" + outcome))
	digest[6] = (digest[6] & 0x0f) | 0x50
	digest[8] = (digest[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", digest[0:4], digest[4:6], digest[6:8], digest[8:10], digest[10:16])
}

func (s *Service) trustedSource(sourceID string) bool {
	for _, allowed := range s.Options.TrustedImageSourceIDs {
		if allowed == sourceID {
			return true
		}
	}
	return false
}

func (s *Service) supportedArchitecture(architecture string) bool {
	if len(s.Options.SupportedArchitectures) == 0 {
		return architecture == "x86_64" || architecture == "amd64" || architecture == "aarch64" || architecture == "arm64"
	}
	for _, allowed := range s.Options.SupportedArchitectures {
		if architecture == allowed {
			return true
		}
	}
	return false
}

func importRequestMap(request ImportRequest) map[string]any {
	source := map[string]any{"kind": request.Source.Kind, "format": request.Source.Format}
	if request.Source.SourceID != "" {
		source["source_id"] = request.Source.SourceID
	}
	if request.Source.ExpectedFingerprint != "" {
		source["expected_fingerprint"] = request.Source.ExpectedFingerprint
	}
	if request.Source.Format == "unified" {
		source["expected_sha256"] = request.Source.ExpectedSHA256
		source["expected_size_bytes"] = request.Source.ExpectedSize
	} else if request.Source.Format == "split" {
		source["metadata"] = map[string]any{"expected_sha256": request.Source.Metadata.ExpectedSHA256, "expected_size_bytes": request.Source.Metadata.ExpectedSize}
		source["rootfs"] = map[string]any{"expected_sha256": request.Source.Rootfs.ExpectedSHA256, "expected_size_bytes": request.Source.Rootfs.ExpectedSize}
	}
	return map[string]any{"operation_id": request.OperationID, "actor": request.Actor, "reason": request.Reason, "target_version": request.TargetVersion, "version_label": request.VersionLabel, "source": source}
}

func importRequestFromOperation(operation *state.ManagementOperation) (ImportRequest, error) {
	requestValue, ok := operation.Request["request"].(map[string]any)
	if !ok {
		return ImportRequest{}, errors.New("invalid_state: import operation request is missing")
	}
	request := ImportRequest{OperationID: stringValue(requestValue["operation_id"]), Actor: stringValue(requestValue["actor"]), Reason: stringValue(requestValue["reason"]), TargetVersion: stringValue(requestValue["target_version"]), VersionLabel: stringValue(requestValue["version_label"])}
	sourceValue, ok := requestValue["source"].(map[string]any)
	if !ok {
		return ImportRequest{}, errors.New("invalid_state: import source is missing")
	}
	request.Source = ImportSource{Kind: stringValue(sourceValue["kind"]), Format: stringValue(sourceValue["format"]), SourceID: stringValue(sourceValue["source_id"]), ExpectedFingerprint: stringValue(sourceValue["expected_fingerprint"]), ExpectedSHA256: stringValue(sourceValue["expected_sha256"]), ExpectedSize: intValue(sourceValue["expected_size_bytes"])}
	if metadata, ok := sourceValue["metadata"].(map[string]any); ok {
		request.Source.Metadata = ImportPart{ExpectedSHA256: stringValue(metadata["expected_sha256"]), ExpectedSize: intValue(metadata["expected_size_bytes"])}
	}
	if rootfs, ok := sourceValue["rootfs"].(map[string]any); ok {
		request.Source.Rootfs = ImportPart{ExpectedSHA256: stringValue(rootfs["expected_sha256"]), ExpectedSize: intValue(rootfs["expected_size_bytes"])}
	}
	return request, nil
}

func digestBytes(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func safeError(err error) map[string]any {
	if isUncertain(err) {
		return map[string]any{"code": "internal_error", "retryable": true}
	}
	var referenceErr *state.ImageReferenceError
	if errors.As(err, &referenceErr) {
		code := "image_referenced"
		if referenceErr.Default {
			code = "default_image_required"
		}
		return map[string]any{"code": code, "references": referenceErr.Counts}
	}
	message := err.Error()
	if index := strings.IndexByte(message, ':'); index > 0 {
		return map[string]any{"code": safeCode(message[:index])}
	}
	return map[string]any{"code": "internal_error"}
}

func safeCode(code string) string {
	if contains([]string{"invalid_request", "source_not_allowed", "stale_target", "unsupported_action", "not_found", "image_not_applicable", "fingerprint_mismatch", "upload_too_large", "staging_capacity_low", "default_image_required", "image_referenced", "authority_unavailable", "operation_conflict", "internal_error"}, code) {
		return code
	}
	return "internal_error"
}

type uncertainError interface{ Uncertain() bool }

func isUncertain(err error) bool {
	var value uncertainError
	return errors.As(err, &value) && value.Uncertain()
}

func stringPointer(value string) *string { return &value }

func stringPointerIfNonEmpty(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func stringValue(value any) string {
	parsed, _ := value.(string)
	return parsed
}

func intValue(value any) int64 {
	if parsed, ok := protocol.Integer(value); ok {
		return parsed
	}
	return 0
}

func contains(values []string, candidate string) bool {
	for _, value := range values {
		if value == candidate {
			return true
		}
	}
	return false
}
