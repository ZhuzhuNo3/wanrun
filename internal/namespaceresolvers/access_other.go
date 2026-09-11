//go:build !linux && !darwin

package namespaceresolvers

import (
	"errors"
	"os"
)

func duplicateResolverFile(*os.File) (*os.File, error) {
	return nil, errors.New("resolver capability requires a Unix descriptor")
}
