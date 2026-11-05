package parser

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/recolabs/gnata/internal/errs"
	"github.com/recolabs/gnata/internal/lexer"
)

// bindingPower returns the left-denotation binding power for a token type.
func bindingPower(tt lexer.TokenType) int {
	switch tt { //nolint:exhaustive // only tokens with non-zero binding power
	case lexer.TokenLParen, lexer.TokenLBracket:
		return 80
	case lexer.TokenAt, lexer.TokenHash:
		return 75
	case lexer.TokenDot:
		return 75
	// TokenSemicolon and TokenColon are separators consumed explicitly by their
	// respective NUD handlers; they must not act as binary infix operators.
	case lexer.TokenSemicolon, lexer.TokenColon:
		return 0
	case lexer.TokenStarStar, lexer.TokenStar, lexer.TokenSlash, lexer.TokenPercent:
		return 60
	case lexer.TokenPlus, lexer.TokenMinus, lexer.TokenAmp:
		return 50
	case lexer.TokenNE, lexer.TokenLE, lexer.TokenGE,
		lexer.TokenEquals, lexer.TokenLT, lexer.TokenGT,
		lexer.TokenIn:
		return 40
	case lexer.TokenAnd:
		return 30
	case lexer.TokenOr:
		return 25
	case lexer.TokenDotDot, lexer.TokenQuestion,
		lexer.TokenElvis, lexer.TokenCoalesce:
		return 20
	case lexer.TokenChain:
		return 45 // Higher than comparison ops (40) so "A ~> f() = B" → "(A ~> f()) = B"
	case lexer.TokenAssign:
		return 10
	case lexer.TokenLBrace:
		return 70
	case lexer.TokenCaret:
		return 40
	default:
		return 0
	}
}

// parseError returns syntax error code about token tok, at byte offset pos
// just past the token or node jsonata-js reports it at.
func parseError(code, tok string, pos int, msg string) error {
	return syntaxError(code, tok, tok, pos, msg)
}

// syntaxError is parseError for an error whose jsonata-js token, jsTok,
// differs from the token tok its message has always named.
func syntaxError(code, jsTok, tok string, pos int, msg string) error {
	return errs.Syntax(code, jsTok, pos, msg, fmt.Sprintf("JSONata error %s at token %q: %s", code, tok, msg))
}

// jsPos is the position jsonata-js gives the parsed node n. Its dots nest
// to the left, so a.b.c is at its last dot; here they nest to the right.
func jsPos(n *Node) int {
	for n.Type == NodeBinary && n.Value == "." && n.Right != nil && n.Right.Type == NodeBinary && n.Right.Value == "." {
		n = n.Right
	}
	return n.Pos
}

// tokenText is a token's value as jsonata-js reports it in an error: the
// name of a name or variable, a string's content, and a number, regex or
// value literal as JavaScript converts it to a string. The null literal is
// not reported.
func tokenText(tok *lexer.Token) string {
	switch tok.Type { //nolint:exhaustive // other tokens report their value
	case lexer.TokenEOF:
		return "(end)"
	case lexer.TokenNumber:
		return jsNumberString(tok.NumVal)
	case lexer.TokenRegex:
		return "/" + tok.RegexPat + "/" + tok.RegexFlg
	case lexer.TokenValue:
		if tok.IsNull {
			return ""
		}
	}
	return tok.Value
}

// jsNumberString formats f as JavaScript's String(f) does.
func jsNumberString(f float64) string {
	if a := math.Abs(f); a == 0 || a >= 1e-7 && a < 1e21 {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	s := strconv.FormatFloat(f, 'e', -1, 64)
	mant, exp, _ := strings.Cut(s, "e")
	sign := exp[:1]
	exp = strings.TrimLeft(exp[1:], "0")
	return mant + "e" + sign + exp
}

// MaxDepth is the deepest expression nesting the parser accepts. Each nested
// expression and each chained operator is one level. It keeps the parser and
// the recursive passes over the tree (processing, analysis, evaluation) far
// from Go's fatal stack limit, which deep enough input would otherwise reach.
// jsonata-js itself overflows its stack at about 2,000 levels of nesting.
const MaxDepth = 10_000

// Parser is a top-down operator precedence (Pratt) parser for JSONata.
type Parser struct {
	lex     *lexer.Lexer
	token   lexer.Token
	src     string
	initErr error   // error from initial lexer prime
	slots   []*Slot // the slots of the % operators parsed so far, in order
	depth   int     // active expression calls
	height  int     // tallest subtree finished in the current expression call
	// functions reports that a lambda or transform was parsed, whose depth
	// markFunctionDepths records.
	functions bool
	// deferred is the first error jsonata-js reports only after the whole
	// expression has parsed, such as S0207 for a missing operand, so any
	// parse error later in the source takes precedence over it.
	deferred error
}

// NewParser creates a new Parser for the given source string.
func NewParser(src string) *Parser {
	p := &Parser{
		lex: lexer.NewLexer(src),
		src: src,
	}
	// Prime the lookahead.
	p.initErr = p.advance()
	return p
}

// The lexer reads '/' as division in infix position and as a regex in prefix
// position, and jsonata-js picks the position per token rather than from the
// grammar. The token after the first token of an expression is read in infix
// position, whether that first token is an operand or an opener ([, {, (, -
// or |), so [/a/] raises S0211. The token after an operator, a separator, the
// [ or ( of a subscript, call, lambda or sort, or the { of a group is read in
// prefix position. Among closers, the ] of an array or predicate, the } of an
// object or group and the ) of a block, call or lambda parameter list are
// followed by infix position; an empty [], a sort's ), a lambda body's } and
// a transform's closing | by prefix position.

// advance reads the next token in prefix position.
func (p *Parser) advance() error {
	return p.next(false)
}

// advanceInfix reads the next token in infix position.
func (p *Parser) advanceInfix() error {
	return p.next(true)
}

func (p *Parser) next(infix bool) error {
	tok, err := p.lex.Next(infix)
	if err != nil {
		return err
	}
	// jsonata-js lexes a lone ! or ~ as an operator it has no symbol for,
	// and rejects it as soon as it is read.
	if tok.Type == lexer.TokenBang || tok.Type == lexer.TokenTilde {
		return parseError("S0204", tok.Value, tok.End, "unknown operator")
	}
	p.token = tok
	return nil
}

func (p *Parser) expect(tt lexer.TokenType) error {
	if p.token.Type == lexer.TokenEOF && tt != lexer.TokenEOF {
		return p.endError()
	}
	if p.token.Type != tt {
		return syntaxError("S0202", tokenText(&p.token), p.token.Value, p.token.End,
			fmt.Sprintf("expected token %d, got %d (%q)", tt, p.token.Type, p.token.Value))
	}
	return nil
}

// consume advances past a token that must have the given type, reading the
// next token in prefix position.
func (p *Parser) consume(tt lexer.TokenType) error {
	if err := p.expect(tt); err != nil {
		return err
	}
	return p.advance()
}

// consumeInfix advances past a token that must have the given type, reading
// the next token in infix position.
func (p *Parser) consumeInfix(tt lexer.TokenType) error {
	if err := p.expect(tt); err != nil {
		return err
	}
	return p.advanceInfix()
}

// Parse parses the full expression and returns the root AST node.
func (p *Parser) Parse() (*Node, error) {
	if p.initErr != nil {
		return nil, p.initErr
	}
	node, err := p.expression(0)
	if err != nil {
		return nil, err
	}
	if p.token.Type != lexer.TokenEOF {
		return nil, syntaxError("S0201", tokenText(&p.token), p.token.Value, p.token.End, "unexpected token")
	}
	if p.deferred != nil {
		return nil, p.deferred
	}
	return node, nil
}

// endError reports a token that was required where the expression ended.
func (p *Parser) endError() error {
	return syntaxError("S0203", "(end)", "EOF", len(p.src), "expected a token before end of expression")
}

// deferError records err to report once the expression has parsed; the first
// recorded error wins.
func (p *Parser) deferError(err error) {
	if p.deferred == nil {
		p.deferred = err
	}
}

// expression is the core Pratt parsing function. It also tracks the height of
// the subtree it builds, so a chain like 1+1+...+1, which the loop below
// builds without recursing, counts toward MaxDepth like nesting does.
func (p *Parser) expression(bp int) (*Node, error) {
	if p.depth == MaxDepth {
		return nil, p.depthError()
	}
	p.depth++
	defer func() { p.depth-- }()
	outer := p.height
	p.height = 0
	left, err := p.nud()
	if err != nil {
		return nil, err
	}
	height := p.height + 1
	for {
		if height > MaxDepth {
			return nil, p.depthError()
		}
		if bindingPower(p.token.Type) <= bp {
			break
		}
		p.height = 0
		left, err = p.led(left)
		if err != nil {
			return nil, err
		}
		height = max(height, p.height) + 1
	}
	p.height = max(outer, height)
	return left, nil
}

func (p *Parser) depthError() error {
	return parseError("S0218", p.token.Value, p.token.End,
		fmt.Sprintf("expression nesting exceeds the maximum depth of %d", MaxDepth))
}

// nud is the null denotation (prefix handler).
func (p *Parser) nud() (*Node, error) { //nolint:gocyclo,funlen // dispatch
	tok := p.token

	switch tok.Type { //nolint:exhaustive // prefix tokens only
	case lexer.TokenEOF:
		// An operand is missing, as in 1 +. Parsing continues so that a
		// later expected token reports S0203 first, as in jsonata-js.
		p.deferError(syntaxError("S0207", "(end)", "EOF", len(p.src), "unexpected end of expression"))
		return &Node{Type: NodeValue, Value: "null", Pos: tok.End}, nil

	case lexer.TokenName:
		if err := p.advanceInfix(); err != nil {
			return nil, err
		}
		// Special case: lambda keyword — only when followed by '('.
		if (tok.Value == "function" || tok.Value == "λ") && p.token.Type == lexer.TokenLParen {
			return p.parseLambda()
		}
		return &Node{Type: NodeName, Value: tok.Value, Pos: tok.End}, nil

	case lexer.TokenAnd, lexer.TokenOr, lexer.TokenIn:
		// "and" / "or" / "in" can appear as field names in prefix position.
		if err := p.advanceInfix(); err != nil {
			return nil, err
		}
		return &Node{Type: NodeName, Value: tok.Value, Pos: tok.End}, nil

	case lexer.TokenVariable:
		if err := p.advanceInfix(); err != nil {
			return nil, err
		}
		return &Node{Type: NodeVariable, Value: tok.Value, Pos: tok.End}, nil

	case lexer.TokenString:
		if err := p.advanceInfix(); err != nil {
			return nil, err
		}
		return &Node{Type: NodeString, Value: tok.Value, Pos: tok.End}, nil

	case lexer.TokenNumber:
		if err := p.advanceInfix(); err != nil {
			return nil, err
		}
		return (&Node{Type: NodeNumber, Value: tok.Value, Pos: tok.End}).SetNumVal(tok.NumVal), nil

	case lexer.TokenValue:
		if err := p.advanceInfix(); err != nil {
			return nil, err
		}
		return &Node{Type: NodeValue, Value: tok.Value, Pos: tok.End}, nil

	case lexer.TokenRegex:
		if err := p.advanceInfix(); err != nil {
			return nil, err
		}
		// Store pattern/flags in Value field.
		val := tok.RegexPat
		if tok.RegexFlg != "" {
			val = tok.RegexPat + "/" + tok.RegexFlg
		}
		return &Node{Type: NodeRegex, Value: val, Pos: tok.End}, nil

	case lexer.TokenMinus:
		if err := p.advanceInfix(); err != nil {
			return nil, err
		}
		sub, err := p.expression(70)
		if err != nil {
			return nil, err
		}
		// Fold unary minus into number literal.
		if sub.Type == NodeNumber {
			sub.SetNumVal(-sub.NumVal())
			if v, ok := strings.CutPrefix(sub.Value, "-"); ok {
				sub.Value = v
			} else {
				sub.Value = "-" + sub.Value
			}
			return sub, nil
		}
		return (&Node{Type: NodeUnary, Value: "-", Pos: tok.End}).SetExpression(sub), nil

	case lexer.TokenStar:
		if err := p.advanceInfix(); err != nil {
			return nil, err
		}
		return &Node{Type: NodeWildcard, Value: "*", Pos: tok.End}, nil

	case lexer.TokenStarStar:
		if err := p.advanceInfix(); err != nil {
			return nil, err
		}
		return &Node{Type: NodeDescendant, Value: "**", Pos: tok.End}, nil

	case lexer.TokenPercent:
		if err := p.advanceInfix(); err != nil {
			return nil, err
		}
		slot := &Slot{Label: "!" + strconv.Itoa(len(p.slots)), Level: 1}
		p.slots = append(p.slots, slot)
		return (&Node{Type: NodeParent, Value: "%", Pos: tok.End}).SetSlot(slot), nil

	case lexer.TokenLBracket:
		// Array constructor.
		pos := tok.End
		if err := p.advanceInfix(); err != nil {
			return nil, err
		}
		var exprs []*Node
		for p.token.Type != lexer.TokenRBracket {
			if p.token.Type == lexer.TokenEOF {
				return nil, p.endError()
			}
			expr, err := p.expression(0)
			if err != nil {
				return nil, err
			}
			exprs = append(exprs, expr)
			if p.token.Type == lexer.TokenComma {
				if err := p.advance(); err != nil {
					return nil, err
				}
			} else if err := p.expect(lexer.TokenRBracket); err != nil {
				return nil, err
			}
		}
		if err := p.advanceInfix(); err != nil { // consume ]
			return nil, err
		}
		return (&Node{Type: NodeUnary, Value: "[", Pos: pos}).SetExpressions(exprs), nil

	case lexer.TokenLBrace:
		// Object constructor.
		pos := tok.End
		if err := p.advanceInfix(); err != nil {
			return nil, err
		}
		pairs, err := p.parseObjectPairs()
		if err != nil {
			return nil, err
		}
		if err := p.advanceInfix(); err != nil { // consume }
			return nil, err
		}
		return (&Node{Type: NodeUnary, Value: "{", Pos: pos}).SetLHS(pairs), nil

	case lexer.TokenLParen:
		return p.parseParenOrBlock(tok.End)

	case lexer.TokenPipe:
		// Transform expression: |pattern| update delete? |
		return p.parseTransform(tok.End)

	case lexer.TokenChain:
		return nil, parseError("S0211", "~>", tok.End, "invalid use of ~> as prefix")

	default:
		return nil, syntaxError("S0211", tokenText(&tok), tok.Value, tok.End, "invalid use of token as prefix")
	}
}

// parseLambda parses: function($p1, $p2, ...) { body }
// Called after consuming the "function" or "λ" name token. As in jsonata-js,
// the lambda is at its (.
func (p *Parser) parseLambda() (*Node, error) {
	pos := p.token.End
	if err := p.consume(lexer.TokenLParen); err != nil {
		return nil, err
	}
	// Like jsonata-js, parse each parameter as an expression and require a
	// $variable only once the list has parsed, so function($x/2) is S0208
	// but function($x/) reports the parse error. #, @ and a group make the
	// parameter an expression in jsonata-js; a trailing [] does not.
	var params []*Node
	for p.token.Type != lexer.TokenRParen {
		if p.token.Type == lexer.TokenEOF {
			return nil, p.endError()
		}
		param, err := p.parseArgument()
		if err != nil {
			return nil, err
		}
		params = append(params, param)
		if p.token.Type == lexer.TokenComma {
			if err := p.advance(); err != nil {
				return nil, err
			}
		} else if err := p.expect(lexer.TokenRParen); err != nil {
			return nil, err
		}
	}
	if err := p.advanceInfix(); err != nil { // consume )
		return nil, err
	}
	for i, param := range params {
		if param.Type != NodeVariable || param.Index() != "" || param.Focus() != "" || param.Group() != nil {
			err := errs.Syntax("S0208", param.Value, jsPos(param), "expected $parameter name in lambda",
				fmt.Sprintf("JSONata error S0208 at token %q: expected $parameter name in lambda", param.Value))
			err.Value = i + 1
			return nil, err
		}
	}

	// Optional signature: <sig>
	var sig *Signature
	if p.token.Type == lexer.TokenLT {
		sigPos := p.token.End
		sigStr, err := p.parseSignatureString()
		if err != nil {
			return nil, err
		}
		params, err := ParseSig(sigStr)
		if err != nil {
			// jsonata-js places a signature error at the < plus its offset
			// in the signature, < included.
			if e, ok := err.(*errs.Error); ok { //nolint:errorlint // ParseSig returns it unwrapped
				e.Position += sigPos + 1
			}
			return nil, err
		}
		sig = &Signature{Params: params}
	}

	if err := p.consume(lexer.TokenLBrace); err != nil {
		return nil, err
	}
	body, err := p.expression(0)
	if err != nil {
		return nil, err
	}
	if err := p.consume(lexer.TokenRBrace); err != nil {
		return nil, err
	}
	p.functions = true
	return (&Node{Type: NodeLambda, Pos: pos}).SetArguments(params).SetBody(body).SetSignature(sig), nil
}

// parseSignatureString reads tokens until the matching >.
func (p *Parser) parseSignatureString() (string, error) {
	if err := p.advance(); err != nil { // consume <
		return "", err
	}
	var sb strings.Builder
	depth := 1
	for depth > 0 {
		// As in jsonata-js, a signature ends at the first { or the end of the
		// input, which then report the missing > as S0202 or S0203.
		if p.token.Type == lexer.TokenEOF || p.token.Type == lexer.TokenLBrace {
			return "", p.expect(lexer.TokenGT)
		}
		if p.token.Type == lexer.TokenLT {
			depth++
		} else if p.token.Type == lexer.TokenGT {
			depth--
			if depth == 0 {
				if err := p.advance(); err != nil { // consume >
					return "", err
				}
				break
			}
		}
		sb.WriteString(p.token.Value)
		if err := p.advance(); err != nil {
			return "", err
		}
	}
	return sb.String(), nil
}

// parseParenOrBlock parses ( expr ) or ( expr; ... ).
func (p *Parser) parseParenOrBlock(pos int) (*Node, error) {
	if err := p.advanceInfix(); err != nil {
		return nil, err
	}
	if p.token.Type == lexer.TokenRParen {
		// Empty parens () — treat as empty block.
		if err := p.advanceInfix(); err != nil {
			return nil, err
		}
		return (&Node{Type: NodeBlock, Pos: pos}).SetExpressions(nil), nil
	}
	first, err := p.expression(0)
	if err != nil {
		return nil, err
	}
	if p.token.Type == lexer.TokenRParen {
		// Single expression in parens — still create a NodeBlock for proper lexical scoping.
		// Each (...) creates its own scope so that bindings inside do not escape.
		if err := p.advanceInfix(); err != nil {
			return nil, err
		}
		return (&Node{Type: NodeBlock, Pos: pos}).SetExpressions([]*Node{first}), nil
	}
	// Block: multiple semicolon-separated expressions.
	exprs := []*Node{first}
	for p.token.Type == lexer.TokenSemicolon {
		if err := p.advance(); err != nil {
			return nil, err
		}
		if p.token.Type == lexer.TokenRParen {
			break
		}
		expr, err := p.expression(0)
		if err != nil {
			return nil, err
		}
		exprs = append(exprs, expr)
	}
	if err := p.consumeInfix(lexer.TokenRParen); err != nil {
		return nil, err
	}
	return (&Node{Type: NodeBlock, Pos: pos}).SetExpressions(exprs), nil
}

// parseObjectPairs parses key: value pairs separated by commas.
// Caller has already consumed the opening {. Stops at }.
func (p *Parser) parseObjectPairs() ([]*Node, error) {
	var pairs []*Node
	for p.token.Type != lexer.TokenRBrace {
		if p.token.Type == lexer.TokenEOF {
			return nil, p.endError()
		}
		key, err := p.expression(0)
		if err != nil {
			return nil, err
		}
		if err := p.consume(lexer.TokenColon); err != nil {
			return nil, err
		}
		val, err := p.expression(0)
		if err != nil {
			return nil, err
		}
		pairs = append(pairs, key, val)
		if p.token.Type == lexer.TokenComma {
			if err := p.advance(); err != nil {
				return nil, err
			}
		} else if err := p.expect(lexer.TokenRBrace); err != nil {
			return nil, err
		}
	}
	return pairs, nil
}

// parseTransform parses the | pattern | update [, delete] | transform expression.
// | has no infix meaning, so each part is a full expression that ends at it.
func (p *Parser) parseTransform(pos int) (*Node, error) {
	if err := p.advanceInfix(); err != nil {
		return nil, err
	}
	pattern, err := p.expression(0)
	if err != nil {
		return nil, err
	}
	if err := p.consume(lexer.TokenPipe); err != nil {
		return nil, err
	}
	update, err := p.expression(0)
	if err != nil {
		return nil, err
	}
	var del *Node
	if p.token.Type == lexer.TokenComma {
		if err := p.advance(); err != nil {
			return nil, err
		}
		del, err = p.expression(0)
		if err != nil {
			return nil, err
		}
	}
	if err := p.consume(lexer.TokenPipe); err != nil {
		return nil, err
	}
	p.functions = true
	return (&Node{Type: NodeTransform, Pos: pos}).SetPattern(pattern).SetUpdate(update).SetDelete(del), nil
}

// led is the left denotation (infix handler).
func (p *Parser) led(left *Node) (*Node, error) { //nolint:gocyclo,funlen // dispatch
	tok := p.token
	bp := bindingPower(tok.Type)

	switch tok.Type { //nolint:exhaustive // infix tokens only
	case lexer.TokenLParen:
		// Function call.
		return p.parseFunctionCall(left, tok.End)

	case lexer.TokenLBracket:
		// Array index / predicate.
		return p.parseSubscript(left, tok.End)

	case lexer.TokenDot:
		if err := p.advance(); err != nil {
			return nil, err
		}
		right, err := p.expression(74)
		if err != nil {
			return nil, err
		}
		return &Node{Type: NodeBinary, Value: ".", Left: left, Right: right, Pos: tok.End}, nil

	case lexer.TokenAt, lexer.TokenHash:
		return p.parseBinding(left, tok.Type == lexer.TokenAt, tok.End)

	case lexer.TokenQuestion:
		// Conditional (ternary) operator.
		if err := p.advance(); err != nil {
			return nil, err
		}
		then, err := p.expression(0)
		if err != nil {
			return nil, err
		}
		node := (&Node{Type: NodeCondition, Pos: tok.End}).SetCondition(left).SetThen(then)
		if p.token.Type == lexer.TokenColon {
			if err := p.advance(); err != nil {
				return nil, err
			}
			elseBranch, err := p.expression(0)
			if err != nil {
				return nil, err
			}
			node.SetElse(elseBranch)
		}
		return node, nil

	case lexer.TokenAssign:
		// S0212: left side must be a $variable.
		if left.Type != NodeVariable {
			return nil, syntaxError("S0212", left.Value, tok.Value, jsPos(left), "the left side of := must be a $variable name")
		}
		if err := p.advance(); err != nil {
			return nil, err
		}
		right, err := p.expression(bp - 1)
		if err != nil {
			return nil, err
		}
		return &Node{Type: NodeBind, Left: left, Right: right, Pos: tok.End}, nil

	case lexer.TokenChain:
		if err := p.advance(); err != nil {
			return nil, err
		}
		right, err := p.expression(bp - 1)
		if err != nil {
			return nil, err
		}
		return &Node{Type: NodeBinary, Value: "~>", Left: left, Right: right, Pos: tok.End}, nil

	case lexer.TokenElvis:
		if err := p.advance(); err != nil {
			return nil, err
		}
		right, err := p.expression(bp - 1)
		if err != nil {
			return nil, err
		}
		return &Node{Type: NodeBinary, Value: "?:", Left: left, Right: right, Pos: tok.End}, nil

	case lexer.TokenCoalesce:
		if err := p.advance(); err != nil {
			return nil, err
		}
		right, err := p.expression(bp - 1)
		if err != nil {
			return nil, err
		}
		return &Node{Type: NodeBinary, Value: "??", Left: left, Right: right, Pos: tok.End}, nil

	case lexer.TokenDotDot:
		if err := p.advance(); err != nil {
			return nil, err
		}
		right, err := p.expression(bp - 1)
		if err != nil {
			return nil, err
		}
		return &Node{Type: NodeBinary, Value: "..", Left: left, Right: right, Pos: tok.End}, nil

	case lexer.TokenAnd:
		if err := p.advance(); err != nil {
			return nil, err
		}
		right, err := p.expression(bp)
		if err != nil {
			return nil, err
		}
		return &Node{Type: NodeBinary, Value: "and", Left: left, Right: right, Pos: tok.End}, nil

	case lexer.TokenOr:
		if err := p.advance(); err != nil {
			return nil, err
		}
		right, err := p.expression(bp)
		if err != nil {
			return nil, err
		}
		return &Node{Type: NodeBinary, Value: "or", Left: left, Right: right, Pos: tok.End}, nil

	case lexer.TokenIn:
		if err := p.advance(); err != nil {
			return nil, err
		}
		right, err := p.expression(bp)
		if err != nil {
			return nil, err
		}
		return &Node{Type: NodeBinary, Value: "in", Left: left, Right: right, Pos: tok.End}, nil

	case lexer.TokenEquals:
		if err := p.advance(); err != nil {
			return nil, err
		}
		right, err := p.expression(bp)
		if err != nil {
			return nil, err
		}
		return &Node{Type: NodeBinary, Value: "=", Left: left, Right: right, Pos: tok.End}, nil

	case lexer.TokenNE:
		if err := p.advance(); err != nil {
			return nil, err
		}
		right, err := p.expression(bp)
		if err != nil {
			return nil, err
		}
		return &Node{Type: NodeBinary, Value: "!=", Left: left, Right: right, Pos: tok.End}, nil

	case lexer.TokenLT:
		if err := p.advance(); err != nil {
			return nil, err
		}
		right, err := p.expression(bp)
		if err != nil {
			return nil, err
		}
		return &Node{Type: NodeBinary, Value: "<", Left: left, Right: right, Pos: tok.End}, nil

	case lexer.TokenGT:
		if err := p.advance(); err != nil {
			return nil, err
		}
		right, err := p.expression(bp)
		if err != nil {
			return nil, err
		}
		return &Node{Type: NodeBinary, Value: ">", Left: left, Right: right, Pos: tok.End}, nil

	case lexer.TokenLE:
		if err := p.advance(); err != nil {
			return nil, err
		}
		right, err := p.expression(bp)
		if err != nil {
			return nil, err
		}
		return &Node{Type: NodeBinary, Value: "<=", Left: left, Right: right, Pos: tok.End}, nil

	case lexer.TokenGE:
		if err := p.advance(); err != nil {
			return nil, err
		}
		right, err := p.expression(bp)
		if err != nil {
			return nil, err
		}
		return &Node{Type: NodeBinary, Value: ">=", Left: left, Right: right, Pos: tok.End}, nil

	case lexer.TokenPlus:
		if err := p.advance(); err != nil {
			return nil, err
		}
		right, err := p.expression(bp)
		if err != nil {
			return nil, err
		}
		return &Node{Type: NodeBinary, Value: "+", Left: left, Right: right, Pos: tok.End}, nil

	case lexer.TokenMinus:
		if err := p.advance(); err != nil {
			return nil, err
		}
		right, err := p.expression(bp)
		if err != nil {
			return nil, err
		}
		return &Node{Type: NodeBinary, Value: "-", Left: left, Right: right, Pos: tok.End}, nil

	case lexer.TokenStar:
		if err := p.advance(); err != nil {
			return nil, err
		}
		right, err := p.expression(bp)
		if err != nil {
			return nil, err
		}
		return &Node{Type: NodeBinary, Value: "*", Left: left, Right: right, Pos: tok.End}, nil

	case lexer.TokenSlash:
		if err := p.advance(); err != nil {
			return nil, err
		}
		right, err := p.expression(bp)
		if err != nil {
			return nil, err
		}
		return &Node{Type: NodeBinary, Value: "/", Left: left, Right: right, Pos: tok.End}, nil

	case lexer.TokenPercent:
		if err := p.advance(); err != nil {
			return nil, err
		}
		right, err := p.expression(bp)
		if err != nil {
			return nil, err
		}
		return &Node{Type: NodeBinary, Value: "%", Left: left, Right: right, Pos: tok.End}, nil

	case lexer.TokenStarStar:
		if err := p.advance(); err != nil {
			return nil, err
		}
		right, err := p.expression(bp)
		if err != nil {
			return nil, err
		}
		return &Node{Type: NodeBinary, Value: "**", Left: left, Right: right, Pos: tok.End}, nil

	case lexer.TokenAmp:
		if err := p.advance(); err != nil {
			return nil, err
		}
		right, err := p.expression(bp)
		if err != nil {
			return nil, err
		}
		return &Node{Type: NodeBinary, Value: "&", Left: left, Right: right, Pos: tok.End}, nil

	case lexer.TokenLBrace:
		// Group expression: attach key-value pairs to left.
		pos := tok.End
		// S0210: A step can only have one group-by expression.
		if left.Group() != nil {
			p.deferError(syntaxError("S0210", "", "{", pos, "each step can only have one grouping expression"))
		}
		if err := p.advance(); err != nil {
			return nil, err
		}
		pairs, err := p.parseObjectPairs()
		if err != nil {
			return nil, err
		}
		if err := p.advanceInfix(); err != nil { // consume }
			return nil, err
		}
		group := &GroupExpr{Pairs: pairsToGroupPairs(pairs), Pos: pos, OnPath: isPathLike(left)}
		left.SetGroup(group)
		return left, nil

	case lexer.TokenCaret:
		// Sort: ^(term, term, ...)
		return p.parseSortExpr(left, tok.End)

	default:
		return nil, syntaxError("S0201", tokenText(&tok), tok.Value, tok.End, "unexpected token in infix position")
	}
}

// parseBinding parses the $variable bound by @ (focus) or # (index). Like
// jsonata-js, it parses the right side as an expression, binding as tightly as
// a subscript, and then requires a plain $variable (S0214). It reports @ after
// a predicate or sort (S0215, S0216) only once the whole expression has parsed.
func (p *Parser) parseBinding(left *Node, focus bool, pos int) (*Node, error) {
	op := "#"
	if focus {
		op = "@"
	}
	if err := p.advance(); err != nil {
		return nil, err
	}
	rhs, err := p.expression(bindingPower(lexer.TokenLBracket))
	if err != nil {
		return nil, err
	}
	if rhs.Type != NodeVariable {
		return nil, parseError("S0214", op, jsPos(rhs), "the right side of "+op+" must be a $variable")
	}
	left.setIndexLast(!focus)
	if !focus {
		left.SetIndex(rhs.Value)
		return left, nil
	}
	switch {
	case left.Type == NodeBinary && left.Value == "[":
		p.deferError(syntaxError("S0215", "", "@", pos, "the @ operator cannot follow a predicate expression"))
	case left.Type == NodeSort:
		p.deferError(syntaxError("S0216", "", "@", pos, "the @ operator cannot follow a sort expression"))
	}
	left.SetFocus(rhs.Value)
	return left, nil
}

// parseFunctionCall parses a function call: callee(arg, arg, ...).
// Called after consuming the opening (.
func (p *Parser) parseFunctionCall(callee *Node, pos int) (*Node, error) {
	if err := p.advance(); err != nil { // consume (
		return nil, err
	}
	var args []*Node
	partial := false
	for p.token.Type != lexer.TokenRParen {
		if p.token.Type == lexer.TokenEOF {
			return nil, p.endError()
		}
		arg, err := p.parseArgument()
		if err != nil {
			return nil, err
		}
		if arg.Type == NodePlaceholder {
			partial = true
		}
		args = append(args, arg)
		if p.token.Type == lexer.TokenComma {
			if err := p.advance(); err != nil {
				return nil, err
			}
		} else if err := p.expect(lexer.TokenRParen); err != nil {
			return nil, err
		}
	}
	if err := p.advanceInfix(); err != nil { // consume )
		return nil, err
	}

	nodeType := NodeFunction
	if partial {
		nodeType = NodePartial
	}
	return (&Node{Type: nodeType, Value: callee.Value, Pos: pos}).SetProcedure(callee).SetArguments(args), nil
}

// parseArgument parses one call argument or lambda parameter. As in
// jsonata-js, a ? placeholder is only recognized as a whole argument, so
// $f(? + 1) is S0202 and a ? anywhere else is S0211.
func (p *Parser) parseArgument() (*Node, error) {
	if p.token.Type != lexer.TokenQuestion {
		return p.expression(0)
	}
	pos := p.token.End
	if err := p.advance(); err != nil {
		return nil, err
	}
	return &Node{Type: NodePlaceholder, Value: "?", Pos: pos}, nil
}

// parseSubscript parses [ expr ] in infix position.
func (p *Parser) parseSubscript(left *Node, pos int) (*Node, error) {
	if err := p.advance(); err != nil { // consume [
		return nil, err
	}
	if p.token.Type == lexer.TokenRBracket {
		// Empty [] → keep array flag.
		if err := p.advance(); err != nil {
			return nil, err
		}
		alreadyKept := left.KeepArray()
		left.SetKeepArray(true)
		// jsonata-js gives the [] to the # it follows, past any predicates
		// after it, or else to the chain's base.
		bound := left
		for !bound.indexLast() && bound.Type == NodeBinary && bound.Value == "[" && bound.Left != nil {
			bound = bound.Left
		}
		if bound.indexLast() {
			bound.SetKeptAfterIndex(true)
			bound.SetIndexKeepArray(bound.IndexKeepArray() || !alreadyKept)
		}
		return left, nil
	}
	// S0209: A predicate cannot follow a step's group-by expression. A group
	// on a path, as in a{k: v}[0], applies after all of it instead (see
	// ProcessAST).
	if left.Group() != nil && !left.Group().OnPath {
		p.deferError(syntaxError("S0209", "", "[", pos, "a predicate cannot follow a grouping expression in a step"))
	}
	expr, err := p.expression(0)
	if err != nil {
		return nil, err
	}
	if err := p.consumeInfix(lexer.TokenRBracket); err != nil {
		return nil, err
	}
	return &Node{Type: NodeBinary, Value: "[", Left: left, Right: expr, Pos: pos}, nil
}

// parseSortExpr parses ^(term, term, ...) sort expressions.
func (p *Parser) parseSortExpr(left *Node, pos int) (*Node, error) {
	if err := p.advance(); err != nil { // consume ^
		return nil, err
	}
	if err := p.consume(lexer.TokenLParen); err != nil {
		return nil, err
	}
	var terms []SortTerm
	for p.token.Type != lexer.TokenRParen {
		if p.token.Type == lexer.TokenEOF {
			return nil, p.endError()
		}
		descending := p.token.Type == lexer.TokenGT
		if descending || p.token.Type == lexer.TokenLT {
			if err := p.advance(); err != nil {
				return nil, err
			}
		}
		expr, err := p.expression(0)
		if err != nil {
			return nil, err
		}
		terms = append(terms, SortTerm{Descending: descending, Expression: expr})
		if p.token.Type == lexer.TokenComma {
			if err := p.advance(); err != nil {
				return nil, err
			}
		} else if err := p.expect(lexer.TokenRParen); err != nil {
			return nil, err
		}
	}
	if err := p.advance(); err != nil { // consume )
		return nil, err
	}
	return (&Node{Type: NodeSort, Left: left, Pos: pos}).SetTerms(terms), nil
}

// pairsToGroupPairs converts flat [k, v, k, v, ...] slice to [][2]*Node pairs.
func pairsToGroupPairs(flat []*Node) [][2]*Node {
	pairs := make([][2]*Node, 0, len(flat)/2)
	for i := 0; i+1 < len(flat); i += 2 {
		pairs = append(pairs, [2]*Node{flat[i], flat[i+1]})
	}
	return pairs
}
