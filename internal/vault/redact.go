package vault

import "strings"

// matcher finds every occurrence of a set of values in one pass over a text
// (Aho–Corasick) and redacts the union of their spans, so a value that
// overlaps or contains another never leaves part of itself behind.
type matcher struct {
	root  [256]int32 // root's children by byte; -1 when absent
	nodes []node
}

type node struct {
	edges []edge
	fail  int32
	// longest is the length of the longest value that ends at this node or
	// at any node on its fail chain; 0 when none does.
	longest int32
}

type edge struct {
	b  byte
	to int32
}

func newMatcher(values [][]byte) *matcher {
	m := &matcher{nodes: []node{{}}}
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
				c = int32(len(m.nodes))
				m.nodes = append(m.nodes, node{})
				if n == 0 {
					m.root[b] = c
				} else {
					m.nodes[n].edges = append(m.nodes[n].edges, edge{b, c})
				}
			}
			n = c
		}
		m.nodes[n].longest = int32(len(v))
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
		for _, e := range m.nodes[n].edges {
			f := m.nodes[n].fail
			for f != 0 && m.child(f, e.b) < 0 {
				f = m.nodes[f].fail
			}
			fail := max(m.child(f, e.b), 0)
			m.nodes[e.to].fail = fail
			m.nodes[e.to].longest = max(m.nodes[e.to].longest, m.nodes[fail].longest)
			queue = append(queue, e.to)
		}
	}
	return m
}

func (m *matcher) child(n int32, b byte) int32 {
	if n == 0 {
		return m.root[b]
	}
	for _, e := range m.nodes[n].edges {
		if e.b == b {
			return e.to
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
			n = m.nodes[n].fail
		}
		l := int(m.nodes[n].longest)
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
