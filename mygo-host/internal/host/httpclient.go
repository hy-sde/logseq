package host

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// httpClient returns the shared client used for renderer-initiated requests.
// Proxy handling: milestone 1 follows env/system defaults; explicit proxy
// options are stored but only applied to testProxyUrl.
func httpClient() *http.Client {
	return &http.Client{Timeout: 0}
}

func jsonReader(v any) io.Reader {
	data, _ := json.Marshal(v)
	return bytes.NewReader(data)
}

func readAllBody(r io.Reader) ([]byte, error) { return io.ReadAll(r) }

// testProxyURLCmd ports handler testProxyUrl: fetch url with proxy options,
// 2xx → {code, response-ms}, else error; 10s timeout.
func (h *Host) testProxyURLCmd(urlToTest string, opts map[string]any) (any, error) {
	start := timeNow()
	client := &http.Client{Timeout: 10 * time.Second}
	if proxyURL := proxyURLFromOptions(opts); proxyURL != "" {
		transport := &http.Transport{}
		if pu, err := url.Parse(proxyURL); err == nil {
			transport.Proxy = http.ProxyURL(pu)
			if pu.Scheme == "socks5" {
				// stdlib supports socks5 via Transport? Not directly; register dialer is
				// out of scope for milestone 1 (documented deviation).
				_ = pu
			}
		}
		client.Transport = transport
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, urlToTest, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		if isTimeoutErr(err) {
			return nil, errors.New("Timeout")
		}
		return nil, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	elapsed := timeNow().Sub(start).Milliseconds()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return map[string]any{"code": resp.StatusCode, "response-ms": elapsed}, nil
	}
	return nil, fmt.Errorf("HTTP status %d", resp.StatusCode)
}

func isTimeoutErr(err error) bool {
	var ne interface{ Timeout() bool }
	return errors.As(err, &ne) && ne.Timeout()
}

func proxyURLFromOptions(opts map[string]any) string {
	if opts == nil {
		return ""
	}
	// Renderer shape (components/plugins.cljs normalize-opts): always
	// {:type "system"|"direct"|"http"|"socks5", :test url}; http/socks5 add
	// :protocol, :host and :port as a STRING (util/normalize-port-input).
	switch strings.ToLower(stringOf(opts, "type")) {
	case "socks5", "http":
		host := stringOf(opts, "host")
		port := proxyPort(opts["port"])
		if host != "" && port > 0 {
			return fmt.Sprintf("%s://%s:%d", strings.ToLower(stringOf(opts, "type")), host, port)
		}
	}
	return ""
}

// proxyPort accepts the renderer's string port and tolerates numeric JSON.
func proxyPort(v any) int {
	switch p := v.(type) {
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil {
			return 0
		}
		return n
	case float64:
		return int(p)
	case int:
		return p
	default:
		return 0
	}
}

// httpFetchJSONCmd ports handler httpFetchJSON.
func (h *Host) httpFetchJSONCmd(urlToFetch string, opts map[string]any) (any, error) {
	result, err := h.httpRequestCmd("", map[string]any{
		"url":        urlToFetch,
		"returnType": "json",
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// httpRequestCmd ports handler httpRequest (handler.cljs:475).
func (h *Host) httpRequestCmd(reqID string, opts map[string]any) (any, error) {
	rawURL := stringOf(opts, "url")
	if rawURL == "" {
		return nil, errors.New("url is required")
	}
	method := strings.ToUpper(stringOf(opts, "method"))
	if method == "" {
		method = http.MethodGet
	}
	returnType := strings.ToLower(stringOf(opts, "returnType"))
	if returnType == "" {
		returnType = "json"
	}
	timeout := int64Of(opts, "timeout")
	abortable := boolOf(opts, "abortable")

	ctx := context.Background()
	var cancel context.CancelFunc
	if abortable || timeout > 0 {
		ctx, cancel = context.WithCancel(ctx)
		key := reqID
		if key == "" {
			key = fmt.Sprintf("req-%d", timeNow().UnixNano())
		}
		h.registerAbort(key, cancel)
		defer h.popAbort(key)
		if timeout > 0 {
			var cancelTimeout context.CancelFunc
			ctx, cancelTimeout = context.WithTimeout(ctx, time.Duration(timeout)*time.Millisecond)
			defer cancelTimeout()
		}
	}

	var bodyReader io.Reader
	if method != http.MethodGet && method != http.MethodHead {
		payload := argOf([]any{opts}, 0)
		if b, ok := opts["body"]; ok && b != nil {
			payload = b
		} else if d, ok := opts["data"]; ok && d != nil {
			payload = d
		}
		bodyReader = bodyReaderFor(payload)
	}

	req, err := http.NewRequestWithContext(ctx, method, rawURL, bodyReader)
	if err != nil {
		return nil, err
	}
	if headers, ok := opts["headers"].(map[string]any); ok {
		for k, v := range headers {
			if s, ok := v.(string); ok {
				req.Header.Set(k, s)
			}
		}
	}
	client := &http.Client{Timeout: 0}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	bodyIsEmpty := method == http.MethodHead || resp.StatusCode == 204 || resp.StatusCode == 205
	var body any
	if !bodyIsEmpty {
		data, err := readAllBody(resp.Body)
		if err != nil {
			return nil, err
		}
		switch returnType {
		case "json":
			var parsed any
			if len(data) > 0 {
				if err := json.Unmarshal(data, &parsed); err != nil {
					return nil, err
				}
			}
			body = parsed
		case "text":
			body = string(data)
		case "base64":
			body = base64.StdEncoding.EncodeToString(data)
		case "arraybuffer":
			body = map[string]any{"__lqBytes": base64.StdEncoding.EncodeToString(data)}
		default:
			body = string(data)
		}
	}
	if boolOf(opts, "includeResponse") {
		respHeaders := map[string]any{}
		for k, vs := range resp.Header {
			if len(vs) == 1 {
				respHeaders[k] = vs[0]
			} else {
				respHeaders[k] = append([]any{}, vs)
			}
		}
		return map[string]any{
			"status":     resp.StatusCode,
			"statusText": resp.Status,
			"ok":         resp.StatusCode >= 200 && resp.StatusCode < 300,
			"url":        rawURL,
			"headers":    respHeaders,
			"body":       body,
		}, nil
	}
	return body, nil
}

// bodyReaderFor converts renderer payloads to a request body.
func bodyReaderFor(payload any) io.Reader {
	switch v := payload.(type) {
	case nil:
		return nil
	case string:
		return strings.NewReader(v)
	case map[string]any:
		if b64 := stringOf(v, "__lqBytes"); b64 != "" {
			if data, err := base64.StdEncoding.DecodeString(b64); err == nil {
				return bytes.NewReader(data)
			}
		}
		return jsonReader(v)
	case []any:
		return jsonReader(v)
	default:
		return jsonReader(v)
	}
}

func (h *Host) httpRequestAbortCmd(reqID string) {
	if cancel := h.abortFunc(reqID); cancel != nil {
		cancel()
	}
}

// aborts registry accessors (backing the registerAbort/popAbort on Host).
type abortRegistry struct {
	mu     sync.Mutex
	aborts map[string]context.CancelFunc
}

func (h *Host) abortFunc(reqID string) context.CancelFunc {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.aborts == nil {
		return nil
	}
	return h.aborts[reqID]
}

func int64Of(m map[string]any, key string) int64 {
	switch v := m[key].(type) {
	case int64:
		return v
	case int:
		return int64(v)
	case float64:
		return int64(v)
	case json.Number:
		n, _ := v.Int64()
		return n
	}
	return 0
}
