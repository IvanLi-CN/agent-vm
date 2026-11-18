package install

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"time"

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
	var receipt deployd.UpdateReceipt
	if err != nil {
		if !errors.Is(err, os.ErrExist) {
			return receipt, err
		}
		if err := verifyReservedAccountBundle(filepath.Join(staging, stagingID), report.BundleSHA256); err != nil {
			return receipt, err
		}
		receipt, err = client.ResumeBlueGreenPreparation(ctx, stagingID, report.ActiveSlot, target, "")
	} else {
		if err := prepareLegacyCandidateUnit(ctx, report.ActiveSlot, input, product, report.SourceRevision); err != nil {
			return receipt, fmt.Errorf("prepare account candidate in %s: %w", stagingID, err)
		}
		receipt, err = client.PrepareBlueGreen(ctx, target, bundle)
	}
	if err != nil {
		return receipt, fmt.Errorf("inspect original deployment transaction for staging ID %s before retrying: %w", stagingID, err)
	}
	if err := deployd.VerifyRunningSlotAccount(ctx, target); err != nil {
		return receipt, err
	}
	payload, _, err := client.Call(ctx, "GET", "/v1/status", nil)
	if err != nil {
		return receipt, err
	}
	if err := checkPreparedAccountCandidate(payload, receipt.ID, stagingID, report, target, time.Now()); err != nil {
		return receipt, err
	}
	return receipt, nil
}

// activateAccountCandidate leaves the incumbent deploy daemon responsible for
// the existing transaction, including its ordinary activation compensation.
func activateAccountCandidate(ctx context.Context, client deployd.Client, prepared deployd.UpdateReceipt, report Report) (deployd.AccountHandoff, error) {
	var empty deployd.AccountHandoff
	target := "blue"
	if report.ActiveSlot == "blue" {
		target = "green"
	}
	stagingID := AccountMigrationStagingPrefix + report.BundleSHA256
	payload, _, err := client.Call(ctx, "GET", "/v1/status", nil)
	if err != nil {
		return empty, err
	}
	if err := checkPreparedAccountCandidate(payload, prepared.ID, stagingID, report, target, time.Now()); err != nil {
		return empty, err
	}
	if err := deployd.VerifyRunningSlotAccount(ctx, target); err != nil {
		return empty, err
	}
	if _, err := client.ActivateBlueGreen(ctx, prepared); err != nil {
		return empty, fmt.Errorf("activation of %s was not acknowledged; inspect the original deployment transaction before continuing: %w", prepared.ID, err)
	}
	journal, _, err := client.Call(ctx, "GET", "/v1/transactions/"+url.PathEscape(prepared.ID), nil)
	if err != nil {
		return empty, err
	}
	status, _, err := client.Call(ctx, "GET", "/v1/status", nil)
	if err != nil {
		return empty, err
	}
	return accountHandoffAfterActivation(journal, status, prepared.ID, stagingID, target, report, time.Now())
}

type accountMigrationTransaction struct {
	ID               string `json:"transaction_id"`
	StagingID        string `json:"staging_id"`
	Previous         string `json:"previous_slot"`
	Target           string `json:"target_slot"`
	State            string `json:"state"`
	Phase            string `json:"phase"`
	Generation       int64  `json:"generation"`
	RecoveryRequired bool   `json:"recovery_required"`
	RollbackUsed     bool   `json:"rollback_used"`
}

func (tx accountMigrationTransaction) matches(id, stagingID, previous, target string) bool {
	return id != "" && stagingID != "" && tx.ID == id && tx.StagingID == stagingID &&
		tx.Previous == previous && tx.Target == target && !tx.RecoveryRequired && !tx.RollbackUsed &&
		((previous == "blue" && target == "green") || (previous == "green" && target == "blue"))
}

type accountMigrationStatus struct {
	OK    bool `json:"ok"`
	Value struct {
		Observation  deployd.RuntimeSnapshot       `json:"observation"`
		Transactions []accountMigrationTransaction `json:"transactions"`
		Dedicated    bool                          `json:"dedicated_service_account"`
	} `json:"value"`
}

func accountHandoffAfterActivation(journal, payload []byte, id, stagingID, target string, report Report, now time.Time) (deployd.AccountHandoff, error) {
	var transaction struct {
		OK    bool `json:"ok"`
		Value struct {
			Transaction accountMigrationTransaction `json:"transaction"`
		} `json:"value"`
	}
	var status accountMigrationStatus
	if json.Unmarshal(journal, &transaction) != nil || !transaction.OK ||
		json.Unmarshal(payload, &status) != nil || !status.OK {
		return deployd.AccountHandoff{}, errors.New("activation journal or live observation is unavailable")
	}
	tx := transaction.Value.Transaction
	if !tx.matches(id, stagingID, report.ActiveSlot, target) || tx.Phase != deployd.PhaseFinalize ||
		tx.State != "finalized" || tx.Generation <= 0 || status.Value.Dedicated || len(status.Value.Transactions) != 0 {
		return deployd.AccountHandoff{}, errors.New("original activation is not finalized by the incumbent deployment service")
	}
	snapshot := status.Value.Observation
	active, old := snapshot.Slots[target], snapshot.Slots[report.ActiveSlot]
	lease := active.ControllerLease
	if !snapshot.Consistent || !snapshot.FreshUntil.After(now) || snapshot.ActiveSlot != target || snapshot.ControllerSlot != target ||
		!active.ProcessUp || !active.ProcessReady || !active.Ready || !active.ControllerOwned || !active.TrafficAccepting || active.Draining ||
		active.SourceRevision != report.SourceRevision || report.previousAuthority == "" || active.StateAuthority != report.previousAuthority ||
		lease == nil || lease.Validate(now, target) != nil || lease.Generation != tx.Generation || lease.OwnerPID <= 0 || lease.OwnerStartTime == "" ||
		!old.CountersKnown || old.ProcessUp || old.ProcessReady || old.Ready || old.ControllerOwned || old.TrafficAccepting ||
		old.InFlightRequests != 0 || old.NativeWorkers != 0 || old.PendingOperations != 0 {
		return deployd.AccountHandoff{}, errors.New("live slots do not confirm the finalized account migration")
	}
	return deployd.AccountHandoff{Lease: *lease, StateAuthority: active.StateAuthority}, nil
}

func checkPreparedAccountCandidate(data []byte, id, stagingID string, report Report, target string, now time.Time) error {
	var status accountMigrationStatus
	if err := json.Unmarshal(data, &status); err != nil || !status.OK || status.Value.Dedicated || len(status.Value.Transactions) != 1 {
		return errors.New("cannot verify the original prepared account migration transaction")
	}
	tx := status.Value.Transactions[0]
	if !tx.matches(id, stagingID, report.ActiveSlot, target) || tx.State != "active" || tx.Phase != deployd.PhaseControllerHandoff {
		return errors.New("prepared account migration transaction changed")
	}
	snapshot := status.Value.Observation
	active, standby := snapshot.Slots[report.ActiveSlot], snapshot.Slots[target]
	if !snapshot.Consistent || !snapshot.FreshUntil.After(now) || snapshot.ActiveSlot != report.ActiveSlot ||
		snapshot.ControllerSlot != report.ActiveSlot || !active.ProcessUp || !active.Ready || !active.ControllerOwned ||
		!active.TrafficAccepting || active.Draining || active.SourceRevision != report.PreviousSource ||
		active.ReleaseID != report.previousReleaseID || report.previousAuthority == "" || active.StateAuthority != report.previousAuthority ||
		!standby.ProcessUp || !standby.ProcessReady || standby.ControllerOwned || standby.TrafficAccepting ||
		standby.SourceRevision != report.SourceRevision || standby.StateAuthority != report.previousAuthority {
		return errors.New("prepared account migration no longer has a serving incumbent and a fenced candidate on the same state backend")
	}
	return nil
}

func verifyReservedAccountBundle(directory, expected string) error {
	file, err := materialFile(directory, "product.tar")
	if err != nil {
		return fmt.Errorf("read reserved migration bundle: %w", err)
	}
	defer file.Close()
	digest := sha256.New()
	const limit = 256 << 20
	size, err := io.Copy(digest, io.LimitReader(file, limit+1))
	if err != nil || size > limit || hex.EncodeToString(digest.Sum(nil)) != expected {
		return errors.New("reserved migration bundle failed digest verification")
	}
	return nil
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
