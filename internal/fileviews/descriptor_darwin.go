//go:build darwin

package fileviews

import "os"

func openChildViewDescriptor(root *os.File) (*os.File, error) {
	return duplicateFileViewDescriptor(root, "child-file-view-root")
}
