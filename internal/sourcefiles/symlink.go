package sourcefiles

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const maximumSourceSymlinkDepth = 40

type resolvedSourceTarget struct {
	kind     sourceObjectKind
	identity sourceIdentity
	anchor   *os.File
	name     string
	size     uint64
}

type sourceSymlinkWalk struct {
	directory        *os.File
	components       []string
	requireDirectory bool
	followedLinks    int
}

func (scanner *sourceScanner) followSymlink(parent *os.File, name, logicalPath string,
	observed sourceIdentity,
) (SourceEntry, bool, error) {
	target, err := resolveSourceSymlink(parent, name, observed)
	if err != nil {
		return SourceEntry{}, false, fmt.Errorf("resolve source symlink %q: %w", logicalPath, err)
	}
	scanner.summary.followedSymlinks++
	switch target.kind {
	case sourceDirectoryObject:
		return scanner.captureSymlinkDirectory(logicalPath, target)
	case sourceRegularObject:
		return scanner.captureSymlinkFile(logicalPath, target)
	default:
		scanner.summary.ignoredSpecialFiles++
		return SourceEntry{}, false, nil
	}
}

func resolveSourceSymlink(parent *os.File, name string,
	observed sourceIdentity,
) (target resolvedSourceTarget, resultErr error) {
	linkTarget, err := readObservedSourceSymlink(parent, name, observed)
	if err != nil {
		return resolvedSourceTarget{}, err
	}
	walk, err := newSourceSymlinkWalk(parent, linkTarget)
	if err != nil {
		return resolvedSourceTarget{}, err
	}
	defer func() {
		if walk.directory != nil {
			resultErr = errors.Join(resultErr, walk.directory.Close())
			if resultErr != nil {
				target = resolvedSourceTarget{}
			}
		}
	}()
	return walk.resolve()
}

func newSourceSymlinkWalk(parent *os.File, linkTarget string) (*sourceSymlinkWalk, error) {
	directory, err := symlinkStartingDirectory(parent, linkTarget)
	if err != nil {
		return nil, err
	}
	return &sourceSymlinkWalk{directory: directory, components: sourcePathComponents(linkTarget),
		requireDirectory: strings.HasSuffix(linkTarget, string(filepath.Separator)), followedLinks: 1}, nil
}

func (walk *sourceSymlinkWalk) resolve() (resolvedSourceTarget, error) {
	for {
		component, exists := walk.nextComponent()
		if !exists {
			return walk.resolveCurrentDirectory()
		}
		if component == "" || component == "." {
			continue
		}
		identity, statErr := statSourceAt(walk.directory, component)
		if statErr != nil {
			return resolvedSourceTarget{}, statErr
		}
		if identity.kind == sourceSymlinkObject {
			if err := walk.followNestedSymlink(component, identity); err != nil {
				return resolvedSourceTarget{}, err
			}
			continue
		}
		if len(walk.components) > 0 || component == ".." {
			if err := walk.enterDirectory(component, identity); err != nil {
				return resolvedSourceTarget{}, err
			}
			continue
		}
		return walk.resolveFinalEntry(component, identity)
	}
}

func (walk *sourceSymlinkWalk) nextComponent() (string, bool) {
	if len(walk.components) == 0 {
		return "", false
	}
	component := walk.components[0]
	walk.components = walk.components[1:]
	return component, true
}

func (walk *sourceSymlinkWalk) followNestedSymlink(component string,
	identity sourceIdentity,
) error {
	if walk.followedLinks >= maximumSourceSymlinkDepth {
		return errors.New("source symlink target forms a link cycle")
	}
	walk.followedLinks++
	target, err := readObservedSourceSymlink(walk.directory, component, identity)
	if err != nil {
		return err
	}
	if filepath.IsAbs(target) {
		root, err := openSourceDirectoryPath(string(filepath.Separator))
		if err != nil {
			return err
		}
		if err := walk.replaceDirectory(root); err != nil {
			return err
		}
	}
	if len(walk.components) == 0 && strings.HasSuffix(target, string(filepath.Separator)) {
		walk.requireDirectory = true
	}
	walk.components = append(sourcePathComponents(target), walk.components...)
	return nil
}

func (walk *sourceSymlinkWalk) enterDirectory(component string, identity sourceIdentity) error {
	if identity.kind != sourceDirectoryObject {
		return fmt.Errorf("source symlink component %q is not a directory", component)
	}
	directory, err := openObservedSourceDirectory(walk.directory, component, identity)
	if err != nil {
		return err
	}
	return walk.replaceDirectory(directory)
}

func (walk *sourceSymlinkWalk) replaceDirectory(replacement *os.File) error {
	err := walk.directory.Close()
	walk.directory = nil
	if err != nil {
		return errors.Join(err, replacement.Close())
	}
	walk.directory = replacement
	return nil
}

func (walk *sourceSymlinkWalk) resolveCurrentDirectory() (resolvedSourceTarget, error) {
	identity, err := inspectDescriptor(walk.directory)
	if err != nil || identity.kind != sourceDirectoryObject {
		return resolvedSourceTarget{}, errors.Join(
			errors.New("source symlink target is not a directory"), err)
	}
	return walk.takeDirectoryTarget(identity), nil
}

func (walk *sourceSymlinkWalk) resolveFinalEntry(component string,
	identity sourceIdentity,
) (resolvedSourceTarget, error) {
	switch identity.kind {
	case sourceDirectoryObject:
		directory, err := openObservedSourceDirectory(walk.directory, component, identity)
		if err != nil {
			return resolvedSourceTarget{}, err
		}
		if err := walk.replaceDirectory(directory); err != nil {
			return resolvedSourceTarget{}, err
		}
		return walk.takeDirectoryTarget(identity), nil
	case sourceRegularObject:
		return walk.resolveRegularFile(component, identity)
	default:
		if walk.requireDirectory {
			return resolvedSourceTarget{}, errors.New("source symlink target is not a directory")
		}
		return resolvedSourceTarget{kind: sourceSpecialObject, identity: identity}, nil
	}
}

func (walk *sourceSymlinkWalk) takeDirectoryTarget(identity sourceIdentity) resolvedSourceTarget {
	directory := walk.directory
	walk.directory = nil
	return resolvedSourceTarget{kind: sourceDirectoryObject, identity: identity,
		anchor: directory, name: "."}
}

func (walk *sourceSymlinkWalk) resolveRegularFile(component string,
	identity sourceIdentity,
) (resolvedSourceTarget, error) {
	if walk.requireDirectory {
		return resolvedSourceTarget{}, errors.New("source symlink target is not a directory")
	}
	file, err := openSourceRegularAt(walk.directory, component)
	if err != nil {
		return resolvedSourceTarget{}, err
	}
	opened, size, inspectErr := inspectOpenedRegular(file)
	closeErr := file.Close()
	if inspectErr != nil || closeErr != nil || opened != identity {
		return resolvedSourceTarget{}, errors.Join(
			errors.New("source symlink target changed while opening"), inspectErr, closeErr)
	}
	parent := walk.directory
	walk.directory = nil
	return resolvedSourceTarget{kind: sourceRegularObject, identity: identity,
		anchor: parent, name: component, size: size}, nil
}

func readObservedSourceSymlink(parent *os.File, name string,
	observed sourceIdentity,
) (string, error) {
	if observed.kind != sourceSymlinkObject {
		return "", errors.New("observed source object is not a symlink")
	}
	before, err := statSourceAt(parent, name)
	if err != nil || before != observed {
		return "", errors.Join(errors.New("source symlink changed before reading"), err)
	}
	target, err := readSourceSymlinkAt(parent, name)
	if err != nil {
		return "", err
	}
	after, err := statSourceAt(parent, name)
	if err != nil || after != observed {
		return "", errors.Join(errors.New("source symlink changed while reading"), err)
	}
	return target, nil
}

func symlinkStartingDirectory(parent *os.File, target string) (*os.File, error) {
	if filepath.IsAbs(target) {
		return openSourceDirectoryPath(string(filepath.Separator))
	}
	return duplicateSourceDescriptor(parent, "source-symlink-parent")
}

func sourcePathComponents(path string) []string {
	return strings.Split(path, string(filepath.Separator))
}

func openObservedSourceDirectory(parent *os.File, name string,
	observed sourceIdentity,
) (*os.File, error) {
	directory, err := openSourceDirectoryAt(parent, name)
	if err != nil {
		return nil, err
	}
	opened, err := inspectDescriptor(directory)
	if err != nil || opened != observed {
		_ = directory.Close()
		return nil, errors.Join(errors.New("source directory changed while opening"), err)
	}
	return directory, nil
}

func (scanner *sourceScanner) captureSymlinkDirectory(logicalPath string,
	target resolvedSourceTarget,
) (SourceEntry, bool, error) {
	if _, cyclic := scanner.ancestors[target.identity]; cyclic {
		_ = target.anchor.Close()
		return SourceEntry{}, false,
			fmt.Errorf("source symlink %q forms a recursive directory cycle", logicalPath)
	}
	anchor, err := scanner.owner.addBackingDirectory(target.anchor)
	if err != nil {
		_ = target.anchor.Close()
		return SourceEntry{}, false, fmt.Errorf("retain source symlink target %q: %w", logicalPath, err)
	}
	scanner.ancestors[target.identity] = struct{}{}
	defer delete(scanner.ancestors, target.identity)
	captured, err := scanner.scanDirectory(target.anchor, logicalPath,
		BackingReference{anchor: anchor, relativePath: "."})
	return directoryEntry(captured), err == nil, err
}

func (scanner *sourceScanner) captureSymlinkFile(logicalPath string,
	target resolvedSourceTarget,
) (SourceEntry, bool, error) {
	anchor, err := scanner.owner.addBackingDirectory(target.anchor)
	if err != nil {
		_ = target.anchor.Close()
		return SourceEntry{}, false, fmt.Errorf("retain source symlink target %q: %w", logicalPath, err)
	}
	captured, err := newSourceFile(logicalPath, target.size,
		BackingReference{anchor: anchor, relativePath: target.name})
	return fileEntry(captured), err == nil, err
}
