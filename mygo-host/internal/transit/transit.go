// Package transit implements the subset of transit-json needed to talk to
// the Logseq renderer and db-worker-node daemon.
//
// The renderer (electron.ipc) serializes main-channel payloads with
// cognitect transit :json (write-transit-str), and the daemon returns
// {ok, resultTransit} where resultTransit is a transit-json string. This
// package decodes those payloads into plain Go values:
//
//	~:kw -> ":kw" (keyword), ~$sym -> "$sym", ~n123 -> int64, ~f1.5 -> float64,
//	~b<base64> -> []byte, ~cX -> rune, ~i<uuid> -> string, ~t/~m -> RFC3339 string,
//	"~~" -> "~", ["~#'", v] -> v (quote), ["~#list", ...] -> []any,
//	["~#set", ...] -> []any, ["~#cmap", k1 v1 ...] -> map[string]any,
//	JSON objects -> map[string]any (keys decoded like scalar strings),
//	custom tags ("error", "js/Error", "datascript/Entity") -> their payload map.
//
// A best-effort key cache ("~^N") is honored; cache indexes are decoded
// base62 (0-9, a-z, A-Z). Encoding is intentionally minimal: Go -> daemon
// args are plain JSON arrays of strings, which transit reads unchanged.
package transit

import (
	"encoding/base64"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// Decode parses a transit-json document into plain Go values.
func Decode(data string) (any, error) {
	p := &parser{src: data}
	p.ws()
	v, err := p.value(true, true)
	if err != nil {
		return nil, err
	}
	return v, nil
}

// DecodeArray decodes and requires the top value to be an array (the
// renderer always sends a vector [cmd, args...]).
func DecodeArray(data string) ([]any, error) {
	v, err := Decode(data)
	if err != nil {
		return nil, err
	}
	arr, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("transit: expected array payload, got %T", v)
	}
	return arr, nil
}

type parser struct {
	src   string
	pos   int
	cache []any
}

func (p *parser) ws() {
	for p.pos < len(p.src) {
		switch p.src[p.pos] {
		case ' ', '\t', '\n', '\r', ',':
			p.pos++
		default:
			return
		}
	}
}

func (p *parser) value(asKey bool, top bool) (any, error) {
	if p.pos >= len(p.src) {
		return nil, fmt.Errorf("transit: unexpected end of input")
	}
	switch p.src[p.pos] {
	case '{':
		return p.object()
	case '[':
		return p.array()
	case '"':
		s, err := p.str()
		if err != nil {
			return nil, err
		}
		return decodeStringToken(s, asKey, p)
	case 'n':
		p.pos++ // null
		return nil, nil
	case 't':
		p.pos++ // true
		return true, nil
	case 'f':
		p.pos++ // false
		return false, nil
	default:
		// number (possibly with leading -)
		start := p.pos
		for p.pos < len(p.src) {
			c := p.src[p.pos]
			if (c >= '0' && c <= '9') || c == '-' || c == '+' || c == '.' || c == 'e' || c == 'E' {
				p.pos++
			} else {
				break
			}
		}
		numStr := p.src[start:p.pos]
		if numStr == "" {
			return nil, fmt.Errorf("transit: unexpected char %q at %d", p.src[p.pos], p.pos)
		}
		return parseNumber(numStr)
	}
}

func parseNumber(s string) (any, error) {
	if iv, err := strconv.ParseInt(s, 10, 64); err == nil {
		return iv, nil
	}
	if fv, err := strconv.ParseFloat(s, 64); err == nil {
		return fv, nil
	}
	return nil, fmt.Errorf("transit: bad number %q", s)
}

func (p *parser) object() (any, error) {
	p.pos++ // {
	out := map[string]any{}
	p.ws()
	if p.pos < len(p.src) && p.src[p.pos] == '}' {
		p.pos++
		return out, nil
	}
	for {
		p.ws()
		if p.pos >= len(p.src) {
			return nil, fmt.Errorf("transit: unterminated object")
		}
		k, err := p.value(true, false)
		if err != nil {
			return nil, err
		}
		p.ws()
		if p.pos >= len(p.src) || p.src[p.pos] != ':' {
			return nil, fmt.Errorf("transit: expected ':' in object at %d", p.pos)
		}
		p.pos++
		p.ws()
		v, err := p.value(false, false)
		if err != nil {
			return nil, err
		}
		key := keyString(k)
		out[key] = v
		cacheKey(p, key)
		p.ws()
		if p.pos < len(p.src) && p.src[p.pos] == ',' {
			p.pos++
			continue
		}
		if p.pos < len(p.src) && p.src[p.pos] == '}' {
			p.pos++
			return out, nil
		}
		return nil, fmt.Errorf("transit: expected ',' or '}' at %d", p.pos)
	}
}

func (p *parser) array() (any, error) {
	p.pos++ // [
	items := []any{}
	p.ws()
	if p.pos < len(p.src) && p.src[p.pos] == ']' {
		p.pos++
		return items, nil
	}
	// tagged value: ["~#tag", ...]
	if p.pos < len(p.src) && p.src[p.pos] == '"' {
		save := p.pos
		s, err := p.str()
		if err == nil && strings.HasPrefix(s, "~#") {
			tag := s[2:]
			p.ws()
			var vals []any
			if p.pos < len(p.src) && p.src[p.pos] != ']' {
				for {
					p.ws()
					v, err := p.value(false, false)
					if err != nil {
						return nil, err
					}
					vals = append(vals, v)
					p.ws()
					if p.pos < len(p.src) && p.src[p.pos] == ',' {
						p.pos++
						continue
					}
					break
				}
			}
			p.ws()
			if p.pos >= len(p.src) || p.src[p.pos] != ']' {
				return nil, fmt.Errorf("transit: unterminated tagged array at %d", p.pos)
			}
			p.pos++
			return decodeTagged(tag, vals), nil
		}
		p.pos = save
	}
	for {
		p.ws()
		v, err := p.value(false, false)
		if err != nil {
			return nil, err
		}
		items = append(items, v)
		p.ws()
		if p.pos < len(p.src) && p.src[p.pos] == ',' {
			p.pos++
			continue
		}
		if p.pos < len(p.src) && p.src[p.pos] == ']' {
			p.pos++
			return items, nil
		}
		return nil, fmt.Errorf("transit: expected ',' or ']' at %d", p.pos)
	}
}

func (p *parser) str() (string, error) {
	if p.pos >= len(p.src) || p.src[p.pos] != '"' {
		return "", fmt.Errorf("transit: expected string at %d", p.pos)
	}
	p.pos++
	var sb strings.Builder
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		if c == '"' {
			p.pos++
			return sb.String(), nil
		}
		if c == '\\' && p.pos+1 < len(p.src) {
			e := p.src[p.pos+1]
			switch e {
			case '"':
				sb.WriteByte('"')
			case '\\':
				sb.WriteByte('\\')
			case '/':
				sb.WriteByte('/')
			case 'b':
				sb.WriteByte('\b')
			case 'f':
				sb.WriteByte('\f')
			case 'n':
				sb.WriteByte('\n')
			case 'r':
				sb.WriteByte('\r')
			case 't':
				sb.WriteByte('\t')
			case 'u':
				if p.pos+5 < len(p.src) {
					code, err := strconv.ParseUint(p.src[p.pos+2:p.pos+6], 16, 32)
					if err == nil {
						sb.WriteRune(rune(code))
						p.pos += 4
					}
				}
			default:
				sb.WriteByte(e)
			}
			p.pos += 2
			continue
		}
		sb.WriteByte(c)
		p.pos++
	}
	return "", fmt.Errorf("transit: unterminated string")
}

func keyString(k any) string {
	switch v := k.(type) {
	case string:
		return strings.TrimPrefix(v, ":")
	default:
		return fmt.Sprintf("%v", v)
	}
}

func decodeTagged(tag string, vals []any) any {
	switch tag {
	case "'":
		if len(vals) == 1 {
			return vals[0]
		}
		return vals
	case "list", "set", "vec":
		return append([]any{}, vals...)
	case "cmap":
		out := map[string]any{}
		for i := 0; i+1 < len(vals); i += 2 {
			out[keyString(vals[i])] = vals[i+1]
		}
		return out
	case "link":
		if len(vals) > 0 {
			if m, ok := vals[0].(map[string]any); ok {
				return m
			}
		}
		return map[string]any{}
	default:
		// unknown/custom tag ("error", "js/Error", "datascript/Entity", ...):
		// payload is its single rep (a map for our custom handlers)
		if len(vals) == 1 {
			return vals[0]
		}
		if len(vals) == 0 {
			return nil
		}
		return append([]any{}, vals...)
	}
}

func decodeStringToken(s string, asKey bool, p *parser) (any, error) {
	if !strings.HasPrefix(s, "~") {
		return s, nil
	}
	body := s[1:]
	if body == "" {
		return s, nil
	}
	switch body[0] {
	case '~':
		return "~", nil
	case ':':
		kw := body[1:]
		if asKey {
			return kw, nil // keyword keys arrive colonless
		}
		return ":" + kw, nil
	case '$':
		return "$" + body[1:], nil
	case '^':
		if len(body) > 1 {
			idx := decodeCacheIndex(body[1:])
			if idx >= 0 && idx < len(p.cache) {
				return p.cache[idx], nil
			}
		}
		return s, nil
	case 'n':
		if iv, err := strconv.ParseInt(body[1:], 10, 64); err == nil {
			return iv, nil
		}
		if iv, err := strconv.ParseInt(body[1:], 16, 64); err == nil {
			return iv, nil
		}
		return s, nil
	case 'f':
		if fv, err := strconv.ParseFloat(body[1:], 64); err == nil {
			return fv, nil
		}
		return s, nil
	case 'b':
		if raw, err := base64.StdEncoding.DecodeString(body[1:]); err == nil {
			return raw, nil
		}
		return s, nil
	case 'c':
		return body[1:], nil
	case 'i':
		return body[1:], nil // uuid
	case 't':
		return body[1:], nil // RFC3339 instant
	case 'm':
		if ms, err := strconv.ParseInt(body[1:], 10, 64); err == nil {
			return time.UnixMilli(ms).UTC().Format(time.RFC3339), nil
		}
		return s, nil
	case 'd':
		if days, err := strconv.ParseInt(body[1:], 10, 64); err == nil {
			return time.Unix(days*86400, 0).UTC().Format("2006-01-02"), nil
		}
		return s, nil
	case 'z':
		return body[1:], nil // big decimal as string
	case 'r':
		return body[1:], nil
	case 'U':
		return math.NaN(), nil
	default:
		return s, nil
	}
}

func decodeCacheIndex(s string) int {
	// transit cache indexes: base62, digits then lower then upper case
	n := 0
	for _, c := range s {
		var d int
		switch {
		case c >= '0' && c <= '9':
			d = int(c - '0')
		case c >= 'a' && c <= 'z':
			d = int(c-'a') + 10
		case c >= 'A' && c <= 'Z':
			d = int(c-'A') + 36
		default:
			return -1
		}
		n = n*62 + d
	}
	return n
}

func cacheKey(p *parser, key string) {
	if len(key) > 3 && !strings.HasPrefix(key, "~") {
		p.cache = append(p.cache, key)
	}
}
