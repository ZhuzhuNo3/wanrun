package fileviews

import (
	"errors"
	"fmt"

	"github.com/ZhuzhuNo3/transferlanes/internal/sourcefiles"
	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
)

type viewEntry struct {
	name       string
	kind       sourcefiles.EntryKind
	backing    sourcefiles.BackingReference
	children   []*viewEntry
	fileOwner  transfernumber.Number
	emptyOwner transfernumber.Number
	members    uint64
}

type viewTree struct {
	baseName  string
	root      *viewEntry
	transfers []transfernumber.Number
}

func newViewTree(allocation sourcefiles.TransferAllocation) (*viewTree, error) {
	if allocation.BaseName() == "" || allocation.TransferCount() == 0 {
		return nil, errors.New("file-view allocation is incomplete")
	}
	root, err := buildViewDirectory(allocation.Snapshot().RootDirectory(), allocation)
	if err != nil {
		return nil, err
	}
	transfers := make([]transfernumber.Number, 0, allocation.TransferCount())
	for transfer := range allocation.Transfers() {
		transfers = append(transfers, transfer)
	}
	return &viewTree{baseName: allocation.BaseName(), root: root, transfers: transfers}, nil
}

func buildViewDirectory(directory sourcefiles.SourceDirectory,
	allocation sourcefiles.TransferAllocation,
) (*viewEntry, error) {
	node := &viewEntry{name: entryName(directory.RelativePath()), kind: sourcefiles.DirectoryEntry,
		backing: directory.Backing()}
	if directory.RelativePath() != "" && directory.Empty() {
		owner, exists := allocation.OwnerOf(directory.RelativePath())
		if !exists || owner.Value() == 0 {
			return nil, fmt.Errorf("logical empty directory %q has no transfer", directory.RelativePath())
		}
		node.emptyOwner = owner
		node.members = transferBit(owner)
	}
	for entry := range directory.Children() {
		child, err := buildViewEntry(entry, allocation)
		if err != nil {
			return nil, err
		}
		node.children = append(node.children, child)
		node.members |= child.members
	}
	return node, nil
}

func buildViewEntry(entry sourcefiles.SourceEntry,
	allocation sourcefiles.TransferAllocation,
) (*viewEntry, error) {
	switch entry.Kind() {
	case sourcefiles.DirectoryEntry:
		directory, ok := entry.Directory()
		if !ok {
			return nil, errors.New("logical tree contains an invalid directory")
		}
		return buildViewDirectory(directory, allocation)
	case sourcefiles.RegularFileEntry:
		file, ok := entry.File()
		if !ok {
			return nil, errors.New("logical tree contains an invalid file")
		}
		owner, exists := allocation.OwnerOf(file.RelativePath())
		if !exists || owner.Value() == 0 {
			return nil, fmt.Errorf("logical file %q has no transfer", file.RelativePath())
		}
		return &viewEntry{name: entryName(file.RelativePath()), kind: sourcefiles.RegularFileEntry,
			backing: file.Backing(), fileOwner: owner, members: transferBit(owner)}, nil
	default:
		return nil, errors.New("logical tree contains an invalid entry type")
	}
}

func (node *viewEntry) child(name string, transfer transfernumber.Number) *viewEntry {
	for _, child := range node.children {
		if child.name == name && child.visibleTo(transfer) {
			return child
		}
	}
	return nil
}

func (node *viewEntry) visibleChildren(transfer transfernumber.Number) []*viewEntry {
	children := make([]*viewEntry, 0, len(node.children))
	for _, child := range node.children {
		if child.visibleTo(transfer) {
			children = append(children, child)
		}
	}
	return children
}

func (node *viewEntry) visibleTo(transfer transfernumber.Number) bool {
	return node.members&transferBit(transfer) != 0
}

func transferBit(transfer transfernumber.Number) uint64 {
	if transfer.Value() == 0 {
		return 0
	}
	return uint64(1) << (transfer.Value() - 1)
}

func entryName(relative string) string {
	if relative == "" {
		return ""
	}
	for index := len(relative) - 1; index >= 0; index-- {
		if relative[index] == '/' {
			return relative[index+1:]
		}
	}
	return relative
}
