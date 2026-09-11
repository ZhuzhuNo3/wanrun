package sourcefiles

import (
	"errors"
	"os"
)

func openRelativeDirectory(anchor *os.File, relative string) (*os.File, error) {
	return openRelative(anchor, relative, true)
}

func openRelativeRegular(anchor *os.File, relative string) (*os.File, error) {
	return openRelative(anchor, relative, false)
}

func openRelative(anchor *os.File, relative string, wantDirectory bool) (*os.File, error) {
	if anchor == nil {
		return nil, errors.New("source backing anchor is absent")
	}
	if relative == "." {
		if !wantDirectory {
			return nil, errors.New("regular source backing path is invalid")
		}
		return duplicateSourceDescriptor(anchor, "source-backing-directory")
	}
	return openBackingObject(anchor, splitRelative(relative), wantDirectory)
}
