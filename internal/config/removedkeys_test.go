package config

import (
	"context"
	"log/slog"
	"sync"
	"testing"
)

// recordHandler captures slog records so a test can assert on what an operator would actually see.
type recordHandler struct {
	mu      sync.Mutex
	records []slog.Record
	attrs   []map[string]string
}

func (h *recordHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *recordHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r)
	attrs := make(map[string]string, r.NumAttrs())
	r.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.String()
		return true
	})
	h.attrs = append(h.attrs, attrs)
	return nil
}

func (h *recordHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordHandler) WithGroup(string) slog.Handler      { return h }

// TestWarnRemovedKeys_EmitsOneActionableLinePerKey proves the emission half of the disclosure
// pipeline: every key Load collected produces exactly one WARN carrying both the key and a non-empty
// reason, so an operator who set a removed knob learns it no longer applies. Deleting the body of
// WarnRemovedKeys fails here.
func TestWarnRemovedKeys_EmitsOneActionableLinePerKey(t *testing.T) {
	cfg := &Config{RemovedKeys: []string{
		"execution.acp_session_mode",
		"AGY_GW_AUTO_APPROVE_PERMISSIONS",
	}}
	keys := cfg.RemovedKeys

	h := &recordHandler{}
	WarnRemovedKeys(slog.New(h), cfg)

	if len(h.records) != len(keys) {
		t.Fatalf("expected %d warnings, got %d", len(keys), len(h.records))
	}
	for i, rec := range h.records {
		if rec.Level != slog.LevelWarn {
			t.Errorf("record %d must be WARN, got %v", i, rec.Level)
		}
		if got := h.attrs[i]["key"]; got != keys[i] {
			t.Errorf("record %d key = %q, want %q", i, got, keys[i])
		}
		if h.attrs[i]["reason"] == "" {
			t.Errorf("record %d carries no reason", i)
		}
	}
}

// TestWarnRemovedKeys_NilSafe guards the early return used by callers that run before configuration
// exists.
func TestWarnRemovedKeys_NilSafe(t *testing.T) {
	WarnRemovedKeys(nil, &Config{RemovedKeys: []string{"x"}})
	WarnRemovedKeys(slog.New(&recordHandler{}), nil)
}

// TestRemovedKeyNotes_CoverEveryRemovedKey keeps the two tables in sync: a key with no note would
// emit a WARN with an empty reason, which is the "ignored, and you'll never know why" failure the
// disclosure exists to prevent.
func TestRemovedKeyNotes_CoverEveryRemovedKey(t *testing.T) {
	for _, entry := range removedConfigKeys {
		if note := RemovedKeyNote(entry.section + "." + entry.key); note == "" {
			t.Errorf("removed YAML key %s.%s has no note", entry.section, entry.key)
		}
	}
	for _, name := range removedEnvKeys {
		if note := RemovedKeyNote(name); note == "" {
			t.Errorf("removed env var %s has no note", name)
		}
	}
}
