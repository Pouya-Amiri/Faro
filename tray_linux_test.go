package main

import "testing"

// linuxSystemTray stands in for Wails' tray type of the same name, whose
// Activate and Event methods both call the tray's click handler.
type linuxSystemTray struct{ click func() }

//go:noinline
func (s *linuxSystemTray) Activate() { s.click() }

//go:noinline
func (s *linuxSystemTray) Event() { s.click() }

func TestTrayMenuOpeningTellsMenuFromClick(t *testing.T) {
	var opening bool
	tray := &linuxSystemTray{click: func() { opening = trayMenuOpening() }}

	tray.Activate()
	if opening {
		t.Fatal("a primary click was taken for the menu opening")
	}
	tray.Event()
	if !opening {
		t.Fatal("the menu opening was taken for a primary click")
	}
}
