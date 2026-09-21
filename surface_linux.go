//go:build linux && cgo && !gtk3 && !server

package main

/*
#cgo pkg-config: gtk4
#include <gtk/gtk.h>

// Faro draws its own title bar, so GTK decorates nothing and Wails pins
// `border-radius: 0` on the toplevel. The toplevel then still painted the
// theme's opaque background, which is what showed as a square black corner
// behind the rounded page. This is the standard GTK4 client-side-decoration
// recipe: a transparent toplevel clipped to a rounded rectangle. Descendants
// are clipped as well, so the WebKitWebView underneath can never paint over
// the transparent corners.
static void faroInstallSurfaceStyle(int radius) {
	static gboolean installed = FALSE;
	if (installed) {
		return;
	}
	installed = TRUE;

	gchar *css = g_strdup_printf(
		"window.faro-surface { background: transparent; }"
		"window.faro-surface.faro-surface-rounded { border-radius: %dpx; }",
		radius);
	GtkCssProvider *provider = gtk_css_provider_new();
	gtk_css_provider_load_from_string(provider, css);
	g_free(css);
	// USER priority beats the `.wails-frameless { border-radius: 0 }` rule Wails
	// installs on the same widget at APPLICATION priority.
	gtk_style_context_add_provider_for_display(gdk_display_get_default(),
		GTK_STYLE_PROVIDER(provider), GTK_STYLE_PROVIDER_PRIORITY_USER);
	// The provider is deliberately never released: it must outlive the display.
}

// gdk_toplevel_get_state also reports the tiled states a compositor uses for
// edge snapping. GTK exposes them nowhere else and Wails does not forward them.
// wantFlush carries the maximised/fullscreen state Wails already tracks, so a
// compositor that omits a flag cannot leave corners rounded at a screen edge.
static gboolean faroSurfaceFlush(GtkWindow *window, gboolean wantFlush) {
	if (wantFlush) {
		return TRUE;
	}
	GdkSurface *surface = gtk_native_get_surface(GTK_NATIVE(window));
	if (surface == NULL) {
		return FALSE;
	}
	const GdkToplevelState flush = GDK_TOPLEVEL_STATE_MAXIMIZED | GDK_TOPLEVEL_STATE_FULLSCREEN |
		GDK_TOPLEVEL_STATE_TILED | GDK_TOPLEVEL_STATE_TOP_TILED | GDK_TOPLEVEL_STATE_RIGHT_TILED |
		GDK_TOPLEVEL_STATE_BOTTOM_TILED | GDK_TOPLEVEL_STATE_LEFT_TILED;
	return (gdk_toplevel_get_state(GDK_TOPLEVEL(surface)) & flush) != 0;
}

static void faroShapeSurface(GtkWindow *window, gboolean flush) {
	GtkWidget *widget = GTK_WIDGET(window);
	gtk_widget_add_css_class(widget, "faro-surface");
	gtk_widget_set_overflow(widget, GTK_OVERFLOW_HIDDEN);
	if (flush) {
		gtk_widget_remove_css_class(widget, "faro-surface-rounded");
	} else {
		gtk_widget_add_css_class(widget, "faro-surface-rounded");
	}
}
*/
import "C"

import (
	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
)

// surfaceCornerRadius is the radius of the rounded Linux window. GTK4 owns the
// window shape, so the radius is declared only here.
const surfaceCornerRadius = 15

// watchWindowSurface adds the Linux-only state changes Wails does not report as
// common window events: the first load, by which time the toplevel exists, and
// resizes, which is how edge snapping and tiling appear.
func watchWindowSurface(window application.Window) {
	notify := func(*application.WindowEvent) { syncWindowSurface(window) }
	window.OnWindowEvent(events.Linux.WindowLoadStarted, notify)
	window.OnWindowEvent(events.Linux.WindowDidResize, notify)
}

// syncWindowSurface gives the frameless GTK toplevel a transparent, rounded
// shape, or a square one while the window is flush with the screen edges.
// Window events are delivered on their own goroutine, so the GTK calls are
// dispatched to the main thread.
func syncWindowSurface(window application.Window) {
	native := window.NativeWindow()
	if native == nil {
		return
	}
	// IsMaximised and IsFullscreen dispatch to the main thread themselves, so
	// they must be read outside the InvokeSync below.
	wantFlush := C.gboolean(0)
	if window.IsMaximised() || window.IsFullscreen() {
		wantFlush = 1
	}
	application.InvokeSync(func() {
		gtkWindow := (*C.GtkWindow)(native)
		C.faroInstallSurfaceStyle(C.int(surfaceCornerRadius))
		C.faroShapeSurface(gtkWindow, C.faroSurfaceFlush(gtkWindow, wantFlush))
	})
}
