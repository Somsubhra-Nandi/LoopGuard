package shim

import (
	"errors"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// decodeReply decodes the daemon's /v1/events reply,
// {"stdout": "<hook output>", "exit": <code>}, without encoding/json (see
// the note in shim.go). Member names match case-insensitively and other
// members are skipped, as with encoding/json; a missing or null member
// leaves its zero value. Anything else, including a non-integer exit,
// is an error.
func decodeReply(b []byte) (stdout string, exit int, err error) {
	p := parser{s: string(b)}
	defer func() {
		if r := recover(); r != nil {
			stdout, exit, err = "", 0, errBadJSON
		}
	}()
	p.ws()
	p.expect('{')
	p.ws()
	if p.peek() == '}' {
		p.i++
		return "", 0, nil
	}
	for {
		p.ws()
		key := p.str()
		p.ws()
		p.expect(':')
		p.ws()
		switch {
		case p.null():
		case strings.EqualFold(key, "stdout"):
			stdout = p.str()
		case strings.EqualFold(key, "exit"):
			exit = p.integer()
		default:
			p.skip(0)
		}
		p.ws()
		if p.peek() == '}' {
			p.i++
			return stdout, exit, nil
		}
		p.expect(',')
	}
}

var errBadJSON = errors.New("malformed reply")

// parser panics on malformed input; decodeReply recovers.
type parser struct {
	s string
	i int
}

func (p *parser) fail() { panic(errBadJSON) }

func (p *parser) peek() byte {
	if p.i >= len(p.s) {
		p.fail()
	}
	return p.s[p.i]
}

func (p *parser) expect(c byte) {
	if p.peek() != c {
		p.fail()
	}
	p.i++
}

func (p *parser) ws() {
	for p.i < len(p.s) && (p.s[p.i] == ' ' || p.s[p.i] == '\t' || p.s[p.i] == '\n' || p.s[p.i] == '\r') {
		p.i++
	}
}

func (p *parser) literal(lit string) bool {
	if strings.HasPrefix(p.s[p.i:], lit) {
		p.i += len(lit)
		return true
	}
	return false
}

func (p *parser) null() bool { return p.literal("null") }

// str parses a string, replacing invalid UTF-8 and lone surrogates with
// U+FFFD, as encoding/json does.
func (p *parser) str() string {
	p.expect('"')
	var b strings.Builder
	for {
		c := p.peek()
		switch {
		case c == '"':
			p.i++
			return b.String()
		case c < 0x20:
			p.fail()
		case c == '\\':
			p.i++
			e := p.peek()
			p.i++
			switch e {
			case '"', '\\', '/':
				b.WriteByte(e)
			case 'b':
				b.WriteByte('\b')
			case 'f':
				b.WriteByte('\f')
			case 'n':
				b.WriteByte('\n')
			case 'r':
				b.WriteByte('\r')
			case 't':
				b.WriteByte('\t')
			case 'u':
				r := p.hex4()
				if utf16.IsSurrogate(r) {
					r2 := rune(-1)
					if strings.HasPrefix(p.s[p.i:], `\u`) {
						save := p.i
						p.i += 2
						r2 = p.hex4()
						if d := utf16.DecodeRune(r, r2); d != utf8.RuneError {
							b.WriteRune(d)
							continue
						}
						p.i = save
					}
					r = utf8.RuneError
				}
				b.WriteRune(r)
			default:
				p.fail()
			}
		case c < utf8.RuneSelf:
			b.WriteByte(c)
			p.i++
		default:
			r, n := utf8.DecodeRuneInString(p.s[p.i:])
			b.WriteRune(r) // RuneError for invalid bytes
			p.i += n
		}
	}
}

func (p *parser) hex4() rune {
	if p.i+4 > len(p.s) {
		p.fail()
	}
	v, err := strconv.ParseUint(p.s[p.i:p.i+4], 16, 16)
	if err != nil || strings.ContainsAny(p.s[p.i:p.i+4], "+-") {
		p.fail()
	}
	p.i += 4
	return rune(v)
}

// integer parses a JSON number that must be an integer that fits an int.
func (p *parser) integer() int {
	start := p.i
	if p.peek() == '-' {
		p.i++
	}
	digits := p.i
	for p.i < len(p.s) && p.s[p.i] >= '0' && p.s[p.i] <= '9' {
		p.i++
	}
	if p.i == digits || (p.s[digits] == '0' && p.i-digits > 1) {
		p.fail()
	}
	if p.i < len(p.s) && (p.s[p.i] == '.' || p.s[p.i] == 'e' || p.s[p.i] == 'E') {
		p.fail()
	}
	n, err := strconv.Atoi(p.s[start:p.i])
	if err != nil {
		p.fail()
	}
	return n
}

// skip skips any JSON value, nested at most 32 deep.
func (p *parser) skip(depth int) {
	if depth > 32 {
		p.fail()
	}
	switch c := p.peek(); {
	case c == '"':
		p.str()
	case c == '{' || c == '[':
		end := byte('}')
		if c == '[' {
			end = ']'
		}
		p.i++
		p.ws()
		if p.peek() == end {
			p.i++
			return
		}
		for {
			p.ws()
			if c == '{' {
				p.str()
				p.ws()
				p.expect(':')
				p.ws()
			}
			p.skip(depth + 1)
			p.ws()
			if p.peek() == end {
				p.i++
				return
			}
			p.expect(',')
		}
	case p.literal("true"), p.literal("false"), p.literal("null"):
	case c == '-' || (c >= '0' && c <= '9'):
		p.number()
	default:
		p.fail()
	}
}

// number skips a JSON number.
func (p *parser) number() {
	digits := func() int {
		n := 0
		for p.i < len(p.s) && p.s[p.i] >= '0' && p.s[p.i] <= '9' {
			p.i++
			n++
		}
		return n
	}
	if p.peek() == '-' {
		p.i++
	}
	intStart := p.i
	if n := digits(); n == 0 || (p.s[intStart] == '0' && n > 1) {
		p.fail()
	}
	if p.i < len(p.s) && p.s[p.i] == '.' {
		p.i++
		if digits() == 0 {
			p.fail()
		}
	}
	if p.i < len(p.s) && (p.s[p.i] == 'e' || p.s[p.i] == 'E') {
		p.i++
		if p.i < len(p.s) && (p.s[p.i] == '+' || p.s[p.i] == '-') {
			p.i++
		}
		if digits() == 0 {
			p.fail()
		}
	}
}
