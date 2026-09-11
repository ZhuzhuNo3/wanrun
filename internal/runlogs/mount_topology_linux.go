//go:build linux

package runlogs

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

type backingLocation struct {
	device uint64
	path   string
}

type descriptorBacking struct {
	location    backingLocation
	mountID     uint64
	visiblePath string
}

type mountRecord struct {
	id         uint64
	parentID   uint64
	device     uint64
	root       string
	mountPoint string
}

type mountTopology struct {
	mounts []mountRecord
	byID   map[uint64]mountRecord
}

func readMountTopology() (mountTopology, error) {
	file, err := os.Open("/proc/thread-self/mountinfo")
	if err != nil {
		return mountTopology{}, err
	}
	defer file.Close()
	topology := mountTopology{byID: make(map[uint64]mountRecord)}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	for scanner.Scan() {
		mount, err := parseMountRecord(scanner.Text())
		if err != nil {
			return mountTopology{}, err
		}
		if _, duplicate := topology.byID[mount.id]; duplicate {
			return mountTopology{}, fmt.Errorf("mountinfo repeats mount id %d", mount.id)
		}
		topology.mounts = append(topology.mounts, mount)
		topology.byID[mount.id] = mount
	}
	if err := scanner.Err(); err != nil {
		return mountTopology{}, err
	}
	if len(topology.mounts) == 0 {
		return mountTopology{}, errors.New("mountinfo is empty")
	}
	return topology, nil
}

func parseMountRecord(line string) (mountRecord, error) {
	fields := strings.Fields(line)
	separator := -1
	for index := 6; index < len(fields); index++ {
		if fields[index] == "-" {
			separator = index
			break
		}
	}
	if len(fields) < 10 || separator < 6 || separator+3 >= len(fields) {
		return mountRecord{}, errors.New("mountinfo record is malformed")
	}
	id, idErr := strconv.ParseUint(fields[0], 10, 64)
	parentID, parentErr := strconv.ParseUint(fields[1], 10, 64)
	device, deviceErr := parseMountDevice(fields[2])
	root, rootErr := unescapeMountPath(fields[3])
	mountPoint, pointErr := unescapeMountPath(fields[4])
	if err := errors.Join(idErr, parentErr, deviceErr, rootErr, pointErr); err != nil {
		return mountRecord{}, fmt.Errorf("parse mountinfo record: %w", err)
	}
	if id == 0 || root == "" || !validMountPath(mountPoint) {
		return mountRecord{}, fmt.Errorf("mountinfo record %d has invalid root or mount point", id)
	}
	return mountRecord{id: id, parentID: parentID, device: device, root: root,
		mountPoint: mountPoint}, nil
}

func parseMountDevice(value string) (uint64, error) {
	majorText, minorText, found := strings.Cut(value, ":")
	if !found || majorText == "" || minorText == "" || strings.ContainsRune(minorText, ':') {
		return 0, errors.New("mountinfo device is malformed")
	}
	major, majorErr := strconv.ParseUint(majorText, 10, 32)
	minor, minorErr := strconv.ParseUint(minorText, 10, 32)
	if err := errors.Join(majorErr, minorErr); err != nil {
		return 0, err
	}
	return unix.Mkdev(uint32(major), uint32(minor)), nil
}

func unescapeMountPath(value string) (string, error) {
	var result strings.Builder
	for index := 0; index < len(value); {
		if value[index] != '\\' {
			result.WriteByte(value[index])
			index++
			continue
		}
		if index+4 > len(value) {
			return "", errors.New("mountinfo path escape is truncated")
		}
		escape := value[index+1 : index+4]
		if escape != "040" && escape != "011" && escape != "012" && escape != "134" {
			return "", errors.New("mountinfo path escape is unsupported")
		}
		parsed, err := strconv.ParseUint(escape, 8, 8)
		if err != nil {
			return "", errors.New("mountinfo path escape is invalid")
		}
		result.WriteByte(byte(parsed))
		index += 4
	}
	return result.String(), nil
}

func validMountPath(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path
}

func (topology mountTopology) backingLocationForDescriptor(fd int) (descriptorBacking, error) {
	var info unix.Stat_t
	if err := unix.Fstat(fd, &info); err != nil {
		return descriptorBacking{}, err
	}
	var statx unix.Statx_t
	if err := unix.Statx(fd, "", unix.AT_EMPTY_PATH|unix.AT_STATX_SYNC_AS_STAT,
		unix.STATX_MNT_ID, &statx); err != nil || statx.Mask&unix.STATX_MNT_ID == 0 {
		return descriptorBacking{}, errors.Join(errors.New("mount identity is unavailable"), err)
	}
	mount, found := topology.byID[statx.Mnt_id]
	if !found {
		return descriptorBacking{}, fmt.Errorf("mount %d is absent from mountinfo snapshot", statx.Mnt_id)
	}
	if mount.device != uint64(info.Dev) || !validMountPath(mount.root) {
		return descriptorBacking{}, errors.New("descriptor disagrees with mountinfo")
	}
	path, err := os.Readlink("/proc/thread-self/fd/" + strconv.Itoa(fd))
	if err != nil || !validMountPath(path) || strings.HasSuffix(path, " (deleted)") {
		return descriptorBacking{}, errors.Join(errors.New("descriptor path is unavailable"), err)
	}
	relative, err := filepath.Rel(mount.mountPoint, path)
	if err != nil || relative == ".." || filepath.IsAbs(relative) ||
		strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return descriptorBacking{}, errors.Join(errors.New("descriptor escaped its mount point"), err)
	}
	return descriptorBacking{location: backingLocation{device: mount.device,
		path: filepath.Join(mount.root, relative)}, mountID: mount.id, visiblePath: path}, nil
}

func (mount mountRecord) backingRoot() (backingLocation, error) {
	if !validMountPath(mount.root) {
		return backingLocation{}, fmt.Errorf("mount %d has no path-addressable backing root", mount.id)
	}
	return backingLocation{device: mount.device, path: mount.root}, nil
}
