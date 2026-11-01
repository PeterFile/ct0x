package domain

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

var ErrReceptionInvalid = errors.New("默认接待配置参数无效")

// ReceptionDefaults is an immutable saved snapshot with server-supplied metadata.
type ReceptionDefaults struct {
	ID         string    `json:"id"`
	Revision   int64     `json:"revision"`
	ProviderID string    `json:"provider_id"`
	Model      string    `json:"model"`
	Role       string    `json:"role"`
	CreatedAt  time.Time `json:"created_at"`
}

// NewReceptionDefaults validates structure, not authentication, provider readiness,
// or version support. Server-side callers supply authoritative identity, revision,
// and timestamp; this constructor never advances the revision.
func NewReceptionDefaults(input ReceptionDefaults) (ReceptionDefaults, error) {
	stamp := input.CreatedAt.UTC()
	year := stamp.Year()
	if strings.TrimSpace(input.ID) == "" || input.Revision < 1 || stamp.IsZero() || year < 1 || year > 9999 {
		return ReceptionDefaults{}, fmt.Errorf("%w：配置标识、版本或创建时间无效", ErrReceptionInvalid)
	}
	if !utf8.ValidString(input.ProviderID) || strings.TrimSpace(input.ProviderID) == "" {
		return ReceptionDefaults{}, fmt.Errorf("%w：运行器标识必须是非空 UTF-8 文本", ErrReceptionInvalid)
	}
	if !utf8.ValidString(input.Model) || strings.TrimSpace(input.Model) == "" || len(input.Model) > 256 || strings.ContainsAny(input.Model, "\x00\r\n") {
		return ReceptionDefaults{}, fmt.Errorf("%w：模型必须是非空 UTF-8 文本且不超过 256 字节，不能含 NUL、回车或换行", ErrReceptionInvalid)
	}
	if !utf8.ValidString(input.Role) || strings.TrimSpace(input.Role) == "" || len(input.Role) > 4096 {
		return ReceptionDefaults{}, fmt.Errorf("%w：职责必须是非空 UTF-8 文本且不超过 4096 字节", ErrReceptionInvalid)
	}
	input.CreatedAt = stamp
	return input, nil
}
