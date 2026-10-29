package host

import (
	"os"
	"regexp"

	"github.com/egoist/mygo"
)

// mygoWindow aliases the mygo window type used throughout the host.
type mygoWindow = mygo.Window

// mygoApp returns the mygo application singleton.
func mygoApp() *mygo.Application { return mygo.App }

// Event names pushed to the renderer. Payloads are string-keyed maps
// (or plain values) sent through window.mygo events; the renderer's
// window.apis listeners receive the payload as the first callback arg.
var (
	evGraphLifecycle = mygo.NewEvent[any]("graph-lifecycle")
	evNotification   = mygo.NewEvent[any]("notification")
	evRedirect       = mygo.NewEvent[any]("redirect")
	evRedirectExists = mygo.NewEvent[any]("redirectWhenExists")
	evFoundInPage    = mygo.NewEvent[any]("foundInPage")
	evQuickCapture   = mygo.NewEvent[any]("quickCapture")
	evInvokeCommand  = mygo.NewEvent[any]("invokeCommand")
	evNewWinOfGraph  = mygo.NewEvent[any]("openNewWindowOfGraph")
	evHandbook       = mygo.NewEvent[any]("handbook")
	evLspUpdates     = mygo.NewEvent[any]("lsp-updates")
	evSetCurGraph    = mygo.NewEvent[any]("setCurrentGraph")
	evUpdates        = mygo.NewEvent[any]("updates-callback")
	evPersistZoom    = mygo.NewEvent[any]("persist-zoom-level")
	evRestoreZoom    = mygo.NewEvent[any]("restore-zoom-level")
	evFullScreen     = mygo.NewEvent[any]("full-screen")
	evMaximize       = mygo.NewEvent[any]("maximize")
	evSyncServerSt   = mygo.NewEvent[any]("syncAPIServerState")
)

// emitToAll broadcasts an event payload to every live window.
func (h *Host) emitToAll(ev *mygo.Event[any], payload any) {
	for _, hd := range h.allHandles() {
		if hd.win != nil && !hd.win.IsDestroyed() {
			ev.Emit(hd.win, payload)
		}
	}
}

// emitToFocused sends to the focused window when there is one.
func (h *Host) emitToFocused(ev *mygo.Event[any], payload any) {
	for _, hd := range h.allHandles() {
		if hd.win != nil && !hd.win.IsDestroyed() && hd.win.IsFocused() {
			ev.Emit(hd.win, payload)
			return
		}
	}
}

// notifyGraphLifecycle pushes {repo, phase, generation} to all windows.
func (h *Host) notifyGraphLifecycle(repo, phase, generation string) {
	h.emitToAll(evGraphLifecycle, map[string]any{
		"repo":       repo,
		"phase":      phase,
		"generation": generation,
	})
}

// notifyError pushes an error notification to the given window.
func (h *Host) notifyError(w *mygoWindow, payload, i18nKey string, i18nArgs []any) {
	p := map[string]any{"type": "error", "payload": payload}
	if i18nKey != "" {
		p["i18n-key"] = i18nKey
		p["i18n-args"] = i18nArgs
	}
	if w != nil {
		evNotification.Emit(w, p)
	} else {
		h.emitToAll(evNotification, p)
	}
}

// emitTo sends one event to a single window.
func (h *Host) emitTo(w *mygoWindow, ev *mygo.Event[any], payload any) {
	if w != nil && !w.IsDestroyed() {
		ev.Emit(w, payload)
	}
}

// extractDaemonRevision scrapes the baked logseq.common.version/REVISION
// (git describe output) out of the built db-worker-node.js so the host can
// stop daemons from a different build. Returns "" when the build does not
// carry a revision (dev builds) — the mismatch check is then skipped.
func extractDaemonRevision(script string) string {
	data, err := os.ReadFile(script)
	if err != nil {
		return ""
	}
	text := string(data)
	for _, re := range []*regexp.Regexp{
		regexp.MustCompile(`REVISION\s*=\s*"([0-9a-zA-Z][0-9a-zA-Z.\-]*)"`),
		regexp.MustCompile(`revision[\"']?\s*[:=]\s*[\"']([0-9a-f]{7,40}(?:-[0-9]+)?(?:-dirty)?)[\"']`),
	} {
		if m := re.FindStringSubmatch(text); len(m) > 1 && m[1] != "" && m[1] != "dev" {
			return m[1]
		}
	}
	return ""
}
