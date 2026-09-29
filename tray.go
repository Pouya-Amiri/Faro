package main

import (
	_ "embed"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Pouya-Amiri/Faro/internal/updater"
	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
)

//go:embed build/tray-icon.png
var trayIcon []byte

// quitConfirmWindow is how long a repeated quit goes ahead without asking
// again, so Faro can always be quit even if the page cannot answer.
const quitConfirmWindow = 10 * time.Second

// trayController keeps Faro running in the system tray after its window is
// closed, so a room, a hosted server or a shared file stays up while the
// player is in use. Without a tray to come back from, closing quits as usual.
type trayController struct {
	app      *application.App
	window   *application.WebviewWindow
	desktop  *Desktop
	quitting atomic.Bool

	confirmMu    sync.Mutex
	confirmAsked time.Time

	updateMu    sync.Mutex
	updateItem  *application.MenuItem
	updateLabel string
}

func setupTray(app *application.App, window *application.WebviewWindow, desktop *Desktop) *trayController {
	controller := &trayController{app: app, window: window, desktop: desktop}
	menu := app.NewMenu()
	menu.Add("Show Faro").OnClick(func(*application.Context) { controller.showWindow() })
	// Shown while an update is available; opens the update dialog.
	controller.updateItem = menu.Add("Update Faro…").SetHidden(true).OnClick(func(*application.Context) {
		controller.showWindow()
		app.Event.Emit("faro:open-update", nil)
	})
	menu.AddSeparator()
	menu.Add("Quit Faro").OnClick(func(*application.Context) { controller.requestQuit() })
	tray := app.SystemTray.New()
	tray.SetIcon(trayIcon).SetMenu(menu).OnClick(func() {
		// Only a primary click shows the window; opening the menu does not.
		if !trayMenuOpening() {
			controller.showWindow()
		}
	})
	tray.SetTooltip("Faro")

	window.RegisterHook(events.Common.WindowClosing, func(event *application.WindowEvent) {
		if controller.quitting.Load() {
			return
		}
		if !loadWindowSettings().QuitOnClose && trayHostAvailable() {
			event.Cancel()
			window.Hide()
			return
		}
		// Closing the window quits Faro here, which ends a hosted room for
		// everyone, so the page asks first.
		if controller.needsConfirmation() {
			event.Cancel()
		}
	})
	// macOS: clicking the Dock icon brings a hidden window back.
	app.Event.OnApplicationEvent(events.Mac.ApplicationShouldHandleReopen, func(*application.ApplicationEvent) {
		controller.showWindow()
	})
	return controller
}

// showUpdate reflects an available update in the tray menu. The menu is only
// rebuilt when its text changes, not for every progress report.
func (c *trayController) showUpdate(status updater.Status) {
	label := ""
	if status.Available {
		switch status.Phase {
		case updater.PhaseDownloading:
			label = "Downloading update…"
		case updater.PhaseInstalling, updater.PhaseRestarting:
			label = "Installing update…"
		default:
			label = "Update to Faro " + status.LatestVersion + "…"
		}
	}
	c.updateMu.Lock()
	defer c.updateMu.Unlock()
	if label == c.updateLabel {
		return
	}
	c.updateLabel = label
	if label != "" {
		c.updateItem.SetLabel(label)
	}
	c.updateItem.SetHidden(label == "")
}

func (c *trayController) showWindow() {
	c.window.Show()
	if c.window.IsMinimised() {
		c.window.Restore()
	}
	c.window.Focus()
}

// requestQuit quits at once when nothing is lost by it, and otherwise shows
// the window and lets the page confirm.
func (c *trayController) requestQuit() {
	if !c.needsConfirmation() {
		c.quit()
	}
}

// needsConfirmation reports whether quitting now would leave a room, in which
// case the page finishes the quit through Desktop.Quit: it asks first, in the
// shown window, unless the user turned that question off. Either way the page
// gets to pause playback for the room on the way out. A second request within
// quitConfirmWindow does not wait for the page again.
func (c *trayController) needsConfirmation() bool {
	service := c.desktop.service
	if service == nil || !service.InRoom() && !service.ServerStatus().Running {
		return false
	}
	c.confirmMu.Lock()
	repeated := time.Since(c.confirmAsked) < quitConfirmWindow
	c.confirmAsked = time.Now()
	c.confirmMu.Unlock()
	if repeated {
		return false
	}
	ask := !loadWindowSettings().SkipQuitConfirm
	if ask {
		c.showWindow()
	}
	c.app.Event.Emit("faro:confirm-quit", map[string]bool{"hosting": service.ServerStatus().Running, "ask": ask})
	return true
}

func (c *trayController) quit() {
	c.quitting.Store(true)
	c.app.Quit()
}

// Quit ends Faro after the page has confirmed it.
func (d *Desktop) Quit() {
	if d.tray != nil {
		d.tray.quit()
		return
	}
	d.app.Quit()
}

// singleInstance routes a second launch to the running Faro, which matters
// once Faro can live in the tray: launching it again shows the window (and
// opens any files it was given) instead of starting a second copy.
// FARO_ALLOW_MULTIPLE=1 opts out, for running a host and a guest side by side
// while testing.
func singleInstance(secondLaunch func(application.SecondInstanceData)) *application.SingleInstanceOptions {
	if os.Getenv("FARO_ALLOW_MULTIPLE") != "" {
		return nil
	}
	return &application.SingleInstanceOptions{
		UniqueID:               linuxAppID,
		OnSecondInstanceLaunch: secondLaunch,
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

// ConfirmQuit reports whether quitting from a room asks first.
func (d *Desktop) ConfirmQuit() bool { return !loadWindowSettings().SkipQuitConfirm }

func (d *Desktop) SetConfirmQuit(enabled bool) error {
	return updateWindowSettings(func(settings *windowSettings) { settings.SkipQuitConfirm = !enabled })
}
