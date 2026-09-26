package main

import (
	"slices"
	"testing"

	"github.com/wailsapp/wails/v3/pkg/application"
)

func TestParseWindowChrome(t *testing.T) {
	cases := []struct {
		layout, doubleClick string
		side                string
		buttons             []string
		action              string
	}{
		{"", "", "right", []string{"minimize", "maximize", "close"}, "toggle-maximize"},
		{"appmenu:close", "toggle-maximize", "right", []string{"close"}, "toggle-maximize"},
		{"icon:minimize,maximize,close", "minimize", "right", []string{"minimize", "maximize", "close"}, "minimize"},
		{"close,minimize,maximize:", "lower", "left", []string{"close", "minimize", "maximize"}, "none"},
		{"close,close:menu", "menu", "left", []string{"close"}, "none"},
		{"menu:", "", "right", []string{}, "toggle-maximize"},
		{" appmenu : minimize , close ", "toggle-maximize-vertically", "right", []string{"minimize", "close"}, "toggle-maximize"},
	}
	for _, test := range cases {
		chrome := parseWindowChrome(test.layout, test.doubleClick)
		if chrome.ButtonsSide != test.side || !slices.Equal(chrome.Buttons, test.buttons) || chrome.DoubleClick != test.action {
			t.Errorf("parseWindowChrome(%q, %q) = %+v", test.layout, test.doubleClick, chrome)
		}
	}
}

func TestWindowBackgroundRoundTrip(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	if got := loadWindowBackground(); got != defaultWindowBackground {
		t.Fatalf("missing settings should use the default background, got %+v", got)
	}
	if err := saveWindowBackground("#F7F5F2"); err != nil {
		t.Fatal(err)
	}
	if got, want := loadWindowBackground(), application.NewRGB(0xf7, 0xf5, 0xf2); got != want {
		t.Fatalf("loadWindowBackground() = %+v, want %+v", got, want)
	}
	desktop := &Desktop{}
	if err := desktop.SetWindowBackground("not a colour"); err == nil {
		t.Fatal("invalid colours must be rejected")
	}
}
