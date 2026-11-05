//go:build !tinygo && !js

package gnata_test

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/recolabs/gnata"
)

const (
	// testBudget is the memory budget the tests evaluate with.
	testBudget = 32 << 20
	// heapMargin is how far the heap may grow past the budget during an
	// evaluation: the allocations the budget does not charge (environments,
	// bookkeeping, the garbage of growing slices) and the measurement's own.
	heapMargin = 16 << 20
)

// peakHeap evaluates expr with the budget and a timeout, returning how far
// the heap grew above where it started, sampled throughout, and its error.
func peakHeap(t testing.TB, expr string, data any) (grew uint64, evalErr error) {
	t.Helper()
	e, err := gnata.Compile(expr, gnata.WithMemoryBudget(testBudget), gnata.WithTimeout(5*time.Second))
	if err != nil {
		return 0, err
	}
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	base := ms.HeapAlloc
	var peak atomic.Uint64
	peak.Store(base)
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		var m runtime.MemStats
		for {
			runtime.ReadMemStats(&m)
			if m.HeapAlloc > peak.Load() {
				peak.Store(m.HeapAlloc)
			}
			select {
			case <-stop:
				return
			case <-time.After(100 * time.Microsecond):
			}
		}
	}()
	_, err = e.Eval(context.Background(), data)
	close(stop)
	<-done
	runtime.ReadMemStats(&ms)
	if ms.HeapAlloc > peak.Load() {
		peak.Store(ms.HeapAlloc)
	}
	return peak.Load() - base, err
}

// budgetBombs build values that grow exponentially or to gigabytes.
var budgetBombs = []string{
	// String doubling, which reached 1.8 GiB before WithTimeout stopped it.
	`$reduce([1..40], function($a, $v){ $a & $a }, "a")`,
	`$reduce([1..40], function($a, $v){ $string([$a, $a]) }, "a")`,
	`$reduce([1..40], function($a, $v){ $replace($a, "a", "aa") }, "a")`,
	`$reduce([1..40], function($a, $v){ $uppercase($a & $lowercase($a)) }, "a")`,
	// $pad's width is capped, so padding bombs by repetition.
	`[1..1000000].$pad("", 10000, "🙂")`,
	`$reduce([1..40], function($a, $v){ $pad($a, 10000, $a) & $a }, "ab")`,
	// $join over a large range.
	`$join([1..9000000].$string($), ",")`,
	// Array doubling.
	`$reduce([1..40], function($a, $v){ $append($a, $a) }, [1])`,
	`$reduce([1..40], function($a, $v){ [$a, $a] ~> $flatten() }, [1])`,
	`$reduce([1..40], function($a, $v){ $split($a & $a, "") ~> $join() }, "ab")`,
	`[1..9999999]`,
	`[1..3000].([1..3000])`,
	// Object doubling with $merge.
	`$reduce([1..40], function($a, $i){ $merge([$a, $merge($each($a, function($val, $k){ {$k & "." & $string($i): $val} }))]) }, {"a": 1})`,
	// Recursive transform: each copy holds the previous one twice.
	`$reduce([1..40], function($a, $v){ $a ~> |$|{"x": $a, "y": $a}| }, {"a": 1})`,
	`$reduce([1..40], function($a, $v){ {"l": $a, "r": $a} ~> |**|{"z": 1}| }, {"a": 1})`,
	// Group-by and wildcards over large data.
	`[1..2000000]{$string($ % 1000000): $}`,
	`$reduce([1..40], function($a, $v){ {"l": $a, "r": $a}.** }, {"a": 1})`,
}

func TestMemoryBudgetBombs(t *testing.T) {
	for _, expr := range budgetBombs {
		t.Run(expr, func(t *testing.T) {
			grew, err := peakHeap(t, expr, nil)
			if err == nil || !strings.HasPrefix(err.Error(), "D1013:") {
				t.Fatalf("got error %v, want D1013", err)
			}
			if grew > testBudget+heapMargin {
				t.Errorf("heap grew %d MiB, budget %d MiB plus %d MiB", grew>>20, testBudget>>20, heapMargin>>20)
			}
			t.Logf("D1013 with the heap %.1f MiB above its start", float64(grew)/(1<<20))
		})
	}
}

// TestMemoryBudgetAllows checks that expressions within the budget are
// unaffected by it.
func TestMemoryBudgetAllows(t *testing.T) {
	for _, tc := range []struct {
		expr string
		want any
	}{
		{`$length($reduce([1..20], function($a, $v){ $a & $a }, "a"))`, float64(1 << 20)},
		{`$count([1..100000])`, float64(100000)},
		{
			`$count($keys($reduce([1..10], function($a, $i){ ` +
				`$merge([$a, $merge($each($a, function($val, $k){ {$k & "." & $string($i): $val} }))]) }, {"a": 1})))`,
			float64(1024),
		},
		{`$string({"a": [1, "x\n"]}, true)`, "{\n  \"a\": [\n    1,\n    \"x\\n\"\n  ]\n}"},
	} {
		e, err := gnata.Compile(tc.expr, gnata.WithMemoryBudget(testBudget))
		if err != nil {
			t.Fatal(err)
		}
		got, err := e.Eval(context.Background(), nil)
		if err != nil {
			t.Fatalf("%s: %v", tc.expr, err)
		}
		if got != tc.want {
			t.Errorf("%s = %#v, want %#v", tc.expr, got, tc.want)
		}
	}
}

// budgetPieces are the operators and functions FuzzMemoryBudget combines:
// each takes the value grown so far, $a, and the step, $i.
var budgetPieces = []string{
	`$a & $a`,
	`$string([$a, $a])`,
	`$string({"k": $a, "j": [$a]}, true)`,
	`$join([$string($a), $string($a)], $string($i))`,
	`$pad($string($a), 10000, "🙂")`,
	`$replace($string($a), /[a-z]/, "$0$0")`,
	`$replace($string($a), "a", function($m){ $m.match & $m.match })`,
	`$formatNumber($i, "#,##0.00")`,
	`$substring($string($a), 1)`,
	`$uppercase($string($a))`,
	`$base64encode($string($a))`,
	`$encodeUrlComponent($string($a))`,
	`[$a, $a]`,
	`[1..$i * 1000]`,
	`$append($a, $a)`,
	`$map([1..$i * 100], function($v){ $a })`,
	`$filter([$a, $a, $a], function($v){ true })`,
	`$split($string($a), "")`,
	`$spread({"x": $a, "y": $a})`,
	`$keys({"k" & $string($i): $a})`,
	`$sort([$a, $a])`,
	`$reverse([$a, $a])`,
	`$zip([$a, $a], [$a, $a])`,
	`$distinct([$a, $i])`,
	`{"l": $a, "r": $a}`,
	`{"l": $a, "r": $a}.*`,
	`{"l": $a, "r": $a}.**`,
	`[$a, $a]{$string($i): $}`,
	`$merge([{"x": $a}, {"k" & $string($i): $a}])`,
	`$a ~> |$|{"x": $a}|`,
	`$lookup({"l": $a}, "l")`,
	`$each({"l": $a, "r": $a}, function($v, $k){ $v })`,
	`$sift({"l": $a, "r": $a}, function($v){ true })`,
	`$clone({"l": $a, "r": $a})`,
}

// fuzzExpression builds an expression growing a value with the pieces the
// bytes of data pick, nested in a $reduce over as many steps as it says.
func fuzzExpression(data []byte) string {
	if len(data) == 0 {
		return `"a"`
	}
	steps := 10 + int(data[0])%40
	var body strings.Builder
	body.WriteString("$a")
	for i, b := range data[1:min(len(data), 5)] {
		piece := budgetPieces[int(b)%len(budgetPieces)]
		if i == 0 {
			body.Reset()
			body.WriteString(piece)
			continue
		}
		// Feed the expression so far into the next piece as $a.
		inner := body.String()
		body.Reset()
		body.WriteString("($a := (" + inner + "); " + piece + ")")
	}
	return fmt.Sprintf(`$reduce([1..%d], function($a, $i){ %s }, "ab")`, steps, body.String())
}

// FuzzMemoryBudget evaluates random combinations of operators and
// functions under the budget: none may grow the heap past it by more than
// the margin. Each finding is an allocation the budget does not charge.
func FuzzMemoryBudget(f *testing.F) {
	for i := range budgetPieces {
		f.Add([]byte{30, byte(i)})
		f.Add([]byte{40, byte(i), byte(i + 7), byte(i + 13)})
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		expr := fuzzExpression(data)
		grew, err := peakHeap(t, expr, nil)
		if grew > testBudget+heapMargin {
			t.Fatalf("%s: heap grew %d MiB (error %v)", expr, grew>>20, err)
		}
	})
}
