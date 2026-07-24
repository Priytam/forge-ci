package compiler

// A small, SAFE expression evaluator for rules: if: clauses.
//
// This is deliberately NOT a general-purpose language: there is no arbitrary
// code execution, no function calls, no arithmetic — only string/variable
// comparison and boolean composition, exactly the subset GitLab's rules if:
// uses in practice. Everything is a hand-written tokenizer + recursive-descent
// parser + tree walker over this grammar:
//
//	expr    := or
//	or      := and ( '||' and )*
//	and     := cmp ( '&&' cmp )*
//	cmp     := unary ( ('==' | '!=' | '=~' | '!~') unary )?
//	unary   := '!' unary | primary
//	primary := '(' expr ')' | variable | string | regex | 'null'
//
// Operands are strings, $VARIABLES (which resolve against the pipeline
// variable context, or are "undefined" when absent), /regex/ literals, and the
// null keyword. Undefined variables behave like GitLab's: `$X == null` is true
// when X is unset, `$X == "y"` is false when X is unset, and a bare `$X` is
// truthy only when X is set to a non-empty value.

import (
	"fmt"
	"regexp"
	"strings"
)

type tokKind int

const (
	tEOF tokKind = iota
	tLParen
	tRParen
	tEq     // ==
	tNe     // !=
	tMatch  // =~
	tNMatch // !~
	tAnd    // &&
	tOr     // ||
	tNot    // !
	tString // 'literal' or "literal"
	tRegex  // /pattern/
	tVar    // $NAME or ${NAME}
	tNull   // null
)

type token struct {
	kind tokKind
	text string
}

// lex turns the raw expression into tokens. It returns a clear error for
// unterminated strings/regexes and unexpected characters.
func lex(s string) ([]token, error) {
	var toks []token
	i, n := 0, len(s)
	for i < n {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '(':
			toks = append(toks, token{tLParen, "("})
			i++
		case c == ')':
			toks = append(toks, token{tRParen, ")"})
			i++
		case c == '=' && i+1 < n && s[i+1] == '=':
			toks = append(toks, token{tEq, "=="})
			i += 2
		case c == '=' && i+1 < n && s[i+1] == '~':
			toks = append(toks, token{tMatch, "=~"})
			i += 2
		case c == '!' && i+1 < n && s[i+1] == '=':
			toks = append(toks, token{tNe, "!="})
			i += 2
		case c == '!' && i+1 < n && s[i+1] == '~':
			toks = append(toks, token{tNMatch, "!~"})
			i += 2
		case c == '!':
			toks = append(toks, token{tNot, "!"})
			i++
		case c == '&' && i+1 < n && s[i+1] == '&':
			toks = append(toks, token{tAnd, "&&"})
			i += 2
		case c == '|' && i+1 < n && s[i+1] == '|':
			toks = append(toks, token{tOr, "||"})
			i += 2
		case c == '\'' || c == '"':
			quote := c
			j := i + 1
			var b strings.Builder
			for j < n && s[j] != quote {
				if s[j] == '\\' && j+1 < n {
					b.WriteByte(s[j+1])
					j += 2
					continue
				}
				b.WriteByte(s[j])
				j++
			}
			if j >= n {
				return nil, fmt.Errorf("unterminated string literal")
			}
			toks = append(toks, token{tString, b.String()})
			i = j + 1
		case c == '/':
			j := i + 1
			var b strings.Builder
			for j < n && s[j] != '/' {
				if s[j] == '\\' && j+1 < n {
					// keep the escape so regexp sees e.g. \/ or \d intact
					b.WriteByte(s[j])
					b.WriteByte(s[j+1])
					j += 2
					continue
				}
				b.WriteByte(s[j])
				j++
			}
			if j >= n {
				return nil, fmt.Errorf("unterminated regex literal")
			}
			toks = append(toks, token{tRegex, b.String()})
			i = j + 1
		case c == '$':
			j := i + 1
			braced := false
			if j < n && s[j] == '{' {
				braced = true
				j++
			}
			start := j
			for j < n && (isIdent(s[j])) {
				j++
			}
			if start == j {
				return nil, fmt.Errorf("expected variable name after '$'")
			}
			name := s[start:j]
			if braced {
				if j >= n || s[j] != '}' {
					return nil, fmt.Errorf("unterminated ${...} variable")
				}
				j++
			}
			toks = append(toks, token{tVar, name})
			i = j
		default:
			// bare identifier: only `null` is meaningful here.
			if isIdentStart(c) {
				j := i
				for j < n && isIdent(s[j]) {
					j++
				}
				word := s[i:j]
				if word == "null" {
					toks = append(toks, token{tNull, "null"})
				} else {
					return nil, fmt.Errorf("unexpected identifier %q (variables must be written $%s)", word, word)
				}
				i = j
				continue
			}
			return nil, fmt.Errorf("unexpected character %q in expression", string(c))
		}
	}
	toks = append(toks, token{tEOF, ""})
	return toks, nil
}

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}
func isIdent(c byte) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9')
}

// ---- values ----

type valKind int

const (
	vString valKind = iota
	vBool
	vNull
	vUndef // a variable that is not set
)

type value struct {
	kind valKind
	s    string
	b    bool
}

func truthy(v value) bool {
	switch v.kind {
	case vBool:
		return v.b
	case vString:
		return v.s != ""
	default: // vNull, vUndef
		return false
	}
}

// coerceStr renders a value as a string for equality/regex purposes.
func coerceStr(v value) string {
	switch v.kind {
	case vBool:
		if v.b {
			return "true"
		}
		return "false"
	case vString:
		return v.s
	default:
		return ""
	}
}

func nullish(v value) bool { return v.kind == vNull || v.kind == vUndef }

// ---- AST ----

type node interface {
	eval(ctx map[string]string) value
}

type litNode struct{ v value }

func (n litNode) eval(map[string]string) value { return n.v }

type varNode struct{ name string }

func (n varNode) eval(ctx map[string]string) value {
	if val, ok := ctx[n.name]; ok {
		return value{kind: vString, s: val}
	}
	return value{kind: vUndef}
}

type notNode struct{ x node }

func (n notNode) eval(ctx map[string]string) value {
	return value{kind: vBool, b: !truthy(n.x.eval(ctx))}
}

type boolNode struct {
	op   tokKind // tAnd | tOr
	l, r node
}

func (n boolNode) eval(ctx map[string]string) value {
	lt := truthy(n.l.eval(ctx))
	if n.op == tAnd {
		if !lt {
			return value{kind: vBool, b: false}
		}
		return value{kind: vBool, b: truthy(n.r.eval(ctx))}
	}
	// tOr
	if lt {
		return value{kind: vBool, b: true}
	}
	return value{kind: vBool, b: truthy(n.r.eval(ctx))}
}

type cmpNode struct {
	op   tokKind // tEq | tNe
	l, r node
}

func (n cmpNode) eval(ctx map[string]string) value {
	l, r := n.l.eval(ctx), n.r.eval(ctx)
	var eq bool
	if nullish(l) || nullish(r) {
		eq = nullish(l) && nullish(r)
	} else {
		eq = coerceStr(l) == coerceStr(r)
	}
	if n.op == tNe {
		eq = !eq
	}
	return value{kind: vBool, b: eq}
}

type matchNode struct {
	negate bool
	l, r   node
	re     *regexp.Regexp // precompiled when RHS is a literal
}

func (n matchNode) eval(ctx map[string]string) value {
	l := n.l.eval(ctx)
	matched := false
	if !nullish(l) {
		re := n.re
		if re == nil {
			r := n.r.eval(ctx)
			if compiled, err := regexp.Compile(coerceStr(r)); err == nil {
				re = compiled
			}
		}
		if re != nil {
			matched = re.MatchString(coerceStr(l))
		}
	}
	if n.negate {
		matched = !matched
	}
	return value{kind: vBool, b: matched}
}

// ---- parser ----

type parser struct {
	toks []token
	pos  int
}

func (p *parser) peek() token { return p.toks[p.pos] }
func (p *parser) next() token { t := p.toks[p.pos]; p.pos++; return t }

func (p *parser) parseExpr() (node, error) { return p.parseOr() }

func (p *parser) parseOr() (node, error) {
	left, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for p.peek().kind == tOr {
		p.next()
		right, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		left = boolNode{op: tOr, l: left, r: right}
	}
	return left, nil
}

func (p *parser) parseAnd() (node, error) {
	left, err := p.parseCmp()
	if err != nil {
		return nil, err
	}
	for p.peek().kind == tAnd {
		p.next()
		right, err := p.parseCmp()
		if err != nil {
			return nil, err
		}
		left = boolNode{op: tAnd, l: left, r: right}
	}
	return left, nil
}

func (p *parser) parseCmp() (node, error) {
	left, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	switch p.peek().kind {
	case tEq, tNe:
		op := p.next().kind
		right, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		return cmpNode{op: op, l: left, r: right}, nil
	case tMatch, tNMatch:
		op := p.next().kind
		right, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		m := matchNode{negate: op == tNMatch, l: left, r: right}
		// Precompile when the RHS is a literal so a bad pattern is a parse
		// error, not a silent no-match.
		if lit, ok := right.(litNode); ok && (lit.v.kind == vString) {
			re, err := regexp.Compile(lit.v.s)
			if err != nil {
				return nil, fmt.Errorf("invalid regex %q: %w", lit.v.s, err)
			}
			m.re = re
		}
		return m, nil
	}
	return left, nil
}

func (p *parser) parseUnary() (node, error) {
	if p.peek().kind == tNot {
		p.next()
		x, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		return notNode{x: x}, nil
	}
	return p.parsePrimary()
}

func (p *parser) parsePrimary() (node, error) {
	t := p.next()
	switch t.kind {
	case tLParen:
		inner, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		if p.peek().kind != tRParen {
			return nil, fmt.Errorf("expected ')'")
		}
		p.next()
		return inner, nil
	case tVar:
		return varNode{name: t.text}, nil
	case tString:
		return litNode{v: value{kind: vString, s: t.text}}, nil
	case tRegex:
		// A bare regex used as a value is unusual; represent it as its source
		// string so `$X == /re/` degrades to string comparison rather than
		// erroring. Regex semantics apply only via =~ / !~ (handled above).
		return litNode{v: value{kind: vString, s: t.text}}, nil
	case tNull:
		return litNode{v: value{kind: vNull}}, nil
	case tEOF:
		return nil, fmt.Errorf("unexpected end of expression")
	default:
		return nil, fmt.Errorf("unexpected token %q", t.text)
	}
}

// evalExpr parses and evaluates a rules if: expression against the variable
// context and reports its boolean truth value. A syntactically invalid
// expression is a hard error (surfaced as a config error), never a silent
// false.
func evalExpr(expr string, ctx map[string]string) (bool, error) {
	toks, err := lex(expr)
	if err != nil {
		return false, err
	}
	p := &parser{toks: toks}
	root, err := p.parseExpr()
	if err != nil {
		return false, err
	}
	if p.peek().kind != tEOF {
		return false, fmt.Errorf("unexpected trailing token %q", p.peek().text)
	}
	return truthy(root.eval(ctx)), nil
}
