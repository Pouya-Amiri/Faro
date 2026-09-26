package main

import (
	_ "embed"
	"os"
	"sync/atomic"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
)

//go:embed build/tray-icon.png
var trayIcon []byte

// trayController keeps Faro running in the system tray after its window is
// closed, so a room, a hosted server or a shared file stays up while the
// player is in use. Without a tray to come back from, closing quits as usual.
type trayController struct {
	app      *application.App
	window   *application.WebviewWindow
	quitting atomic.Bool
}

func setupTray(app *application.App, window *application.WebviewWindow) *trayController {
	controller := &trayController{app: app, window: window}
	menu := app.NewMenu()
	menu.Add("Show Faro").OnClick(func(*application.Context) { controller.showWindow() })
	menu.AddSeparator()
	menu.Add("Quit Faro").OnClick(func(*application.Context) { controller.quit() })
	tray := app.SystemTray.New()
	tray.SetIcon(trayIcon).SetMenu(menu).OnClick(controller.showWindow)
	tray.SetTooltip("Faro")

	window.RegisterHook(events.Common.WindowClosing, func(event *application.WindowEvent) {
		if controller.quitting.Load() || loadWindowSettings().QuitOnClose || !trayHostAvailable() {
			return
		}
		event.Cancel()
		window.Hide()
	})
	// macOS: clicking the Dock icon brings a hidden window back.
	app.Event.OnApplicationEvent(events.Mac.ApplicationShouldHandleReopen, func(*application.ApplicationEvent) {
		controller.showWindow()
	})
	return controller
}

func (c *trayController) showWindow() {
	c.window.Show()
	if c.window.IsMinimised() {
		c.window.Restore()
	}
	c.window.Focus()
}

func (c *trayController) quit() {
	c.quitting.Store(true)
	c.app.Quit()
}

// singleInstance routes a second launch to the running Faro, which matters
// once Faro can live in the tray: launching it again shows the window instead
// of starting a second copy. FARO_ALLOW_MULTIPLE=1 opts out, for running a
// host and a guest side by side while testing.
func singleInstance(show func()) *application.SingleInstanceOptions {
	if os.Getenv("FARO_ALLOW_MULTIPLE") != "" {
		return nil
	}
	return &application.SingleInstanceOptions{
		UniqueID:               linuxAppID,
		OnSecondInstanceLaunch: func(application.SecondInstanceData) { show() },
	}
}

// TrayStatus reports the close-to-tray preference and whether a tray is
// available right now to return to.
type TrayStatus struct {
	CloseToTray bool `json:"closeToTray"`
	Available   bool `json:"available"`
}

func (d *Desktop) TrayStatus() TrayStatus {
	return TrayStatus{CloseToTray: !loadWindowSettings().QuitOnClose, Available: trayHostAvailable()}
}

func (d *Desktop) SetCloseToTray(enabled bool) error {
	return updateWindowSettings(func(settings *windowSettings) { settings.QuitOnClose = !enabled })
}
