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
