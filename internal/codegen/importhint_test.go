// Why: the import hint must stay out of the builds and packs its gates
// name, or it changes code the constant-time readings or a host linker see.
// What: TestImportHintGates, with hintUnit.
// How: a hand-built unit whose pack 0 takes &T of a const
// std.core.AllocTable over Ent, defined in pack 1, emitted under each gate;
// the ungated build is the control that has the hint.

package codegen

import (
	"bytes"
	"strings"
	"testing"

	"mk/internal/mir"
	"mk/internal/types"
)

// hintUnit is Get(n) taking &T in pack 0, Ent(n) in pack 1, and const
// T = AllocTable(Ent). With ct set, std.crypto's Ct calls Get, in pack
// ctPack.
func hintUnit(e *testEnv, ct bool, ctPack int) *Unit {
	fnT := e.tt.Fn(types.FnType{Params: []types.FnParam{{Type: types.Int}}, Result: types.Int})
	tab := e.structIn("std.core", "AllocTable", fnT)
	ent, eb := soleLeaf(e, "Ent", 1, false)
	v := e.consts.Intern(types.ConstValue{Type: tab, Align: 8, Bytes: make([]byte, 8),
		Relocs: []types.Reloc{{Offset: 0, Kind: types.RelocFn, Fn: types.FnRef{Item: ent}}}})
	tc := e.item("T", types.KindConst, &types.ConstDecl{Type: tab, Value: v, HasValue: true})
	get := e.body(e.fn("Get", []types.TypeID{types.Int}, types.Int, 0))
	p := get.local(e.tt.Ref(tab))
	get.assign(place(p), mir.Rvalue{Kind: mir.RvGlobalRef, Item: tc, Type: e.tt.Ref(tab)}, get.at(2, 3))
	get.assign(place(0), use(mir.Copy(place(1))), get.at(3, 3))
	get.ret(get.at(4, 3))
	bodies := []*mir.Body{get.b, eb.b}
	packs := [][]int{{0}, {1}}
	if ct {
		c := soleCaller(e, "Ct", get.item, false, 0)
		e.items.Get(c.item).Key = types.ItemKey("std.crypto", "Ct")
		bodies = append(bodies, c.b)
		packs[ctPack] = append(packs[ctPack], 2)
	}
	u := unitOf(e, bodies)
	u.Packs = packs
	u.Funcs[1].Pack = 1
	if ct {
		u.Funcs[2].Pack = ctPack
	}
	return u
}

// TestImportHintGates: pack 0's release IR on x86_64-linux has
// mk.importhint calling Ent; a dev build, a macOS target, a library,
// --profile size, a std.crypto caller of Get in pack 0 and one in pack 1
// (Get then reached from a constant-time function) each leave it out.
func TestImportHintGates(t *testing.T) {
	for _, c := range []struct {
		name   string
		cfg    Config
		ct     bool
		ctPack int
		want   bool
	}{
		{"release", Config{Target: types.X86_64Linux, Profile: types.Release}, false, 0, true},
		{"windows", Config{Target: types.X86_64Windows, Profile: types.Release}, false, 0, true},
		{"dev", Config{Target: types.X86_64Linux, Profile: types.Dev}, false, 0, false},
		{"macos", Config{Target: types.Aarch64Macos, Profile: types.Release}, false, 0, false},
		{"library", Config{Target: types.X86_64Linux, Profile: types.Release, Library: true}, false, 0, false},
		{"size", Config{Target: types.X86_64Linux, Profile: types.Release, Size: true}, false, 0, false},
		{"ct-in-pack", Config{Target: types.X86_64Linux, Profile: types.Release}, true, 0, false},
		{"ct-caller", Config{Target: types.X86_64Linux, Profile: types.Release}, true, 1, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newTestEnv()
			u := hintUnit(e, c.ct, c.ctPack)
			var b bytes.Buffer
			if _, err := EmitPack(u, 0, e, c.cfg, &b); err != nil {
				t.Fatal(err)
			}
			ll := b.String()
			has := strings.Contains(ll, "define internal void @mk.importhint()")
			if has != c.want {
				t.Fatalf("hint %v, want %v:\n%s", has, c.want, ll)
			}
			if has && !strings.Contains(ll, "call i64 @mk.main.Ent(i64 0)") {
				t.Errorf("the hint does not call Ent with a zero argument:\n%s", ll)
			}
		})
	}
}
