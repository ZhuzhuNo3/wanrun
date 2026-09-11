package sourcefiles

import (
	"errors"
	"fmt"
	"iter"
	"math"
	"math/bits"
	"sort"

	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
)

type TransferWeight struct {
	transfer transfernumber.Number
	weight   uint64
}

type TransferWeights struct{ values []TransferWeight }

// TransferAllocation is the immutable, complete owner assignment for one snapshot.
type TransferAllocation struct {
	snapshot       SourceSnapshot
	transfers      []transfernumber.Number
	transferIndex  map[transfernumber.Number]int
	fileOwners     []transfernumber.Number
	emptyDirOwners []transfernumber.Number
	totalBytes     []uint64
}

type transferLoad struct {
	weight TransferWeight
	bytes  uint64
}

func NewTransferWeight(transfer transfernumber.Number, weight uint64) (TransferWeight, error) {
	if transfer.Value() == 0 || weight == 0 {
		return TransferWeight{}, errors.New("source transfer weight must be positive")
	}
	return TransferWeight{transfer: transfer, weight: weight}, nil
}

func (weight TransferWeight) Transfer() transfernumber.Number { return weight.transfer }
func (weight TransferWeight) Weight() uint64                  { return weight.weight }

func NewTransferWeights(values []TransferWeight) (TransferWeights, error) {
	if len(values) == 0 || len(values) > transfernumber.Maximum {
		return TransferWeights{}, fmt.Errorf("source allocation requires 1..%d transfers", transfernumber.Maximum)
	}
	copied := append([]TransferWeight(nil), values...)
	sort.Slice(copied, func(i, j int) bool { return copied[i].transfer.Value() < copied[j].transfer.Value() })
	for index, weight := range copied {
		if weight.transfer.Value() == 0 || weight.weight == 0 {
			return TransferWeights{}, fmt.Errorf("source transfer weight %d is invalid", index)
		}
		if index > 0 && copied[index-1].transfer == weight.transfer {
			return TransferWeights{}, fmt.Errorf("source transfer %d is repeated", weight.transfer.Value())
		}
	}
	return TransferWeights{values: copied}, nil
}

// AllocateTransferItems assigns every transferable item exactly once using stable weighted load.
func AllocateTransferItems(snapshot SourceSnapshot, weights TransferWeights) (TransferAllocation, error) {
	if snapshot.contents == nil || snapshot.FileCount()+snapshot.EmptyDirectoryCount() == 0 {
		return TransferAllocation{}, ErrNoTransferItems
	}
	if len(weights.values) == 0 {
		return TransferAllocation{}, errors.New("source allocation requires transfer weights")
	}
	loads := make([]transferLoad, len(weights.values))
	transfers := make([]transfernumber.Number, len(weights.values))
	transferIndex := make(map[transfernumber.Number]int, len(weights.values))
	for index, weight := range weights.values {
		if weight.transfer.Value() == 0 || weight.weight == 0 {
			return TransferAllocation{}, fmt.Errorf("source transfer weight %d is invalid", index)
		}
		loads[index].weight = weight
		transfers[index] = weight.transfer
		transferIndex[weight.transfer] = index
	}
	fileOwners, err := allocateFiles(snapshot.contents.files, loads)
	if err != nil {
		return TransferAllocation{}, err
	}
	emptyDirOwners := allocateEmptyDirectories(len(snapshot.contents.emptyDirs), transfers)
	totals := make([]uint64, len(loads))
	for index, load := range loads {
		totals[index] = load.bytes
	}
	allocation := TransferAllocation{snapshot: snapshot, transfers: transfers,
		transferIndex: transferIndex, fileOwners: fileOwners, emptyDirOwners: emptyDirOwners,
		totalBytes: totals}
	if err := validateTransferAllocation(allocation); err != nil {
		return TransferAllocation{}, err
	}
	return allocation, nil
}

func allocateFiles(files []SourceFile, loads []transferLoad) ([]transfernumber.Number, error) {
	order := make([]int, len(files))
	for index := range order {
		order[index] = index
	}
	sort.Slice(order, func(left, right int) bool {
		leftFile, rightFile := files[order[left]], files[order[right]]
		if leftFile.size != rightFile.size {
			return leftFile.size > rightFile.size
		}
		return leftFile.relativePath < rightFile.relativePath
	})
	owners := make([]transfernumber.Number, len(files))
	for _, fileIndex := range order {
		chosen := minimumLoad(loads)
		if loads[chosen].bytes > math.MaxUint64-files[fileIndex].size {
			return nil, fmt.Errorf("source transfer %d byte total overflows", loads[chosen].weight.transfer.Value())
		}
		loads[chosen].bytes += files[fileIndex].size
		owners[fileIndex] = loads[chosen].weight.transfer
	}
	return owners, nil
}

func allocateEmptyDirectories(count int, transfers []transfernumber.Number) []transfernumber.Number {
	owners := make([]transfernumber.Number, count)
	for index := range owners {
		owners[index] = transfers[index%len(transfers)]
	}
	return owners
}

func minimumLoad(loads []transferLoad) int {
	minimum := 0
	for index := 1; index < len(loads); index++ {
		if compareLoad(loads[index], loads[minimum]) < 0 {
			minimum = index
		}
	}
	return minimum
}

func compareLoad(left, right transferLoad) int {
	leftHigh, leftLow := bits.Mul64(left.bytes, right.weight.weight)
	rightHigh, rightLow := bits.Mul64(right.bytes, left.weight.weight)
	if leftHigh < rightHigh || leftHigh == rightHigh && leftLow < rightLow {
		return -1
	}
	if leftHigh == rightHigh && leftLow == rightLow {
		return 0
	}
	return 1
}

func validateTransferAllocation(allocation TransferAllocation) error {
	contents := allocation.snapshot.contents
	if contents == nil || len(allocation.transfers) == 0 ||
		len(allocation.fileOwners) != len(contents.files) ||
		len(allocation.emptyDirOwners) != len(contents.emptyDirs) ||
		len(allocation.totalBytes) != len(allocation.transfers) ||
		len(allocation.transferIndex) != len(allocation.transfers) {
		return errors.New("source allocation is incomplete")
	}
	for index, transfer := range allocation.transfers {
		if transfer.Value() == 0 || allocation.transferIndex[transfer] != index {
			return errors.New("source allocation transfers are invalid")
		}
	}
	calculated := make([]uint64, len(allocation.transfers))
	for index, owner := range allocation.fileOwners {
		transferIndex, exists := allocation.transferIndex[owner]
		if !exists || calculated[transferIndex] > math.MaxUint64-contents.files[index].size {
			return errors.New("source allocation file owner or byte total is invalid")
		}
		calculated[transferIndex] += contents.files[index].size
	}
	for _, owner := range allocation.emptyDirOwners {
		if _, exists := allocation.transferIndex[owner]; !exists {
			return errors.New("source allocation empty-directory owner is invalid")
		}
	}
	for index := range calculated {
		if calculated[index] != allocation.totalBytes[index] {
			return errors.New("source allocation byte totals are inconsistent")
		}
	}
	return nil
}

func (allocation TransferAllocation) Snapshot() SourceSnapshot { return allocation.snapshot }
func (allocation TransferAllocation) BaseName() string         { return allocation.snapshot.BaseName() }
func (allocation TransferAllocation) BelongsTo(root *SourceRoot) bool {
	contents := allocation.snapshot.contents
	return root != nil && contents != nil && root.BaseName() == contents.baseName &&
		root.sameSnapshotSource(contents.sourceIdentity, contents.source)
}
func (allocation TransferAllocation) Transfers() iter.Seq[transfernumber.Number] {
	return func(yield func(transfernumber.Number) bool) {
		for _, transfer := range allocation.transfers {
			if !yield(transfer) {
				return
			}
		}
	}
}
func (allocation TransferAllocation) TransferCount() int { return len(allocation.transfers) }
func (allocation TransferAllocation) OwnerOf(relativePath string) (transfernumber.Number, bool) {
	contents := allocation.snapshot.contents
	if contents == nil {
		return transfernumber.Number{}, false
	}
	switch contents.entryKinds[relativePath] {
	case RegularFileEntry:
		index := contents.fileIndex[relativePath]
		return allocation.fileOwners[index], true
	case DirectoryEntry:
		index, exists := contents.emptyDirIndex[relativePath]
		if !exists {
			return transfernumber.Number{}, false
		}
		return allocation.emptyDirOwners[index], true
	default:
		return transfernumber.Number{}, false
	}
}
func (allocation TransferAllocation) TotalBytes(transfer transfernumber.Number) (uint64, bool) {
	index, exists := allocation.transferIndex[transfer]
	if !exists {
		return 0, false
	}
	return allocation.totalBytes[index], true
}
func (allocation TransferAllocation) Files(transfer transfernumber.Number) iter.Seq[SourceFile] {
	return func(yield func(SourceFile) bool) {
		if allocation.snapshot.contents == nil {
			return
		}
		for index, file := range allocation.snapshot.contents.files {
			if allocation.fileOwners[index] == transfer && !yield(file) {
				return
			}
		}
	}
}
func (allocation TransferAllocation) EmptyDirectories(transfer transfernumber.Number) iter.Seq[SourceDirectory] {
	return func(yield func(SourceDirectory) bool) {
		if allocation.snapshot.contents == nil {
			return
		}
		for index, directory := range allocation.snapshot.contents.emptyDirs {
			if allocation.emptyDirOwners[index] == transfer && !yield(directory) {
				return
			}
		}
	}
}
