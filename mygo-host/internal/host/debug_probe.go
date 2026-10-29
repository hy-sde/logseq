package host

import (
	"encoding/json"
	"log"
	"os"
	"time"

	"github.com/egoist/mygo"
)

// Debug probes (temporary, milestone-1 smoke diagnostics). Enabled with
// LOGSEQ_DEBUG=1: logs page load failures and, a few seconds after
// creation, evaluates a probe expression in the page and logs the result.
func (h *Host) attachDebugProbe(w *mygoWindow) {
	if os.Getenv("LOGSEQ_DEBUG") != "1" {
		return
	}
	w.Page().OnDidFailLoad(func(err *mygo.LoadError) {
		log.Printf("[debug] did-fail-load url=%s code=%d desc=%s", err.URL, err.Code, err.Description)
	})
	install := `(function(){
		if (window.__lqProbe) return "already";
		window.__lqProbe = true;
		window.__lqErrors = [];
		window.addEventListener("error", function(e){ window.__lqErrors.push(String(e.message) + " @ " + String(e.filename) + ":" + String(e.lineno)); });
		window.addEventListener("unhandledrejection", function(e){ window.__lqErrors.push("rejection: " + String(e.reason && e.reason.stack || e.reason)); });
		var oe = console.error;
		console.error = function(){ try { window.__lqErrors.push("console.error: " + Array.from(arguments).map(function(a){return a && a.stack || String(a)}).join(" | ")); } catch(_){} oe.apply(console, arguments); };
		return "installed";
	})()`
	probe := `(function(){
		var ac = document.getElementById('app-container');
		var cs = ac ? getComputedStyle(ac) : null;
		var sheets = Array.from(document.styleSheets).map(function(s){return (s.href||'inline').split('/').pop()}).slice(0,12);
		var cssRules = 0;
		try { cssRules = document.styleSheets.length ? document.styleSheets[0].cssRules.length : -1 } catch(e){ cssRules = 'ERR:' + e.message }
		var bodyCS = getComputedStyle(document.body);
		return JSON.stringify({
			sheets: sheets,
			cssRules: cssRules,
			ac: cs ? {opacity: cs.opacity, vis: cs.visibility, disp: cs.display, bg: cs.backgroundColor, transform: cs.transform} : 'NO AC',
			bodyBg: bodyCS.backgroundColor, bodyColor: bodyCS.color,
			fonts: document.fonts ? document.fonts.status : 'n/a',
			text: String(document.body.innerText).slice(0, 150),
			errs: (window.__lqErrors || []).slice(0, 6)
		});
	})()`
	go func() {
		time.Sleep(8 * time.Second)
		if png, err := w.CapturePage(); err != nil {
			log.Printf("[debug] capture error: %v", err)
		} else if err := os.WriteFile("/tmp/mygo-capture.png", png, 0o644); err != nil {
			log.Printf("[debug] capture write: %v", err)
		} else {
			log.Printf("[debug] capture saved %d bytes", len(png))
		}
	}()
	go func() {
		time.Sleep(3 * time.Second)
		if v, err := w.Page().Eval(install); err != nil {
			log.Printf("[debug] install error: %v", err)
		} else {
			log.Printf("[debug] collector: %v", v)
		}
		for i := 0; i < 15; i++ {
			time.Sleep(3 * time.Second)
			v, err := w.Page().Eval(probe)
			if err != nil {
				log.Printf("[debug] probe error: %v", err)
				continue
			}
			b, _ := json.Marshal(v)
			log.Printf("[debug] probe#%d: %s", i, string(b))
		}
	}()
}
