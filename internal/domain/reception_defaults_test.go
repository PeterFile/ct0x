package domain

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func receptionDefaultsValue() ReceptionDefaults {
	return ReceptionDefaults{
		ID: " defaults / opaque ", Revision: 7,
		ProviderID: " custom runner/路径:unknown ", Model: " model/unsupported@future ",
		Role:      " \t接待职责\nDo not infer authentication. ",
		CreatedAt: time.Date(2026, 11, 1, 14, 0, 0, 123456789, time.FixedZone("test", 8*60*60)),
	}
}

func TestNewReceptionDefaultsPreservesValuesWithoutMutation(t *testing.T) {
	input := receptionDefaultsValue()
	before := input
	want := input
	want.CreatedAt = input.CreatedAt.UTC()
	got, err := NewReceptionDefaults(input)
	if err != nil || got != want {
		t.Fatalf("new defaults: %+v, %v; want %+v", got, err, want)
	}
	if input != before {
		t.Fatal("creation mutated caller data")
	}
	again, err := NewReceptionDefaults(input)
	if err != nil || again != got {
		t.Fatalf("creation was not deterministic: %+v, %v", again, err)
	}
	normalized, err := NewReceptionDefaults(got)
	if err != nil || normalized != got {
		t.Fatalf("creation changed an already normalized snapshot: %+v, %v", normalized, err)
	}
	got.ID, got.ProviderID, got.Model, got.Role = "changed", "changed", "changed", "changed"
	got.Revision++
	got.CreatedAt = got.CreatedAt.Add(time.Hour)
	if input != before || again != want {
		t.Fatal("returned value mutation affected earlier snapshots")
	}
}

func TestReceptionDefaultsValidation(t *testing.T) {
	for name, change := range map[string]func(*ReceptionDefaults){
		"empty id":          func(d *ReceptionDefaults) { d.ID = "" },
		"blank id":          func(d *ReceptionDefaults) { d.ID = " \t\u3000" },
		"zero revision":     func(d *ReceptionDefaults) { d.Revision = 0 },
		"negative revision": func(d *ReceptionDefaults) { d.Revision = -1 },
		"zero time":         func(d *ReceptionDefaults) { d.CreatedAt = time.Time{} },
		"time before range": func(d *ReceptionDefaults) { d.CreatedAt = time.Date(0, 12, 31, 23, 0, 0, 0, time.UTC) },
		"time after range":  func(d *ReceptionDefaults) { d.CreatedAt = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC) },
		"empty provider":    func(d *ReceptionDefaults) { d.ProviderID = "" },
		"blank provider":    func(d *ReceptionDefaults) { d.ProviderID = " \t\u3000" },
		"invalid provider":  func(d *ReceptionDefaults) { d.ProviderID = "provider\xff" },
		"empty model":       func(d *ReceptionDefaults) { d.Model = "" },
		"blank model":       func(d *ReceptionDefaults) { d.Model = " \t\u3000" },
		"invalid model":     func(d *ReceptionDefaults) { d.Model = "model\xfe" },
		"model NUL":         func(d *ReceptionDefaults) { d.Model = "model\x00name" },
		"model CR":          func(d *ReceptionDefaults) { d.Model = "model\rname" },
		"model LF":          func(d *ReceptionDefaults) { d.Model = "model\nname" },
		"empty role":        func(d *ReceptionDefaults) { d.Role = "" },
		"blank role":        func(d *ReceptionDefaults) { d.Role = " \t\u3000" },
		"invalid role":      func(d *ReceptionDefaults) { d.Role = "role\xc3" },
		"time before UTC range": func(d *ReceptionDefaults) {
			d.CreatedAt = time.Date(1, 1, 1, 0, 30, 0, 0, time.FixedZone("test", 60*60))
		},
		"time after UTC range": func(d *ReceptionDefaults) {
			d.CreatedAt = time.Date(9999, 12, 31, 23, 30, 0, 0, time.FixedZone("test", -60*60))
		},
	} {
		t.Run(name, func(t *testing.T) {
			input := receptionDefaultsValue()
			change(&input)
			before := input
			got, err := NewReceptionDefaults(input)
			if !errors.Is(err, ErrReceptionInvalid) || got != (ReceptionDefaults{}) {
				t.Fatalf("want invalid input and no snapshot, got %+v, %v", got, err)
			}
			if input != before {
				t.Fatal("rejected creation mutated caller data")
			}
		})
	}
}

func TestReceptionDefaultsMetadataBoundaries(t *testing.T) {
	for name, change := range map[string]func(*ReceptionDefaults){
		"first revision": func(d *ReceptionDefaults) { d.Revision = 1 },
		"last revision":  func(d *ReceptionDefaults) { d.Revision = 1<<63 - 1 },
		"first nonzero time": func(d *ReceptionDefaults) {
			d.CreatedAt = time.Date(1, 1, 1, 0, 0, 0, 1, time.UTC)
		},
		"last time": func(d *ReceptionDefaults) {
			d.CreatedAt = time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC)
		},
	} {
		t.Run(name, func(t *testing.T) {
			input := receptionDefaultsValue()
			change(&input)
			want := input
			want.CreatedAt = input.CreatedAt.UTC()
			if got, err := NewReceptionDefaults(input); err != nil || got != want {
				t.Fatalf("metadata boundary: %+v, %v; want %+v", got, err, want)
			}
		})
	}
}

func TestReceptionDefaultsTextByteLimits(t *testing.T) {
	for _, field := range []struct {
		name  string
		limit int
		set   func(*ReceptionDefaults, string)
	}{
		{"model", 256, func(d *ReceptionDefaults, value string) { d.Model = value }},
		{"role", 4096, func(d *ReceptionDefaults, value string) { d.Role = value }},
	} {
		t.Run(field.name, func(t *testing.T) {
			multibyte := strings.Repeat("界", field.limit/3) + strings.Repeat("a", field.limit%3)
			for _, tt := range []struct {
				name, value string
				invalid     bool
			}{
				{"ascii at limit", strings.Repeat("a", field.limit), false},
				{"ascii over limit", strings.Repeat("a", field.limit+1), true},
				{"multibyte at limit", multibyte, false},
				{"multibyte over limit", multibyte + "a", true},
			} {
				t.Run(tt.name, func(t *testing.T) {
					input := receptionDefaultsValue()
					field.set(&input, tt.value)
					got, err := NewReceptionDefaults(input)
					if tt.invalid {
						if !errors.Is(err, ErrReceptionInvalid) || got != (ReceptionDefaults{}) {
							t.Fatalf("accepted %d-byte %s: %v", len(tt.value), field.name, err)
						}
						return
					}
					want := input
					want.CreatedAt = input.CreatedAt.UTC()
					if err != nil || got != want {
						t.Fatalf("changed or rejected %d-byte %s: %v", len(tt.value), field.name, err)
					}
				})
			}
		})
	}
}

func TestReceptionDefaultsJSONContract(t *testing.T) {
	defaults, err := NewReceptionDefaults(receptionDefaultsValue())
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(defaults)
	const want = `{"id":" defaults / opaque ","revision":7,"provider_id":" custom runner/路径:unknown ","model":" model/unsupported@future ","role":" \t接待职责\nDo not infer authentication. ","created_at":"2026-11-01T06:00:00.123456789Z"}`
	if err != nil || string(data) != want {
		t.Fatalf("unexpected snapshot fields or values: %s, %v", data, err)
	}
}
