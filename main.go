package main

import (
	"fmt"
	"os"
	"runtime"

	"github.com/Pouya-Amiri/Faro/frontend"
	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
)

const (
	windowMinWidth  = 760
	windowMinHeight = 560
	// Flathub maps underscores in a code-hosting ID back to hyphens, so this
	// identifies github.com/Pouya-Amiri/Faro while remaining a valid app ID.
	linuxAppID = "io.github.pouya_amiri.Faro"
)

func main() {
	preferWayland()

	wailsApp := application.New(application.Options{
		Name:        "Faro",
		Description: "Secure synchronized media playback",
		Assets: application.AssetOptions{
			Handler: application.BundledAssetFileServer(frontend.Assets),
		},
		Mac: application.MacOptions{
			ApplicationShouldTerminateAfterLastWindowClosed: true,
		},
		Linux: application.LinuxOptions{
			ApplicationID: linuxAppID,
		},
	})

	desktop := NewDesktop(wailsApp)
	wailsApp.RegisterService(application.NewService(desktop))

	mainWindow := wailsApp.Window.NewWithOptions(application.WebviewWindowOptions{
		Name:             "main",
		Title:            "Faro",
		URL:              "/",
		Width:            1360,
		Height:           860,
		MinWidth:         windowMinWidth,
		MinHeight:        windowMinHeight,
		Frameless:        runtime.GOOS != "darwin",
		EnableFileDrop:   true,
		BackgroundColour: application.NewRGB(24, 24, 26),
		Mac: application.MacWindow{
			TitleBar:                application.MacTitleBarHiddenInset,
			InvisibleTitleBarHeight: 38,
		},
		Linux: application.LinuxWindow{
			// The frameless surface itself is shaped once the GTK toplevel
			// exists; see surface_linux.go.
			WebviewGpuPolicy: application.WebviewGpuPolicyNever,
		},
	})
	desktop.setWindow(mainWindow)

	mainWindow.OnWindowEvent(events.Common.WindowFilesDropped, func(event *application.WindowEvent) {
		wailsApp.Event.Emit("faro:file-drop", map[string]any{
			"paths":  event.Context().DroppedFiles(),
			"target": event.Context().DropTargetDetails(),
		})
	})
	// Faro is frameless, so Faro owns the window shape: rounded while floating,
	// square while maximised, fullscreen or tiled. This is a no-op away from
	// Linux, where the toolkit or window manager already draws the corners.
	syncSurface := func(*application.WindowEvent) { syncWindowSurface(mainWindow) }
	for _, eventType := range []events.WindowEventType{
		events.Common.WindowRuntimeReady,
		events.Common.WindowMaximise,
		events.Common.WindowUnMaximise,
		events.Common.WindowFullscreen,
		events.Common.WindowUnFullscreen,
	} {
		mainWindow.OnWindowEvent(eventType, syncSurface)
	}
	watchWindowSurface(mainWindow)

	if err := wailsApp.Run(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "faro:", err)
		os.Exit(1)
	}
}

// preferWayland makes the modern backend the first choice while retaining an
// X11 fallback for users launching Faro outside a Wayland session. An explicit
// user setting always wins.
func preferWayland() {
	if runtime.GOOS == "linux" && os.Getenv("GDK_BACKEND") == "" {
		_ = os.Setenv("GDK_BACKEND", "wayland,x11")
	}
}
