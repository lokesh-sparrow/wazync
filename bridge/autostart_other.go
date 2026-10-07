//go:build !windows

package main

// setAutostart is only supported on Windows.
func setAutostart(enable bool, args string) error {
	return nil
}
