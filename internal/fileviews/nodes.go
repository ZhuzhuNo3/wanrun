package fileviews

import (
	"context"
	"os"
	"syscall"
	"time"

	"github.com/ZhuzhuNo3/transferlanes/internal/sourcefiles"
	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
	goFuseFS "github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

type mountRoot struct {
	goFuseFS.Inode
	tree   *viewTree
	access *sourcefiles.SourceAccess
}

type transferNode struct {
	goFuseFS.Inode
	tree     *viewTree
	access   *sourcefiles.SourceAccess
	transfer transfernumber.Number
}

type directoryNode struct {
	goFuseFS.Inode
	entry    *viewEntry
	access   *sourcefiles.SourceAccess
	transfer transfernumber.Number
}

type regularFileNode struct {
	goFuseFS.Inode
	entry  *viewEntry
	access *sourcefiles.SourceAccess
}

type readOnlyFile struct{ file *os.File }

func (root *mountRoot) Lookup(ctx context.Context, name string,
	out *fuse.EntryOut,
) (*goFuseFS.Inode, syscall.Errno) {
	transfer, ok := parseTransferDirectoryName(name, root.tree.transfers)
	if !ok {
		return nil, syscall.ENOENT
	}
	node := &transferNode{tree: root.tree, access: root.access, transfer: transfer}
	fillSyntheticDirectoryEntry(out)
	return root.NewInode(ctx, node, goFuseFS.StableAttr{Mode: syscall.S_IFDIR}), 0
}

func (root *mountRoot) Readdir(context.Context) (goFuseFS.DirStream, syscall.Errno) {
	entries := make([]fuse.DirEntry, len(root.tree.transfers))
	for index, transfer := range root.tree.transfers {
		entries[index] = fuse.DirEntry{Name: transferDirectoryName(transfer), Mode: syscall.S_IFDIR}
	}
	return goFuseFS.NewListDirStream(entries), 0
}

func (root *mountRoot) Getattr(_ context.Context, _ goFuseFS.FileHandle,
	out *fuse.AttrOut,
) syscall.Errno {
	fillSyntheticDirectoryAttr(out)
	return 0
}

func (node *transferNode) Lookup(ctx context.Context, name string,
	out *fuse.EntryOut,
) (*goFuseFS.Inode, syscall.Errno) {
	if name != node.tree.baseName {
		return nil, syscall.ENOENT
	}
	child := &directoryNode{entry: node.tree.root, access: node.access, transfer: node.transfer}
	if errno := child.fillEntry(out); errno != 0 {
		return nil, errno
	}
	return node.NewInode(ctx, child, goFuseFS.StableAttr{Mode: syscall.S_IFDIR}), 0
}

func (node *transferNode) Readdir(context.Context) (goFuseFS.DirStream, syscall.Errno) {
	return goFuseFS.NewListDirStream([]fuse.DirEntry{{Name: node.tree.baseName,
		Mode: syscall.S_IFDIR}}), 0
}

func (node *transferNode) Getattr(_ context.Context, _ goFuseFS.FileHandle,
	out *fuse.AttrOut,
) syscall.Errno {
	fillSyntheticDirectoryAttr(out)
	return 0
}

func (node *directoryNode) Lookup(ctx context.Context, name string,
	out *fuse.EntryOut,
) (*goFuseFS.Inode, syscall.Errno) {
	entry := node.entry.child(name, node.transfer)
	if entry == nil {
		return nil, syscall.ENOENT
	}
	if entry.kind == sourcefiles.DirectoryEntry {
		child := &directoryNode{entry: entry, access: node.access, transfer: node.transfer}
		if errno := child.fillEntry(out); errno != 0 {
			return nil, errno
		}
		return node.NewInode(ctx, child, goFuseFS.StableAttr{Mode: syscall.S_IFDIR}), 0
	}
	child := &regularFileNode{entry: entry, access: node.access}
	if errno := child.fillEntry(out); errno != 0 {
		return nil, errno
	}
	return node.NewInode(ctx, child, goFuseFS.StableAttr{Mode: syscall.S_IFREG}), 0
}

func (node *directoryNode) Readdir(context.Context) (goFuseFS.DirStream, syscall.Errno) {
	children := node.entry.visibleChildren(node.transfer)
	entries := make([]fuse.DirEntry, len(children))
	for index, child := range children {
		mode := uint32(syscall.S_IFREG)
		if child.kind == sourcefiles.DirectoryEntry {
			mode = syscall.S_IFDIR
		}
		entries[index] = fuse.DirEntry{Name: child.name, Mode: mode}
	}
	return goFuseFS.NewListDirStream(entries), 0
}

func (node *directoryNode) Getattr(_ context.Context, _ goFuseFS.FileHandle,
	out *fuse.AttrOut,
) syscall.Errno {
	metadata, err := node.access.DirectoryMetadata(node.entry.backing)
	if err != nil {
		return goFuseFS.ToErrno(err)
	}
	fillMetadata(&out.Attr, metadata)
	out.SetTimeout(0)
	return 0
}

func (node *directoryNode) fillEntry(out *fuse.EntryOut) syscall.Errno {
	metadata, err := node.access.DirectoryMetadata(node.entry.backing)
	if err != nil {
		return goFuseFS.ToErrno(err)
	}
	fillMetadata(&out.Attr, metadata)
	out.SetAttrTimeout(0)
	out.SetEntryTimeout(time.Hour)
	return 0
}

func (node *regularFileNode) Getattr(_ context.Context, _ goFuseFS.FileHandle,
	out *fuse.AttrOut,
) syscall.Errno {
	file, metadata, err := node.access.OpenRegular(node.entry.backing)
	if err != nil {
		return goFuseFS.ToErrno(err)
	}
	closeErr := file.Close()
	if closeErr != nil {
		return goFuseFS.ToErrno(closeErr)
	}
	fillMetadata(&out.Attr, metadata)
	out.SetTimeout(0)
	return 0
}

func (node *regularFileNode) fillEntry(out *fuse.EntryOut) syscall.Errno {
	file, metadata, err := node.access.OpenRegular(node.entry.backing)
	if err != nil {
		return goFuseFS.ToErrno(err)
	}
	if err := file.Close(); err != nil {
		return goFuseFS.ToErrno(err)
	}
	fillMetadata(&out.Attr, metadata)
	out.SetAttrTimeout(0)
	out.SetEntryTimeout(time.Hour)
	return 0
}

func (node *regularFileNode) Open(_ context.Context, flags uint32) (
	goFuseFS.FileHandle, uint32, syscall.Errno,
) {
	if flags&syscall.O_ACCMODE != syscall.O_RDONLY ||
		flags&(syscall.O_TRUNC|syscall.O_APPEND|syscall.O_CREAT) != 0 {
		return nil, 0, syscall.EROFS
	}
	file, _, err := node.access.OpenRegular(node.entry.backing)
	if err != nil {
		return nil, 0, goFuseFS.ToErrno(err)
	}
	return &readOnlyFile{file: file}, fuse.FOPEN_DIRECT_IO, 0
}

func (file *readOnlyFile) Read(_ context.Context, destination []byte,
	offset int64,
) (fuse.ReadResult, syscall.Errno) {
	return fuse.ReadResultFd(file.file.Fd(), offset, len(destination)), 0
}

func (file *readOnlyFile) Getattr(_ context.Context, out *fuse.AttrOut) syscall.Errno {
	info, err := file.file.Stat()
	if err != nil {
		return goFuseFS.ToErrno(err)
	}
	out.Mode = fuseRegularFileMode(info.Mode())
	out.Size = uint64(info.Size())
	out.Nlink = 1
	out.Mtime = uint64(info.ModTime().Unix())
	out.Mtimensec = uint32(info.ModTime().Nanosecond())
	out.SetTimeout(0)
	return 0
}

func fuseRegularFileMode(mode os.FileMode) uint32 {
	result := uint32(syscall.S_IFREG) | uint32(mode.Perm())
	if mode&os.ModeSetuid != 0 {
		result |= syscall.S_ISUID
	}
	if mode&os.ModeSetgid != 0 {
		result |= syscall.S_ISGID
	}
	if mode&os.ModeSticky != 0 {
		result |= syscall.S_ISVTX
	}
	return result
}

func (file *readOnlyFile) Release(context.Context) syscall.Errno {
	return goFuseFS.ToErrno(file.file.Close())
}

func fillSyntheticDirectoryEntry(out *fuse.EntryOut) {
	out.Mode = syscall.S_IFDIR | 0o555
	out.Nlink = 1
	out.SetAttrTimeout(0)
	out.SetEntryTimeout(time.Hour)
}

func fillSyntheticDirectoryAttr(out *fuse.AttrOut) {
	out.Mode = syscall.S_IFDIR | 0o555
	out.Nlink = 1
	out.SetTimeout(0)
}

func fillMetadata(out *fuse.Attr, metadata sourcefiles.FileMetadata) {
	out.Mode = metadata.Mode()
	out.Size = metadata.Size()
	out.Nlink = 1
	out.Mtime = uint64(metadata.ModTime().Unix())
	out.Mtimensec = uint32(metadata.ModTime().Nanosecond())
}

var _ goFuseFS.NodeLookuper = (*mountRoot)(nil)
var _ goFuseFS.NodeReaddirer = (*mountRoot)(nil)
var _ goFuseFS.NodeGetattrer = (*mountRoot)(nil)
var _ goFuseFS.NodeLookuper = (*transferNode)(nil)
var _ goFuseFS.NodeReaddirer = (*transferNode)(nil)
var _ goFuseFS.NodeGetattrer = (*transferNode)(nil)
var _ goFuseFS.NodeLookuper = (*directoryNode)(nil)
var _ goFuseFS.NodeReaddirer = (*directoryNode)(nil)
var _ goFuseFS.NodeGetattrer = (*directoryNode)(nil)
var _ goFuseFS.NodeGetattrer = (*regularFileNode)(nil)
var _ goFuseFS.NodeOpener = (*regularFileNode)(nil)
var _ goFuseFS.FileReader = (*readOnlyFile)(nil)
var _ goFuseFS.FileGetattrer = (*readOnlyFile)(nil)
var _ goFuseFS.FileReleaser = (*readOnlyFile)(nil)
