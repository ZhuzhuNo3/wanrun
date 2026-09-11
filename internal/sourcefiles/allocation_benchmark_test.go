package sourcefiles

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
)

var allocationBenchmarkSink TransferAllocation

func BenchmarkAllocateTransferItems(b *testing.B) {
	snapshot := benchmarkSourceSnapshot(b)
	weights := benchmarkTransferWeights(b, 8)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		allocated, err := AllocateTransferItems(snapshot, weights)
		if err != nil {
			b.Fatal(err)
		}
		allocationBenchmarkSink = allocated
	}
}

func benchmarkSourceSnapshot(b *testing.B) SourceSnapshot {
	b.Helper()
	root := filepath.Join(b.TempDir(), "source")
	createBenchmarkSourceTree(b, root)
	source, err := OpenSourceRoot(root)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		if err := source.Close(); err != nil {
			b.Error(err)
		}
	})
	snapshot, err := source.Scan(context.Background(), false)
	if err != nil {
		b.Fatal(err)
	}
	return snapshot
}

func createBenchmarkSourceTree(b *testing.B, root string) {
	b.Helper()
	for directory := range 64 {
		path := filepath.Join(root, fmt.Sprintf("group-%02d", directory%8),
			fmt.Sprintf("branch-%02d", directory))
		if err := os.MkdirAll(path, 0o700); err != nil {
			b.Fatal(err)
		}
		if directory%4 == 0 {
			if err := os.Mkdir(filepath.Join(path, "empty"), 0o700); err != nil {
				b.Fatal(err)
			}
		}
		for file := range 32 {
			size := 64 + (directory*17+file*29)%1024
			name := filepath.Join(path, fmt.Sprintf("file-%02d.bin", file))
			if err := os.WriteFile(name, make([]byte, size), 0o600); err != nil {
				b.Fatal(err)
			}
		}
	}
}

func benchmarkTransferWeights(b *testing.B, count int) TransferWeights {
	b.Helper()
	values := make([]TransferWeight, count)
	for index := range values {
		number, err := transfernumber.New(index + 1)
		if err != nil {
			b.Fatal(err)
		}
		values[index], err = NewTransferWeight(number, uint64(index+1))
		if err != nil {
			b.Fatal(err)
		}
	}
	weights, err := NewTransferWeights(values)
	if err != nil {
		b.Fatal(err)
	}
	return weights
}
