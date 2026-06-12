package shim

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/suman724/mirage/internal/chunk"
)

func newTable(t *testing.T) *Table {
	t.Helper()
	tab, err := OpenTable(filepath.Join(t.TempDir(), "journal.jsonl"), quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tab.Close() })
	return tab
}

func TestBuildSkeleton(t *testing.T) {
	files := fixtureContent()
	manifest, _ := buildManifest(t, files)
	root := t.TempDir()
	tab := newTable(t)
	buildTime := time.Now().Add(-2 * time.Hour).Truncate(time.Second)

	res, err := BuildSkeleton(root, manifest, tab, buildTime, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	if res.Files != len(files) || res.Created != len(files) {
		t.Errorf("res = %+v, want %d files all created", res, len(files))
	}

	for rel, content := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		fi, err := os.Lstat(path)
		if err != nil {
			t.Errorf("placeholder %q missing: %v", rel, err)
			continue
		}
		if fi.Size() != int64(len(content)) {
			t.Errorf("%q apparent size = %d, want %d", rel, fi.Size(), len(content))
		}
		if got := blocksOf(t, path); got != 0 {
			t.Errorf("%q allocated %d blocks, want fully sparse", rel, got)
		}
		if !fi.ModTime().Truncate(time.Second).Equal(buildTime) {
			t.Errorf("%q mtime = %v, want %v", rel, fi.ModTime(), buildTime)
		}
		if fi.Mode().Perm() != 0o644 {
			t.Errorf("%q mode = %v, want 0644", rel, fi.Mode().Perm())
		}
		if st, ok := tab.Get(rel); !ok || st != StatePlaceholder {
			t.Errorf("%q state = %q/%v, want placeholder", rel, st, ok)
		}
		if m, ok := tab.MarkerFor(rel); !ok || m.Size != int64(len(content)) {
			t.Errorf("%q marker = %+v/%v, want size %d", rel, m, ok, len(content))
		}
	}
}

func TestBuildSkeletonAppliesManifestMode(t *testing.T) {
	manifest := &chunk.Manifest{}
	refs, _, err := chunk.Split([]byte("#!/bin/sh\necho hi\n"))
	if err != nil {
		t.Fatal(err)
	}
	manifest.Files = append(manifest.Files, chunk.FileEntry{Path: "run.sh", Mode: 0o755, Chunks: refs})

	root := t.TempDir()
	if _, err := BuildSkeleton(root, manifest, newTable(t), time.Now(), quietLogger()); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(filepath.Join(root, "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o755 {
		t.Errorf("mode = %v, want 0755", fi.Mode().Perm())
	}
}

func TestBuildSkeletonRejectsTraversal(t *testing.T) {
	for _, evil := range []string{"../evil.txt", "a/../../evil.txt", "/abs.txt"} {
		manifest := &chunk.Manifest{Files: []chunk.FileEntry{{Path: evil}}}
		if _, err := BuildSkeleton(t.TempDir(), manifest, newTable(t), time.Now(), quietLogger()); err == nil {
			t.Errorf("manifest path %q must be rejected", evil)
		}
	}
}

func TestBuildSkeletonIdempotent(t *testing.T) {
	files := fixtureContent()
	manifest, _ := buildManifest(t, files)
	root := t.TempDir()
	tab := newTable(t)
	buildTime := time.Now()

	if _, err := BuildSkeleton(root, manifest, tab, buildTime, quietLogger()); err != nil {
		t.Fatal(err)
	}

	// Simulate restart-over-persistent-workspace: one file now holds real
	// (user) content. The rebuild must not touch it.
	edited := filepath.Join(root, "hello.txt")
	if err := os.WriteFile(edited, []byte("user edit"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := BuildSkeleton(root, manifest, tab, buildTime, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	if res.Created != 0 {
		t.Errorf("rebuild created %d placeholders over an existing tree", res.Created)
	}
	got, err := os.ReadFile(edited)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "user edit" {
		t.Errorf("rebuild clobbered existing content: %q", got)
	}
}

func TestBuildSkeletonRefusesNonRegularCollision(t *testing.T) {
	manifest := &chunk.Manifest{Files: []chunk.FileEntry{{Path: "x"}}}
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := BuildSkeleton(root, manifest, newTable(t), time.Now(), quietLogger()); err == nil {
		t.Error("a directory where the manifest expects a file must fail loudly")
	}
}
