(ns electron.ipc
  "Provides fns to send ipc messages to electron's main process. Under the mygo
  (Go) desktop host the same window.apis surface is served with plain JSON
  payloads instead of transit."
  (:require [cljs-bean.core :as bean]
            [clojure.string :as string]
            [promesa.core :as p]
            [frontend.util :as util]
            [logseq.db.sqlite.util :as sqlite-util]))

(defn- maybe-read-transit
  [result]
  (if (string? result)
    (sqlite-util/read-transit-str result)
    result))

(defn- js-pure-object?
  [x]
  (or (array? x)
    (and (some? x)
      (= "[object Object]"
        (.call (.-toString (.-prototype js/Object)) x))
      (let [prototype (js/Object.getPrototypeOf x)]
        (or (nil? prototype)
          (nil? (js/Object.getPrototypeOf prototype)))))))

(defn- has-js-obj?
  [args]
  (some js-pure-object? args))

(defn- u8->base64
  "Encode a Uint8Array as base64 (chunked to stay under btoa argument limits)."
  [^js u8]
  (->> (iterate #(+ % 0x8000) 0)
       (take-while #(< % (.-length u8)))
       (map (fn [start]
              (let [end (min (+ start 0x8000) (.-length u8))]
                (js/btoa (.apply js/String.fromCharCode nil (.slice u8 start end))))))
       (string/join "")))

(defn- base64->u8
  [^string b64]
  (let [bin (js/atob b64)
        n (.-length bin)
        u8 (js/Uint8Array. n)]
    (dotimes [i n]
      (aset u8 i (.charCodeAt bin i)))
    u8))

(defn- binary?
  [x]
  (or (instance? js/ArrayBuffer x)
      (instance? js/Uint8Array x)
      (instance? js/DataView x)))

(defn- kw-name
  [x]
  (let [s (str x)]
    (if (string/starts-with? s ":") (subs s 1) s)))

(defn- ->mygo-arg
  "JSON-able conversion for the mygo host. Keywords keep their full name
  (:logseq/local-graph -> \"logseq/local-graph\"), binary payloads become base64
  strings, cljs collections convert recursively; plain JS values pass through."
  [x]
  (cond
    (keyword? x)      (kw-name x)
    (symbol? x)       (str x)
    (binary? x)       (u8->base64 (if (instance? js/Uint8Array x) x (js/Uint8Array. x)))
    (map? x)          (clj->js (into {} (map (fn [[k v]]
                                               [(if (keyword? k) (kw-name k) k)
                                                (->mygo-arg v)])
                                             x)))
    (set? x)          (clj->js (mapv ->mygo-arg (vec x)))
    (sequential? x)   (clj->js (mapv ->mygo-arg x))
    :else             (clj->js x)))

(def ^:private binary-result-cmds
  "Commands whose Electron main returned Node Buffers (structured-cloned to
  Uint8Array in the renderer). The mygo host returns base64 strings for these
  and the conversion happens here."
  #{"readFileRaw" "db-get" "db-export" "db-export-as"})

(defn- mygo-postprocess
  [cmd result]
  (cond
    (and (contains? binary-result-cmds cmd) (string? result))
    (base64->u8 result)

    (or (array? result) (js-pure-object? result))
    (js->clj result :keywordize-keys true)

    :else result))

(defn ipc
  [& args]
  (when (util/electron?)
    (cond
      (util/mygo?)
      (p/let [result (js/window.apis.doAction (clj->js (mapv ->mygo-arg args)))]
        (mygo-postprocess (kw-name (first args)) result))

      (has-js-obj? args)
      (js/window.apis.doAction (bean/->js (conj (vec args) :js-obj)))

      :else
      (p/let [payload (sqlite-util/write-transit-str args)
              maybe-result-str (js/window.apis.doAction payload)]
        (maybe-read-transit maybe-result-str)))))

(defn invoke
  [channel & args]
  (when (util/electron?)
    (p/let [result (if (util/mygo?)
                     (js/window.apis.invoke channel (clj->js (mapv ->mygo-arg args)))
                     (js/window.apis.invoke channel (bean/->js args)))]
      result)))
