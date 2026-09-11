package sourcefiles

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

var (
	// ErrSourceRootBorrowed reports that ownership cannot move while a caller uses a borrow.
	ErrSourceRootBorrowed = errors.New("source root is borrowed")
	// ErrSourceRootTransferred reports that ownership has already moved to another caller.
	ErrSourceRootTransferred = errors.New("source root ownership was transferred")
	// ErrSourceRootScanned reports that the fixed source membership was already captured.
	ErrSourceRootScanned = errors.New("source root was already scanned")
	// ErrSourceRootClosed reports that the source capability no longer owns a descriptor.
	ErrSourceRootClosed = errors.New("source root is closed")
)

// SourceRoot owns one exact source directory inode for scanning and view publication.
type SourceRoot struct {
	mu          sync.Mutex
	root        *os.File
	anchors     []*os.File
	label       string
	baseName    string
	stableRoot  string
	identity    sourceIdentity
	capability  *sourceCapability
	borrows     int
	scanned     bool
	transferred bool
	closed      bool
}

// SourceRootBorrow is a short-lived descriptor borrow with no ownership-transfer authority.
type SourceRootBorrow struct {
	once  sync.Once
	owner *SourceRoot
	root  *os.File
	err   error
}

// OpenSourceRoot binds the final non-symlink source directory while allowing symlinks in its
// ancestors. The requested path itself must be absolute.
func OpenSourceRoot(path string) (*SourceRoot, error) {
	label, baseName, err := normalizedSourceLabel(path)
	if err != nil {
		return nil, err
	}
	before, err := lstatSource(label)
	if err != nil {
		return nil, fmt.Errorf("inspect source root %q: %w", label, err)
	}
	if before.kind != sourceDirectoryObject {
		return nil, fmt.Errorf("source root %q must be an actual non-symlink directory", label)
	}
	root, err := openSourceDirectoryPath(label)
	if err != nil {
		return nil, fmt.Errorf("open source root %q: %w", label, err)
	}
	opened, err := inspectDescriptor(root)
	if err != nil || opened != before {
		_ = root.Close()
		return nil, errors.Join(errors.New("source root changed while opening"), err)
	}
	return newSourceRoot(root, label, baseName, opened), nil
}

// TakeSourceRoot consumes a transferred source descriptor, including on validation failure.
func TakeSourceRoot(root *os.File, label string) (*SourceRoot, error) {
	normalized, baseName, err := normalizedSourceLabel(label)
	if err != nil {
		if root != nil {
			_ = root.Close()
		}
		return nil, err
	}
	if root == nil {
		return nil, errors.New("inherited source descriptor is absent")
	}
	identity, err := inspectDescriptor(root)
	if err != nil || identity.kind != sourceDirectoryObject {
		_ = root.Close()
		return nil, errors.Join(errors.New("inherited source descriptor is not a directory"), err)
	}
	return newSourceRoot(root, normalized, baseName, identity), nil
}

func newSourceRoot(root *os.File, label, baseName string, identity sourceIdentity) *SourceRoot {
	return &SourceRoot{root: root, label: label, baseName: baseName, identity: identity,
		capability: &sourceCapability{},
		stableRoot: stableDescriptorRoot(os.Getpid(), int(root.Fd()), label)}
}

func normalizedSourceLabel(path string) (string, string, error) {
	if !filepath.IsAbs(path) {
		return "", "", fmt.Errorf("source root %q must be absolute", path)
	}
	absolute := filepath.Clean(path)
	baseName, err := validateRootPath(absolute)
	if err != nil {
		return "", "", err
	}
	return absolute, baseName, nil
}

// Label returns the normalized user-facing source name. It is never an opening authority.
func (root *SourceRoot) Label() string {
	if root == nil {
		return ""
	}
	return root.label
}

// BaseName returns the final source name preserved in every transfer view.
func (root *SourceRoot) BaseName() string {
	if root == nil {
		return ""
	}
	return root.baseName
}

// StableRoot returns a process-visible path rooted in the owned descriptor.
func (root *SourceRoot) StableRoot() string {
	if root == nil {
		return ""
	}
	root.mu.Lock()
	defer root.mu.Unlock()
	if root.root == nil {
		return ""
	}
	return root.stableRoot
}

// Borrow duplicates the exact source root until the returned borrow is closed.
func (root *SourceRoot) Borrow() (*SourceRootBorrow, error) {
	if root == nil {
		return nil, errors.New("source root is absent")
	}
	root.mu.Lock()
	defer root.mu.Unlock()
	duplicate, err := root.borrowDescriptorLocked("transferlanes-source-root-borrow")
	if err != nil {
		return nil, err
	}
	return &SourceRootBorrow{owner: root, root: duplicate}, nil
}

func (root *SourceRoot) borrowDescriptorLocked(name string) (*os.File, error) {
	if root.transferred {
		return nil, ErrSourceRootTransferred
	}
	if root.closed || root.root == nil {
		return nil, ErrSourceRootClosed
	}
	duplicate, err := duplicateSourceDescriptor(root.root, name)
	if err != nil {
		return nil, err
	}
	root.borrows++
	return duplicate, nil
}

// Descriptor returns the borrowed directory descriptor. Close returns the borrow.
func (borrow *SourceRootBorrow) Descriptor() uintptr {
	if borrow == nil || borrow.root == nil {
		return ^uintptr(0)
	}
	return borrow.root.Fd()
}

// Close returns this borrow exactly once.
func (borrow *SourceRootBorrow) Close() error {
	if borrow == nil {
		return nil
	}
	borrow.once.Do(func() {
		borrow.err = borrow.root.Close()
		borrow.owner.mu.Lock()
		borrow.owner.borrows--
		borrow.owner.mu.Unlock()
		borrow.root = nil
	})
	return borrow.err
}

// TransferDescriptor moves the owned descriptor to the caller exactly once. Active borrows must
// first be returned. The caller becomes responsible for closing or transferring the descriptor.
func (root *SourceRoot) TransferDescriptor() (*os.File, error) {
	if root == nil {
		return nil, errors.New("source root is absent")
	}
	root.mu.Lock()
	defer root.mu.Unlock()
	if root.transferred {
		return nil, ErrSourceRootTransferred
	}
	if root.closed || root.root == nil {
		return nil, ErrSourceRootClosed
	}
	if root.borrows != 0 {
		return nil, ErrSourceRootBorrowed
	}
	descriptor := root.root
	root.root = nil
	root.transferred = true
	for _, anchor := range root.anchors {
		_ = anchor.Close()
	}
	root.anchors = nil
	return descriptor, nil
}

// Close releases the source capability exactly once when this value still owns it.
func (root *SourceRoot) Close() error {
	if root == nil {
		return nil
	}
	root.mu.Lock()
	defer root.mu.Unlock()
	if root.transferred {
		return ErrSourceRootTransferred
	}
	if root.closed {
		return nil
	}
	if root.borrows != 0 {
		return ErrSourceRootBorrowed
	}
	descriptor := root.root
	root.root = nil
	root.closed = true
	var failures []error
	if descriptor != nil {
		failures = append(failures, descriptor.Close())
	}
	for _, anchor := range root.anchors {
		failures = append(failures, anchor.Close())
	}
	root.anchors = nil
	return errors.Join(failures...)
}

func (root *SourceRoot) beginScan() (*os.File, sourceIdentity, *sourceCapability, error) {
	if root == nil {
		return nil, sourceIdentity{}, nil, errors.New("source root is absent")
	}
	root.mu.Lock()
	defer root.mu.Unlock()
	if root.scanned {
		return nil, sourceIdentity{}, nil, ErrSourceRootScanned
	}
	descriptor, err := root.borrowDescriptorLocked("transferlanes-source-root-scan")
	if err != nil {
		return nil, sourceIdentity{}, nil, err
	}
	root.scanned = true
	return descriptor, root.identity, root.capability, nil
}

func (root *SourceRoot) sameSnapshotSource(identity sourceIdentity,
	capability *sourceCapability,
) bool {
	if root == nil || capability == nil {
		return false
	}
	root.mu.Lock()
	defer root.mu.Unlock()
	return root.root != nil && root.identity == identity && root.capability == capability
}

func (root *SourceRoot) finishScan(descriptor *os.File) error {
	closeErr := descriptor.Close()
	root.mu.Lock()
	root.borrows--
	root.mu.Unlock()
	return closeErr
}

func (root *SourceRoot) addBackingDirectory(directory *os.File) (uint32, error) {
	if directory == nil {
		return 0, errors.New("backing directory is absent")
	}
	root.mu.Lock()
	defer root.mu.Unlock()
	if root.transferred || root.closed || root.root == nil {
		return 0, ErrSourceRootClosed
	}
	if len(root.anchors) >= int(^uint32(0))-1 {
		return 0, errors.New("too many source backing directories")
	}
	root.anchors = append(root.anchors, directory)
	return uint32(len(root.anchors)), nil
}
