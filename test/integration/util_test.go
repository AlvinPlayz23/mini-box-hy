//go:build integration

package integration

import (
	"os/exec"
	"strconv"
)

// runCmd runs an external command and returns its combined output.
func runCmd(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	return string(out), err
}

// parseHex64 parses a hexadecimal capability mask.
func parseHex64(s string) (uint64, error) {
	return strconv.ParseUint(s, 16, 64)
}
