//go:build windows

package main

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows/registry"
)

const runKey = `Software\Microsoft\Windows\CurrentVersion\Run`
const runValue = "Wazync"

// setAutostart adds or removes the bridge from the current user's sign-in programs.
func setAutostart(enable bool, args string) error {
	key, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.SET_VALUE|registry.QUERY_VALUE)
	if err != nil {
		return err
	}
	defer key.Close()
	if !enable {
		err := key.DeleteValue(runValue)
		if err == registry.ErrNotExist {
			return nil
		}
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	return key.SetStringValue(runValue, fmt.Sprintf("\"%s\" %s", exe, args))
}
