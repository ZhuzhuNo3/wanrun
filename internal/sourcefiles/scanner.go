package sourcefiles

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type sourceScanner struct {
	ctx            context.Context
	owner          *SourceRoot
	followSymlinks bool
	summary        SourceSummary
	ancestors      map[sourceIdentity]struct{}
}

// Scan captures one frozen logical tree from the exact owned source directory.
func (root *SourceRoot) Scan(ctx context.Context, followSymlinks bool) (
	snapshot SourceSnapshot, resultErr error,
) {
	if ctx == nil {
		return SourceSnapshot{}, errors.New("source scan requires context")
	}
	if err := ctx.Err(); err != nil {
		return SourceSnapshot{}, err
	}
	rootFile, identity, capability, err := root.beginScan()
	if err != nil {
		return SourceSnapshot{}, fmt.Errorf("begin source scan: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, root.finishScan(rootFile)) }()

	scanner := sourceScanner{ctx: ctx, owner: root, followSymlinks: followSymlinks,
		ancestors: map[sourceIdentity]struct{}{identity: {}}}
	rootDirectory, err := scanner.scanDirectory(rootFile, "",
		BackingReference{anchor: 0, relativePath: "."})
	if err != nil {
		return SourceSnapshot{}, err
	}
	snapshot, err = newSourceSnapshot(root.BaseName(), identity, capability,
		SourceTree{root: rootDirectory}, scanner.summary)
	if err != nil {
		return SourceSnapshot{}, err
	}
	if snapshot.FileCount()+snapshot.EmptyDirectoryCount() == 0 {
		return snapshot, ErrNoTransferItems
	}
	return snapshot, nil
}

func (scanner *sourceScanner) scanDirectory(directory *os.File, logicalPath string,
	backing BackingReference,
) (SourceDirectory, error) {
	if err := scanner.ctx.Err(); err != nil {
		return SourceDirectory{}, err
	}
	before, err := inspectDescriptor(directory)
	if err != nil || before.kind != sourceDirectoryObject {
		return SourceDirectory{}, errors.Join(fmt.Errorf("source directory %q is not stable", logicalPath), err)
	}
	names, err := readSourceNames(directory)
	if err != nil {
		return SourceDirectory{}, fmt.Errorf("read source directory %q: %w", logicalPath, err)
	}
	observed := make(map[string]sourceIdentity, len(names))
	children := make([]SourceEntry, 0, len(names))
	for _, name := range names {
		if err := scanner.ctx.Err(); err != nil {
			return SourceDirectory{}, err
		}
		identity, err := statSourceAt(directory, name)
		if err != nil {
			return SourceDirectory{}, fmt.Errorf("inspect source entry %q: %w", joinRelative(logicalPath, name), err)
		}
		observed[name] = identity
		entry, included, err := scanner.captureEntry(directory, logicalPath, name, identity, backing)
		if err != nil {
			return SourceDirectory{}, err
		}
		if included {
			children = append(children, entry)
		}
	}
	after, err := inspectDescriptor(directory)
	if err != nil || after != before {
		return SourceDirectory{}, errors.Join(fmt.Errorf("source directory %q changed identity during scan", logicalPath), err)
	}
	if err := verifySourceNames(directory, logicalPath, observed); err != nil {
		return SourceDirectory{}, err
	}
	result, err := newSourceDirectory(logicalPath, backing, children)
	if err == nil && logicalPath != "" && result.Empty() {
		scanner.summary.emptyDirectories++
	}
	return result, err
}

func (scanner *sourceScanner) captureEntry(parent *os.File, parentLogical, name string,
	identity sourceIdentity, parentBacking BackingReference,
) (SourceEntry, bool, error) {
	logicalPath := joinRelative(parentLogical, name)
	switch identity.kind {
	case sourceSymlinkObject:
		if !scanner.followSymlinks {
			scanner.summary.ignoredSymlinks++
			return SourceEntry{}, false, nil
		}
		entry, included, err := scanner.followSymlink(parent, name, logicalPath, identity)
		return entry, included, err
	case sourceDirectoryObject:
		return scanner.captureDirectory(parent, name, logicalPath, identity, parentBacking)
	case sourceRegularObject:
		return scanner.captureRegularFile(parent, name, logicalPath, identity, parentBacking)
	default:
		scanner.summary.ignoredSpecialFiles++
		return SourceEntry{}, false, nil
	}
}

func (scanner *sourceScanner) captureDirectory(parent *os.File, name, logicalPath string,
	identity sourceIdentity, parentBacking BackingReference,
) (SourceEntry, bool, error) {
	directory, err := openSourceDirectoryAt(parent, name)
	if err != nil {
		return SourceEntry{}, false, fmt.Errorf("open source directory %q: %w", logicalPath, err)
	}
	defer directory.Close()
	opened, err := inspectDescriptor(directory)
	if err != nil || opened != identity {
		return SourceEntry{}, false,
			errors.Join(fmt.Errorf("source directory %q changed while opening", logicalPath), err)
	}
	if _, cyclic := scanner.ancestors[opened]; cyclic {
		return SourceEntry{}, false, fmt.Errorf("source directory %q forms a recursive cycle", logicalPath)
	}
	scanner.ancestors[opened] = struct{}{}
	defer delete(scanner.ancestors, opened)
	directoryBacking := BackingReference{anchor: parentBacking.anchor,
		relativePath: joinBacking(parentBacking.relativePath, name)}
	captured, err := scanner.scanDirectory(directory, logicalPath, directoryBacking)
	return directoryEntry(captured), err == nil, err
}

func (scanner *sourceScanner) captureRegularFile(parent *os.File, name, logicalPath string,
	identity sourceIdentity, parentBacking BackingReference,
) (SourceEntry, bool, error) {
	file, err := openSourceRegularAt(parent, name)
	if err != nil {
		return SourceEntry{}, false, fmt.Errorf("open source file %q: %w", logicalPath, err)
	}
	defer file.Close()
	opened, size, err := inspectOpenedRegular(file)
	if err != nil || opened != identity {
		return SourceEntry{}, false,
			errors.Join(fmt.Errorf("source file %q changed while opening", logicalPath), err)
	}
	captured, err := newSourceFile(logicalPath, size, BackingReference{
		anchor: parentBacking.anchor, relativePath: joinBacking(parentBacking.relativePath, name)})
	return fileEntry(captured), err == nil, err
}

func verifySourceNames(directory *os.File, relative string, observed map[string]sourceIdentity) error {
	names, err := readSourceNames(directory)
	if err != nil {
		return err
	}
	if len(names) != len(observed) {
		return fmt.Errorf("source directory %q changed entries during scan", relative)
	}
	for _, name := range names {
		identity, exists := observed[name]
		if !exists {
			return fmt.Errorf("source directory %q gained entry %q during scan", relative, name)
		}
		current, err := statSourceAt(directory, name)
		if err != nil || current != identity {
			return errors.Join(fmt.Errorf("source entry %q changed during scan", joinRelative(relative, name)), err)
		}
	}
	return nil
}

func readSourceNames(directory *os.File) ([]string, error) {
	reader, err := openSourceDirectoryAt(directory, ".")
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	names, err := reader.Readdirnames(-1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	sort.Strings(names)
	return names, nil
}

func joinRelative(parent, name string) string {
	if parent == "" {
		return name
	}
	return filepath.Join(parent, name)
}

func joinBacking(parent, name string) string {
	if parent == "." {
		return name
	}
	return filepath.Join(parent, name)
}

func splitRelative(path string) []string {
	return strings.Split(path, string(filepath.Separator))
}
