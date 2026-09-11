package sourcefiles

import (
	"errors"
	"fmt"
	"iter"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type sourceObjectKind uint8

const (
	sourceDirectoryObject sourceObjectKind = iota + 1
	sourceRegularObject
	sourceSymlinkObject
	sourceSpecialObject
)

type sourceIdentity struct {
	device uint64
	inode  uint64
	kind   sourceObjectKind
}

type sourceCapability struct{ identity byte }

// ErrNoTransferItems identifies a source with neither a regular file nor a logical empty directory.
var ErrNoTransferItems = errors.New("source contains no transferable files or empty directories")

// BackingReference names an object relative to one descriptor owned by SourceRoot.
type BackingReference struct {
	anchor       uint32
	relativePath string
}

// EntryKind is the stable logical type captured in a source tree.
type EntryKind uint8

const (
	DirectoryEntry EntryKind = iota + 1
	RegularFileEntry
)

// SourceFile is one frozen logical file member. Its contents are intentionally not cached.
type SourceFile struct {
	relativePath string
	size         uint64
	backing      BackingReference
}

// SourceDirectory is one directory in the frozen logical tree.
type SourceDirectory struct {
	relativePath string
	backing      BackingReference
	children     []SourceEntry
}

// SourceEntry contains exactly one directory or regular file.
type SourceEntry struct {
	kind      EntryKind
	directory *SourceDirectory
	file      *SourceFile
}

// SourceTree is the ordered logical source tree captured by one scan.
type SourceTree struct{ root SourceDirectory }

// FileMetadata is metadata read from the current backing object.
type FileMetadata struct {
	mode  uint32
	size  uint64
	mtime time.Time
}

// SourceSummary reports source entries handled specially by the scan.
type SourceSummary struct {
	followedSymlinks    uint64
	ignoredSymlinks     uint64
	emptyDirectories    uint64
	ignoredSpecialFiles uint64
}

type sourceSnapshotContents struct {
	baseName       string
	sourceIdentity sourceIdentity
	source         *sourceCapability
	tree           SourceTree
	files          []SourceFile
	emptyDirs      []SourceDirectory
	fileIndex      map[string]int
	emptyDirIndex  map[string]int
	entryKinds     map[string]EntryKind
	summary        SourceSummary
}

// SourceSnapshot is a lightweight handle to one immutable logical source capture.
type SourceSnapshot struct{ contents *sourceSnapshotContents }

func newSourceFile(relativePath string, size uint64, backing ...BackingReference) (SourceFile, error) {
	normalized, err := normalizeRelativePath(relativePath)
	if err != nil {
		return SourceFile{}, err
	}
	reference := BackingReference{relativePath: normalized}
	if len(backing) > 1 {
		return SourceFile{}, errors.New("source file has multiple backing references")
	}
	if len(backing) == 1 {
		reference = backing[0]
	}
	return SourceFile{relativePath: normalized, size: size, backing: reference}, nil
}

func newSourceDirectory(relativePath string, backing BackingReference,
	children []SourceEntry,
) (SourceDirectory, error) {
	if relativePath != "" {
		if _, err := normalizeRelativePath(relativePath); err != nil {
			return SourceDirectory{}, err
		}
	}
	copied := copyEntries(children)
	sort.Slice(copied, func(left, right int) bool { return copied[left].Name() < copied[right].Name() })
	for index, child := range copied {
		if child.kind != DirectoryEntry && child.kind != RegularFileEntry {
			return SourceDirectory{}, fmt.Errorf("source directory child %d has invalid type", index)
		}
		if index > 0 && copied[index-1].Name() == child.Name() {
			return SourceDirectory{}, fmt.Errorf("source directory repeats child %q", child.Name())
		}
	}
	return SourceDirectory{relativePath: relativePath, backing: backing, children: copied}, nil
}

func directoryEntry(directory SourceDirectory) SourceEntry {
	copy := copyDirectory(directory)
	return SourceEntry{kind: DirectoryEntry, directory: &copy}
}

func fileEntry(file SourceFile) SourceEntry {
	copy := file
	return SourceEntry{kind: RegularFileEntry, file: &copy}
}

func (reference BackingReference) valid(anchorCount int) bool {
	if int(reference.anchor) >= anchorCount {
		return false
	}
	if reference.relativePath == "." {
		return true
	}
	_, err := normalizeRelativePath(reference.relativePath)
	return err == nil
}

func (file SourceFile) RelativePath() string { return file.relativePath }
func (file SourceFile) Size() uint64         { return file.size }
func (file SourceFile) Backing() BackingReference {
	return file.backing
}

func (directory SourceDirectory) RelativePath() string { return directory.relativePath }
func (directory SourceDirectory) Backing() BackingReference {
	return directory.backing
}
func (directory SourceDirectory) Children() iter.Seq[SourceEntry] {
	return func(yield func(SourceEntry) bool) {
		for _, entry := range directory.children {
			if !yield(entry) {
				return
			}
		}
	}
}
func (directory SourceDirectory) ChildCount() int { return len(directory.children) }
func (directory SourceDirectory) Empty() bool     { return len(directory.children) == 0 }

func (entry SourceEntry) Kind() EntryKind { return entry.kind }
func (entry SourceEntry) Name() string {
	return filepath.Base(entry.RelativePath())
}
func (entry SourceEntry) RelativePath() string {
	if entry.directory != nil {
		return entry.directory.relativePath
	}
	if entry.file != nil {
		return entry.file.relativePath
	}
	return ""
}
func (entry SourceEntry) Directory() (SourceDirectory, bool) {
	if entry.kind != DirectoryEntry || entry.directory == nil {
		return SourceDirectory{}, false
	}
	return *entry.directory, true
}
func (entry SourceEntry) File() (SourceFile, bool) {
	if entry.kind != RegularFileEntry || entry.file == nil {
		return SourceFile{}, false
	}
	return *entry.file, true
}

func (metadata FileMetadata) Mode() uint32       { return metadata.mode }
func (metadata FileMetadata) Size() uint64       { return metadata.size }
func (metadata FileMetadata) ModTime() time.Time { return metadata.mtime }

func (summary SourceSummary) FollowedSymlinks() uint64 { return summary.followedSymlinks }
func (summary SourceSummary) IgnoredSymlinks() uint64  { return summary.ignoredSymlinks }
func (summary SourceSummary) EmptyDirectories() uint64 { return summary.emptyDirectories }
func (summary SourceSummary) IgnoredSpecialFiles() uint64 {
	return summary.ignoredSpecialFiles
}

func newSourceSnapshot(baseName string, identity sourceIdentity, source *sourceCapability,
	tree SourceTree, summary SourceSummary,
) (SourceSnapshot, error) {
	if err := validateBaseName(baseName); err != nil {
		return SourceSnapshot{}, err
	}
	if identity.kind != sourceDirectoryObject || source == nil {
		return SourceSnapshot{}, errors.New("source snapshot root identity is invalid")
	}
	entries, err := flattenTree(tree)
	if err != nil {
		return SourceSnapshot{}, err
	}
	contents := &sourceSnapshotContents{baseName: baseName, sourceIdentity: identity, source: source,
		tree: tree, files: entries.files, emptyDirs: entries.emptyDirs, fileIndex: entries.fileIndex,
		emptyDirIndex: entries.emptyDirIndex, entryKinds: entries.entryKinds, summary: summary}
	return SourceSnapshot{contents: contents}, nil
}

func (snapshot SourceSnapshot) BaseName() string {
	if snapshot.contents == nil {
		return ""
	}
	return snapshot.contents.baseName
}
func (snapshot SourceSnapshot) RootDirectory() SourceDirectory {
	if snapshot.contents == nil {
		return SourceDirectory{}
	}
	return snapshot.contents.tree.root
}
func (snapshot SourceSnapshot) Files() iter.Seq[SourceFile] {
	return func(yield func(SourceFile) bool) {
		if snapshot.contents == nil {
			return
		}
		for _, file := range snapshot.contents.files {
			if !yield(file) {
				return
			}
		}
	}
}
func (snapshot SourceSnapshot) FileCount() int {
	if snapshot.contents == nil {
		return 0
	}
	return len(snapshot.contents.files)
}
func (snapshot SourceSnapshot) EmptyDirectories() iter.Seq[SourceDirectory] {
	return func(yield func(SourceDirectory) bool) {
		if snapshot.contents == nil {
			return
		}
		for _, directory := range snapshot.contents.emptyDirs {
			if !yield(directory) {
				return
			}
		}
	}
}
func (snapshot SourceSnapshot) EmptyDirectoryCount() int {
	if snapshot.contents == nil {
		return 0
	}
	return len(snapshot.contents.emptyDirs)
}
func (snapshot SourceSnapshot) Summary() SourceSummary {
	if snapshot.contents == nil {
		return SourceSummary{}
	}
	return snapshot.contents.summary
}

type flattenedSourceTree struct {
	files         []SourceFile
	emptyDirs     []SourceDirectory
	fileIndex     map[string]int
	emptyDirIndex map[string]int
	entryKinds    map[string]EntryKind
}

func flattenTree(tree SourceTree) (flattenedSourceTree, error) {
	if tree.root.relativePath != "" {
		return flattenedSourceTree{}, errors.New("source tree root is invalid")
	}
	var files []SourceFile
	var emptyDirs []SourceDirectory
	seen := make(map[string]EntryKind)
	var walk func(SourceDirectory) error
	walk = func(directory SourceDirectory) error {
		for _, entry := range directory.children {
			path := entry.RelativePath()
			if _, exists := seen[path]; exists {
				return fmt.Errorf("source tree repeats path %q", path)
			}
			seen[path] = entry.kind
			switch entry.kind {
			case RegularFileEntry:
				file, ok := entry.File()
				if !ok {
					return errors.New("source tree contains an invalid file")
				}
				files = append(files, file)
			case DirectoryEntry:
				child, ok := entry.Directory()
				if !ok {
					return errors.New("source tree contains an invalid directory")
				}
				if child.Empty() {
					emptyDirs = append(emptyDirs, child)
				}
				if err := walk(child); err != nil {
					return err
				}
			default:
				return errors.New("source tree contains an invalid entry type")
			}
		}
		return nil
	}
	if err := walk(tree.root); err != nil {
		return flattenedSourceTree{}, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].relativePath < files[j].relativePath })
	sort.Slice(emptyDirs, func(i, j int) bool { return emptyDirs[i].relativePath < emptyDirs[j].relativePath })
	fileIndex := make(map[string]int, len(files))
	for index, file := range files {
		fileIndex[file.relativePath] = index
	}
	emptyDirIndex := make(map[string]int, len(emptyDirs))
	for index, directory := range emptyDirs {
		emptyDirIndex[directory.relativePath] = index
	}
	return flattenedSourceTree{files: files, emptyDirs: emptyDirs, fileIndex: fileIndex,
		emptyDirIndex: emptyDirIndex, entryKinds: seen}, nil
}

func copyDirectory(directory SourceDirectory) SourceDirectory {
	directory.children = append([]SourceEntry(nil), directory.children...)
	return directory
}

func copyEntries(entries []SourceEntry) []SourceEntry {
	return append([]SourceEntry(nil), entries...)
}

func validateRootPath(root string) (string, error) {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return "", fmt.Errorf("source root %q is not normalized absolute", root)
	}
	volumeRoot := filepath.VolumeName(root) + string(filepath.Separator)
	if root == volumeRoot {
		return "", fmt.Errorf("source root cannot be filesystem root %q", root)
	}
	baseName := filepath.Base(root)
	if err := validateBaseName(baseName); err != nil {
		return "", fmt.Errorf("source root %q has no safe base name: %w", root, err)
	}
	return baseName, nil
}

func validateBaseName(baseName string) error {
	if baseName == "" || filepath.Base(baseName) != baseName || baseName == "." || baseName == ".." {
		return fmt.Errorf("source base name %q is invalid", baseName)
	}
	return nil
}

func normalizeRelativePath(path string) (string, error) {
	cleaned := filepath.Clean(path)
	if path == "" || filepath.IsAbs(path) || cleaned == "." || cleaned != path {
		return "", fmt.Errorf("source relative path %q is not normalized", path)
	}
	parentPrefix := ".." + string(filepath.Separator)
	if cleaned == ".." || strings.HasPrefix(cleaned, parentPrefix) {
		return "", fmt.Errorf("source relative path %q escapes its root", path)
	}
	return cleaned, nil
}
