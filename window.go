package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
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
	// Style is the look the controls take: "gnome", "kde" or "windows".
	Style string `json:"style"`
}

// WindowReady is called by the page once its first render is complete.
func (d *Desktop) WindowReady() { d.revealWindow() }

// WindowChrome returns the desktop's title bar button layout and double-click
// behaviour.
func (d *Desktop) WindowChrome() WindowChrome {
	if runtime.GOOS == "darwin" {
		// macOS draws its own traffic lights on the left of the window.
		return WindowChrome{ButtonsSide: "left", Buttons: []string{}, DoubleClick: "toggle-maximize", Style: "mac"}
	}
	chrome := parseWindowChrome(platformWindowChrome())
	chrome.Style = windowControlStyle(runtime.GOOS, os.Getenv("XDG_CURRENT_DESKTOP"))
	return chrome
}

// windowControlStyle picks the window-control look that matches the desktop.
// Other Linux desktops get the GNOME/GTK look, which is what GTK4's own
// client-side decorations use there too.
func windowControlStyle(goos, desktop string) string {
	switch {
	case goos == "windows":
		return "windows"
	case goos != "linux":
		return "windows"
	case strings.Contains(strings.ToUpper(desktop), "KDE"):
		return "kde"
	default:
		return "gnome"
	}
}

// SetWindowBackground remembers the page's resolved background colour so the
// next launch paints the window in the right theme before the page loads.
func (d *Desktop) SetWindowBackground(colour string) error {
	colour = strings.TrimSpace(colour)
	if !hexColour.MatchString(colour) {
		return errors.New("window background must be a #rrggbb colour")
	}
	background := parseHexColour(colour)
	d.backgroundMu.Lock()
	changed := d.background != background
	d.background = background
	d.backgroundMu.Unlock()
	if changed && d.window != nil {
		d.window.SetBackgroundColour(background)
		setWindowFrameStyle(background)
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
		d.backgroundMu.Lock()
		background := d.background
		d.backgroundMu.Unlock()
		prepareWindowSurface(d.window, background)
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

// windowFrameCSS styles GTK's client-side frame the way libadwaita styles
// GNOME's own windows: a faint inner highlight, a hairline edge and a soft
// shadow that lightens while the window is inactive. The frame's background is
// the page's background, so the anti-aliased rounded clip of the webview
// blends into the same colour instead of the GTK theme's (light) window
// colour, which showed as a pale outline around the corners. Only rgba() is
// used: color-mix() needs GTK 4.16.
func windowFrameCSS(background application.RGBA) string {
	edge := "rgba(0, 0, 6, 0.15)"
	if 0.2126*float64(background.Red)+0.7152*float64(background.Green)+0.0722*float64(background.Blue) < 128 {
		edge = "rgba(255, 255, 255, 0.15)"
	}
	fill := fmt.Sprintf("rgb(%d, %d, %d)", background.Red, background.Green, background.Blue)
	tiled := "window.csd.faro.tiled, window.csd.faro.tiled-top, window.csd.faro.tiled-bottom, window.csd.faro.tiled-left, window.csd.faro.tiled-right"
	tiledBackdrop := strings.ReplaceAll(tiled, ", ", ":backdrop, ") + ":backdrop"
	return fmt.Sprintf(`window.csd.faro {
  background-color: %[1]s;
  border-radius: %[3]dpx;
  outline: 1px solid rgba(255, 255, 255, 0.07);
  outline-offset: -1px;
  box-shadow: 0 0 14px 5px rgba(0, 0, 0, 0.15), 0 0 5px 2px rgba(0, 0, 0, 0.1), 0 0 0 1px rgba(0, 0, 0, 0.05);
}
window.csd.faro:backdrop {
  box-shadow: 0 0 14px 5px transparent, 0 0 10px 5px rgba(0, 0, 0, 0.08), 0 0 0 1px rgba(0, 0, 0, 0.05);
  transition: box-shadow 200ms ease-out;
}
%[4]s, %[5]s {
  border-radius: 0;
  outline: none;
  box-shadow: 0 0 0 1px %[2]s, 0 0 0 20px transparent;
}
window.csd.faro.maximized, window.csd.faro.fullscreen,
window.csd.faro.maximized:backdrop, window.csd.faro.fullscreen:backdrop {
  border-radius: 0;
  outline: none;
  box-shadow: none;
  transition: none;
}
window.solid-csd.faro, window.solid-csd.faro:backdrop {
  margin: 0;
  padding: 4px;
  border: none;
  border-radius: 0;
  outline: none;
  background-color: %[1]s;
  box-shadow: inset 0 0 0 4px %[1]s, inset 0 0 0 1px %[2]s;
}
`, fill, edge, windowCornerRadius, tiled, tiledBackdrop)
}

// windowCornerRadius matches libadwaita's window radius.
const windowCornerRadius = 15

// windowSettings is read before the window exists, so it lives in a small
// file next to the app's other configuration rather than in the webview.
type windowSettings struct {
	Background           string `json:"background,omitempty"`
	HardwareAcceleration bool   `json:"hardwareAcceleration,omitempty"`
	// QuitOnClose turns off keeping Faro in the tray when its window closes.
	QuitOnClose bool `json:"quitOnClose,omitempty"`
}

func windowSettingsPath() (string, error) {
	root, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "faro", "window.json"), nil
}

func loadWindowSettings() windowSettings {
	var settings windowSettings
	path, err := windowSettingsPath()
	if err != nil {
		return settings
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) > 4096 || json.Unmarshal(data, &settings) != nil {
		return windowSettings{}
	}
	if !hexColour.MatchString(settings.Background) {
		settings.Background = ""
	}
	return settings
}

// updateWindowSettings applies change to the stored settings and writes them
// back atomically, keeping fields the change does not touch.
func updateWindowSettings(change func(*windowSettings)) error {
	path, err := windowSettingsPath()
	if err != nil {
		return err
	}
	settings := loadWindowSettings()
	before := settings
	change(&settings)
	if settings == before {
		if _, statErr := os.Stat(path); statErr == nil {
			return nil
		}
	}
	data, err := json.Marshal(settings)
	if err != nil {
		return err
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

func loadWindowBackground() application.RGBA {
	if background := loadWindowSettings().Background; background != "" {
		return parseHexColour(background)
	}
	return defaultWindowBackground
}

func saveWindowBackground(colour string) error {
	return updateWindowSettings(func(settings *windowSettings) { settings.Background = strings.ToLower(colour) })
}

// HardwareAcceleration reports whether GPU rendering is enabled for the next
// launch.
func (d *Desktop) HardwareAcceleration() bool { return loadWindowSettings().HardwareAcceleration }

// SetHardwareAcceleration stores the GPU rendering preference. WebKit chooses
// its rendering path when the window is created, so it applies on restart.
func (d *Desktop) SetHardwareAcceleration(enabled bool) error {
	return updateWindowSettings(func(settings *windowSettings) { settings.HardwareAcceleration = enabled })
}

func parseHexColour(colour string) application.RGBA {
	value, err := strconv.ParseUint(strings.TrimPrefix(colour, "#"), 16, 32)
	if err != nil {
		return defaultWindowBackground
	}
	return application.NewRGB(uint8(value>>16), uint8(value>>8), uint8(value))
}
