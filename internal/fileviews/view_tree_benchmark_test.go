package fileviews

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/ZhuzhuNo3/transferlanes/internal/sourcefiles"
	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
)

var viewTreeBenchmarkSink *viewTree

func BenchmarkBuildViewTree(b *testing.B) {
	source, allocation := benchmarkViewAllocation(b)
	b.Cleanup(func() {
		if err := source.Close(); err != nil {
			b.Error(err)
		}
	})
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		tree, err := newViewTree(allocation)
		if err != nil {
			b.Fatal(err)
		}
		viewTreeBenchmarkSink = tree
	}
}

func benchmarkViewAllocation(b *testing.B) (*sourcefiles.SourceRoot, sourcefiles.TransferAllocation) {
	b.Helper()
	root := filepath.Join(b.TempDir(), "source")
	createBenchmarkViewSource(b, root)
	source, err := sourcefiles.OpenSourceRoot(root)
	if err != nil {
		b.Fatal(err)
	}
	snapshot, err := source.Scan(context.Background(), false)
	if err != nil {
		_ = source.Close()
		b.Fatal(err)
	}
	values := make([]sourcefiles.TransferWeight, 8)
	for index := range values {
		number, numberErr := transfernumber.New(index + 1)
		if numberErr != nil {
			_ = source.Close()
			b.Fatal(numberErr)
		}
		values[index], err = sourcefiles.NewTransferWeight(number, uint64(index+1))
		if err != nil {
			_ = source.Close()
			b.Fatal(err)
		}
	}
	weights, err := sourcefiles.NewTransferWeights(values)
	if err != nil {
		_ = source.Close()
		b.Fatal(err)
	}
	allocation, err := sourcefiles.AllocateTransferItems(snapshot, weights)
	if err != nil {
		_ = source.Close()
		b.Fatal(err)
	}
	return source, allocation
}

func createBenchmarkViewSource(b *testing.B, root string) {
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
