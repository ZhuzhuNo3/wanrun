//go:build linux || darwin

package fileviews

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/ZhuzhuNo3/transferlanes/internal/sourcefiles"
	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
)

func TestViewTreeFiltersFilesAndRequiredParentsWithoutCopyingTrees(t *testing.T) {
	root, allocation := viewTreeFixture(t)
	defer root.Close()
	tree, err := newViewTree(allocation)
	if err != nil {
		t.Fatal(err)
	}
	first, _ := transfernumber.New(1)
	second, _ := transfernumber.New(2)
	if got, want := childNames(tree.root, first), []string{"large", "nested"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("first root children=%v want=%v", got, want)
	}
	if got, want := childNames(tree.root, second), []string{"nested"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("second root children=%v want=%v", got, want)
	}
	nested := tree.root.child("nested", first)
	if nested == nil || nested != tree.root.child("nested", second) {
		t.Fatal("transfers did not share the immutable directory node")
	}
}

func viewTreeFixture(t *testing.T) (*sourcefiles.SourceRoot, sourcefiles.TransferAllocation) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "source")
	for name, contents := range map[string]string{
		"large": "12345678", "nested/small": "x",
	} {
		full := filepath.Join(path, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(path, "nested", "empty"), 0o700); err != nil {
		t.Fatal(err)
	}
	root, err := sourcefiles.OpenSourceRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := root.Scan(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	weights := make([]sourcefiles.TransferWeight, 2)
	for index := range weights {
		id, _ := transfernumber.New(index + 1)
		weights[index], _ = sourcefiles.NewTransferWeight(id, 1)
	}
	selected, _ := sourcefiles.NewTransferWeights(weights)
	allocation, err := sourcefiles.AllocateTransferItems(snapshot, selected)
	if err != nil {
		t.Fatal(err)
	}
	return root, allocation
}

func childNames(entry *viewEntry, transfer transfernumber.Number) []string {
	children := entry.visibleChildren(transfer)
	result := make([]string, len(children))
	for index, child := range children {
		result[index] = child.name
	}
	return result
}
