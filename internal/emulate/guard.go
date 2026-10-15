package emulate

import (
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"text/template"
	"unicode/utf8"
)

// A template has no loops, but its string functions can still build a large
// string before anything is written: each printf may double a value kept in
// a variable ({{$b := printf "%s%s" $a $a}}). The guard replaces the
// builtins that make strings with bounded ones: no single result may be over
// the render's limit, and all of them together may not be over budgetFactor
// times it. It also stops a render that ran past its deadline.
type guard struct {
	limit   int
	budget  int // bytes the functions may still produce; used by one goroutine
	stopped atomic.Bool
}

// budgetFactor bounds what a render's functions build, in all, against its
// limit.
const budgetFactor = 4

var errStopped = errors.New("rendering stopped")

func newGuard(limit int) *guard { return &guard{limit: limit, budget: budgetFactor * limit} }

// allow refuses a result that could be over the limit, before it is built.
func (g *guard) allow(fn string, upTo int) error {
	if g.stopped.Load() {
		return errStopped
	}
	if upTo > g.limit {
		return fmt.Errorf("%s could build more than %d bytes", fn, g.limit)
	}
	return nil
}

// spend charges a built result against the render's budget.
func (g *guard) spend(s string) (string, error) {
	if len(s) > g.budget {
		return "", fmt.Errorf("the template's functions build more than %d bytes in all", budgetFactor*g.limit)
	}
	g.budget -= len(s)
	return s, nil
}

func (g *guard) funcs() template.FuncMap {
	escaper := func(name string, esc func(...any) string) func(...any) (string, error) {
		return func(args ...any) (string, error) {
			// An escape writes at most 6 bytes per input byte (<).
			if err := g.allow(name, 6*argsLen(args)+len(args)); err != nil {
				return "", err
			}
			return g.spend(esc(args...))
		}
	}
	return template.FuncMap{
		"printf": func(format string, args ...any) (string, error) {
			if err := g.allow("printf", printfBound(format, args)); err != nil {
				return "", err
			}
			return g.spend(fmt.Sprintf(format, args...))
		},
		"print": func(args ...any) (string, error) {
			if err := g.allow("print", argsLen(args)+len(args)); err != nil {
				return "", err
			}
			return g.spend(fmt.Sprint(args...))
		},
		"println": func(args ...any) (string, error) {
			if err := g.allow("println", argsLen(args)+len(args)+1); err != nil {
				return "", err
			}
			return g.spend(fmt.Sprintln(args...))
		},
		"html":     escaper("html", template.HTMLEscaper),
		"js":       escaper("js", template.JSEscaper),
		"urlquery": escaper("urlquery", template.URLQueryEscaper),
	}
}

// argLen is an upper bound of the bytes fmt writes for a with %v, %s or %d.
// Template data holds strings and small values; anything else is measured
// by printing it, which is no larger than the data it prints.
func argLen(a any) int {
	switch x := a.(type) {
	case nil:
		return len("<nil>")
	case string:
		return len(x)
	case []byte:
		return 4 * len(x) // %v writes [1 2 3]
	default:
		return len(fmt.Sprintf("%#v", x)) + len(fmt.Sprint(x))
	}
}

func argsLen(args []any) int {
	n := 0
	for _, a := range args {
		n += argLen(a)
	}
	return n
}

// printfBound is an upper bound of the length of fmt.Sprintf(format, args...),
// found by walking the format as fmt does: each verb writes its argument
// (at most 4 bytes per byte, for %q and %x) or its width, whichever is more.
// An argument may be printed by several verbs ("%[1]s%[1]s"), and each counts.
func printfBound(format string, args []any) int {
	const slack = 64 // signs, prefixes, quotes, %!verb(…) notes
	n, arg := 0, 0
	number := func(i int) (int, int) { // digits from i: value (capped) and end
		v := 0
		for i < len(format) && format[i] >= '0' && format[i] <= '9' {
			if v < 1<<30 {
				v = v*10 + int(format[i]-'0')
			}
			i++
		}
		return v, i
	}
	index := func(i int) int { // an explicit argument index "[n]" from i
		if i < len(format) && format[i] == '[' {
			if v, j := number(i + 1); j < len(format) && format[j] == ']' {
				arg = v - 1
				return j + 1
			}
		}
		return i
	}
	starArg := func() int { // a width or precision taken from an argument
		w := 0
		if arg >= 0 && arg < len(args) {
			switch x := args[arg].(type) {
			case int:
				w = x
			case int64:
				w = int(x)
			default:
				w = argLen(x) // not an int: fmt prints a note instead
			}
		}
		arg++
		if w < 0 {
			w = -w
		}
		return w
	}
	for i := 0; i < len(format); {
		if format[i] != '%' {
			_, size := utf8.DecodeRuneInString(format[i:])
			n, i = n+size, i+size
			continue
		}
		i++
		if i < len(format) && format[i] == '%' {
			n, i = n+1, i+1
			continue
		}
		for i < len(format) && strings.IndexByte("+-# 0", format[i]) >= 0 {
			i++
		}
		i = index(i)
		width := 0
		if i < len(format) && format[i] == '*' {
			width, i = starArg(), i+1
		} else {
			width, i = number(i)
		}
		if i < len(format) && format[i] == '.' {
			i = index(i + 1)
			if i < len(format) && format[i] == '*' {
				width, i = max(width, starArg()), i+1 // a precision pads numbers too
			} else {
				var p int
				p, i = number(i)
				width = max(width, p)
			}
		}
		i = index(i)
		if i >= len(format) {
			n += slack
			break
		}
		verb, size := utf8.DecodeRuneInString(format[i:])
		i += size
		out := slack
		if arg >= 0 && arg < len(args) {
			l := argLen(args[arg])
			if strings.ContainsRune("qxX", verb) {
				l *= 4
			}
			out += l
		}
		arg++
		n += max(out, width+slack)
	}
	return n
}
