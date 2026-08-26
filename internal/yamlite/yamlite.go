// Package yamlite parses the subset of YAML that AgentGate configuration uses,
// with no third-party dependencies.
//
// Why this exists: in a regulated financial-services environment every
// third-party module in the build is a security-review line item. The
// configuration format is fixed and small, so the parser is fixed and small
// too. See docs/adr/0014-dependency-policy.md.
//
// Supported:
//
//	# comments (whole-line and trailing)
//	key: value                      scalars: string, int, float, bool, null
//	key:                            nested block mappings by indentation
//	  child: value
//	list:                           block sequences
//	  - item
//	  - key: value                  sequences of mappings
//	    other: value
//	inline: [a, b, c]               flow sequences
//	inline: {a: 1, b: 2}            flow mappings
//	text: |                         literal block scalars
//	  line one
//	  line two
//	folded: >                       folded block scalars
//	"quoted: keys"                  single- and double-quoted scalars
//	---                             document separators (first document wins)
//
// Deliberately unsupported (rejected with a line-numbered error rather than
// silently mis-parsed): anchors and aliases, tags, multiple documents,
// complex keys, tab indentation.
package yamlite

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Unmarshal parses YAML into v. It works by parsing to a generic tree and then
// round-tripping through encoding/json, so struct tags, custom
// json.Unmarshaler implementations and type conversions all behave exactly as
// they do for JSON configuration.
func Unmarshal(data []byte, v any) error {
	tree, err := Parse(data)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(tree)
	if err != nil {
		return fmt.Errorf("yamlite: re-encode: %w", err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("yamlite: decode into %T: %w", v, err)
	}
	return nil
}

// Parse converts YAML into map[string]any / []any / scalar values.
func Parse(data []byte) (any, error) {
	lines, err := scan(string(data))
	if err != nil {
		return nil, err
	}
	if len(lines) == 0 {
		return map[string]any{}, nil
	}
	p := &parser{lines: lines}
	val, err := p.block(lines[0].indent)
	if err != nil {
		return nil, err
	}
	if p.pos < len(p.lines) {
		return nil, fmt.Errorf("yamlite: line %d: unexpected indentation", p.lines[p.pos].num)
	}
	return val, nil
}

type line struct {
	num    int    // 1-based source line, for errors
	indent int    // leading spaces
	text   string // content with indentation and trailing comment removed
	seq    bool   // line begins a sequence item ("- ")
}

// scan strips comments and blank lines and records indentation.
func scan(src string) ([]line, error) {
	var out []line
	for i, raw := range strings.Split(src, "\n") {
		num := i + 1
		if strings.ContainsRune(raw, '\t') && strings.TrimSpace(raw) != "" {
			if lead := raw[:len(raw)-len(strings.TrimLeft(raw, " \t"))]; strings.ContainsRune(lead, '\t') {
				return nil, fmt.Errorf("yamlite: line %d: tab indentation is not supported", num)
			}
		}
		trimmed := stripComment(raw)
		if strings.TrimSpace(trimmed) == "" {
			continue
		}
		if strings.TrimSpace(trimmed) == "---" {
			if len(out) > 0 {
				break // first document only
			}
			continue
		}
		if strings.TrimSpace(trimmed) == "..." {
			break
		}
		indent := len(trimmed) - len(strings.TrimLeft(trimmed, " "))
		body := strings.TrimRight(strings.TrimLeft(trimmed, " "), " ")
		if strings.HasPrefix(body, "&") || strings.HasPrefix(body, "*") {
			return nil, fmt.Errorf("yamlite: line %d: anchors and aliases are not supported", num)
		}
		l := line{num: num, indent: indent, text: body}
		if body == "-" || strings.HasPrefix(body, "- ") {
			l.seq = true
		}
		out = append(out, l)
	}
	return out, nil
}

// stripComment removes a trailing "#" comment that is not inside quotes.
func stripComment(s string) string {
	var quote rune
	for i, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			}
		case r == '\'' || r == '"':
			quote = r
		case r == '#':
			if i == 0 || s[i-1] == ' ' || s[i-1] == '\t' {
				return s[:i]
			}
		}
	}
	return s
}

type parser struct {
	lines []line
	pos   int
}

func (p *parser) peek() (line, bool) {
	if p.pos >= len(p.lines) {
		return line{}, false
	}
	return p.lines[p.pos], true
}

// block parses either a mapping or a sequence at the given indentation.
func (p *parser) block(indent int) (any, error) {
	l, ok := p.peek()
	if !ok {
		return nil, nil
	}
	if l.seq {
		return p.sequence(indent)
	}
	return p.mapping(indent)
}

func (p *parser) mapping(indent int) (any, error) {
	out := map[string]any{}
	for {
		l, ok := p.peek()
		if !ok || l.indent < indent {
			return out, nil
		}
		if l.indent > indent {
			return nil, fmt.Errorf("yamlite: line %d: unexpected indentation in mapping", l.num)
		}
		if l.seq {
			return nil, fmt.Errorf("yamlite: line %d: sequence item inside a mapping", l.num)
		}
		key, rest, err := splitKey(l.text, l.num)
		if err != nil {
			return nil, err
		}
		p.pos++
		val, err := p.value(rest, l, indent)
		if err != nil {
			return nil, err
		}
		out[key] = val
	}
}

func (p *parser) sequence(indent int) (any, error) {
	out := []any{}
	for {
		l, ok := p.peek()
		if !ok || l.indent < indent {
			return out, nil
		}
		if l.indent > indent {
			return nil, fmt.Errorf("yamlite: line %d: unexpected indentation in sequence", l.num)
		}
		if !l.seq {
			return out, nil
		}
		rest := strings.TrimSpace(strings.TrimPrefix(l.text, "-"))
		p.pos++
		if rest == "" {
			// Item body is the indented block that follows.
			next, ok := p.peek()
			if !ok || next.indent <= indent {
				out = append(out, nil)
				continue
			}
			v, err := p.block(next.indent)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
			continue
		}
		if key, kr, err := tryKey(rest); err == nil && key != "" {
			// "- key: value" starts a mapping whose remaining keys are the
			// following lines indented past the dash.
			child := indent + 2
			m := map[string]any{}
			v, err := p.value(kr, line{num: l.num, indent: child}, child)
			if err != nil {
				return nil, err
			}
			m[key] = v
			for {
				n, ok := p.peek()
				if !ok || n.indent <= indent || n.seq {
					break
				}
				k2, r2, err := splitKey(n.text, n.num)
				if err != nil {
					return nil, err
				}
				p.pos++
				v2, err := p.value(r2, n, n.indent)
				if err != nil {
					return nil, err
				}
				m[k2] = v2
			}
			out = append(out, m)
			continue
		}
		v, err := scalar(rest, l.num)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
}

// value resolves the right-hand side of "key:" — inline scalar, flow
// collection, block scalar, or an indented block.
func (p *parser) value(rest string, at line, indent int) (any, error) {
	rest = strings.TrimSpace(rest)
	switch {
	case rest == "|", rest == "|-", rest == ">", rest == ">-":
		return p.blockScalar(rest, indent)
	case rest == "":
		next, ok := p.peek()
		if !ok || next.indent <= indent {
			return nil, nil
		}
		return p.block(next.indent)
	default:
		return scalar(rest, at.num)
	}
}

func (p *parser) blockScalar(style string, indent int) (any, error) {
	var parts []string
	for {
		l, ok := p.peek()
		if !ok || l.indent <= indent {
			break
		}
		parts = append(parts, l.text)
		p.pos++
	}
	joiner := "\n"
	if strings.HasPrefix(style, ">") {
		joiner = " "
	}
	s := strings.Join(parts, joiner)
	if !strings.HasSuffix(style, "-") && s != "" {
		s += "\n"
	}
	return s, nil
}

func splitKey(text string, num int) (string, string, error) {
	k, rest, err := tryKey(text)
	if err != nil {
		return "", "", fmt.Errorf("yamlite: line %d: %w", num, err)
	}
	return k, rest, nil
}

// tryKey splits "key: rest", honouring quoted keys and ignoring colons that
// appear inside quotes or flow collections.
func tryKey(text string) (string, string, error) {
	var quote rune
	depth := 0
	for i, r := range text {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			}
		case r == '\'' || r == '"':
			quote = r
		case r == '[' || r == '{':
			depth++
		case r == ']' || r == '}':
			depth--
		case r == ':' && depth == 0:
			if i+1 < len(text) && text[i+1] != ' ' {
				continue // e.g. a bare URL value like http://host
			}
			key := strings.TrimSpace(text[:i])
			key = unquote(key)
			if key == "" {
				return "", "", fmt.Errorf("empty mapping key")
			}
			return key, strings.TrimSpace(text[i+1:]), nil
		}
	}
	if strings.HasSuffix(text, ":") {
		return unquote(strings.TrimSpace(strings.TrimSuffix(text, ":"))), "", nil
	}
	return "", "", fmt.Errorf("expected \"key: value\", got %q", text)
}

func unquote(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			inner := s[1 : len(s)-1]
			if s[0] == '"' {
				if out, err := strconv.Unquote(s); err == nil {
					return out
				}
			}
			return strings.ReplaceAll(inner, "''", "'")
		}
	}
	return s
}

// scalar converts an inline value: flow collections, quoted strings, bools,
// null, integers, floats, or a bare string.
func scalar(s string, num int) (any, error) {
	s = strings.TrimSpace(s)
	switch {
	case s == "" || s == "~" || s == "null" || s == "Null" || s == "NULL":
		return nil, nil
	case strings.HasPrefix(s, "["):
		return parseFlowSeq(s, num)
	case strings.HasPrefix(s, "{"):
		return parseFlowMap(s, num)
	case len(s) >= 2 && (s[0] == '"' || s[0] == '\''):
		return unquote(s), nil
	case s == "true" || s == "True" || s == "TRUE" || s == "yes" || s == "on":
		return true, nil
	case s == "false" || s == "False" || s == "FALSE" || s == "no" || s == "off":
		return false, nil
	}
	if i, err := strconv.ParseInt(s, 0, 64); err == nil {
		return i, nil
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return f, nil
	}
	return s, nil
}

// splitFlow splits the inside of a flow collection on top-level commas.
func splitFlow(body string) []string {
	var out []string
	var quote rune
	depth, start := 0, 0
	for i, r := range body {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			}
		case r == '\'' || r == '"':
			quote = r
		case r == '[' || r == '{':
			depth++
		case r == ']' || r == '}':
			depth--
		case r == ',' && depth == 0:
			out = append(out, body[start:i])
			start = i + 1
		}
	}
	if strings.TrimSpace(body[start:]) != "" {
		out = append(out, body[start:])
	}
	return out
}

func parseFlowSeq(s string, num int) (any, error) {
	if !strings.HasSuffix(s, "]") {
		return nil, fmt.Errorf("yamlite: line %d: unterminated flow sequence", num)
	}
	body := strings.TrimSpace(s[1 : len(s)-1])
	out := []any{}
	if body == "" {
		return out, nil
	}
	for _, item := range splitFlow(body) {
		v, err := scalar(item, num)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

func parseFlowMap(s string, num int) (any, error) {
	if !strings.HasSuffix(s, "}") {
		return nil, fmt.Errorf("yamlite: line %d: unterminated flow mapping", num)
	}
	body := strings.TrimSpace(s[1 : len(s)-1])
	out := map[string]any{}
	if body == "" {
		return out, nil
	}
	for _, item := range splitFlow(body) {
		k, rest, err := tryKey(strings.TrimSpace(item))
		if err != nil {
			return nil, fmt.Errorf("yamlite: line %d: %w", num, err)
		}
		v, err := scalar(rest, num)
		if err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, nil
}
