package buildinfo

import "testing"

func TestFormatBuildIdentity(t *testing.T) {
	for _, test := range []struct {
		name     string
		release  string
		commit   string
		modified string
		want     string
	}{
		{name: "ordinary release", release: "76312e322e33", want: "transferlanes v1.2.3\n"},
		{name: "option-shaped release", release: "2d2d70726576696577", want: "transferlanes --preview\n"},
		{name: "release punctuation and spaces", release: "72656c6561736520225c2220e78988e69cac", want: `transferlanes release "\" 版本` + "\n"},
		{name: "clean development", commit: "0123456789abcdef0123456789abcdef01234567", modified: "false", want: "transferlanes devel commit=0123456789abcdef0123456789abcdef01234567 modified=false\n"},
		{name: "modified development", commit: "0123456789abcdef0123456789abcdef01234567", modified: "true", want: "transferlanes devel commit=0123456789abcdef0123456789abcdef01234567 modified=true\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := format(test.release, test.commit, test.modified)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("format() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestFormatRejectsIncompleteOrUnsafeBuildIdentity(t *testing.T) {
	for _, test := range []struct {
		name     string
		release  string
		commit   string
		modified string
	}{
		{name: "missing identity"},
		{name: "mixed identity", release: "7631", commit: "0123456789abcdef0123456789abcdef01234567", modified: "false"},
		{name: "odd release transport", release: "1"},
		{name: "uppercase release transport", release: "AB"},
		{name: "invalid release transport", release: "gg"},
		{name: "invalid UTF-8 release", release: "ff"},
		{name: "empty release", release: ""},
		{name: "release control", release: "610a62"},
		{name: "release line separator", release: "e280a8"},
		{name: "release paragraph separator", release: "e280a9"},
		{name: "abbreviated commit", commit: "0123456", modified: "false"},
		{name: "non-hex commit", commit: "z123456789abcdef0123456789abcdef01234567", modified: "false"},
		{name: "uppercase commit", commit: "0123456789ABCDEF0123456789abcdef01234567", modified: "false"},
		{name: "missing modified", commit: "0123456789abcdef0123456789abcdef01234567"},
		{name: "invalid modified", commit: "0123456789abcdef0123456789abcdef01234567", modified: "yes"},
		{name: "missing commit", modified: "false"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := format(test.release, test.commit, test.modified); err == nil {
				t.Fatal("format unexpectedly succeeded")
			}
		})
	}
}
