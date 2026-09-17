package db

import (
	"os"
	"path/filepath"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestWindowsSQLitePathAndSidecarPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "runtime #1.db")
	if err := PreparePrivateSQLitePath(path); err != nil {
		t.Fatal(err)
	}
	database, err := OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.Exec(`CREATE TABLE probe (value TEXT); INSERT INTO probe VALUES ('persisted')`); err != nil {
		t.Fatal(err)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range []string{path, path + "-wal", path + "-shm"} {
		if _, err := os.Stat(candidate); err != nil {
			t.Fatal(err)
		}
		// Check inherited access before tightening: SQLite-created sidecars
		// must never briefly grant another account access to learner data.
		sd, err := windows.GetNamedSecurityInfo(candidate, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			t.Fatal(err)
		}
		dacl, _, err := sd.DACL()
		if err != nil || dacl == nil || dacl.AceCount != 1 {
			t.Fatalf("%s: not a single owner-only ACE: %v", candidate, err)
		}
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, 0, &ace); err != nil {
			t.Fatal(err)
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || !windows.EqualSid((*windows.SID)(unsafe.Pointer(&ace.SidStart)), user.User.Sid) {
			t.Fatalf("%s: another principal has access", candidate)
		}
	}
	if err := SecureSQLiteFiles(path); err != nil {
		t.Fatal(err)
	}
	for _, candidate := range []string{path, path + "-wal", path + "-shm"} {
		sd, err := windows.GetNamedSecurityInfo(candidate, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
		if err != nil {
			t.Fatal(err)
		}
		owner, _, err := sd.Owner()
		if err != nil || !windows.EqualSid(owner, user.User.Sid) {
			t.Fatalf("%s: owner is not the user: %v", candidate, err)
		}
	}
	ro, err := OpenDBReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	var value string
	if err := ro.QueryRow(`SELECT value FROM probe`).Scan(&value); err != nil || value != "persisted" {
		t.Fatalf("file URI round trip: %q %v", value, err)
	}
}
