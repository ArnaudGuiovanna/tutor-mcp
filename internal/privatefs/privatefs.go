// Package privatefs protects local learner data and keys before they are opened.
package privatefs

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

func PrepareDir(path string, private bool) error {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		if err = makePrivateDirs(path); err != nil {
			return err
		}
		if err = protectNewDir(path); err != nil {
			return err
		}
		info, err = os.Lstat(path)
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("directory %q must be a real directory, not a symlink", path)
	}
	return checkDir(path, info, private)
}

func SecureFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("file %q must be a regular file, not a symlink", path)
	}
	return protectFile(path, info)
}

// CreateFile uses exclusive creation and applies permissions before any bytes
// are written. Callers must not replace an existing secret on restart.
func CreateFile(path string) (*os.File, error) { return createFile(path) }

// CreateTemp creates a private file with an explicit owner on Windows, too.
// Filename entropy is independent of any secret that the caller will write.
func CreateTemp(dir, prefix string) (*os.File, error) {
	if filepath.Base(prefix) != prefix || prefix == "." || prefix == ".." {
		return nil, fmt.Errorf("invalid private temporary file prefix")
	}
	for range 10 {
		var suffix [16]byte
		if _, err := rand.Read(suffix[:]); err != nil {
			return nil, err
		}
		file, err := CreateFile(filepath.Join(dir, prefix+hex.EncodeToString(suffix[:])))
		if !errors.Is(err, fs.ErrExist) {
			return file, err
		}
	}
	return nil, fmt.Errorf("cannot allocate private temporary file: %w", fs.ErrExist)
}
