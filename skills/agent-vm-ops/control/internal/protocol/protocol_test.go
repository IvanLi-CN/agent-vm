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