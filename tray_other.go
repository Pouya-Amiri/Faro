//go:build !linux

package main

// Windows and macOS always have a notification area or menu bar.
func trayHostAvailable() bool { return true }

// Windows and macOS report a primary click and opening the tray menu to
// separate handlers.
func trayMenuOpening() bool { return false }
