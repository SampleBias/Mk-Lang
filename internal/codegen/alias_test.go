// Why: Win64 passes a slice's data pointer by load (Indirect), losing the
// noalias fact System V's parameter has; scoped alias metadata replaces it.
// What: TestAliasScopes and TestAliasMarkDropped.
// How: the accesses through a mut []T, and through the str and Freeze []T
// parameters beside it, must carry what LLVM's inliner writes for a noalias
// argument: one domain, one scope per parameter declared in the entry block
// (llvm.experimental.noalias.scope.decl), !alias.scope of its own scope and
// !noalias of the scopes it is paired with (a mut []T against every other;
// two shared slices never).

package codegen

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"mk/internal/mir"
	"mk/internal/types"
)

// aliasKern is Name(b: B, c: mut []E, i: int, j: int): E {
// c[i] = 1; t := b[j]; c[i] = 2; return t }, with E the element of B
// (u8 for a str), or E its first field when B's element is a struct
// (b[j].0): the first store is dead exactly when the load of b[j] cannot
// read c[i]. cVar makes c a var parameter, which is never tagged.
func aliasKern(e *testEnv, name string, bt types.TypeID, cVar bool) *mir.Body {
	el := types.TypeID(types.U8)
	if e.tt.Get(bt).Kind != types.KStr {
		el = types.Int
	}
	ms := e.tt.MutSlice(el)
	f := e.body(e.fn(name, []types.TypeID{bt, ms, types.Int, types.Int}, el, 0))
	f.b.Locals[2].Var = cVar
	ci := f.b.Place(2, mir.Projection{Kind: mir.ProjIndex, A: 3, Type: el})
	var bj mir.Place
	if bty := e.tt.Get(bt); bty.Kind != types.KStr && bty.Elem != types.Int {
		bj = f.b.Place(1, mir.Projection{Kind: mir.ProjIndex, A: 4, Type: bty.Elem},
			mir.Projection{Kind: mir.ProjField, A: 0, Type: types.Int})
	} else {
		bj = f.b.Place(1, mir.Projection{Kind: mir.ProjIndex, A: 4, Type: el})
	}
	f.assign(ci, use(mir.Imm(el, 1)), f.at(2, 3))
	f.assign(place(0), use(mir.Copy(bj)), f.at(3, 3))
	f.assign(ci, use(mir.Imm(el, 2)), f.at(4, 3))
	f.ret(f.at(5, 3))
	return f.b
}

func TestAliasScopes(t *testing.T) {
	e := newTestEnv()
	cell := e.structT("CellInt", types.Int)
	e.noFreeze[cell] = true
	pair := e.structT("PairInt", types.Int, types.Int)
	bodies := []*mir.Body{
		aliasKern(e, "KernPair", e.tt.Slice(pair), false),
		aliasKern(e, "Kern", e.tt.Slice(types.Int), false),
		aliasKern(e, "KernStr", types.Str, false),
		aliasKern(e, "KernVar", e.tt.Slice(types.Int), true),
		aliasKern(e, "KernCell", e.tt.Slice(cell), false),
	}
	// Two shared slices only: Shared(a: []int, b: []int, i: int): int {
	// return a[i] + b[i] } is never tagged.
	sl := e.tt.Slice(types.Int)
	sf := e.body(e.fn("Shared", []types.TypeID{sl, sl, types.Int}, types.Int, 0))
	sf.assign(place(0), use(mir.Copy(sf.b.Place(1, mir.Projection{Kind: mir.ProjIndex, A: 3, Type: types.Int}))), sf.at(2, 3))
	sf.assign(place(0), use(mir.Copy(sf.b.Place(2, mir.Projection{Kind: mir.ProjIndex, A: 3, Type: types.Int}))), sf.at(3, 3))
	sf.ret(sf.at(4, 3))
	bodies = append(bodies, sf.b)
	// An export extern body is never tagged (C may call it with any
	// views): KernX is Kern's body with Sig.Extern set.
	kx := aliasKern(e, "KernX", e.tt.Slice(types.Int), false)
	e.items.Get(kx.Item).Decl.(*types.FuncDecl).Sig.Extern = true
	bodies = append(bodies, kx)
	// Two shared slices beside a mut one: Three(a: []int, b: []int, c: mut
	// []int, i: int): int { c[i] = 1; return a[i]; return b[i] }. a and b
	// are each paired with c only, never with each other.
	ms := e.tt.MutSlice(types.Int)
	tf := e.body(e.fn("Three", []types.TypeID{sl, sl, ms, types.Int}, types.Int, 0))
	tf.assign(tf.b.Place(3, mir.Projection{Kind: mir.ProjIndex, A: 4, Type: types.Int}), use(mir.Imm(types.Int, 1)), tf.at(2, 3))
	tf.assign(place(0), use(mir.Copy(tf.b.Place(1, mir.Projection{Kind: mir.ProjIndex, A: 4, Type: types.Int}))), tf.at(3, 3))
	tf.assign(place(0), use(mir.Copy(tf.b.Place(2, mir.Projection{Kind: mir.ProjIndex, A: 4, Type: types.Int}))), tf.at(4, 3))
	tf.ret(tf.at(5, 3))
	bodies = append(bodies, tf.b)
	u := cCallable(unitOf(e, bodies)) // the C ABI's Indirect slices (internalabi_test.go)
	for i := range u.Funcs {
		if u.Funcs[i].Item == kx.Item {
			u.Funcs[i].Symbol = "KernX"
		}
	}

	win := emit(t, e, u, types.X86_64Windows, types.Release)
	md := map[string]string{}
	for _, m := range regexp.MustCompile(`(?m)^(![0-9]+) = (.*)$`).FindAllStringSubmatch(win, -1) {
		md[m[1]] = m[2]
	}
	scopeName := func(list string) string { // !{!s} -> the scope's name
		s := strings.TrimSuffix(strings.TrimPrefix(md[list], "!{"), "}")
		m := regexp.MustCompile(`"([^"]*)"\}$`).FindStringSubmatch(md[s])
		if m == nil || !strings.HasPrefix(md[s], "distinct !{"+s+", ") {
			t.Fatalf("%s = %s is not a distinct self-referential scope", s, md[s])
		}
		return m[1]
	}
	decl := regexp.MustCompile(`call void @llvm\.experimental\.noalias\.scope\.decl\(metadata (![0-9]+)\)`)
	tagged := regexp.MustCompile(`(store i(?:64|8) [12]|load i(?:64|8)), ptr %v[0-9]+, align [18](?:, !noundef ![0-9]+)?, !alias\.scope (![0-9]+), !noalias (![0-9]+)`)
	for _, k := range []string{"Kern", "KernStr", "KernPair"} {
		fn := fnText(win, "mk.main."+k)
		ds := decl.FindAllStringSubmatch(fn, -1)
		if len(ds) != 2 {
			t.Fatalf("%s: want 2 scope declarations (b and c), got %d:\n%s", k, len(ds), fn)
		}
		b, c := ds[0][1], ds[1][1]
		if scopeName(b) != "mk.main."+k+": param 0" || scopeName(c) != "mk.main."+k+": param 1" {
			t.Errorf("%s: scope names %q, %q", k, scopeName(b), scopeName(c))
		}
		acc := tagged.FindAllStringSubmatch(fn, -1)
		if len(acc) != 3 {
			t.Fatalf("%s: want 3 tagged accesses (two stores to c, a load of b), got %d:\n%s", k, len(acc), fn)
		}
		for _, a := range acc {
			own, other := c, b
			if strings.HasPrefix(a[1], "load") {
				own, other = b, c
			}
			if a[2] != own || a[3] != other {
				t.Errorf("%s: %s has !alias.scope %s !noalias %s, want %s and %s", k, a[1], a[2], a[3], own, other)
			}
		}
		if !strings.Contains(win, "declare void @llvm.experimental.noalias.scope.decl(metadata)") {
			t.Errorf("the intrinsic is not declared")
		}
	}
	for _, k := range []string{"mk.main.KernVar", "mk.main.KernCell", "mk.main.Shared", "KernX"} {
		fn := fnText(win, k)
		if fn == "" {
			t.Fatalf("no %s in the pack", k)
		}
		if strings.Contains(fn, "alias.scope") || strings.Contains(fn, "noalias.scope.decl") {
			t.Errorf("%s is tagged, but its pair is not proved disjoint:\n%s", k, fn)
		}
	}
	// Three: a's and b's loads carry !noalias of c's scope alone, and c's
	// store !noalias of a's and b's.
	members := func(list string) []string {
		return strings.Split(strings.TrimSuffix(strings.TrimPrefix(md[list], "!{"), "}"), ", ")
	}
	three := fnText(win, "mk.main.Three")
	ds := decl.FindAllStringSubmatch(three, -1)
	if len(ds) != 3 {
		t.Fatalf("Three: want 3 scope declarations (a, b and c), got %d:\n%s", len(ds), three)
	}
	sa, sb, sc := members(ds[0][1])[0], members(ds[1][1])[0], members(ds[2][1])[0]
	acc3 := regexp.MustCompile(`(store i64 1|load i64), ptr %v[0-9]+, align 8(?:, !noundef ![0-9]+)?, !alias\.scope (![0-9]+), !noalias (![0-9]+)`).FindAllStringSubmatch(three, -1)
	if len(acc3) != 3 {
		t.Fatalf("Three: want 3 tagged accesses, got %d:\n%s", len(acc3), three)
	}
	want3 := []struct{ own, noalias string }{{sc, sa + ", " + sb}, {sa, sc}, {sb, sc}}
	for i, a := range acc3 {
		if own, na := strings.Join(members(a[2]), ", "), strings.Join(members(a[3]), ", "); own != want3[i].own || na != want3[i].noalias {
			t.Errorf("Three: %s has scope %s and noalias {%s}, want %s and {%s}", a[1], own, na, want3[i].own, want3[i].noalias)
		}
	}
	// System V already passes the data pointers as noalias parameters.
	if ll := emit(t, e, u, types.X86_64Linux, types.Release); strings.Contains(ll, "alias.scope") {
		t.Errorf("x86_64-linux: scoped alias metadata beside the noalias parameters")
	}
	// aarch64 passes a slice as one [2 x i64], but mk builds no aarch64
	// code, so codegen leaves the metadata out there until a benchmark row
	// can show it pays.
	if ll := emit(t, e, u, types.Aarch64Linux, types.Release); strings.Contains(ll, "alias.scope") {
		t.Errorf("aarch64-linux: scoped alias metadata, which only Win64 gets")
	}

	// The fact moves code: after clang -O2 the first store to c[i] is dead
	// in Kern and KernStr (the load of b[j] cannot read it), and both stay
	// in KernVar and KernCell, which carry no fact.
	dir := t.TempDir()
	clangC(t, types.X86_64Windows, dir, "alias", win, "-O2 -S -emit-llvm")
	opt, err := os.ReadFile(filepath.Join(dir, "alias.o"))
	if err != nil {
		t.Fatal(err)
	}
	stores := regexp.MustCompile(`store i(?:64|8) [12], ptr`)
	for k, want := range map[string]int{"Kern": 1, "KernStr": 1, "KernPair": 1, "KernVar": 2, "KernCell": 2} {
		if n := len(stores.FindAllString(fnText(string(opt), "mk.main."+k), -1)); n != want {
			t.Errorf("%s after clang -O2: %d stores to c[i], want %d:\n%s", k, n, want, fnText(string(opt), "mk.main."+k))
		}
	}
}

// TestAliasMarkDropped is the codegen stand-in for TestPC_MutationCaught
// on the metadata's other half: the access rules, not the parameter rules. Each
// body is Name(b: B, c: mut []int, i, j): int { c[i] = 1; t := <read of
// b>; c[i] = 2; return t }, and in each the read must not carry b's scope:
//   - KernDeref, *b[j] with b: []&int: b's element (the pointer) is b's
//     memory and is tagged; the int behind it is not;
//   - KernNested, b[j][j] with b: [][]int: the inner slice's elements are
//     not b's memory;
//   - KernRef, a body that borrows b whole (&b): b is not tagged at all;
//   - KernAsm, a body with asm: nothing is tagged.
//
// After clang -O2 both stores to c[i] stay in every one, because the read
// may alias c[i] as far as LLVM is told.
func TestAliasMarkDropped(t *testing.T) {
	e := newTestEnv()
	ms := e.tt.MutSlice(types.Int)
	kern := func(name string, bt types.TypeID, pre func(f *fnb), read func(f *fnb) mir.Place) *mir.Body {
		f := e.body(e.fn(name, []types.TypeID{bt, ms, types.Int, types.Int}, types.Int, 0))
		if pre != nil {
			pre(f)
		}
		ci := f.b.Place(2, mir.Projection{Kind: mir.ProjIndex, A: 3, Type: types.Int})
		f.assign(ci, use(mir.Imm(types.Int, 1)), f.at(2, 3))
		f.assign(place(0), use(mir.Copy(read(f))), f.at(3, 3))
		f.assign(ci, use(mir.Imm(types.Int, 2)), f.at(4, 3))
		f.ret(f.at(5, 3))
		return f.b
	}
	plain := func(f *fnb) mir.Place {
		return f.b.Place(1, mir.Projection{Kind: mir.ProjIndex, A: 4, Type: types.Int})
	}
	refInt, sl := e.tt.Ref(types.Int), e.tt.Slice(types.Int)
	bodies := []*mir.Body{
		kern("KernDeref", e.tt.Slice(refInt), nil, func(f *fnb) mir.Place {
			return f.b.Place(1, mir.Projection{Kind: mir.ProjIndex, A: 4, Type: refInt},
				mir.Projection{Kind: mir.ProjDeref, Type: types.Int})
		}),
		kern("KernNested", e.tt.Slice(sl), nil, func(f *fnb) mir.Place {
			return f.b.Place(1, mir.Projection{Kind: mir.ProjIndex, A: 4, Type: sl},
				mir.Projection{Kind: mir.ProjIndex, A: 4, Type: types.Int})
		}),
		kern("KernRef", sl, func(f *fnb) {
			r := f.local(e.tt.Ref(sl))
			f.assign(place(r), mir.Rvalue{Kind: mir.RvRef, Borrow: mir.Shared, Place: place(1)}, f.at(1, 3))
		}, plain),
		kern("KernAsm", sl, func(f *fnb) { f.asmBody(mir.Asm{Template: "nop"}, f.at(1, 3)) }, plain),
	}
	win := emit(t, e, cCallable(unitOf(e, bodies)), types.X86_64Windows, types.Release)
	decl := regexp.MustCompile(`noalias\.scope\.decl\(metadata`)
	scoped := regexp.MustCompile(`(?m)^ +(?:%v[0-9]+ = )?(load|store) ([a-z0-9]+).*!alias\.scope`)
	for name, want := range map[string]struct {
		decls  int
		tagged []string // the tagged accesses, in order: kind and type
	}{
		"KernDeref":  {2, []string{"store i64", "load ptr", "store i64"}},
		"KernNested": {2, []string{"store i64", "store i64"}},
		"KernRef":    {0, nil},
		"KernAsm":    {0, nil},
	} {
		fn := fnText(win, "mk.main."+name)
		if n := len(decl.FindAllString(fn, -1)); n != want.decls {
			t.Errorf("%s: %d scope declarations, want %d:\n%s", name, n, want.decls, fn)
		}
		var got []string
		for _, m := range scoped.FindAllStringSubmatch(fn, -1) {
			got = append(got, m[1]+" "+m[2])
		}
		if strings.Join(got, ", ") != strings.Join(want.tagged, ", ") {
			t.Errorf("%s: tagged accesses %q, want %q:\n%s", name, got, want.tagged, fn)
		}
	}
	dir := t.TempDir()
	clangC(t, types.X86_64Windows, dir, "dropped", win, "-O2 -S -emit-llvm")
	opt, err := os.ReadFile(filepath.Join(dir, "dropped.o"))
	if err != nil {
		t.Fatal(err)
	}
	stores := regexp.MustCompile(`store i64 [12], ptr`)
	for _, name := range []string{"KernDeref", "KernNested", "KernRef", "KernAsm"} {
		if n := len(stores.FindAllString(fnText(string(opt), "mk.main."+name), -1)); n != 2 {
			t.Errorf("%s after clang -O2: %d stores to c[i], want 2:\n%s", name, n, fnText(string(opt), "mk.main."+name))
		}
	}
}
