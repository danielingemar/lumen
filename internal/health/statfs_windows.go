//go:build windows

package health

import "errors"

func statfs(string) (uint64, uint64, error) { return 0, 0, errors.New("not measured on Windows") }
