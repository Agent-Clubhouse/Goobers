package sqliteuri

import (
	"net/url"
	"path/filepath"
	"strings"
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

func TestFromPathRejectsUnusablePathsAndEscapes(t *testing.T) {
	for _, path := range []string{"", "/tmp/db?foo=1"} {
		if got := FromPath(path); got != "" {
			t.Errorf("FromPath(%q) = %q, want empty", path, got)
		}
	}
	got := FromPath(filepath.Join(t.TempDir(), "my instance", "x.db"))
	if got == "" || got[:8] != "file:///" || got == "file:///" {
		t.Fatalf("FromPath = %q, want rooted file URI", got)
	}
	if want := "%20"; !strings.Contains(got, want) {
		t.Errorf("FromPath left an unescaped space: %q", got)
	}
}
