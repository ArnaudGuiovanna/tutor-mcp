package privatefs

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

func privateDescriptor(directory bool) (*windows.SECURITY_DESCRIPTOR, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	inherit := ""
	if directory {
		inherit = "OICI"
	}
	// Protected DACL: only this account; child files inherit the same boundary.
	return windows.SecurityDescriptorFromString("O:" + user.User.Sid.String() + "D:P(A;" + inherit + ";FA;;;" + user.User.Sid.String() + ")")
}
func checkOwner(path string) (*windows.SECURITY_DESCRIPTOR, error) {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return nil, err
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return nil, err
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	if !windows.EqualSid(owner, user.User.Sid) {
		// Files created by SQLite inherit the private DACL, but Windows uses
		// the token's default owner (which can be Administrators under UAC).
		// Accept only that exact process owner, then normalize private files
		// to the user's SID in applyPrivate. Other accounts remain rejected.
		var size uint32
		token := windows.GetCurrentProcessToken()
		err := windows.GetTokenInformation(token, windows.TokenOwner, nil, 0, &size)
		if err != windows.ERROR_INSUFFICIENT_BUFFER {
			return nil, fmt.Errorf("read process owner: %w", err)
		}
		buffer := make([]byte, size)
		if err := windows.GetTokenInformation(token, windows.TokenOwner, &buffer[0], size, &size); err != nil {
			return nil, err
		}
		defaultOwner := *(**windows.SID)(unsafe.Pointer(&buffer[0]))
		owned := windows.EqualSid(owner, defaultOwner)
		runtime.KeepAlive(buffer)
		if !owned {
			return nil, fmt.Errorf("path %q is not owned by the current user or process owner", path)
		}
	}
	return sd, nil
}
func applyPrivate(path string, directory bool) error {
	if _, err := checkOwner(path); err != nil {
		return err
	}
	sd, err := privateDescriptor(directory)
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, owner, nil, dacl, nil)
}
func protectNewDir(path string) error { return applyPrivate(path, true) }
func makePrivateDirs(path string) error {
	if info, err := os.Lstat(path); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("invalid private directory %q", path)
		}
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	parent := filepath.Dir(path)
	if parent == path {
		return fmt.Errorf("missing volume for private directory")
	}
	if err := makePrivateDirs(parent); err != nil {
		return err
	}
	sd, err := privateDescriptor(true)
	if err != nil {
		return err
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	sa := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	if err := windows.CreateDirectory(name, &sa); err != nil && !os.IsExist(err) {
		return err
	}
	return nil
}
func checkDir(path string, _ fs.FileInfo, private bool) error {
	sd, err := checkOwner(path)
	if err != nil || !private {
		return err
	}
	expected, err := privateDescriptor(true)
	if err != nil {
		return err
	}
	// Compare just the DACL: the primary group is irrelevant to access.
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	want, _, err := expected.DACL()
	if err != nil {
		return err
	}
	actualSD, err := windows.NewSecurityDescriptor()
	if err != nil {
		return err
	}
	if err = actualSD.SetDACL(dacl, true, false); err != nil {
		return err
	}
	wantSD, err := windows.NewSecurityDescriptor()
	if err != nil {
		return err
	}
	if err = wantSD.SetDACL(want, true, false); err != nil {
		return err
	}
	if actualSD.String() != wantSD.String() {
		return fmt.Errorf("directory %q requires a private owner-only ACL; use a new data directory", path)
	}
	return nil
}
func protectFile(path string, _ fs.FileInfo) error { return applyPrivate(path, false) }
func createFile(path string) (*os.File, error) {
	sd, err := privateDescriptor(false)
	if err != nil {
		return nil, err
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	sa := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	h, err := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, &sa, windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &os.PathError{Op: "create", Path: path, Err: err}
	}
	return os.NewFile(uintptr(h), path), nil
}
