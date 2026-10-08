package vault

import (
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
)

func matcherFor(values ...string) *matcher {
	raw := make([][]byte, len(values))
	for i, v := range values {
		raw[i] = []byte(v)
	}
	return newMatcher(raw)
}

func TestRedactOverlappingValues(t *testing.T) {
	for _, tc := range []struct {
		values   []string
		in, want string
	}{
		{[]string{"abc", "abcdef"}, "x abcdef abc", "x [REDACTED] [REDACTED]"},
		{[]string{"abcdef", "abc"}, "x abcdef abc", "x [REDACTED] [REDACTED]"},
		{[]string{"abcd", "cdef"}, "xabcdefx", "x[REDACTED]x"},
		{[]string{"cdef", "abcd"}, "xabcdefx", "x[REDACTED]x"},
		{[]string{"bc", "abcde"}, "abcabcde", "a[REDACTED]"},
		{[]string{"aa"}, "aaa", "[REDACTED]"},
		{[]string{"tok"}, "no secrets", "no secrets"},
		{nil, "nothing to match", "nothing to match"},
		{[]string{"é"}, "café", "caf[REDACTED]"},
	} {
		if got := matcherFor(tc.values...).redact(tc.in); got != tc.want {
			t.Errorf("%q over %q = %q, want %q", tc.values, tc.in, got, tc.want)
		}
	}
}

// naiveRedact marks every byte inside any occurrence of any value, then
// replaces each run of marked bytes.
func naiveRedact(values []string, text string) string {
	covered := make([]bool, len(text))
	for _, v := range values {
		for i := 0; i+len(v) <= len(text); i++ {
			if text[i:i+len(v)] == v {
				for j := i; j < i+len(v); j++ {
					covered[j] = true
				}
			}
		}
	}
	var out strings.Builder
	for i := 0; i < len(text); i++ {
		if !covered[i] {
			out.WriteByte(text[i])
		} else if i == 0 || !covered[i-1] {
			out.WriteString("[REDACTED]")
		}
	}
	return out.String()
}

func TestRedactMatchesNaive(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	word := func(n int) string {
		b := make([]byte, n)
		for i := range b {
			b[i] = "abc"[r.IntN(3)]
		}
		return string(b)
	}
	for i := range 2000 {
		values := make([]string, r.IntN(6))
		for j := range values {
			values[j] = word(2 + r.IntN(5))
		}
		text := word(r.IntN(40))
		want := naiveRedact(values, text)
		if got := matcherFor(values...).redact(text); got != want {
			t.Fatalf("case %d: matcher: %q over %q = %q, want %q", i, values, text, got, want)
		}
		covered := make([]bool, len(text))
		for _, v := range values {
			cover(covered, text, []byte(v))
		}
		if got := redactCovered(text, covered); got != want {
			t.Fatalf("case %d: cover: %q over %q = %q, want %q", i, values, text, got, want)
		}
	}
}

func BenchmarkRedact(b *testing.B) {
	values := make([]string, 2000)
	for i := range values {
		values[i] = fmt.Sprintf("secret-%04d-%x", i, i*7919)
	}
	m := matcherFor(values...)
	var log strings.Builder
	for i := 0; log.Len() < 500*120; i++ {
		fmt.Fprintf(&log, "2026-10-08T12:00:00Z GET /api/items/%d 200 token=secret-%04d-%x\n", i, i%2000, (i%2000)*7919)
	}
	text := log.String()
	b.SetBytes(int64(len(text)))
	for b.Loop() {
		m.redact(text)
	}
}
