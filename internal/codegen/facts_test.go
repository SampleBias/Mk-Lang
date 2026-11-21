// Why: a parameter attribute such as noalias or readonly is a promise to
// LLVM; a wrong one miscompiles.
// What: TestAttributes: each row of the facts table and its negative,
// and no noalias or readonly on any golden's return.
// How: paramAttrs reads the attributes from the emitted definitions.

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

// paramAttrs are the attribute texts of a definition's parameters.
func paramAttrs(t *testing.T, ll, symbol string) (ret string, params []string) {
	t.Helper()
	re := regexp.MustCompile(`(?m)^define ([^@]*)@"?` + regexp.QuoteMeta(symbol) + `"?\((.*)\) [a-z]`)
	m := re.FindStringSubmatch(ll)
	if m == nil {
		t.Fatalf("no definition of %s", symbol)
	}
	for _, p := range strings.Split(m[2], ", ") {
		params = append(params, regexp.MustCompile(` %p\d+$`).ReplaceAllString(p, ""))
	}
	return m[1], params
}

// TestAttributes: no readonly or noalias through a non-Freeze pointee
// such as a Cell; no nonnull on ?&T or *T; no pointee facts on an aggregate
// passed indirectly.
func TestAttributes(t *testing.T) {
	e := newTestEnv()
	pt := e.structT("Point", types.Int, types.Int)
	cell := e.structT("CellInt", types.Int)
	e.noFreeze[cell] = true
	big := e.structT("Big", e.tt.Ref(types.Int), types.Int, types.Int, types.Int)
	type row struct {
		name  string
		param types.TypeID
		want  []string // the attribute text of each LLVM part, in order
	}
	rows := []row{
		{"RefFrozen", e.tt.Ref(pt), []string{"ptr noundef nonnull align 8 dereferenceable(16) noalias readonly"}},
		{"RefCell", e.tt.Ref(cell), []string{"ptr noundef nonnull align 8 dereferenceable(8)"}},
		{"MutPoint", e.tt.Mut(pt), []string{"ptr noundef nonnull align 8 dereferenceable(16) noalias"}},
		{"MutCell", e.tt.Mut(cell), []string{"ptr noundef nonnull align 8 dereferenceable(8) noalias"}},
		{"OptRef", e.tt.Optional(e.tt.Ref(types.Int)), []string{"ptr noundef"}},
		{"Raw", e.tt.RawPtr(types.Int), []string{"ptr noundef"}},
		{"Fn", e.tt.Fn(types.FnType{Result: types.Void}), []string{"ptr noundef"}},
		{"SliceInt", e.tt.Slice(types.Int), []string{"ptr noundef nonnull noalias readonly", "i64 noundef range(i64 0, 9007199254740993)"}},
		{"SliceCell", e.tt.Slice(cell), []string{"ptr noundef nonnull", "i64 noundef range(i64 0, 9007199254740993)"}},
		{"MutSlice", e.tt.MutSlice(types.Int), []string{"ptr noundef nonnull noalias", "i64 noundef range(i64 0, 9007199254740993)"}},
		{"Str", types.Str, []string{"ptr noundef nonnull noalias readonly", "i64 noundef range(i64 0, 72057594037927937)"}},
		{"SliceZero", e.tt.Slice(e.structT("Z0")), []string{"ptr noundef nonnull noalias readonly", "i64 noundef range(i64 0, -9223372036854775808)"}}, // [0, 2^63)
		{"Bool", types.Bool, []string{"i1 noundef zeroext"}},
		{"I8", types.I8, []string{"i8 noundef signext"}},
		{"Big", big, []string{"ptr noundef byval([32 x i8]) align 8"}},
		{"Pair", pt, []string{"i64", "i64"}},
	}
	var bodies []*mir.Body
	for _, r := range rows {
		f := e.body(e.fn("P"+r.name, []types.TypeID{r.param}, types.Void, 0))
		f.ret(f.at(2, 3))
		bodies = append(bodies, f.b)
	}
	// First(p: &Point): &int returns a reborrow: return facts, never noalias.
	fr := e.body(e.fn("First", []types.TypeID{e.tt.Ref(pt)}, e.tt.Ref(types.Int), 0))
	fr.assign(place(0), mir.Rvalue{Kind: mir.RvRef, Place: fr.b.Place(1, mir.Projection{Kind: mir.ProjDeref, Type: pt},
		mir.Projection{Kind: mir.ProjField, A: 0, Type: types.Int})}, fr.at(4, 3))
	fr.ret(fr.at(4, 3))
	bodies = append(bodies, fr.b)
	ll := emit(t, e, unitOf(e, bodies), types.X86_64Linux, types.Dev)
	for _, r := range rows {
		_, got := paramAttrs(t, ll, "mk.main.P"+r.name)
		if strings.Join(got, ", ") != strings.Join(r.want, ", ") {
			t.Errorf("%s: %q, want %q", r.name, got, r.want)
		}
	}
	ret, _ := paramAttrs(t, ll, "mk.main.First")
	if ret != "hidden noundef nonnull align 8 dereferenceable(8) ptr " {
		t.Errorf("First returns %q", ret)
	}
	// Win64: an aggregate over 8 bytes is a pointer to a fresh copy, with
	// the copy's facts and none of its fields'; a non-var parameter
	// is also readonly and captures(none) (immutarg_test.go).
	win := emit(t, e, cCallable(unitOf(e, bodies)), types.X86_64Windows, types.Dev) // the C ABI
	if _, got := paramAttrs(t, win, "mk.main.PBig"); len(got) != 1 || got[0] != "ptr noundef nonnull noalias align 8 dereferenceable(32) readonly captures(none)" {
		t.Errorf("Win64 Big: %q", got)
	}
	// The slice's own facts (its elements are readonly) never reach the
	// pointer to its header; the header is readonly as a non-var parameter.
	if _, got := paramAttrs(t, win, "mk.main.PSliceInt"); len(got) != 1 || got[0] != "ptr noundef nonnull noalias align 8 dereferenceable(16) readonly captures(none)" {
		t.Errorf("Win64 slice (indirect): %q", got)
	}
	for _, target := range types.Targets() {
		clangC(t, target, t.TempDir(), "facts", emit(t, e, unitOf(e, bodies), target, types.Release), "-O2")
	}
	// No return attribute noalias or readonly in any golden.
	files, _ := filepath.Glob(filepath.Join("testdata", "*.ll.golden"))
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(b), "\n") {
			if !strings.HasPrefix(line, "define ") && !strings.HasPrefix(line, "declare ") {
				continue
			}
			head := line[:strings.Index(line, "@")]
			if strings.Contains(head, "noalias") || strings.Contains(head, "readonly") {
				t.Errorf("%s: return attribute in %s", f, line)
			}
		}
	}
}
