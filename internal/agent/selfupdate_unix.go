//go:build !windows

package agent

import "syscall"

// ExecInto replaces this process with another program (same process id). It returns only if that failed.
func ExecInto(path string, args, env []string) error {
	if len(args) == 0 {
		args = []string{path}
	}
	args = append([]string{path}, args[1:]...)
	return syscall.Exec(path, args, env)
}
