//go:build linux && rootintegration && protocolacceptance

package root_test

import (
	"crypto/sha256"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

type protocolSourceMode struct {
	name           string
	followSymlinks bool
}

func protocolSourceModes() []protocolSourceMode {
	return []protocolSourceMode{
		{name: "default_omits_source_symlinks"},
		{name: "transferlanes_follow_symlinks_expands_logical_paths", followSymlinks: true},
	}
}

type protocolEntryKind uint8

const (
	protocolRegularFile protocolEntryKind = iota + 1
	protocolEmptyDirectory
)

type protocolEntry struct {
	kind   protocolEntryKind
	size   int64
	sha256 [sha256.Size]byte
}

type protocolSourceTree struct {
	defaultEntries  map[string]protocolEntry
	followedEntries map[string]protocolEntry
}

func createProtocolSourceTree(t *testing.T, scenario supervisorRootScenario) protocolSourceTree {
	t.Helper()
	source := os.Getenv(supervisorTransferEnv)
	if err := os.Mkdir(filepath.Join(source, "empty-source-directory"), 0o700); err != nil {
		t.Fatal(err)
	}
	externalFile := filepath.Join(scenario.markers, "external-file-target")
	if err := os.WriteFile(externalFile, []byte("external-file"), 0o600); err != nil {
		t.Fatal(err)
	}
	externalDirectory := filepath.Join(scenario.markers, "external-directory-target")
	if err := os.MkdirAll(filepath.Join(externalDirectory, "empty-target-directory"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(externalDirectory, "outside"),
		[]byte("external-directory-file"), 0o600); err != nil {
		t.Fatal(err)
	}
	links := map[string]string{
		"internal-file":      "alpha",
		"internal-directory": "nested",
		"external-file":      externalFile,
		"external-directory": externalDirectory,
	}
	for name, target := range links {
		if err := os.Symlink(target, filepath.Join(source, name)); err != nil {
			t.Fatal(err)
		}
	}

	defaultEntries := protocolEntriesFromContents(transferSourceContents())
	defaultEntries["empty-source-directory"] = protocolEntry{kind: protocolEmptyDirectory}
	followedEntries := maps.Clone(defaultEntries)
	for path, contents := range map[string]string{
		"internal-file":              "a",
		"internal-directory/echo":    "eeeee",
		"internal-directory/foxtrot": "ffffff",
		"external-file":              "external-file",
		"external-directory/outside": "external-directory-file",
	} {
		followedEntries[path] = protocolFileEntry(contents)
	}
	followedEntries["external-directory/empty-target-directory"] =
		protocolEntry{kind: protocolEmptyDirectory}
	return protocolSourceTree{defaultEntries: defaultEntries, followedEntries: followedEntries}
}

func protocolEntriesFromContents(contents map[string]string) map[string]protocolEntry {
	result := make(map[string]protocolEntry, len(contents))
	for path, content := range contents {
		result[path] = protocolFileEntry(content)
	}
	return result
}

func protocolFileEntry(contents string) protocolEntry {
	return protocolEntry{kind: protocolRegularFile, size: int64(len(contents)),
		sha256: sha256.Sum256([]byte(contents))}
}

func pathSHA256(path string) ([sha256.Size]byte, error) {
	content, err := os.ReadFile(path)
	return sha256.Sum256(content), err
}

func (tree protocolSourceTree) entries(mode protocolSourceMode) map[string]protocolEntry {
	if mode.followSymlinks {
		return tree.followedEntries
	}
	return tree.defaultEntries
}

func (tree protocolSourceTree) summary(mode protocolSourceMode) string {
	if mode.followSymlinks {
		return "followed_symlinks=4 ignored_symlinks=0 empty_directories=2 ignored_special_files=0"
	}
	return "followed_symlinks=0 ignored_symlinks=4 empty_directories=1 ignored_special_files=0"
}

func assertProtocolSourceSummary(t *testing.T, output string, tree protocolSourceTree,
	mode protocolSourceMode,
) {
	t.Helper()
	want := tree.summary(mode)
	if count := strings.Count(output, want); count != 1 {
		t.Fatalf("source summary occurrence count=%d, want 1: %q", count, output)
	}
}

func protocolRegularEntries(entries map[string]protocolEntry) map[string]protocolEntry {
	result := make(map[string]protocolEntry)
	for path, entry := range entries {
		if entry.kind == protocolRegularFile {
			result[path] = entry
		}
	}
	return result
}

func describeProtocolEntry(entry protocolEntry) string {
	switch entry.kind {
	case protocolRegularFile:
		return fmt.Sprintf("file:size=%d:sha256=%x", entry.size, entry.sha256)
	case protocolEmptyDirectory:
		return "empty-directory"
	default:
		return fmt.Sprintf("unknown:%d", entry.kind)
	}
}

func describeProtocolManifest(entries map[string]protocolEntry) string {
	paths := make([]string, 0, len(entries))
	for path := range entries {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	var result strings.Builder
	for index, path := range paths {
		if index > 0 {
			result.WriteString(", ")
		}
		result.WriteString(path)
		result.WriteByte('=')
		result.WriteString(describeProtocolEntry(entries[path]))
	}
	return result.String()
}
