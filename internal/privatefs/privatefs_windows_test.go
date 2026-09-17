package privatefs

import (
	"golang.org/x/sys/windows"
	"os"
	"path/filepath"
	"testing"
)

func TestWindowsRejectsSharedACLAndProtectsInheritedFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private")
	if err := PrepareDir(dir, true); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "sqlite-wal")
	if err := os.WriteFile(path, []byte("private data"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := checkOwner(path); err != nil {
		t.Fatal(err)
	}
	if err := SecureFile(path); err != nil {
		t.Fatal(err)
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;WD)")
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
	if err := PrepareDir(dir, true); err == nil {
		t.Fatal("shared parent accepted")
	}
	// Restore access for cleanup without using the rejected parent for data.
	if err := applyPrivate(dir, true); err != nil {
		t.Fatal(err)
	}
}
