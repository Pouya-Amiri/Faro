package main

import (
	"runtime"
	"strings"

	"github.com/godbus/dbus/v5"
)

// trayHostAvailable reports whether a StatusNotifierItem host is running.
// Stock GNOME has none unless the AppIndicator extension is enabled, and
// hiding the window there would leave Faro running with no way back to it.
func trayHostAvailable() bool {
	conn, err := dbus.SessionBus()
	if err != nil {
		return false
	}
	var owned bool
	if conn.BusObject().Call("org.freedesktop.DBus.NameHasOwner", 0, "org.kde.StatusNotifierWatcher").Store(&owned) != nil || !owned {
		return false
	}
	registered, err := conn.Object("org.kde.StatusNotifierWatcher", "/StatusNotifierWatcher").
		GetProperty("org.kde.StatusNotifierWatcher.IsStatusNotifierHostRegistered")
	if err != nil {
		return false
	}
	host, ok := registered.Value().(bool)
	return ok && host
}

// trayMenuOpening reports whether the tray click handler is running because
// the tray menu opened. Wails' Linux tray calls the click handler both for a
// primary click (StatusNotifierItem.Activate) and when the host opens the
// menu (a dbusmenu "opened" Event, which is what a right click sends), and it
// exposes no separate hook for the latter, so the caller tells them apart.
func trayMenuOpening() bool {
	callers := make([]uintptr, 8)
	frames := runtime.CallersFrames(callers[:runtime.Callers(2, callers)])
	for {
		frame, more := frames.Next()
		if strings.HasSuffix(frame.Function, ".(*linuxSystemTray).Event") {
			return true
		}
		if !more {
			return false
		}
	}
}
