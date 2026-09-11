//go:build linux

package hostnetwork

import "golang.org/x/sys/unix"

func renameNoReplaceAt(directory int, oldName, newName string) error {
	return unix.Renameat2(directory, oldName, directory, newName, unix.RENAME_NOREPLACE)
}
