package recovery

import (
	"strings"
	"testing"
)

func TestVerifyBundleHeaderBytesDelta(t *testing.T) {
	record := storageTestRecord()
	header := func(lines ...string) []byte {
		return []byte(strings.Join(lines, "\n") + "\n\n")
	}
	for _, test := range []struct {
		name    string
		header  []byte
		wantErr bool
	}{
		{
			name: "single prerequisite matches base",
			header: header("# v3 git bundle", "@object-format=sha1",
				"-"+record.BaseSHA, record.SnapshotSHA+" "+record.Ref),
		},
		{
			name: "single prerequisite carries a log subject",
			header: header("# v3 git bundle", "@object-format=sha1",
				"-"+record.BaseSHA+" some commit subject", record.SnapshotSHA+" "+record.Ref),
		},
		{
			// #5103: a branch merged from its repeatedly-advancing base more
			// than once makes git declare every boundary commit, not just
			// BaseSHA, as its own prerequisite line.
			name: "multiple prerequisites including base",
			header: header("# v3 git bundle", "@object-format=sha1",
				"-"+strings.Repeat("1", 40)+" mainadv2",
				"-"+record.BaseSHA+" mainadv1",
				"-"+strings.Repeat("2", 40)+" base",
				record.SnapshotSHA+" "+record.Ref),
		},
		{
			name: "multiple prerequisites missing base is rejected",
			header: header("# v3 git bundle", "@object-format=sha1",
				"-"+strings.Repeat("1", 40),
				"-"+strings.Repeat("2", 40),
				record.SnapshotSHA+" "+record.Ref),
			wantErr: true,
		},
		{
			name: "malformed prerequisite line is rejected",
			header: header("# v3 git bundle", "@object-format=sha1",
				"-not-an-object-id",
				record.SnapshotSHA+" "+record.Ref),
			wantErr: true,
		},
		{
			name: "prerequisite line missing its leading dash is rejected",
			header: header("# v3 git bundle", "@object-format=sha1",
				record.BaseSHA,
				record.SnapshotSHA+" "+record.Ref),
			wantErr: true,
		},
		{
			name: "no prerequisite line at all is rejected",
			header: header("# v3 git bundle", "@object-format=sha1",
				record.SnapshotSHA+" "+record.Ref),
			wantErr: true,
		},
		{
			name: "wrong ref line is rejected",
			header: header("# v3 git bundle", "@object-format=sha1",
				"-"+record.BaseSHA, record.SnapshotSHA+" refs/heads/somewhere-else"),
			wantErr: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := verifyBundleHeaderBytes(test.header, record, archiveFormatDelta)
			if (err != nil) != test.wantErr {
				t.Fatalf("verifyBundleHeaderBytes() error = %v, wantErr %t", err, test.wantErr)
			}
		})
	}
}
