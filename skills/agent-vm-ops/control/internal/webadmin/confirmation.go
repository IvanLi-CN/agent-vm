package webadmin

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/protocol"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/state"
)

const confirmationTTL = 5 * time.Minute

type confirmationStore struct {
	mu     sync.Mutex
	values map[string]confirmation
}

type confirmation struct {
	Subject      string
	Action       string
	Target       string
	TargetVer    string
	ParamsDigest string
	ExpiresAt    time.Time
}

func newConfirmationStore() *confirmationStore {
	return &confirmationStore{values: make(map[string]confirmation)}
}

func (store *confirmationStore) issue(now time.Time, subject, action, target, targetVersion string, params map[string]any) (string, time.Time, error) {
	if store == nil {
		return "", time.Time{}, errors.New("confirmation store is nil")
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
	store.mu.Lock()
	store.pruneLocked(now.UTC())
	store.values[token] = confirmation{Subject: subject, Action: action, Target: target, TargetVer: targetVersion, ParamsDigest: digest, ExpiresAt: expiresAt}
	store.mu.Unlock()
	return token, expiresAt, nil
}

func (store *confirmationStore) consume(now time.Time, token, subject, action, target, targetVersion string, params map[string]any) error {
	if store == nil || token == "" {
		return errors.New("precondition_required: confirmation token is required")
	}
	digest, err := protocol.Digest(protocol.Request(params))
	if err != nil {
		return errors.New("invalid_request: confirmation parameters cannot be canonicalized")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	store.pruneLocked(now.UTC())
	value, ok := store.values[token]
	if !ok || !value.ExpiresAt.After(now.UTC()) || value.Subject != subject || value.Action != action || value.Target != target || value.TargetVer != targetVersion || value.ParamsDigest != digest {
		return errors.New("precondition_required: confirmation token is invalid or expired")
	}
	delete(store.values, token)
	return nil
}

func (store *confirmationStore) pruneLocked(now time.Time) {
	for token, value := range store.values {
		if !value.ExpiresAt.After(now) {
			delete(store.values, token)
		}
	}
}

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
