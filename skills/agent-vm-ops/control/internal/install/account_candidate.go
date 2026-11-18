package install

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/deployd"
)

const AccountMigrationStagingPrefix = ".update-account-"

// prepareAccountCandidate runs with the release-update lock held and a report
// from preflight on the private input snapshot. The old coordinator remains
// the sole deployment writer during candidate preparation.
func prepareAccountCandidate(ctx context.Context, input Inputs, product string, report Report, client deployd.Client) (deployd.UpdateReceipt, error) {
	target := "green"
	switch report.ActiveSlot {
	case "blue":
	case "green":
		target = "blue"
	default:
		return deployd.UpdateReceipt{}, errors.New("account migration requires an observed active slot")
	}
	staging := filepath.Join(deployd.Defaults().ReleaseRoot, "release-staging")
	bundle, stagingID, err := stageAccountBundle(staging, input.Bundle, report.BundleSHA256)
	if err != nil {
		return deployd.UpdateReceipt{}, err
	}
	if err := prepareLegacyCandidateUnit(ctx, report.ActiveSlot, input.Materials, product, report.SourceRevision); err != nil {
		return deployd.UpdateReceipt{}, fmt.Errorf("prepare account candidate in %s: %w", stagingID, err)
	}
	receipt, err := client.PrepareBlueGreen(ctx, target, bundle)
	if err != nil {
		return receipt, fmt.Errorf("inspect original deployment transaction for staging ID %s before retrying: %w", stagingID, err)
	}
	return receipt, nil
}

// Reserve the deterministic staging ID before asking the coordinator to do
// work. On re-entry the existing directory is evidence of an earlier attempt,
// even if that attempt never received the stage acknowledgement.
func stageAccountBundle(root, source, digest string) (path, stagingID string, err error) {
	decoded, err := hex.DecodeString(digest)
	if err != nil || len(decoded) != 32 || hex.EncodeToString(decoded) != digest {
		return "", "", errors.New("account migration requires a verified candidate digest")
	}
	stagingID = AccountMigrationStagingPrefix + digest
	directory := filepath.Join(root, stagingID)
	if err := safeParents(root); err != nil {
		return "", stagingID, err
	}
	if err := os.Mkdir(directory, 0700); err != nil {
		return "", stagingID, fmt.Errorf("cannot reserve staging ID %s; inspect deployment and staging before retrying: %w", stagingID, err)
	}
	path = filepath.Join(directory, "product.tar")
	if err := copyInstall(source, path, 0600, ""); err != nil {
		return "", stagingID, err
	}
	if copied, err := digestFile(path); err != nil || copied != digest {
		return "", stagingID, errors.New("protected migration bundle failed digest verification")
	}
	if err := syncDir(root); err != nil {
		return "", stagingID, err
	}
	return path, stagingID, nil
}
