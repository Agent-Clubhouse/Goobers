package providers

import "testing"

func TestValidateCommitRequest(t *testing.T) {
	tests := []struct {
		name string
		req  CommitRequest
		want string
	}{
		{name: "branch", req: CommitRequest{}, want: "branch is required"},
		{name: "message", req: CommitRequest{Branch: "work"}, want: "message is required"},
		{name: "files", req: CommitRequest{Branch: "work", Message: "change"}, want: "at least one file is required"},
		{name: "valid", req: CommitRequest{Branch: "work", Message: "change", Files: []CommitFile{{Path: "README.md"}}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateCommitRequest(test.req)
			if test.want == "" {
				if err != nil {
					t.Fatalf("validateCommitRequest: %v", err)
				}
				return
			}
			if err == nil || err.Error() != test.want {
				t.Fatalf("validateCommitRequest error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestValidateCommitFile(t *testing.T) {
	if err := validateCommitFile(CommitFile{}); err == nil || err.Error() != "file path is required" {
		t.Fatalf("validateCommitFile error = %v, want file path is required", err)
	}
	if err := validateCommitFile(CommitFile{Path: "README.md"}); err != nil {
		t.Fatalf("validateCommitFile valid file: %v", err)
	}
}
