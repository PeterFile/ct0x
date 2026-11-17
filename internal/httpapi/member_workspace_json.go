package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"

	"agentworkspace/internal/store"
)

func decodeMemberWorkspaceJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	return decodeMemberToolJSON(w, r, &memberWorkspaceJSON{dst: dst})
}

type memberWorkspaceJSON struct{ dst any }

func (input *memberWorkspaceJSON) UnmarshalJSON(body []byte) error {
	// Validate before memberToolField or encoding/json can replace surrogates.
	if !validMemberWorkspaceJSONStrings(body) {
		return store.ErrInvalid
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	return decoder.Decode(input.dst)
}

// Check only raw string escapes; the existing decoder owns JSON syntax, UTF-8,
// duplicate fields and body limits. Never apply this to opaque MCP metadata.
func validMemberWorkspaceJSONStrings(body []byte) bool {
	if len(body) > memberToolJSONBytes {
		return false
	}
	inString := false
	for i := 0; i < len(body); i++ {
		switch body[i] {
		case '"':
			inString = !inString
		case '\\':
			if !inString {
				continue
			}
			i++
			if i >= len(body) {
				return false
			}
			if body[i] != 'u' {
				continue
			}
			unit, ok := memberWorkspaceJSONCodeUnit(body[i+1:])
			if !ok {
				return false
			}
			i += 4
			switch {
			case unit >= 0xd800 && unit <= 0xdbff:
				if i+6 >= len(body) || body[i+1] != '\\' || body[i+2] != 'u' {
					return false
				}
				low, ok := memberWorkspaceJSONCodeUnit(body[i+3:])
				if !ok || low < 0xdc00 || low > 0xdfff {
					return false
				}
				i += 6
			case unit >= 0xdc00 && unit <= 0xdfff:
				return false
			}
		}
	}
	return !inString
}

func memberWorkspaceJSONCodeUnit(body []byte) (uint16, bool) {
	if len(body) < 4 {
		return 0, false
	}
	var unit uint16
	for _, char := range body[:4] {
		var digit byte
		switch {
		case char >= '0' && char <= '9':
			digit = char - '0'
		case char >= 'a' && char <= 'f':
			digit = char - 'a' + 10
		case char >= 'A' && char <= 'F':
			digit = char - 'A' + 10
		default:
			return 0, false
		}
		unit = unit<<4 | uint16(digit)
	}
	return unit, true
}
