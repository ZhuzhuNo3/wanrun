//go:build linux

package childprocesses

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

const (
	helperArgument      = "--transferlanes-internal-child-process"
	helperNamespaceFD   = 3
	helperBarrierFD     = 4
	helperReadyFD       = 5
	helperPayloadFD     = 6
	helperResolverFD    = 7
	helperCapabilityFD  = 8
	helperReleaseMarker = byte(0xa7)
	maximumResolverSize = 64 * 1024
)

// MaybeRunHelper recognizes only a child launched with the fixed inherited descriptor protocol.
func MaybeRunHelper(argv []string) (bool, int) {
	if len(argv) != 1 || argv[0] != helperArgument {
		return false, 0
	}
	if err := runInheritedHelper(); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "transferlanes child process: %v\n", err)
		return true, 1
	}
	return true, 0
}

func runInheritedHelper() error {
	runtime.LockOSThread()
	request, err := decodeHelperRequest(os.NewFile(helperPayloadFD, "child-payload"))
	if err != nil {
		return err
	}
	if err := validateHelperDescriptors(request); err != nil {
		return err
	}
	if _, err := unix.FcntlInt(uintptr(helperReadyFD), unix.F_SETFD, unix.FD_CLOEXEC); err != nil {
		return fmt.Errorf("protect child exec handoff descriptor: %w", err)
	}
	if err := unix.Setns(helperNamespaceFD, unix.CLONE_NEWNET); err != nil {
		return fmt.Errorf("enter child network namespace: %w", err)
	}
	if err := enterPrivateMounts(); err != nil {
		return err
	}
	if request.view.root.inode != 0 {
		if err := installPrivateViewAlias(request); err != nil {
			return err
		}
	}
	if err := installPrivateResolver(); err != nil {
		return err
	}
	if err := os.Chdir(request.cwd); err != nil {
		return fmt.Errorf("enter child working directory: %w", err)
	}
	executable, err := resolveExecutable(request.argv[0], request.environment)
	if err != nil {
		return err
	}
	if err := establishChildSession(request.interactive); err != nil {
		return err
	}
	if err := writeHelperHandoff(helperReadyWriter{}, helperHandoff{kind: helperPrepared}); err != nil {
		return err
	}
	released, err := awaitBarrier()
	if err != nil || !released {
		return err
	}
	closeHelperControlDescriptors()
	if err := unix.Exec(executable, request.argv, request.environment); err != nil {
		execErr := fmt.Errorf("exec exact child argv: %w", err)
		reportErr := writeHelperHandoff(helperReadyWriter{}, helperHandoff{kind: helperExecFailed,
			diagnostic: execErr.Error()})
		_ = unix.Close(helperReadyFD)
		return errors.Join(execErr, reportErr)
	}
	return nil
}

type helperReadyWriter struct{}

func (helperReadyWriter) Write(contents []byte) (int, error) {
	for {
		written, err := unix.Write(helperReadyFD, contents)
		if err == unix.EINTR {
			continue
		}
		return written, err
	}
}

func validateHelperDescriptors(request helperRequest) error {
	var namespace, resolver unix.Stat_t
	if err := unix.Fstat(helperNamespaceFD, &namespace); err != nil || namespace.Ino != request.namespaceInode {
		return errors.Join(fmt.Errorf("child namespace descriptor identity changed"), err)
	}
	if err := validatePipeDirection(helperBarrierFD, unix.O_RDONLY); err != nil {
		return fmt.Errorf("validate child barrier: %w", err)
	}
	if err := validatePipeDirection(helperReadyFD, unix.O_WRONLY); err != nil {
		return fmt.Errorf("validate child ready pipe: %w", err)
	}
	if err := unix.Fstat(helperResolverFD, &resolver); err != nil || resolver.Mode&unix.S_IFMT != unix.S_IFREG {
		return errors.Join(fmt.Errorf("child resolver descriptor is not a regular file"), err)
	}
	if request.execChannel.inode != 0 {
		var channel unix.Stat_t
		if err := unix.Fstat(helperCapabilityFD, &channel); err != nil ||
			channel.Mode&unix.S_IFMT != unix.S_IFSOCK || uint64(channel.Dev) != request.execChannel.device ||
			channel.Ino != request.execChannel.inode {
			return errors.Join(fmt.Errorf("child exec channel descriptor identity changed"), err)
		}
	}
	if request.view.root.inode != 0 {
		if err := validateViewDescriptor(request.view); err != nil {
			return err
		}
	}
	return nil
}

func validateViewDescriptor(expected viewDescriptorIdentity) error {
	var root unix.Stat_t
	if err := unix.Fstat(helperCapabilityFD, &root); err != nil ||
		root.Mode&unix.S_IFMT != unix.S_IFDIR || uint64(root.Dev) != expected.root.device ||
		root.Ino != expected.root.inode {
		return errors.Join(fmt.Errorf("child file-view root identity changed"), err)
	}
	return nil
}

func validatePipeDirection(fd, access int) error {
	var info unix.Stat_t
	if err := unix.Fstat(fd, &info); err != nil {
		return err
	}
	if info.Mode&unix.S_IFMT != unix.S_IFIFO {
		return fmt.Errorf("descriptor is not a pipe")
	}
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0)
	if err != nil {
		return err
	}
	if flags&unix.O_ACCMODE != access {
		return fmt.Errorf("pipe direction is invalid")
	}
	return nil
}

func enterPrivateMounts() error {
	if err := unix.Unshare(unix.CLONE_NEWNS); err != nil {
		return fmt.Errorf("create private child mount namespace: %w", err)
	}
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("make child mounts private: %w", err)
	}
	return installPrivateMountAuthority()
}

func installPrivateMountAuthority() error {
	var authority unix.Stat_t
	if err := unix.Stat(descriptorBoundViewAuthority, &authority); err != nil ||
		authority.Mode&unix.S_IFMT != unix.S_IFDIR {
		return errors.Join(fmt.Errorf("child mount authority is not a directory"), err)
	}
	flags := uintptr(unix.MS_NODEV | unix.MS_NOSUID | unix.MS_NOEXEC)
	if err := unix.Mount("transferlanes-child", descriptorBoundViewAuthority, "tmpfs", flags,
		"mode=0700,size=131072,nr_inodes=64"); err != nil {
		return fmt.Errorf("create private child mount authority: %w", err)
	}
	var mounted unix.Stat_t
	if err := unix.Stat(descriptorBoundViewAuthority, &mounted); err != nil ||
		mounted.Mode&unix.S_IFMT != unix.S_IFDIR ||
		mounted.Dev == authority.Dev && mounted.Ino == authority.Ino {
		return errors.Join(fmt.Errorf("private child mount authority was not isolated"), err)
	}
	return nil
}

func installPrivateViewAlias(request helperRequest) error {
	expected := request.view
	aliasPath, err := DescriptorBoundViewPath(expected.runID, request.transfer, expected.baseName)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(aliasPath), 0o700); err != nil {
		return fmt.Errorf("create private child view owner directories: %w", err)
	}
	if err := os.Mkdir(aliasPath, 0o700); err != nil {
		return fmt.Errorf("create private child view mount point: %w", err)
	}
	descriptorPath := "/proc/self/fd/" + strconv.Itoa(helperCapabilityFD)
	target, err := os.Readlink(descriptorPath)
	if err != nil || !filepath.IsAbs(target) || filepath.Clean(target) != target {
		return errors.Join(fmt.Errorf("resolve exact child view descriptor"), err)
	}
	if err := unix.Mount(target, aliasPath, "", unix.MS_BIND, ""); err != nil {
		return fmt.Errorf("bind exact private child view: %w", err)
	}
	var alias unix.Stat_t
	if err := unix.Stat(aliasPath, &alias); err != nil || alias.Mode&unix.S_IFMT != unix.S_IFDIR ||
		uint64(alias.Dev) != expected.root.device || alias.Ino != expected.root.inode {
		return errors.Join(fmt.Errorf("private child file-view alias identity changed"), err)
	}
	if err := unix.Close(helperCapabilityFD); err != nil {
		return fmt.Errorf("close mounted child file-view descriptor: %w", err)
	}
	return nil
}

func installPrivateResolver() error {
	content, err := readResolverDescriptor()
	if err != nil {
		return err
	}
	authority, err := unix.Open(descriptorBoundViewAuthority,
		unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("open private resolver authority: %w", err)
	}
	defer unix.Close(authority)
	const resolverName = ".resolver"
	resolver, err := unix.Openat(authority, resolverName,
		unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return fmt.Errorf("create private resolver source: %w", err)
	}
	defer unix.Close(resolver)
	if err := writeResolverContents(resolver, content); err != nil {
		return err
	}
	var sourceInfo, target unix.Stat_t
	if err := unix.Fstat(resolver, &sourceInfo); err != nil || sourceInfo.Mode&unix.S_IFMT != unix.S_IFREG {
		return errors.Join(fmt.Errorf("identify private resolver source"), err)
	}
	if err := unix.Stat("/etc/resolv.conf", &target); err != nil || target.Mode&unix.S_IFMT != unix.S_IFREG {
		return errors.Join(fmt.Errorf("resolver mount target is not a regular file"), err)
	}
	source := filepath.Join(descriptorBoundViewAuthority, resolverName)
	if err := attachResolverMount(resolver, source); err != nil {
		return err
	}
	if err := unix.Unlinkat(authority, resolverName, 0); err != nil {
		_ = unix.Unmount("/etc/resolv.conf", unix.MNT_DETACH)
		return fmt.Errorf("unlink private resolver source: %w", err)
	}
	var mounted unix.Stat_t
	if err := unix.Stat("/etc/resolv.conf", &mounted); err != nil ||
		mounted.Dev != sourceInfo.Dev || mounted.Ino != sourceInfo.Ino {
		_ = unix.Unmount("/etc/resolv.conf", unix.MNT_DETACH)
		return errors.Join(fmt.Errorf("private child resolver identity changed"), err)
	}
	flags := uintptr(unix.MS_BIND | unix.MS_REMOUNT | unix.MS_RDONLY)
	if err := unix.Mount("", "/etc/resolv.conf", "", flags, ""); err != nil {
		_ = unix.Unmount("/etc/resolv.conf", unix.MNT_DETACH)
		return fmt.Errorf("make private child resolver read-only: %w", err)
	}
	return nil
}

func readResolverDescriptor() ([]byte, error) {
	var source unix.Stat_t
	if err := unix.Fstat(helperResolverFD, &source); err != nil ||
		source.Mode&unix.S_IFMT != unix.S_IFREG || source.Size < 0 || source.Size > maximumResolverSize {
		return nil, errors.Join(fmt.Errorf("resolver source is not a bounded regular file"), err)
	}
	content := make([]byte, int(source.Size))
	for offset := 0; offset < len(content); {
		count, err := unix.Pread(helperResolverFD, content[offset:], int64(offset))
		if err == unix.EINTR {
			continue
		}
		if err != nil || count == 0 {
			return nil, errors.Join(fmt.Errorf("read resolver source"), err)
		}
		offset += count
	}
	return content, nil
}

func writeResolverContents(fd int, content []byte) error {
	for len(content) != 0 {
		count, err := unix.Write(fd, content)
		if err == unix.EINTR {
			continue
		}
		if err != nil || count <= 0 {
			return errors.Join(fmt.Errorf("write private resolver source"), err)
		}
		content = content[count:]
	}
	return nil
}

func attachResolverMount(descriptorFD int, descriptorPath string) error {
	if err := unix.Mount(descriptorPath, "/etc/resolv.conf", "", unix.MS_BIND, ""); err == nil {
		return nil
	} else if err != unix.EINVAL {
		return fmt.Errorf("install descriptor-bound child resolver: %w", err)
	}
	tree, err := unix.OpenTree(descriptorFD, "",
		unix.OPEN_TREE_CLONE|unix.OPEN_TREE_CLOEXEC|unix.AT_EMPTY_PATH)
	if err == unix.EINVAL {
		target, readErr := os.Readlink(descriptorPath)
		if readErr != nil || !filepath.IsAbs(target) || filepath.Clean(target) != target {
			return errors.Join(fmt.Errorf("resolve descriptor-bound child resolver"), readErr)
		}
		tree, err = unix.OpenTree(unix.AT_FDCWD, target,
			unix.OPEN_TREE_CLONE|unix.OPEN_TREE_CLOEXEC)
	}
	if err != nil {
		return fmt.Errorf("clone descriptor-bound child resolver mount: %w", err)
	}
	defer unix.Close(tree)
	if err := unix.MoveMount(tree, "", unix.AT_FDCWD, "/etc/resolv.conf",
		unix.MOVE_MOUNT_F_EMPTY_PATH); err != nil {
		return fmt.Errorf("install descriptor-bound child resolver mount: %w", err)
	}
	return nil
}

func establishChildSession(interactive bool) error {
	if _, err := unix.Setsid(); err != nil {
		return fmt.Errorf("establish child session: %w", err)
	}
	if !interactive {
		return nil
	}
	if err := unix.IoctlSetInt(unix.Stdin, unix.TIOCSCTTY, 0); err != nil {
		return fmt.Errorf("attach child controlling PTY: %w", err)
	}
	return nil
}

func awaitBarrier() (bool, error) {
	var marker [1]byte
	for {
		count, err := unix.Read(helperBarrierFD, marker[:])
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return false, fmt.Errorf("read child start barrier: %w", err)
		}
		if count == 0 {
			return false, nil
		}
		if count != 1 || marker[0] != helperReleaseMarker {
			return false, fmt.Errorf("child start barrier marker is invalid")
		}
		return true, nil
	}
}

func resolveExecutable(name string, environment []string) (string, error) {
	if strings.ContainsRune(name, filepath.Separator) {
		if err := validateExecutable(name); err != nil {
			return "", err
		}
		return name, nil
	}
	path := ""
	for _, value := range environment {
		if strings.HasPrefix(value, "PATH=") {
			path = strings.TrimPrefix(value, "PATH=")
			break
		}
	}
	for _, directory := range filepath.SplitList(path) {
		if directory == "" {
			directory = "."
		}
		candidate := filepath.Join(directory, name)
		if validateExecutable(candidate) == nil {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("child executable was not found in PATH")
}

func validateExecutable(path string) error {
	var info unix.Stat_t
	if err := unix.Stat(path, &info); err != nil {
		return fmt.Errorf("inspect child executable: %w", err)
	}
	if info.Mode&unix.S_IFMT != unix.S_IFREG {
		return fmt.Errorf("child executable is not a regular file")
	}
	if err := unix.Access(path, unix.X_OK); err != nil {
		return fmt.Errorf("child executable is not accessible: %w", err)
	}
	return nil
}

func closeHelperControlDescriptors() {
	for _, fd := range []int{helperNamespaceFD, helperBarrierFD, helperPayloadFD, helperResolverFD} {
		_ = unix.Close(fd)
	}
}

func waitStatusResult(status unix.WaitStatus) (int, int) {
	if status.Signaled() {
		return -1, int(status.Signal())
	}
	if status.Exited() {
		return status.ExitStatus(), 0
	}
	return -1, int(syscall.SIGKILL)
}
