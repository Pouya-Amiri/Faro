package main

import (
	"fmt"
	"os"
	"runtime"
	"time"

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

	background := loadWindowBackground()
	desktop := NewDesktop(wailsApp, background)
	wailsApp.RegisterService(application.NewService(desktop))

	mainWindow := wailsApp.Window.NewWithOptions(application.WebviewWindowOptions{
		Name:      "main",
		Title:     "Faro",
		URL:       "/",
		Width:     1360,
		Height:    860,
		MinWidth:  windowMinWidth,
		MinHeight: windowMinHeight,
		// Faro draws its own title bar. On Linux GTK keeps its client-side
		// decorations (corners, shadow, resize borders) and only its title bar
		// is replaced; see surface_linux.go.
		Frameless:        runtime.GOOS != "darwin" && !nativeClientDecorations,
		Hidden:           revealWhenReady,
		EnableFileDrop:   true,
		BackgroundColour: background,
		Mac: application.MacWindow{
			TitleBar:                application.MacTitleBarHiddenInset,
			InvisibleTitleBarHeight: 38,
		},
		Linux: application.LinuxWindow{
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
	// The page reports window state so its own controls can show the restore
	// icon and square corners while maximised or fullscreen.
	emitWindowState := func(*application.WindowEvent) {
		wailsApp.Event.Emit("faro:window-state", map[string]bool{
			"maximised":  mainWindow.IsMaximised(),
			"fullscreen": mainWindow.IsFullscreen(),
		})
	}
	for _, eventType := range []events.WindowEventType{
		events.Common.WindowMaximise,
		events.Common.WindowUnMaximise,
		events.Common.WindowFullscreen,
		events.Common.WindowUnFullscreen,
		events.Common.WindowRestore,
	} {
		mainWindow.OnWindowEvent(eventType, emitWindowState)
	}
	if revealWhenReady {
		// The page calls Desktop.WindowReady after its first render. These
		// fallbacks guarantee the window still appears if it never does.
		mainWindow.OnWindowEvent(events.Common.WindowRuntimeReady, func(*application.WindowEvent) {
			time.AfterFunc(2*time.Second, desktop.revealWindow)
		})
		wailsApp.Event.OnApplicationEvent(events.Common.ApplicationStarted, func(*application.ApplicationEvent) {
			time.AfterFunc(6*time.Second, desktop.revealWindow)
		})
	}

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
