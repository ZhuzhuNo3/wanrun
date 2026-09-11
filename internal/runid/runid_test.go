package runid_test

import (
	"testing"

	"github.com/ZhuzhuNo3/transferlanes/internal/runid"
)

func TestNewProducesUniqueRoundTrippableDirectoryNames(t *testing.T) {
	t.Parallel()

	seen := make(map[runid.ID]struct{}, 128)
	for range 128 {
		id, err := runid.New()
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if _, duplicate := seen[id]; duplicate {
			t.Fatalf("New returned duplicate ID %q", id)
		}
		seen[id] = struct{}{}

		parsed, err := runid.Parse(id.String())
		if err != nil {
			t.Fatalf("Parse(New().String()): %v", err)
		}
		if parsed != id {
			t.Fatalf("round trip = %q, want %q", parsed, id)
		}
	}
}

func TestParseRejectsUnsafeOrNonCanonicalNames(t *testing.T) {
	t.Parallel()

	for _, value := range []string{
		"",
		"/",
		"../0123456789abcdef0123456789abcdef",
		"0123456789abcdef0123456789abcde",
		"0123456789abcdef0123456789abcdef0",
		"0123456789abcdef0123456789abcdeg",
		"0123456789ABCDEF0123456789ABCDEF",
		"00000000000000000000000000000000",
	} {
		if _, err := runid.Parse(value); err == nil {
			t.Errorf("Parse(%q) succeeded", value)
		}
	}
}
