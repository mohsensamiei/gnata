package gnata_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/recolabs/gnata"
)

// errorPositionCase is one jsonata-js 2.2.2 error, as
// scripts/error-positions/generate.js records it.
type errorPositionCase struct {
	Expr     string          `json:"expr"`
	Mutation bool            `json:"mutation"`
	Dataset  string          `json:"dataset"`
	Data     json.RawMessage `json:"data"`
	Bindings map[string]any  `json:"bindings"`
	Code     string          `json:"code"`
	Token    string          `json:"token"`
	Position int             `json:"position"`
}

// errorPositionDivergences are the cases where gnata reports a different
// error than jsonata-js for a reason other than its position: gnata raises
// D1001 for an overflowing product at its *, where jsonata-js only fails on
// the infinite operand of the / using it.
var errorPositionDivergences = map[string]bool{
	"1/(10e300 * 10e100) ": true,
	"(\"نام\u200cخانوادگی 🙂\"; 1/(10e300 * 10e100) )":        true,
	"($کالا\u200cها := \"۱۲۳\u200c\"; 1/(10e300 * 10e100) )": true,
	"(/* توضیح\u200c 😀 */ 1/(10e300 * 10e100) )":             true,
}

// TestErrorPositions compares the code, token and position of gnata's
// errors with those of jsonata-js 2.2.2 (see
// scripts/error-positions/generate.js). A mutation case, a syntax error made
// by cutting a suite expression short or deleting a character, is compared
// only when gnata raises the same error code and jsonata-js does not reject
// a .. outside [ ], which gnata accepts: where its grammar is more lenient,
// it reports a later error.
func TestErrorPositions(t *testing.T) {
	raw, err := os.ReadFile("testdata/error_positions.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []errorPositionCase
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	otherCode := 0
	for _, tc := range cases {
		if errorPositionDivergences[tc.Expr] {
			continue
		}
		got := errorPosition(t, &tc)
		if tc.Mutation && (got.Code != tc.Code || tc.Token == "..") {
			otherCode++
			continue
		}
		if got.Code != tc.Code || got.Token != tc.Token || got.Position != tc.Position {
			t.Errorf("%q: got code %q token %q position %d, jsonata-js gives code %q token %q position %d",
				tc.Expr, got.Code, got.Token, got.Position, tc.Code, tc.Token, tc.Position)
		}
	}
	t.Logf("%d cases; %d mutation cases with another error", len(cases), otherCode)
}

// errorPosition compiles and evaluates tc, returning the error gnata
// reports: a zero error when it reports none, and one with only a Message
// when it is not a *gnata.Error.
func errorPosition(t *testing.T, tc *errorPositionCase) gnata.Error {
	t.Helper()
	var input any
	switch {
	case tc.Dataset != "":
		ds, err := getDataset(tc.Dataset)
		if err != nil {
			t.Fatal(err)
		}
		input = ds
	case len(tc.Data) > 0:
		v, err := gnata.DecodeJSON(tc.Data)
		if err != nil {
			t.Fatal(err)
		}
		input = v
	}
	expr, err := gnata.Compile(tc.Expr)
	if err == nil && !tc.Mutation {
		_, err = expr.EvalWithVars(context.Background(), input, tc.Bindings)
	}
	var e *gnata.Error
	switch {
	case errors.As(err, &e):
		return *e
	case err != nil:
		return gnata.Error{Message: err.Error()}
	}
	return gnata.Error{}
}

func TestErrorPositionsAPI(t *testing.T) {
	for _, tc := range []struct {
		expr     string
		code     string
		position int
	}{
		{"result.(", "S0203", 8},
		{"$value >", "S0207", 8},
		{`$number("abc")`, "D3030", 8},
		// Persian text with a zero-width non-joiner and an astral emoji
		// before the error: positions count UTF-16 code units.
		{"\"نام\u200cخانوادگی 🙂\" & $number(\"x\")", "D3030", 28},
	} {
		expr, err := gnata.Compile(tc.expr)
		if err == nil {
			_, err = expr.Eval(context.Background(), nil)
		}
		var e *gnata.Error
		if !errors.As(err, &e) {
			t.Fatalf("%q: error %v is not a *gnata.Error", tc.expr, err)
		}
		if e.Code != tc.code || e.Position != tc.position {
			t.Errorf("%q: got %s at %d, want %s at %d", tc.expr, e.Code, e.Position, tc.code, tc.position)
		}
	}
	// The message of a syntax error is unchanged.
	if _, err := gnata.Compile("result.("); !strings.Contains(err.Error(), `at token "EOF"`) {
		t.Errorf("syntax error message changed: %v", err)
	}
}

// TestCompileRejectsInvalidRegex checks that Compile rejects a regex literal
// Go's RE2 cannot compile (a lookaround or a backreference, which jsonata-js
// accepts, or a bad repeat), with S0303 at the end of the literal, and never
// later at evaluation.
func TestCompileRejectsInvalidRegex(t *testing.T) {
	for _, tc := range []struct {
		expr     string
		position int
	}{
		{`$match(name, /a(?=b)/)`, 21},
		{`$replace(name, /(a)\1/, "x")`, 22},
		{`$contains(name, /(?<=a)b/i)`, 26},
		{`$match(name, /a{2,1}/)`, 21},
		// Positions count UTF-16 code units, as the other errors' do.
		{"(\"نام\u200cخانوادگی 🙂\"; $match(name, /a(?!b)/))", 41},
	} {
		_, err := gnata.Compile(tc.expr)
		var e *gnata.Error
		if !errors.As(err, &e) {
			t.Fatalf("%q: Compile gave %v, want a *gnata.Error", tc.expr, err)
		}
		if e.Code != "S0303" || e.Position != tc.position {
			t.Errorf("%q: got %s at %d, want S0303 at %d", tc.expr, e.Code, e.Position, tc.position)
		}
	}
	for _, expr := range []string{`$match(name, /a(b)c/i)`, `$split(name, /[,;]\s*/)`, `/[)]/`, `/(a})/`} {
		if _, err := gnata.Compile(expr); err != nil {
			t.Errorf("%q: Compile gave %v, want no error", expr, err)
		}
	}
}
