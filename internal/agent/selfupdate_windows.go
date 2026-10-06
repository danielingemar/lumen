//go:build windows

package agent

import "errors"

// ExecInto is not available on Windows: run the installer again to update there.
func ExecInto(path string, args, env []string) error {
	return errors.New("updating in place is not supported on Windows")
}
