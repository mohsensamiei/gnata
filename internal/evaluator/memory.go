package evaluator

import (
	"encoding/json"
	"math"
	"unicode/utf8"
	"unsafe"
)

// The sizes Charge estimates the values an evaluation builds with (see
// WithMemoryBudget): the bytes of every string, plus a fixed overhead per
// array element and per object entry. They are a little above what the Go
// runtime spends on a 64-bit target, so that the budget bounds the heap.
const (
	// ElementBytes is the overhead of one array element: the interface value
	// in the backing array and, for a number, the boxed float64 (measured:
	// 40 bytes per item of a range), and the backing arrays an array built by
	// appending leaves behind: Go grows a large slice by a quarter, so they
	// add up to four times the final one (measured: about 80 bytes per item
	// a wildcard collects).
	ElementBytes = 96
	// SlotBytes is ElementBytes for an array allocated at its final size at
	// once, as the range operator's is.
	SlotBytes = 48
	// EntryBytes is the overhead of one object entry: the key in the order
	// slice and the key and value in the map, and the earlier slices and
	// groups both leave behind as they grow.
	EntryBytes = 128
	// ObjectBytes is the fixed size of an object: the ordered map and the
	// first group of slots of its map (measured: about 300 bytes for an
	// object of one entry).
	ObjectBytes = 256
	// stringBytes is the fixed size of a string value outside an array.
	stringBytes = 16
	// ScopeBytes is what a block or a function call allocates for its scope:
	// the environment and, for a call, its arguments.
	ScopeBytes = 160
	// callBytes is what a builtin call allocates around its result.
	callBytes = 64
)

// ShallowSize is the size of v not counting the values it holds: what
// building it allocates when those values exist already.
func ShallowSize(v any) int64 {
	switch val := v.(type) {
	case string:
		return int64(len(val))
	case []any:
		return int64(len(val)) * ElementBytes
	case ConsArray:
		return int64(len(val)) * ElementBytes
	case KeptArray:
		return int64(len(val)) * ElementBytes
	case RawSequence:
		return int64(len(val)) * ElementBytes
	case *Sequence:
		return int64(len(val.Values)) * ElementBytes
	case *OrderedMap:
		return ObjectBytes + int64(val.Len())*EntryBytes
	case map[string]any:
		return ObjectBytes + int64(len(val))*EntryBytes
	}
	return 0
}

// SizeOf is the size of a copy of v, counting each occurrence of a value
// shared at several places: what copying it allocates. It stops counting
// once the size passes limit, returning a size above limit, so that a value
// sharing arrays at many levels costs no more than the budget to measure.
func SizeOf(v any, limit int64) int64 {
	var n int64
	sizeOf(v, limit, &n)
	return n
}

func sizeOf(v any, limit int64, n *int64) {
	if *n > limit {
		return
	}
	*n += ShallowSize(v)
	switch val := v.(type) {
	case string:
		*n += stringBytes
	case *OrderedMap:
		val.Range(func(k string, item any) bool {
			*n += int64(len(k))
			sizeOf(item, limit, n)
			return *n <= limit
		})
	case map[string]any:
		for k, item := range val {
			*n += int64(len(k))
			sizeOf(item, limit, n)
			if *n > limit {
				return
			}
		}
	default:
		if items, ok := AsArray(v); ok {
			for _, item := range items {
				sizeOf(item, limit, n)
				if *n > limit {
					return
				}
			}
		}
	}
}

// JSONSizeBound is at least the length of v serialized as JSON, as $string
// and & lay it out (see AppendJSON): strings exactly, each float64 as at
// most 32 bytes. Like SizeOf it stops past limit.
func JSONSizeBound(v any, limit int64) int64 {
	var n int64
	jsonSizeBound(v, limit, &n)
	return n
}

func jsonSizeBound(v any, limit int64, n *int64) {
	if *n > limit {
		return
	}
	switch val := v.(type) {
	case string:
		*n += jsonStringLen(val)
	case float64:
		*n += 32
	case json.Number:
		// Laid out as its decimal value, which can run to a hundred digits.
		*n += max(int64(len(val)), 128)
	case *OrderedMap:
		*n += 2
		val.Range(func(k string, item any) bool {
			*n += jsonStringLen(k) + 2
			jsonSizeBound(item, limit, n)
			return *n <= limit
		})
	case map[string]any:
		*n += 2
		for k, item := range val {
			*n += jsonStringLen(k) + 2
			jsonSizeBound(item, limit, n)
			if *n > limit {
				return
			}
		}
	default:
		if items, ok := AsArray(v); ok {
			*n += 2
			for _, item := range items {
				*n++
				jsonSizeBound(item, limit, n)
				if *n > limit {
					return
				}
			}
			return
		}
		*n += 8 // true, false, null; a function serializes as ""
	}
}

// jsonStringLen is the length of s as appendJSONString writes it.
func jsonStringLen(s string) int64 {
	n := int64(2)
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			switch {
			case c >= 0x20 && c != '"' && c != '\\':
				n++
			case c == '"' || c == '\\' || c == '\b' || c == '\f' || c == '\n' || c == '\r' || c == '\t':
				n += 2
			default:
				n += 6
			}
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			n += 3
		case r == '\u2028' || r == '\u2029':
			n += 6
		default:
			n += int64(size)
		}
		i += size
	}
	return n
}

// ChargeStringify charges laying out v as JSON before it is done: a copy
// of v (see JSONValue), the buffer, sized by JSONBuffer, and the string made
// from it. It returns the bytes charged for that string, which Settle
// brings down to its length.
func (e *Environment) ChargeStringify(v any) (int64, error) {
	if !e.MemoryLimited() {
		return 0, nil
	}
	limit := e.remaining()
	text := JSONSizeBound(v, limit)
	if err := e.Charge(saturatingAdd(SizeOf(v, limit), text)); err != nil {
		return 0, err
	}
	return text, e.Charge(text)
}

// JSONBuffer is the buffer to lay out JSON text in after ChargeStringify
// charged text bytes for it: one holding them all under a memory budget, so
// that it is allocated once, as charged, and nil otherwise.
func JSONBuffer(text int64) []byte {
	if text <= 0 {
		return nil
	}
	return make([]byte, 0, text)
}

// IndentBound is at least how many bytes indenting the JSON text of v by
// two spaces per level adds to it: a newline and the indentation before
// each value and closing bracket. It stops past limit.
func IndentBound(v any, limit int64) int64 {
	var n int64
	indentBound(v, 1, limit, &n)
	return n
}

func indentBound(v any, depth, limit int64, n *int64) {
	if *n > limit {
		return
	}
	line := 2*depth + 2 // newline, indentation and ": " after a key
	switch val := v.(type) {
	case *OrderedMap:
		*n += line
		val.Range(func(_ string, item any) bool {
			*n += line
			indentBound(item, depth+1, limit, n)
			return *n <= limit
		})
	case map[string]any:
		*n += line
		for _, item := range val {
			*n += line
			indentBound(item, depth+1, limit, n)
			if *n > limit {
				return
			}
		}
	default:
		if items, ok := AsArray(v); ok {
			*n += line
			for _, item := range items {
				*n += line
				indentBound(item, depth+1, limit, n)
				if *n > limit {
					return
				}
			}
		}
	}
}

// remaining is how much of the budget is left, the limit for SizeOf and
// JSONSizeBound: measuring further would only find the budget exceeded.
func (e *Environment) remaining() int64 {
	if !e.MemoryLimited() {
		return math.MaxInt64
	}
	g := e.callCounter().guard
	return g.memLimit - g.memUsed
}

// Remaining is the memory budget left to the evaluation, or math.MaxInt64
// without a budget.
func (e *Environment) Remaining() int64 { return e.remaining() }

// ChargeCopy charges a copy of v (see SizeOf).
func (e *Environment) ChargeCopy(v any) error {
	if !e.MemoryLimited() {
		return nil
	}
	return e.Charge(SizeOf(v, e.remaining()))
}

// ChargeJSON charges the JSON text of v before it is laid out, returning
// the bytes charged, which Settle brings down to its length.
func (e *Environment) ChargeJSON(v any) (int64, error) {
	if !e.MemoryLimited() {
		return 0, nil
	}
	n := JSONSizeBound(v, e.remaining())
	return n, e.Charge(n)
}

// Settle adjusts a charge of charged bytes, made before the allocation from
// an upper bound, to the size actually allocated.
func (e *Environment) Settle(charged, actual int64) error {
	if !e.MemoryLimited() {
		return nil
	}
	if actual > charged {
		return e.Charge(actual - charged)
	}
	e.Refund(charged - actual)
	return nil
}

// chargeBuiltin charges a call of builtin f with args, before the call,
// from f's Cost estimate, returning the bytes charged for its result.
func chargeBuiltin(f *SignedBuiltin, args []any, env *Environment) (int64, error) {
	if !env.MemoryLimited() {
		return 0, nil
	}
	if f.Cost == nil {
		return 0, env.Charge(callBytes)
	}
	result, scratch := f.Cost(args, env.remaining())
	return result, env.Charge(saturatingAdd(result, scratch+callBytes))
}

// settleBuiltin settles the charge for a builtin call's result to its size,
// nothing when the builtin returned one of its arguments.
func settleBuiltin(charged int64, args []any, result any, env *Environment) error {
	if !env.MemoryLimited() || charged == 0 {
		return nil
	}
	actual := ShallowSize(result)
	for _, arg := range args {
		if sharesMemory(arg, result) {
			actual = 0
			break
		}
	}
	return env.Settle(charged, actual)
}

// sharesMemory reports whether a and b are the same string or array, sharing
// their memory.
func sharesMemory(a, b any) bool {
	switch av := a.(type) {
	case string:
		bv, ok := b.(string)
		return ok && av != "" && len(av) == len(bv) && unsafe.StringData(av) == unsafe.StringData(bv)
	case []any:
		bv, ok := b.([]any)
		return ok && sameArray(av, bv)
	}
	return false
}

// saturatingAdd adds two non-negative sizes without overflowing.
func saturatingAdd(a, b int64) int64 {
	if a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}

// RuneBytes is at most how many bytes n runes of s take: n times its
// longest rune.
func RuneBytes(s string, n int64) int64 {
	widest := 1
	for _, r := range s {
		widest = max(widest, utf8.RuneLen(r))
	}
	return n * int64(widest)
}
