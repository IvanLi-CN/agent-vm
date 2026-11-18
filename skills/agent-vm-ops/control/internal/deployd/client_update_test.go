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
