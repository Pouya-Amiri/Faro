package main

import (
	"context"
	"errors"
	"time"

	"github.com/Pouya-Amiri/Faro/internal/buildinfo"
	"github.com/Pouya-Amiri/Faro/internal/updater"
)

var errUpdatesUnavailable = errors.New("updates are not available for this build of Faro")

// startUpdates creates the updater. Background checks start once the page
// reports its "Check for updates" preference through SetUpdateChecks.
func (d *Desktop) startUpdates() {
	manager, err := updater.New(updater.Options{
		CurrentVersion: buildinfo.EffectiveVersion(),
		Notify: func(status updater.Status) {
			d.app.Event.Emit("faro:update", status)
			if d.tray != nil {
				d.tray.showUpdate(status)
			}
		},
		BeforeRestart: d.prepareUpdateRestart,
		// The page has already warned about leaving a room before it asked
		// for the install, so quitting here does not ask again.
		Restart: d.Quit,
	})
	if err == nil {
		d.updates = manager
	}
}

// UpdateStatus reports what the updater knows and is doing.
func (d *Desktop) UpdateStatus() updater.Status {
	if d.updates == nil {
		return updater.Status{CurrentVersion: buildinfo.EffectiveVersion(), Phase: updater.PhaseIdle, ReleaseURL: updater.ReleasesURL, Method: updater.MethodManual}
	}
	return d.updates.Status()
}

// CheckForUpdates asks GitHub for the latest release now.
func (d *Desktop) CheckForUpdates() (updater.Status, error) {
	if d.updates == nil {
		return d.UpdateStatus(), errUpdatesUnavailable
	}
	return d.updates.Check(context.Background())
}

// SetUpdateChecks turns the periodic background check on or off.
func (d *Desktop) SetUpdateChecks(enabled bool) {
	if d.updates != nil {
		d.updates.SetAutomaticChecks(enabled)
	}
}

// InstallUpdate downloads and installs the available update, then restarts
// Faro. Progress arrives as faro:update events. pauseRoom carries the page's
// pause-on-leave preference, which the restart honours like a normal quit.
func (d *Desktop) InstallUpdate(pauseRoom bool) error {
	if d.updates == nil {
		return errUpdatesUnavailable
	}
	d.pauseBeforeUpdate.Store(pauseRoom)
	return d.updates.Install()
}

// prepareUpdateRestart pauses the room, when asked to, before Faro restarts.
// It runs in the updater and is waited for, rather than depending on the page
// seeing a particular progress event. Pausing is refused without playback
// control, and a slow room must not hold the restart up for long.
func (d *Desktop) prepareUpdateRestart() {
	if !d.pauseBeforeUpdate.Load() || d.service == nil || !d.service.InRoom() {
		return
	}
	done := make(chan struct{})
	go func() {
		_ = d.service.SetPaused(true)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
	}
}

// CancelUpdate stops an update download.
func (d *Desktop) CancelUpdate() {
	if d.updates != nil {
		d.updates.Cancel()
	}
}

// OpenUpdateDownload shows the verified download so it can be installed by
// hand when installing it automatically failed.
func (d *Desktop) OpenUpdateDownload() error {
	if d.updates == nil {
		return errUpdatesUnavailable
	}
	return d.updates.OpenDownload()
}
