package provider

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
)

const acpFrameLimit = 1 << 20

var errACPProtocol = errors.New("ACP v1 协议无效或超过限制")

type acpMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   json.RawMessage `json:"error,omitempty"`
}

// Validate every object, including ignored metadata, before decoding projections.
// Reuse only the existing lossless UTF-8/surrogate lexer, not a Codex wire schema.
func acpStrictJSON(raw []byte) bool {
	if len(raw) == 0 || len(raw) > acpFrameLimit || !codexRPCStrictStrings(raw) {
		return false
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var value func(int) bool
	value = func(depth int) bool {
		if depth > 64 {
			return false
		}
		token, err := d.Token()
		if err != nil {
			return false
		}
		delim, compound := token.(json.Delim)
		if !compound {
			return true
		}
		switch delim {
		case '{':
			seen := make(map[string]bool)
			for d.More() {
				key, err := d.Token()
				name, ok := key.(string)
				if err != nil || !ok || seen[name] {
					return false
				}
				seen[name] = true
				if !value(depth + 1) {
					return false
				}
			}
			end, err := d.Token()
			return err == nil && end == json.Delim('}')
		case '[':
			for d.More() {
				if !value(depth + 1) {
					return false
				}
			}
			end, err := d.Token()
			return err == nil && end == json.Delim(']')
		}
		return false
	}
	return value(0) && d.Decode(new(json.RawMessage)) == io.EOF
}

func acpObject(raw []byte) (map[string]json.RawMessage, bool) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return nil, false
	}
	return fields, true
}

func acpString(fields map[string]json.RawMessage, key string) (string, bool) {
	var value string
	raw := fields[key]
	if len(raw) == 0 || raw[0] != '"' || json.Unmarshal(raw, &value) != nil {
		return "", false
	}
	return value, true
}

// IDs are type-sensitive; numeric IDs must be lossless, bounded integers.
func acpID(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || len(raw) > 512 {
		return "", false
	}
	if raw[0] == '"' {
		var id string
		if json.Unmarshal(raw, &id) == nil && id != "" {
			return "s:" + id, true
		}
		return "", false
	}
	id, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil || id < -(1<<53-1) || id > 1<<53-1 || strconv.FormatInt(id, 10) != string(raw) {
		return "", false
	}
	return "n:" + string(raw), true
}

func decodeACPMessage(raw []byte) (acpMessage, error) {
	var m acpMessage
	if !acpStrictJSON(raw) {
		return m, errACPProtocol
	}
	fields, ok := acpObject(raw)
	if !ok || string(fields["jsonrpc"]) != `"2.0"` {
		return m, errACPProtocol
	}
	m.JSONRPC, m.ID, m.Params, m.Result, m.Error = "2.0", fields["id"], fields["params"], fields["result"], fields["error"]
	if m.ID != nil {
		if _, ok := acpID(m.ID); !ok {
			return acpMessage{}, errACPProtocol
		}
	}
	if fields["method"] != nil {
		m.Method, ok = acpString(fields, "method")
		if !ok || m.Method == "" || len(m.Method) > 256 || m.Result != nil || m.Error != nil {
			return acpMessage{}, errACPProtocol
		}
		if m.Params != nil {
			if _, ok := acpObject(m.Params); !ok {
				return acpMessage{}, errACPProtocol
			}
		}
	} else {
		if m.ID == nil || m.Params != nil || (m.Result == nil) == (m.Error == nil) {
			return acpMessage{}, errACPProtocol
		}
		if m.Result != nil {
			if _, ok := acpObject(m.Result); !ok {
				return acpMessage{}, errACPProtocol
			}
		}
		if m.Error != nil {
			errFields, ok := acpObject(m.Error)
			var code int64
			_, textOK := acpString(errFields, "message")
			if !ok || !textOK || json.Unmarshal(errFields["code"], &code) != nil {
				return acpMessage{}, errACPProtocol
			}
		}
	}
	return m, nil
}

func acpJSON(value any) json.RawMessage {
	raw, _ := json.Marshal(value)
	return raw
}

type acpRead struct {
	message acpMessage
	damaged bool
	done    bool
	eof     bool
}

// Parsing backpressure is bounded to one frame; damage switches to raw discard.
// Unterminated final JSON is not a complete JSONL frame.
func readACPOutput(source io.Reader, output chan<- acpRead) {
	r := bufio.NewReaderSize(source, 4096)
	for {
		var frame []byte
		for {
			part, err := r.ReadSlice('\n')
			if len(frame)+len(part) > acpFrameLimit+1 {
				output <- acpRead{damaged: true}
				_, drainErr := io.Copy(io.Discard, r)
				output <- acpRead{done: true, eof: drainErr == nil}
				return
			}
			frame = append(frame, part...)
			if err == bufio.ErrBufferFull {
				continue
			}
			if err == io.EOF && len(frame) == 0 {
				output <- acpRead{done: true, eof: true}
				return
			}
			if err != nil {
				output <- acpRead{damaged: true}
				output <- acpRead{done: true, eof: err == io.EOF}
				return
			}
			m, decodeErr := decodeACPMessage(frame[:len(frame)-1])
			if decodeErr != nil {
				output <- acpRead{damaged: true}
				_, drainErr := io.Copy(io.Discard, r)
				output <- acpRead{done: true, eof: drainErr == nil}
				return
			}
			output <- acpRead{message: m}
			break
		}
	}
}

func acpReference(value string) bool {
	return value != "" && len(value) <= 512 && !strings.ContainsAny(value, "\x00\r\n")
}
