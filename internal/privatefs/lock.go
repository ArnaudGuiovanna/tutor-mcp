package privatefs

import (
	"context"
	"errors"
	"os"
	"time"
)

// Lock is process-wide coordination released by the OS even after a crash.
// The lock file remains on disk; deleting it would allow two independent locks.
func Lock(ctx context.Context, path string) (func(), error) {
	f, err := CreateFile(path)
	if errors.Is(err, os.ErrExist) {
		if err = SecureFile(path); err != nil {
			return nil, err
		}
		f, err = os.OpenFile(path, os.O_RDWR, 0600)
	}
	if err != nil {
		return nil, err
	}
	for {
		ok, err := tryLock(f)
		if err != nil {
			f.Close()
			return nil, err
		}
		if ok {
			return func() { _ = unlock(f); _ = f.Close() }, nil
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}
