package privatefs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPrivateProfileFilesAndLock(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private")
	if err := PrepareDir(dir, true); err != nil {
		t.Fatal(err)
	}
	if err := PrepareDir(dir, true); err != nil {
		t.Fatal("private directory not reusable:", err)
	}
	tmp, err := CreateTemp(dir, ".keys-")
	if err != nil {
		t.Fatal(err)
	}
	if err = tmp.Close(); err != nil {
		t.Fatal(err)
	}
	if err = SecureFile(tmp.Name()); err != nil {
		t.Fatal("temporary key file ownership:", err)
	}
	path := filepath.Join(dir, "key")
	f, err := CreateFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.WriteString("private key"); err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	if err = SecureFile(path); err != nil {
		t.Fatal(err)
	}
	if other, err := CreateFile(path); !errors.Is(err, os.ErrExist) {
		if other != nil {
			other.Close()
		}
		t.Fatalf("exclusive creation: %v", err)
	}
	lockPath := filepath.Join(dir, "startup.lock")
	release, err := Lock(context.Background(), lockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if releaseOther, err := Lock(ctx, lockPath); !errors.Is(err, context.DeadlineExceeded) {
		if releaseOther != nil {
			releaseOther()
		}
		t.Fatalf("competing lock: %v", err)
	}
}
