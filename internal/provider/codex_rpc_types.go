package provider

import "encoding/json"

// Experimental passive envelope, not a dispatcher or authority boundary.
// Raw fields preserve absence/null and IDs without float64; Error includes data.
// Other envelope metadata is ignored, including trace and jsonrpc on output.
type codexRPCMessage struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method *string         `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  json.RawMessage `json:"error,omitempty"`
}

func validCodexRPCID(raw json.RawMessage) bool {
	var text *string
	if json.Unmarshal(raw, &text) == nil && text != nil {
		return true
	}
	var number *int64
	return json.Unmarshal(raw, &number) == nil && number != nil
}

func validCodexRPCError(raw json.RawMessage) bool {
	fields, err := codexRPCObject(raw, "code", "message", "data")
	if err != nil {
		return false
	}
	var code *int64
	var message *string
	return json.Unmarshal(fields["code"], &code) == nil && code != nil &&
		json.Unmarshal(fields["message"], &message) == nil && message != nil
}
