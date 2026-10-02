//go:build windows

package fleet

import (
	"bytes"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestPlatformStorageBaseDirUsesLocalAppDataKnownFolder(t *testing.T) {
	want := filepath.Join(t.TempDir(), "Local")
	original := knownFolderPath
	t.Cleanup(func() { knownFolderPath = original })

	var requested windows.KNOWNFOLDERID
	var flags uint32
	knownFolderPath = func(folderID *windows.KNOWNFOLDERID, requestedFlags uint32) (string, error) {
		requested = *folderID
		flags = requestedFlags
		return want, nil
	}
	t.Setenv("APPDATA", filepath.Join(t.TempDir(), "OneDrive", "Roaming"))
	t.Setenv("LOCALAPPDATA", filepath.Join(t.TempDir(), "wrong"))

	got, err := platformStorageBaseDir()
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("platformStorageBaseDir = %q, want %q", got, want)
	}
	if requested != *windows.FOLDERID_LocalAppData {
		t.Fatalf("requested known folder = %v, want LocalAppData", requested)
	}
	if flags != windows.KF_FLAG_DEFAULT {
		t.Fatalf("known-folder flags = %d, want %d", flags, windows.KF_FLAG_DEFAULT)
	}
}

// TestDPAPIProtectUnprotectRoundTrip exercises the real CryptProtectData /
// CryptUnprotectData round trip (#4258): no stub, so a bad flag, a DataBlob
// lifetime bug, or LocalFree misuse in cryptData fails here instead of in the
// field.
func TestDPAPIProtectUnprotectRoundTrip(t *testing.T) {
	large := bytes.Repeat([]byte("fleet-secret-"), 4096)
	cases := map[string][]byte{
		"single byte": {0x01},
		"credential":  []byte("bearer-secret"),
		"binary":      {0x00, 0xff, 0x00, 0x7f, 0x80, 0x00},
		"large":       large,
	}
	for name, plaintext := range cases {
		t.Run(name, func(t *testing.T) {
			original := append([]byte(nil), plaintext...)
			ciphertext, err := protect(plaintext)
			if err != nil {
				t.Fatalf("protect: %v", err)
			}
			if !bytes.Equal(plaintext, original) {
				t.Fatal("protect mutated its input")
			}
			if len(ciphertext) <= len(plaintext) || bytes.Contains(ciphertext, plaintext) {
				t.Fatalf("protect returned %d bytes that do not look encrypted", len(ciphertext))
			}
			got, err := unprotect(ciphertext)
			if err != nil {
				t.Fatalf("unprotect: %v", err)
			}
			if !bytes.Equal(got, original) {
				t.Fatalf("unprotect(protect(x)) = %d bytes, want the original %d bytes", len(got), len(original))
			}
		})
	}
}

func TestDPAPIUnprotectRejectsCiphertextItDidNotProduce(t *testing.T) {
	ciphertext, err := protect([]byte("bearer-secret"))
	if err != nil {
		t.Fatalf("protect: %v", err)
	}
	tampered := append([]byte(nil), ciphertext...)
	tampered[len(tampered)-1] ^= 0xff
	truncated := ciphertext[:len(ciphertext)/2]

	cases := map[string][]byte{
		"garbage":   []byte("this was never produced by CryptProtectData"),
		"plaintext": []byte("bearer-secret"),
		"tampered":  tampered,
		"truncated": truncated,
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			if got, err := unprotect(input); err == nil {
				t.Fatalf("unprotect accepted %s input and returned %d bytes", name, len(got))
			}
		})
	}
}

func TestDPAPIRejectsEmptyInput(t *testing.T) {
	if _, err := protect(nil); err == nil {
		t.Fatal("protect(nil) succeeded, want error")
	}
	if _, err := unprotect([]byte{}); err == nil {
		t.Fatal("unprotect(empty) succeeded, want error")
	}
}
