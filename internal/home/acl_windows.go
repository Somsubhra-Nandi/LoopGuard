package home

import "golang.org/x/sys/windows"

// restrictToUser replaces the file's ACL with one entry granting the
// current user full access, and stops inheriting entries from the parent
// directory. File modes don't protect files on Windows.
func restrictToUser(path string) error {
	sid, err := currentUserSID()
	if err != nil {
		return err
	}
	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       windows.NO_INHERITANCE,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_USER,
			TrusteeValue: windows.TrusteeValueFromSID(sid),
		},
	}}, nil)
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, acl, nil)
}

func currentUserSID() (*windows.SID, error) {
	// A pseudo-handle: nothing to close.
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	// The SID points into the returned buffer; return a copy.
	return u.User.Sid.Copy()
}
