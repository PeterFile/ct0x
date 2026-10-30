package httpapi

import (
	"errors"
	"net/netip"
	"strings"
)

type Config struct {
	Address   string
	DevOrigin string // One opt-in Vite proxy origin; empty disables it.
}

// ValidateDevOrigin accepts only a literal HTTP loopback authority with an
// explicit nonzero port. No URL decorations or origin lists are accepted.
func ValidateDevOrigin(origin string) error {
	if origin == "" {
		return nil
	}
	authority, httpScheme := strings.CutPrefix(origin, "http://")
	address, err := netip.ParseAddrPort(authority)
	if !httpScheme || err != nil || !address.Addr().IsLoopback() || address.Addr().Zone() != "" || address.Port() == 0 {
		return errors.New("开发来源必须是包含明确非零端口的 HTTP 回环 IP 来源，不得含有路径或其他 URL 内容")
	}
	return nil
}
