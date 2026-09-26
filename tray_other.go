//go:build !linux

package main

// Windows and macOS always have a notification area or menu bar.
func trayHostAvailable() bool { return true }
