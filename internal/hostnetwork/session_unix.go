//go:build linux || darwin

package hostnetwork

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
	"golang.org/x/sys/unix"
)

var errSessionClosing = errors.New("host-network session is closing")

// Session retains exclusive cleanup ownership for one complete network group.
type Session struct {
	mu           sync.Mutex
	closeMu      sync.Mutex
	owner        *Owner
	root         *os.File
	claim        networkClaim
	installation *hostInstallation
	namespaces   map[transfernumber.Number]*os.File
	leases       int
	drained      chan struct{}
	closing      bool
	closed       bool
}

// NamespaceLease owns one duplicate descriptor for an exact network namespace.
type NamespaceLease struct {
	once    sync.Once
	file    *os.File
	session *Session
	err     error
}

func newSession(owner *Owner, root *os.File, claim networkClaim,
	installation *hostInstallation) *Session {
	var namespaces map[transfernumber.Number]*os.File
	if installation != nil {
		namespaces = installation.namespaces
	}
	return &Session{owner: owner, root: root, claim: claim, installation: installation,
		namespaces: namespaces,
		drained:    make(chan struct{})}
}

// OpenNamespace borrows a descriptor-bound namespace capability for one transfer.
func (session *Session) OpenNamespace(id transfernumber.Number) (*NamespaceLease, error) {
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.closing || session.closed {
		return nil, errSessionClosing
	}
	base, exists := session.namespaces[id]
	if !exists {
		return nil, fmt.Errorf("transfer %d is not part of this host-network session", id.Value())
	}
	fd, err := unix.FcntlInt(base.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("duplicate transfer %d namespace descriptor: %w", id.Value(), err)
	}
	session.leases++
	return &NamespaceLease{file: os.NewFile(uintptr(fd), "transferlanes-network-namespace"), session: session}, nil
}

// Descriptor returns the lease-owned descriptor; Close releases it.
func (lease *NamespaceLease) Descriptor() uintptr { return lease.file.Fd() }

// Close releases the duplicate namespace descriptor exactly once.
func (lease *NamespaceLease) Close() error {
	lease.once.Do(func() {
		lease.err = lease.file.Close()
		lease.session.releaseLease()
	})
	return lease.err
}

func (session *Session) releaseLease() {
	session.mu.Lock()
	defer session.mu.Unlock()
	session.leases--
	if session.closing && session.leases == 0 {
		select {
		case <-session.drained:
		default:
			close(session.drained)
		}
	}
}

// Close prevents new leases, waits for borrowed descriptors, then removes the group.
// The caller must first terminate and reap every process that entered a leased namespace.
func (session *Session) Close(ctx context.Context) error {
	if err := session.beginClose(ctx); err != nil {
		return err
	}
	session.closeMu.Lock()
	defer session.closeMu.Unlock()
	session.mu.Lock()
	if session.closed {
		session.mu.Unlock()
		return nil
	}
	session.mu.Unlock()
	if err := session.owner.closeSession(ctx, session.root, session.claim, session.installation); err != nil {
		return err
	}
	closeNamespaceFiles(session.namespaces)
	return session.finishClose()
}

func (session *Session) beginClose(ctx context.Context) error {
	session.mu.Lock()
	if !session.closing {
		session.closing = true
		if session.leases == 0 {
			close(session.drained)
		}
	}
	drained := session.drained
	session.mu.Unlock()
	select {
	case <-drained:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (session *Session) finishClose() error {
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.closed {
		return nil
	}
	session.closed = true
	if session.root == nil {
		return nil
	}
	return session.root.Close()
}

func (owner *Owner) closeSession(ctx context.Context, root *os.File, expected networkClaim,
	installation *hostInstallation) error {
	lock, err := acquireHostLock(ctx, owner.lockPath)
	if err != nil {
		return err
	}
	defer lock.Close()
	evidence, exists, err := loadNetworkEvidence(root, expected.runID, owner.syncer)
	if err != nil {
		return err
	}
	if !exists || !evidence.activated || !claimsEqual(evidence.claim, expected) {
		return errors.New("host-network recovery evidence changed")
	}
	if err := installation.requireComplete(expected); err != nil {
		return err
	}
	if err := owner.host.Close(ctx, expected, installation); err != nil {
		return fmt.Errorf("close host network: %w", err)
	}
	return removeNetworkEvidence(root, evidence, owner.syncer)
}

func validateNamespaceFiles(claim networkClaim, files map[transfernumber.Number]*os.File) error {
	if len(files) != len(claim.transfers) {
		return errors.New("host returned an incomplete namespace descriptor set")
	}
	for _, value := range claim.transfers {
		if file := files[value.transfer]; file == nil {
			return fmt.Errorf("host returned no namespace descriptor for transfer %d", value.number)
		}
	}
	return nil
}

func closeNamespaceFiles(files map[transfernumber.Number]*os.File) {
	for _, file := range files {
		if file != nil {
			_ = file.Close()
		}
	}
}
