package protocol

import (
	"strings"
	"testing"
)

func TestCanonicalBaselineDigests(t *testing.T) {
	cases := []struct {
		source string
		digest string
	}{
		{`{"protocol":4,"action":"drain-enter","session_id":"baseline","generation":0,"operation_id":"baseline-success"}`, "c2cd33cdb94c29a2e22cb8ecf23615def67804d0a273e67daa282654340ea36f"},
		{`{"protocol":4,"action":"acquire","session_id":"pending-baseline","generation":0,"operation_id":"baseline-pending","float":1.0,"unicode":"心羽𐀀","exact":9007199254740993}`, "50f980849447704388f0235b8b9173ed38cbabb55ca07d0bb56ec0598382e49f"},
		{`{"protocol":4,"action":"renew","session_id":"baseline","generation":9007199254740993,"operation_id":"baseline-error"}`, "2883f6bf3d395c3c73eede18cca83342779736a8e64c3a025fdbfa126f1207ac"},
	}
	for _, testCase := range cases {
		request, err := Decode([]byte(testCase.source))
		if err != nil {
			t.Fatal(err)
		}
		got, err := Digest(request)
		if err != nil {
			t.Fatal(err)
		}
		if got != testCase.digest {
			t.Fatalf("digest = %s, want %s", got, testCase.digest)
		}
	}
}

func TestCanonicalNumbersKeepIntegerAndFractionalIdentity(t *testing.T) {
	integerValue, err := DecodeValue([]byte(`{"value":1}`))
	if err != nil {
		t.Fatal(err)
	}
	fractionValue, err := DecodeValue([]byte(`{"value":1.0}`))
	if err != nil {
		t.Fatal(err)
	}
	exponentValue, err := DecodeValue([]byte(`{"value":1e0}`))
	if err != nil {
		t.Fatal(err)
	}
	integerDigest, _ := Digest(integerValue)
	fractionDigest, _ := Digest(fractionValue)
	exponentDigest, _ := Digest(exponentValue)
	if integerDigest == fractionDigest {
		t.Fatal("integer and fractional JSON must have different digests")
	}
	if fractionDigest != exponentDigest {
		t.Fatal("equivalent fractional JSON must have the same digest")
	}
	canonical, _ := CanonicalJSON(fractionValue)
	if canonical != `{"value": 1.0}` {
		t.Fatalf("canonical = %s", canonical)
	}
}

func TestValidateRequestRejectsFractionalGeneration(t *testing.T) {
	request, err := Decode([]byte(`{"protocol":4,"action":"status","session_id":"s","generation":1.0}`))
	if err != nil {
		t.Fatal(err)
	}
	if control := ValidateRequest(request); control == nil || control.Code != "invalid_request" {
		t.Fatalf("validation = %#v", control)
	}
}

func TestSuccessIncludesServerTimeForFormatTwoClients(t *testing.T) {
	response := Success(Request{"protocol": Version, "action": "status", "session_id": "s"}, map[string]any{})
	if value, ok := response["server_time"].(string); !ok || strings.TrimSpace(value) == "" {
		t.Fatalf("server_time = %#v", response["server_time"])
	}
}
