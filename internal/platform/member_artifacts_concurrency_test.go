//go:build unix

package platform

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agentworkspace/internal/provider"
	"agentworkspace/internal/store"
)

// Only the private test grant is wrapped. All opens, verification and reads
// delegate to the actual blob owner; the database scope is never substituted.
// Gates model callback drain, not interruption of a blocked kernel syscall.
type memberArtifactGate struct {
	phase   string
	failure string
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	closed  atomic.Int32
}

func (g *memberArtifactGate) pause(phase string) {
	if g.phase == phase {
		g.once.Do(func() { close(g.entered); <-g.release })
	}
}

type memberArtifactGatedReader struct {
	io.ReadCloser
	gate *memberArtifactGate
}

func (r *memberArtifactGatedReader) Read(p []byte) (int, error) {
	if r.gate.failure == "skip" && len(p) > 1 {
		p = p[:1]
	}
	n, err := r.ReadCloser.Read(p)
	r.gate.pause("read")
	if r.gate.failure == "read" || r.gate.failure == "skip" {
		return n, errors.New("PRIVATE-READ-ERROR /private/blob")
	}
	return n, err
}

func (r *memberArtifactGatedReader) Close() error {
	err := r.ReadCloser.Close()
	r.gate.closed.Add(1)
	r.gate.pause("close")
	if r.gate.failure == "close" {
		return errors.New("PRIVATE-CLOSE-ERROR /private/blob")
	}
	return err
}

func memberArtifactInstallGate(t *testing.T, f *memberArtifactFixture, phase, failure string) (*memberArtifactGate, func()) {
	t.Helper()
	f.tools.Revoke(f.token)
	g := &memberArtifactGate{phase: phase, failure: failure, entered: make(chan struct{}), release: make(chan struct{})}
	realOpen := f.grant.open
	f.grant.open = func(hash string) (io.ReadCloser, error) {
		reader, err := realOpen(hash)
		g.pause("open")
		if err != nil {
			return nil, err
		}
		return &memberArtifactGatedReader{ReadCloser: reader, gate: g}, nil
	}
	f.issue(t)
	var once sync.Once
	release := func() { once.Do(func() { close(g.release) }) }
	t.Cleanup(release)
	return g, release
}

func memberArtifactWait(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a controlled artifact callback")
	}
}

func memberArtifactChangeScope(t *testing.T, f *memberArtifactFixture, change string, cancel context.CancelFunc) {
	t.Helper()
	ctx := context.Background()
	var err error
	switch change {
	case "active":
	case "cancel":
		_, err = f.db.RequestTurnCancel(ctx, f.groupID, f.claim.TurnID)
	case "terminal":
		err = f.db.RecordTurnEvent(ctx, f.claim, provider.Event{Kind: provider.AcceptedEvent, NativeSessionID: "native"})
		if err == nil {
			err = f.db.RecordTurnEvent(ctx, f.claim, provider.Event{Kind: provider.TerminalEvent, NativeSessionID: "native", Terminal: provider.OutcomeSucceeded})
		}
	case "claim":
		_, err = f.raw.Exec("UPDATE turns SET claim_token=? WHERE id=?", "replacement-claim", f.claim.TurnID)
	case "context":
		cancel()
	default:
		t.Fatalf("unknown scope transition: %s", change)
	}
	artifactsMust(t, err)
}

func TestMemberArtifactFinalFenceAfterIOCloseAndBudgetErrors(t *testing.T) {
	for _, operation := range []string{"open", "read", "close", "open-error", "skip-error", "read-error", "close-error", "budget"} {
		for _, change := range []string{"active", "cancel", "terminal", "claim", "context"} {
			t.Run(operation+"/"+change, func(t *testing.T) {
				f := newMemberArtifactFixture(t, 16384)
				data, phase, failure := "actual verified bytes", operation, ""
				input := MemberArtifactReadInput{MaxBytes: 16384}
				switch operation {
				case "open-error":
					phase = "open"
				case "skip-error":
					phase, failure, input.Offset = "read", "skip", 2
				case "read-error":
					phase, failure = "read", "read"
				case "close-error":
					phase, failure = "close", "close"
				case "budget":
					phase, data = "close", strings.Repeat("<", 16384)
				}
				v := f.publish(t, PublishArtifactInput{Name: "report"}, data, "")
				input.ArtifactID, input.VersionID = v.ArtifactID, v.ID
				if operation == "open-error" {
					artifactsMust(t, f.private.Remove(filepath.Join("blobs", v.BlobHash)))
				}
				gate, release := memberArtifactInstallGate(t, f, phase, failure)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				type outcome struct {
					result MemberArtifactReadResult
					err    error
				}
				done := make(chan outcome, 1)
				go func() {
					result, err := f.tools.ReadArtifact(ctx, f.token, input)
					done <- outcome{result, err}
				}()
				memberArtifactWait(t, gate.entered)
				memberArtifactChangeScope(t, f, change, cancel)
				select {
				case <-done:
					t.Fatal("read escaped the controlled callback before drain")
				default:
				}
				release()
				got := <-done
				wantClosed := int32(1)
				if operation == "open-error" {
					wantClosed = 0
				}
				if gate.closed.Load() != wantClosed {
					t.Fatalf("reader closed %d times, want %d", gate.closed.Load(), wantClosed)
				}
				if change == "context" {
					memberArtifactZeroError(t, got.result, got.err, context.Canceled)
				} else if change != "active" {
					memberArtifactZeroError(t, got.result, got.err, store.ErrMemberToolDenied)
				} else if operation == "budget" {
					memberArtifactZeroError(t, got.result, got.err, store.ErrInvalid)
				} else if strings.HasSuffix(operation, "-error") {
					if got.result != (MemberArtifactReadResult{}) || got.err == nil || got.err.Error() != "读取成果内容失败" {
						t.Fatalf("raw or missing IO error: %+v, %v", got.result, got.err)
					}
				} else if got.err != nil || got.result.Content != data || !got.result.ContentComplete {
					t.Fatalf("active read was discarded: %+v, %v", got.result, got.err)
				}
				_, err := f.db.GetGroup(context.Background(), f.groupID)
				artifactsMust(t, err)
				if operation != "open-error" {
					reader, err := f.blobs.Open(v.BlobHash)
					artifactsMust(t, err)
					artifactsRead(t, reader, data)
				}
			})
		}
	}
}

func TestMemberArtifactGlobalRevokeAndOtherRunControlDrain(t *testing.T) {
	for _, phase := range []string{"open", "close"} {
		for _, control := range []string{"revoke", "revoke-turn", "other-issue", "other-revoke", "other-revoke-turn"} {
			t.Run(phase+"/"+control, func(t *testing.T) {
				f := newMemberArtifactFixture(t, 64)
				v := f.publish(t, PublishArtifactInput{Name: "report"}, "verified", "")
				otherClaim := memberCapabilityClaim(t, memberArtifactLocal(f.artifactsFixture), f.otherID)
				otherToken := ""
				if control != "other-issue" {
					var err error
					otherToken, err = f.tools.Issue(context.Background(), otherClaim, MemberToolGrants{Operations: []string{"list_members"}})
					artifactsMust(t, err)
				}
				gate, release := memberArtifactInstallGate(t, f, phase, "")
				used := make(chan error, 1)
				go func() {
					got, err := f.tools.ReadArtifact(context.Background(), f.token, MemberArtifactReadInput{ArtifactID: v.ArtifactID, VersionID: v.ID})
					if err == nil && got.Content != "verified" {
						err = errors.New("admitted read lost bytes")
					}
					used <- err
				}()
				memberArtifactWait(t, gate.entered)
				started, controlled := make(chan struct{}), make(chan error, 1)
				go func() {
					close(started)
					var err error
					switch control {
					case "revoke":
						f.tools.Revoke(f.token)
					case "revoke-turn":
						f.tools.RevokeTurn(f.claim.TurnID)
					case "other-issue":
						_, err = f.tools.Issue(context.Background(), otherClaim, MemberToolGrants{Operations: []string{"list_members"}})
					case "other-revoke":
						f.tools.Revoke(otherToken)
					case "other-revoke-turn":
						f.tools.RevokeTurn(otherClaim.TurnID)
					}
					controlled <- err
				}()
				<-started
				select {
				case <-controlled:
					t.Error("global registry writer escaped an admitted artifact callback")
				case <-time.After(25 * time.Millisecond):
				}
				release()
				artifactsMust(t, <-used)
				artifactsMust(t, <-controlled)
				if gate.closed.Load() != 1 {
					t.Fatal("drained reader was not closed once")
				}
				if control == "revoke" || control == "revoke-turn" {
					got, err := f.tools.ReadArtifact(context.Background(), f.token, MemberArtifactReadInput{ArtifactID: v.ArtifactID, VersionID: v.ID})
					memberArtifactZeroError(t, got, err, store.ErrMemberToolDenied)
				}
				reader, err := f.blobs.Open(v.BlobHash)
				artifactsMust(t, err)
				artifactsRead(t, reader, "verified")
				_, err = f.db.GetGroup(context.Background(), f.groupID)
				artifactsMust(t, err)
			})
		}
	}
}
