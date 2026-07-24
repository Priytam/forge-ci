package compiler

import "testing"

// TestEvalExprTable exercises the safe expression evaluator across the grammar:
// equality, inequality, regex match/non-match, &&/||/!, precedence, parentheses,
// undefined variables, and null checks.
func TestEvalExprTable(t *testing.T) {
	ctx := map[string]string{
		"CI_COMMIT_BRANCH":   "main",
		"CI_COMMIT_REF":      "main",
		"CI_PIPELINE_SOURCE": "push",
		"DEPLOY":             "yes",
		"EMPTY":              "",
	}
	cases := []struct {
		expr string
		want bool
	}{
		// equality / inequality
		{`$CI_COMMIT_BRANCH == "main"`, true},
		{`$CI_COMMIT_BRANCH == "dev"`, false},
		{`$CI_COMMIT_BRANCH != "dev"`, true},
		{`$CI_PIPELINE_SOURCE == "push"`, true},
		{`$CI_PIPELINE_SOURCE == "web"`, false},
		// undefined variable semantics
		{`$MISSING == null`, true},
		{`$MISSING != null`, false},
		{`$CI_COMMIT_BRANCH == null`, false},
		{`$CI_COMMIT_BRANCH != null`, true},
		{`$MISSING == "x"`, false},
		{`$MISSING != "x"`, true},
		// bare variable truthiness
		{`$DEPLOY`, true},
		{`$EMPTY`, false},   // defined but empty -> falsy
		{`$MISSING`, false}, // undefined -> falsy
		{`!$MISSING`, true},
		{`!$DEPLOY`, false},
		// regex match / non-match
		{`$CI_COMMIT_BRANCH =~ /^main$/`, true},
		{`$CI_COMMIT_BRANCH =~ /^feat/`, false},
		{`$CI_COMMIT_REF =~ /ma.n/`, true},
		{`$CI_COMMIT_BRANCH !~ /^feat/`, true},
		{`$CI_COMMIT_BRANCH !~ /^main$/`, false},
		{`$MISSING =~ /x/`, false}, // undefined never matches
		{`$MISSING !~ /x/`, true},  // ... so !~ is true
		// && / || / precedence
		{`$CI_COMMIT_BRANCH == "main" && $DEPLOY == "yes"`, true},
		{`$CI_COMMIT_BRANCH == "main" && $DEPLOY == "no"`, false},
		{`$CI_COMMIT_BRANCH == "dev" || $DEPLOY == "yes"`, true},
		{`$CI_COMMIT_BRANCH == "dev" || $DEPLOY == "no"`, false},
		// && binds tighter than || : false && false || true == true
		{`$CI_COMMIT_BRANCH == "dev" && $DEPLOY == "yes" || $CI_PIPELINE_SOURCE == "push"`, true},
		// ... and would be false if && were looser and grouped as false && (…||…)
		{`$CI_PIPELINE_SOURCE == "push" || $CI_COMMIT_BRANCH == "dev" && $DEPLOY == "no"`, true},
		// parentheses override precedence
		{`($CI_COMMIT_BRANCH == "main" || $CI_COMMIT_BRANCH == "dev") && $DEPLOY == "yes"`, true},
		{`($CI_COMMIT_BRANCH == "dev" || $CI_COMMIT_BRANCH == "stage") && $DEPLOY == "yes"`, false},
		// negation with parentheses
		{`!($CI_COMMIT_BRANCH == "dev")`, true},
		{`!($CI_COMMIT_BRANCH == "main")`, false},
		// regex combined with boolean
		{`$CI_COMMIT_BRANCH =~ /^main$/ && $CI_PIPELINE_SOURCE == "push"`, true},
	}
	for _, c := range cases {
		got, err := evalExpr(c.expr, ctx)
		if err != nil {
			t.Errorf("evalExpr(%q) unexpected error: %v", c.expr, err)
			continue
		}
		if got != c.want {
			t.Errorf("evalExpr(%q) = %v, want %v", c.expr, got, c.want)
		}
	}
}

// TestEvalExprErrors verifies malformed expressions are hard errors, never a
// silent false.
func TestEvalExprErrors(t *testing.T) {
	bad := []string{
		`$CI_COMMIT_BRANCH ==`,         // missing rhs
		`== "main"`,                    // missing lhs
		`$CI_COMMIT_BRANCH == "main`,   // unterminated string
		`$CI_COMMIT_BRANCH =~ /unterm`, // unterminated regex
		`(a == "b"`,                    // unbalanced paren (bare ident, but paren err too)
		`$`,                            // dangling variable sigil
		`foo == "bar"`,                 // bare identifier (must be $foo)
		`$X =~ /[/`,                    // invalid regex literal
		`$X == "a" $Y == "b"`,          // trailing tokens
	}
	for _, expr := range bad {
		if _, err := evalExpr(expr, map[string]string{}); err == nil {
			t.Errorf("evalExpr(%q) expected an error, got nil", expr)
		}
	}
}
