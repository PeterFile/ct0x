package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"
)

const MaxJSONBodyBytes = 1 << 20

// DecodeJSON writes a JSON error and returns false on invalid input. Callers must
// use a pointer to a narrow request DTO without server-owned identity fields and
// stop on false. Query parameters are never merged into the body; their meaning
// and duplicate validation belong to each route, not to this decoder.
func DecodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	types := r.Header.Values("Content-Type")
	if len(types) != 1 {
		WriteError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "请求必须使用单一的 application/json 内容类型")
		return false
	}
	mediaType, params, err := mime.ParseMediaType(types[0])
	if err != nil || mediaType != "application/json" || params["charset"] != "" && !strings.EqualFold(params["charset"], "utf-8") {
		WriteError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "请求内容类型必须为 application/json，字符编码必须为 UTF-8")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, MaxJSONBodyBytes)
	body, err := io.ReadAll(r.Body)
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		WriteError(w, http.StatusRequestEntityTooLarge, "body_too_large", "请求体不能超过 1 MiB")
		return false
	}
	trimmed := bytes.TrimSpace(body)
	if err == nil && utf8.Valid(body) && len(trimmed) > 0 && trimmed[0] == '{' {
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.DisallowUnknownFields()
		if decoder.Decode(dst) == nil && decoder.Decode(new(any)) == io.EOF && uniqueJSONKeys(json.NewDecoder(bytes.NewReader(body))) {
			return true
		}
	}
	WriteError(w, http.StatusBadRequest, "invalid_json", "请求体必须是有效的单个 JSON 对象，且不得含有未知或重复字段")
	return false
}

// Token traversal catches duplicate keys in nested objects, including arrays.
func uniqueJSONKeys(decoder *json.Decoder) bool {
	token, err := decoder.Token()
	if err != nil {
		return false
	}
	delimiter, container := token.(json.Delim)
	if !container {
		return true
	}
	keys := make(map[string]bool)
	for decoder.More() {
		if delimiter == '{' {
			token, err := decoder.Token()
			key, ok := token.(string)
			if err != nil || !ok {
				return false
			}
			key = foldedJSONKey(key)
			if keys[key] {
				return false
			}
			keys[key] = true
		}
		if !uniqueJSONKeys(decoder) {
			return false
		}
	}
	_, err = decoder.Token()
	return err == nil
}

func foldedJSONKey(key string) string {
	// encoding/json also matches struct field names case-insensitively.
	return strings.Map(func(r rune) rune {
		minimum := r
		for next := unicode.SimpleFold(r); next != r; next = unicode.SimpleFold(next) {
			if next < minimum {
				minimum = next
			}
		}
		return minimum
	}, key)
}
