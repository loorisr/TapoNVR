package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// storage.max_size_gb prunes the oldest unlocked detections until the
// recordings fit; locked ones survive (and still count toward the total).
func TestDiskGuardMaxSize(t *testing.T) {
	store, err := openDB(filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{}
	configDefaults(&cfg)
	cfg.Server.DataDir = t.TempDir()
	cfg.Storage.MaxSizeGB = 0.0025 // 2.5 MB
	app := &App{cfg: &cfg, db: store}
	dir := filepath.Join(cfg.Server.DataDir, "detections", "2026-11-20", "cam0")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	var base = time.Now().Unix() - 24*3600
	mk := func(i int) string {
		var d Detection
		d.id = newID()
		d.cameraID, d.camName, d.detType = "cam0", "c", "vehicle"
		d.startedAt = base + int64(i)*600
		d.endedAt = d.startedAt + 120
		d.clipPath = filepath.Join(dir, d.id+".mp4")
		if err := os.WriteFile(d.clipPath, make([]byte, 1_000_000), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := dbInsertDetection(store, &d); err != nil {
			t.Fatal(err)
		}
		return d.id
	}
	locked, d2, d3, d4 := mk(0), mk(1), mk(2), mk(3) // 4 MB total
	if err := dbSetLocked(store, locked, true); err != nil {
		t.Fatal(err)
	}
	diskGuardPrune(app)
	for _, c := range []struct {
		id   string
		keep bool
	}{{locked, true}, {d2, false}, {d3, false}, {d4, true}} {
		got, _ := getDetection(store, c.id)
		if (got != nil) != c.keep {
			t.Fatalf("detection %s: kept=%v, want %v", c.id, got != nil, c.keep)
		}
	}
	if sz := recordingsSizeBytes(app); sz != 2_000_000 {
		t.Fatalf("recordings size after prune = %d, want 2000000", sz)
	}
	// Under the limit: nothing more is removed.
	diskGuardPrune(app)
	if got, _ := getDetection(store, d4); got == nil {
		t.Fatal("newest detection pruned while under the limit")
	}
}
