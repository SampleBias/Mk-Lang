// Why: float compares (ordered and unordered, NaN) and the support pack's
// CallMain and ErrorName are only right if a real program behaves right.
// What: TestFloatPredicates and TestCallMainErrorNameRun, on Windows
// and WSL Linux.
// How: rtProgram's bodies are compiled and linked with a harness, run
// natively, and their output compared.

package codegen

import (
	"fmt"
	"math"
	"strings"
	"testing"

	"mk/internal/mir"
	"mk/internal/types"
)

// TestFloatPredicates: == is oeq, != is une, the orders are ordered,
// so every compare with a NaN is false except !=, and -0 == 0.
func TestFloatPredicates(t *testing.T) {
	ops := []struct {
		name string
		op   mir.BinOp
	}{{"FEq", mir.Eq}, {"FNe", mir.Ne}, {"FLt", mir.Lt}, {"FLe", mir.Le}, {"FGt", mir.Gt}, {"FGe", mir.Ge}}
	nan := math.NaN()
	pairs := [][2]float64{{nan, nan}, {nan, 1}, {1, nan}, {1, 2}, {2, 1}, {math.Copysign(0, -1), 0}, {math.Inf(1), math.Inf(1)}}
	var body, want strings.Builder
	n := 0
	for _, o := range ops {
		for _, p := range pairs {
			fmt.Fprintf(&body, "  %%r%d = call i1 @mk.main.%s(double %s, double %s)\n  %%w%d = zext i1 %%r%d to i64\n  call void @h_num(i64 %%w%d)\n  call void @h_write(ptr @s.nl, i64 1)\n",
				n, o.name, f64(p[0]), f64(p[1]), n, n, n)
			var r bool
			switch o.op {
			case mir.Eq:
				r = p[0] == p[1]
			case mir.Ne:
				r = p[0] != p[1]
			case mir.Lt:
				r = p[0] < p[1]
			case mir.Le:
				r = p[0] <= p[1]
			case mir.Gt:
				r = p[0] > p[1]
			case mir.Ge:
				r = p[0] >= p[1]
			}
			fmt.Fprintf(&want, "%d\n", b2u(r))
			n++
		}
	}
	for _, target := range runTargets {
		for _, opt := range []string{"-O0", "-O2"} {
			t.Run(target.Name+opt, func(t *testing.T) {
				if _, why := hostRunner(target); why != "" {
					skipRunner(t, why)
				}
				e := newTestEnv()
				var bodies []*mir.Body
				var decls strings.Builder
				for _, o := range ops {
					bodies = append(bodies, binFn(e, o.name, o.op, types.F64, types.Bool, mir.Copy(place(2))))
					fmt.Fprintf(&decls, "declare i1 @mk.main.%s(double, double)\n", o.name)
				}
				ll := emit(t, e, unitOf(e, bodies), target, types.Dev)
				dir := t.TempDir()
				osIR, kernel := harnessOS(target)
				h := osIR + harnessRT + decls.String() + `
define void @"mk.std.task.Start"(ptr %sp) noreturn nounwind {
` + body.String() + `  call void @h_exit(i32 0)
  unreachable
}
`
				objs := []string{clangC(t, target, dir, "support", supportLL(t, target), "-O2"),
					clangC(t, target, dir, "pack", ll, opt), clangC(t, target, dir, "harness", h, "-O0")}
				out, code := runExe(t, target, linkExe(t, target, dir, objs, linkOpts{kernel: kernel}), "")
				if code != 0 || out != want.String() {
					t.Fatalf("exit %d, output\n%s\nwant\n%s", code, out, want.String())
				}
			})
		}
	}
}

// rtProgram is Main(sys: &System): NotFound!int (NotFound when sys.f0 is 0,
// else f0 * 2), with the probes
//
//	Probe(n: int): int { r := rt.CallMain(&System(n)); return r.code *% 1000 +% r.exit }
//	NameAt(code: u32, i: int): int { s := rt.ErrorName(code); return s.Len() *% 1000 +% s[i] }
func rtProgram(e *testEnv) ([]*mir.Body, []types.ItemID) {
	nf := e.errItem("NotFound")
	e.errItem("Closed", types.I64)
	e.stdErrItem("std.core", "ParseError")
	e.stdErrItem("std.fs", "NotFound")
	set := e.tt.ErrorSet([]types.ErrorMember{{Item: nf}})
	sysT := e.structT("System", types.Int)
	mainRes := e.structT("MainResult", types.U32, types.Int)
	cm := e.stdFn("std.rt", "", "CallMain", 0, []types.TypeID{e.tt.Ref(sysT)}, mainRes)
	en := e.stdFn("std.rt", "", "ErrorName", 0, []types.TypeID{types.U32}, types.Str)

	m := e.body(e.fn("Main", []types.TypeID{e.tt.Ref(sysT)}, types.Int, set))
	f0 := m.local(types.Int)
	c := m.local(types.Bool)
	el := m.local(e.tt.ErrorSetType(set))
	m.assign(place(f0), use(mir.Copy(m.b.Place(1, mir.Projection{Kind: mir.ProjDeref, Type: sysT},
		mir.Projection{Kind: mir.ProjField, A: 0, Type: types.Int}))), m.at(2, 3))
	m.assign(place(c), bin(mir.Eq, mir.Copy(place(f0)), mir.Imm(types.Int, 0)), m.at(2, 3))
	bad, good := m.newBlock(), m.newBlock()
	m.term(mir.Terminator{Kind: mir.TermSwitch, On: mir.Copy(place(c)), Cases: m.b.AddCases(
		mir.SwitchCase{Value: 1, Target: bad}, mir.SwitchCase{Value: 0, Target: good}), Default: mir.NoBlock, Span: m.at(2, 3)})
	m.cur = bad
	m.assign(place(el), mir.Rvalue{Kind: mir.RvAggregate, Agg: mir.AggError, Item: nf}, m.at(3, 3))
	m.assign(place(0), mir.Rvalue{Kind: mir.RvAggregate, Agg: mir.AggErr, Ops: m.b.AddOperands(mir.Move(place(el)))}, m.at(3, 3))
	m.ret(m.at(3, 3))
	m.cur = good
	d := m.local(types.Int)
	m.assign(place(d), bin(mir.MulWrap, mir.Copy(place(f0)), mir.Imm(types.Int, 2)), m.at(4, 3))
	m.assign(place(0), mir.Rvalue{Kind: mir.RvAggregate, Agg: mir.AggOk, Ops: m.b.AddOperands(mir.Copy(place(d)))}, m.at(4, 3))
	m.ret(m.at(4, 3))

	p := e.body(e.fn("Probe", []types.TypeID{types.Int}, types.Int, 0))
	sys, ref, r := p.local(sysT), p.local(e.tt.Ref(sysT)), p.local(mainRes)
	cd, cd64, k := p.local(types.U32), p.local(types.Int), p.local(types.Int)
	p.assign(place(sys), mir.Rvalue{Kind: mir.RvAggregate, Agg: mir.AggStruct, Item: e.tt.Get(sysT).Item,
		Ops: p.b.AddOperands(mir.Copy(place(1)))}, p.at(6, 3))
	p.assign(place(ref), mir.Rvalue{Kind: mir.RvRef, Place: place(sys)}, p.at(6, 3))
	p.callTo(direct(cm), place(r), mir.NoLocal, p.at(6, 10), mir.Copy(place(ref)))
	p.assign(place(cd), use(mir.Copy(p.b.Place(r, mir.Projection{Kind: mir.ProjField, A: 0, Type: types.U32}))), p.at(7, 3))
	p.assign(place(cd64), mir.Rvalue{Kind: mir.RvCast, Cast: mir.CastLossless, A: mir.Copy(place(cd)), Type: types.Int}, p.at(7, 3))
	p.assign(place(k), bin(mir.MulWrap, mir.Copy(place(cd64)), mir.Imm(types.Int, 1000)), p.at(7, 3))
	p.assign(place(0), bin(mir.AddWrap, mir.Copy(place(k)), mir.Copy(p.b.Place(r, mir.Projection{Kind: mir.ProjField, A: 1, Type: types.Int}))), p.at(7, 3))
	p.ret(p.at(7, 3))

	na := e.body(e.fn("NameAt", []types.TypeID{types.U32, types.Int}, types.Int, 0))
	s, ln, ln2, by, by64 := na.local(types.Str), na.local(types.Int), na.local(types.Int), na.local(types.U8), na.local(types.Int)
	na.callTo(direct(en), place(s), mir.NoLocal, na.at(9, 10), mir.Copy(place(1)))
	na.assign(place(ln), mir.Rvalue{Kind: mir.RvLen, Place: place(s)}, na.at(9, 3))
	na.assign(place(ln2), bin(mir.MulWrap, mir.Copy(place(ln)), mir.Imm(types.Int, 1000)), na.at(9, 3))
	na.assign(place(by), use(mir.Copy(na.b.Place(s, mir.Projection{Kind: mir.ProjIndex, A: 2, Type: types.U8}))), na.at(9, 3))
	na.assign(place(by64), mir.Rvalue{Kind: mir.RvCast, Cast: mir.CastLossless, A: mir.Copy(place(by)), Type: types.Int}, na.at(9, 3))
	na.assign(place(0), bin(mir.AddWrap, mir.Copy(place(ln2)), mir.Copy(place(by64))), na.at(9, 3))
	na.ret(na.at(9, 3))
	return []*mir.Body{m.b, p.b, na.b}, []types.ItemID{cm, en}
}

// TestCallMainErrorNameRun: CallMain returns MainResult{0, 42} for an ok
// Main and {code(NotFound), 0} for a failing one; ErrorName finds each
// error's display form by its code (main.NotFound and main.Closed by key,
// std.core's ParseError bare, std.fs's NotFound as fs.NotFound) and
// "unknown error" otherwise.
func TestCallMainErrorNameRun(t *testing.T) {
	nf := types.ErrorCode(types.ItemKey("main", "NotFound"))
	cl := types.ErrorCode(types.ItemKey("main", "Closed"))
	pe := types.ErrorCode(types.ItemKey("std.core", "ParseError"))
	fnf := types.ErrorCode(types.ItemKey("std.fs", "NotFound"))
	body := pn(0, "@mk.main.Probe(i64 21)") + pn(1, "@mk.main.Probe(i64 0)") +
		pn(2, fmt.Sprintf("@mk.main.NameAt(i32 %d, i64 5)", nf)) + pn(3, fmt.Sprintf("@mk.main.NameAt(i32 %d, i64 5)", cl)) +
		pn(4, "@mk.main.NameAt(i32 12345, i64 5)") +
		pn(5, fmt.Sprintf("@mk.main.NameAt(i32 %d, i64 0)", pe)) + pn(6, fmt.Sprintf("@mk.main.NameAt(i32 %d, i64 0)", fnf))
	want := fmt.Sprintf("42\n%d\n%d\n%d\n%d\n%d\n%d\n", int64(nf)*1000, 13*1000+'N', 11*1000+'C', 13*1000+'w',
		10*1000+'P', 11*1000+'f')
	for _, target := range runTargets {
		t.Run(target.Name, func(t *testing.T) {
			if _, why := hostRunner(target); why != "" {
				skipRunner(t, why)
			}
			e := newTestEnv()
			bodies, bodiless := rtProgram(e)
			ll := emit(t, e, cCallable(unitOf(e, bodies, bodiless...)), target, types.Dev) // the harness calls them
			var sb strings.Builder
			// ErrorName names the errors the program makes (errnames.go):
			// these four, made by one more body.
			var made []types.ItemID
			for _, k := range []types.Key{types.ItemKey("main", "NotFound"), types.ItemKey("main", "Closed"),
				types.ItemKey("std.core", "ParseError"), types.ItemKey("std.fs", "NotFound")} {
				id, _ := e.items.ByKey(k)
				made = append(made, id)
			}
			if err := EmitSupport(errorMaker(e, made...), e, Config{Target: target, RootModule: "main"}, &sb); err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			osIR, kernel := harnessOS(target)
			h := osIR + harnessRT + `
declare i64 @mk.main.Probe(i64)
declare i64 @mk.main.NameAt(i32, i64)

define internal void @h_pn(i64 %v) nounwind {
  call void @h_num(i64 %v)
  call void @h_write(ptr @s.nl, i64 1)
  ret void
}

define void @"mk.std.task.Start"(ptr %sp) noreturn nounwind {
` + body + `  call void @h_exit(i32 0)
  unreachable
}
`
			objs := []string{clangC(t, target, dir, "support", sb.String(), "-O2"),
				clangC(t, target, dir, "pack", ll, "-O0"), clangC(t, target, dir, "harness", h, "-O0")}
			out, code := runExe(t, target, linkExe(t, target, dir, objs, linkOpts{kernel: kernel}), "")
			if code != 0 || out != want {
				t.Fatalf("exit %d, output\n%s\nwant\n%s", code, out, want)
			}
		})
	}
}
