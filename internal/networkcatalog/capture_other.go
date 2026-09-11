//go:build !linux

package networkcatalog

import (
	"context"
	"fmt"
	"runtime"
)

func capture(context.Context) (Snapshot, error) {
	return Snapshot{}, fmt.Errorf("network catalog capture is unsupported on %s", runtime.GOOS)
}
