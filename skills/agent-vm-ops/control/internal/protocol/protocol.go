// Package protocol contains the transport-independent Agent VM request contract.
package protocol

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"
)

const Version = 4

var (
	SessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9_.:@/-]{1,160}$`)
	TaskIDPattern    = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,47}$`)
	PortNamePattern  = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)
)

var Actions = map[string]struct{}{
	"acquire": {}, "admin-gc": {}, "admin-metrics": {}, "admin-reconcile": {},
	"admin-session-list": {}, "admin-status": {}, "destroy": {}, "doctor": {},
	"drain-enter": {}, "drain-exit": {}, "drain-status": {}, "port-add": {},
	"port-list": {}, "port-remove": {}, "release": {}, "renew": {}, "resize": {},
	"restart": {}, "ssh-info": {}, "status": {}, "timer-age-gc": {},
	"timer-expire": {}, "timer-reconcile": {},
}

// Error is the stable structured error sent over the control API.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Detail  any    `json:"detail,omitempty"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func NewError(code, message string, detail any) *Error {
	return &Error{Code: code, Message: message, Detail: detail}
}

func AsError(err error) *Error {
	var control *Error
	if errors.As(err, &control) {
		return control
	}
	return NewError("internal_error", err.Error(), nil)
}

type Request map[string]any

type Schema struct {
	Schema           int64
	SemanticRevision int64
}

type CompatibilityWindow struct {
	SchemaMin   int64
	SchemaMax   int64
	SemanticMin int64
	SemanticMax int64
}

type ActionCompatibility struct {
	Request        Schema
	Response       Schema
	RequestWindow  CompatibilityWindow
	ResponseWindow CompatibilityWindow
}

// ReleaseMetadata is the validated, immutable compatibility view of the
// installed control release. It deliberately contains no filesystem paths.
type ReleaseMetadata struct {
	Version           string
	ReleaseID         string
	SourceRevision    string
	Protocol          int64
	Transport         string
	ManifestPresent   bool
	ArtifactsVerified bool
	Actions           map[string]ActionCompatibility
}

func Decode(data []byte) (Request, error) {
	value, err := DecodeValue(data)
	if err != nil {
		return nil, err
	}
	request, ok := value.(map[string]any)
	if !ok {
		return nil, NewError("invalid_request", "request must be an object", nil)
	}
	return Request(request), nil
}

func DecodeValue(data []byte) (any, error) {
	parser := jsonParser{data: data}
	value, err := parser.value()
	if err != nil {
		return nil, NewError("invalid_request", "request is not valid JSON", map[string]any{"cause": err.Error()})
	}
	parser.space()
	if parser.position != len(data) {
		return nil, NewError("invalid_request", "request contains trailing JSON", nil)
	}
	return value, nil
}

// jsonParser keeps lone UTF-16 surrogate code units in a private invalid UTF-8
// representation. The legacy protocol hashes the escaped code unit, while
// encoding/json replaces it with U+FFFD before a digest can be calculated.
type jsonParser struct {
	data     []byte
	position int
}

func (parser *jsonParser) space() {
	for parser.position < len(parser.data) {
		switch parser.data[parser.position] {
		case ' ', '\t', '\r', '\n':
			parser.position++
		default:
			return
		}
	}
}

func (parser *jsonParser) value() (any, error) {
	parser.space()
	if parser.position >= len(parser.data) {
		return nil, errors.New("missing JSON value")
	}
	switch parser.data[parser.position] {
	case '"':
		return parser.string()
	case '{':
		return parser.object()
	case '[':
		return parser.array()
	case 't':
		return parser.literal("true", true)
	case 'f':
		return parser.literal("false", false)
	case 'n':
		return parser.literal("null", nil)
	case 'N':
		return parser.numberLiteral("NaN")
	case 'I':
		return parser.numberLiteral("Infinity")
	case '-':
		if bytes.HasPrefix(parser.data[parser.position:], []byte("-Infinity")) {
			return parser.numberLiteral("-Infinity")
		}
		return parser.number()
	default:
		return parser.number()
	}
}

func (parser *jsonParser) literal(raw string, value any) (any, error) {
	if !bytes.HasPrefix(parser.data[parser.position:], []byte(raw)) {
		return nil, fmt.Errorf("invalid literal at byte %d", parser.position)
	}
	parser.position += len(raw)
	return value, nil
}

func (parser *jsonParser) object() (map[string]any, error) {
	parser.position++
	result := make(map[string]any)
	parser.space()
	if parser.consume('}') {
		return result, nil
	}
	for {
		parser.space()
		if parser.position >= len(parser.data) || parser.data[parser.position] != '"' {
			return nil, fmt.Errorf("object key must be a string at byte %d", parser.position)
		}
		key, err := parser.string()
		if err != nil {
			return nil, err
		}
		parser.space()
		if !parser.consume(':') {
			return nil, fmt.Errorf("object key is missing a colon at byte %d", parser.position)
		}
		value, err := parser.value()
		if err != nil {
			return nil, err
		}
		result[key] = value
		parser.space()
		if parser.consume('}') {
			return result, nil
		}
		if !parser.consume(',') {
			return nil, fmt.Errorf("object is missing a separator at byte %d", parser.position)
		}
	}
}

func (parser *jsonParser) array() ([]any, error) {
	parser.position++
	result := make([]any, 0)
	parser.space()
	if parser.consume(']') {
		return result, nil
	}
	for {
		value, err := parser.value()
		if err != nil {
			return nil, err
		}
		result = append(result, value)
		parser.space()
		if parser.consume(']') {
			return result, nil
		}
		if !parser.consume(',') {
			return nil, fmt.Errorf("array is missing a separator at byte %d", parser.position)
		}
	}
}

func (parser *jsonParser) consume(want byte) bool {
	if parser.position < len(parser.data) && parser.data[parser.position] == want {
		parser.position++
		return true
	}
	return false
}

func (parser *jsonParser) number() (json.Number, error) {
	start := parser.position
	if parser.consume('-') && parser.position >= len(parser.data) {
		return "", errors.New("number is missing digits")
	}
	if parser.position < len(parser.data) && parser.data[parser.position] == '0' {
		parser.position++
	} else {
		if parser.position >= len(parser.data) || parser.data[parser.position] < '1' || parser.data[parser.position] > '9' {
			return "", fmt.Errorf("invalid number at byte %d", start)
		}
		for parser.position < len(parser.data) && parser.data[parser.position] >= '0' && parser.data[parser.position] <= '9' {
			parser.position++
		}
	}
	if parser.consume('.') {
		fractionStart := parser.position
		for parser.position < len(parser.data) && parser.data[parser.position] >= '0' && parser.data[parser.position] <= '9' {
			parser.position++
		}
		if parser.position == fractionStart {
			return "", fmt.Errorf("number fraction is missing digits at byte %d", parser.position)
		}
	}
	if parser.position < len(parser.data) && (parser.data[parser.position] == 'e' || parser.data[parser.position] == 'E') {
		parser.position++
		if parser.position < len(parser.data) && (parser.data[parser.position] == '+' || parser.data[parser.position] == '-') {
			parser.position++
		}
		exponentStart := parser.position
		for parser.position < len(parser.data) && parser.data[parser.position] >= '0' && parser.data[parser.position] <= '9' {
			parser.position++
		}
		if parser.position == exponentStart {
			return "", fmt.Errorf("number exponent is missing digits at byte %d", parser.position)
		}
	}
	return json.Number(parser.data[start:parser.position]), nil
}

func (parser *jsonParser) numberLiteral(raw string) (json.Number, error) {
	if !bytes.HasPrefix(parser.data[parser.position:], []byte(raw)) {
		return "", fmt.Errorf("invalid number at byte %d", parser.position)
	}
	parser.position += len(raw)
	return json.Number(raw), nil
}

func (parser *jsonParser) string() (string, error) {
	if !parser.consume('"') {
		return "", fmt.Errorf("string must start with a quote at byte %d", parser.position)
	}
	var result strings.Builder
	for parser.position < len(parser.data) {
		value := parser.data[parser.position]
		parser.position++
		switch value {
		case '"':
			return result.String(), nil
		case '\\':
			if parser.position >= len(parser.data) {
				return "", errors.New("string escape is incomplete")
			}
			escape := parser.data[parser.position]
			parser.position++
			switch escape {
			case '"', '\\', '/':
				result.WriteByte(escape)
			case 'b':
				result.WriteByte('\b')
			case 'f':
				result.WriteByte('\f')
			case 'n':
				result.WriteByte('\n')
			case 'r':
				result.WriteByte('\r')
			case 't':
				result.WriteByte('\t')
			case 'u':
				code, err := parser.unicodeUnit()
				if err != nil {
					return "", err
				}
				if code >= 0xd800 && code <= 0xdbff && parser.hasUnicodeEscape() {
					low, lowErr := parser.peekUnicodeUnit()
					if lowErr == nil && low >= 0xdc00 && low <= 0xdfff {
						parser.position += 6
						result.WriteRune(rune(0x10000 + (uint32(code)-0xd800)*0x400 + (uint32(low) - 0xdc00)))
						continue
					}
				}
				writeUTF16CodeUnit(&result, code)
			default:
				return "", fmt.Errorf("invalid string escape at byte %d", parser.position-1)
			}
		case 0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10, 0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18, 0x19, 0x1a, 0x1b, 0x1c, 0x1d, 0x1e, 0x1f:
			return "", fmt.Errorf("unescaped control character at byte %d", parser.position-1)
		default:
			parser.position--
			runeValue, size := utf8.DecodeRune(parser.data[parser.position:])
			if runeValue == utf8.RuneError && size == 1 {
				return "", fmt.Errorf("invalid UTF-8 in string at byte %d", parser.position)
			}
			result.Write(parser.data[parser.position : parser.position+size])
			parser.position += size
		}
	}
	return "", errors.New("unterminated string")
}

func (parser *jsonParser) unicodeUnit() (uint16, error) {
	if parser.position+4 > len(parser.data) {
		return 0, errors.New("unicode escape is incomplete")
	}
	var value uint16
	for index := 0; index < 4; index++ {
		digit, ok := hexDigit(parser.data[parser.position+index])
		if !ok {
			return 0, fmt.Errorf("invalid unicode escape at byte %d", parser.position+index)
		}
		value = value<<4 | uint16(digit)
	}
	parser.position += 4
	return value, nil
}

func (parser *jsonParser) hasUnicodeEscape() bool {
	return parser.position+6 <= len(parser.data) && parser.data[parser.position] == '\\' && parser.data[parser.position+1] == 'u'
}

func (parser *jsonParser) peekUnicodeUnit() (uint16, error) {
	if !parser.hasUnicodeEscape() {
		return 0, errors.New("unicode escape is absent")
	}
	position := parser.position
	parser.position += 2
	value, err := parser.unicodeUnit()
	parser.position = position
	return value, err
}

func hexDigit(value byte) (byte, bool) {
	switch {
	case value >= '0' && value <= '9':
		return value - '0', true
	case value >= 'a' && value <= 'f':
		return value - 'a' + 10, true
	case value >= 'A' && value <= 'F':
		return value - 'A' + 10, true
	default:
		return 0, false
	}
}

func writeUTF16CodeUnit(out *strings.Builder, value uint16) {
	if value >= 0xd800 && value <= 0xdfff {
		out.WriteByte(0xe0 | byte(value>>12))
		out.WriteByte(0x80 | byte((value>>6)&0x3f))
		out.WriteByte(0x80 | byte(value&0x3f))
		return
	}
	out.WriteRune(rune(value))
}

func Encode(value any) ([]byte, error) {
	return json.Marshal(value)
}

func ValidateRequest(request Request) *Error {
	protocol, ok := integer(request["protocol"])
	if !ok || protocol != Version {
		return NewError("protocol_mismatch", fmt.Sprintf("protocol must be %d", Version), nil)
	}
	action, ok := request["action"].(string)
	if !ok || action == "" {
		return NewError("invalid_request", "action must be a non-empty string", nil)
	}
	if _, ok := Actions[action]; !ok {
		return NewError("unknown_action", "unknown action: "+action, nil)
	}
	if session, exists := request["session_id"]; exists {
		if value, ok := session.(string); !ok || !SessionIDPattern.MatchString(value) {
			return NewError("invalid_request", "session_id contains unsupported characters", nil)
		}
	}
	if operation, exists := request["operation_id"]; exists {
		if value, ok := operation.(string); !ok || !SessionIDPattern.MatchString(value) {
			return NewError("invalid_request", "operation_id contains unsupported characters", nil)
		}
	}
	if _, exists := request["generation"]; exists {
		if value, ok := integer(request["generation"]); !ok || value < 0 {
			return NewError("invalid_request", "generation must be a non-negative integer", nil)
		}
	}
	return nil
}

func ValidateCompatibility(request Request, release *ReleaseMetadata) ([]any, *Error) {
	if release == nil {
		return nil, nil
	}
	action, _ := request["action"].(string)
	local, ok := release.Actions[action]
	if !ok {
		return nil, NewError("compatibility_invalid", "action is not declared in the control release", map[string]any{"action": action})
	}
	raw, present := request["compatibility"]
	if !present || raw == nil {
		return []any{compatibilityWarning("legacy_compatibility", "peer omitted release capability; using legacy protocol-4 compatibility", action)}, nil
	}
	peer, ok := raw.(map[string]any)
	if !ok {
		return nil, NewError("compatibility_invalid", "compatibility must be an object", map[string]any{"action": action})
	}
	peerProtocol, ok := integer(peer["protocol"])
	if !ok || peerProtocol != release.Protocol {
		return nil, NewError("protocol_mismatch", "peer protocol is not supported", map[string]any{"expected": release.Protocol, "actual": peer["protocol"]})
	}
	if peerAction, ok := peer["action"].(string); !ok || peerAction != action {
		return nil, NewError("compatibility_action_mismatch", "peer capability action does not match request action", map[string]any{"action": action})
	}
	peerRequest, control := decodeSchema(peer["request"], "compatibility.request")
	if control != nil {
		return nil, control
	}
	acceptedResponse, control := decodeWindow(peer["accepted_response"], "compatibility.accepted_response")
	if control != nil {
		return nil, control
	}
	if !within(peerRequest, local.RequestWindow) {
		if peerRequest.Schema < local.RequestWindow.SchemaMin || peerRequest.Schema > local.RequestWindow.SchemaMax {
			return nil, NewError("request_schema_incompatible", "request schema is outside the control compatibility window", map[string]any{"action": action})
		}
		return nil, NewError("request_semantic_incompatible", "request semantic revision is outside the control compatibility window", map[string]any{"action": action})
	}
	if !within(local.Response, acceptedResponse) {
		if local.Response.Schema < acceptedResponse.SchemaMin || local.Response.Schema > acceptedResponse.SchemaMax {
			return nil, NewError("response_schema_incompatible", "control response schema is not accepted by the client", map[string]any{"action": action})
		}
		return nil, NewError("response_semantic_incompatible", "control response semantic revision is not accepted by the client", map[string]any{"action": action})
	}
	releaseID, ok := peer["release_id"].(string)
	if !ok || releaseID == "" || (releaseID != "legacy-protocol-4" && !revisionPattern.MatchString(releaseID)) {
		return nil, NewError("compatibility_invalid", "compatibility.release_id must be a Git revision or legacy-protocol-4", map[string]any{"action": action})
	}
	if legacy, exists := peer["legacy"]; exists {
		if _, ok := legacy.(bool); !ok {
			return nil, NewError("compatibility_invalid", "compatibility.legacy must be a boolean", map[string]any{"action": action})
		}
	}
	if releaseID == release.ReleaseID {
		return nil, nil
	}
	return []any{map[string]any{
		"code":    "compatible_release",
		"message": "peer release differs but the requested operation is explicitly compatible",
		"action":  action,
		"detail":  map[string]any{"peer_release": releaseID, "control_release": release.ReleaseID},
	}}, nil
}

var revisionPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

func compatibilityWarning(code, message, action string) map[string]any {
	return map[string]any{"code": code, "message": message, "action": action}
}

func decodeSchema(value any, label string) (Schema, *Error) {
	object, ok := value.(map[string]any)
	if !ok || len(object) != 2 {
		return Schema{}, NewError("compatibility_invalid", label+" must declare schema and semantic_revision", nil)
	}
	schema, schemaOK := integer(object["schema"])
	semantic, semanticOK := integer(object["semantic_revision"])
	if !schemaOK || !semanticOK || schema < 1 || semantic < 1 {
		return Schema{}, NewError("compatibility_invalid", label+" is invalid", nil)
	}
	return Schema{Schema: schema, SemanticRevision: semantic}, nil
}

func decodeWindow(value any, label string) (CompatibilityWindow, *Error) {
	object, ok := value.(map[string]any)
	if !ok || len(object) != 4 {
		return CompatibilityWindow{}, NewError("compatibility_invalid", label+" must declare schema and semantic min/max", nil)
	}
	values := make(map[string]int64, 4)
	for _, key := range []string{"schema_min", "schema_max", "semantic_min", "semantic_max"} {
		parsed, valid := integer(object[key])
		if !valid || parsed < 1 {
			return CompatibilityWindow{}, NewError("compatibility_invalid", label+" is invalid", nil)
		}
		values[key] = parsed
	}
	if values["schema_min"] > values["schema_max"] || values["semantic_min"] > values["semantic_max"] {
		return CompatibilityWindow{}, NewError("compatibility_invalid", label+" has inverted bounds", nil)
	}
	return CompatibilityWindow{SchemaMin: values["schema_min"], SchemaMax: values["schema_max"], SemanticMin: values["semantic_min"], SemanticMax: values["semantic_max"]}, nil
}

func within(value Schema, window CompatibilityWindow) bool {
	return value.Schema >= window.SchemaMin && value.Schema <= window.SchemaMax && value.SemanticRevision >= window.SemanticMin && value.SemanticRevision <= window.SemanticMax
}

func integer(value any) (int64, bool) {
	switch value := value.(type) {
	case json.Number:
		if strings.ContainsAny(string(value), ".eE") {
			return 0, false
		}
		parsed, err := strconv.ParseInt(string(value), 10, 64)
		return parsed, err == nil
	case int:
		return int64(value), true
	case int64:
		return value, true
	case uint64:
		if value > math.MaxInt64 {
			return 0, false
		}
		return int64(value), true
	default:
		return 0, false
	}
}

func Integer(value any) (int64, bool) { return integer(value) }

// CanonicalJSON matches the existing protocol's key ordering, spaces, exact
// integer representation and distinction between integer and fractional JSON.
func CanonicalJSON(value any) (string, error) {
	var out strings.Builder
	if err := writeCanonical(&out, value); err != nil {
		return "", err
	}
	return out.String(), nil
}

func Digest(value any) (string, error) {
	canonical, err := CanonicalJSON(value)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(hash[:]), nil
}

func writeCanonical(out *strings.Builder, value any) error {
	switch value := value.(type) {
	case Request:
		return writeCanonical(out, map[string]any(value))
	case nil:
		out.WriteString("null")
	case bool:
		if value {
			out.WriteString("true")
		} else {
			out.WriteString("false")
		}
	case string:
		out.WriteString(quote(value))
	case json.Number:
		out.WriteString(canonicalNumber(string(value)))
	case float64:
		out.WriteString(canonicalFloat(value))
	case float32:
		out.WriteString(canonicalFloat(float64(value)))
	case int:
		out.WriteString(strconv.FormatInt(int64(value), 10))
	case int64:
		out.WriteString(strconv.FormatInt(value, 10))
	case uint64:
		out.WriteString(strconv.FormatUint(value, 10))
	case []any:
		out.WriteByte('[')
		for index, item := range value {
			if index > 0 {
				out.WriteString(", ")
			}
			if err := writeCanonical(out, item); err != nil {
				return err
			}
		}
		out.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(value))
		for key := range value {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		out.WriteByte('{')
		for index, key := range keys {
			if index > 0 {
				out.WriteString(", ")
			}
			out.WriteString(quote(key))
			out.WriteString(": ")
			if err := writeCanonical(out, value[key]); err != nil {
				return err
			}
		}
		out.WriteByte('}')
	default:
		return fmt.Errorf("unsupported canonical JSON value %T", value)
	}
	return nil
}

func canonicalNumber(raw string) string {
	if !strings.ContainsAny(raw, ".eE") {
		if parsed, err := strconv.ParseInt(raw, 10, 64); err == nil {
			return strconv.FormatInt(parsed, 10)
		}
		// JSON integers are already canonical with respect to leading zeroes.
		return raw
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return raw
	}
	return canonicalFloat(value)
}

func canonicalFloat(value float64) string {
	if math.IsNaN(value) {
		return "NaN"
	}
	if math.IsInf(value, 1) {
		return "Infinity"
	}
	if math.IsInf(value, -1) {
		return "-Infinity"
	}
	if value == 0 {
		if math.Signbit(value) {
			return "-0.0"
		}
		return "0.0"
	}
	negative := value < 0
	if negative {
		value = -value
	}
	raw := strconv.FormatFloat(value, 'g', -1, 64)
	parts := strings.SplitN(strings.ToLower(raw), "e", 2)
	mantissa := parts[0]
	exponent := 0
	if len(parts) == 2 {
		exponent, _ = strconv.Atoi(parts[1])
	}
	point := strings.IndexByte(mantissa, '.')
	all := strings.ReplaceAll(mantissa, ".", "")
	leading := strings.IndexFunc(all, func(r rune) bool { return r != '0' })
	if leading < 0 {
		return "0.0"
	}
	digits := strings.TrimRight(all[leading:], "0")
	exponent += func() int {
		if point < 0 {
			return len(mantissa)
		}
		return point
	}() - leading - 1
	var result string
	if exponent < -4 || exponent >= 16 {
		fraction := ""
		if len(digits) > 1 {
			fraction = "." + digits[1:]
		}
		result = digits[:1] + fraction + "e" + signAndTwoDigits(exponent)
	} else {
		offset := exponent + 1
		switch {
		case offset <= 0:
			result = "0." + strings.Repeat("0", -offset) + digits
		case offset >= len(digits):
			result = digits + strings.Repeat("0", offset-len(digits)) + ".0"
		default:
			result = digits[:offset] + "." + digits[offset:]
		}
	}
	if negative {
		return "-" + result
	}
	return result
}

func signAndTwoDigits(value int) string {
	if value < 0 {
		return "-" + fmt.Sprintf("%02d", -value)
	}
	return "+" + fmt.Sprintf("%02d", value)
}

func quote(value string) string {
	var out strings.Builder
	out.WriteByte('"')
	for index := 0; index < len(value); {
		if code, ok := encodedSurrogate(value[index:]); ok {
			fmt.Fprintf(&out, `\u%04x`, code)
			index += 3
			continue
		}
		r, size := utf8.DecodeRuneInString(value[index:])
		if r == utf8.RuneError && size == 1 && value[index] >= 0x80 {
			out.WriteString(`\ufffd`)
			index++
			continue
		}
		switch r {
		case '"':
			out.WriteString(`\"`)
		case '\\':
			out.WriteString(`\\`)
		case '\b':
			out.WriteString(`\b`)
		case '\f':
			out.WriteString(`\f`)
		case '\n':
			out.WriteString(`\n`)
		case '\r':
			out.WriteString(`\r`)
		case '\t':
			out.WriteString(`\t`)
		default:
			if r < 0x20 || r >= 0x7f {
				for _, unit := range utf16.Encode([]rune{r}) {
					out.WriteString(fmt.Sprintf(`\u%04x`, unit))
				}
			} else {
				out.WriteRune(r)
			}
		}
		index += size
	}
	out.WriteByte('"')
	return out.String()
}

func encodedSurrogate(value string) (uint16, bool) {
	if len(value) < 3 || value[0] != 0xed || value[1] < 0xa0 || value[1] > 0xbf || value[2] < 0x80 || value[2] > 0xbf {
		return 0, false
	}
	code := uint16(value[0]&0x0f)<<12 | uint16(value[1]&0x3f)<<6 | uint16(value[2]&0x3f)
	return code, code >= 0xd800 && code <= 0xdfff
}

func Success(request Request, result any) map[string]any {
	return SuccessWithRelease(request, result, nil, nil)
}

func SuccessWithRelease(request Request, result any, release *ReleaseMetadata, warnings []any) map[string]any {
	if warnings == nil {
		warnings = []any{}
	}
	response := map[string]any{
		"ok": true, "protocol": Version, "action": request["action"],
		"session_id": request["session_id"], "generation": request["generation"],
		"operation_id": request["operation_id"], "outcome": "success", "server_time": time.Now().UTC().Format(time.RFC3339Nano), "result": result,
		"response": map[string]any{"schema": 1, "semantic_revision": 1},
		"warnings": warnings,
	}
	if release != nil {
		response["release"] = ReleaseEnvelope(release)
	}
	return response
}

func ReleaseEnvelope(release *ReleaseMetadata) map[string]any {
	if release == nil {
		return nil
	}
	result := map[string]any{
		"release_id":         release.ReleaseID,
		"source_revision":    release.SourceRevision,
		"protocol":           release.Protocol,
		"transport":          release.Transport,
		"manifest_present":   release.ManifestPresent,
		"artifacts_verified": release.ArtifactsVerified,
	}
	if release.Version != "" {
		result["version"] = release.Version
	}
	return result
}

func ErrorEnvelope(request Request, err error) map[string]any {
	control := AsError(err)
	result := map[string]any{"ok": false, "protocol": Version, "error": control}
	for _, key := range []string{"action", "session_id", "generation", "operation_id"} {
		if value, exists := request[key]; exists {
			result[key] = value
		}
	}
	return result
}
