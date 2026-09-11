//go:build linux

package runlogs

import (
	"fmt"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/ZhuzhuNo3/transferlanes/internal/rundirectory"
)

type backingReachability struct{ subtrees []backingLocation }

func rejectProtectedMountLocation(sourceFD int, recovery *rundirectory.RecoveryRootBorrow,
	candidateFD int, missing []string,
) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	topology, err := readMountTopology()
	if err != nil {
		return fmt.Errorf("read mount topology for raw logs: %w", err)
	}
	candidate, err := topology.backingLocationForDescriptor(candidateFD)
	if err != nil {
		return fmt.Errorf("identify raw log directory: %w", err)
	}
	candidate.location.path = filepath.Join(append([]string{candidate.location.path}, missing...)...)
	source, err := directoryBackingSubtrees(topology, sourceFD)
	if err != nil {
		return fmt.Errorf("identify source reachability: %w", err)
	}
	if err := source.reject(candidate.location, "source directory"); err != nil {
		return err
	}
	recoveryReachability, err := recoveryBackingSubtrees(topology, recovery)
	if err != nil {
		return fmt.Errorf("identify recovery reachability: %w", err)
	}
	return recoveryReachability.reject(candidate.location, "recovery authority")
}

func recoveryBackingSubtrees(topology mountTopology,
	recovery *rundirectory.RecoveryRootBorrow,
) (backingReachability, error) {
	anchor, err := topology.backingLocationForDescriptor(int(recovery.Descriptor()))
	if err != nil {
		return backingReachability{}, err
	}
	missing := recovery.MissingPath()
	anchor.location.path = filepath.Join(append([]string{anchor.location.path}, missing...)...)
	if len(missing) != 0 {
		return backingReachability{subtrees: []backingLocation{anchor.location}}, nil
	}
	return backingSubtrees(topology, anchor, recovery.LogicalPath())
}

func directoryBackingSubtrees(topology mountTopology, fd int) (backingReachability, error) {
	anchor, err := topology.backingLocationForDescriptor(fd)
	if err != nil {
		return backingReachability{}, err
	}
	return backingSubtrees(topology, anchor, anchor.visiblePath)
}

func backingSubtrees(topology mountTopology, anchor descriptorBacking,
	logicalRoot string,
) (backingReachability, error) {
	reachable := backingReachability{subtrees: []backingLocation{anchor.location}}
	reachableMounts := map[uint64]bool{anchor.mountID: true}
	for added := true; added; {
		added = false
		for _, mount := range topology.mounts {
			if reachableMounts[mount.id] || !reachableMounts[mount.parentID] ||
				!pathWithin(mount.mountPoint, logicalRoot) {
				continue
			}
			backingRoot, err := mount.backingRoot()
			if err != nil {
				return backingReachability{}, err
			}
			reachableMounts[mount.id] = true
			reachable.subtrees = append(reachable.subtrees, backingRoot)
			added = true
		}
	}
	return reachable, nil
}

func (reachable backingReachability) reject(candidate backingLocation, protected string) error {
	if len(reachable.subtrees) == 0 {
		return fmt.Errorf("%s reachability is empty", protected)
	}
	for _, subtree := range reachable.subtrees {
		if candidate.device == subtree.device && pathWithin(candidate.path, subtree.path) {
			return fmt.Errorf("raw log directory resolves inside %s", protected)
		}
	}
	return nil
}

func pathWithin(candidate, authority string) bool {
	relative, err := filepath.Rel(authority, candidate)
	return err == nil && relative != ".." && !filepath.IsAbs(relative) &&
		!strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
