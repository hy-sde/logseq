package host

// App menu: port of src/electron/electron/core.cljs set-app-menu!.
// Labels are plain English (the Go host has no main-process i18n;
// Electron resolves :electron/new-window etc. from the main-process dicts).

import (
	"fmt"
	"runtime"

	"github.com/egoist/mygo"
)

func (h *Host) installAppMenu() {
	isMac := runtime.GOOS == "darwin"

	var template []*mygo.MenuItem
	if isMac {
		// App menu: about / services / hide / hideOthers / unhide / quit.
		template = append(template, &mygo.MenuItem{Role: mygo.RoleAppMenu})
	}
	// fileMenu: New Window + (mac: close without Cmd+W | other: quit).
	newWindowAccel := "Shift+CmdOrCtrl+N" // avoid Ctrl+N text-editor conflict
	if isMac {
		newWindowAccel = "CmdOrCtrl+N"
	}
	var fileItems []*mygo.MenuItem
	fileItems = append(fileItems, &mygo.MenuItem{
		Label:       "New Window",
		Accelerator: newWindowAccel,
		Click:       func(*mygo.MenuItem, *mygo.Window) { h.openNewWindowCmd("") },
	})
	if isMac {
		// Electron disables the Command+W shortcut on the close role.
		fileItems = append(fileItems, &mygo.MenuItem{
			Label: "Close Window",
			Click: func(_ *mygo.MenuItem, w *mygo.Window) {
				if w != nil {
					w.Close()
				}
			},
		})
	} else {
		fileItems = append(fileItems, &mygo.MenuItem{Role: mygo.RoleQuit})
	}
	template = append(template, &mygo.MenuItem{
		Role:    mygo.RoleFileMenu,
		Submenu: fileItems,
	})

	template = append(template,
		&mygo.MenuItem{Role: mygo.RoleEditMenu},
		&mygo.MenuItem{Role: mygo.RoleViewMenu},
	)

	// windowMenu: (non-mac: minimize/zoom/close-no-accel) + Always on Top.
	var windowItems []*mygo.MenuItem
	if !isMac {
		windowItems = append(windowItems,
			&mygo.MenuItem{Role: mygo.RoleMinimize},
			&mygo.MenuItem{Role: mygo.RoleZoom},
			&mygo.MenuItem{Role: mygo.RoleClose}, // Control+W disabled upstream; mygo keeps the default
		)
	}
	windowItems = append(windowItems, &mygo.MenuItem{
		Label: "Always on Top",
		Type:  mygo.MenuItemCheckbox,
		Click: func(it *mygo.MenuItem, w *mygo.Window) {
			if w != nil {
				w.SetAlwaysOnTop(it.Checked)
			}
		},
	})
	template = append(template, &mygo.MenuItem{
		Role:    mygo.RoleWindowMenu,
		Submenu: windowItems,
	})

	// help: official docs (+ non-mac about). On non-darwin mygo hides the
	// RoleHelp/RoleAbout items entirely (its macOnlyRoles set), so plain
	// labeled items are used there instead of the roles.
	helpItems := []*mygo.MenuItem{{
		Label: "Official Docs",
		Click: func(*mygo.MenuItem, *mygo.Window) {
			_ = mygoShell().OpenExternal("https://docs.logseq.com/")
		},
	}}
	if isMac {
		helpItems = append(helpItems, &mygo.MenuItem{Role: mygo.RoleAbout})
	} else {
		helpItems = append(helpItems, &mygo.MenuItem{
			Label: "About",
			Click: func(*mygo.MenuItem, *mygo.Window) { h.showAboutDialog() },
		})
	}
	helpMenu := &mygo.MenuItem{Submenu: helpItems}
	if isMac {
		helpMenu.Role = mygo.RoleHelp
	} else {
		helpMenu.Label = "Help"
	}
	template = append(template, helpMenu)

	// Enable Cmd/Ctrl+= zoom in.
	template = append(template, &mygo.MenuItem{Role: mygo.RoleZoomIn, Accelerator: "CmdOrCtrl+="})

	mygoApp().SetMenu(mygo.NewMenu(template))
}

// showAboutDialog ports the non-mac about-fn message box (no icon asset in
// the Go host).
func (h *Host) showAboutDialog() {
	_, _ = mygo.Dialog.Message(mygo.MessageOptions{
		Title:   "Logseq",
		Message: fmt.Sprintf("Logseq %s", appVersion()),
	})
}
