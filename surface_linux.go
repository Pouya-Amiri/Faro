//go:build linux && cgo && !gtk3 && !server

package main

/*
#cgo pkg-config: gtk4
#include <stdlib.h>
#include <gtk/gtk.h>

// Faro draws its own title bar in the page, but it keeps GTK's client-side
// decorations: GTK4 then owns the rounded corners, the shadow, the resize
// borders outside the visible frame, the opaque region, and the square shape
// while maximised, fullscreen or tiled. The window becomes "frameless" by
// replacing GTK's title bar with an empty one. The window's own CSS state
// classes (.maximized, .tiled-*, .solid-csd, ...) switch in the same frame as
// the state change, so the corners never lag behind the window state.
//
// The frame style itself is generated in Go (windowFrameCSS) from the page's
// theme colour and replaced whenever the theme changes.
static void faroSetFrameStyle(const char *css) {
	static GtkCssProvider *provider = NULL;
	if (provider == NULL) {
		provider = gtk_css_provider_new();
		// USER priority beats the GTK theme's window.csd rules. The provider is
		// deliberately never released: it must outlive the display.
		gtk_style_context_add_provider_for_display(gdk_display_get_default(),
			GTK_STYLE_PROVIDER(provider), GTK_STYLE_PROVIDER_PRIORITY_USER);
	}
	gtk_css_provider_load_from_string(provider, css);
}

extern void faroWindowChromeChanged(void);

static void faroChromeSettingChanged(GObject *settings, GParamSpec *spec, gpointer data) {
	faroWindowChromeChanged();
}

// faroWatchWindowChrome follows changes to the desktop's title bar button
// layout and double-click action, which GNOME and KDE apply without a restart.
static void faroWatchWindowChrome(void) {
	static gboolean watching = FALSE;
	GtkSettings *settings = gtk_settings_get_default();
	if (watching || settings == NULL) {
		return;
	}
	watching = TRUE;
	g_signal_connect(settings, "notify::gtk-decoration-layout", G_CALLBACK(faroChromeSettingChanged), NULL);
	g_signal_connect(settings, "notify::gtk-titlebar-double-click", G_CALLBACK(faroChromeSettingChanged), NULL);
}

// faroPrepareWindow must run before the window is first mapped: GTK does not
// promise that a title bar can be replaced on a visible window.
static void faroPrepareWindow(GtkWindow *window) {
	if (g_object_get_data(G_OBJECT(window), "faro-prepared") != NULL) {
		return;
	}
	g_object_set_data(G_OBJECT(window), "faro-prepared", GINT_TO_POINTER(1));
	faroWatchWindowChrome();

	GtkWidget *titlebar = gtk_box_new(GTK_ORIENTATION_HORIZONTAL, 0);
	gtk_widget_set_visible(titlebar, FALSE);
	gtk_window_set_titlebar(window, titlebar);
	gtk_window_set_decorated(window, TRUE);
	gtk_widget_add_css_class(GTK_WIDGET(window), "faro");
	// Clip the webview to the rounded frame so its square corners never paint
	// over the transparent corner area.
	gtk_widget_set_overflow(GTK_WIDGET(window), GTK_OVERFLOW_HIDDEN);
}

static gchar *faroStringSetting(const char *name) {
	gchar *value = NULL;
	GtkSettings *settings = gtk_settings_get_default();
	if (settings != NULL) {
		g_object_get(settings, name, &value, NULL);
	}
	return value;
}
*/
import "C"

import (
	"unsafe"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// nativeClientDecorations keeps GTK's decorations and hides only its title bar.
const nativeClientDecorations = true

// revealWhenReady creates the window hidden and shows it once the page has
// rendered, so no half-styled frame is ever visible.
const revealWhenReady = true

// prepareWindowSurface installs the empty title bar and the frame style for
// the given page background. It is idempotent and reports false when the
// native window does not exist yet.
func prepareWindowSurface(window application.Window, background application.RGBA) bool {
	native := window.NativeWindow()
	if native == nil {
		return false
	}
	setWindowFrameStyle(background)
	application.InvokeSync(func() {
		C.faroPrepareWindow((*C.GtkWindow)(native))
	})
	return true
}

// setWindowFrameStyle restyles the GTK frame to match the page's theme.
func setWindowFrameStyle(background application.RGBA) {
	css := C.CString(windowFrameCSS(background))
	defer C.free(unsafe.Pointer(css))
	application.InvokeSync(func() { C.faroSetFrameStyle(css) })
}

// platformWindowChrome reports the desktop's title bar button layout and
// double-click action so the page can mirror them.
func platformWindowChrome() (layout, doubleClick string) {
	application.InvokeSync(func() {
		layout = takeSetting("gtk-decoration-layout")
		doubleClick = takeSetting("gtk-titlebar-double-click")
	})
	return layout, doubleClick
}

func takeSetting(name string) string {
	key := C.CString(name)
	defer C.free(unsafe.Pointer(key))
	value := C.faroStringSetting(key)
	if value == nil {
		return ""
	}
	defer C.g_free(C.gpointer(value))
	return C.GoString(value)
}
