package host

import (
	"context"
	"fmt"
	"log"
)

// LqChannels handles every non-main, non-sync invoke channel:
// "call-application", "call-main-win", "toggle-max-or-min-active-win",
// "export-publish-assets", "set-quit-dirty-state", "check-for-updates",
// "install-updates", "get-downloaded-update" plus the preload-only
// "open-external"/"open-path" routes.
//
// Electron wires each name to a dedicated ipcMain.handle; the preload shim
// routes unknown channel names here (core.cljs aux handlers).
type LqChannels struct{ H *Host }

// Handle dispatches one aux channel call. args carries the call arguments.
func (l *LqChannels) Handle(ctx context.Context, channel string, args []any) (any, error) {
	h := l.H
	switch channel {
	case "call-application", "call-main-win":
		// js-invoke app/win type args — plugin/app integration points that
		// milestone 1 accepts and drops.
		if len(args) > 0 {
			log.Printf("[host] aux channel %s type=%v (ignored)", channel, args[0])
		}
		return nil, nil
	case "toggle-max-or-min-active-win":
		h.toggleMaxOrMinActiveWindow(boolOfAny(argOf(args, 0)))
		return nil, nil
	case "export-publish-assets":
		// Publishing asset export — accepted and dropped in milestone 1
		// (deviation: Electron copies exported asset files).
		log.Printf("[host] export-publish-assets (stub)")
		return nil, nil
	case "set-quit-dirty-state":
		h.setQuitDirty(len(args) > 0 && boolOfAny(args[0]))
		return nil, nil
	case "check-for-updates":
		// Updater stub: no auto-update in milestone 1.
		evUpdates.Broadcast(map[string]any{"type": "update-not-available", "payload": nil})
		return nil, nil
	case "install-updates":
		return nil, nil
	case "get-downloaded-update":
		return nil, nil
	case "open-external":
		return nil, h.openExternalCmd(stringArg(args, 0))
	case "open-path":
		return nil, h.openPathCmd(stringArg(args, 0))
	default:
		return nil, fmt.Errorf("no handler for channel %s", channel)
	}
}

// toggleMaxOrMinActiveWindow mirrors core.cljs toggle-max-or-min-active-win:
// minimize/restore when isToggleMin, else max/unmax, on the focused window.
func (h *Host) toggleMaxOrMinActiveWindow(isToggleMin bool) {
	for _, hd := range h.allHandles() {
		if hd.win == nil || hd.win.IsDestroyed() || !hd.win.IsFocused() {
			continue
		}
		if isToggleMin {
			if hd.win.IsMinimized() {
				hd.win.Restore()
			} else {
				hd.win.Minimize()
			}
		} else if hd.win.IsMaximized() {
			hd.win.Unmaximize()
		} else {
			hd.win.Maximize()
		}
		return
	}
}

// boolOfAny interprets a loosely typed boolean argument.
func boolOfAny(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		return t == "true"
	default:
		return false
	}
}
