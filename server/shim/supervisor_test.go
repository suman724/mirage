package shim

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/folbricht/desync"

	"github.com/suman724/mirage/internal/chunk"
)

func TestEnsureMaterializesFile(t *testing.T) {
	e := newEnv(t)
	const rel = "sub/dir/big"

	if got := blocksOf(t, e.abs(rel)); got != 0 {
		t.Fatalf("placeholder not sparse before ENSURE (%d blocks)", got)
	}

	if resp := request(t, e.sock, "ENSURE "+e.abs(rel)); resp != "OK" {
		t.Fatalf("ENSURE = %q", resp)
	}

	got, err := os.ReadFile(e.abs(rel))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, e.files[rel]) {
		t.Fatalf("materialized content differs (%d vs %d bytes)", len(got), len(e.files[rel]))
	}
	if st, _ := e.table.Get(rel); st != StateMaterialized {
		t.Errorf("state = %q, want materialized", st)
	}

	// Materialization must be mtime-invisible (skeleton stamp preserved).
	fi, err := os.Lstat(e.abs(rel))
	if err != nil {
		t.Fatal(err)
	}
	if !fi.ModTime().Truncate(time.Second).Equal(e.buildTime) {
		t.Errorf("mtime = %v, want skeleton stamp %v", fi.ModTime(), e.buildTime)
	}

	// Laziness: exactly this file's chunks were fetched, nothing else.
	want := uniqueChunks(e.manifest, rel)
	if got := int(e.store.gets.Load()); got != want {
		t.Errorf("store fetches = %d, want %d (only the ensured file)", got, want)
	}

	// Other placeholders stayed sparse.
	if got := blocksOf(t, e.abs("hello.txt")); got != 0 {
		t.Errorf("untouched placeholder lost sparseness (%d blocks)", got)
	}

	// Second ENSURE is a no-op: no new fetches.
	if resp := request(t, e.sock, "ENSURE "+e.abs(rel)); resp != "OK" {
		t.Fatalf("second ENSURE = %q", resp)
	}
	if got := int(e.store.gets.Load()); got != want {
		t.Errorf("warm ENSURE refetched: %d fetches, want %d", got, want)
	}
}

func TestConcurrentEnsuresFillOnce(t *testing.T) {
	e := newEnv(t)
	const rel = "sub/dir/big"

	var wg sync.WaitGroup
	errs := make(chan string, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if resp := request(t, e.sock, "ENSURE "+e.abs(rel)); resp != "OK" {
				errs <- resp
			}
		}()
	}
	wg.Wait()
	close(errs)
	for resp := range errs {
		t.Errorf("concurrent ENSURE = %q", resp)
	}

	got, err := os.ReadFile(e.abs(rel))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, e.files[rel]) {
		t.Fatal("content differs after concurrent ENSUREs")
	}
	// Singleflight: one fill, so each unique chunk fetched exactly once.
	if want, gotN := uniqueChunks(e.manifest, rel), int(e.store.gets.Load()); gotN != want {
		t.Errorf("store fetches = %d, want %d (per-path singleflight)", gotN, want)
	}
}

func TestDirtyThenEnsureDoesNotClobber(t *testing.T) {
	e := newEnv(t)
	const rel = "hello.txt"
	user := []byte("user wrote this in place")

	// A tool opened the placeholder for write: shim sends DIRTY, bytes land.
	if resp := request(t, e.sock, "DIRTY "+e.abs(rel)); resp != "OK" {
		t.Fatalf("DIRTY = %q", resp)
	}
	if err := os.WriteFile(e.abs(rel), user, 0o644); err != nil {
		t.Fatal(err)
	}

	if resp := request(t, e.sock, "ENSURE "+e.abs(rel)); resp != "OK" {
		t.Fatalf("ENSURE after DIRTY = %q", resp)
	}
	got, err := os.ReadFile(e.abs(rel))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, user) {
		t.Fatalf("ENSURE clobbered local content: %q", got)
	}
}

func TestPristineCheckCatchesRenameOver(t *testing.T) {
	e := newEnv(t)
	const rel = "hello.txt"
	user := []byte("atomically saved by an editor")

	// The §4.1 data-loss scenario: write a temp file and rename it over the
	// placeholder. No DIRTY is ever sent (rename is invisible to the shim).
	tmp := e.abs(rel) + ".tmp"
	if err := os.WriteFile(tmp, user, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, e.abs(rel)); err != nil {
		t.Fatal(err)
	}

	// The next reader's ENSURE must detect the swap and preserve the save.
	if resp := request(t, e.sock, "ENSURE "+e.abs(rel)); resp != "OK" {
		t.Fatalf("ENSURE = %q", resp)
	}
	got, err := os.ReadFile(e.abs(rel))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, user) {
		t.Fatalf("silent data loss: ENSURE overwrote a rename-over save with %q", got)
	}
	if st, _ := e.table.Get(rel); st != StateLocal {
		t.Errorf("state = %q, want local", st)
	}
}

func TestEnsureDeletedPlaceholderPreservesDeletion(t *testing.T) {
	e := newEnv(t)
	const rel = "hello.txt"
	if err := os.Remove(e.abs(rel)); err != nil {
		t.Fatal(err)
	}
	if resp := request(t, e.sock, "ENSURE "+e.abs(rel)); resp != "OK" {
		t.Fatalf("ENSURE = %q", resp)
	}
	if _, err := os.Lstat(e.abs(rel)); !os.IsNotExist(err) {
		t.Error("ENSURE resurrected a file the user deleted")
	}
	if st, _ := e.table.Get(rel); st != StateLocal {
		t.Errorf("state = %q, want local", st)
	}
}

func TestEnsureUntrackedAndInvalidPaths(t *testing.T) {
	e := newEnv(t)

	// Untracked path under the root: not ours to answer; the caller's real
	// open() decides (ENOENT or a tool-created local file).
	if resp := request(t, e.sock, "ENSURE "+filepath.Join(e.root, "not-in-manifest.txt")); resp != "OK" {
		t.Errorf("untracked ENSURE = %q, want OK", resp)
	}

	for _, bad := range []string{
		"ENSURE /etc/passwd",
		"ENSURE " + filepath.Clean(filepath.Join(e.root, "..", "outside.txt")),
		"ENSURE relative/path.txt",
		"ENSURE " + e.root, // the root itself, not a file in it
		"ENSURE",
	} {
		if resp := request(t, e.sock, bad); !strings.HasPrefix(resp, "ERR ") {
			t.Errorf("%q = %q, want ERR", bad, resp)
		}
	}

	if resp := request(t, e.sock, "FROBNICATE /x"); !strings.HasPrefix(resp, "ERR ") {
		t.Errorf("unknown verb = %q, want ERR", resp)
	}
}

func TestMaterializeAll(t *testing.T) {
	e := newEnv(t)

	resp := request(t, e.sock, "MATERIALIZE_ALL")
	if want := fmt.Sprintf("OK files=%d", len(e.files)); resp != want {
		t.Fatalf("MATERIALIZE_ALL = %q, want %q", resp, want)
	}
	for rel, want := range e.files {
		got, err := os.ReadFile(e.abs(rel))
		if err != nil {
			t.Errorf("read %q: %v", rel, err)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%q differs after MATERIALIZE_ALL", rel)
		}
	}

	stats := request(t, e.sock, "STATS")
	if !strings.Contains(stats, "placeholder=0") || !strings.Contains(stats, fmt.Sprintf("materialized=%d", len(e.files))) {
		t.Errorf("STATS after MATERIALIZE_ALL = %q", stats)
	}
}

func TestStatsCounters(t *testing.T) {
	e := newEnv(t)

	request(t, e.sock, "ENSURE "+e.abs("hello.txt"))
	request(t, e.sock, "DIRTY "+e.abs("sub/other.txt"))

	stats := request(t, e.sock, "STATS")
	for _, want := range []string{"OK ", "ensures=1", "dirty=1", "materialized=1", "local=1",
		fmt.Sprintf("placeholder=%d", len(e.files)-2), "errors=0"} {
		if !strings.Contains(stats, want) {
			t.Errorf("STATS = %q, missing %q", stats, want)
		}
	}
}

func TestFillFailureIsLoudAndRetryable(t *testing.T) {
	e := newEnv(t)
	const rel = "sub/dir/big"

	// Break the store: hide one of the file's chunks.
	var hidden desync.ChunkID
	var hiddenData []byte
	for _, f := range e.manifest.Files {
		if f.Path == rel {
			hidden = desync.ChunkID(f.Chunks[len(f.Chunks)/2].Hash)
		}
	}
	hiddenData = e.store.chunks[hidden]
	delete(e.store.chunks, hidden)

	if resp := request(t, e.sock, "ENSURE "+e.abs(rel)); !strings.HasPrefix(resp, "ERR ") {
		t.Fatalf("ENSURE with missing chunk = %q, want ERR (fail loud)", resp)
	}
	if !e.table.IsTorn(rel) {
		t.Error("failed fill must mark the path torn for retry")
	}

	// Heal the store; the retry must re-fill (torn path skips the pristine
	// check — the file legitimately has partial content now).
	e.store.chunks[hidden] = hiddenData
	if resp := request(t, e.sock, "ENSURE "+e.abs(rel)); resp != "OK" {
		t.Fatalf("ENSURE retry = %q", resp)
	}
	got, err := os.ReadFile(e.abs(rel))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, e.files[rel]) {
		t.Fatal("content differs after torn retry")
	}
}

func TestRestartRecovery(t *testing.T) {
	// Build a full env, materialize one file, dirty another, then simulate a
	// crash mid-fill of a third. "Restart" = new table (journal replay), new
	// skeleton pass, new supervisor over the same workspace + state dir.
	files := fixtureContent()
	manifest, store := buildManifest(t, files)
	root := shortTempDir(t, "mshim-root-")
	stateDir := shortTempDir(t, "mshim-state-")
	journal := filepath.Join(stateDir, "journal.jsonl")
	buildTime := time.Now().Add(-1 * time.Hour).Truncate(time.Second)

	tab1, err := OpenTable(journal, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := BuildSkeleton(root, manifest, tab1, buildTime, quietLogger()); err != nil {
		t.Fatal(err)
	}
	sup1 := startSupervisor(t, root, stateDir, manifest, store, tab1, buildTime)

	if resp := request(t, sup1.SocketPath(), "ENSURE "+filepath.Join(sup1.Root(), "hello.txt")); resp != "OK" {
		t.Fatal(resp)
	}
	if resp := request(t, sup1.SocketPath(), "DIRTY "+filepath.Join(sup1.Root(), "sub/other.txt")); resp != "OK" {
		t.Fatal(resp)
	}
	userEdit := []byte("survives restarts")
	if err := os.WriteFile(filepath.Join(root, "sub/other.txt"), userEdit, 0o644); err != nil {
		t.Fatal(err)
	}
	// Crash mid-fill of big: intent journaled, partial bytes on disk.
	if err := tab1.SetMaterializing("sub/dir/big"); err != nil {
		t.Fatal(err)
	}
	bigPath := filepath.Join(root, "sub/dir/big")
	f, err := os.OpenFile(bigPath, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(files["sub/dir/big"][:10*1024]); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := sup1.Close(); err != nil {
		t.Fatal(err)
	}
	tab1.Close()

	// --- restart ---
	tab2, err := OpenTable(journal, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tab2.Close() })
	if _, err := BuildSkeleton(root, manifest, tab2, buildTime, quietLogger()); err != nil {
		t.Fatal(err)
	}
	sup2 := startSupervisor(t, root, stateDir, manifest, store, tab2, buildTime)

	// Materialized file: state survived, content intact, no re-fill needed.
	if st, _ := tab2.Get("hello.txt"); st != StateMaterialized {
		t.Errorf("hello.txt = %q after restart, want materialized", st)
	}
	// Local file: never clobbered, even though its journal entry is all that
	// protects it now.
	if resp := request(t, sup2.SocketPath(), "ENSURE "+filepath.Join(sup2.Root(), "sub/other.txt")); resp != "OK" {
		t.Fatal(resp)
	}
	got, err := os.ReadFile(filepath.Join(root, "sub/other.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, userEdit) {
		t.Fatalf("restart lost a local edit: %q", got)
	}
	// Torn file: replayed as torn, re-filled to correct content despite the
	// partial bytes (which would fail a naive pristine check).
	if !tab2.IsTorn("sub/dir/big") {
		t.Error("mid-fill crash must replay as torn")
	}
	if resp := request(t, sup2.SocketPath(), "ENSURE "+filepath.Join(sup2.Root(), "sub/dir/big")); resp != "OK" {
		t.Fatal(resp)
	}
	got, err = os.ReadFile(bigPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, files["sub/dir/big"]) {
		t.Fatal("torn file not re-filled correctly after restart")
	}
}

func TestSupervisorConfigValidation(t *testing.T) {
	if _, err := NewSupervisor(Config{}); err == nil {
		t.Error("empty config must be rejected")
	}
	m := &chunk.Manifest{}
	if _, err := NewSupervisor(Config{Root: "/nonexistent-root-xyz", SocketPath: "/tmp/x.sock", Manifest: m, Store: &memStore{}, Table: &Table{}}); err == nil {
		t.Error("nonexistent root must be rejected")
	}
}
