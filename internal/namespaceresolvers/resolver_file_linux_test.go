//go:build linux

package namespaceresolvers

import (
	"errors"
	"io"
	"testing"

	"golang.org/x/sys/unix"
)

func TestSealedResolverFileRetainsImmutableContents(t *testing.T) {
	want := []byte("nameserver 192.0.2.53\n")
	file, err := sealedResolverFile(want)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	got, err := io.ReadAll(file)
	if err != nil || string(got) != string(want) {
		t.Fatalf("resolver contents=%q error=%v", got, err)
	}
	seals, err := unix.FcntlInt(file.Fd(), unix.F_GET_SEALS, 0)
	if err != nil {
		t.Fatal(err)
	}
	wantSeals := unix.F_SEAL_SEAL | unix.F_SEAL_SHRINK | unix.F_SEAL_GROW | unix.F_SEAL_WRITE
	if seals&wantSeals != wantSeals {
		t.Fatalf("resolver seals=%#x want bits=%#x", seals, wantSeals)
	}
	if _, err := file.WriteAt([]byte{'x'}, 0); !errors.Is(err, unix.EPERM) {
		t.Fatalf("sealed resolver write error=%v, want EPERM", err)
	}
}
