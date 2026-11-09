package provider

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var errExecutorConfiguration = errors.New("提供方执行配置无效或版本不受支持")

// Preserve the actual stat cause for trusted inspection without rendering paths.
type executorStatFailure struct{ cause error }

func (e *executorStatFailure) Error() string { return errExecutorConfiguration.Error() }
func (e *executorStatFailure) Unwrap() []error {
	return []error{errExecutorConfiguration, e.cause}
}

// NewExecutor selects the verified fresh-turn baseline without probing the CLI,
// credentials or account. Pi disables tools; Codex uses a read-only sandbox and
// may use native read tools. Versions must already be normalized. Account and
// discovery capability flags do not authorize or upgrade this baseline.
// A zero timeout uses the adapter's ten-minute default; negatives are invalid.
func NewExecutor(selected Provider, timeout time.Duration) (Executor, error) {
	if strings.TrimSpace(selected.ID) == "" || selected.Installation != Installed || timeout < 0 ||
		!filepath.IsAbs(selected.Path) || strings.ContainsRune(selected.Path, 0) {
		return nil, errExecutorConfiguration
	}
	if (selected.Name != Pi || selected.Version != "1.4.0") &&
		(selected.Name != Codex || selected.Version != "0.162.1" && selected.Version != "0.166.0") {
		return nil, errExecutorConfiguration
	}
	info, err := os.Stat(selected.Path)
	if err != nil {
		return nil, &executorStatFailure{cause: err}
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return nil, errExecutorConfiguration
	}
	if selected.Name == Pi {
		return NewPiExecutor(selected.Path, timeout)
	}
	return NewCodexExecutor(selected.Path, timeout)
}
