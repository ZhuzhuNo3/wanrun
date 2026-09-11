package rundirectory

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZhuzhuNo3/transferlanes/internal/runid"
)

func TestRecoverySnapshotDoesNotObserveConcurrentCreateStaging(t *testing.T) {
	authority := filepath.Join(realPath(t, t.TempDir()), "transferlanes")
	setupOwner := mustTestOwner(t, authority, noOpDurability{})
	initializeEmptyAuthority(t, setupOwner)
	staleID := makeStaleRun(t, setupOwner)
	stagingPath := makeUnpublishedStaging(t, authority)

	recoverySync := newRecoverySnapshotHeldDurability(authority, stagingPath)
	t.Cleanup(recoverySync.Release)
	recoveryOwner := mustTestOwner(t, authority, recoverySync)
	recoveryDone := make(chan recoveryResult, 1)
	go func() {
		var recovered []runid.ID
		err := recoveryOwner.RecoverStale(context.Background(), func(stale *StaleRun) error {
			recovered = append(recovered, stale.ID())
			return nil
		})
		recoveryDone <- recoveryResult{ids: recovered, err: err}
	}()
	waitForPhase(t, recoverySync.held, "recovery snapshot held after staging cleanup")

	createSync := newAuthorityLockContenderDurability(filepath.Dir(authority))
	createOwner := mustTestOwner(t, authority, createSync)
	createID := newTestRunID(t)
	createDone := make(chan createResult, 1)
	createSync.Arm()
	go func() {
		live, err := createOwner.Create(createID)
		createDone <- createResult{live: live, err: err}
	}()
	waitForPhase(t, createSync.contending, "Create owner reaching authority lock")
	recoverySync.Release()

	recovery := waitForRecoveryResult(t, recoveryDone, "recovery after concurrent Create")
	created := waitForCreateResult(t, createDone, "concurrent Create")
	if recovery.err != nil {
		t.Fatalf("RecoverStale: %v", recovery.err)
	}
	assertRecoveredIDs(t, recovery.ids, staleID)
	if created.err != nil {
		t.Fatalf("concurrent Create: %v", created.err)
	}
	if created.live == nil {
		t.Fatal("concurrent Create returned no LiveRun")
	}
	if created.live.ID() != createID {
		t.Fatalf("created run ID = %s, want %s", created.live.ID(), createID)
	}
	if err := created.live.Complete(context.Background()); err != nil {
		t.Fatalf("complete concurrent run: %v", err)
	}
}

func TestRecoverySnapshotDoesNotObserveConcurrentCompletion(t *testing.T) {
	authority := filepath.Join(realPath(t, t.TempDir()), "transferlanes")
	completionSync := newAuthorityLockContenderDurability(filepath.Dir(authority))
	completionOwner := mustTestOwner(t, authority, completionSync)
	liveID := newTestRunID(t)
	live, err := completionOwner.Create(liveID)
	if err != nil {
		t.Fatalf("Create live run: %v", err)
	}

	setupOwner := mustTestOwner(t, authority, noOpDurability{})
	staleID := makeStaleRun(t, setupOwner)
	stagingPath := makeUnpublishedStaging(t, authority)
	recoverySync := newRecoverySnapshotHeldDurability(authority, stagingPath)
	t.Cleanup(recoverySync.Release)
	recoveryOwner := mustTestOwner(t, authority, recoverySync)
	recoveryDone := make(chan recoveryResult, 1)
	go func() {
		var recovered []runid.ID
		err := recoveryOwner.RecoverStale(context.Background(), func(stale *StaleRun) error {
			recovered = append(recovered, stale.ID())
			return nil
		})
		recoveryDone <- recoveryResult{ids: recovered, err: err}
	}()
	waitForPhase(t, recoverySync.held, "recovery snapshot held after staging cleanup")

	completionSync.Arm()
	completeDone := make(chan error, 1)
	go func() { completeDone <- live.Complete(context.Background()) }()
	waitForPhase(t, completionSync.contending, "Complete owner reaching authority lock")
	recoverySync.Release()

	recovery := waitForRecoveryResult(t, recoveryDone, "recovery after concurrent completion")
	if err := waitForErrorResult(t, completeDone, "concurrent completion"); err != nil {
		t.Fatalf("concurrent Complete: %v", err)
	}
	if recovery.err != nil {
		t.Fatalf("RecoverStale: %v", recovery.err)
	}
	assertRecoveredIDs(t, recovery.ids, staleID)
	assertPathAbsent(t, filepath.Join(authority, liveID.String()))
}

func TestCreatePublicationPendingPhasePrecedesFinalIdentity(t *testing.T) {
	authority := filepath.Join(realPath(t, t.TempDir()), "transferlanes")
	id := newTestRunID(t)
	publicationSync := newCreatePublicationPendingDurability(id)
	t.Cleanup(publicationSync.Release)
	owner := mustTestOwner(t, authority, publicationSync)
	result := make(chan createResult, 1)
	go func() {
		live, err := owner.Create(id)
		result <- createResult{live: live, err: err}
	}()

	waitForPhase(t, publicationSync.pending, "Create publication pending")
	assertDirectoryExists(t, publicationSync.stagingPath)
	assertPathAbsent(t, filepath.Join(authority, id.String()))
	publicationSync.Release()

	created := waitForCreateResult(t, result, "Create publication")
	if created.err != nil {
		t.Fatalf("Create: %v", created.err)
	}
	if created.live == nil {
		t.Fatal("Create returned no LiveRun")
	}
	assertDirectoryExists(t, filepath.Join(authority, id.String()))
	assertPathAbsent(t, publicationSync.stagingPath)
	if err := created.live.Complete(context.Background()); err != nil {
		t.Fatalf("complete published run: %v", err)
	}
}

func TestCompletionRenamePendingPhaseFollowsExactRunIsolation(t *testing.T) {
	authority := filepath.Join(realPath(t, t.TempDir()), "transferlanes")
	id := newTestRunID(t)
	completionSync := newCompletionRenamePendingDurability(authority, id)
	owner := mustTestOwner(t, authority, completionSync)
	live, err := owner.Create(id)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	completionSync.Arm()
	t.Cleanup(completionSync.Release)
	result := make(chan error, 1)
	go func() { result <- live.Complete(context.Background()) }()

	waitForPhase(t, completionSync.pending, "completion rename pending durability")
	assertPathAbsent(t, filepath.Join(authority, id.String()))
	assertDirectoryExists(t, completionSync.removingPath)
	completionSync.Release()

	if err := waitForErrorResult(t, result, "completion after rename durability"); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	assertPathAbsent(t, completionSync.removingPath)
}

func TestRecoveryCallbackRunsWithoutAuthorityLock(t *testing.T) {
	owner, _ := newTestOwner(t)
	makeStaleRun(t, owner)
	createdID := newTestRunID(t)
	done := make(chan error, 1)
	go func() {
		done <- owner.RecoverStale(context.Background(), func(*StaleRun) error {
			live, err := owner.Create(createdID)
			if err != nil {
				return err
			}
			return live.Close()
		})
	}()
	if err := waitForErrorResult(t, done, "recovery callback reacquiring authority"); err != nil {
		t.Fatalf("RecoverStale: %v", err)
	}
}

type createResult struct {
	live *LiveRun
	err  error
}

type recoveryResult struct {
	ids []runid.ID
	err error
}

func mustTestOwner(t *testing.T, authority string, durability durableFiles) *Owner {
	t.Helper()
	owner, err := newOwner(authority, durability)
	if err != nil {
		t.Fatalf("newOwner(%q): %v", authority, err)
	}
	return owner
}

func initializeEmptyAuthority(t *testing.T, owner *Owner) {
	t.Helper()
	live, err := owner.Create(newTestRunID(t))
	if err != nil {
		t.Fatalf("Create authority: %v", err)
	}
	if err := live.Complete(context.Background()); err != nil {
		t.Fatalf("Complete authority initializer: %v", err)
	}
}

func makeUnpublishedStaging(t *testing.T, authority string) string {
	t.Helper()
	staging := filepath.Join(authority, newStagingName(t))
	if err := os.Mkdir(staging, 0o700); err != nil {
		t.Fatalf("create unpublished staging: %v", err)
	}
	if err := os.WriteFile(filepath.Join(staging, livenessLockName), nil, 0o600); err != nil {
		t.Fatalf("create unpublished liveness lock: %v", err)
	}
	return staging
}

func waitForPhase(t *testing.T, phase <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-phase:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for phase %q", name)
	}
}

func waitForRecoveryResult(t *testing.T, result <-chan recoveryResult, name string) recoveryResult {
	t.Helper()
	select {
	case recovered := <-result:
		return recovered
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out joining %q", name)
		return recoveryResult{}
	}
}

func waitForCreateResult(t *testing.T, result <-chan createResult, name string) createResult {
	t.Helper()
	select {
	case created := <-result:
		return created
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out joining %q", name)
		return createResult{}
	}
}

func waitForErrorResult(t *testing.T, result <-chan error, name string) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out joining %q", name)
		return nil
	}
}

func assertRecoveredIDs(t *testing.T, got []runid.ID, want ...runid.ID) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("recovered IDs = %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("recovered IDs = %v, want %v", got, want)
		}
	}
}

type recoverySnapshotHeldDurability struct {
	authority   string
	staging     string
	held        chan struct{}
	release     chan struct{}
	heldOnce    sync.Once
	releaseOnce sync.Once
}

func newRecoverySnapshotHeldDurability(authority, staging string) *recoverySnapshotHeldDurability {
	return &recoverySnapshotHeldDurability{
		authority: authority,
		staging:   staging,
		held:      make(chan struct{}),
		release:   make(chan struct{}),
	}
}

func (durability *recoverySnapshotHeldDurability) Sync(file *os.File) error {
	if file.Name() != durability.authority {
		return nil
	}
	if _, err := os.Lstat(durability.staging); !errors.Is(err, os.ErrNotExist) {
		return err
	}
	durability.heldOnce.Do(func() { close(durability.held) })
	<-durability.release
	return nil
}

func (durability *recoverySnapshotHeldDurability) Release() {
	durability.releaseOnce.Do(func() { close(durability.release) })
}

type authorityLockContenderDurability struct {
	parent     string
	contending chan struct{}
	once       sync.Once
	armed      bool
}

func newAuthorityLockContenderDurability(parent string) *authorityLockContenderDurability {
	return &authorityLockContenderDurability{
		parent:     parent,
		contending: make(chan struct{}),
	}
}

func (durability *authorityLockContenderDurability) Arm() { durability.armed = true }

func (durability *authorityLockContenderDurability) Sync(file *os.File) error {
	if durability.armed && file.Name() == durability.parent {
		durability.once.Do(func() { close(durability.contending) })
	}
	return nil
}

type createPublicationPendingDurability struct {
	id          runid.ID
	pending     chan struct{}
	release     chan struct{}
	stagingPath string
	pendingOnce sync.Once
	releaseOnce sync.Once
}

func newCreatePublicationPendingDurability(id runid.ID) *createPublicationPendingDurability {
	return &createPublicationPendingDurability{
		id:      id,
		pending: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (durability *createPublicationPendingDurability) Sync(file *os.File) error {
	wantPrefix := stagingPrefix + durability.id.String() + "-"
	if !strings.HasPrefix(filepath.Base(file.Name()), wantPrefix) {
		return nil
	}
	durability.pendingOnce.Do(func() {
		durability.stagingPath = file.Name()
		close(durability.pending)
		<-durability.release
	})
	return nil
}

func (durability *createPublicationPendingDurability) Release() {
	durability.releaseOnce.Do(func() { close(durability.release) })
}

type completionRenamePendingDurability struct {
	authority    string
	id           runid.ID
	pending      chan struct{}
	release      chan struct{}
	removingPath string
	armed        bool
	pendingOnce  sync.Once
	releaseOnce  sync.Once
}

func newCompletionRenamePendingDurability(authority string, id runid.ID) *completionRenamePendingDurability {
	return &completionRenamePendingDurability{
		authority: authority,
		id:        id,
		pending:   make(chan struct{}),
		release:   make(chan struct{}),
	}
}

func (durability *completionRenamePendingDurability) Arm() { durability.armed = true }

func (durability *completionRenamePendingDurability) Sync(file *os.File) error {
	if !durability.armed || file.Name() != durability.authority {
		return nil
	}
	wantPrefix := removingPrefix + durability.id.String() + "-"
	entries, err := os.ReadDir(durability.authority)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), wantPrefix) {
			durability.pendingOnce.Do(func() {
				durability.removingPath = filepath.Join(durability.authority, entry.Name())
				close(durability.pending)
				<-durability.release
			})
			break
		}
	}
	return nil
}

func (durability *completionRenamePendingDurability) Release() {
	durability.releaseOnce.Do(func() { close(durability.release) })
}
