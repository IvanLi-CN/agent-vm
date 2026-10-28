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

func TestNativeOutcomeErrorsExposeUncertainty(t *testing.T) {
	if !NewError("restart_partial", "guest stop failed", map[string]any{"outcome_verified": false}).Uncertain() {
		t.Fatal("restart partial error must be uncertain")
	}
	if !NewError("cleanup_incomplete", "cleanup did not complete", nil).Uncertain() {
		t.Fatal("cleanup error must be uncertain")
	}
	if NewError("lease_expired", "lease expired", map[string]any{"outcome_verified": true}).Uncertain() {
		t.Fatal("verified lease error must not be uncertain")
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

func TestCanonicalJSONPreservesEscapedUnpairedSurrogates(t *testing.T) {
	value, err := DecodeValue([]byte(`{"pair":"\ud800\udc00","surrogate":"\ud800"}`))
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := CanonicalJSON(value)
	if err != nil {
		t.Fatal(err)
	}
	if canonical != `{"pair": "\ud800\udc00", "surrogate": "\ud800"}` {
		t.Fatalf("canonical = %s", canonical)
	}
}

func TestCanonicalJSONMatchesLegacyCorpus(t *testing.T) {
	cases := []struct {
		source    string
		digest    string
		canonical string
	}{
		{`{"z":1,"a":{"b":true,"a":null},"items":[3,2,1]}`, "4289537ed976b87787b53020ff034601bd8e529ca6144b32f92babff2d198f43", `{"a": {"a": null, "b": true}, "items": [3, 2, 1], "z": 1}`},
		{`{"generation":9007199254740993,"negative":-9223372036854775809}`, "352bee174a457e8cd06bc1ea993dc15c0d1739d48691c1f561ac46009d137dc6", `{"generation": 9007199254740993, "negative": -9223372036854775809}`},
		{`{"value":1}`, "e1d70a18cc129fcc812ebbe309bc5197df6ffa2228c77a4a7b98653ec5605354", `{"value": 1}`},
		{`{"value":1.0}`, "1f5a54566322afe41ae853c8344fb4fbd68226c1fe1cea2fbd08eeb6533fc5f9", `{"value": 1.0}`},
		{`{"value":1e0}`, "1f5a54566322afe41ae853c8344fb4fbd68226c1fe1cea2fbd08eeb6533fc5f9", `{"value": 1.0}`},
		{`{"value":-0.0}`, "f2d5cc6fe0d5157446bc8c339543a44d451dadb801a457d9f1ba07bab385a1a3", `{"value": -0.0}`},
		{`{"floats":[0.0001,0.00001,1e-7,1e15,1e16,1e23,1.2345678901234567,5e-324,1.7976931348623157e308]}`, "a95cab8f414b68e1264fd77515373e68c593945318b71d2351bc04d8589ba9d4", `{"floats": [0.0001, 1e-05, 1e-07, 1000000000000000.0, 1e+16, 1e+23, 1.2345678901234567, 5e-324, 1.7976931348623157e+308]}`},
		{`{"floats":[9007199254740993.0,NaN,Infinity,-Infinity]}`, "128c9469ddc871cc9ccc18b54d1608b433f9ba2307c854f2cb3b2f2872ba3b5e", `{"floats": [9007199254740992.0, NaN, Infinity, -Infinity]}`},
		{`{"\u5fc3\u7fbd":"\u732b \ud83d\udc08","\u00e9":"e\u0301","\ue000":1,"\ud800\udc00":2}`, "e85c97e1973d06033305438d42970bf3e0bb2b7915efc916c7982c8dbf01df37", `{"\u00e9": "e\u0301", "\u5fc3\u7fbd": "\u732b \ud83d\udc08", "\ue000": 1, "\ud800\udc00": 2}`},
		{`{"escape":"\b\t\n\f\r\"\\/\u007f","surrogate":"\ud800"}`, "3862bfb95011ad98358d3d4f4d9d5f5002da78cd41f5660df5c3738690b3aac3", `{"escape": "\b\t\n\f\r\"\\/\u007f", "surrogate": "\ud800"}`},
		{`{"__proto__":{"polluted":true},"constructor":"literal"}`, "760943ec7649b963a493e8e2a2b365ee66fbbfcf231fadfd4c1ba490d3da3da4", `{"__proto__": {"polluted": true}, "constructor": "literal"}`},
	}
	for _, testCase := range cases {
		value, err := DecodeValue([]byte(testCase.source))
		if err != nil {
			t.Fatalf("decode %s: %v", testCase.source, err)
		}
		canonical, err := CanonicalJSON(value)
		if err != nil {
			t.Fatal(err)
		}
		if canonical != testCase.canonical {
			t.Fatalf("canonical = %s, want %s", canonical, testCase.canonical)
		}
		digest, err := Digest(value)
		if err != nil {
			t.Fatal(err)
		}
		if digest != testCase.digest {
			t.Fatalf("digest = %s, want %s", digest, testCase.digest)
		}
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

func TestValidateRequestRejectsMalformedOperationID(t *testing.T) {
	request := Request{"protocol": Version, "action": "drain-enter", "generation": int64(0), "operation_id": ""}
	if control := ValidateRequest(request); control == nil || control.Code != "invalid_request" {
		t.Fatalf("malformed operation id control = %#v", control)
	}
}

func testRelease() *ReleaseMetadata {
	actions := make(map[string]ActionCompatibility, len(Actions))
	for action := range Actions {
		actions[action] = ActionCompatibility{
			Request:        Schema{Schema: 1, SemanticRevision: 1},
			Response:       Schema{Schema: 1, SemanticRevision: 1},
			RequestWindow:  CompatibilityWindow{SchemaMin: 1, SchemaMax: 1, SemanticMin: 1, SemanticMax: 1},
			ResponseWindow: CompatibilityWindow{SchemaMin: 1, SchemaMax: 1, SemanticMin: 1, SemanticMax: 1},
		}
	}
	return &ReleaseMetadata{ReleaseID: strings.Repeat("a", 40), SourceRevision: strings.Repeat("a", 40), Protocol: Version, Transport: "go-request", ManifestPresent: true, ArtifactsVerified: true, Actions: actions}
}

func TestValidateCompatibilityRejectsBeforeDispatchWindow(t *testing.T) {
	release := testRelease()
	request := Request{
		"protocol": Version, "action": "acquire",
		"compatibility": map[string]any{
			"release_id": strings.Repeat("b", 40), "protocol": Version, "action": "acquire",
			"request":           map[string]any{"schema": 2, "semantic_revision": 1},
			"accepted_response": map[string]any{"schema_min": 1, "schema_max": 1, "semantic_min": 1, "semantic_max": 1},
		},
	}
	warnings, control := ValidateCompatibility(request, release)
	if warnings != nil || control == nil || control.Code != "request_schema_incompatible" {
		t.Fatalf("warnings=%#v control=%#v", warnings, control)
	}
}

func TestValidateCompatibilityReturnsReleaseWarning(t *testing.T) {
	release := testRelease()
	request := Request{
		"protocol": Version, "action": "status",
		"compatibility": map[string]any{
			"release_id": strings.Repeat("b", 40), "protocol": Version, "action": "status",
			"request":           map[string]any{"schema": 1, "semantic_revision": 1},
			"accepted_response": map[string]any{"schema_min": 1, "schema_max": 1, "semantic_min": 1, "semantic_max": 1},
		},
	}
	warnings, control := ValidateCompatibility(request, release)
	if control != nil || len(warnings) != 1 || warnings[0].(map[string]any)["code"] != "compatible_release" {
		t.Fatalf("warnings=%#v control=%#v", warnings, control)
	}
}

func TestSuccessIncludesServerTimeForFormatTwoClients(t *testing.T) {
	response := Success(Request{"protocol": Version, "action": "status", "session_id": "s"}, map[string]any{})
	if value, ok := response["server_time"].(string); !ok || strings.TrimSpace(value) == "" {
		t.Fatalf("server_time = %#v", response["server_time"])
	}
}
