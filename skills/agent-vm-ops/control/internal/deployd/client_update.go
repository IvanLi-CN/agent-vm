package deployd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
)

type UpdateReceipt struct {
	ID      string
	Action  string
	Payload []byte
}

// RunBlueGreen advances only on a confirmed response for the same transaction.
// A failed response may follow a committed effect; callers must retain the bundle.
func (client Client) RunBlueGreen(ctx context.Context, targetSlot, bundle string) (UpdateReceipt, error) {
	return runBlueGreen(ctx, client.Call, targetSlot, bundle)
}

func runBlueGreen(ctx context.Context, call func(context.Context, string, string, any) ([]byte, int, error), targetSlot, bundle string) (UpdateReceipt, error) {
	result, err := prepareBlueGreen(ctx, call, targetSlot, bundle)
	if err != nil {
		return result, err
	}
	return activateBlueGreen(ctx, call, result)
}

// PrepareBlueGreen stages and verifies the candidate, without changing the
// active route. Activation must use the returned transaction ID.
func (client Client) PrepareBlueGreen(ctx context.Context, targetSlot, bundle string) (UpdateReceipt, error) {
	return prepareBlueGreen(ctx, client.Call, targetSlot, bundle)
}

func prepareBlueGreen(ctx context.Context, call updateCall, targetSlot, bundle string) (UpdateReceipt, error) {
	if validSlot(targetSlot) != nil || !filepath.IsAbs(bundle) {
		return UpdateReceipt{}, errors.New("blue-green update requires a fixed slot and an absolute bundle path")
	}
	result, err := blueGreenStep(ctx, call, "", "stage", PhaseStandbyValidation, "active", map[string]any{"target_slot": targetSlot, "bundle": filepath.Clean(bundle)})
	if err != nil {
		return result, err
	}
	return blueGreenStep(ctx, call, result.ID, "verify", PhaseControllerHandoff, "active", map[string]any{"transaction_id": result.ID})
}

// ResumeBlueGreenPreparation resolves a lost acknowledgement from the existing
// coordinator's status. It never stages a second transaction or activates.
func (client Client) ResumeBlueGreenPreparation(ctx context.Context, stagingID, previous, target, expectedID string) (UpdateReceipt, error) {
	return resumeBlueGreenPreparation(ctx, client.Call, stagingID, previous, target, expectedID)
}

func resumeBlueGreenPreparation(ctx context.Context, call updateCall, stagingID, previous, target, expectedID string) (UpdateReceipt, error) {
	if stagingID == "" || validSlot(previous) != nil || validSlot(target) != nil || previous == target {
		return UpdateReceipt{}, errors.New("blue-green recovery requires a staging ID and two distinct fixed slots")
	}
	payload, _, err := call(ctx, "GET", "/v1/status", nil)
	if err != nil {
		return UpdateReceipt{}, err
	}
	var status struct {
		OK    bool `json:"ok"`
		Value struct {
			Observed     RuntimeSnapshot `json:"observation"`
			Transactions []struct {
				updateAcknowledgement
				StagingID        string `json:"staging_id"`
				Previous         string `json:"previous_slot"`
				Target           string `json:"target_slot"`
				RecoveryRequired bool   `json:"recovery_required"`
			} `json:"transactions"`
		} `json:"value"`
	}
	if json.Unmarshal(payload, &status) != nil || !status.OK || status.Value.Observed.ActiveSlot != previous ||
		len(status.Value.Transactions) != 1 {
		return UpdateReceipt{}, errors.New("original deployment transaction could not be identified")
	}
	tx := status.Value.Transactions[0]
	if tx.ID == "" || (expectedID != "" && tx.ID != expectedID) || tx.StagingID != stagingID ||
		tx.Previous != previous || tx.Target != target {
		return UpdateReceipt{}, errors.New("active deployment transaction does not match the reserved candidate")
	}
	receipt := UpdateReceipt{ID: tx.ID, Action: "recover", Payload: payload}
	if tx.RecoveryRequired || tx.State != "active" {
		return receipt, errors.New("original deployment transaction requires explicit recovery")
	}
	switch tx.Phase {
	case PhaseStandbyValidation:
		return blueGreenStep(ctx, call, tx.ID, "verify", PhaseControllerHandoff, "active", map[string]any{"transaction_id": tx.ID})
	case PhaseControllerHandoff:
		receipt.Action = "verify"
		return receipt, nil
	default:
		return receipt, errors.New("original deployment transaction is not at a preparation phase")
	}
}

func activateBlueGreen(ctx context.Context, call updateCall, prepared UpdateReceipt) (UpdateReceipt, error) {
	if prepared.ID == "" || prepared.Action != "verify" {
		return prepared, errors.New("blue-green activation requires a verified candidate")
	}
	return blueGreenStep(ctx, call, prepared.ID, "activate", PhaseFinalize, "finalized", map[string]any{"transaction_id": prepared.ID})
}

type updateCall func(context.Context, string, string, any) ([]byte, int, error)

func blueGreenStep(ctx context.Context, call updateCall, id, action, phase, state string, input any) (UpdateReceipt, error) {
	payload, _, err := call(ctx, "POST", "/v1/"+action, input)
	result := UpdateReceipt{ID: id, Action: action, Payload: payload}
	ack, ok := decodeUpdateAcknowledgement(payload)
	if action == "stage" && id == "" {
		result.ID = ack.ID
	}
	if err != nil {
		return result, err
	}
	if !ok || result.ID == "" || !ack.OK || ack.ID != result.ID || ack.Phase != phase || ack.State != state {
		return result, fmt.Errorf("deployd %s acknowledgement is incomplete", action)
	}
	return result, nil
}

type updateAcknowledgement struct {
	OK    bool
	ID    string `json:"transaction_id"`
	Phase string
	State string
}

func decodeUpdateAcknowledgement(payload []byte) (updateAcknowledgement, bool) {
	var response struct {
		updateAcknowledgement
		Value updateAcknowledgement `json:"value"`
	}
	if json.Unmarshal(payload, &response) != nil {
		return updateAcknowledgement{}, false
	}
	if response.Value.ID == "" {
		return response.updateAcknowledgement, true
	}
	response.Value.OK = response.OK
	return response.Value, true
}
