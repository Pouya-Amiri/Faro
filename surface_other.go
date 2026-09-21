//go:build !linux || !cgo || gtk3 || server

package main

import "github.com/wailsapp/wails/v3/pkg/application"

// Window shaping is a Linux/GTK4 concern (see surface_linux.go). Everywhere
// else the native toolkit or window manager owns the window corners.

func watchWindowSurface(application.Window) {}

func syncWindowSurface(application.Window) {}
