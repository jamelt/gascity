package zcode_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fixedSleepThreshold separates a poll's own retry-interval sleep (this
// file's longest is 250ms, inside TestInterruptKillsASigintIgnoringTurn's
// manual bounded loop) from a blind "wait long enough" guess sized for an
// idle box (this file's shortest, before ga-b9lkux, was 2s). Anything at or
// above it is a synchronization guess, not a poll interval.
const fixedSleepThreshold = 1 * time.Second

// TestAdapterHarnessHasNoFixedWallClockSleeps guards adapter_test.go against
// reintroducing the flake ga-b9lkux fixes: a fixed time.Sleep standing in for
// "wait long enough" synchronization, sized for an idle box, which a
// contended one silently outruns — the reported symptom was a different test
// failing each run under t.Parallel(). Every lifecycle wait must poll an
// observable condition via waitForTurns/waitForFailures/waitForOutput
// (bounded by adapterWaitBudget) instead of guessing a duration.
//
// This is a structural check: it flags any time.Sleep call in this package
// whose duration is a compile-time constant at or above fixedSleepThreshold,
// or whose duration this test cannot statically evaluate at all (fail closed
// rather than silently pass a sleep it cannot classify). No assertion here
// depends on adapterWaitBudget's value.
func TestAdapterHarnessHasNoFixedWallClockSleeps(t *testing.T) {
	t.Parallel()

	const src = "adapter_test.go"
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, src, nil, 0)
	if err != nil {
		t.Fatalf("parsing %s: %v", src, err)
	}

	var violations []string
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Sleep" {
			return true
		}
		pkgIdent, ok := sel.X.(*ast.Ident)
		if !ok || pkgIdent.Name != "time" {
			return true
		}
		if len(call.Args) != 1 {
			return true
		}
		pos := fset.Position(call.Pos())
		d, ok := constSleepDuration(call.Args[0])
		switch {
		case !ok:
			violations = append(violations, fmt.Sprintf("%s:%d: time.Sleep with a non-constant or unrecognized duration", src, pos.Line))
		case d >= fixedSleepThreshold:
			violations = append(violations, fmt.Sprintf("%s:%d: time.Sleep(%s)", src, pos.Line, d))
		}
		return true
	})

	if len(violations) > 0 {
		t.Fatalf("found fixed time.Sleep call(s) that look like \"wait long enough\" synchronization "+
			"rather than a poll retry interval (threshold %s) — this is the load-sensitive flake "+
			"ga-b9lkux fixes; replace each with a poll on an observable condition "+
			"(waitForTurns/waitForFailures/waitForOutput) bounded by adapterWaitBudget:\n%s",
			fixedSleepThreshold, strings.Join(violations, "\n"))
	}
}

// constSleepDuration evaluates a duration expression of the form
// `<int literal> * time.<Unit>` — the only shape time.Sleep's argument takes
// anywhere in this package today. Anything else reports ok=false so the
// caller fails closed instead of silently skipping a sleep it cannot
// classify.
func constSleepDuration(arg ast.Expr) (time.Duration, bool) {
	bin, ok := arg.(*ast.BinaryExpr)
	if !ok || bin.Op != token.MUL {
		return 0, false
	}
	lit, ok := bin.X.(*ast.BasicLit)
	if !ok || lit.Kind != token.INT {
		return 0, false
	}
	n, err := strconv.ParseInt(lit.Value, 10, 64)
	if err != nil {
		return 0, false
	}
	sel, ok := bin.Y.(*ast.SelectorExpr)
	if !ok {
		return 0, false
	}
	pkgIdent, ok := sel.X.(*ast.Ident)
	if !ok || pkgIdent.Name != "time" {
		return 0, false
	}
	var unit time.Duration
	switch sel.Sel.Name {
	case "Nanosecond":
		unit = time.Nanosecond
	case "Microsecond":
		unit = time.Microsecond
	case "Millisecond":
		unit = time.Millisecond
	case "Second":
		unit = time.Second
	case "Minute":
		unit = time.Minute
	case "Hour":
		unit = time.Hour
	default:
		return 0, false
	}
	return time.Duration(n) * unit, true
}

// TestAdapterWaitBudgetStaysAHangDetector guards adapterWaitBudget against
// being shrunk back into flake territory. ga-b9lkux converts eight fixed
// sleeps (longest 3s, in TestInterruptMidTurnContinuesTheLoop) into polls
// bounded by this constant; shrinking it below a justified floor would
// silently reintroduce the same class of failure on a machine slow enough to
// need the full budget.
//
// The floor is twice the largest fixed deadline removed, matching the
// convention already established for hangBudget in
// cmd/gc/hangbudget_helpers_test.go (TestHangBudgetStaysAHangDetector).
func TestAdapterWaitBudgetStaysAHangDetector(t *testing.T) {
	t.Parallel()

	const largestReplacedDeadline = 3 * time.Second
	if adapterWaitBudget < 2*largestReplacedDeadline {
		t.Fatalf("adapterWaitBudget = %s, want >= %s (twice the largest fixed sleep it replaced); "+
			"it is a hang detector, not a latency assertion — do not tune it to make a test pass",
			adapterWaitBudget, 2*largestReplacedDeadline)
	}
}
