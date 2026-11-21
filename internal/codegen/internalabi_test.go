// Why: on Win64 a function only Mk direct calls reach passes and returns
// a 16-byte two-integer aggregate in registers (abi.LowerInternal), while
// every function C can reach keeps the C ABI; the two must never mix.
// What: TestInternalABIWin64 (the definition and the call of an internal
// function, and the C ABI of an address-taken one, an extern, a float pair
// and a 32-byte value), TestInternalABIOtherTargets (no other target's IR
// changes), TestInternalABIValueIsICE, TestInternalABIPackFacts (a
// callee's become in another pack re-keys the caller's pack),
// TestInternalABIOwnPackFacts (a const naming a function in a third pack
// re-keys the function's own pack) and
// cCallable, which the tests of the C ABI and of C harnesses use.
// How: the definition and call lines are read from the emitted IR; the
// address-taken copy of the unit is the C ABI reference.

package codegen

import (
	"regexp"
	"strings"
	"testing"

	"mk/internal/mir"
	"mk/internal/source"
	"mk/internal/types"
)

// cCallable marks every function of u with a body AddrTaken, as mono
// marks a function C may call, and returns u: a test that reads the Win64
// C ABI of a Mk function, or a harness that calls one by its C signature,
// uses it.
func cCallable(u *Unit) *Unit {
	for i := range u.Funcs {
		u.Funcs[i].AddrTaken = u.Funcs[i].Body != nil
	}
	return u
}

// internalUnit is Callee(s: str, p: Pair, f: FPair, b: Big): Pair, which
// returns p, and Caller(s, p, f, b): Pair, which returns Callee's result;
// with extern X(s: str): Pair.
func internalUnit(e *testEnv) (*Unit, func() *Unit) {
	pair := e.structT("Pair", types.Int, types.Int)
	fpair := e.structT("FPair", types.F64, types.F64)
	big := e.structT("Big", types.Int, types.Int, types.Int, types.Int)
	params := []types.TypeID{types.Str, pair, fpair, big}
	callee := e.body(e.fn("Callee", params, pair, 0))
	callee.assign(place(0), use(mir.Copy(place(2))), callee.at(2, 3))
	callee.ret(callee.at(3, 3))
	caller := e.body(e.fn("Caller", params, pair, 0))
	caller.callTo(direct(callee.b.Item), place(0), mir.NoLocal, caller.at(5, 3),
		mir.Copy(place(1)), mir.Copy(place(2)), mir.Copy(place(3)), mir.Copy(place(4)))
	caller.ret(caller.at(6, 3))
	x := e.fn("X", []types.TypeID{types.Str}, pair, 0)
	e.items.Get(x).Decl.(*types.FuncDecl).Sig.Extern = true
	xc := e.body(e.fn("CallsX", []types.TypeID{types.Str}, pair, 0))
	xc.callTo(direct(x), place(0), mir.NoLocal, xc.at(8, 3), mir.Copy(place(1)))
	xc.ret(xc.at(9, 3))
	bodies := []*mir.Body{callee.b, caller.b, xc.b}
	mk := func() *Unit { return unitOf(e, bodies, x) }
	return mk(), mk
}

// internalDef is Callee's definition under the internal ABI.
var internalDef = regexp.MustCompile(`(?m)^define dso_local \{ i64, i64 \} @mk\.main\.Callee\(ptr [^%]*%p0, i64 [^%]*%p1, i64 [^%]*%p2, i64 [^%]*%p3, ptr [^%]*%p4, ptr [^%]*%p5\)`)

func TestInternalABIWin64(t *testing.T) {
	e := newTestEnv()
	u, mk := internalUnit(e)
	for _, prof := range []types.Profile{types.Dev, types.Release} {
		ll := emit(t, e, u, types.X86_64Windows, prof)
		// str as ptr and length, Pair as two i64 and the result in RAX:RDX;
		// FPair (floats) and Big (32 bytes) stay by reference.
		if !internalDef.MatchString(ll) {
			t.Errorf("profile %d: Callee is not { i64, i64 } (ptr, i64, i64, i64, ptr, ptr):\n%s", prof, fnText(ll, "mk.main.Callee"))
		}
		if strings.Contains(fnText(ll, "mk.main.Callee"), "sret") {
			t.Errorf("profile %d: Callee returns through sret:\n%s", prof, fnText(ll, "mk.main.Callee"))
		}
		if call := fnText(ll, "mk.main.Caller"); prof == types.Dev && !strings.Contains(call, `call { i64, i64 } @mk.main.Callee(ptr `) {
			t.Errorf("Caller does not call Callee under the internal ABI:\n%s", call)
		}
		// An extern keeps the C ABI: sret and str by reference.
		if xc := fnText(ll, "mk.main.CallsX"); !strings.Contains(xc, "@X(ptr") || !strings.Contains(xc, "sret") {
			t.Errorf("profile %d: the extern call changed:\n%s", prof, xc)
		}
	}
	// Address taken: C may call it, so the C ABI, as before.
	at := mk()
	at.Funcs[0].AddrTaken = true
	ll := emit(t, e, at, types.X86_64Windows, types.Dev)
	if _, ps := paramAttrs(t, ll, "mk.main.Callee"); len(ps) != 5 || !strings.Contains(ps[0], "sret") {
		t.Errorf("address-taken Callee is %q, want sret and four pointers", ps)
	}
	if call := fnText(ll, "mk.main.Caller"); !strings.Contains(call, "call void @mk.main.Callee(ptr") {
		t.Errorf("Caller does not call the address-taken Callee under the C ABI:\n%s", call)
	}
	for _, prof := range []types.Profile{types.Dev, types.Release} {
		clangC(t, types.X86_64Windows, t.TempDir(), "internalabi", emit(t, e, u, types.X86_64Windows, prof), "-O2")
	}
}

// TestInternalABIOtherTargets: System V and AAPCS64 already pass these
// values in registers, so internalABI changes no other target's IR.
func TestInternalABIOtherTargets(t *testing.T) {
	e := newTestEnv()
	u, mk := internalUnit(e)
	for _, target := range types.Targets() {
		if target == types.X86_64Windows {
			continue
		}
		at := mk()
		for i := range at.Funcs {
			at.Funcs[i].AddrTaken = at.Funcs[i].Body != nil
		}
		if got, want := emit(t, e, u, target, types.Dev), emit(t, e, at, target, types.Dev); got != want {
			t.Errorf("%s: the IR depends on AddrTaken", target.Name)
		}
	}
}

// TestInternalABIValueIsICE: a fn value of a function mono did not mark
// AddrTaken would be a C pointer to an internal-ABI body; codegen refuses.
func TestInternalABIValueIsICE(t *testing.T) {
	e := newTestEnv()
	u, _ := internalUnit(e)
	g := newPackGen(u, 0, e, &Config{Target: types.X86_64Windows, Profile: types.Dev}, nil)
	if !g.internalABI(0) {
		t.Fatal("Callee does not have the internal ABI")
	}
	ie := source.Guard("fnRef", source.Span{}, func() { g.fnRef(u.Funcs[0].Item, nil, 1) })
	if ie == nil || !strings.Contains(ie.What, "internal ABI") {
		t.Errorf("fnRef of an internal-ABI function: %v, want an ICE", ie)
	}
}

// TestInternalABIPackFacts: a `become` of Callee added in pack 1 gives
// Callee the C ABI (internalABI excludes tailItems), so pack 0, whose
// Caller calls it, must change its facts though none of its bodies did.
func TestInternalABIPackFacts(t *testing.T) {
	build := func(become bool) (*testEnv, *Unit) {
		e := newTestEnv()
		params := []types.TypeID{types.Str}
		callee := e.body(e.fn("Callee", params, types.Void, 0))
		callee.ret(callee.at(2, 3))
		caller := e.body(e.fn("Caller", params, types.Void, 0))
		caller.callTo(direct(callee.item), place(0), mir.NoLocal, caller.at(4, 3), mir.Copy(place(1)))
		caller.ret(caller.at(5, 3))
		other := e.body(e.fn("Other", params, types.Void, 0))
		if become {
			other.term(mir.Terminator{Kind: mir.TermTailCall, Callee: direct(callee.item),
				Args: other.b.AddArgs(mir.Arg{Operand: mir.Copy(place(1))}), Dest: place(0), Spawn: mir.NoLocal, Span: other.at(7, 3)})
		} else {
			other.ret(other.at(7, 3))
		}
		u := unitOf(e, []*mir.Body{caller.b, callee.b, other.b})
		u.Packs = [][]int{{0}, {1, 2}}
		u.Funcs[1].Pack, u.Funcs[2].Pack = 1, 1
		return e, u
	}
	cfg := Config{Target: types.X86_64Windows, Profile: types.Dev}
	e0, u0 := build(false)
	e1, u1 := build(true)
	g0 := newPackGen(u0, 0, e0, &cfg, nil)
	g1 := newPackGen(u1, 0, e1, &cfg, nil)
	if !g0.internalABI(1) || g1.internalABI(1) {
		t.Fatalf("Callee internal: %v without the become, %v with it; want true, false", g0.internalABI(1), g1.internalABI(1))
	}
	h0, _ := pfHash(u0, 0, e0, cfg)
	h1, _ := pfHash(u1, 0, e1, cfg)
	if h0 == h1 {
		t.Error("a become of Callee in pack 1 left pack 0's facts unchanged")
	}
}

// TestInternalABIConstValues: a function whose address a var global, a
// section const or a const a body reads holds keeps the C ABI though
// mono left it unmarked (a section const is emitted whether or not a body
// reads it; an IoTable entry no body reads waits unmarked), and the pack
// emits without the fnRef ICE.
func TestInternalABIConstValues(t *testing.T) {
	for _, kind := range []string{"var", "section", "read"} {
		t.Run(kind, func(t *testing.T) {
			e := newTestEnv()
			fnT := e.tt.Fn(types.FnType{Params: []types.FnParam{{Type: types.Str}}, Result: types.Void})
			ops := e.structT("Ops", fnT)
			callee := e.body(e.fn("Callee", []types.TypeID{types.Str}, types.Void, 0))
			callee.ret(callee.at(2, 3))
			caller := e.body(e.fn("Caller", []types.TypeID{types.Str}, types.Void, 0))
			caller.callTo(direct(callee.item), place(0), mir.NoLocal, caller.at(4, 3), mir.Copy(place(1)))
			v := e.consts.Intern(types.ConstValue{Type: ops, Align: 8, Bytes: make([]byte, 8),
				Relocs: []types.Reloc{{Offset: 0, Kind: types.RelocFn, Fn: types.FnRef{Item: callee.item}}}})
			var globals []Global
			switch kind {
			case "var":
				globals = []Global{{Item: e.item("Table", types.KindGlobal, &types.GlobalDecl{Type: ops, Value: v, HasValue: true}), Pack: 0}}
			case "section":
				globals = []Global{{Item: e.item("Table", types.KindConst, &types.ConstDecl{Type: ops, Value: v, HasValue: true, Section: ".mktab"}), Pack: 0}}
			case "read":
				c := e.item("Table", types.KindConst, &types.ConstDecl{Type: ops, Value: v, HasValue: true})
				tmp := caller.local(ops)
				caller.assign(place(tmp), mir.Rvalue{Kind: mir.RvConstRef, Item: c, Type: ops}, caller.at(5, 3))
			}
			caller.ret(caller.at(6, 3))
			u := unitOf(e, []*mir.Body{callee.b, caller.b})
			u.Globals = globals
			g := newPackGen(u, 0, e, &Config{Target: types.X86_64Windows, Profile: types.Dev}, nil)
			if g.internalABI(0) {
				t.Fatal("Callee, whose address the const holds, has the internal ABI")
			}
			ll := emit(t, e, u, types.X86_64Windows, types.Dev)
			if _, ps := paramAttrs(t, ll, "mk.main.Callee"); len(ps) != 1 || !strings.HasPrefix(ps[0], "ptr ") || !strings.Contains(ps[0], "dereferenceable(16)") {
				t.Errorf("Callee is %q, want the C ABI's str by reference", ps)
			}
		})
	}
}

// TestInternalABINotUnderDebug: --debug keeps the C ABI, which cdb and
// WinDbg read a parameter of more than 8 bytes by.
func TestInternalABINotUnderDebug(t *testing.T) {
	e := newTestEnv()
	u, _ := internalUnit(e)
	for _, prof := range []types.Profile{types.Dev, types.Release} {
		g := newPackGen(u, 0, e, &Config{Target: types.X86_64Windows, Profile: prof, Debug: true}, nil)
		if g.internalABI(0) {
			t.Errorf("profile %d --debug: Callee has the internal ABI", prof)
		}
	}
}

// TestInternalABIOwnPackFacts: a const naming Callee, read by a body in
// a third pack, gives Callee the C ABI (valueFns), so Callee's own pack
// must change its facts though none of its bodies did.
func TestInternalABIOwnPackFacts(t *testing.T) {
	build := func(read bool) (*testEnv, *Unit) {
		e := newTestEnv()
		params := []types.TypeID{types.Str}
		fnT := e.tt.Fn(types.FnType{Params: []types.FnParam{{Type: types.Str}}, Result: types.Void})
		ops := e.structT("Ops", fnT)
		callee := e.body(e.fn("Callee", params, types.Void, 0))
		callee.ret(callee.at(2, 3))
		caller := e.body(e.fn("Caller", params, types.Void, 0))
		caller.callTo(direct(callee.item), place(0), mir.NoLocal, caller.at(4, 3), mir.Copy(place(1)))
		caller.ret(caller.at(5, 3))
		reader := e.body(e.fn("Reader", nil, types.Void, 0))
		if read {
			v := e.consts.Intern(types.ConstValue{Type: ops, Align: 8, Bytes: make([]byte, 8),
				Relocs: []types.Reloc{{Offset: 0, Kind: types.RelocFn, Fn: types.FnRef{Item: callee.item}}}})
			c := e.item("Table", types.KindConst, &types.ConstDecl{Type: ops, Value: v, HasValue: true})
			tmp := reader.local(ops)
			reader.assign(place(tmp), mir.Rvalue{Kind: mir.RvConstRef, Item: c, Type: ops}, reader.at(7, 3))
		}
		reader.ret(reader.at(8, 3))
		u := unitOf(e, []*mir.Body{caller.b, callee.b, reader.b})
		u.Packs = [][]int{{0}, {1}, {2}}
		u.Funcs[1].Pack, u.Funcs[2].Pack = 1, 2
		return e, u
	}
	cfg := Config{Target: types.X86_64Windows, Profile: types.Dev}
	e0, u0 := build(false)
	e1, u1 := build(true)
	if g0, g1 := newPackGen(u0, 1, e0, &cfg, nil), newPackGen(u1, 1, e1, &cfg, nil); !g0.internalABI(1) || g1.internalABI(1) {
		t.Fatalf("Callee internal: %v without the const, %v with it; want true, false", g0.internalABI(1), g1.internalABI(1))
	}
	h0, _ := pfHash(u0, 1, e0, cfg)
	h1, _ := pfHash(u1, 1, e1, cfg)
	if h0 == h1 {
		t.Error("a const naming Callee in pack 2 left Callee's own pack's facts unchanged")
	}
}
