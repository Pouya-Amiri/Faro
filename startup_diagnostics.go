package main

import (
	"sync/atomic"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// launchedAt is when this process started, for startup timings.
var launchedAt = time.Now()

const (
	// responsivenessWatch is how long after launch the window is watched for
	// stalls; a slow start is what users notice.
	responsivenessWatch = time.Minute
	// stallThreshold is the shortest stall worth logging: below it, moving
	// the window or clicking still feels immediate.
	stallThreshold = 300 * time.Millisecond
	// maxStallReports keeps a pathological start from filling the log.
	maxStallReports = 20
)

var stallReports atomic.Int32

func sinceLaunch() time.Duration { return time.Since(launchedAt).Round(time.Millisecond) }

// logStall records that a part of the window could not respond for a while.
func (d *Desktop) logStall(part string, blocked time.Duration) {
	if blocked < stallThreshold || stallReports.Add(1) > maxStallReports {
		return
	}
	d.diagnostics.Printf("ui: the %s did not respond for %s, %s after launch", part, blocked.Round(time.Millisecond), sinceLaunch())
}

// watchMainThread measures how long the main thread, which moves and
// redraws the window, takes to run an empty task during the first minute.
// A long wait means something blocked it; the log says when and for how
// long, so a slow start can be told apart from a slow page.
func (d *Desktop) watchMainThread() {
	go func() {
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for range ticker.C {
			if time.Since(launchedAt) > responsivenessWatch {
				return
			}
			started := time.Now()
			application.InvokeSync(func() {})
			d.logStall("main thread", time.Since(started))
		}
	}()
}

// ReportStall is called by the page when its own thread was blocked during
// the first minute, which stops it from handling clicks, including the ones
// that move the window by its title bar.
func (d *Desktop) ReportStall(milliseconds int) {
	if milliseconds > 0 && milliseconds < 10*60*1000 {
		d.logStall("page", time.Duration(milliseconds)*time.Millisecond)
	}
}
