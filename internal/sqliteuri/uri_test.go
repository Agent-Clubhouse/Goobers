package sqliteuri

import (
	"net/url"
	"path/filepath"
	"testing"
)

func TestFileURIRootsDrivePathsWithoutAnAuthority(t *testing.T) {
	for _, test := range []struct{ path, want string }{
		{"D:/instance/accepted.db", "file:///D:/instance/accepted.db"},
		{"/var/lib/accepted.db", "file:///var/lib/accepted.db"},
		{"D:/space #?%/receipt.db", "file:///D:/space%20%23%3F%25/receipt.db"},
		{"//server/share/receipt.db", "file:////server/share/receipt.db"},
	} {
		if got := File(test.path); got != test.want {
			t.Errorf("File(%q)=%q, want %q", test.path, got, test.want)
		}
	}
	uri, err := url.Parse(File(filepath.Join(t.TempDir(), "native.db")))
	if err != nil || uri.Host != "" || uri.Scheme != "file" {
		t.Fatalf("native file URI has invalid authority: %v %v", uri, err)
	}
}
