package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"agentworkspace/internal/platform"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/store"
)

const backgroundQualificationLifetime = 30 * time.Second

var errBackgroundApproval = errors.New("群组后台调度预览授权无效")

type backgroundDispatchApproval struct {
	ProviderID       string                           `json:"provider_id"`
	Model            string                           `json:"model"`
	MaxActiveMembers int                              `json:"max_active_members"`
	ApprovalExpires  time.Time                        `json:"approval_expires_at"`
	Paused           bool                             `json:"paused"`
	Preset           backgroundDispatchPreset         `json:"preset"`
	Qualification    *backgroundDispatchQualification `json:"qualification"`
}

type backgroundDispatchPreset struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Role string `json:"role"`
}

// A local operator attests an actual successful run for this exact account/model
// and transport. Neither a startup probe nor an adapter report supplies this.
// This preview has no built-in producer of such evidence.
type backgroundDispatchQualification struct {
	ProviderID string    `json:"provider_id"`
	Model      string    `json:"model"`
	Path       string    `json:"path"`
	Version    string    `json:"version"`
	Transport  string    `json:"transport"`
	ObservedAt time.Time `json:"observed_at"`
}

func (a backgroundDispatchApproval) profile() store.BackgroundDispatchProfile {
	return store.BackgroundDispatchProfile{ProviderID: a.ProviderID, Model: a.Model}
}

func (a backgroundDispatchApproval) qualified(selected provider.Provider, now time.Time) bool {
	q := a.Qualification
	return !a.Paused && now.Before(a.ApprovalExpires) && q != nil && q.ProviderID == a.ProviderID && q.Model == a.Model &&
		q.Path == selected.Path && q.Version == selected.Version && q.Transport == provider.PiOneShotAdapterID &&
		!q.ObservedAt.IsZero() && !now.Before(q.ObservedAt) && now.Before(q.ObservedAt.Add(backgroundQualificationLifetime))
}

func readBackgroundDispatchApproval(path, providerID string) (a backgroundDispatchApproval, err error) {
	const maxBytes = 16 << 10
	file, err := openBackgroundDispatchFile(path)
	if err != nil {
		return a, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || (info.Mode() != 0600 && info.Mode() != 0400) || info.Size() > maxBytes {
		return a, errBackgroundApproval
	}
	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil || len(data) > maxBytes || !utf8.Valid(data) || !json.Valid(data) {
		return a, errBackgroundApproval
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&a) != nil || a.ProviderID != providerID || !backgroundApprovalText(a.Model, 256) || strings.HasPrefix(a.Model, "-") ||
		a.MaxActiveMembers < 1 || a.MaxActiveMembers > 200 || a.ApprovalExpires.IsZero() ||
		!backgroundApprovalText(a.Preset.ID, 128) || !backgroundApprovalText(a.Preset.Name, 128) || !backgroundApprovalText(a.Preset.Role, 512) {
		return a, errBackgroundApproval
	}
	// Do not use a just-withdrawn or replaced grant from an already open inode.
	live, err := os.Lstat(path)
	if err != nil || !sameBackgroundFile(info, live) {
		return a, errBackgroundApproval
	}
	return a, nil
}

func backgroundApprovalText(s string, limit int) bool {
	return s != "" && len(s) <= limit && strings.TrimSpace(s) == s && !strings.ContainsAny(s, "\x00\r\n")
}

func sameBackgroundFile(initial, live os.FileInfo) bool {
	return os.SameFile(initial, live) && initial.Mode() == live.Mode() && initial.Size() == live.Size() && initial.ModTime().Equal(live.ModTime())
}

// Compose one synchronous callback; sample A1 last, immediately before the
// scheduler scans for claims. Disabled startup never reads an approval file.
func (c config) withBackgroundDispatchPass(db *store.Store, preparer *platform.Preparer, providers []provider.Provider, intentPass func(context.Context), output io.Writer) (func(context.Context), error) {
	if c.groupBackgroundDispatchPreview == "" {
		return intentPass, nil
	}
	i := slices.IndexFunc(providers, func(p provider.Provider) bool {
		return p.Name == provider.Pi && p.Path == filepath.Clean(c.piPath)
	})
	pathErr := errors.New("群组后台调度预览需要目录中明确选定的受支持 Pi --pi-path")
	if i < 0 {
		return nil, pathErr
	}
	selected := providers[i]
	read := func() (backgroundDispatchApproval, error) {
		return readBackgroundDispatchApproval(c.groupBackgroundDispatchPreview, selected.ID)
	}
	initial, err := read()
	if err != nil {
		return nil, fmt.Errorf("读取群组后台调度预览授权失败：%w", err)
	}
	pinned, err := os.Stat(selected.Path)
	if err != nil || !preparer.SupportsBackgroundDiscussionAdapter(initial.profile()) {
		return nil, pathErr
	}
	return func(ctx context.Context) {
		if intentPass != nil {
			intentPass(ctx)
		}
		// The Store requires a valid capacity even to publish an empty list.
		// Never retain the initial approval, preset or evidence as fallback.
		options := store.BackgroundDispatchOptions{MaxActiveMembers: initial.MaxActiveMembers}
		approval, readErr := read()
		if readErr == nil {
			options.MaxActiveMembers = approval.MaxActiveMembers
			profile := approval.profile()
			live, statErr := os.Stat(selected.Path)
			if statErr == nil && sameBackgroundFile(pinned, live) && preparer.SupportsBackgroundDiscussionAdapter(profile) && approval.qualified(selected, time.Now()) {
				options.ReadyProfiles = []store.BackgroundDispatchProfile{profile}
				options.Presets = []store.BackgroundDispatchTemplate{{ID: approval.Preset.ID, Name: approval.Preset.Name, Role: approval.Preset.Role, Profile: profile}}
			}
		} else if errors.Is(readErr, os.ErrNotExist) {
			readErr = nil // Removing the file is an explicit withdrawal, not an error.
		}
		if err := errors.Join(readErr, db.RunGroupBackgroundDispatch(ctx, options)); err != nil {
			fmt.Fprintf(output, "群组后台调度扫描失败：%v\n", err)
		}
	}, nil
}
