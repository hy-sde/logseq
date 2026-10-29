// Package edn parses and prints the EDN subset that Logseq persists:
// graphs.edn (vector of registry entry maps) and configs.edn (config map).
// Supported: nil/true/false, integers, floats, strings, keywords, symbols,
// vectors, maps, sets; #tag values parse as the tagged value (tag dropped).
package edn

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// Parse parses one EDN form into Go values:
// keyword -> ":kw" string, symbol -> "sym", vector/list -> []any,
// map -> map[string]any (keys stringified like transit), set -> []any,
// strings/numbers/bools/nil native.
func Parse(src string) (any, error) {
	p := &parser{src: src}
	p.ws()
	v, err := p.form()
	if err != nil {
		return nil, err
	}
	return v, nil
}

type parser struct {
	src string
	pos int
}

func (p *parser) ws() {
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		if c == ';' { // comment to end of line
			for p.pos < len(p.src) && p.src[p.pos] != '\n' {
				p.pos++
			}
			continue
		}
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',' {
			p.pos++
			continue
		}
		return
	}
}

func (p *parser) form() (any, error) {
	p.ws()
	if p.pos >= len(p.src) {
		return nil, fmt.Errorf("edn: unexpected end of input")
	}
	c := p.src[p.pos]
	switch {
	case c == '(' || c == '[':
		close := byte(')')
		if c == '[' {
			close = ']'
		}
		return p.seq(close)
	case c == '{':
		return p.mapForm()
	case c == '#' && p.pos+1 < len(p.src) && p.src[p.pos+1] == '{':
		p.pos++
		return p.seq('}')
	case c == '#':
		// tagged literal or set: drop the tag, parse the value
		p.pos++
		for p.pos < len(p.src) && !unicode.IsSpace(rune(p.src[p.pos])) && p.src[p.pos] != '(' && p.src[p.pos] != '[' && p.src[p.pos] != '{' {
			p.pos++
		}
		return p.form()
	case c == '"':
		return p.str()
	default:
		return p.token()
	}
}

func (p *parser) seq(close byte) (any, error) {
	p.pos++ // opening
	items := []any{}
	for {
		p.ws()
		if p.pos >= len(p.src) {
			return nil, fmt.Errorf("edn: unterminated sequence")
		}
		if p.src[p.pos] == close {
			p.pos++
			return items, nil
		}
		v, err := p.form()
		if err != nil {
			return nil, err
		}
		items = append(items, v)
	}
}

func (p *parser) mapForm() (any, error) {
	p.pos++ // {
	out := map[string]any{}
	for {
		p.ws()
		if p.pos >= len(p.src) {
			return nil, fmt.Errorf("edn: unterminated map")
		}
		if p.src[p.pos] == '}' {
			p.pos++
			return out, nil
		}
		k, err := p.form()
		if err != nil {
			return nil, err
		}
		p.ws()
		v, err := p.form()
		if err != nil {
			return nil, err
		}
		out[Key(k)] = v
	}
}

func (p *parser) str() (string, error) {
	p.pos++ // opening quote
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
			case 'n':
				sb.WriteByte('\n')
			case 't':
				sb.WriteByte('\t')
			case 'r':
				sb.WriteByte('\r')
			case '"':
				sb.WriteByte('"')
			case '\\':
				sb.WriteByte('\\')
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
	return "", fmt.Errorf("edn: unterminated string")
}

func (p *parser) token() (any, error) {
	start := p.pos
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',' || c == ')' || c == ']' || c == '}' {
			break
		}
		p.pos++
	}
	tok := p.src[start:p.pos]
	if tok == "" {
		return nil, fmt.Errorf("edn: empty token at %d", p.pos)
	}
	switch tok {
	case "nil":
		return nil, nil
	case "true":
		return true, nil
	case "false":
		return false, nil
	}
	if strings.HasPrefix(tok, ":") {
		return tok, nil
	}
	if iv, err := strconv.ParseInt(tok, 10, 64); err == nil {
		return iv, nil
	}
	if fv, err := strconv.ParseFloat(tok, 64); err == nil {
		return fv, nil
	}
	return tok, nil // symbol
}

// Key stringifies an EDN map key: keywords and symbols lose their leading
// colon, matching how the renderer sees them via cljs.
func Key(k any) string {
	switch v := k.(type) {
	case string:
		return strings.TrimPrefix(v, ":")
	default:
		return fmt.Sprintf("%v", v)
	}
}

// Print renders Go values back to EDN (pr-str shape, one line).
func Print(v any) string {
	var sb strings.Builder
	printValue(&sb, v)
	return sb.String()
}

func printValue(sb *strings.Builder, v any) {
	switch t := v.(type) {
	case nil:
		sb.WriteString("nil")
	case bool:
		if t {
			sb.WriteString("true")
		} else {
			sb.WriteString("false")
		}
	case int:
		sb.WriteString(strconv.Itoa(t))
	case int64:
		sb.WriteString(strconv.FormatInt(t, 10))
	case float64:
		sb.WriteString(strconv.FormatFloat(t, 'g', -1, 64))
	case string:
		if strings.HasPrefix(t, ":") && isValidEDNKeyword(t) {
			sb.WriteString(t) // keyword
			return
		}
		sb.WriteString(strconv.Quote(t))
	case []any:
		sb.WriteString("[")
		for i, e := range t {
			if i > 0 {
				sb.WriteString(" ")
			}
			printValue(sb, e)
		}
		sb.WriteString("]")
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		sb.WriteString("{")
		for i, k := range keys {
			if i > 0 {
				sb.WriteString(", ")
			}
			printValue(sb, ednKey(k))
			sb.WriteString(" ")
			printValue(sb, t[k])
		}
		sb.WriteString("}")
	default:
		sb.WriteString(fmt.Sprintf("%v", v))
	}
}

func ednKey(k string) string {
	if isValidEDNKeyword(":" + k) {
		return ":" + k
	}
	return k
}

func isValidEDNKeyword(s string) bool {
	if len(s) < 2 || s[0] != ':' {
		return false
	}
	for _, c := range s[1:] {
		if unicode.IsSpace(c) || c == '"' || c == '(' || c == ')' || c == '[' || c == ']' || c == '{' || c == '}' || c == ',' {
			return false
		}
	}
	return true
}

// VectorOf casts an EDN parse result to a vector of values ([]any).
func VectorOf(v any) []any {
	switch t := v.(type) {
	case []any:
		return t
	case nil:
		return nil
	default:
		return []any{v}
	}
}

