package shim

import (
	"os"
	"path/filepath"
	"testing"
)

func TestJournalReplay(t *testing.T) {
	dir := t.TempDir()
	jp := filepath.Join(dir, "journal.jsonl")

	tab, err := OpenTable(jp, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	if err := tab.SetMaterialized("done.txt"); err != nil {
		t.Fatal(err)
	}
	if err := tab.SetLocal("edited.txt"); err != nil {
		t.Fatal(err)
	}
	// Simulate a crash mid-fill: intent journaled, no completion record.
	if err := tab.SetMaterializing("torn.bin"); err != nil {
		t.Fatal(err)
	}
	if err := tab.Close(); err != nil {
		t.Fatal(err)
	}

	re, err := OpenTable(jp, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer re.Close()

	if st, ok := re.Get("done.txt"); !ok || st != StateMaterialized {
		t.Errorf("done.txt replayed as %q/%v, want materialized", st, ok)
	}
	if st, ok := re.Get("edited.txt"); !ok || st != StateLocal {
		t.Errorf("edited.txt replayed as %q/%v, want local", st, ok)
	}
	st, ok := re.Get("torn.bin")
	if !ok || st != StatePlaceholder {
		t.Errorf("torn.bin replayed as %q/%v, want placeholder", st, ok)
	}
	if !re.IsTorn("torn.bin") {
		t.Error("torn.bin must replay as torn (mid-fill crash)")
	}
}

func TestJournalStateProgression(t *testing.T) {
	dir := t.TempDir()
	jp := filepath.Join(dir, "journal.jsonl")

	tab, err := OpenTable(jp, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	// Full successful lifecycle: the completion record supersedes the intent.
	if err := tab.SetMaterializing("a"); err != nil {
		t.Fatal(err)
	}
	if err := tab.SetMaterialized("a"); err != nil {
		t.Fatal(err)
	}
	tab.Close()

	re, err := OpenTable(jp, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer re.Close()
	if st, _ := re.Get("a"); st != StateMaterialized {
		t.Errorf("a replayed as %q, want materialized (last record wins)", st)
	}
	if re.IsTorn("a") {
		t.Error("a must not be torn after a completed fill")
	}
}

func TestJournalToleratesMalformedLines(t *testing.T) {
	dir := t.TempDir()
	jp := filepath.Join(dir, "journal.jsonl")
	// A valid record, garbage, an unknown state, and a torn final append.
	content := `{"path":"good.txt","state":"materialized"}
this is not json
{"path":"weird.txt","state":"quantum"}
{"path":"half`
	if err := os.WriteFile(jp, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	tab, err := OpenTable(jp, quietLogger())
	if err != nil {
		t.Fatalf("replay must tolerate malformed lines, got: %v", err)
	}
	defer tab.Close()

	if st, ok := tab.Get("good.txt"); !ok || st != StateMaterialized {
		t.Errorf("good.txt = %q/%v, want materialized", st, ok)
	}
	if _, ok := tab.Get("weird.txt"); ok {
		t.Error("unknown-state record must be skipped, not tracked")
	}
}

func TestTrackPreservesReplayedState(t *testing.T) {
	dir := t.TempDir()
	jp := filepath.Join(dir, "journal.jsonl")

	tab, err := OpenTable(jp, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	if err := tab.SetLocal("edited.txt"); err != nil {
		t.Fatal(err)
	}
	tab.Close()

	re, err := OpenTable(jp, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer re.Close()

	// Skeleton build re-observes every manifest path; a replayed local file
	// must stay local (its content is the user's).
	re.Track("edited.txt", Marker{Ino: 42, Size: 7})
	if st, _ := re.Get("edited.txt"); st != StateLocal {
		t.Errorf("Track downgraded replayed state to %q", st)
	}
	if m, ok := re.MarkerFor("edited.txt"); !ok || m.Ino != 42 {
		t.Errorf("Track must still record the marker, got %+v/%v", m, ok)
	}

	re.Track("fresh.txt", Marker{Ino: 7, Size: 1})
	if st, _ := re.Get("fresh.txt"); st != StatePlaceholder {
		t.Errorf("fresh path = %q, want placeholder", st)
	}
}

func TestSetLocalOnUntrackedPath(t *testing.T) {
	tab, err := OpenTable(filepath.Join(t.TempDir(), "j"), quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer tab.Close()

	// DIRTY for a tool-created file outside the manifest must be tracked —
	// it is part of the future write-back set.
	if err := tab.SetLocal("brand-new.txt"); err != nil {
		t.Fatal(err)
	}
	if st, ok := tab.Get("brand-new.txt"); !ok || st != StateLocal {
		t.Errorf("untracked DIRTY path = %q/%v, want local", st, ok)
	}
}

func TestAppendAfterCloseFails(t *testing.T) {
	tab, err := OpenTable(filepath.Join(t.TempDir(), "j"), quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	tab.Close()
	if err := tab.SetMaterializing("x"); err == nil {
		t.Error("SetMaterializing after Close must fail (fill intent must be durable)")
	}
}
