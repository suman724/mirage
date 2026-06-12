// Package shim implements the server side of Shimmer (docs/design-shimmer.md):
// a FUSE-free projection of a published workspace for platforms that forbid
// kernel mounts (e.g. AWS Fargate). The workspace is a real directory tree of
// sparse placeholder files (skeleton.go); a supervisor on a unix socket
// (supervisor.go) fills a placeholder with its manifest content the first time
// a tool asks to open it. Content travels through the existing desync store
// chain — Shimmer is purely a new consumer of it (design G5).
//
// This file holds the per-path state table and its crash journal. The state
// machine per path is:
//
//	placeholder ──ENSURE──▶ materializing ──▶ materialized
//	     │  ▲ (crash: torn)      │
//	     └──┴──── DIRTY / pristine-check failure ──▶ local
//
// `local` means the on-disk content diverged from the manifest (a tool wrote
// or replaced it); the supervisor must never overwrite a local file with
// manifest content (design §4.1: no silent clobber). With only open()
// intercepted, the local set is a lower bound on actual divergence — tree
// mutations (rename/unlink/...) are invisible until namespace interception
// lands (reserved issue #21).
package shim

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sync"

	"github.com/suman724/mirage/internal/logging"
)

// State is a path's materialization state.
type State string

const (
	// StatePlaceholder: the on-disk file is a sparse placeholder; content
	// lives in the manifest + chunk store.
	StatePlaceholder State = "placeholder"
	// StateMaterializing: a fill is in progress. Journaled BEFORE the first
	// content byte is written, so a crash mid-fill is detectable on replay
	// (the file is "torn": part manifest content, part zeros — it must be
	// re-filled, never mistaken for a user edit).
	StateMaterializing State = "materializing"
	// StateMaterialized: the real file holds exactly the manifest content.
	StateMaterialized State = "materialized"
	// StateLocal: the real file diverged from the manifest (tool write,
	// replaced placeholder, or deletion). Never overwritten.
	StateLocal State = "local"
)

// Marker is the pristine-placeholder fingerprint recorded when a placeholder
// is created or re-observed (design §4.1 safeguard 1). Before a fill, the
// supervisor confirms the on-disk file still matches: same inode, same size,
// and zero allocated blocks (sparse). A mismatch means something replaced the
// placeholder out from under us — the file is treated as local.
type Marker struct {
	Ino  uint64
	Size int64
}

// entry is the in-memory record for one tracked path.
type entry struct {
	state  State
	torn   bool // replayed mid-fill crash: must re-fill, skip pristine check
	marker Marker
}

// journalRecord is one JSON line in the append-only journal.
type journalRecord struct {
	Path  string `json:"path"`
	State State  `json:"state"`
}

// Counts is a snapshot of the table for STATS reporting.
type Counts struct {
	Placeholder  int
	Materialized int
	Local        int
	Torn         int
}

// Table tracks per-path state with an append-only JSON-lines journal so a
// supervisor restart (with a persistent state dir) never re-serves an
// already-materialized or locally-modified file as a pristine placeholder.
// All methods are safe for concurrent use.
type Table struct {
	mu      sync.Mutex
	entries map[string]*entry
	journal *os.File
	path    string
	log     *slog.Logger
}

// OpenTable opens (creating if needed) the journal at path and replays it.
// Paths are tracked lazily afterwards via Track (skeleton build).
func OpenTable(path string, logger *slog.Logger) (*Table, error) {
	t := &Table{
		entries: make(map[string]*entry),
		path:    path,
		log:     logging.OrDefault(logger),
	}
	if err := t.replay(path); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("shim: open journal %q: %w", path, err)
	}
	t.journal = f
	return t, nil
}

// replay loads prior state from the journal, last record per path winning.
// A record of `materializing` with no later record means the process died
// mid-fill: the path replays as a torn placeholder that must be re-filled.
// Malformed lines (e.g. a torn final line from a crash mid-append) are
// skipped loudly rather than failing the whole session.
func (t *Table) replay(path string) error {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil // fresh state dir
	}
	if err != nil {
		return fmt.Errorf("shim: open journal %q for replay: %w", path, err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	line, replayed, skipped := 0, 0, 0
	for sc.Scan() {
		line++
		var rec journalRecord
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			t.log.Warn("skipping malformed journal line (torn append?)",
				"journal", path, "line", line, "err", err)
			skipped++
			continue
		}
		switch rec.State {
		case StateMaterialized:
			t.entries[rec.Path] = &entry{state: StateMaterialized}
		case StateLocal:
			t.entries[rec.Path] = &entry{state: StateLocal}
		case StateMaterializing:
			t.entries[rec.Path] = &entry{state: StatePlaceholder, torn: true}
		case StatePlaceholder:
			t.entries[rec.Path] = &entry{state: StatePlaceholder}
		default:
			t.log.Warn("skipping journal line with unknown state",
				"journal", path, "line", line, "state", rec.State)
			skipped++
		}
		replayed++
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("shim: replay journal %q: %w", path, err)
	}
	if replayed > 0 || skipped > 0 {
		t.log.Info("journal replayed",
			"journal", path, "records", replayed, "skipped", skipped, "paths", len(t.entries))
	}
	return nil
}

// Track registers a path observed at skeleton build with its pristine marker.
// State replayed from the journal is preserved; an untracked path starts as a
// placeholder.
func (t *Table) Track(path string, m Marker) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if e, ok := t.entries[path]; ok {
		e.marker = m
		return
	}
	t.entries[path] = &entry{state: StatePlaceholder, marker: m}
}

// Get returns the path's state and whether it is tracked at all.
func (t *Table) Get(path string) (State, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.entries[path]
	if !ok {
		return "", false
	}
	return e.state, true
}

// IsTorn reports whether the path replayed from a mid-fill crash and must be
// re-filled regardless of the pristine check.
func (t *Table) IsTorn(path string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.entries[path]
	return ok && e.torn
}

// MarkerFor returns the pristine marker recorded for the path.
func (t *Table) MarkerFor(path string) (Marker, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.entries[path]
	if !ok {
		return Marker{}, false
	}
	return e.marker, true
}

// SetMaterializing durably records fill intent. The journal append is synced
// BEFORE the in-memory flip and before any content byte is written, so a
// crash mid-fill replays as torn (re-fill) rather than as a pristine-check
// failure that would misclassify half-written manifest content as a user
// edit. If the append fails, the fill must not proceed.
func (t *Table) SetMaterializing(path string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.append(journalRecord{Path: path, State: StateMaterializing}); err != nil {
		return err
	}
	e, ok := t.entries[path]
	if !ok {
		e = &entry{}
		t.entries[path] = e
	}
	e.state = StateMaterializing
	return nil
}

// SetMaterialized records a completed fill. The in-memory state flips first:
// if the journal append then fails, the worst case after a restart is a
// redundant re-fill, never wrong content.
func (t *Table) SetMaterialized(path string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.entries[path]
	if !ok {
		e = &entry{}
		t.entries[path] = e
	}
	e.state = StateMaterialized
	e.torn = false
	if err := t.append(journalRecord{Path: path, State: StateMaterialized}); err != nil {
		return err
	}
	return nil
}

// SetLocal records that the on-disk content diverged from the manifest (tool
// write, replaced placeholder, deletion). The in-memory state flips first so
// the no-clobber guarantee holds for this process even if the append fails;
// a failed append degrades restart recovery only (the pristine check is the
// backstop) and is reported to the caller.
func (t *Table) SetLocal(path string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.entries[path]
	if !ok {
		e = &entry{}
		t.entries[path] = e
	}
	e.state = StateLocal
	e.torn = false
	if err := t.append(journalRecord{Path: path, State: StateLocal}); err != nil {
		return err
	}
	return nil
}

// MarkTorn flags an in-process failed fill: the file holds partial manifest
// content (not a user edit), so the retry must re-fill, skipping the pristine
// check. The journal already says `materializing`, which replays as torn —
// no new record is needed.
func (t *Table) MarkTorn(path string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if e, ok := t.entries[path]; ok {
		e.state = StatePlaceholder
		e.torn = true
	}
}

// append writes one record and syncs. Callers hold t.mu.
func (t *Table) append(rec journalRecord) error {
	if t.journal == nil {
		return fmt.Errorf("shim: journal %q is closed", t.path)
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("shim: marshal journal record: %w", err)
	}
	if _, err := t.journal.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("shim: append journal %q: %w", t.path, err)
	}
	if err := t.journal.Sync(); err != nil {
		return fmt.Errorf("shim: sync journal %q: %w", t.path, err)
	}
	return nil
}

// Counts snapshots the table for STATS.
func (t *Table) Counts() Counts {
	t.mu.Lock()
	defer t.mu.Unlock()
	var c Counts
	for _, e := range t.entries {
		switch e.state {
		case StatePlaceholder, StateMaterializing:
			c.Placeholder++
		case StateMaterialized:
			c.Materialized++
		case StateLocal:
			c.Local++
		}
		if e.torn {
			c.Torn++
		}
	}
	return c
}

// Close syncs and closes the journal. The table must not be mutated after.
func (t *Table) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.journal == nil {
		return nil
	}
	err := t.journal.Close()
	t.journal = nil
	if err != nil {
		return fmt.Errorf("shim: close journal %q: %w", t.path, err)
	}
	return nil
}
