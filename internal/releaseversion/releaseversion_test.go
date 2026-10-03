package releaseversion

import "testing"

func TestParse(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		value   string
		opts    Options
		want    Version
		wantErr string
	}{
		{
			name:  "valid version",
			value: "v12.34.56",
			want:  Version{Major: 12, Minor: 34, Patch: 56},
		},
		{
			name:    "leading whitespace",
			value:   " v1.2.3",
			wantErr: "must use vMAJOR.MINOR.PATCH",
		},
		{
			name:    "trailing whitespace",
			value:   "v1.2.3\n",
			wantErr: "must use vMAJOR.MINOR.PATCH",
		},
		{
			name:    "missing v",
			value:   "1.2.3",
			wantErr: "must use vMAJOR.MINOR.PATCH",
		},
		{
			name:    "too few parts",
			value:   "v1.2",
			wantErr: "must use vMAJOR.MINOR.PATCH",
		},
		{
			name:    "too many parts",
			value:   "v1.2.3.4",
			wantErr: "must use vMAJOR.MINOR.PATCH",
		},
		{
			name:    "leading zero",
			value:   "v1.02.3",
			wantErr: "must use canonical vMAJOR.MINOR.PATCH",
		},
		{
			name:    "empty part",
			value:   "v1..3",
			wantErr: "must use canonical vMAJOR.MINOR.PATCH",
		},
		{
			name:    "bad digits",
			value:   "v1.two.3",
			wantErr: "must use vMAJOR.MINOR.PATCH",
		},
		{
			name:    "overflow",
			value:   "v18446744073709551616.2.3",
			wantErr: "must use vMAJOR.MINOR.PATCH",
		},
		{
			name:  "allowed development sentinel",
			value: "dev",
			opts: Options{
				DevelopmentSentinel: "dev",
				AllowDevelopment:    true,
			},
			want: Version{Development: true},
		},
		{
			name:  "disallowed development sentinel",
			value: "dev",
			opts: Options{
				DevelopmentSentinel: "dev",
			},
			wantErr: `"dev" is only valid for the initial pre-release baseline`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got, err := Parse(test.value, test.opts)
			if test.wantErr != "" {
				if err == nil || err.Error() != test.wantErr {
					t.Fatalf("Parse(%q) error = %v, want %q", test.value, err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse(%q) error = %v", test.value, err)
			}
			if got != test.want {
				t.Errorf("Parse(%q) = %+v, want %+v", test.value, got, test.want)
			}
		})
	}
}
