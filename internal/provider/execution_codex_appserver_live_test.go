//go:build providerlive

package provider

import "testing"

func TestProviderLiveCodexAppServer(t *testing.T) {
	providerLiveSmoke(t, Codex, "gpt-6.1-sol", "", NewCodexAppServerExecutor)
}
