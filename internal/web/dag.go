package web

import (
	"slices"
	"strings"
)

// The workflow graph is laid out here, in Go, and drawn as inline SVG by
// the template: Sugiyama-lite. A task's layer (column, left to right) is
// the longest path to it from a root; an edge that spans several layers
// passes through a dummy slot in each layer between, so it never runs
// under a task; the tasks of a layer are ordered by the barycenter of
// their neighbours' positions, in a few sweeps down and up.

// Graph geometry, in SVG units (CSS px at zoom 1).
const (
	dagNodeW  = 184 // a task's box
	dagNodeH  = 56
	dagGapX   = 64 // between columns, where edges bend
	dagGapY   = 30 // between tasks of a column; an undo badge hangs into it
	dagDummyH = 10 // the slot of an edge passing through a column
	dagPad    = 20 // around the graph; the bottom one holds a last undo badge
	dagSweeps = 4  // ordering sweeps: down, up, down, up
	// dagMaxSlots bounds tasks plus dummies: a graph past it is not drawn
	// (the table lists the tasks), so a huge record cannot eat the server.
	dagMaxSlots = 5000
)

// dagLayout is where each task goes. Ids 0..n-1 are the tasks; ids from n
// are dummies, the slots of long edges.
type dagLayout struct {
	n      int
	layer  []int     // per id
	order  [][]int   // per layer: ids, top to bottom
	y      []float64 // per id: its centre
	chains [][]int   // per drawn edge: ids from its parent, through dummies, to its child
	width  float64
	height float64
}

// layoutDAG lays out n tasks; deps[i] are the indexes of task i's
// dependencies. Out-of-range and self deps are ignored, so is a dep that
// closes a cycle (varmq refuses cyclic plans; this only keeps a bad
// record from looping): it gets no edge. ok is false, and nothing laid
// out, when tasks and dummy slots would pass dagMaxSlots.
func layoutDAG(n int, deps [][]int) (l dagLayout, ok bool) {
	if n > dagMaxSlots {
		return dagLayout{}, false
	}
	l = dagLayout{n: n, layer: make([]int, n)}

	// Layers: the longest path from a root. state 1: on the DFS stack.
	state := make([]int8, n)
	var depth func(i int) int
	depth = func(i int) int {
		switch state[i] {
		case 1:
			return -1 // a cycle: this dep is left out
		case 2:
			return l.layer[i]
		}
		state[i] = 1
		d := 0
		for _, p := range validDeps(i, n, deps) {
			if pd := depth(p); pd >= 0 {
				d = max(d, pd+1)
			}
		}
		state[i], l.layer[i] = 2, d
		return d
	}
	layers := 0
	for i := range n {
		layers = max(layers, depth(i)+1)
	}
	l.order = make([][]int, layers)
	for i := range n {
		l.order[l.layer[i]] = append(l.order[l.layer[i]], i)
	}

	// Edges, with a dummy in each layer a long one crosses.
	slots := n
	for i := range n {
		for _, p := range validDeps(i, n, deps) {
			slots += max(l.layer[i]-l.layer[p]-1, 0)
		}
	}
	if slots > dagMaxSlots {
		return dagLayout{}, false
	}
	for i := range n {
		for _, p := range validDeps(i, n, deps) {
			if l.layer[p] >= l.layer[i] {
				continue // closes a cycle
			}
			chain := []int{p}
			for ly := l.layer[p] + 1; ly < l.layer[i]; ly++ {
				id := len(l.layer)
				l.layer = append(l.layer, ly)
				l.order[ly] = append(l.order[ly], id)
				chain = append(chain, id)
			}
			l.chains = append(l.chains, append(chain, i))
		}
	}
	pred := make([][]int, len(l.layer))
	succ := make([][]int, len(l.layer))
	for _, c := range l.chains {
		for k := 1; k < len(c); k++ {
			succ[c[k-1]] = append(succ[c[k-1]], c[k])
			pred[c[k]] = append(pred[c[k]], c[k-1])
		}
	}

	l.y = make([]float64, len(l.layer))
	l.place()
	for s := range dagSweeps {
		down := s%2 == 0
		for k := range layers {
			ly, near := k, pred
			if !down {
				ly, near = layers-1-k, succ
			}
			l.sortLayer(ly, near)
		}
	}

	l.width = 2*dagPad + float64(layers)*dagNodeW + float64(max(layers-1, 0))*dagGapX
	return l, true
}

// validDeps are the deps of task i that name another task.
func validDeps(i, n int, deps [][]int) []int {
	if i >= len(deps) {
		return nil
	}
	return slices.DeleteFunc(slices.Clone(deps[i]), func(p int) bool { return p < 0 || p >= n || p == i })
}

// slotH is the height of id's slot in its column.
func (l *dagLayout) slotH(id int) float64 {
	if id < l.n {
		return dagNodeH
	}
	return dagDummyH
}

// place sets every id's centre from the order: each column stacked, centred
// on the tallest. It sets the height too.
func (l *dagLayout) place() {
	heights := make([]float64, len(l.order))
	tallest := 0.0
	for ly, ids := range l.order {
		for k, id := range ids {
			heights[ly] += l.slotH(id)
			if k > 0 {
				heights[ly] += dagGapY
			}
		}
		tallest = max(tallest, heights[ly])
	}
	for ly, ids := range l.order {
		y := dagPad + (tallest-heights[ly])/2
		for _, id := range ids {
			h := l.slotH(id)
			l.y[id] = y + h/2
			y += h + dagGapY
		}
	}
	l.height = 2*dagPad + tallest
}

// sortLayer orders layer ly by the mean centre of each id's near
// neighbours (its parents sweeping down, its children sweeping up), then
// places the graph again. An id with none keeps its own centre; ties keep
// their order.
func (l *dagLayout) sortLayer(ly int, near [][]int) {
	ids := l.order[ly]
	bary := make(map[int]float64, len(ids))
	for _, id := range ids {
		if len(near[id]) == 0 {
			bary[id] = l.y[id]
			continue
		}
		sum := 0.0
		for _, o := range near[id] {
			sum += l.y[o]
		}
		bary[id] = sum / float64(len(near[id]))
	}
	slices.SortStableFunc(ids, func(a, b int) int {
		switch {
		case bary[a] < bary[b]:
			return -1
		case bary[a] > bary[b]:
			return 1
		}
		return 0
	})
	l.place()
}

// x is the left edge of layer ly's column.
func dagX(ly int) float64 { return dagPad + float64(ly)*(dagNodeW+dagGapX) }

// path is the SVG path of chain c: from the right side of its parent,
// straight through each dummy's column, into the left side of its child,
// bending as a horizontal-tangent cubic in every gap.
func (l *dagLayout) path(c []int) string {
	type pt struct{ x, y float64 }
	var pts []pt
	for k, id := range c {
		x, y := dagX(l.layer[id]), l.y[id]
		switch {
		case k == 0:
			pts = append(pts, pt{x + dagNodeW, y})
		case k == len(c)-1:
			pts = append(pts, pt{x - 2, y}) // the arrowhead's tip on the box
		default:
			pts = append(pts, pt{x, y}, pt{x + dagNodeW, y})
		}
	}
	var b strings.Builder
	b.WriteString("M" + num(pts[0].x) + " " + num(pts[0].y))
	for k := 1; k < len(pts); k++ {
		a, z := pts[k-1], pts[k]
		if k%2 == 0 { // through a dummy's column: straight
			b.WriteString(" L" + num(z.x) + " " + num(z.y))
			continue
		}
		mid := (z.x - a.x) / 2
		b.WriteString(" C" + num(a.x+mid) + " " + num(a.y) + " " + num(z.x-mid) + " " + num(z.y) + " " + num(z.x) + " " + num(z.y))
	}
	return b.String()
}
