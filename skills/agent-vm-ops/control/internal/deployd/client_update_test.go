package deployd

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestBlueGreenRetainsOriginalTransactionAcrossCoordinatorEnvelopes(t *testing.T) {
	for _, verifyID := range []string{"tx-1", "unexpected"} {
		t.Run(verifyID, func(t *testing.T) {
			var calls []string
			result, err := runBlueGreen(t.Context(), func(_ context.Context, method, path string, raw any) ([]byte, int, error) {
				calls = append(calls, path)
				input := raw.(map[string]any)
				if method != "POST" || (path != "/v1/stage" && input["transaction_id"] != "tx-1") {
					t.Fatalf("unexpected request %s %s %v", method, path, input)
				}
				switch path {
				case "/v1/stage":
					return []byte(`{"ok":true,"transaction_id":"tx-1","phase":"standby_validation","state":"active"}`), 200, nil
				case "/v1/verify":
					return []byte(fmt.Sprintf(`{"ok":true,"value":{"transaction_id":%q,"phase":"controller_handoff","state":"active"}}`, verifyID)), 200, nil
				case "/v1/activate":
					return []byte(`{"ok":true,"value":{"transaction_id":"tx-1","phase":"finalize","state":"finalized"}}`), 200, nil
				default:
					t.Fatalf("unexpected path %s", path)
					return nil, 0, nil
				}
			}, "green", "/var/tmp/package.tar")
			if result.ID != "tx-1" {
				t.Fatalf("original ID lost: %+v", result)
			}
			want := "/v1/stage /v1/verify"
			if verifyID == "tx-1" {
				want += " /v1/activate"
			}
			if strings.Join(calls, " ") != want || (err == nil) != (verifyID == "tx-1") {
				t.Fatalf("calls=%v error=%v", calls, err)
			}
		})
	}
}

func TestBlueGreenPrepareDoesNotActivate(t *testing.T) {
	var calls []string
	result, err := prepareBlueGreen(t.Context(), func(_ context.Context, _, path string, _ any) ([]byte, int, error) {
		calls = append(calls, path)
		switch path {
		case "/v1/stage":
			return []byte(`{"ok":true,"transaction_id":"tx-1","phase":"standby_validation","state":"active"}`), 200, nil
		case "/v1/verify":
			return []byte(`{"ok":true,"transaction_id":"tx-1","phase":"controller_handoff","state":"active"}`), 200, nil
		default:
			t.Fatalf("unexpected request: %s", path)
			return nil, 0, nil
		}
	}, "green", "/protected/bundle.tar")
	if err != nil || result.ID != "tx-1" || result.Action != "verify" || strings.Join(calls, " ") != "/v1/stage /v1/verify" {
		t.Fatalf("receipt=%+v calls=%v err=%v", result, calls, err)
	}
}

func TestResumePreparationReusesOnlyMatchingOriginalTransaction(t *testing.T) {
	for _, tc := range []struct {
		name, staging, phase, expectedID  string
		needsRecovery, wantOK, wantVerify bool
	}{
		{"lost stage reply", ".update-account-fixture", PhaseStandbyValidation, "", false, true, true},
		{"lost verify reply", ".update-account-fixture", PhaseControllerHandoff, "tx-original", false, true, false},
		{"other staging", ".update-unrelated", PhaseStandbyValidation, "", false, false, false},
		{"other transaction", ".update-account-fixture", PhaseStandbyValidation, "tx-other", false, false, false},
		{"unknown effect", ".update-account-fixture", PhaseStage, "", false, false, false},
		{"needs recovery", ".update-account-fixture", PhaseStandbyValidation, "", true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var verified bool
			call := func(_ context.Context, method, path string, body any) ([]byte, int, error) {
				switch method + " " + path {
				case "GET /v1/status":
					return []byte(fmt.Sprintf(`{"ok":true,"value":{"observation":{"active_slot":"blue"},"transactions":[{"transaction_id":"tx-original","staging_id":%q,"phase":%q,"previous_slot":"blue","target_slot":"green","state":"active","recovery_required":%t}]}}`, tc.staging, tc.phase, tc.needsRecovery)), 200, nil
				case "POST /v1/verify":
					if verified || body.(map[string]any)["transaction_id"] != "tx-original" {
						t.Fatalf("unexpected verify: %v", body)
					}
					verified = true
					return []byte(`{"ok":true,"value":{"transaction_id":"tx-original","phase":"controller_handoff","state":"active"}}`), 200, nil
				default:
					t.Fatalf("unexpected request %s %s", method, path)
					return nil, 0, nil
				}
			}
			receipt, err := resumeBlueGreenPreparation(t.Context(), call, ".update-account-fixture", "blue", "green", tc.expectedID)
			if (err == nil) != tc.wantOK || verified != tc.wantVerify || tc.wantOK && receipt.ID != "tx-original" {
				t.Fatalf("receipt=%+v err=%v verified=%v", receipt, err, verified)
			}
		})
	}
}
