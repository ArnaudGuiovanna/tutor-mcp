//go:build !windows

package privatefs

import (
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

func owned(info fs.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) == os.Geteuid()
}
func protectNewDir(path string) error   { return os.Chmod(path, 0700) }
func makePrivateDirs(path string) error { return os.MkdirAll(path, 0700) }
func checkDir(path string, info fs.FileInfo, private bool) error {
	if !owned(info) {
		return fmt.Errorf("directory %q is not owned by the current user", path)
	}
	if private && info.Mode().Perm() != 0700 {
		return fmt.Errorf("directory %q permissions are %04o; require 0700", path, info.Mode().Perm())
	}
	return nil
}
func protectFile(path string, info fs.FileInfo) error {
	if !owned(info) {
		return fmt.Errorf("file %q is not owned by the current user", path)
	}
	return os.Chmod(path, 0600)
}
func createFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
}
