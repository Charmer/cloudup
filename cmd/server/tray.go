package main

import (
	_ "embed"
	"log"
	"net/http"

	"fyne.io/systray"
)

//go:embed assets/icon.ico
var trayIconBytes []byte

// runWithTray runs cloudup as a background/tray app instead of a normal
// foreground console process - the experience the user asked for: no
// visible console window, a tray icon appears, the browser opens on its
// own. Left-clicking the icon reopens the browser if it was closed;
// right-clicking shows a menu whose only item is Exit, which shuts the
// whole app down (HTTP server + upload queue) rather than just closing a
// window - there is no window to close, the browser tab is just a client
// of the API like any other.
//
// systray.Run blocks the calling goroutine until systray.Quit() fires (see
// the "Exit" handler below), and on some platforms must run on the main
// OS thread - so unlike the non-tray path in run(), the HTTP server here
// is started from inside onReady, not before Run is called.
func runWithTray(url string, httpServer *http.Server, shutdown func(), openOnStart bool) {
	onReady := func() {
		systray.SetIcon(trayIconBytes)
		systray.SetTitle("Cloud Uploader")
		systray.SetTooltip("Cloud Uploader — " + url)

		openItem := systray.AddMenuItem("Open", "Open the web interface")
		systray.AddSeparator()
		exitItem := systray.AddMenuItem("Exit", "Quit Cloud Uploader")

		open := func() { go openInBrowser(url) }
		systray.SetOnTapped(open) // left click

		go func() {
			for {
				select {
				case <-openItem.ClickedCh:
					open()
				case <-exitItem.ClickedCh:
					systray.Quit()
					return
				}
			}
		}()

		go func() {
			if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Printf("http server error: %v", err)
			}
		}()

		if openOnStart {
			open()
		}
	}

	onExit := shutdown

	systray.Run(onReady, onExit)
}

// requestTrayQuit lets main's Ctrl+C signal handler converge on the exact
// same shutdown path as the tray's Exit menu item, instead of duplicating
// shutdown logic - systray.Quit() triggers runWithTray's onExit callback
// exactly as if the user had clicked Exit.
func requestTrayQuit() {
	systray.Quit()
}
