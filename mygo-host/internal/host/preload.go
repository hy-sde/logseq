package host

// preloadJS is injected into every page before its own scripts. The mygo
// bridge (window.mygo) is injected before this script, so it can bind
// immediately. It reproduces the Electron preload (resources/js/preload.js)
// surface on top of window.mygo.
//
// Binary conventions: JS ArrayBuffers are sent to Go as {"__lqBytes": base64}
// and Go binary results are returned as {"__lqBytes": base64} which this
// shim converts back to Uint8Array (JSON cannot carry binary).
const preloadJS = `(() => {
  if (window.apis) return;
  // Boot-error collector: must exist before any app script runs.
  window.__lqErrors = [];
  window.addEventListener("error", function(e){ window.__lqErrors.push(String(e.message) + " @ " + String(e.filename) + ":" + String(e.lineno)); });
  window.addEventListener("unhandledrejection", function(e){ window.__lqErrors.push("rejection: " + String(e.reason && (e.reason.stack || e.reason.message) || e.reason)); });
  const MY = () => window.mygo;
  // mygo maps JS call arguments POSITIONALLY onto Go method params: for
  // LqChannels.Handle(ctx, channel string, args []any) the JS call must be
  // call('LqChannels.Handle', channel, <args array>) — the channel args go
  // as ONE array, never spread.
  const call = (method, ...args) => MY().call(method, ...args);
  const mainCmd = (arg) => call('LqMain.Handle', arg);

  const bytesToBase64 = (buf) => {
    const bytes = new Uint8Array(buf);
    let bin = '';
    const CHUNK = 0x8000;
    for (let i = 0; i < bytes.length; i += CHUNK) {
      bin += String.fromCharCode.apply(null, bytes.subarray(i, i + CHUNK));
    }
    return btoa(bin);
  };

  // Electron sends ArrayBuffers over IPC; JSON cannot. Wrap binary as a
  // base64-carrying marker object Go understands.
  const wrapBinary = (data) => {
    if (data == null) return data;
    if (data instanceof ArrayBuffer) return { __lqBytes: bytesToBase64(data) };
    if (ArrayBuffer.isView(data)) {
      return { __lqBytes: bytesToBase64(data.buffer.slice(data.byteOffset, data.byteOffset + data.byteLength)) };
    }
    return data;
  };

  // Go binary results arrive as {"__lqBytes": base64}; convert to Uint8Array
  // (the Electron IPC equivalent of a Buffer).
  const unwrapBinary = (v) => {
    if (v && typeof v === 'object' && !Array.isArray(v) && typeof v.__lqBytes === 'string') {
      const bin = atob(v.__lqBytes);
      const out = new Uint8Array(bin.length);
      for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
      return out;
    }
    return v;
  };

  const listeners = new Map();
  // Electron's addListener binds ipcRenderer.on: callbacks receive the
  // IpcRendererEvent first and the payload second. mygo events dispatch
  // listeners with a single payload argument, so the shim forwards the
  // Electron (event, payload) shape here. (apis.on strips the event like
  // Electron's preload on-channel helper; listener.cljs relies on that.)
  const addListenerImpl = (channel, cb) => {
    const off = MY().on(channel, (payload) => cb(null, payload));
    if (!listeners.has(channel)) listeners.set(channel, new Set());
    listeners.get(channel).add(off);
    return off;
  };

  const ALLOWED_OPEN_EXTERNAL = ['http:', 'https:', 'mailto:', 'zotero:', 'file:'];

  const isAbsolutePath = (path) => {
    if (typeof path !== 'string' || path.length === 0) return false;
    if (path.startsWith('/')) return true;
    if (/^[a-zA-Z]:[\\\\/]/.test(path)) return true; // win drive
    if (path.startsWith('~')) return true;
    return false;
  };

  window.apis = {
    getFilePath(file) {
      // Electron: webUtils.getPathForFile(File). mygo pages cannot resolve
      // File objects to OS paths; renderer falls back to file.path.
      return file && typeof file === 'object' && typeof file.path === 'string' ? file.path : null;
    },

    doAction(arg) {
      // arg is the [cmd, ...args] payload (array; legacy transit string).
      return mainCmd(arg);
    },

    invoke(channel, ...args) {
      if (channel === 'main') return mainCmd(args[0]);
      if (channel.startsWith(':electron.server/sync!')) {
        return call('LqSync.Handle', channel, ...args);
      }
      return call('LqChannels.Handle', channel, ...args);
    },

    addListener(channel, cb) { addListenerImpl(channel, cb); },
    removeListener(channel, cb) {
      // cb was returned an unsubscribe when registered; mygo listeners are
      // removed by handle, so this is best-effort via stored handles.
      const set = listeners.get(channel);
      if (set && set.size) {
        set.forEach((off) => off());
        listeners.delete(channel);
      }
    },
    removeAllListeners(channel) {
      const set = listeners.get(channel);
      if (set) { set.forEach((off) => off()); listeners.delete(channel); }
    },
    on(channel, cb) { return addListenerImpl(channel, cb); },
    off(channel, cb) { window.apis.removeListener(channel, cb); },
    once(channel, cb) { return MY().once(channel, (payload) => cb(payload)); },

    checkForUpdates(...args) { return call('LqChannels.Handle', 'check-for-updates', args); },
    setUpdatesCallback(cb) { return addListenerImpl('updates-callback', cb); },
    installUpdatesAndQuitApp() { return call('LqChannels.Handle', 'install-updates', [true]); },

    openExternal(url, options) {
      let parsed;
      try { parsed = new URL(url); } catch { throw new Error('illegal protocol: ' + url); }
      if (!ALLOWED_OPEN_EXTERNAL.includes(parsed.protocol)) {
        throw new Error('illegal protocol');
      }
      return call('LqChannels.Handle', 'open-external', [url, options ?? null]);
    },

    openPath(relativePath) {
      return call('LqChannels.Handle', 'open-path', [relativePath]);
    },

    exportPublishAssets(html, customCSSPath, exportCSSPath, repoPath, assetFilenames, outputDir) {
      return call('LqChannels.Handle', 'export-publish-assets',
        [html, customCSSPath, exportCSSPath, repoPath, assetFilenames ?? [], outputDir]);
    },

    toggleMaxOrMinActiveWindow(isToggleMin) {
      return call('LqChannels.Handle', 'toggle-max-or-min-active-win', [!!isToggleMin]);
    },

    _callApplication(type, ...args) {
      return call('LqChannels.Handle', 'call-application', [type, ...args]);
    },

    _callMainWin(type, ...args) {
      return call('LqChannels.Handle', 'call-main-win', [type, ...args]);
    },

    writeFileBytes(filePath, data) {
      const b64 = data instanceof ArrayBuffer || ArrayBuffer.isView(data) ? bytesToBase64(
        data instanceof ArrayBuffer ? data
          : data.buffer.slice(data.byteOffset, data.byteOffset + data.byteLength)) : null;
      return mainCmd(['writeFileBytes', filePath, b64]);
    },

    getFilePathFromClipboard() { return mainCmd(['getFilePathFromClipboard']); },

    getClipboardData(format) { return mainCmd(['getClipboardData', format]).then(unwrapBinary); },

    setZoomFactor(factor) { return mainCmd(['setZoomFactor', factor]); },
    setZoomLevel(level) { return mainCmd(['setZoomLevel', level]); },

    isAbsolutePath,
  };
})();`
