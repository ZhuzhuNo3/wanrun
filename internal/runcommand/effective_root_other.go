//go:build !unix

package runcommand

import "errors"

func requireEffectiveRoot() error { return errors.New("run requires effective root on Unix") }
