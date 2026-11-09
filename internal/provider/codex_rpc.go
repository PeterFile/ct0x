package provider

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strconv"
	"unicode/utf8"
)

const codexRPCFrameLimit = 1 << 20

var (
	errCodexRPCFrame = errors.New("无效的 Codex RPC 帧")
	errCodexRPCSize  = errors.New("Codex RPC 帧超过 1 MiB")
)

// The caller must bound and frame input before allocation and strip LF; CR counts.
func decodeCodexRPCFrame(frame []byte) (codexRPCMessage, error) {
	if len(frame) > codexRPCFrameLimit {
		return codexRPCMessage{}, errCodexRPCSize
	}
	if !codexRPCStrictStrings(frame) {
		return codexRPCMessage{}, errCodexRPCFrame
	}
	fields, err := codexRPCObject(frame, "id", "method", "params", "result", "error", "jsonrpc", "trace")
	if err != nil {
		return codexRPCMessage{}, err
	}
	if raw := fields["jsonrpc"]; raw != nil {
		var version string
		if json.Unmarshal(raw, &version) != nil || version != "2.0" {
			return codexRPCMessage{}, errCodexRPCFrame
		}
	}
	message := codexRPCMessage{ID: fields["id"], Params: fields["params"], Result: fields["result"], Error: fields["error"]}
	if message.ID != nil && !validCodexRPCID(message.ID) {
		return codexRPCMessage{}, errCodexRPCFrame
	}
	if raw := fields["method"]; raw != nil {
		if json.Unmarshal(raw, &message.Method) != nil || message.Method == nil {
			return codexRPCMessage{}, errCodexRPCFrame
		}
	}
	if message.Method != nil {
		if message.Result != nil || message.Error != nil {
			return codexRPCMessage{}, errCodexRPCFrame
		}
	} else if message.ID == nil || message.Params != nil || (message.Result == nil) == (message.Error == nil) {
		return codexRPCMessage{}, errCodexRPCFrame
	}
	if message.Error != nil && !validCodexRPCError(message.Error) {
		return codexRPCMessage{}, errCodexRPCFrame
	}
	return message, nil
}

func encodeCodexRPCFrame(message codexRPCMessage) ([]byte, error) {
	if message.Method != nil && len(*message.Method) > codexRPCFrameLimit {
		return nil, errCodexRPCSize
	}
	if message.Method != nil && !utf8.ValidString(*message.Method) {
		return nil, errCodexRPCFrame
	}
	for _, raw := range []json.RawMessage{message.ID, message.Params, message.Result, message.Error} {
		if len(raw) > codexRPCFrameLimit {
			return nil, errCodexRPCSize
		}
		if raw != nil && (!codexRPCStrictStrings(raw) || !json.Valid(raw)) {
			return nil, errCodexRPCFrame
		}
	}
	frame, err := json.Marshal(message)
	if err != nil {
		return nil, errCodexRPCFrame
	}
	if _, err := decodeCodexRPCFrame(frame); err != nil {
		return nil, err
	}
	return frame, nil
}

// Only named protocol fields are retained; unknown metadata is ignored.
func codexRPCObject(raw []byte, keys ...string) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, errCodexRPCFrame
	}
	fields := make(map[string]json.RawMessage)
	for decoder.More() {
		token, err := decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok {
			return nil, errCodexRPCFrame
		}
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return nil, errCodexRPCFrame
		}
		if slices.Contains(keys, key) {
			if _, duplicate := fields[key]; duplicate {
				return nil, errCodexRPCFrame
			}
			fields[key] = value
		}
	}
	if _, err := decoder.Token(); err != nil {
		return nil, errCodexRPCFrame
	}
	if decoder.Decode(new(json.RawMessage)) != io.EOF {
		return nil, errCodexRPCFrame
	}
	return fields, nil
}

// Reject lossy Unicode before encoding/json runs. This is a host policy.
// Skip escaped bytes so a literal backslash followed by uD800 stays literal.
func codexRPCStrictStrings(raw []byte) bool {
	if !utf8.Valid(raw) {
		return false
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] != '\\' {
			continue
		}
		i++
		if i >= len(raw) || raw[i] != 'u' {
			continue
		}
		if i+4 >= len(raw) {
			return false
		}
		value, err := strconv.ParseUint(string(raw[i+1:i+5]), 16, 16)
		if err != nil || value >= 0xDC00 && value <= 0xDFFF {
			return false
		}
		i += 4
		if value < 0xD800 || value > 0xDBFF {
			continue
		}
		if i+6 >= len(raw) || raw[i+1] != '\\' || raw[i+2] != 'u' {
			return false
		}
		low, err := strconv.ParseUint(string(raw[i+3:i+7]), 16, 16)
		if err != nil || low < 0xDC00 || low > 0xDFFF {
			return false
		}
		i += 6
	}
	return true
}
