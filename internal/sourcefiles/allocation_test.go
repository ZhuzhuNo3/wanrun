package sourcefiles

import (
	"errors"
	"fmt"
	"math"
	"reflect"
	"sort"
	"testing"

	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
)

func TestAllocateUsesDeterministicWeightsAndStableEmptyDirectoryRotation(t *testing.T) {
	snapshot := fixtureSnapshot(t,
		map[string]uint64{"a": 100, "b": 60, "c": 40, "d": 20},
		[]string{"empty-a", "nested/empty-b", "nested/empty-c"})
	first, second := mustTransferNumber(t, 1), mustTransferNumber(t, 2)
	weights := mustTransferWeights(t, []TransferWeight{
		mustTransferWeight(t, second, 2), mustTransferWeight(t, first, 1),
	})

	allocation, err := AllocateTransferItems(snapshot, weights)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := encodeAllocation(allocation),
		"1=100:a|empty-a,nested/empty-c;2=120:b,c,d|nested/empty-b"; got != want {
		t.Fatalf("allocation=%s want=%s", got, want)
	}
	repeated, err := AllocateTransferItems(snapshot, weights)
	if err != nil || encodeAllocation(repeated) != encodeAllocation(allocation) {
		t.Fatalf("allocation is not deterministic: %v", err)
	}
	assertAllocationUnion(t, snapshot, allocation)
}

func TestAllocateAllowsOnlyEmptyDirectoriesAndKeepsEveryTransfer(t *testing.T) {
	snapshot := fixtureSnapshot(t, nil, []string{"a", "b"})
	weights := mustTransferWeights(t, []TransferWeight{
		mustTransferWeight(t, mustTransferNumber(t, 3), 1),
		mustTransferWeight(t, mustTransferNumber(t, 1), 1),
		mustTransferWeight(t, mustTransferNumber(t, 2), 1),
	})
	allocation, err := AllocateTransferItems(snapshot, weights)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := encodeAllocation(allocation), "1=0:|a;2=0:|b;3=0:|"; got != want {
		t.Fatalf("allocation=%s want=%s", got, want)
	}
}

func TestAllocateBreaksEqualSizeTiesByPath(t *testing.T) {
	snapshot := fixtureSnapshot(t, map[string]uint64{
		"z-last":   10,
		"a-first":  10,
		"m-third":  10,
		"b-second": 10,
	}, nil)
	weights := mustTransferWeights(t, []TransferWeight{
		mustTransferWeight(t, mustTransferNumber(t, 1), 1),
		mustTransferWeight(t, mustTransferNumber(t, 2), 1),
	})

	first, err := AllocateTransferItems(snapshot, weights)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := encodeAllocation(first), "1=20:a-first,m-third|;2=20:b-second,z-last|"; got != want {
		t.Fatalf("equal-size allocation=%s want=%s", got, want)
	}
	second, err := AllocateTransferItems(snapshot, weights)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := encodeAllocation(second), encodeAllocation(first); got != want {
		t.Fatalf("repeated equal-size allocation=%s want=%s", got, want)
	}
}

func TestAllocateRejectsMissingSnapshotOrWeights(t *testing.T) {
	weight := mustTransferWeights(t, []TransferWeight{mustTransferWeight(t, mustTransferNumber(t, 1), 1)})
	if _, err := AllocateTransferItems(SourceSnapshot{}, weight); !errors.Is(err, ErrNoTransferItems) {
		t.Fatalf("zero snapshot error=%v", err)
	}
	snapshot := fixtureSnapshot(t, map[string]uint64{"file": 1}, nil)
	if _, err := AllocateTransferItems(snapshot, TransferWeights{}); err == nil {
		t.Fatal("zero weights were accepted")
	}
}

func TestAllocateKeepsLargeBytesAndWeightsExact(t *testing.T) {
	first, second := mustTransferNumber(t, 1), mustTransferNumber(t, 2)
	snapshot := fixtureSnapshot(t, map[string]uint64{
		"a": math.MaxUint64 / 2, "b": math.MaxUint64/2 - 1, "c": 1,
	}, nil)
	weights := mustTransferWeights(t, []TransferWeight{
		mustTransferWeight(t, first, math.MaxUint64),
		mustTransferWeight(t, second, math.MaxUint64-1),
	})
	allocation, err := AllocateTransferItems(snapshot, weights)
	if err != nil {
		t.Fatal(err)
	}
	if owner, ok := allocation.OwnerOf("c"); !ok || owner != second {
		t.Fatalf("c owner=%v,%v want transfer 2", owner, ok)
	}
	wantTotal := uint64(math.MaxUint64 / 2)
	for _, transfer := range []transfernumber.Number{first, second} {
		if total, ok := allocation.TotalBytes(transfer); !ok || total != wantTotal {
			t.Fatalf("transfer %d total=%d,%v want %d", transfer.Value(), total, ok, wantTotal)
		}
	}
	assertAllocationUnion(t, snapshot, allocation)
}

func fixtureSnapshot(t *testing.T, files map[string]uint64, emptyDirs []string) SourceSnapshot {
	t.Helper()
	type mutableDirectory struct {
		children map[string]*mutableDirectory
		files    map[string]uint64
	}
	root := &mutableDirectory{children: map[string]*mutableDirectory{}, files: map[string]uint64{}}
	var directoryAt func(string) *mutableDirectory
	directoryAt = func(path string) *mutableDirectory {
		current := root
		if path == "." || path == "" {
			return current
		}
		for _, component := range splitRelative(path) {
			if current.children[component] == nil {
				current.children[component] = &mutableDirectory{children: map[string]*mutableDirectory{}, files: map[string]uint64{}}
			}
			current = current.children[component]
		}
		return current
	}
	for path, size := range files {
		directoryAt(parentPath(path)).files[entryNameForTest(path)] = size
	}
	for _, path := range emptyDirs {
		directoryAt(path)
	}
	var freeze func(*mutableDirectory, string) SourceDirectory
	freeze = func(value *mutableDirectory, relative string) SourceDirectory {
		var entries []SourceEntry
		for name, child := range value.children {
			path := joinRelative(relative, name)
			entries = append(entries, directoryEntry(freeze(child, path)))
		}
		for name, size := range value.files {
			path := joinRelative(relative, name)
			file, _ := newSourceFile(path, size)
			entries = append(entries, fileEntry(file))
		}
		directory, _ := newSourceDirectory(relative, BackingReference{relativePath: backingPath(relative)}, entries)
		return directory
	}
	snapshot, err := newSourceSnapshot("source", sourceIdentity{device: 1, inode: 1,
		kind: sourceDirectoryObject}, &sourceCapability{}, SourceTree{root: freeze(root, "")}, SourceSummary{})
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func parentPath(path string) string {
	for index := len(path) - 1; index >= 0; index-- {
		if path[index] == '/' {
			return path[:index]
		}
	}
	return ""
}

func entryNameForTest(path string) string {
	parent := parentPath(path)
	if parent == "" {
		return path
	}
	return path[len(parent)+1:]
}

func backingPath(path string) string {
	if path == "" {
		return "."
	}
	return path
}

func mustTransferNumber(t *testing.T, value int) transfernumber.Number {
	t.Helper()
	result, err := transfernumber.New(value)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func mustTransferWeight(t *testing.T, transfer transfernumber.Number, weight uint64) TransferWeight {
	t.Helper()
	result, err := NewTransferWeight(transfer, weight)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func mustTransferWeights(t *testing.T, values []TransferWeight) TransferWeights {
	t.Helper()
	result, err := NewTransferWeights(values)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func encodeAllocation(value TransferAllocation) string {
	result := ""
	index := 0
	for transfer := range value.Transfers() {
		if index > 0 {
			result += ";"
		}
		var files []string
		for file := range value.Files(transfer) {
			files = append(files, file.RelativePath())
		}
		var directories []string
		for directory := range value.EmptyDirectories(transfer) {
			directories = append(directories, directory.RelativePath())
		}
		total, _ := value.TotalBytes(transfer)
		result += fmt.Sprintf("%d=%d:%s|%s", transfer.Value(), total,
			stringsJoin(files), stringsJoin(directories))
		index++
	}
	return result
}

func stringsJoin(values []string) string {
	sort.Strings(values)
	result := ""
	for index, value := range values {
		if index > 0 {
			result += ","
		}
		result += value
	}
	return result
}

func assertAllocationUnion(t *testing.T, snapshot SourceSnapshot, allocation TransferAllocation) {
	t.Helper()
	want := append(filePaths(snapshot), emptyPaths(snapshot)...)
	got := make([]string, 0, len(want))
	for transfer := range allocation.Transfers() {
		for file := range allocation.Files(transfer) {
			got = append(got, file.RelativePath())
			if owner, ok := allocation.OwnerOf(file.RelativePath()); !ok || owner != transfer {
				t.Fatalf("file %q owner=%v,%v want %v", file.RelativePath(), owner, ok, transfer)
			}
		}
		for directory := range allocation.EmptyDirectories(transfer) {
			got = append(got, directory.RelativePath())
			if owner, ok := allocation.OwnerOf(directory.RelativePath()); !ok || owner != transfer {
				t.Fatalf("directory %q owner=%v,%v want %v", directory.RelativePath(), owner, ok, transfer)
			}
		}
	}
	sort.Strings(want)
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("union=%v want=%v", got, want)
	}
}
