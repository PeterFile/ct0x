package preflight

import "testing"

func TestCIObserverRequiredFailureReceipt(t *testing.T) {
	t.Fatal("intentional CI observer receipt fault; keep this draft red")
}
