// Copyright ©2014 The bíogo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// victore is a post processor for grouping families defined by igor.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"os"
	"sort"

	"github.com/biogo/biogo/io/featio/gff"
	"github.com/biogo/biogo/seq"
	"github.com/biogo/store/step"

	"github.com/gonum/graph"
	"github.com/gonum/graph/concrete"
	"github.com/gonum/graph/encoding/dot"
	"github.com/gonum/graph/network"
	"github.com/gonum/graph/search"
)

type family struct {
	id      int
	members []feature
	length  int
}

type byMembers []family

func (f byMembers) Len() int           { return len(f) }
func (f byMembers) Less(i, j int) bool { return len(f[i].members) > len(f[j].members) }
func (f byMembers) Swap(i, j int)      { f[i], f[j] = f[j], f[i] }

type feature struct {
	Chr    string     `json:"C"`
	Start  int        `json:"S"`
	End    int        `json:"E"`
	Orient seq.Strand `json:"O"`
}

// stepBool is a bool type satisfying the step.Equaler interface.
type stepBool bool

// Equal returns whether b equals e. Equal assumes the underlying type of e is a stepBool.
func (b stepBool) Equal(e step.Equaler) bool {
	return b == e.(stepBool)
}

var (
	in     = flag.String("in", "", "Specifies the input json file name.")
	dotOut = flag.String("dot", "", "Specifies the output DOT file name.")
	thresh = flag.Float64("thresh", 0.1, "Specifies minimum family intersection to report.")
)

func main() {
	flag.Parse()
	if *in == "" {
		flag.Usage()
		os.Exit(0)
	}

	f, err := os.Open(*in)
	if err != nil {
		log.Fatalf("failed reading %q: %v", *in, err)
	}
	defer f.Close()
	r := bufio.NewReader(f)

	var families []family
	for i := 0; ; i++ {
		l, err := r.ReadBytes('\n')
		if err != nil {
			break
		}
		var v []feature
		err = json.Unmarshal(l, &v)
		if err != nil {
			log.Fatalf("failed unmarshaling json for family %d: %v", i, err)
		}
		fam := family{id: i, members: v, length: length(v)}

		families = append(families, fam)
	}
	sort.Sort(byMembers(families))

	var edges []edge
	for i, a := range families[:len(families)-1] {
		for _, b := range families[i+1:] {
			upper, lower := intersection(a, b)
			if upper < *thresh {
				continue
			}

			aid, bid := a.id, b.id
			if a.length > b.length {
				aid, bid = bid, aid
			}

			fmt.Fprintln(os.Stderr, aid, bid, upper)
			edges = append(edges, edge{
				from:   concrete.Node(aid),
				to:     concrete.Node(bid),
				weight: upper,
			})

			if lower < *thresh {
				continue
			}

			fmt.Fprintln(os.Stderr, bid, aid, lower)
			edges = append(edges, edge{
				from:   concrete.Node(bid),
				to:     concrete.Node(aid),
				weight: lower,
			})
		}
	}

	if *dotOut != "" {
		writeDOT(*dotOut, edges)
	}

	const minSubClique = 3
	grps := groups(families, edges, minSubClique)

	clusterIdentity := make(map[int]int)
	cliqueIdentity := make(map[int][]int)
	cliqueMemberships := make(map[int]int)
	for _, g := range grps {
		fmt.Fprintf(os.Stderr, "clique=%t", g.isClique)
		for _, m := range g.members {
			fmt.Fprintf(os.Stderr, " %d", m.id)
			clusterIdentity[m.id] = g.pageRank[0].id
			if g.isClique {
				cliqueMemberships[m.id]++
				cliqueIdentity[m.id] = []int{g.pageRank[0].id}
			}
		}
		if len(g.cliques) != 0 {
			fmt.Fprintf(os.Stderr, " (%d+)-cliquesIn=%v", minSubClique, g.cliques)
		}
		// Collate counts for clique memberships. We can do this
		// one group at a time; a member of a group cannot be a
		// clique member of another group since they are in
		// separate connected components.
		for _, clique := range g.cliques {
			for _, m := range clique {
				cliqueMemberships[m]++
			}
		}
		for _, clique := range g.cliques {
			// Make PageRanked version of clique.
			cliqueHas := make(map[int]bool)
			for _, m := range clique {
				cliqueHas[m] = true
			}
			clique = make([]int, 0, len(clique))
			for _, m := range g.pageRank {
				if cliqueHas[m.id] {
					clique = append(clique, m.id)
				}
			}

			// Annotate families as meaningfully but concisely as possible.
			unique := cliqueMemberships[clique[0]] == 1
			for i, m := range clique {
				if cliqueMemberships[m] == 1 {
					if unique {
						cliqueIdentity[m] = clique[:1]
					} else {
						cliqueIdentity[m] = clique
					}
				} else {
					cliqueIdentity[m] = clique[i : i+1]
				}
			}
		}
		fmt.Fprintf(os.Stderr, " PageRank=%+v\n", g.pageRank)
	}

	b := bufio.NewWriter(os.Stdout)
	defer b.Flush()
	w := gff.NewWriter(b, 60, false)
	ft := &gff.Feature{
		Source:  "igor/victor",
		Feature: "repeat",
		FeatAttributes: gff.Attributes{
			{Tag: "Family"},
			{Tag: "Cluster"},
			{Tag: "Clique"},
		},
	}
	for _, fam := range families {
		clustID, isClustered := clusterIdentity[fam.id]
		for _, m := range fam.members {
			ft.SeqName = m.Chr
			ft.FeatStart = m.Start
			ft.FeatEnd = m.End
			ft.FeatStrand = m.Orient
			ft.FeatFrame = gff.NoFrame
			ft.FeatAttributes[0].Value = fmt.Sprint(fam.id)
			if isClustered {
				ft.FeatAttributes = ft.FeatAttributes[:3]
				ft.FeatAttributes[1].Value = fmt.Sprint(clustID)
				switch id := cliqueIdentity[fam.id]; {
				case id == nil:
					ft.FeatAttributes = ft.FeatAttributes[:2]
				case cliqueMemberships[fam.id] == 1:
					ft.FeatAttributes[2].Value = dotted(id)
				default:
					ft.FeatAttributes[2].Value = fmt.Sprintf("%d*", id[0])
				}
			} else {
				ft.FeatAttributes = ft.FeatAttributes[:1]
			}
			_, err := w.Write(ft)
			if err != nil {
				log.Fatalf("error: %v", err)
			}
		}
	}
}

func dotted(id []int) string {
	var buf bytes.Buffer
	for i, e := range id {
		if i != 0 {
			fmt.Fprint(&buf, ".")
		}
		fmt.Fprint(&buf, e)
	}
	return buf.String()
}

func writeDOT(file string, edges []edge) {
	g := concrete.NewDirectedGraph()
	for _, e := range edges {
		for _, n := range []graph.Node{e.From(), e.To()} {
			if !g.NodeExists(n) {
				g.AddNode(n)
			}
		}
		g.AddDirectedEdge(e, 0)
	}

	f, err := os.Create(*dotOut)
	if err != nil {
		log.Printf("failed to create %q DOT output file: %v", *dotOut, err)
		return
	}
	defer f.Close()
	b, err := dot.Marshal(g, "", "", "  ", false)
	if err != nil {
		log.Printf("failed to create DOT bytes: %v", err)
		return
	}
	_, err = f.Write(b)
	if err != nil {
		log.Printf("failed to write DOT: %v", err)
	}
}

// pair is a [2]bool type satisfying the step.Equaler interface.
type pair [2]bool

// Equal returns whether p equals e. Equal assumes the underlying type of e is pair.
func (p pair) Equal(e step.Equaler) bool {
	return p == e.(pair)
}

func length(v []feature) int {
	vecs := make(map[string]*step.Vector)
	for _, f := range v {
		vec, ok := vecs[f.Chr]
		if !ok {
			var err error
			vec, err = step.New(f.Start, f.End, stepBool(false))
			if err != nil {
				panic(err)
			}
			vec.Relaxed = true
			vecs[f.Chr] = vec
		}
		vec.SetRange(f.Start, f.End, stepBool(true))
	}
	var len int
	for _, vec := range vecs {
		vec.Do(func(start, end int, e step.Equaler) {
			if e.(stepBool) {
				len += end - start
			}
		})
	}
	return len
}

func intersection(a, b family) (upper, lower float64) {
	// TODO(kortschak): Consider orientation agreement.
	vecs := make(map[string]*step.Vector)
	for i, v := range []family{a, b} {
		for _, f := range v.members {
			vec, ok := vecs[f.Chr]
			if !ok {
				var err error
				vec, err = step.New(f.Start, f.End, pair{})
				if err != nil {
					panic(err)
				}
				vec.Relaxed = true
				vecs[f.Chr] = vec
			}
			err := vec.ApplyRange(f.Start, f.End, func(e step.Equaler) step.Equaler {
				p := e.(pair)
				p[i] = true
				return p
			})
			if err != nil {
				panic(err)
			}
		}
	}
	var (
		aLen, bLen int
		intersect  int
	)
	for _, vec := range vecs {
		vec.Do(func(start, end int, e step.Equaler) {
			p := e.(pair)
			if p[0] {
				aLen += end - start
			}
			if p[1] {
				bLen += end - start
			}
			if p[0] && p[1] {
				intersect += end - start
			}
		})
	}
	if aLen != a.length || bLen != b.length {
		panic("length mismatch")
	}

	upper = float64(intersect) / math.Min(float64(a.length), float64(b.length))
	lower = float64(intersect) / math.Max(float64(a.length), float64(b.length))
	return upper, lower
}

type group struct {
	members  []family
	isClique bool
	cliques  [][]int
	pageRank ranks
}

type edge struct {
	from, to graph.Node
	weight   float64
}

var _ dot.Attributer = edge{}

func (e edge) From() graph.Node { return e.from }
func (e edge) To() graph.Node   { return e.to }
func (e edge) DOTAttributes() []dot.Attribute {
	return []dot.Attribute{{"weight", fmt.Sprint(e.weight)}}
}

func groups(fams []family, edges []edge, minSubClique int) []group {
	g := concrete.NewGraph()
	for _, e := range edges {
		for _, n := range []graph.Node{e.From(), e.To()} {
			if !g.NodeExists(n) {
				g.AddNode(n)
			}
		}
		g.AddUndirectedEdge(e, 0)
	}

	ltable := make(map[int]int, len(fams))
	for i, f := range fams {
		ltable[f.id] = i
	}
	var grps []group
	cc := search.ConnectedComponents(g)
	for _, c := range cc {
		var grp group
		for _, n := range c {
			grp.members = append(grp.members, fams[ltable[n.ID()]])
		}
		if len(grp.members) == 2 || edgesIn(g, c)*2 == len(c)*(len(c)-1) {
			grp.isClique = true
		} else {
			grp.cliques = cliquesIn(grp, edges, minSubClique)
		}
		if len(grp.members) > 1 {
			grp.pageRank = ranksOf(grp, edges)
		}

		grps = append(grps, grp)
	}

	return grps
}

func edgesIn(g graph.Graph, n []graph.Node) int {
	e := make(map[[2]int]struct{})
	for _, u := range n {
		for _, v := range g.Neighbors(u) {
			if u.ID() < v.ID() {
				e[[2]int{u.ID(), v.ID()}] = struct{}{}
			}
		}
	}
	return len(e)
}

func cliquesIn(grp group, edges []edge, min int) [][]int {
	isMember := make(map[int]struct{})
	for _, fam := range grp.members {
		isMember[fam.id] = struct{}{}
	}

	g := concrete.NewGraph()
outer:
	for _, e := range edges {
		for _, n := range []graph.Node{e.From(), e.To()} {
			_, ok := isMember[n.ID()]
			if !ok {
				continue outer
			}
		}
		for _, n := range []graph.Node{e.From(), e.To()} {
			if !g.NodeExists(n) {
				g.AddNode(n)
			}
		}
		g.AddUndirectedEdge(e, 0)
	}

	clqs := search.BronKerbosch(g)
	var cliqueIDs [][]int
	for _, clq := range clqs {
		if len(clq) < min {
			continue
		}
		ids := make([]int, 0, len(clq))
		for _, n := range clq {
			ids = append(ids, n.ID())
		}
		cliqueIDs = append(cliqueIDs, ids)
	}

	return cliqueIDs
}

func ranksOf(grp group, edges []edge) ranks {
	isMember := make(map[int]struct{})
	for _, fam := range grp.members {
		isMember[fam.id] = struct{}{}
	}

	g := concrete.NewDirectedGraph()
outer:
	for _, e := range edges {
		for _, n := range []graph.Node{e.From(), e.To()} {
			_, ok := isMember[n.ID()]
			if !ok {
				continue outer
			}
		}
		for _, n := range []graph.Node{e.From(), e.To()} {
			if !g.NodeExists(n) {
				g.AddNode(n)
			}
		}
		g.AddDirectedEdge(e, 0)
	}

	r := network.PageRank(g, 0.85, 1e-6)
	o := make(ranks, 0, len(r))
	for id, rnk := range r {
		o = append(o, rank{id: id, rank: rnk})
	}
	sort.Sort(o)
	return o
}

type rank struct {
	id   int
	rank float64
}

type ranks []rank

func (o ranks) Len() int { return len(o) }
func (o ranks) Less(i, j int) bool {
	return o[i].rank > o[j].rank || (o[i].rank == o[j].rank && o[i].id < o[j].id)
}
func (o ranks) Swap(i, j int) { o[i], o[j] = o[j], o[i] }
