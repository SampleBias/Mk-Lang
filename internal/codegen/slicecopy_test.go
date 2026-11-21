// Why: on Win64 a []T, mut []T or str argument is passed as a pointer to a
// fresh copy, and LLVM can skip that copy only if it sees a plain memcpy.
// What: TestSliceArgMemcpy: the copy is an llvm.memcpy of 16 bytes
// into the fresh alloca, so MemCpyOpt (processImmutArgument) passes the
// source to a readonly captures(none) parameter it proves unchanged.
// How: IR checks per target: a var (not readonly) parameter keeps the copy;
// SysV and AAPCS64 pass these values Direct, so nothing changes there.

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

func TestSliceArgMemcpy(t *testing.T) {
	e := newTestEnv()
	slice := e.tt.Slice(types.Int)
	params := []types.TypeID{slice, e.tt.MutSlice(types.Int), types.Str}
	// The callees, in pack 1, so that clang sees only their declarations in
	// pack 0 and cannot inline them away.
	var callees []*mir.Body
	for i, name := range []string{"ISlice", "IMutSlice", "IStr", "IVarSlice"} {
		p := params[min(i, 2)]
		if name == "IVarSlice" {
			p = slice
		}
		f := e.body(e.fn(name, []types.TypeID{p}, types.Void, 0))
		f.b.Locals[1].Var = name == "IVarSlice"
		f.ret(f.at(2, 3))
		callees = append(callees, f.b)
	}
	// Caller(s, m, x) passes each parameter on: Caller's own parameters
	// are readonly captures(none) noalias pointers to its caller's copies.
	cf := e.body(e.fn("Caller", params, types.Void, 0))
	cf.callTo(direct(callees[0].Item), place(0), mir.NoLocal, cf.at(3, 3), mir.Copy(place(1)))
	cf.callTo(direct(callees[1].Item), place(0), mir.NoLocal, cf.at(4, 3), mir.Copy(place(2)))
	cf.callTo(direct(callees[2].Item), place(0), mir.NoLocal, cf.at(5, 3), mir.Copy(place(3)))
	cf.callTo(direct(callees[3].Item), place(0), mir.NoLocal, cf.at(6, 3), mir.Copy(place(1)))
	cf.ret(cf.at(7, 3))
	u := &Unit{Packs: [][]int{{0}, {1, 2, 3, 4}}}
	for i, b := range append([]*mir.Body{cf.b}, callees...) {
		it := e.items.Get(b.Item)
		u.Funcs = append(u.Funcs, Func{Symbol: "mk." + it.Key.String(), Key: it.Key.String(), Item: b.Item, Body: b, Pack: min(i, 1)})
	}
	u = cCallable(u) // the C ABI's Indirect copies (internalabi_test.go)
	win := emitNoValidate(t, e, u, types.X86_64Windows, types.Release)
	caller := fnText(win, "mk.main.Caller")
	copies := regexp.MustCompile(`call void @llvm\.memcpy\.p0\.p0\.i64\(ptr align 8 (%v\d+), ptr align 8 (%p\d), i64 16, i1 false\)`).FindAllStringSubmatch(caller, -1)
	if len(copies) != 4 {
		t.Fatalf("want 4 aligned 16-byte memcpy copies (one per slice-like argument), got %d in\n%s", len(copies), caller)
	}
	want := []string{"%p0", "%p1", "%p2", "%p0"}
	for i, c := range copies {
		if c[2] != want[i] {
			t.Errorf("copy %d is from %s, want %s", i, c[2], want[i])
		}
	}
	if strings.Contains(caller, "store ptr") {
		t.Errorf("a slice word is still stored by hand (storeSlice) at a call:\n%s", caller)
	}
	// The other targets pass the words Direct: no memcpy at all.
	for _, target := range []types.Target{types.X86_64Linux, types.Aarch64Linux} {
		if ll := fnText(emitNoValidate(t, e, u, target, types.Release), "mk.main.Caller"); strings.Contains(ll, "llvm.memcpy") {
			t.Errorf("%s: a slice argument is copied through memory:\n%s", target.Name, ll)
		}
	}

	// After clang -O2, the three readonly parameters get Caller's own
	// pointer (MemCpyOpt forwarded it) and IVarSlice still gets a copy.
	dir := t.TempDir()
	clangC(t, types.X86_64Windows, dir, "slicecopy", win, "-O2 -S -emit-llvm")
	b, err := os.ReadFile(filepath.Join(dir, "slicecopy.o"))
	if err != nil {
		t.Fatal(err)
	}
	opt := fnText(string(b), "mk.main.Caller")
	for i, name := range []string{"ISlice", "IMutSlice", "IStr"} {
		re := regexp.MustCompile(`call void @mk\.main\.` + name + `\(ptr [^%]*%p` + string(rune('0'+i)) + `\)`)
		if !re.MatchString(opt) {
			t.Errorf("clang -O2 did not pass %%p%d straight to %s:\n%s", i, name, opt)
		}
	}
	if !regexp.MustCompile(`call void @mk\.main\.IVarSlice\(ptr [^%]*%[a-z0-9.]*\)`).MatchString(opt) ||
		regexp.MustCompile(`call void @mk\.main\.IVarSlice\(ptr [^%]*%p0\)`).MatchString(opt) {
		t.Errorf("the var parameter's copy was forwarded or lost:\n%s", opt)
	}
}
