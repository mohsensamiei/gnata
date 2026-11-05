// Package errs defines the structured error that every stage of gnata
// returns: the lexer, the parser and the evaluator.
package errs

import "unicode/utf8"

// Error is a JSONata error. Code is the JSONata error code (S0xxx, T0xxx,
// T1xxx, T2xxx, D1xxx, D2xxx, D3xxx).
//
// Token and Position are those of the error in jsonata-js: Token is the
// token or name the error is about, and Position is a character index into
// the expression, counted in UTF-16 code units as JavaScript counts string
// length. For most errors it is the index just past the token jsonata-js
// reports. Position is 0 when the error has no position, as jsonata-js
// leaves it unset, and for an error at the very start of the expression.
type Error struct {
	Code     string
	Token    string
	Value    any
	Message  string
	Position int

	// text, when set, is the whole Error() string, kept for syntax errors
	// whose text predates this type.
	text string
	// utf16 reports that Position counts UTF-16 code units; until then it is
	// a byte offset into the expression (see InUTF16).
	utf16 bool
}

func (e *Error) Error() string {
	if e.text != "" {
		return e.text
	}
	if e.Message != "" && e.Code != "" {
		return e.Code + ": " + e.Message
	}
	if e.Message != "" {
		return e.Message
	}
	return e.Code
}

// Syntax returns a syntax error at byte offset pos of the expression, whose
// Error() string is text.
func Syntax(code, token string, pos int, msg, text string) *Error {
	return &Error{Code: code, Token: token, Position: pos, Message: msg, text: text}
}

// At returns err placed at byte offset pos of the expression being
// evaluated, as jsonata-js attaches a position to an error passing through
// an operator or a call: tok becomes its Token if it has none, or always
// when setToken is set. A pos of 0, and any error other than a *Error, leave
// err as it is. The error is copied, never changed in place.
func At(err error, pos int, tok string, setToken bool) error {
	e, ok := err.(*Error) //nolint:errorlint // only gnata's own unwrapped errors are placed
	if !ok || pos == 0 {
		return err
	}
	c := *e
	c.Position, c.utf16 = pos, false
	if tok != "" && (setToken || c.Token == "") {
		c.Token = tok
	}
	return &c
}

// InUTF16 returns err with its byte-offset Position into src converted to a
// UTF-16 index. A *Error that is already converted, and any other error, is
// returned as it is. The error is copied, never changed in place, as some
// errors are shared values.
func InUTF16(src string, err error) error {
	e, ok := err.(*Error) //nolint:errorlint // only gnata's own unwrapped errors carry a byte offset
	if !ok || e.utf16 {
		return err
	}
	c := *e
	c.utf16 = true
	c.Position = UTF16Index(src, e.Position)
	return &c
}

// UTF16Index converts byte offset off of src to the number of UTF-16 code
// units before it. An offset past the end counts the excess as single
// units, as jsonata-js can report a position past the end of the source.
func UTF16Index(src string, off int) int {
	if off <= 0 {
		return off
	}
	n, i := 0, 0
	for i < len(src) && i < off {
		r, size := utf8.DecodeRuneInString(src[i:])
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
		i += size
	}
	return n + max(off-len(src), 0)
}
