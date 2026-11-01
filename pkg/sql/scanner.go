// Package sql: scanner turns one COM_QUERY text into tokens. It is the single
// lexical entry point for statement-level classification (design:
// docs/design/statement-classification.md §2).
//
// The scanner deliberately does not build an AST. single/rwsplit modes never
// invoke the TiDB parser; sharding mode uses the AST as a superset on top of
// this token stream.
//
// Lexical assumptions:
//   - backslash escapes are always active inside single/double quoted strings.
//     NO_BACKSLASH_ESCAPES cannot be enabled through the proxy because SET
//     sql_mode is rejected, so the assumption is safe (see design §7).
//   - `--` starts a comment only when followed by whitespace/control or EOF,
//     matching MySQL; `a--1` is two minus operators.
//   - comments are preserved as tokens so hints (/*master*/) stay visible.
package sql

import (
	"fmt"
	"strings"
)

// TokenKind is the lexical class of one token.
type TokenKind uint8

const (
	TokIdent TokenKind = iota
	TokQuotedIdent
	TokString
	TokNumber
	TokUserVar
	TokSystemVar
	TokComment
	TokExecComment
	TokOp
	TokPunct
)

func (k TokenKind) String() string {
	switch k {
	case TokIdent:
		return "ident"
	case TokQuotedIdent:
		return "qident"
	case TokString:
		return "str"
	case TokNumber:
		return "num"
	case TokUserVar:
		return "uvar"
	case TokSystemVar:
		return "svar"
	case TokComment:
		return "comment"
	case TokExecComment:
		return "exec"
	case TokOp:
		return "op"
	case TokPunct:
		return "punct"
	default:
		return fmt.Sprintf("token(%d)", uint8(k))
	}
}

// Token is one lexical unit. Pos is the byte offset of its first byte; End is
// one past its last byte. Text is the raw inner text: quotes/comment delimiters
// are stripped, escapes are not decoded.
type Token struct {
	Kind  TokenKind
	Text  string
	Upper string // uppercased Text for identifiers
	Pos   int
	End   int
}

// Scan tokenizes raw. It returns an error for unterminated strings, quoted
// identifiers, or block comments; callers degrade conservatively (route to
// master and pin the session) rather than rejecting the statement.
func Scan(raw string) ([]Token, error) {
	toks := make([]Token, 0, 16)
	i, n := 0, len(raw)
	for i < n {
		c := raw[i]
		switch {
		case isSpaceByte(c):
			i++

		case c == '#' || (c == '-' && i+1 < n && raw[i+1] == '-' && isLineCommentAt(raw, i)):
			toks = append(toks, scanLineComment(raw, i))
			i = toks[len(toks)-1].End

		case c == '/' && i+1 < n && raw[i+1] == '*':
			tok, err := scanBlockComment(raw, i)
			if err != nil {
				return nil, err
			}
			toks = append(toks, tok)
			i = tok.End

		case c == '\'' || c == '"':
			tok, err := scanQuoted(raw, i, c, TokString)
			if err != nil {
				return nil, err
			}
			toks = append(toks, tok)
			i = tok.End

		case c == '`':
			tok, err := scanQuoted(raw, i, '`', TokQuotedIdent)
			if err != nil {
				return nil, err
			}
			toks = append(toks, tok)
			i = tok.End

		case c == '@':
			tok, err := scanVar(raw, i)
			if err != nil {
				return nil, err
			}
			toks = append(toks, tok)
			i = tok.End

		case isDigitByte(c) || (c == '.' && i+1 < n && isDigitByte(raw[i+1])):
			tok := scanNumber(raw, i)
			toks = append(toks, tok)
			i = tok.End

		case isIdentStartByte(c):
			start := i
			i++
			for i < n && isIdentByte(raw[i]) {
				i++
			}
			text := raw[start:i]
			toks = append(toks, Token{
				Kind: TokIdent, Text: text, Upper: strings.ToUpper(text), Pos: start, End: i,
			})

		case isOpByte(c):
			start := i
			i = scanOpLen(raw, i)
			toks = append(toks, Token{Kind: TokOp, Text: raw[start:i], Pos: start, End: i})

		case isPunctByte(c):
			toks = append(toks, Token{Kind: TokPunct, Text: raw[i : i+1], Pos: i, End: i + 1})
			i++

		default:
			return nil, fmt.Errorf("sql: unexpected byte %q at %d", c, i)
		}
	}
	return toks, nil
}

// isLineCommentAt reports whether the `--` at i starts a comment (MySQL needs
// whitespace or a control character after it).
func isLineCommentAt(raw string, i int) bool {
	j := i + 2
	if j >= len(raw) {
		return true
	}
	return isSpaceByte(raw[j]) || raw[j] < 0x20
}

func scanLineComment(raw string, start int) Token {
	i := start + 1
	if raw[start] == '-' {
		i = start + 2
	}
	body := i
	for i < len(raw) && raw[i] != '\n' {
		i++
	}
	return Token{Kind: TokComment, Text: raw[body:i], Pos: start, End: i}
}

func scanBlockComment(raw string, start int) (Token, error) {
	i := start + 2
	exec := false
	if i < len(raw) && raw[i] == '!' {
		exec = true
		i++
	}
	body := i
	for i+1 < len(raw) && !(raw[i] == '*' && raw[i+1] == '/') {
		i++
	}
	if i+1 >= len(raw) {
		return Token{}, fmt.Errorf("sql: unterminated comment at byte %d", start)
	}
	end := i + 2
	kind := TokComment
	if exec {
		kind = TokExecComment
	}
	return Token{Kind: kind, Text: raw[body:i], Pos: start, End: end}, nil
}

// scanQuoted scans '...', "..." or `...`. A doubled quote inside the body is an
// escape; backslash escapes apply to strings only.
func scanQuoted(raw string, start int, quote byte, kind TokenKind) (Token, error) {
	i := start + 1
	body := i
	for i < len(raw) {
		if quote != '`' && raw[i] == '\\' && i+1 < len(raw) {
			i += 2
			continue
		}
		if raw[i] == quote {
			if i+1 < len(raw) && raw[i+1] == quote {
				i += 2
				continue
			}
			i++
			return Token{Kind: kind, Text: raw[body : i-1], Pos: start, End: i}, nil
		}
		i++
	}
	return Token{}, fmt.Errorf("sql: unterminated quoted token at byte %d", start)
}

func scanVar(raw string, start int) (Token, error) {
	i := start + 1
	if i < len(raw) && raw[i] == '@' {
		i++
		name := i
		for i < len(raw) && (isIdentByte(raw[i]) || raw[i] == '.') {
			i++
		}
		if name == i {
			return Token{}, fmt.Errorf("sql: empty system variable at byte %d", start)
		}
		return Token{Kind: TokSystemVar, Text: raw[name:i], Pos: start, End: i}, nil
	}
	if i < len(raw) && (raw[i] == '\'' || raw[i] == '"' || raw[i] == '`') {
		quote := raw[i]
		i++
		body := i
		for i < len(raw) {
			if quote != '`' && raw[i] == '\\' && i+1 < len(raw) {
				i += 2
				continue
			}
			if raw[i] == quote {
				if i+1 < len(raw) && raw[i+1] == quote {
					i += 2
					continue
				}
				i++
				return Token{Kind: TokUserVar, Text: raw[body : i-1], Pos: start, End: i}, nil
			}
			i++
		}
		return Token{}, fmt.Errorf("sql: unterminated user variable at byte %d", start)
	}
	name := i
	for i < len(raw) && (isIdentByte(raw[i]) || raw[i] == '.') {
		i++
	}
	if name == i {
		return Token{}, fmt.Errorf("sql: empty user variable at byte %d", start)
	}
	return Token{Kind: TokUserVar, Text: raw[name:i], Pos: start, End: i}, nil
}

func scanNumber(raw string, start int) Token {
	i := start
	n := len(raw)
	if raw[i] == '.' {
		i++
		for i < n && isDigitByte(raw[i]) {
			i++
		}
	} else if raw[i] == '0' && i+1 < n && (raw[i+1] == 'x' || raw[i+1] == 'X') {
		i += 2
		for i < n && isHexByte(raw[i]) {
			i++
		}
	} else {
		for i < n && isDigitByte(raw[i]) {
			i++
		}
		if i < n && raw[i] == '.' {
			i++
			for i < n && isDigitByte(raw[i]) {
				i++
			}
		}
		if i < n && (raw[i] == 'e' || raw[i] == 'E') {
			j := i + 1
			if j < n && (raw[j] == '+' || raw[j] == '-') {
				j++
			}
			if j < n && isDigitByte(raw[j]) {
				i = j
				for i < n && isDigitByte(raw[i]) {
					i++
				}
			}
		}
	}
	return Token{Kind: TokNumber, Text: raw[start:i], Pos: start, End: i}
}

// scanOpLen returns the end offset of the operator starting at i.
func scanOpLen(raw string, i int) int {
	three := []string{"<=>", "->>"}
	for _, op := range three {
		if strings.HasPrefix(raw[i:], op) {
			return i + 3
		}
	}
	two := []string{"<=", ">=", "<>", "!=", ":=", "||", "&&", "<<", ">>", "->"}
	for _, op := range two {
		if strings.HasPrefix(raw[i:], op) {
			return i + 2
		}
	}
	return i + 1
}

func isSpaceByte(c byte) bool {
	switch c {
	case ' ', '\t', '\n', '\r', '\f', '\v':
		return true
	default:
		return false
	}
}

func isDigitByte(c byte) bool { return c >= '0' && c <= '9' }

func isHexByte(c byte) bool {
	return isDigitByte(c) || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

func isIdentStartByte(c byte) bool {
	return c == '_' || c == '$' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c >= 0x80
}

func isIdentByte(c byte) bool {
	return isIdentStartByte(c) || isDigitByte(c)
}

func isOpByte(c byte) bool {
	switch c {
	case '=', '<', '>', '!', '+', '-', '*', '/', '%', '&', '|', '^', '~', ':':
		return true
	default:
		return false
	}
}

func isPunctByte(c byte) bool {
	switch c {
	case '(', ')', ',', '.', ';', '?':
		return true
	default:
		return false
	}
}
