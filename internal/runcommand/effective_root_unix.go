//go:build unix

package runcommand

import (
	"errors"
	"os"
)

func requireEffectiveRoot() error {
	if os.Geteuid() != 0 {
		return errors.New("run requires effective root")
	}
	return nil
}
