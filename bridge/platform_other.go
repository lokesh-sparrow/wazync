//go:build !windows

package main

// setAutostart is only supported on Windows.
func setAutostart(enable bool, args string) error {
	return nil
}

// lockInstance is only enforced on Windows.
func lockInstance(path string) error {
	return nil
}

// autostartEnabled is only supported on Windows.
func autostartEnabled() bool {
	return false
}
