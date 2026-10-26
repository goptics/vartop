//go:build !linux && !darwin && !windows

package cmd

import (
	"fmt"
	"runtime"
)

func newSupervisor(runner) supervisor {
	return noService{fmt.Errorf("vartop has no login service on %s", runtime.GOOS)}
}
