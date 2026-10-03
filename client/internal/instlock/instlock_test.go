package instlock

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestAcquireExclusive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "serve.lock")

	release, pid, err := Acquire(path)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if pid != os.Getpid() {
		t.Fatalf("pid = %d, want %d", pid, os.Getpid())
	}

	_, holder, err := Acquire(path)
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("second acquire err = %v, want ErrLocked", err)
	}
	if holder != os.Getpid() {
		t.Fatalf("holder pid = %d, want %d", holder, os.Getpid())
	}

	release()

	release2, _, err := Acquire(path)
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	release2()
}

func TestStaleLockIsReclaimed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "serve.lock")
	// 1 进程必然不存在：留下的锁是陈旧锁，必须能被下一个实例拿走
	if err := os.WriteFile(path, []byte(`{"pid":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	release, _, err := Acquire(path)
	if err != nil {
		t.Fatalf("stale lock not reclaimed: %v", err)
	}
	defer release()
}

func TestStaleLockBarePID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "serve.lock")
	if err := os.WriteFile(path, []byte(strconv.Itoa(1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Acquire(path); err != nil {
		t.Fatalf("bare-pid stale lock not reclaimed: %v", err)
	}
}
