package agent

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStateRoundTripAndPerms(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	st := &State{
		ConsoleURL: "http://127.0.0.1:7700", NodeID: 7, Name: "mac-mini",
		Role: "workstation", NodeToken: "tok", RegisteredAt: 12345,
	}
	if err := SaveState(path, st); err != nil {
		t.Fatalf("save: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("state perm = %o, want 600", perm)
	}
	got, err := LoadState(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.NodeID != 7 || got.NodeToken != "tok" || got.Name != "mac-mini" {
		t.Fatalf("round trip mismatch: %+v", got)
	}
	// 不残留临时文件（os.CreateTemp 命名含随机后缀，须 glob 覆盖）。
	if matches, _ := filepath.Glob(path + ".tmp*"); len(matches) != 0 {
		t.Fatalf("tmp file left behind: %v", matches)
	}
}

// TestSaveStateOverwrite 覆盖写：权限保持 0600、无临时文件残留（审查 R1-#12）。
func TestSaveStateOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	first := &State{ConsoleURL: "http://a", NodeID: 1, Name: "n", NodeToken: "t1"}
	if err := SaveState(path, first); err != nil {
		t.Fatalf("first save: %v", err)
	}
	second := &State{ConsoleURL: "http://b", NodeID: 2, Name: "m", NodeToken: "t2"}
	if err := SaveState(path, second); err != nil {
		t.Fatalf("overwrite save: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("state perm after overwrite = %o, want 600", perm)
	}
	got, err := LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.NodeToken != "t2" || got.NodeID != 2 {
		t.Fatalf("overwrite mismatch: %+v", got)
	}
	if matches, _ := filepath.Glob(path + ".tmp*"); len(matches) != 0 {
		t.Fatalf("tmp file left behind: %v", matches)
	}
}

func TestLoadStateMissing(t *testing.T) {
	if _, err := LoadState(filepath.Join(t.TempDir(), "nope.json")); err != ErrNotRegistered {
		t.Fatalf("err = %v, want ErrNotRegistered", err)
	}
}

func TestLoadStateIncomplete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(`{"console_url":"http://x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadState(path); err == nil {
		t.Fatal("incomplete state should be rejected")
	}
}

// TestJitterRange 抖动区间 [d/2, d)：封顶后重试间隔永不超 backoffMax（审查 R1-#10）。
func TestJitterRange(t *testing.T) {
	d := 60 * time.Second
	for range 200 {
		j := jitter(d)
		if j < d/2 || j >= d {
			t.Fatalf("jitter = %v out of [%v, %v)", j, d/2, d)
		}
	}
}
