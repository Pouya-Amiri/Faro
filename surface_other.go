//go:build !linux || !cgo || gtk3 || server

package main

import "github.com/wailsapp/wails/v3/pkg/application"

// Window shaping is a Linux/GTK4 concern (see surface_linux.go). Everywhere
// else the native toolkit or window manager owns the window corners.

const nativeClientDecorations = false

const revealWhenReady = false

func prepareWindowSurface(application.Window, application.RGBA) bool { return true }

func setWindowFrameStyle(application.RGBA) {}

func platformWindowChrome() (layout, doubleClick string) { return "", "" }
