package install

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/deployd"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/native"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/releasefetch"
)

type accountMigrationProof struct {
	SchemaVersion   int    `json:"schema_version"`
	PreviousSource  string `json:"previous_source_revision"`
	PreviousRelease string `json:"previous_release_id"`
	CandidateSource string `json:"candidate_source_revision"`
	CandidateDigest string `json:"candidate_bundle_sha256"`
	MaterialsDigest string `json:"materials_manifest_sha256"`
}

func verifyAccountMigrationProof(data []byte, report Report) error {
	var proof accountMigrationProof
	if err := decodeStrict(data, &proof); err != nil || proof.SchemaVersion != 1 {
		return errors.New("invalid account migration acceptance record")
	}
	if report.PreviousSource == "" || report.PreviousReleaseID == "" || report.SourceRevision == "" ||
		report.BundleSHA256 == "" || report.MaterialsSHA256 == "" || proof.PreviousSource != report.PreviousSource ||
		proof.PreviousRelease != report.PreviousReleaseID || proof.CandidateSource != report.SourceRevision ||
		proof.CandidateDigest != report.BundleSHA256 || proof.MaterialsDigest != report.MaterialsSHA256 {
		return errors.New("account migration acceptance record does not match the installed release and candidate")
	}
	return nil
}

// ApplyAccountMigration accepts a local root-owned record from the isolated
// acceptance run. Ordinary installation and update never enter this path.
func ApplyAccountMigration(ctx context.Context, input Inputs, proofPath string) (Report, error) {
	if os.Geteuid() != 0 {
		return Report{}, errors.New("account migration requires local root")
	}
	proof, err := protectedRead(proofPath)
	if err != nil {
		return Report{}, err
	}
	if err := safeParents("/run/agent-vm-native"); err != nil {
		return Report{}, err
	}
	lock, err := native.AcquireLock("/run/agent-vm-native/release-update.lock", true, true)
	if err != nil {
		return Report{}, errors.New("another release update is already in progress")
	}
	defer lock.Close()
	input, cleanup, err := snapshotInputs(input)
	if err != nil {
		return Report{}, err
	}
	defer cleanup()
	report, err := PreflightAccountMigration(ctx, input)
	if err != nil {
		return report, err
	}
	if err := verifyAccountMigrationProof(proof, report); err != nil {
		return report, err
	}
	root := filepath.Dir(input.Bundle)
	bundle, err := releasefetch.Prepare(ctx, report.Version, report.SourceRevision, filepath.Join(root, "official"), input.Bundle, "")
	if err != nil {
		return report, err
	}
	digest, err := digestFile(bundle)
	if err != nil || digest != report.BundleSHA256 {
		return report, errors.New("verified release bundle no longer matches the account migration record")
	}
	input.Bundle = bundle
	product := filepath.Join(root, "product")
	file, err := os.Open(bundle)
	if err != nil {
		return report, err
	}
	err = errors.Join(Extract(file, product, 256<<20), file.Close())
	if err != nil {
		return report, err
	}
	client := deployd.Client{Socket: deployd.Defaults().Socket, Timeout: 15 * time.Minute}
	receipt, err := prepareAccountCandidate(ctx, input, product, report, client)
	report.TransactionID = receipt.ID
	if err != nil {
		return report, err
	}
	report.Next = "inspect the original account migration transaction before taking recovery action"
	handoff, err := activateAccountCandidate(ctx, client, receipt, report)
	if err != nil {
		return report, err
	}
	if err := handoffDeploymentUnit(ctx, handoff, input); err != nil {
		return report, err
	}
	report.ActiveSlot = handoff.Lease.Slot
	report.Next = "account migration complete; use agent-vm-deploy update for subsequent releases"
	return report, nil
}
