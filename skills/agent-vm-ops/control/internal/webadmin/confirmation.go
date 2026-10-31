package webadmin

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/protocol"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/state"
)

const confirmationTTL = 5 * time.Minute

func issueDurableConfirmation(ctx context.Context, store *state.Store, now time.Time, subject, action, target, targetVersion string, params map[string]any) (string, time.Time, error) {
	if store == nil {
		return "", time.Time{}, errors.New("state_unavailable: confirmation state is unavailable")
	}
	digest, err := protocol.Digest(protocol.Request(params))
	if err != nil {
		return "", time.Time{}, err
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", time.Time{}, err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	expiresAt := now.UTC().Add(confirmationTTL)
	if err := store.InsertManagementConfirmation(ctx, state.ManagementConfirmation{
		ConfirmationDigest: digestString(token),
		Subject:            subject,
		Action:             action,
		Target:             target,
		TargetVersion:      targetVersion,
		ParamsDigest:       digest,
		CreatedAt:          now.UTC().Format(time.RFC3339Nano),
		ExpiresAt:          expiresAt.Format(time.RFC3339Nano),
	}); err != nil {
		return "", time.Time{}, fmt.Errorf("state_unavailable: %w", err)
	}
	return token, expiresAt, nil
}

func durableConfirmationClaim(token, subject, action, target, targetVersion string, params map[string]any) (state.ManagementConfirmationClaim, error) {
	if token == "" {
		return state.ManagementConfirmationClaim{}, errors.New("precondition_required: confirmation token is required")
	}
	digest, err := protocol.Digest(protocol.Request(params))
	if err != nil {
		return state.ManagementConfirmationClaim{}, errors.New("invalid_request: confirmation parameters cannot be canonicalized")
	}
	return state.ManagementConfirmationClaim{ConfirmationDigest: digestString(token), Subject: subject, Action: action, Target: target, TargetVersion: targetVersion, ParamsDigest: digest}, nil
}

func consumeDurableConfirmation(ctx context.Context, store *state.Store, now time.Time, token, subject, action, target, targetVersion string, params map[string]any) error {
	if store == nil {
		return errors.New("state_unavailable: confirmation state is unavailable")
	}
	claim, err := durableConfirmationClaim(token, subject, action, target, targetVersion, params)
	if err != nil {
		return err
	}
	consumed, err := store.ConsumeManagementConfirmation(ctx, claim.ConfirmationDigest, claim.Subject, claim.Action, claim.Target, claim.TargetVersion, claim.ParamsDigest, now.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("state_unavailable: %w", err)
	}
	if !consumed {
		return errors.New("precondition_required: confirmation token is invalid or expired")
	}
	return nil
}
