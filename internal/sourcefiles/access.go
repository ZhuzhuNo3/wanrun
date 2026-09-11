package sourcefiles

import (
	"errors"
	"os"
	"sync"
)

// SourceAccess is a long-lived read-only lease over all backing directory descriptors.
type SourceAccess struct {
	once    sync.Once
	owner   *SourceRoot
	anchors []*os.File
	err     error
}

// AcquireAccess returns a lease that can reopen only objects named by this snapshot.
func (root *SourceRoot) AcquireAccess(snapshot SourceSnapshot) (*SourceAccess, error) {
	if root == nil || snapshot.contents == nil ||
		!root.sameSnapshotSource(snapshot.contents.sourceIdentity, snapshot.contents.source) {
		return nil, errors.New("source access requires its originating snapshot")
	}
	root.mu.Lock()
	defer root.mu.Unlock()
	if root.transferred || root.closed || root.root == nil {
		return nil, ErrSourceRootClosed
	}
	duplicates, err := duplicateBackingDirectories(root.root, root.anchors)
	if err != nil {
		return nil, err
	}
	root.borrows++
	return &SourceAccess{owner: root, anchors: duplicates}, nil
}

func duplicateBackingDirectories(root *os.File, anchors []*os.File) ([]*os.File, error) {
	sources := make([]*os.File, 0, 1+len(anchors))
	sources = append(sources, root)
	sources = append(sources, anchors...)
	duplicates := make([]*os.File, 0, len(sources))
	for _, source := range sources {
		file, err := duplicateSourceDescriptor(source, "transferlanes-source-backing")
		if err != nil {
			return nil, errors.Join(err, errors.Join(closeFiles(duplicates)...))
		}
		duplicates = append(duplicates, file)
	}
	return duplicates, nil
}

// OpenRegular opens the current ordinary file at a frozen backing reference.
func (access *SourceAccess) OpenRegular(reference BackingReference) (*os.File, FileMetadata, error) {
	if access == nil || !reference.valid(len(access.anchors)) {
		return nil, FileMetadata{}, errors.New("source backing reference is invalid")
	}
	file, err := openRelativeRegular(access.anchors[reference.anchor], reference.relativePath)
	if err != nil {
		return nil, FileMetadata{}, err
	}
	metadata, err := metadataFromDescriptor(file, sourceRegularObject)
	if err != nil {
		_ = file.Close()
		return nil, FileMetadata{}, err
	}
	return file, metadata, nil
}

// DirectoryMetadata reads current metadata for a frozen logical directory.
func (access *SourceAccess) DirectoryMetadata(reference BackingReference) (FileMetadata, error) {
	if access == nil || !reference.valid(len(access.anchors)) {
		return FileMetadata{}, errors.New("source backing reference is invalid")
	}
	directory, err := openRelativeDirectory(access.anchors[reference.anchor], reference.relativePath)
	if err != nil {
		return FileMetadata{}, err
	}
	defer directory.Close()
	return metadataFromDescriptor(directory, sourceDirectoryObject)
}

// Close returns this long-lived lease exactly once.
func (access *SourceAccess) Close() error {
	if access == nil {
		return nil
	}
	access.once.Do(func() {
		access.err = errors.Join(closeFiles(access.anchors)...)
		access.anchors = nil
		access.owner.mu.Lock()
		access.owner.borrows--
		access.owner.mu.Unlock()
	})
	return access.err
}

func closeFiles(files []*os.File) []error {
	failures := make([]error, 0, len(files))
	for _, file := range files {
		if file != nil {
			failures = append(failures, file.Close())
		}
	}
	return failures
}
