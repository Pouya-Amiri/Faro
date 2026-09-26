package main

import (
	"slices"
	"strings"
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

func TestWindowFrameCSSFollowsTheme(t *testing.T) {
	dark := windowFrameCSS(application.NewRGB(0x26, 0x26, 0x28))
	light := windowFrameCSS(application.NewRGB(0xf0, 0xee, 0xeb))
	for name, css := range map[string]string{"dark": dark, "light": light} {
		// GTK older than 4.16 rejects color-mix(), which would drop the rule.
		if strings.Contains(css, "color-mix") {
			t.Errorf("%s frame CSS uses color-mix()", name)
		}
		for _, want := range []string{"window.csd.faro {", "window.csd.faro:backdrop", "window.csd.faro.tiled-top:backdrop", "window.csd.faro.maximized", "window.solid-csd.faro", "border-radius: 15px"} {
			if !strings.Contains(css, want) {
				t.Errorf("%s frame CSS is missing %q", name, want)
			}
		}
	}
	if !strings.Contains(dark, "background-color: rgb(38, 38, 40)") || !strings.Contains(dark, "rgba(255, 255, 255, 0.15)") {
		t.Errorf("dark frame should use the page colour and a light edge:\n%s", dark)
	}
	if !strings.Contains(light, "background-color: rgb(240, 238, 235)") || !strings.Contains(light, "rgba(0, 0, 6, 0.15)") {
		t.Errorf("light frame should use the page colour and a dark edge:\n%s", light)
	}
}
