// Why: instances that differ only in facts must never be folded, or one
// gets the other's !nonnull, !range or noalias promises.
// What: TestFoldRespectsFacts: the pairs First[&int]/First[?&int],
// Wrap[E1]/Wrap[E2] (u8 tags {0,1} and {0,1,2}) and
// Keep[i64]/Keep[Cell[i64]] are instances of one item each, and their IR
// differs in exactly the !nonnull, !range and noalias readonly facts.
// How: normalize strips those facts and compares; a release run of
// First[?&int] on a slice holding none takes the none branch.

package codegen

import (
	"regexp"
	"testing"

	"mk/internal/mir"
	"mk/internal/source"
	"mk/internal/types"
)

// instance is a body of item with concrete params and result.
func instance(e *testEnv, item types.ItemID, params []types.TypeID, result types.TypeID) *fnb {
	b := mir.NewBody(item, source.Span{File: 1, Start: 0, End: 1}, result)
	for _, p := range params {
		b.AddParam(p, e.in.Intern("p"), 0)
	}
	f := &fnb{e: e, item: item, b: b}
	f.cur = b.AddBlock(mir.OriginBody, mir.NoDefer)
	return f
}

type d074 struct {
	u                                *Unit
	firstRef, firstOpt, wrap1, wrap2 string
	keepInt, keepCell                string
	first                            types.ItemID
}

func foldUnit(e *testEnv) *d074 {
	gen := func(name string) types.ItemID {
		id := e.fn(name, nil, types.Void, 0)
		e.items.Get(id).Decl.(*types.FuncDecl).Sig.Generics = []types.GenericParam{{}}
		return id
	}
	first, wrap, keep := gen("First"), gen("Wrap"), gen("Keep")
	d := &d074{u: &Unit{Packs: [][]int{nil}}, first: first}
	add := func(f *fnb, sym string, arg types.TypeID) string {
		d.u.Packs[0] = append(d.u.Packs[0], len(d.u.Funcs))
		d.u.Funcs = append(d.u.Funcs, Func{Symbol: sym, Key: sym, Item: f.item, Body: f.b, Args: []types.GenericArg{types.TypeArg(arg)}})
		return sym
	}
	// First[T](xs: []T, i: int): T { return xs[i] }
	firstOf := func(t types.TypeID, sym string) string {
		f := instance(e, first, []types.TypeID{e.tt.Slice(t), types.Int}, t)
		f.assign(place(0), use(mir.Copy(f.b.Place(1, mir.Projection{Kind: mir.ProjIndex, A: 2, Type: t}))), f.at(2, 3))
		f.ret(f.at(2, 3))
		return add(f, sym, t)
	}
	d.firstRef = firstOf(e.tt.Ref(types.Int), "mk.main.First.1111111111111111")
	d.firstOpt = firstOf(e.tt.Optional(e.tt.Ref(types.Int)), "mk.main.First.2222222222222222")
	// Wrap[E](x: &E): E { return *x }
	wrapOf := func(t types.TypeID, sym string) string {
		f := instance(e, wrap, []types.TypeID{e.tt.Ref(t)}, t)
		f.assign(place(0), use(mir.Copy(f.b.Place(1, mir.Projection{Kind: mir.ProjDeref, Type: t}))), f.at(4, 3))
		f.ret(f.at(4, 3))
		return add(f, sym, t)
	}
	d.wrap1 = wrapOf(e.enumT("E1", types.U8, []uint64{0, 1}), "mk.main.Wrap.3333333333333333")
	d.wrap2 = wrapOf(e.enumT("E2", types.U8, []uint64{0, 1, 2}), "mk.main.Wrap.4444444444444444")
	// Keep[T](p: &T): int { return 0 }
	keepOf := func(t types.TypeID, sym string) string {
		f := instance(e, keep, []types.TypeID{e.tt.Ref(t)}, types.Int)
		f.assign(place(0), use(mir.Imm(types.Int, 0)), f.at(6, 3))
		f.ret(f.at(6, 3))
		return add(f, sym, t)
	}
	cell := e.structT("Cell", types.I64)
	e.noFreeze[cell] = true
	d.keepInt = keepOf(e.structT("Plain", types.I64), "mk.main.Keep.5555555555555555")
	d.keepCell = keepOf(cell, "mk.main.Keep.6666666666666666")
	// IsNone(xs: []?&int): int { match First[?&int](xs, 0) { none => 1, some => 2 } }
	opt := e.tt.Optional(e.tt.Ref(types.Int))
	isn := e.body(e.fn("IsNone", []types.TypeID{e.tt.Slice(opt)}, types.Int, 0))
	v, dv := isn.local(opt), isn.local(types.U64)
	ga := isn.b.AddGenericArgs(types.TypeArg(opt))
	isn.callTo(mir.Callee{Kind: mir.CallDirect, Fn: mir.FnRef{Item: first, Args: ga}}, place(v), mir.NoLocal, isn.at(8, 9),
		mir.Copy(place(1)), mir.Imm(types.Int, 0))
	isn.assign(place(dv), mir.Rvalue{Kind: mir.RvDiscriminant, Place: place(v)}, isn.at(8, 3))
	none, some := isn.newBlock(), isn.newBlock()
	isn.term(mir.Terminator{Kind: mir.TermSwitch, On: mir.Copy(place(dv)), Cases: isn.b.AddCases(
		mir.SwitchCase{Value: 0, Target: none}, mir.SwitchCase{Value: 1, Target: some}), Default: mir.NoBlock, Span: isn.at(8, 3)})
	isn.cur = none
	isn.assign(place(0), use(mir.Imm(types.Int, 1)), isn.at(9, 3))
	isn.ret(isn.at(9, 3))
	isn.cur = some
	isn.assign(place(0), use(mir.Imm(types.Int, 2)), isn.at(10, 3))
	isn.ret(isn.at(10, 3))
	d.u.Packs[0] = append(d.u.Packs[0], len(d.u.Funcs))
	d.u.Funcs = append(d.u.Funcs, Func{Symbol: "mk.main.IsNone", Key: "main.IsNone", Item: isn.item, Body: isn.b})
	return d
}

// normalize removes symbols and the facts under test, spells each !range
// by its node's text, and numbers no other metadata.
func normalize(ll, fn string, facts *regexp.Regexp) string {
	nodes := map[string]string{}
	for _, m := range regexp.MustCompile(`(?m)^(!\d+) = (.*)$`).FindAllStringSubmatch(ll, -1) {
		nodes[m[1]] = m[2]
	}
	fn = regexp.MustCompile(`@"?mk\.main\.[A-Za-z]+\.[0-9]{16}"?`).ReplaceAllString(fn, "@F")
	fn = facts.ReplaceAllString(fn, "")
	fn = regexp.MustCompile(`!range (!\d+)`).ReplaceAllStringFunc(fn, func(r string) string { return "!range " + nodes[r[len("!range "):]] })
	return regexp.MustCompile(`!\d+`).ReplaceAllString(fn, "!N")
}

func TestFoldRespectsFacts(t *testing.T) {
	e := newTestEnv()
	d := foldUnit(e)
	ll := emit(t, e, d.u, types.X86_64Linux, types.Release)
	pairs := []struct {
		a, b  string
		facts *regexp.Regexp
	}{
		{d.firstRef, d.firstOpt, regexp.MustCompile(`, !nonnull !\d+|, !align !\d+|, !noundef !\d+|nonnull align 8 dereferenceable\(8\) `)},
		{d.wrap1, d.wrap2, regexp.MustCompile(`, !range !\d+`)},
		{d.keepInt, d.keepCell, regexp.MustCompile(` noalias readonly`)},
	}
	for _, p := range pairs {
		fa, fb := fnText(ll, p.a), fnText(ll, p.b)
		if fa == "" || fb == "" {
			t.Fatalf("no definitions of %s and %s", p.a, p.b)
		}
		if normalize(ll, fa, regexp.MustCompile(`$^`)) == normalize(ll, fb, regexp.MustCompile(`$^`)) {
			t.Errorf("%s and %s do not differ", p.a, p.b)
		}
		if na, nb := normalize(ll, fa, p.facts), normalize(ll, fb, p.facts); na != nb {
			t.Errorf("%s and %s differ beyond %s:\n%s\n%s", p.a, p.b, p.facts, na, nb)
		}
	}
	elem := regexp.MustCompile(`!nonnull !\d+, !noundef !\d+, !align`)
	if !elem.MatchString(fnText(ll, d.firstRef)) || elem.MatchString(fnText(ll, d.firstOpt)) {
		t.Error("!nonnull is not on exactly First[&int]'s element load")
	}
	for _, target := range runTargets {
		t.Run(target.Name, func(t *testing.T) {
			if _, why := hostRunner(target); why != "" {
				skipRunner(t, why)
			}
			e := newTestEnv()
			d := foldUnit(e)
			ll := emit(t, e, cCallable(d.u), target, types.Release) // the harness calls IsNone
			dir := t.TempDir()
			osIR, kernel := harnessOS(target)
			call := "  %r = call i64 @mk.main.IsNone(ptr @xs, i64 1)\n"
			decl := "declare i64 @mk.main.IsNone(ptr, i64)\n"
			if target.OS == types.OSWindows {
				call = "  %s = alloca [16 x i8], align 8\n  store ptr @xs, ptr %s, align 8\n  %sl = getelementptr inbounds i8, ptr %s, i64 8\n" +
					"  store i64 1, ptr %sl, align 8\n  %r = call i64 @mk.main.IsNone(ptr %s)\n"
				decl = "declare i64 @mk.main.IsNone(ptr)\n"
			}
			h := osIR + harnessRT + decl + `
@xs = internal global [1 x ptr] [ptr null], align 8

define void @"mk.std.task.Start"(ptr %sp) noreturn nounwind {
` + call + `  call void @h_num(i64 %r)
  call void @h_write(ptr @s.nl, i64 1)
  call void @h_exit(i32 0)
  unreachable
}
`
			objs := []string{clangC(t, target, dir, "support", supportLL(t, target), "-O2"),
				clangC(t, target, dir, "pack", ll, "-O2"), clangC(t, target, dir, "harness", h, "-O0")}
			out, code := runExe(t, target, linkExe(t, target, dir, objs, linkOpts{kernel: kernel, release: true}), "")
			if code != 0 || out != "1\n" {
				t.Fatalf("exit %d, output %q; want 1 (the none branch)", code, out)
			}
		})
	}
}
