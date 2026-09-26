package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// defaultWindowBackground matches the dark theme's --bg token.
var defaultWindowBackground = application.NewRGB(24, 24, 26)

var hexColour = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

// WindowChrome describes the platform title bar conventions the page mirrors
// with its own window controls.
type WindowChrome struct {
	// ButtonsSide is "left" or "right".
	ButtonsSide string `json:"buttonsSide"`
	// Buttons lists "minimize", "maximize" and "close" in display order.
	Buttons []string `json:"buttons"`
	// DoubleClick is "toggle-maximize", "minimize" or "none".
	DoubleClick string `json:"doubleClick"`
}

// WindowReady is called by the page once its first render is complete.
func (d *Desktop) WindowReady() { d.revealWindow() }

// WindowChrome returns the desktop's title bar button layout and double-click
// behaviour.
func (d *Desktop) WindowChrome() WindowChrome {
	return parseWindowChrome(platformWindowChrome())
}

// SetWindowBackground remembers the page's resolved background colour so the
// next launch paints the window in the right theme before the page loads.
func (d *Desktop) SetWindowBackground(colour string) error {
	colour = strings.TrimSpace(colour)
	if !hexColour.MatchString(colour) {
		return errors.New("window background must be a #rrggbb colour")
	}
	if d.window != nil {
		d.window.SetBackgroundColour(parseHexColour(colour))
	}
	return saveWindowBackground(colour)
}

// revealWindow shows the main window exactly once, after installing the
// platform surface. It is called by the page, or by a fallback timer if the
// page never reports that it is ready.
func (d *Desktop) revealWindow() {
	if d.window == nil {
		return
	}
	d.revealOnce.Do(func() {
		prepareWindowSurface(d.window)
		d.window.Show()
	})
}

func parseWindowChrome(layout, doubleClick string) WindowChrome {
	chrome := WindowChrome{ButtonsSide: "right", Buttons: []string{"minimize", "maximize", "close"}}
	if layout = strings.TrimSpace(layout); layout != "" {
		left, right, _ := strings.Cut(layout, ":")
		if buttons := windowButtons(right); len(buttons) != 0 {
			chrome.Buttons = buttons
		} else if buttons := windowButtons(left); len(buttons) != 0 {
			chrome.ButtonsSide, chrome.Buttons = "left", buttons
		} else {
			chrome.Buttons = []string{}
		}
	}
	switch strings.TrimSpace(doubleClick) {
	case "minimize":
		chrome.DoubleClick = "minimize"
	case "none", "lower", "menu":
		chrome.DoubleClick = "none"
	default:
		chrome.DoubleClick = "toggle-maximize"
	}
	return chrome
}

func windowButtons(section string) []string {
	buttons := []string{}
	seen := map[string]bool{}
	for _, name := range strings.Split(section, ",") {
		name = strings.TrimSpace(name)
		if (name == "minimize" || name == "maximize" || name == "close") && !seen[name] {
			seen[name] = true
			buttons = append(buttons, name)
		}
	}
	return buttons
}

type windowSettings struct {
	Background string `json:"background"`
}

func windowSettingsPath() (string, error) {
	root, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "faro", "window.json"), nil
}

func loadWindowBackground() application.RGBA {
	path, err := windowSettingsPath()
	if err != nil {
		return defaultWindowBackground
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) > 4096 {
		return defaultWindowBackground
	}
	var settings windowSettings
	if json.Unmarshal(data, &settings) != nil || !hexColour.MatchString(settings.Background) {
		return defaultWindowBackground
	}
	return parseHexColour(settings.Background)
}

func saveWindowBackground(colour string) error {
	path, err := windowSettingsPath()
	if err != nil {
		return err
	}
	data, err := json.Marshal(windowSettings{Background: strings.ToLower(colour)})
	if err != nil {
		return err
	}
	if existing, readErr := os.ReadFile(path); readErr == nil && string(existing) == string(data) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

func parseHexColour(colour string) application.RGBA {
	value, err := strconv.ParseUint(strings.TrimPrefix(colour, "#"), 16, 32)
	if err != nil {
		return defaultWindowBackground
	}
	return application.NewRGB(uint8(value>>16), uint8(value>>8), uint8(value))
}
