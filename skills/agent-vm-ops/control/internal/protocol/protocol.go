// Package protocol contains the transport-independent Agent VM request contract.
package protocol

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
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
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, NewError("invalid_request", "request is not valid JSON", map[string]any{"cause": err.Error()})
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, NewError("invalid_request", "request contains trailing JSON", nil)
	}
	return value, nil
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
	if _, exists := request["generation"]; exists {
		if value, ok := integer(request["generation"]); !ok || value < 0 {
			return NewError("invalid_request", "generation must be a non-negative integer", nil)
		}
	}
	return nil
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
	for _, r := range value {
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
	}
	out.WriteByte('"')
	return out.String()
}

func Success(request Request, result any) map[string]any {
	return map[string]any{
		"ok": true, "protocol": Version, "action": request["action"],
		"session_id": request["session_id"], "generation": request["generation"],
		"operation_id": request["operation_id"], "outcome": "success", "result": result,
		"response": map[string]any{"schema": 1, "semantic_revision": 1},
		"warnings": []any{},
	}
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
