package functions

import (
	"math"
	"strings"

	"github.com/recolabs/gnata/internal/evaluator"
)

// costFunc bounds what a builtin call allocates (see SignedBuiltin.Cost).
type costFunc = func(args []any, limit int64) (result, scratch int64)

// costs bound the memory of the builtins whose results grow with their
// arguments, charged under WithMemoryBudget before each call. Builtins whose
// results are numbers, booleans or substrings sharing their argument's
// memory need none; $string and $replace charge as they build their result.
var costs = map[string]costFunc{
	"join":               costJoin,
	"pad":                costPad,
	"substring":          costRunes(5), // the []rune copy, then the result
	"uppercase":          costStringTimes(3),
	"lowercase":          costStringTimes(3),
	"trim":               costStringTimes(1),
	"base64encode":       costBase64,
	"base64decode":       costStringTimes(1),
	"encodeUrl":          costEncode,
	"encodeUrlComponent": costEncode,
	"decodeUrl":          costStringTimes(1),
	"decodeUrlComponent": costStringTimes(1),
	"formatNumber":       costPicture,
	"formatInteger":      costPicture,
	"formatBase":         costPicture,
	"fromMillis":         costPicture,
	"split":              costSplit,
	"append":             costAppend,
	"reverse":            costElements(1),
	"shuffle":            costElements(1),
	"sort":               costElements(2), // the result and a copy to sort
	"distinct":           costElements(2), // the result and the set of seen values
	"map":                costElements(1),
	"filter":             costElements(1),
	"zip":                costZip,
	"flatten":            costFlatten,
	"keys":               costEntries(evaluator.ElementBytes, evaluator.EntryBytes), // and the set of keys seen
	"values":             costEntries(evaluator.ElementBytes, 0),
	"spread":             costEntries(evaluator.ElementBytes, evaluator.ObjectBytes+evaluator.EntryBytes), // and an object per entry
	"merge":              costEntries(evaluator.EntryBytes, 0),
	"each":               costEntries(evaluator.ElementBytes, 0),
	"sift":               costEntries(evaluator.EntryBytes, 0),
	"lookup":             costEntries(evaluator.ElementBytes, 0),
	"clone":              costClone,
}

// argString is args[i] when it is a string.
func argString(args []any, i int) string {
	if i < len(args) {
		if s, ok := args[i].(string); ok {
			return s
		}
	}
	return ""
}

// argItems is args[i] as an array, a single value as one item.
func argItems(args []any, i int) []any {
	if i >= len(args) || args[i] == nil {
		return nil
	}
	if items, ok := evaluator.AsArray(args[i]); ok {
		return items
	}
	return []any{args[i]}
}

// mul multiplies two sizes, saturating instead of overflowing.
func mul(a, b int64) int64 {
	if a != 0 && b > math.MaxInt64/a {
		return math.MaxInt64
	}
	return a * b
}

func costStringTimes(k int64) costFunc {
	return func(args []any, _ int64) (int64, int64) { return k * int64(len(argString(args, 0))), 0 }
}

func costRunes(k int64) costFunc {
	return func(args []any, _ int64) (int64, int64) {
		n := int64(len(argString(args, 0)))
		return n, (k - 1) * n
	}
}

func costJoin(args []any, _ int64) (result, scratch int64) {
	items := argItems(args, 0)
	var n int64
	for _, item := range items {
		if s, ok := item.(string); ok {
			n += int64(len(s))
		}
	}
	n += int64(max(len(items)-1, 0)) * int64(len(argString(args, 1)))
	// strings.Join copies the parts into a slice first.
	return n, int64(len(items)) * 16
}

// costPad bounds $pad, whose width is capped (D3010), by the padding as
// runes and as the string it becomes.
func costPad(args []any, _ int64) (result, scratch int64) {
	s := argString(args, 0)
	var width float64
	if len(args) > 1 {
		width, _ = evaluator.ToFloat64(args[1])
	}
	w := int64(min(math.Abs(width), 10_000))
	pad := argString(args, 2)
	if pad == "" {
		pad = " "
	}
	return int64(len(s)) + evaluator.RuneBytes(pad, w), 4*int64(len(s)) + 8*w
}

// costEncode bounds percent-encoding: three bytes per byte at most, built
// in a growing builder whose earlier buffers add up to four times the final
// one (measured).
func costEncode(args []any, _ int64) (result, scratch int64) {
	n := 3 * int64(len(argString(args, 0)))
	return n, 4 * n
}

func costBase64(args []any, _ int64) (result, scratch int64) {
	n := int64(len(argString(args, 0)))
	return 4 * ((n + 2) / 3), n
}

// costPicture bounds a formatting builtin by its picture, which its result
// follows, plus room for the digits or names it fills in.
func costPicture(args []any, _ int64) (result, scratch int64) {
	return 4*int64(len(argString(args, 1))) + 512, 0
}

// costSplit bounds $split by its pieces: exactly counted for a string
// separator, at most one per byte for a regex.
func costSplit(args []any, _ int64) (result, scratch int64) {
	s := argString(args, 0)
	pieces := int64(len(s)) + 1
	if len(args) > 1 {
		if sep, ok := args[1].(string); ok {
			if sep == "" {
				pieces = int64(len(s))
			} else {
				pieces = int64(strings.Count(s, sep)) + 1
			}
		}
	}
	return mul(pieces, evaluator.ElementBytes+16), 0
}

func costAppend(args []any, _ int64) (result, scratch int64) {
	n := int64(len(argItems(args, 0)) + len(argItems(args, 1)))
	return n * evaluator.ElementBytes, 0
}

func costElements(k int64) costFunc {
	return func(args []any, _ int64) (int64, int64) {
		n := int64(len(argItems(args, 0))) * evaluator.ElementBytes
		return n, (k - 1) * n
	}
}

func costZip(args []any, _ int64) (result, scratch int64) {
	shortest := -1
	for i := range args {
		if n := len(argItems(args, i)); shortest < 0 || n < shortest {
			shortest = n
		}
	}
	rows := int64(max(shortest, 0))
	return rows * evaluator.ElementBytes, rows * int64(len(args)) * evaluator.ElementBytes // and an array per row
}

// costFlatten bounds $flatten by the number of leaves, counted up to the
// budget left.
func costFlatten(args []any, limit int64) (result, scratch int64) {
	var n int64
	var count func(v any)
	count = func(v any) {
		items, ok := evaluator.AsArray(v)
		if !ok {
			n += evaluator.ElementBytes
			return
		}
		for _, item := range items {
			count(item)
			if n > limit {
				return
			}
		}
	}
	if len(args) > 0 {
		count(args[0])
	}
	return n, 0
}

// costEntries bounds a builtin by the entries of the objects in its first
// argument: perEntry bytes each in its result, and scratch bytes each in
// what it allocates besides, such as the objects its result holds.
func costEntries(perEntry, scratch int64) costFunc {
	return func(args []any, _ int64) (int64, int64) {
		var n int64
		for _, item := range argItems(args, 0) {
			switch m := item.(type) {
			case *evaluator.OrderedMap:
				n += int64(m.Len())
			case map[string]any:
				n += int64(len(m))
			default:
				n++
			}
		}
		return evaluator.ObjectBytes + n*perEntry, n * scratch
	}
}

func costClone(args []any, limit int64) (result, scratch int64) {
	if len(args) == 0 {
		return 0, 0
	}
	// The copy is all scratch: the result's own size covers only its top.
	return 0, evaluator.SizeOf(args[0], limit)
}
