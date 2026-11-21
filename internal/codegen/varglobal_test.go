// Why: mono may canonicalize a fn literal to an equal literal elsewhere, and
// a var global's initializer must then name the instance that exists.
// What: TestVarGlobalCanonicalFnLiteral: the relocation names the
// canonical instance through Unit.Lookup.
// How: two equal literals, one without an instance, built with and without
// a Lookup. The driver leaves Lookup nil and relies on codegen's exact index.

package codegen

import (
	"errors"
	"strings"
	"testing"

	"mk/internal/mir"
	"mk/internal/types"
)

// varGlobalUnit: literals L1 and L2, both fn(a: int): int { return a + 1 };
// mono keeps L1 (canonical, with a body in the pack) and gives L2 no
// instance; var Table: Ops = Ops(f: L2).
func varGlobalUnit(e *testEnv, lookup bool) *Unit {
	fnT := e.tt.Fn(types.FnType{Params: []types.FnParam{{Type: types.Int}}, Result: types.Int})
	ops := e.structT("Ops", fnT)
	lit := func(name string) *mir.Body {
		f := e.body(e.fn(name, []types.TypeID{types.Int}, types.Int, 0))
		f.assign(place(0), bin(mir.AddWrap, mir.Copy(place(1)), mir.Imm(types.Int, 1)), f.at(2, 5))
		f.ret(f.at(2, 5))
		return f.b
	}
	l1, l2 := lit("L1"), lit("L2")
	v := e.consts.Intern(types.ConstValue{Type: ops, Align: 8, Bytes: make([]byte, 8),
		Relocs: []types.Reloc{{Offset: 0, Kind: types.RelocFn, Fn: types.FnRef{Item: l2.Item}}}})
	table := e.item("Table", types.KindGlobal, &types.GlobalDecl{Type: ops, Value: v, HasValue: true})
	u := cCallable(unitOf(e, []*mir.Body{l1})) // a const reloc: AddrTaken
	u.Globals = []Global{{Item: table, Pack: 0}}
	if lookup {
		u.Lookup = func(item types.ItemID, args []types.GenericArg) (int, bool) {
			if item == l2.Item || item == l1.Item {
				return 0, true // mono's canonical literal
			}
			return 0, false
		}
	}
	return u
}

func TestVarGlobalCanonicalFnLiteral(t *testing.T) {
	for _, target := range types.Targets() {
		t.Run(target.Name, func(t *testing.T) {
			e := newTestEnv()
			ll := emit(t, e, varGlobalUnit(e, true), target, types.Release)
			if !strings.Contains(ll, "@mk.main.Table = ") || !strings.Contains(ll, "ptr @mk.main.L1") {
				t.Errorf("the global does not relocate to the canonical literal:\n%s", ll)
			}
			clangC(t, target, t.TempDir(), "pack", ll, "-O2")
			// Without Lookup the exact index cannot see the canonicalization:
			// the ICE a nil Lookup gives.
			e2 := newTestEnv()
			_, err := EmitPack(varGlobalUnit(e2, false), 0, e2, Config{Target: target}, new(strings.Builder))
			var ie *InternalError
			if !errors.As(err, &ie) || !strings.Contains(ie.IE.What, "no instance of main.L2") {
				t.Errorf("nil Lookup: %v, want the no-instance ICE", err)
			}
		})
	}
}
