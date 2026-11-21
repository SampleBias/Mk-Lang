// Why: an Indirect by-value parameter the callee can never write is
// readonly and captures(none), so MemCpyOpt may pass the caller's storage
// instead of the fresh copy.
// What: TestImmutIndirectParams (a var parameter, a type with inline
// interior mutability, an extern and a byval parameter keep the plain copy)
// and TestImmutParamWrittenIsICE.
// How: the parameter attributes are read per target; emitNoValidate emits a
// body that writes a readonly parameter, which must be an ICE.

package codegen

import (
	"bytes"
	"strings"
	"testing"

	"mk/internal/mir"
	"mk/internal/types"
)

func TestImmutIndirectParams(t *testing.T) {
	e := newTestEnv()
	big := e.structT("Big2", e.tt.Ref(types.Int), types.Int, types.Int, types.Int)
	cell := e.structT("Cell2", types.Int)
	e.noFreeze[cell] = true
	cellBig := e.structT("CellBig", cell, types.Int, types.Int)
	e.noFreeze[cellBig] = true // the shape pass propagates Interior; the test env does not
	const ro = "ptr noundef nonnull noalias align 8 dereferenceable(32) readonly captures(none)"
	const plain = "ptr noundef nonnull noalias align 8 dereferenceable(32)"
	type row struct {
		name   string
		param  types.TypeID
		isVar  bool
		extern bool
		win    string // the Win64 parameter's attributes
	}
	rows := []row{
		{"Imm", big, false, false, ro},
		{"Var", big, true, false, plain},
		{"Interior", cellBig, false, false, "ptr noundef nonnull noalias align 8 dereferenceable(24)"},
		{"Extern", big, false, true, plain},
		{"Slice", e.tt.Slice(types.Int), false, false, "ptr noundef nonnull noalias align 8 dereferenceable(16) readonly captures(none)"},
		{"MutSlice", e.tt.MutSlice(types.Int), false, false, "ptr noundef nonnull noalias align 8 dereferenceable(16) readonly captures(none)"},
	}
	var bodies []*mir.Body
	for _, r := range rows {
		item := e.fn("I"+r.name, []types.TypeID{r.param}, types.Void, 0)
		if r.extern {
			fd := e.items.Get(item).Decl.(*types.FuncDecl)
			fd.Sig.Extern = true
		}
		f := e.body(item)
		f.b.Locals[1].Var = r.isVar
		f.ret(f.at(2, 3))
		bodies = append(bodies, f.b)
	}
	// Caller(x: Big2) calls IImm(x): the call site carries the facts too.
	cf := e.body(e.fn("Caller", []types.TypeID{big}, types.Void, 0))
	cf.callTo(direct(bodies[0].Item), place(0), mir.NoLocal, cf.at(3, 3), mir.Copy(place(1)))
	cf.ret(cf.at(4, 3))
	bodies = append(bodies, cf.b)
	win := emitNoValidate(t, e, cCallable(unitOf(e, bodies)), types.X86_64Windows, types.Release) // the C ABI
	for _, r := range rows {
		if _, got := paramAttrs(t, win, "mk.main.I"+r.name); len(got) != 1 || got[0] != r.win {
			t.Errorf("Win64 %s: %q, want %q", r.name, got, r.win)
		}
	}
	if !strings.Contains(fnText(win, "mk.main.Caller"), "dereferenceable(32) readonly captures(none) %") {
		t.Errorf("the call of IImm carries no readonly captures(none):\n%s", fnText(win, "mk.main.Caller"))
	}
	// SysV: Big2 is byval (LLVM copies), the slices Direct; nothing changes.
	lin := emitNoValidate(t, e, unitOf(e, bodies), types.X86_64Linux, types.Release)
	if strings.Contains(lin, "captures(none)") {
		t.Errorf("x86_64-linux gained captures(none)")
	}
	for _, target := range types.Targets() {
		clangC(t, target, t.TempDir(), "immutarg", emitNoValidate(t, e, unitOf(e, bodies), target, types.Release), "-O2")
	}
}

// TestImmutParamWrittenIsICE: a body that assigns a field of a
// non-var Indirect parameter breaks the rule that only a var parameter is
// assigned, and codegen says so instead of emitting it under readonly. A
// write through a slice parameter's data pointer (an index) is not a write
// of the parameter.
func TestImmutParamWrittenIsICE(t *testing.T) {
	e := newTestEnv()
	big := e.structT("Big3", types.Int, types.Int, types.Int, types.Int)
	f := e.body(e.fn("Writes", []types.TypeID{big}, types.Void, 0))
	f.assign(f.b.Place(1, mir.Projection{Kind: mir.ProjField, A: 0, Type: types.Int}),
		mir.Rvalue{Kind: mir.RvUse, A: mir.Operand{Kind: mir.OpImm, Type: types.Int, Bits: 1}}, f.at(2, 3))
	f.ret(f.at(3, 3))
	var b bytes.Buffer
	_, err := EmitPack(unitOf(e, []*mir.Body{f.b}), 0, e, Config{Target: types.X86_64Windows, Profile: types.Release}, &b)
	if err == nil || !strings.Contains(err.Error(), "is written but declared readonly") {
		t.Fatalf("err = %v, want the ICE of a written readonly parameter", err)
	}
	// The same write under var is the parameter's own copy: no ICE.
	g := e.body(e.fn("WritesVar", []types.TypeID{big}, types.Void, 0))
	g.b.Locals[1].Var = true
	g.assign(g.b.Place(1, mir.Projection{Kind: mir.ProjField, A: 0, Type: types.Int}),
		mir.Rvalue{Kind: mir.RvUse, A: mir.Operand{Kind: mir.OpImm, Type: types.Int, Bits: 1}}, g.at(2, 3))
	g.ret(g.at(3, 3))
	// buf[0] = 1 through a mut []int parameter writes the elements only.
	h := e.body(e.fn("WritesElem", []types.TypeID{e.tt.MutSlice(types.Int)}, types.Void, 0))
	i0 := h.local(types.Int)
	h.assign(place(i0), mir.Rvalue{Kind: mir.RvUse, A: mir.Operand{Kind: mir.OpImm, Type: types.Int, Bits: 0}}, h.at(2, 3))
	h.assign(h.b.Place(1, mir.Projection{Kind: mir.ProjIndex, A: uint32(i0), Type: types.Int}),
		mir.Rvalue{Kind: mir.RvUse, A: mir.Operand{Kind: mir.OpImm, Type: types.Int, Bits: 1}}, h.at(3, 3))
	h.ret(h.at(4, 3))
	b.Reset()
	if _, err := EmitPack(unitOf(e, []*mir.Body{g.b, h.b}), 0, e, Config{Target: types.X86_64Windows, Profile: types.Release}, &b); err != nil {
		t.Fatalf("var or element write: %v", err)
	}
}

// emitNoValidate is emit without mir.Validate, for bodies that set Var on
// a parameter local the test signature does not declare var.
func emitNoValidate(t *testing.T, e *testEnv, u *Unit, target types.Target, profile types.Profile) string {
	t.Helper()
	var b bytes.Buffer
	if _, err := EmitPack(u, 0, e, Config{Target: target, Profile: profile}, &b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}
