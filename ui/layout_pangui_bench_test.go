package ui

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The PanGUI layout benchmarks lay out the workloads of PanGUI's layout
// benchmarks (testdata/pangui), which Taffy keeps and "Are we layout yet?"
// (arewelayoutyet.mespin.me) runs with Rust's layout engines. Each is
// translated into flexbox as Taffy's benchmark translates it
// (benches/pangui.rs), and laid out from scratch each time with the tree
// already built, as the retained-tree engines are timed there.

// panguiSize is a template's size: Pixels, Fit, Expand, Percentage or
// Ratio, with its value.
type panguiSize struct {
	mode  string
	value float32
}

// panguiNode is a node of a template.
type panguiNode struct {
	width, height, minW, minH, maxW, maxH *panguiSize
	gap                                   float32
	pad, margin                           [4]float32 // top, right, bottom, left
	wrap, row                             bool
	children                              []*panguiNode
}

func (n *panguiNode) count() int {
	c := 1
	for _, ch := range n.children {
		c += ch.count()
	}
	return c
}

// panguiParser reads the template format: `# Input`, then a node, its
// properties joined by dots and its children in braces, where
// `Repeat(n) { ... }` repeats children.
type panguiParser struct {
	s   string
	pos int
}

func (p *panguiParser) skip() {
	for p.pos < len(p.s) {
		switch {
		case strings.ContainsRune(" \t\r\n", rune(p.s[p.pos])):
			p.pos++
		case strings.HasPrefix(p.s[p.pos:], "//"):
			for p.pos < len(p.s) && p.s[p.pos] != '\n' {
				p.pos++
			}
		default:
			return
		}
	}
}

func (p *panguiParser) peek() byte {
	p.skip()
	if p.pos < len(p.s) {
		return p.s[p.pos]
	}
	return 0
}

func (p *panguiParser) expect(c byte) {
	if p.peek() != c {
		panic(fmt.Sprintf("offset %d: expected %q", p.pos, c))
	}
	p.pos++
}

func (p *panguiParser) ident() string {
	p.skip()
	start := p.pos
	for p.pos < len(p.s) && (p.s[p.pos] >= 'A' && p.s[p.pos] <= 'Z' || p.s[p.pos] >= 'a' && p.s[p.pos] <= 'z') {
		p.pos++
	}
	return p.s[start:p.pos]
}

func (p *panguiParser) number() float32 {
	p.skip()
	start := p.pos
	for p.pos < len(p.s) && strings.IndexByte("-.0123456789", p.s[p.pos]) >= 0 {
		p.pos++
	}
	v, err := strconv.ParseFloat(p.s[start:p.pos], 32)
	if err != nil {
		panic(fmt.Sprintf("offset %d: %v", start, err))
	}
	return float32(v)
}

// size reads a size; a bare number is Pixels, and Fit and Expand weigh 1
// without one.
func (p *panguiParser) size() *panguiSize {
	if c := p.peek(); c == '-' || c == '.' || c >= '0' && c <= '9' {
		return &panguiSize{"Pixels", p.number()}
	}
	s := &panguiSize{mode: p.ident(), value: 1}
	if p.peek() == '(' {
		p.pos++
		s.value = p.number()
		p.expect(')')
	}
	return s
}

func (p *panguiParser) node() *panguiNode {
	n := &panguiNode{}
	for p.peek() != '{' {
		name := p.ident()
		p.expect('(')
		switch name {
		case "Width":
			n.width = p.size()
		case "Height":
			n.height = p.size()
		case "MinWidth":
			n.minW = p.size()
		case "MinHeight":
			n.minH = p.size()
		case "MaxWidth":
			n.maxW = p.size()
		case "MaxHeight":
			n.maxH = p.size()
		case "Gap":
			n.gap = p.number()
		case "Padding", "PaddingX", "PaddingY", "Margin", "MarginX", "MarginY":
			v := p.number()
			edges := &n.pad
			if strings.HasPrefix(name, "Margin") {
				edges = &n.margin
			}
			if !strings.HasSuffix(name, "X") {
				edges[0], edges[2] = v, v
			}
			if !strings.HasSuffix(name, "Y") {
				edges[1], edges[3] = v, v
			}
		case "Wrap":
			n.wrap = p.ident() == "Auto"
		case "Horizontal":
			n.row = true
		case "Vertical":
			n.row = false
		default:
			panic(fmt.Sprintf("offset %d: unknown property %s", p.pos, name))
		}
		p.expect(')')
		if p.peek() == '.' {
			p.pos++
		}
	}
	p.pos++
	n.children = p.children()
	p.expect('}')
	return n
}

func (p *panguiParser) children() []*panguiNode {
	var out []*panguiNode
	for c := p.peek(); c != '}' && c != 0; c = p.peek() {
		if !strings.HasPrefix(p.s[p.pos:], "Repeat") {
			out = append(out, p.node())
			continue
		}
		p.ident()
		p.expect('(')
		times := int(p.number())
		p.expect(')')
		p.expect('{')
		group := p.children()
		p.expect('}')
		for range times {
			out = append(out, group...)
		}
	}
	return out
}

func parsePanGUI(text string) *panguiNode {
	p := &panguiParser{s: text}
	p.expect('#')
	if p.ident() != "Input" {
		panic("expected # Input")
	}
	return p.node()
}

type panguiBench struct {
	name string
	root *panguiNode
}

// panguiBenches reads the templates, in the order of their names.
func panguiBenches(tb testing.TB) []panguiBench {
	files, err := filepath.Glob(filepath.Join("testdata", "pangui", "*.txt"))
	if err != nil || len(files) == 0 {
		tb.Fatal("no templates", err)
	}
	var out []panguiBench
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			tb.Fatal(err)
		}
		out = append(out, panguiBench{strings.TrimSuffix(filepath.Base(f), ".txt"), parsePanGUI(string(b))})
	}
	return out
}

func (s *panguiSize) is(mode string) bool { return s != nil && s.mode == mode }

// panguiLength is a size as a width or height: Fit, Expand and Ratio are
// automatic, Expand growing or stretching and Ratio an aspect ratio.
func panguiLength(s *panguiSize) length {
	switch {
	case s.is("Pixels"):
		return px(s.value)
	case s.is("Percentage"):
		return percent(s.value * 100)
	}
	return length{}
}

// panguiStyle styles an element as Taffy styles the node: an Expand along
// the parent's main axis is `flex: <weight> 1 0` with no minimum, an
// Expand minimum is `flex: <weight> 0 auto`, and an Expand across the axis
// stretches, as every box stretches its children.
func panguiStyle(e *node, n *panguiNode, parentRow bool) {
	mainSize, mainMin := n.height, n.minH
	if parentRow {
		mainSize, mainMin = n.width, n.minW
	}
	e.row, e.wrap = n.row, n.wrap
	e.align, e.justify, e.alignContent = Stretch, Start, Start
	e.width, e.height = panguiLength(n.width), panguiLength(n.height)
	e.maxW, e.maxH = panguiLength(n.maxW), panguiLength(n.maxH)
	if !n.minW.is("Expand") {
		e.minW = panguiLength(n.minW)
	}
	if !n.minH.is("Expand") {
		e.minH = panguiLength(n.minH)
	}
	switch {
	case mainSize.is("Expand"):
		e.grow, e.shrink, e.basis = mainSize.value, 1, px(0)
		if parentRow && n.minW == nil {
			e.minW = px(0)
		}
		if !parentRow && n.minH == nil {
			e.minH = px(0)
		}
	case mainMin.is("Expand"):
		e.grow, e.shrink = mainMin.value, 0
	}
	switch {
	case n.width.is("Ratio"):
		e.aspect = n.width.value
	case n.height.is("Ratio"):
		e.aspect = 1 / n.height.value
	}
	e.gapX, e.gapY = n.gap, n.gap
	e.pad, e.margin = n.pad, n.margin
}

// panguiRootSize is the size the root is laid out in along an axis: its
// own in pixels, or 1000 as Taffy's.
func panguiRootSize(s *panguiSize) float32 {
	if s.is("Pixels") {
		return s.value
	}
	return 1000
}

// buildPanGUI makes the elements of a template inside a column the size
// the root is laid out in, which Taffy's root is a child of, and returns
// that column and the template's root.
func buildPanGUI(root *panguiNode) (*Tester, *node, *node) {
	var outer, top *node
	var add func(c *context, n *panguiNode, parentRow bool) *node
	add = func(c *context, n *panguiNode, parentRow bool) *node {
		e := coreBox(c)
		panguiStyle(e, n, parentRow)
		if len(n.children) > 0 {
			e.Children(func() {
				for _, ch := range n.children {
					add(c, ch, n.row)
				}
			})
		}
		return e
	}
	w, h := panguiRootSize(root.width), panguiRootSize(root.height)
	view := func(c *context) {
		outer = coreBox(c).AlignItems(Stretch).Size(w, h).Children(func() {
			top = add(c, root, false)
		})
	}
	tt := coreNewTester(view, int(w), int(h))
	return tt, outer, top
}

// BenchmarkLayoutPanGUI lays out the 13 workloads of PanGUI's layout
// benchmarks, as arewelayoutyet.mespin.me times Rust's layout engines:
// the tree built once, laid out from scratch each time. Each reports
// ns/node, over the template's nodes with its root as the site counts
// them.
func BenchmarkLayoutPanGUI(b *testing.B) {
	for _, pb := range panguiBenches(b) {
		b.Run(pb.name, func(b *testing.B) {
			_, outer, _ := buildPanGUI(pb.root)
			n := pb.root.count()
			b.ResetTimer()
			for b.Loop() {
				forgetLayout(outer)
				layoutBox(outer, outer.width.v, outer.height.v)
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/float64(n), "ns/node")
		})
	}
}

// TestLayoutPanGUI checks that the workloads parse into the trees
// arewelayoutyet counts and lay out as flexbox lays them out.
func TestLayoutPanGUI(t *testing.T) {
	nodes := map[string]int{
		"expand_with_max_constraint":        3001,
		"expand_with_min_constraint":        3001,
		"fit_nesting":                       101111,
		"flex_expand_equal_weights":         15001,
		"flex_expand_weights":               15001,
		"nested_vertical_stack":             10001,
		"padding_and_margin":                101,
		"percentage_and_ratio":              10001,
		"perpendicular_expand_with_wrap":    12001,
		"pixels_with_min_expand_constraint": 30001,
		"wide_no_wrap_simple_few":           1001,
		"wide_no_wrap_simple_many":          100001,
		"wide_wrapping":                     10001,
	}
	type box struct{ x, y, w, h float32 }
	at := func(e *node) box { return box{e.x, e.y, e.w, e.h} }
	nth := func(e *node, i int) *node {
		c := e.first
		for range i {
			c = c.next
		}
		return c
	}
	for _, pb := range panguiBenches(t) {
		if got := pb.root.count(); got != nodes[pb.name] {
			t.Errorf("%s: %d nodes, want %d", pb.name, got, nodes[pb.name])
		}
		_, outer, root := buildPanGUI(pb.root)
		layoutBox(outer, outer.width.v, outer.height.v)
		var checks []struct {
			e    *node
			want box
		}
		check := func(e *node, want box) {
			checks = append(checks, struct {
				e    *node
				want box
			}{e, want})
		}
		switch pb.name {
		case "expand_with_max_constraint":
			// The second child stops at 40 and the first takes the rest.
			check(root, box{0, 0, 100, 10})
			check(nth(root.first, 0), box{0, 0, 60, 10})
			check(nth(root.first, 1), box{60, 0, 40, 10})
		case "expand_with_min_constraint":
			check(nth(root.first, 0), box{0, 0, 40, 10})
			check(nth(root.first, 1), box{40, 0, 60, 10})
		case "fit_nesting":
			// 10 rows of 10 columns of 10 rows of leaves 10 high.
			check(root, box{0, 0, 1000, 1010})
			check(root.first, box{5, 5, 990, 100})
			check(root.first.first.first.first, box{0, 0, 0.99, 10})
		case "flex_expand_equal_weights":
			check(root.first, box{0, 0, float32(10000) / 15000, 100})
		case "flex_expand_weights":
			// The boxes of 100 overflow, not shrinking, and share
			// theirs out 1:2.
			check(nth(root, 1), box{100, 0, 100, 100})
			check(nth(root.first, 1), box{100.0 / 3, 0, 200.0 / 3, 100})
		case "nested_vertical_stack":
			check(root, box{0, 0, 200, 20 + 10000 + 9999*5})
			check(nth(root, 1), box{10, 16, 180, 1})
		case "padding_and_margin":
			check(root, box{0, 0, 1000, 40 + 100*24 + 99*10})
			check(root.first, box{22, 22, 956, 20})
		case "percentage_and_ratio":
			// Half as wide as the column and half as high as wide, not
			// shrinking.
			check(nth(root, 1), box{0, 250, 500, 250})
		case "perpendicular_expand_with_wrap":
			// Lines of 10+1+20+1+30+1 and 40+1+50+1, the lines of 1
			// stretching to their line's tallest.
			check(nth(root, 1), box{10, 0, 1, 30})
			check(nth(root, 6), box{0, 30, 40, 40})
			check(nth(root, 9), box{91, 30, 1, 50})
			check(nth(root, 10), box{0, 80, 60, 60})
		case "pixels_with_min_expand_constraint":
			check(nth(root.first, 1), box{50, 0, 50, 10})
		case "wide_no_wrap_simple_few", "wide_no_wrap_simple_many":
			check(nth(root, 3), box{30, 0, 10, 10})
		case "wide_wrapping":
			check(root, box{0, 0, 100, 10000})
			check(nth(root, 13), box{30, 10, 10, 10})
		}
		for _, c := range checks {
			if got := at(c.e); got != c.want {
				t.Errorf("%s: %+v, want %+v", pb.name, got, c.want)
			}
		}
	}
}
