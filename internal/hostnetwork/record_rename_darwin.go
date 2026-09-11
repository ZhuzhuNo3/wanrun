//go:build darwin

package hostnetwork

import "golang.org/x/sys/unix"

func renameNoReplaceAt(directory int, oldName, newName string) error {
	return unix.RenameatxNp(directory, oldName, directory, newName, unix.RENAME_EXCL)
}
