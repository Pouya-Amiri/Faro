//go:build linux && cgo && !gtk3 && !server

package main

import "C"

// faroWindowChromeChanged is called by GTK, on its main thread, when the title
// bar layout settings change. The page is told from another goroutine so the
// signal handler returns at once.
//
//export faroWindowChromeChanged
func faroWindowChromeChanged() {
	go notifyWindowChromeChanged()
}
