//go:build !windows

package main

import "os"

func isWindowsService() bool { return false }

func runWindowsService() {}

func platformCommand(cmd string, args []string) bool { return false }

func defaultDataDir() string {
	if info, err := os.Stat("/data"); err == nil && info.IsDir() {
		return "/data"
	}
	return "./data"
}
