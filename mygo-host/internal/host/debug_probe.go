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
		if (!window.__evtCapture) {
			window.__evtCapture = [];
			["redirectWhenExists", "redirect", "openNewWindowOfGraph", "notification", "updates-callback", "quickCapture"].forEach(function(name){
				try { window.apis.addListener(name, function(d){ window.__evtCapture.push(name + ": " + JSON.stringify(d).slice(0, 220)); }); } catch(_){}
			});
		}
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
			evts: (window.__evtCapture || []).slice(-6),
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
		// Edit-path smoke: insert a block through the plugin API after the
		// graph is open (exercises renderer -> IPC -> daemon -> SQLite).
		time.Sleep(20 * time.Second)
		edit := `(function(){
			var api = window.logseq && window.logseq.api;
			if (!api || !api.get_all_pages) return "no api; keys=" + Object.keys(window.logseq || {}).join(",") + " apiKeys=" + (api ? Object.keys(api).slice(0,40).join(",") : "n/a");
			var ib = typeof api.insertBlock + "/" + typeof api.insert_block;
			return api.get_all_pages().then(function(pages){
				var page = pages && pages.length && (pages.find(function(p){ return /demo/i.test(String(p.name || p["original-name"] || p.title || "")) }) || pages[0]);
				if (!page) return "no pages";
				var id = page.uuid || page.id || page.name;
				return api.insert_block(id, "mygo-host edit smoke " + Date.now()).then(function(b){
					return "inserted uuid=" + (b && (b.uuid || (b.data && b.data.uuid)));
				});
			}).catch(function(e){
				return "ERR:" + ib + " msg=" + String(e && e.message) + " data=" + (e && e.data ? JSON.stringify(e.data).slice(0, 300) : "none") + " str=" + String(e).slice(0, 120);
			});
		})()`
		v, err := w.Page().Eval(edit)
		if err != nil {
			log.Printf("[debug] edit eval error: %v", err)
			return
		}
		b, _ := json.Marshal(v)
		log.Printf("[debug] edit result: %s", string(b))
		// Read back via datascript inside the page (confirms transacted state).
		time.Sleep(3 * time.Second)
		back, err := w.Page().Eval(`(function(){
			var api = window.logseq && window.logseq.api;
			if (!api || !api.datascript_query) return "no datascript_query";
			return api.datascript_query('[:find (count ?b) . :where [?b :block/content ?c] [(re-find #"mygo-host edit smoke" ?c)]]').then(function(r){
				return "readback count=" + JSON.stringify(r);
			}).catch(function(e){ return "RB-ERR:" + String(e && e.message || e); });
		})()`)
		if err != nil {
			log.Printf("[debug] readback eval error: %v", err)
			return
		}
		b2, _ := json.Marshal(back)
		log.Printf("[debug] readback: %s", string(b2))
		// Persistence is verified from the daemon's SQLite file (checked
		// externally with sqlite3) - datascript readback APIs vary by build.
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
