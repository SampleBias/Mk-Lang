// Why: each guard of the small-function rule (smallfn.go) and of its effect
// in solecall.go needs a test, or a regression lets a mark through.
// What: tests of wrapped main, inherited cpu sets, sole-effect counts,
// constant-time modules across packs, the mark repeated once, PackFacts'
// callee-loop and small bits, and the small set after appended Funcs.
// How: each test pairs a unit that keeps a mark off with its twin that
// differs only in the guarded property and carries the mark.

package codegen

import (
	"bytes"
	"strings"
	"testing"

	"mk/internal/mir"
	"mk/internal/types"
)

// emitCfg is pack p of u under cfg.
func emitCfg(t *testing.T, e *testEnv, u *Unit, p int, cfg Config) string {
	t.Helper()
	var b bytes.Buffer
	if _, err := EmitPack(u, p, e, cfg, &b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// TestWrappedMainKeptOut: the Main the report wrapper calls
// (Config.Main set, func.go's wrappedMain) stays out of line, so neither
// mark goes on it: not the small mark (a two-operation Main called twice)
// and not the cross-pack mark (a large Main whose one MIR call is in
// another pack; its definition also carries noinline). Without
// Config.Main, the same Main takes each mark.
func TestWrappedMainKeptOut(t *testing.T) {
	wrapped := Config{Target: types.X86_64Linux, Profile: types.Release, RootModule: "main", Main: types.ItemKey("main", "Run")}
	plain := Config{Target: types.X86_64Linux, Profile: types.Release, RootModule: "main"}
	for _, c := range []struct {
		name string
		cfg  Config
		want bool
	}{{"wrapped", wrapped, false}, {"plain", plain, true}} {
		t.Run("small-"+c.name, func(t *testing.T) {
			e := newTestEnv()
			m, l := smallLeaf(e, "Main", 1)
			f := twiceCaller(e, "Caller", m)
			ll := emitCfg(t, e, unitOf(e, []*mir.Body{l.b, f.b}), 0, c.cfg)
			if got := smallMarked(t, ll, "Main"); got != c.want {
				t.Errorf("Main: marked %v, want %v", got, c.want)
			}
		})
		t.Run("cross-"+c.name, func(t *testing.T) {
			e := newTestEnv()
			m, l := soleLeaf(e, "Main", 200, false)
			f := soleCaller(e, "Caller", m, false, 0)
			u := unitOf(e, []*mir.Body{l.b, f.b})
			u.Packs = [][]int{{0}, {1}}
			u.Funcs[1].Pack = 1
			ll := emitCfg(t, e, u, 0, c.cfg)
			if got := smallMarked(t, ll, "Main"); got != c.want {
				t.Errorf("Main: marked %v, want %v: %s", got, c.want, defLine(ll, "mk.main.Main"))
			}
		})
	}
}

// TestSmallInheritedCPU: a generic G called twice from Fast, which
// has a cpu clause, and twice from Base, which has none. Fast calls the
// instance that inherits Fast's features (its key's cpu suffix, cpuOf),
// which stays unmarked although G's own clause is empty; Base's instance
// is small. (An alwaysinline callee skips LLVM's feature check.)
func TestSmallInheritedCPU(t *testing.T) {
	e := newTestEnv()
	g := e.fn("G", []types.TypeID{types.I64}, types.I64, 0)
	ga := []types.GenericArg{types.TypeArg(types.I64)}
	gb := func() *mir.Body {
		f := e.body(g)
		f.assign(place(0), use(mir.Copy(place(1))), f.at(1, 1))
		f.ret(f.at(1, 1))
		return f.b
	}
	caller := func(name string, cpu []string) *fnb {
		it := e.fn(name, []types.TypeID{types.I64}, types.I64, 0)
		e.items.Get(it).Decl.(*types.FuncDecl).Sig.CPU = cpu
		f := e.body(it)
		for range 2 {
			r := f.call(g, ga, types.I64, mir.Copy(place(1)))
			f.assign(place(0), use(mir.Copy(place(r))), f.at(2, 1))
		}
		f.ret(f.at(2, 1))
		return f
	}
	set, _ := types.CPUClosure(types.ArchX64, []string{"avx2"})
	fast, base := caller("Fast", set), caller("Base", nil)
	u := unitOf(e, []*mir.Body{fast.b, base.b})
	key := e.items.Get(g).Key.String() + "@c544"
	u.Packs[0] = append(u.Packs[0], len(u.Funcs), len(u.Funcs)+1)
	u.Funcs = append(u.Funcs,
		Func{Symbol: "mk.main.G.base", Key: key, Item: g, Args: ga, Body: gb()},
		Func{Symbol: "mk.main.G.avx2", Key: key + cpuOpen + "avx,avx2" + cpuClose, Item: g, Args: ga, Body: gb()})
	ll := emit(t, e, u, types.X86_64Linux, types.Release)
	if !strings.Contains(fnText(ll, "mk.main.Fast"), "@mk.main.G.avx2(") {
		t.Fatalf("Fast does not call the inheriting instance:\n%s", fnText(ll, "mk.main.Fast"))
	}
	if smallMarked(t, ll, "G.avx2") {
		t.Errorf("the instance inheriting avx2 is marked: %s", defLine(ll, "mk.main.G.avx2"))
	}
	if !smallMarked(t, ll, "G.base") {
		t.Errorf("the baseline instance is not marked: %s", defLine(ll, "mk.main.G.base"))
	}
	clangC(t, types.X86_64Linux, t.TempDir(), "inherit", ll, "-O2")
}

// sWithSmallCallee is S(n: int): int with pad statements, then two calls
// of small, in e.
func sWithSmallCallee(e *testEnv, small types.ItemID, pad int) *fnb {
	f := e.body(e.fn("S", []types.TypeID{types.Int}, types.Int, 0))
	for range pad {
		f.assign(place(0), use(mir.Copy(place(1))), f.at(2, 3))
	}
	for range 2 {
		v := f.call(small, nil, types.Int, mir.Copy(place(1)))
		f.assign(place(0), use(mir.Copy(place(v))), f.at(3, 3))
	}
	f.ret(f.at(4, 3))
	return f
}

// TestSoleEffectCountsSmallCallees: soleEffectOf, the effect the
// same-pack single-call rule bounds by soleCallOps, counts each call of a
// small function in another pack (here called twice, so not a single-call
// callee) with its whole effect. Caller's one call of S is marked when S's
// own operations plus two of Leaf's are soleCallOps, and not one operation
// over.
func TestSoleEffectCountsSmallCallees(t *testing.T) {
	for _, c := range []struct {
		name string
		over int
		want bool
	}{{"at-bound", 0, true}, {"over-bound", 1, false}} {
		t.Run(c.name, func(t *testing.T) {
			e := newTestEnv()
			leaf, l := smallLeaf(e, "Leaf", smallOps-1) // bodyOps smallOps
			fixed := bodyOps(sWithSmallCallee(newTestEnv(), leaf, 0).b)
			s := sWithSmallCallee(e, leaf, soleCallOps-2*smallOps-fixed+c.over)
			if got := bodyOps(s.b) + 2*smallOps; got != soleCallOps+c.over {
				t.Fatalf("S's effect is %d, want %d", got, soleCallOps+c.over)
			}
			f := soleCaller(e, "Caller", s.item, false, 0)
			u := unitOf(e, []*mir.Body{l.b, s.b, f.b})
			u.Packs = [][]int{{1, 2}, {0}}
			u.Funcs[0].Pack = 1
			p1 := emitPackN(t, e, u, 1, types.X86_64Linux, types.Release)
			if !smallMarked(t, p1, "Leaf") {
				t.Fatalf("Leaf is not small: %s", defLine(p1, "mk.main.Leaf"))
			}
			p0 := emitPackN(t, e, u, 0, types.X86_64Linux, types.Release)
			if got := soleMarked(t, p0, "Caller", "S"); got != c.want {
				t.Errorf("Caller's call of S: marked %v, want %v", got, c.want)
			}
		})
	}
}

// TestConstantTimeAcrossPacks: the constant-time exclusion holds for
// the cross-pack mark too. CtBig, a large std.crypto function whose one
// call is in another pack, and Big, a large function whose one call is
// from std.crypto's Ct in another pack, stay unmarked; Big2, the same
// shape called from a function outside std.crypto, is marked.
func TestConstantTimeAcrossPacks(t *testing.T) {
	e := newTestEnv()
	ctBig, cb := soleLeaf(e, "CtBig", 200, false)
	e.items.Get(ctBig).Key = types.ItemKey("std.crypto", "CtBig")
	useCt := soleCaller(e, "UseCt", ctBig, false, 0)
	big, b := soleLeaf(e, "Big", 200, false)
	ct := soleCaller(e, "Ct", big, false, 0)
	e.items.Get(ct.item).Key = types.ItemKey("std.crypto", "Ct")
	big2, b2 := soleLeaf(e, "Big2", 200, false)
	plain := soleCaller(e, "Plain", big2, false, 0)
	u := unitOf(e, []*mir.Body{cb.b, ct.b, plain.b, useCt.b, b.b, b2.b})
	u.Packs = [][]int{{0, 1, 2}, {3, 4, 5}}
	for i := 3; i < 6; i++ {
		u.Funcs[i].Pack = 1
	}
	p0 := emitPackN(t, e, u, 0, types.X86_64Linux, types.Release)
	p1 := emitPackN(t, e, u, 1, types.X86_64Linux, types.Release)
	if line := defLine(p0, "mk.std.crypto.CtBig"); !strings.HasPrefix(line, "define ") || strings.Contains(line, " alwaysinline") {
		t.Errorf("std.crypto's CtBig, called once from another pack: %q, want a definition without the mark", line)
	}
	if smallMarked(t, p1, "Big") {
		t.Errorf("Big, called once from std.crypto's Ct in another pack, is marked")
	}
	if !smallMarked(t, p1, "Big2") {
		t.Errorf("Big2, called once from Plain in another pack, is not marked")
	}
}

// TestSmallCallRepeatsTheMarkOnce: a small callee's one call in its
// own pack repeats the definition's alwaysinline (callAttrs), and the
// single-call rule adds no second one (soleCallAttr's attrs.Has check).
func TestSmallCallRepeatsTheMarkOnce(t *testing.T) {
	e := newTestEnv()
	leaf, l := smallLeaf(e, "Leaf", 1)
	f := soleCaller(e, "Caller", leaf, false, 0)
	ll := emit(t, e, unitOf(e, []*mir.Body{l.b, f.b}), types.X86_64Linux, types.Release)
	if !smallMarked(t, ll, "Leaf") {
		t.Fatalf("Leaf is not small: %s", defLine(ll, "mk.main.Leaf"))
	}
	calls := soleCallLines(ll, "mk.main.Caller", "mk.main.Leaf")
	if len(calls) != 1 {
		t.Fatalf("Caller: %d calls of Leaf, want 1", len(calls))
	}
	if n := strings.Count(calls[0], "alwaysinline"); n != 1 {
		t.Errorf("Caller's call of Leaf names alwaysinline %d times, want 1: %s", n, calls[0])
	}
}

// pfLoopUnit is Loop (a loop calling Step once) and Step (calling Advance
// once) in pack 0, and Advance in pack 1: a large function with no loop
// (pad statements) or with a loop of the same operation count.
func pfLoopUnit(t *testing.T, e *testEnv, loop bool) *Unit {
	const pad = 200
	n := pad
	if loop {
		n = pad - 6 // countLoop adds three statements and three terminators
	}
	adv, a := soleLeaf(e, "Advance", n, loop)
	if got := bodyOps(a.b); got != pad+1 {
		t.Fatalf("Advance (loop %v) has %d operations, want %d", loop, got, pad+1)
	}
	s := soleCaller(e, "Step", adv, false, 0)
	m := soleCaller(e, "Loop", s.item, true, 0)
	u := unitOf(e, []*mir.Body{s.b, m.b, a.b})
	u.Packs = [][]int{{0, 1}, {2}}
	u.Funcs[2].Pack = 1
	return u
}

// TestPackFactsCalleeLoop: giving another pack's callee a
// loop, with its operation count and both its marks unchanged, changes
// pack 0's IR (Loop's call of Step loses the single-call mark, since Step
// now carries Advance's loop) and so must change pack 0's facts: the
// per-call loops bit is the only fact that moves.
func TestPackFactsCalleeLoop(t *testing.T) {
	cfg := Config{Target: types.X86_64Windows, Profile: types.Release}
	e0, e1 := newTestEnv(), newTestEnv()
	u0, u1 := pfLoopUnit(t, e0, false), pfLoopUnit(t, e1, true)
	for i, c := range []struct {
		e *testEnv
		u *Unit
	}{{e0, u0}, {e1, u1}} {
		p1 := emitCfg(t, c.e, c.u, 1, cfg)
		if !smallMarked(t, p1, "Advance") {
			t.Fatalf("unit %d: Advance lacks the cross-pack mark: %s", i, defLine(p1, "mk.main.Advance"))
		}
	}
	if !soleMarked(t, emitCfg(t, e0, u0, 0, cfg), "Loop", "Step") {
		t.Error("loop-free Advance: Loop's call of Step is not marked")
	}
	if soleMarked(t, emitCfg(t, e1, u1, 0, cfg), "Loop", "Step") {
		t.Error("looping Advance: Loop's call of Step is marked")
	}
	h0, ok0 := pfHash(u0, 0, e0, cfg)
	h1, ok1 := pfHash(u1, 0, e1, cfg)
	if !ok0 || !ok1 {
		t.Fatal("PackFacts refused a release configuration")
	}
	if h0 == h1 {
		t.Error("Advance's loop in pack 1 changed pack 0's IR but not its facts")
	}
}

// pfBodyLoopUnit is TestPackFactsCalleeBodyLoop's unit: pack 0 holds Outer,
// whose loop calls the thin M, which calls J once; pack 1 holds the thin J,
// which calls Y and Z once each, Z (not thin) calling the looping Q. Y
// has a loop when loop is set, at the same operation count.
func pfBodyLoopUnit(t *testing.T, e *testEnv, loop bool) *Unit {
	n := smallOps + 8
	if loop {
		n -= 6 // countLoop adds three statements and three terminators
	}
	y, yb := soleLeaf(e, "Y", n, loop)
	if got := bodyOps(yb.b); got != smallOps+9 {
		t.Fatalf("Y (loop %v) has %d operations, want %d", loop, got, smallOps+9)
	}
	q, qb := soleLeaf(e, "Q", 1, true)
	z := soleCaller(e, "Z", q, false, thinCallerBlocks)
	j := e.body(e.fn("J", []types.TypeID{types.Int}, types.Int, 0))
	v := j.call(y, nil, types.Int, mir.Copy(place(1)))
	w := j.call(z.item, nil, types.Int, mir.Copy(place(v)))
	j.assign(place(0), use(mir.Copy(place(w))), j.at(6, 3))
	j.ret(j.at(7, 3))
	m := soleCaller(e, "M", j.item, false, 0)
	o := soleCaller(e, "Outer", m.item, true, 0)
	u := unitOf(e, []*mir.Body{o.b, m.b, j.b, yb.b, z.b, qb.b})
	u.Packs = [][]int{{0, 1}, {2, 3, 4, 5}}
	for i := 2; i < 6; i++ {
		u.Funcs[i].Pack = 1
	}
	return u
}

// TestPackFactsCalleeBodyLoop: a loop that ends up in a marked callee's
// body (smallSet.loops) can move alone. J, in pack 1, keeps its operation
// count, its effect's loop (Z counts Q's) and its cross-pack mark when Y
// gains a loop, but J is thin, so Y's loop now ends up in J, M carries it,
// and Outer's loop loses its call mark of M: pack 0's facts must move.
func TestPackFactsCalleeBodyLoop(t *testing.T) {
	cfg := Config{Target: types.X86_64Windows, Profile: types.Release}
	e0, e1 := newTestEnv(), newTestEnv()
	u0, u1 := pfBodyLoopUnit(t, e0, false), pfBodyLoopUnit(t, e1, true)
	for i, c := range []struct {
		e *testEnv
		u *Unit
	}{{e0, u0}, {e1, u1}} {
		if p1 := emitCfg(t, c.e, c.u, 1, cfg); !smallMarked(t, p1, "J") {
			t.Fatalf("unit %d: J lacks the cross-pack mark: %s", i, defLine(p1, "mk.main.J"))
		}
	}
	if !soleMarked(t, emitCfg(t, e0, u0, 0, cfg), "Outer", "M") {
		t.Error("loop-free Y: Outer's call of M is not marked")
	}
	if soleMarked(t, emitCfg(t, e1, u1, 0, cfg), "Outer", "M") {
		t.Error("looping Y: Outer's call of M is marked")
	}
	h0, ok0 := pfHash(u0, 0, e0, cfg)
	h1, ok1 := pfHash(u1, 0, e1, cfg)
	if !ok0 || !ok1 {
		t.Fatal("PackFacts refused a release configuration")
	}
	if h0 == h1 {
		t.Error("Y's loop in pack 1 changed pack 0's IR but not its facts")
	}
}

// TestSmallSetFollowsAppendedFuncs: a unit extended
// between emissions (codegen.go's idxN rule) recomputes its small set, so
// a small function appended after the first emission is marked, not read
// past the end of the first set.
func TestSmallSetFollowsAppendedFuncs(t *testing.T) {
	e := newTestEnv()
	_, l := smallLeaf(e, "Leaf", 1)
	u := unitOf(e, []*mir.Body{l.b})
	if ll := emit(t, e, u, types.X86_64Linux, types.Release); !smallMarked(t, ll, "Leaf") {
		t.Fatalf("Leaf is not small: %s", defLine(ll, "mk.main.Leaf"))
	}
	leaf2, l2 := smallLeaf(e, "Leaf2", 1)
	f := twiceCaller(e, "Caller", leaf2)
	for _, b := range []*mir.Body{l2.b, f.b} {
		it := e.items.Get(b.Item)
		u.Packs[0] = append(u.Packs[0], len(u.Funcs))
		u.Funcs = append(u.Funcs, Func{Symbol: "mk." + it.Key.String(), Key: it.Key.String(), Item: b.Item, Body: b})
	}
	if ll := emit(t, e, u, types.X86_64Linux, types.Release); !smallMarked(t, ll, "Leaf2") {
		t.Errorf("Leaf2, appended after the first emission, is not marked: %s", defLine(ll, "mk.main.Leaf2"))
	}
}

// TestPackFactsMemberSmallBit: a member's own small mark can move
// with nothing else of its pack: Leaf, alone in pack 0, is
// small while Plain in pack 1 calls it, and not once Plain is std.crypto's
// (ctReach). Leaf's definition loses its mark, so pack 0's facts must move;
// only the member's small bit does.
func TestPackFactsMemberSmallBit(t *testing.T) {
	cfg := Config{Target: types.X86_64Windows, Profile: types.Release}
	unit := func(ct bool) (*testEnv, *Unit) {
		e := newTestEnv()
		leaf, l := smallLeaf(e, "Leaf", 1)
		f := twiceCaller(e, "Plain", leaf)
		if ct {
			e.items.Get(f.item).Key = types.ItemKey("std.crypto", "Plain")
		}
		u := unitOf(e, []*mir.Body{l.b, f.b})
		u.Packs = [][]int{{0}, {1}}
		u.Funcs[1].Pack = 1
		return e, u
	}
	e0, u0 := unit(false)
	e1, u1 := unit(true)
	if !smallMarked(t, emitCfg(t, e0, u0, 0, cfg), "Leaf") {
		t.Fatal("Leaf called from Plain is not small")
	}
	if smallMarked(t, emitCfg(t, e1, u1, 0, cfg), "Leaf") {
		t.Fatal("Leaf called from std.crypto's Plain is small")
	}
	h0, ok0 := pfHash(u0, 0, e0, cfg)
	h1, ok1 := pfHash(u1, 0, e1, cfg)
	if !ok0 || !ok1 {
		t.Fatal("PackFacts refused a release configuration")
	}
	if h0 == h1 {
		t.Error("Leaf's small mark moved but pack 0's facts did not")
	}
}
