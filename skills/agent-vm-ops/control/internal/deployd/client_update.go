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
	var result UpdateReceipt
	if validSlot(targetSlot) != nil || !filepath.IsAbs(bundle) {
		return result, errors.New("blue-green update requires a fixed slot and an absolute bundle path")
	}
	for _, step := range []struct{ action, phase, state string }{
		{"stage", PhaseStandbyValidation, "active"},
		{"verify", PhaseControllerHandoff, "active"},
		{"activate", PhaseFinalize, "finalized"},
	} {
		input := map[string]any{"transaction_id": result.ID}
		if step.action == "stage" {
			input = map[string]any{"target_slot": targetSlot, "bundle": filepath.Clean(bundle)}
		}
		result.Action = step.action
		payload, _, err := call(ctx, "POST", "/v1/"+step.action, input)
		result.Payload = payload
		if err != nil {
			return result, err
		}
		ack, ok := decodeUpdateAcknowledgement(payload)
		if step.action == "stage" {
			result.ID = ack.ID
		}
		if !ok || result.ID == "" || !ack.OK || ack.ID != result.ID || ack.Phase != step.phase || ack.State != step.state {
			return result, fmt.Errorf("deployd %s acknowledgement is incomplete", step.action)
		}
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
