package vault

import "strings"

// matcher finds every occurrence of a set of values in one pass over a text
// (Aho–Corasick) and redacts the union of their spans, so a value that
// overlaps or contains another never leaves part of itself behind.
//
// Nodes live in flat arrays, about 17 bytes each and one per distinct value
// prefix, with no allocation per node.
type matcher struct {
	root    [256]int32 // root's children by byte; -1 when absent
	label   []byte     // byte on the edge into each node
	first   []int32    // first child, or -1
	sibling []int32    // next child of the same parent, or -1
	fail    []int32
	// longest is the length of the longest value that ends at a node or at
	// any node on its fail chain; 0 when none does.
	longest []int32
}

func newMatcher(values [][]byte) *matcher {
	size := 1
	for _, v := range values {
		size += len(v)
	}
	m := &matcher{
		label:   make([]byte, 1, size),
		first:   append(make([]int32, 0, size), -1),
		sibling: append(make([]int32, 0, size), -1),
		fail:    make([]int32, 1, size),
		longest: make([]int32, 1, size),
	}
	for i := range m.root {
		m.root[i] = -1
	}
	for _, v := range values {
		if len(v) == 0 {
			continue
		}
		n := int32(0)
		for _, b := range v {
			c := m.child(n, b)
			if c < 0 {
				c = int32(len(m.label))
				m.label = append(m.label, b)
				m.first = append(m.first, -1)
				m.fail = append(m.fail, 0)
				m.longest = append(m.longest, 0)
				if n == 0 {
					m.sibling = append(m.sibling, -1)
					m.root[b] = c
				} else {
					m.sibling = append(m.sibling, m.first[n])
					m.first[n] = c
				}
			}
			n = c
		}
		m.longest[n] = int32(len(v))
	}
	// Breadth first, so every fail target is complete before it is used.
	var queue []int32
	for _, c := range m.root {
		if c >= 0 {
			queue = append(queue, c)
		}
	}
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		for c := m.first[n]; c >= 0; c = m.sibling[c] {
			f := m.fail[n]
			for f != 0 && m.child(f, m.label[c]) < 0 {
				f = m.fail[f]
			}
			fail := max(m.child(f, m.label[c]), 0)
			m.fail[c] = fail
			m.longest[c] = max(m.longest[c], m.longest[fail])
			queue = append(queue, c)
		}
	}
	return m
}

func (m *matcher) child(n int32, b byte) int32 {
	if n == 0 {
		return m.root[b]
	}
	for c := m.first[n]; c >= 0; c = m.sibling[c] {
		if m.label[c] == b {
			return c
		}
	}
	return -1
}

// redact replaces each maximal run of bytes covered by any value with
// [REDACTED].
func (m *matcher) redact(text string) string {
	type span struct{ start, end int }
	var spans []span
	n := int32(0)
	for i := 0; i < len(text); i++ {
		for {
			if c := m.child(n, text[i]); c >= 0 {
				n = c
				break
			}
			if n == 0 {
				break
			}
			n = m.fail[n]
		}
		l := int(m.longest[n])
		if l == 0 {
			continue
		}
		// A longer value can reach back over earlier spans; absorb them.
		s := span{i + 1 - l, i + 1}
		for len(spans) > 0 && spans[len(spans)-1].end >= s.start {
			s.start = min(s.start, spans[len(spans)-1].start)
			spans = spans[:len(spans)-1]
		}
		spans = append(spans, s)
	}
	if len(spans) == 0 {
		return text
	}
	var out strings.Builder
	out.Grow(len(text))
	last := 0
	for _, s := range spans {
		out.WriteString(text[last:s.start])
		out.WriteString("[REDACTED]")
		last = s.end
	}
	out.WriteString(text[last:])
	return out.String()
}

// cover marks every byte of text inside any occurrence of value.
func cover(covered []bool, text string, value []byte) {
	if len(value) == 0 {
		return
	}
	marked := 0
	for i := 0; ; i++ {
		j := strings.Index(text[i:], string(value))
		if j < 0 {
			return
		}
		i += j
		for k := max(i, marked); k < i+len(value); k++ {
			covered[k] = true
		}
		marked = i + len(value)
	}
}

// redactCovered replaces each run of covered bytes with [REDACTED].
func redactCovered(text string, covered []bool) string {
	var out strings.Builder
	out.Grow(len(text))
	for i := 0; i < len(text); i++ {
		if !covered[i] {
			out.WriteByte(text[i])
		} else if i == 0 || !covered[i-1] {
			out.WriteString("[REDACTED]")
		}
	}
	return out.String()
}
