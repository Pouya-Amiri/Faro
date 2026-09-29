package main

import (
	"context"
	"errors"

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
// Faro. Progress arrives as faro:update events.
func (d *Desktop) InstallUpdate() error {
	if d.updates == nil {
		return errUpdatesUnavailable
	}
	return d.updates.Install()
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
