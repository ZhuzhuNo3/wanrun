//go:build darwin

package runlogs

import "github.com/ZhuzhuNo3/transferlanes/internal/rundirectory"

func rejectProtectedMountLocation(int, *rundirectory.RecoveryRootBorrow, int, []string) error {
	return nil
}
