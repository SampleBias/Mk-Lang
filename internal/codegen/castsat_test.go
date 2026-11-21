// Why: SIMD casts, Vec intrinsics as fn values, clobber_abi and asm operands
// each had a codegen bug that only a direct test pins.
// What: TestCastSatWidenClamps ("I to I: clamp and trunc"),
// TestIntrinsicFnValue (the wrapper of a std.simd Vec intrinsic named as
// a fn value), TestClobberABIMaskRegisters (k0 to k7 on sysv64),
// TestSymOperand, TestGlobalAndAllocaAlign, BenchmarkEmitAsm.
// How: genericVec models std.simd's Vec[T, const N]; the IR is checked and
// compiled with clang.

package codegen

import (
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"mk/internal/mir"
	"mk/internal/types"
)

// TestCastSatWidenClamps: CastSat from a signed lane type to a wider
// unsigned one clamps a negative lane to 0 (smax against zero) before it
// zero-extends, as the evaluator's clampTo does; to a wider signed type it
// sign-extends with no clamp.
func TestCastSatWidenClamps(t *testing.T) {
	e := newTestEnv()
	k := newSimdKit(e)
	v8 := k.vec(types.I8, 8)
	v4 := k.vec(types.I32, 4)
	f := e.body(e.fn("Widen", []types.TypeID{v8, v4}, types.Void, 0))
	i8a := []types.GenericArg{types.TypeArg(types.I8), cN(e, 8)}
	i32a := []types.GenericArg{types.TypeArg(types.I32), cN(e, 4)}
	f.call(k.fn("std.simd.Vec:CastSat"), append(append([]types.GenericArg(nil), i8a...), types.TypeArg(types.U16)), k.vec(types.U16, 8), mir.Copy(place(1)))
	f.call(k.fn("std.simd.Vec:CastSat"), append(append([]types.GenericArg(nil), i32a...), types.TypeArg(types.U64)), k.vec(types.U64, 4), mir.Copy(place(2)))
	f.call(k.fn("std.simd.Vec:CastSat"), append(append([]types.GenericArg(nil), i8a...), types.TypeArg(types.I64)), k.vec(types.I64, 8), mir.Copy(place(1)))
	f.ret(f.at(4, 1))
	ll := emit(t, e, unitOf(e, []*mir.Body{f.b}), types.X86_64Windows, types.Dev)
	text := fnText(ll, "mk.main.Widen")
	for _, pair := range [][2]string{
		{"@llvm.smax.v8i8(<8 x i8> ", "zext <8 x i8> "},
		{"@llvm.smax.v4i32(<4 x i32> ", "zext <4 x i32> "},
	} {
		i := strings.Index(text, pair[0])
		j := strings.Index(text, pair[1])
		if i < 0 || j < 0 || j < i {
			t.Errorf("want %q before %q in:\n%s", pair[0], pair[1], text)
		}
		if i >= 0 && !strings.Contains(text[i:], "zeroinitializer)") {
			t.Errorf("the smax of %q is not against zero:\n%s", pair[0], text)
		}
	}
	if n := strings.Count(text, "@llvm.smax"); n != 2 {
		t.Errorf("%d smax calls, want 2 (i8 to i64 only sign-extends):\n%s", n, text)
	}
	if !strings.Contains(text, "sext <8 x i8> ") {
		t.Errorf("i8 to i64 does not sign-extend:\n%s", text)
	}
	clangC(t, types.X86_64Windows, t.TempDir(), "castsat", ll, "-O2")
}

// genericVec is std.simd's Vec[T, const N] with its lanes field [N]T, and
// intrinsic methods declared over it as vec.mk declares them.
type genericVec struct {
	e    *testEnv
	item types.ItemID
	self types.TypeID // Vec[T, N] over the parameters
}

func newGenericVec(e *testEnv) *genericVec {
	nParam := types.ConstArg{Kind: types.CAParam, Param: 1, Type: types.Int}
	d := &types.StructDecl{Fields: []types.FieldDecl{{Name: e.in.Intern("lanes"), Type: e.tt.Array(nParam, e.tt.Param(0))}}}
	item := e.itemIn("std.simd", types.ItemKey("std.simd", "Vec"), "Vec", types.KindStruct, d)
	self := e.tt.Named(item, []types.GenericArg{types.TypeArg(e.tt.Param(0)), types.ConstGenericArg(nParam)})
	return &genericVec{e: e, item: item, self: self}
}

// method declares the bodiless intrinsic Vec[T, N].name(params): result.
func (g *genericVec) method(name string, params []types.TypeID, result types.TypeID) types.ItemID {
	e := g.e
	sig := types.Signature{Name: e.in.Intern(name), Result: result, OwnGenerics: 0,
		Generics: []types.GenericParam{{Name: e.in.Intern("T")}, {Name: e.in.Intern("N"), Const: true, Type: types.Int}}}
	for i, p := range params {
		sig.Params = append(sig.Params, types.Param{Name: e.in.Intern("p" + string(rune('a'+i))), Type: p})
	}
	id := e.itemIn("std.simd", types.MemberKey("std.simd", "Vec", name), name, types.KindFunc,
		&types.FuncDecl{Sig: sig, Bodiless: true, Intrinsic: true})
	it := e.items.Get(id)
	it.Owner = g.item
	it.Decl.(*types.FuncDecl).Sig.Item = id
	return id
}

// at is the Vec type at lane type lane and count n.
func (g *genericVec) at(lane types.TypeID, n int) types.TypeID {
	return g.e.tt.Named(g.item, []types.GenericArg{types.TypeArg(lane), cN(g.e, n)})
}

// TestIntrinsicFnValue: a Vec intrinsic named as a fn value gets a
// wrapper whose signature substitutes the lane count N as well as T (the
// ICE was "instance body holds the generic type Vec[u8, $1]"); a wrapper
// taken in a cpu "fma" function carries that function's target-features,
// so llvm.fma is the instruction, never an fmaf libcall; the module
// compiles on both hosted targets.
func TestIntrinsicFnValue(t *testing.T) {
	e := newTestEnv()
	g := newGenericVec(e)
	tp := e.tt.Param(0)
	splat := g.method("Splat", []types.TypeID{tp}, g.self)
	fma := g.method("Fma", []types.TypeID{g.self, g.self, g.self}, g.self)
	fromArr := g.method("FromArray", []types.TypeID{e.tt.Array(types.ConstArg{Kind: types.CAParam, Param: 1, Type: types.Int}, tp)}, g.self)

	vu8, vf32, vint := g.at(types.U8, 16), g.at(types.F32, 4), g.at(types.Int, 4)
	splatT := e.tt.Fn(types.FnType{Params: []types.FnParam{{Type: types.U8, Into: types.NoParam}}, Result: vu8})
	fmaT := e.tt.Fn(types.FnType{Params: []types.FnParam{{Type: vf32, Into: types.NoParam}, {Type: vf32, Into: types.NoParam}, {Type: vf32, Into: types.NoParam}}, Result: vf32})
	arrT := e.tt.Fn(types.FnType{Params: []types.FnParam{{Type: e.tt.Array(types.ConstArg{Kind: types.CAValue, Value: e.intConst(types.Int, 4)}, types.Int), Into: types.NoParam}}, Result: vint})

	base := e.body(e.fn("TakeSplat", nil, splatT, 0))
	base.assign(place(0), mir.Rvalue{Kind: mir.RvFnRef, Fn: mir.FnRef{Item: splat, Args: base.b.AddGenericArgs(types.TypeArg(types.U8), cN(e, 16))}, Type: splatT}, base.at(1, 1))
	base.ret(base.at(1, 1))

	arr := e.body(e.fn("TakeFromArray", nil, arrT, 0))
	arr.assign(place(0), mir.Rvalue{Kind: mir.RvFnRef, Fn: mir.FnRef{Item: fromArr, Args: arr.b.AddGenericArgs(types.TypeArg(types.Int), cN(e, 4))}, Type: arrT}, arr.at(2, 1))
	arr.ret(arr.at(2, 1))

	fast := e.fn("TakeFma", nil, fmaT, 0)
	set, _ := types.CPUClosure(types.ArchX64, []string{"fma"})
	e.items.Get(fast).Decl.(*types.FuncDecl).Sig.CPU = set
	fb := e.body(fast)
	fb.assign(place(0), mir.Rvalue{Kind: mir.RvFnRef, Fn: mir.FnRef{Item: fma, Args: fb.b.AddGenericArgs(types.TypeArg(types.F32), cN(e, 4))}, Type: fmaT}, fb.at(3, 1))
	fb.ret(fb.at(3, 1))

	for _, target := range []types.Target{types.X86_64Windows, types.X86_64Linux} {
		t.Run(target.Name, func(t *testing.T) {
			ll := emit(t, e, unitOf(e, []*mir.Body{base.b, arr.b, fb.b}), target, types.Release)
			splatW := wrapperText(ll, "mk.std.simd.Vec:Splat.fn.")
			fmaW := wrapperText(ll, "mk.std.simd.Vec:Fma.fn.")
			if splatW == "" || wrapperText(ll, "mk.std.simd.Vec:FromArray.fn.") == "" || fmaW == "" {
				t.Fatalf("a wrapper is missing:\n%s", ll)
			}
			if a := attrGroupOf(ll, fmaW); !strings.Contains(a, "+fma") {
				t.Errorf("the Fma wrapper taken in a cpu \"fma\" function lacks +fma: %s", a)
			}
			if a := attrGroupOf(ll, splatW); strings.Contains(a, "target-features") {
				t.Errorf("the Splat wrapper taken at the baseline has target-features: %s", a)
			}
			if !strings.Contains(fmaW, "@llvm.fma.v4f32") {
				t.Errorf("the Fma wrapper does not call llvm.fma.v4f32:\n%s", fmaW)
			}
			clangC(t, target, t.TempDir(), "fnvalue", ll, "-O2")
			asm := runTool(t, tool(needLLVM(t), "clang"), "--target="+target.LLVMTriple, "-S", "-O2", "-Wno-override-module", "-o", "-", writeLL(t, ll))
			if strings.Contains(asm, "fmaf") {
				t.Errorf("the Fma wrapper calls the fmaf libcall:\n%s", asm)
			}
		})
	}
}

// wrapperText is the definition of the one wrapper whose symbol starts
// with prefix ("" when there is none).
func wrapperText(ll, prefix string) string {
	i := strings.Index(ll, "@\""+prefix)
	if i < 0 {
		return ""
	}
	j := strings.IndexByte(ll[i+2:], '"')
	if j < 0 {
		return ""
	}
	return fnText(ll, ll[i+2:i+2+j])
}

// writeLL writes ll to a file of a fresh temporary directory.
func writeLL(t *testing.T, ll string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "m.ll")
	if err := os.WriteFile(p, []byte(ll), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestClobberABIMaskRegisters: on sysv64 (Linux and freestanding)
// clobber_abi("C") clobbers the AVX-512 mask registers k0 to k7, as
// ~{kN} in a function without avx512f and as discarded ={kN} lateouts in
// one with it (so a kreg input still finds a register); win64 clobbers
// none of them. An explicit k1 output keeps k1 out of the set.
func TestClobberABIMaskRegisters(t *testing.T) {
	for _, tc := range []struct {
		target types.Target
		avx512 bool
		want   []string
		none   bool
	}{
		{types.X86_64Linux, false, []string{"~{k0}", "~{k7}"}, false},
		{types.X86_64Freestanding, false, []string{"~{k0}", "~{k7}"}, false},
		{types.X86_64Linux, true, []string{"={k0}", "={k7}"}, false},
		{types.X86_64Windows, false, nil, true},
	} {
		name := tc.target.Name
		if tc.avx512 {
			name += "-avx512f"
		}
		t.Run(name, func(t *testing.T) {
			e := newTestEnv()
			fn := e.fn("K", []types.TypeID{types.U16}, types.U64, 0)
			ops := []mir.AsmOperand{{Kind: mir.AsmOut, Reg: "rax", Explicit: true, Out: place(0), Type: types.U64}}
			if tc.avx512 {
				set, _ := types.CPUClosure(types.ArchX64, []string{"avx512f"})
				e.items.Get(fn).Decl.(*types.FuncDecl).Sig.CPU = set
				ops = append(ops,
					mir.AsmOperand{Kind: mir.AsmIn, Reg: "kreg", In: mir.Copy(place(1)), Type: types.U16},
					mir.AsmOperand{Kind: mir.AsmOut, Reg: "k1", Explicit: true, Out: mir.Place{Local: mir.NoLocal}, Type: types.U16})
			}
			f := e.body(fn)
			// The template names operands by index, as lower leaves it.
			f.asmBody(mir.Asm{Template: "call {" + strconv.Itoa(len(ops)) + "}", ClobberABI: "C", Operands: append(ops,
				mir.AsmOperand{Kind: mir.AsmSym, Sym: fn})}, f.at(2, 1))
			f.ret(f.at(3, 1))
			ll := emit(t, e, cCallable(unitOf(e, []*mir.Body{f.b})), tc.target, types.Release) // sym: AddrTaken
			text := fnText(ll, "mk.main.K")
			if tc.none {
				if strings.Contains(text, "{k") {
					t.Errorf("win64's clobber_abi names a mask register:\n%s", text)
				}
			} else {
				for _, w := range tc.want {
					if !strings.Contains(text, w) {
						t.Errorf("no %q in:\n%s", w, text)
					}
				}
			}
			if tc.avx512 && strings.Count(text, "{k1}") != 1 {
				t.Errorf("k1, written by an explicit output, is clobbered again:\n%s", text)
			}
			clangC(t, tc.target, t.TempDir(), "kclob", ll, "-O2")
		})
	}
}

// TestSymOperand: a sym operand's hole is its symbol's name in the
// template text (the form for when the `s` constraint is not used), the
// symbol is in @llvm.used, and the module compiles on both hosted targets:
// a function's instance symbol, a var's global, and an extern var's own
// name. clang 21.1.8 also accepts the `s` constraint with ${n:c} for x86_64
// on both triples; the name form stays because older clangs were not
// checked for it.
func TestSymOperand(t *testing.T) {
	for _, target := range []types.Target{types.X86_64Linux, types.X86_64Windows} {
		t.Run(target.Name, func(t *testing.T) {
			e := newTestEnv()
			h := e.body(e.fn("Handler", nil, types.U64, 0))
			h.assign(place(0), use(mir.Imm(types.U64, 7)), h.at(1, 1))
			h.ret(h.at(1, 1))
			v := e.item("Counter", types.KindGlobal, &types.GlobalDecl{Type: types.U64, HasValue: true, Value: e.intConst(types.U64, 3)})
			x := e.item("BootFlag", types.KindGlobal, &types.GlobalDecl{Type: types.U64, Extern: true})
			f := e.body(e.fn("Jump", nil, types.U64, 0))
			r := f.local(types.U64)
			f.asmBody(mir.Asm{Template: "call {1}\nadd rax, qword ptr [rip + {2}]\nadd rax, qword ptr [rip + {3}]", ClobberABI: "C",
				Operands: []mir.AsmOperand{
					{Kind: mir.AsmOut, Reg: "rax", Explicit: true, Out: place(r), Type: types.U64},
					{Kind: mir.AsmSym, Sym: h.item},
					{Kind: mir.AsmSym, Sym: v},
					{Kind: mir.AsmSym, Sym: x},
				}}, f.at(2, 1))
			f.assign(place(0), use(mir.Copy(place(r))), f.at(3, 1))
			f.ret(f.at(3, 1))
			u := cCallable(unitOf(e, []*mir.Body{h.b, f.b})) // sym: AddrTaken
			u.Globals = []Global{{Item: v, Pack: 0}, {Item: x, Pack: 0}}
			ll := emit(t, e, u, target, types.Release)
			text := fnText(ll, "mk.main.Jump")
			if !strings.Contains(text, `"call mk.main.Handler\0Aadd rax, qword ptr [rip + mk.main.Counter]\0Aadd rax, qword ptr [rip + BootFlag]"`) {
				t.Errorf("the sym holes are not the symbols' names:\n%s", text)
			}
			used := ll[strings.Index(ll, "@llvm.used"):]
			for _, sym := range []string{"@mk.main.Handler", "@mk.main.Counter", "@BootFlag"} {
				if !strings.Contains(used[:strings.Index(used, "\n")], sym) {
					t.Errorf("@llvm.used lacks %s:\n%s", sym, used[:strings.Index(used, "\n")])
				}
			}
			if strings.Contains(ll, "define internal i64 @mk.main.Handler") {
				t.Errorf("Handler, named by a sym operand, is internal:\n%s", ll)
			}
			clangC(t, target, t.TempDir(), "sym", ll, "-O2")
		})
	}
}

// TestGlobalAndAllocaAlign: a global of an align(64) struct is aligned
// 64, and so is the stack slot of a local of it, at dev and release; the
// module compiles.
func TestGlobalAndAllocaAlign(t *testing.T) {
	for _, prof := range []types.Profile{types.Dev, types.Release} {
		t.Run(map[types.Profile]string{types.Dev: "dev", types.Release: "release"}[prof], func(t *testing.T) {
			e := newTestEnv()
			bi := e.item("Block", types.KindStruct, &types.StructDecl{Align: 64,
				Fields: []types.FieldDecl{{Name: e.in.Intern("x"), Type: types.U64}, {Name: e.in.Intern("y"), Type: types.U64}}})
			blk := e.tt.Named(bi, nil)
			g := e.item("Table", types.KindGlobal, &types.GlobalDecl{Type: blk, HasValue: true,
				Value: e.consts.Intern(types.ConstValue{Type: blk, Align: 64, Bytes: make([]byte, 64)})})
			f := e.body(e.fn("Use", []types.TypeID{types.U64}, types.U64, 0))
			l := f.local(blk)
			f.assign(place(l), mir.Rvalue{Kind: mir.RvAggregate, Agg: mir.AggStruct, Item: bi, Type: blk,
				Ops: f.b.AddOperands(mir.Copy(place(1)), mir.Imm(types.U64, 2))}, f.at(2, 1))
			p := f.local(e.tt.Ref(blk))
			f.assign(place(p), mir.Rvalue{Kind: mir.RvRef, Borrow: mir.Shared, Place: place(l), Type: e.tt.Ref(blk)}, f.at(3, 1))
			gp := f.local(e.tt.Ref(blk))
			f.assign(place(gp), mir.Rvalue{Kind: mir.RvGlobalRef, Item: g, Type: e.tt.Ref(blk)}, f.at(4, 1))
			ly := f.b.Place(p, mir.Projection{Kind: mir.ProjDeref, Type: blk}, mir.Projection{Kind: mir.ProjField, A: 1, Type: types.U64})
			gx := f.b.Place(gp, mir.Projection{Kind: mir.ProjDeref, Type: blk}, mir.Projection{Kind: mir.ProjField, A: 0, Type: types.U64})
			f.assign(place(0), bin(mir.Add, mir.Copy(ly), mir.Copy(gx)), f.at(5, 1))
			f.ret(f.at(5, 1))
			u := unitOf(e, []*mir.Body{f.b})
			u.Globals = []Global{{Item: g, Pack: 0, Local: true}}
			ll := emit(t, e, u, types.X86_64Linux, prof)
			if !strings.Contains(ll, "@mk.main.Table = ") || !strings.Contains(ll[strings.Index(ll, "@mk.main.Table = "):], "align 64\n") {
				t.Errorf("the align(64) global is not aligned 64:\n%s", ll)
			}
			text := fnText(ll, "mk.main.Use")
			if !strings.Contains(text, "alloca [64 x i8], align 64") && !strings.Contains(text, "x i8], align 64") {
				t.Errorf("the align(64) local's stack slot is not aligned 64:\n%s", text)
			}
			clangC(t, types.X86_64Linux, t.TempDir(), "align", ll, "-O2")
		})
	}
}

// BenchmarkEmitAsm: codegen of one function of
// 10,000 asm statements, each with an output, two inputs, an explicit
// register and clobber_abi("C"). It reports; it gates nothing.
// go test ./internal/codegen -run XXX -bench SY25_EmitAsm
func BenchmarkEmitAsm(b *testing.B) {
	e := newTestEnv()
	f := e.body(e.fn("Many", []types.TypeID{types.U64, types.U64}, types.U64, 0))
	r := f.local(types.U64)
	f.assign(place(r), use(mir.Imm(types.U64, 0)), f.at(1, 1))
	for i := 0; i < 10000; i++ {
		f.asmBody(mir.Asm{Template: "lea {0}, [{1} + {2}]\nxor {0}, {3}", ClobberABI: "C", Operands: []mir.AsmOperand{
			{Kind: mir.AsmInOut, Reg: "reg", In: mir.Copy(place(r)), Out: place(r), Type: types.U64},
			{Kind: mir.AsmIn, Reg: "reg", In: mir.Copy(place(1)), Type: types.U64},
			{Kind: mir.AsmIn, Reg: "reg", In: mir.Copy(place(2)), Type: types.U64},
			{Kind: mir.AsmIn, Reg: "rdx", Explicit: true, In: mir.Copy(place(1)), Type: types.U64},
		}}, f.at(2, 1))
	}
	f.assign(place(0), use(mir.Copy(place(r))), f.at(3, 1))
	f.ret(f.at(3, 1))
	u := unitOf(e, []*mir.Body{f.b})
	cfg := Config{Target: types.X86_64Linux, Profile: types.Release, CPU: "x86-64-v2"}
	if _, err := EmitPack(u, 0, e, cfg, io.Discard); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := EmitPack(u, 0, e, cfg, io.Discard); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/10000, "ns/asm")
}
