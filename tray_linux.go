package main

import "github.com/godbus/dbus/v5"

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
