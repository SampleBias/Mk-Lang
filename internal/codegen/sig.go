// Why: a Mk function has the platform C ABI wherever C can reach it, so
// any non-generic function value is a C function pointer; its LLVM
// signature must match what clang would give the equivalent C
// declaration, plus the facts Mk's types guarantee. A function only Mk
// direct calls reach may differ where that is faster (internalABI).
//
// What: lsig, lowerSig and lowerSigInternal (an ABI classification as an
// LLVM signature), internalABI,
// sigKey, partTypes, paramFacts, retFacts, pointeeFacts, fnAttrs, cpuAttrs
// and tuneCPU, and funcSig.
//
// How: internal/abi classifies each parameter and the result; this file
// picks the LLVM type of each part (a scalar its SSA type, a lone pointer
// leaf ptr where the ABI allows, else iN, float, double or <2 x float>) and
// adds nonnull, noalias, readonly,
// dereferenceable and range facts only where a part is exactly a Mk
// reference, a slice's data pointer or its length.

package codegen

import (
	"mk/internal/abi"
	"mk/internal/llvm"
	"mk/internal/types"
)

// lsig is a lowered signature as codegen declares, defines and calls it.
type lsig struct {
	s      abi.Sig
	params []types.TypeID // Mk parameter types, receiver first
	result types.TypeID   // the Mk result type: E!T when fallible
	ret    llvm.Type
	rattrs llvm.Attrs
	ltypes []llvm.Type
	lattrs []llvm.Attrs
	// first[i] is the LLVM parameter index of Mk parameter i's first part;
	// sret, when present, is LLVM parameter 0.
	first []int
	sret  bool
	// array[i] is true when Mk parameter i's parts travel as one LLVM array
	// (aarch64 composites); retArray likewise for the result.
	array    []bool
	retArray bool
	variadic bool
}

// lowerSig lowers a Mk signature for the build's target under its C ABI;
// memoized per pack by its types.
func (g *packGen) lowerSig(params []types.TypeID, fixed int, variadic bool, result types.TypeID) *lsig {
	key := sigKey(params, fixed, variadic, result)
	if s, ok := g.sigs[key]; ok {
		return s
	}
	return g.lowerFrom(key, abi.Lower(g.tt, g.env.Layout, g.cfg.Target, params, fixed, variadic, result), params, result)
}

// lowerSigInternal lowers the signature of a function only Mk direct calls
// reach (internalABI) under abi.LowerInternal; memoized beside lowerSig's.
func (g *packGen) lowerSigInternal(params []types.TypeID, result types.TypeID) *lsig {
	key := "i" + sigKey(params, len(params), false, result)
	if s, ok := g.sigs[key]; ok {
		return s
	}
	return g.lowerFrom(key, abi.LowerInternal(g.tt, g.env.Layout, g.cfg.Target, params, result), params, result)
}

// lowerFrom builds and memoizes under key the lsig of abi signature s.
func (g *packGen) lowerFrom(key string, s abi.Sig, params []types.TypeID, result types.TypeID) *lsig {
	ls := &lsig{s: s, params: params, result: result, variadic: s.Variadic}
	m := g.m
	switch s.Ret.Kind {
	case abi.Ignore:
		ls.ret = m.Void()
	case abi.Indirect:
		ls.ret = m.Void()
		ls.sret = true
		ls.ltypes = append(ls.ltypes, m.Ptr())
		// writable and dereferenceable(size), as clang gives a C sret: the
		// slot is the caller's fresh result memory, so LLVM may write it
		// early, and MemCpyOpt's call-slot rule then writes an inner call's
		// result straight into it instead of a temporary and a copy
		// (IndexByteAvx512's 16-byte copy of IndexByteAligned's ?int was a
		// store-forwarding stall per call).
		ra := llvm.Attrs{}.Typed("sret", m.Array(s.Ret.Size, m.I(8))).Flag("noalias").
			Flag("nonnull").Int("align", uint64(max(s.Ret.Align, 1)))
		if s.Ret.Size > 0 {
			ra = ra.Flag("writable").Int("dereferenceable", s.Ret.Size)
		}
		ls.lattrs = append(ls.lattrs, ra)
	case abi.Direct:
		parts := g.partTypes(result, s.Ret, true)
		switch {
		case len(parts) == 1 && (g.cfg.Target.Arch == types.ArchX64 || s.Ret.Parts[0].Class != types.RegFloat || g.reprOf(result).kind == rScalar):
			ls.ret = parts[0]
		case g.cfg.Target.Arch == types.ArchX64:
			ls.ret = m.Struct(false, parts...)
		default:
			ls.ret = m.Array(uint64(len(parts)), parts[0])
			ls.retArray = true
		}
		ls.rattrs = g.retFacts(result, s.Ret)
	}
	for i, pt := range params {
		sl := s.Params[i]
		ls.first = append(ls.first, len(ls.ltypes))
		ls.array = append(ls.array, false)
		switch sl.Kind {
		case abi.Ignore:
		case abi.Indirect:
			ls.ltypes = append(ls.ltypes, m.Ptr())
			if sl.Byval {
				ls.lattrs = append(ls.lattrs, llvm.Attrs{}.Flag("noundef").Typed("byval", m.Array(sl.Size, m.I(8))).
					Int("align", uint64(max(sl.Align, 1))))
			} else {
				a := llvm.Attrs{}.Flag("noundef").Flag("nonnull").Flag("noalias").Int("align", uint64(max(sl.Align, 1)))
				if sl.Size > 0 {
					a = a.Int("dereferenceable", sl.Size)
				}
				ls.lattrs = append(ls.lattrs, a)
			}
		case abi.Direct:
			parts := g.partTypes(pt, sl, false)
			facts := g.paramFacts(pt, sl, len(parts))
			r := g.reprOf(pt)
			arr := g.cfg.Target.Arch == types.ArchArm64 && r.kind != rScalar &&
				(len(parts) > 1 || sl.Parts[0].Class == types.RegFloat)
			if arr {
				elem := parts[0]
				for _, p := range parts {
					if p != elem {
						elem = m.I(64)
					}
				}
				a := llvm.Attrs{}
				if sl.AlignStack != 0 {
					a = a.Int("alignstack", uint64(sl.AlignStack))
				}
				ls.ltypes = append(ls.ltypes, m.Array(uint64(len(parts)), elem))
				ls.lattrs = append(ls.lattrs, a)
				ls.array[i] = true
				continue
			}
			ls.ltypes = append(ls.ltypes, parts...)
			ls.lattrs = append(ls.lattrs, facts...)
		}
	}
	g.sigs[key] = ls
	return ls
}

func sigKey(params []types.TypeID, fixed int, variadic bool, result types.TypeID) string {
	b := make([]byte, 0, 4*len(params)+12)
	put := func(x uint32) { b = append(b, byte(x), byte(x>>8), byte(x>>16), byte(x>>24)) }
	put(uint32(result))
	put(uint32(fixed))
	if variadic {
		b = append(b, 1)
	}
	for _, p := range params {
		put(uint32(p))
	}
	return string(b)
}

// partTypes is the LLVM type of each part of a Direct slot: a scalar
// travels as its SSA type; a part is ptr when it is exactly one pointer leaf
// (System V eightbytes, aarch64 arguments, Win64's two-part slots of
// abi.LowerInternal; never a Win64 C ABI part, never in an aarch64
// result), else iN, float, double or <2 x float>.
func (g *packGen) partTypes(t types.TypeID, sl abi.Slot, result bool) []llvm.Type {
	m := g.m
	if r := g.reprOf(t); r.kind == rScalar && len(sl.Parts) == 1 {
		return []llvm.Type{r.ssa}
	}
	out := make([]llvm.Type, len(sl.Parts))
	stride := partStride(sl)
	for i, p := range sl.Parts {
		off := stride * uint64(i)
		switch {
		case p.Class == types.RegVector:
			out[i] = m.Vector(2, m.Float())
		case p.Class == types.RegFloat && p.Bytes == 4:
			out[i] = m.Float()
		case p.Class == types.RegFloat:
			out[i] = m.Double()
		case p.Bytes == 8 && g.pointerPart(t, off, result, len(sl.Parts) > 1):
			out[i] = m.Ptr()
		default:
			out[i] = m.I(8 * int(p.Bytes))
		}
	}
	return out
}

// pointerPart reports whether the 8 bytes of t at off are one pointer leaf,
// where the target's ABI spells such a part ptr. On Win64 a C ABI Direct
// slot has one part, an internal one (abi.LowerInternal) two: only the
// latter, which no C code sees, spells a pointer ptr.
func (g *packGen) pointerPart(t types.TypeID, off uint64, result, pair bool) bool {
	tg := g.cfg.Target
	if tg.OS == types.OSWindows && !pair || tg.Arch == types.ArchArm64 && result {
		return false
	}
	return g.ptrLeafAt(t, off)
}

func (g *packGen) ptrLeafAt(t types.TypeID, off uint64) bool {
	ty := g.tt.Get(t)
	r := g.reprOf(t)
	switch {
	case r.kind == rScalar:
		return off == 0 && (r.class == sPtr || r.class == sRaw)
	case ty.IsSliceLike():
		return off == 0
	case ty.Kind == types.KArray:
		es, _ := g.sizeAlign(ty.Elem)
		return es > 0 && g.ptrLeafAt(ty.Elem, off%es)
	}
	l := g.layout(t)
	if l.HasTag {
		return false
	}
	for _, f := range l.Fields {
		fs, _ := g.sizeAlign(f.Type)
		if off >= f.Offset && off < f.Offset+fs {
			return g.ptrLeafAt(f.Type, off-f.Offset)
		}
	}
	return false
}

func extAttr(a llvm.Attrs, e abi.Ext) llvm.Attrs {
	switch e {
	case abi.SignExt:
		return a.Flag("signext")
	case abi.ZeroExt:
		return a.Flag("zeroext")
	}
	return a
}

// pointeeFacts are the facts of a reference to t.
func (g *packGen) pointeeFacts(a llvm.Attrs, t types.TypeID) llvm.Attrs {
	size, align := g.sizeAlign(t)
	a = a.Int("align", uint64(max(align, 1)))
	if size > 0 {
		a = a.Int("dereferenceable", size)
	}
	return a
}

// paramFacts are the attributes of a Direct parameter's n LLVM parts:
// a fact goes on a part only when it is exactly the Mk reference pointer, a
// slice's data pointer or its length. A char or enum tag gets no
// range attribute: a tag from C is checked, not assumed, and a
// range on a parameter or result would let LLVM fold that check away.
func (g *packGen) paramFacts(t types.TypeID, sl abi.Slot, n int) []llvm.Attrs {
	out := make([]llvm.Attrs, n)
	ty := g.tt.Get(t)
	r := g.reprOf(t)
	nu := llvm.Attrs{}.Flag("noundef")
	switch {
	case r.kind == rScalar && r.class == sPtr:
		a := g.pointeeFacts(nu.Flag("nonnull"), r.pointee)
		if ty.Kind == types.KMut {
			a = a.Flag("noalias")
		} else if g.env.Facts(r.pointee).Freeze {
			a = a.Flag("noalias").Flag("readonly")
		}
		out[0] = a
	case r.kind == rScalar:
		out[0] = extAttr(nu, sl.Ext)
	case ty.IsSliceLike() && n == 2:
		a := nu.Flag("nonnull")
		switch {
		case ty.Kind == types.KStr:
			a = a.Flag("noalias").Flag("readonly")
		case ty.Kind == types.KMutSlice:
			a = a.Flag("noalias")
		case g.env.Facts(ty.Elem).Freeze:
			a = a.Flag("noalias").Flag("readonly")
		}
		// The length part: [0, 2^56/size], as lenAttach's loads.
		out[0], out[1] = a, nu.Range(g.m.I(64), 0, int64(g.lenMax(t)+1))
	}
	return out
}

// retFacts are a Direct result's attributes: never noalias, never readonly.
func (g *packGen) retFacts(t types.TypeID, sl abi.Slot) llvm.Attrs {
	r := g.reprOf(t)
	if r.kind != rScalar {
		return llvm.Attrs{}
	}
	nu := llvm.Attrs{}.Flag("noundef")
	if r.class == sPtr {
		return g.pointeeFacts(nu.Flag("nonnull"), r.pointee)
	}
	return extAttr(nu, sl.Ext)
}

// fnAttrs are the function attributes of every Mk function:
// nounwind, uwtable, noreturn for never, the CPU and its tuning, inline
// stack probes off Windows, noredzone on freestanding, and the
// general-regs-only features in such a bin (groAttrs); never mustprogress.
func (g *packGen) fnAttrs(result types.TypeID) llvm.Attrs {
	a := llvm.Attrs{}.Flag("nounwind").Flag("uwtable")
	if result == types.Never {
		a = a.Flag("noreturn")
	}
	a = cpuAttrs(a, g.cfg.CPU)
	if g.cfg.Target.OS != types.OSWindows {
		a = a.Str("probe-stack", "inline-asm").Str("stack-probe-size", "4096")
	}
	return fpSizeAttrs(groAttrs(noRedZone(a, g.cfg.Target), g.cfg), g.cfg)
}

// cpuAttrs adds "target-cpu" and "tune-cpu" for cpu ("" adds neither).
// Without "tune-cpu" LLVM tunes for the target CPU, and the tuning of
// x86-64 and x86-64-v2 (the default) has fast SHLD rotates, so every
// llvm.fshl(x, x, c) (raw.RotL64) became shldq instead of rolq. Those two,
// and "generic", are tuned as "generic", as clang does for C without
// -march. x86-64-v3 and v4 are tuned for themselves, as clang's
// -march=x86-64-v3 does (it sets no -tune-cpu): with BMI2 a constant rotate
// is rorxq and a variable one rolq %cl under either tuning, and the level's
// own scheduling model unrolls a vector loop as C's (mandelvec's inner
// loop by 2, where "generic" leaves it alone). A named CPU (--cpu=znver4)
// is tuned for itself, as clang's -march=znver4 does.
func cpuAttrs(a llvm.Attrs, cpu string) llvm.Attrs {
	if cpu == "" {
		return a
	}
	return a.Str("target-cpu", cpu).Str("tune-cpu", tuneCPU(cpu))
}

// tuneCPU is the "tune-cpu" of target CPU cpu (cpuAttrs).
func tuneCPU(cpu string) string {
	switch cpu {
	case "generic", "x86-64", "x86-64-v2":
		return "generic"
	}
	return cpu
}

// funcSig is the llvm.FuncSig of a lowered signature.
func (g *packGen) funcSig(ls *lsig, linkage llvm.Linkage, vis llvm.Visibility, dsoLocal bool, attrs llvm.Attrs) llvm.FuncSig {
	return llvm.FuncSig{Linkage: linkage, Visibility: vis, DSOLocal: dsoLocal, Ret: ls.ret, RetAttrs: ls.rattrs,
		Params: ls.ltypes, ParamAttrs: ls.lattrs, Variadic: ls.variadic, FnAttrs: attrs}
}

// internalABI reports whether Funcs[fi] is declared, defined and called
// under abi.LowerInternal: on x86_64 Windows without --debug (cdb and
// WinDbg read a parameter of more than 8 bytes as a pointer, as MSVC
// passes it, so they would show a register pair wrongly), a Mk function with a body that
// only Mk direct calls reach, each of which lowers it by funcSigOf as its
// definition does. Never an extern or export extern (C names), a function
// whose address is taken (a fn value, asm sym operand, const or export
// root: C may call it), a spawned one (its thunk), a naked or section
// function, one on a `become` edge (a musttail call needs the caller's
// lowering, which a fn value's may not be), Main (the support pack calls
// it by its C ABI), std.task.Start (the entry stub's) or one codegen calls
// by its key (codegenCalled, std.task's FromC pair), and none whose address
// some pack's IR takes (valueFns), whatever AddrTaken says. Every input is
// in the unit digest or, for tailItems and valueFns, in PackFacts per
// member and per direct call.
func (g *packGen) internalABI(fi int) bool {
	tg := g.cfg.Target
	if tg.Arch != types.ArchX64 || tg.OS != types.OSWindows || g.cfg.Debug {
		return false
	}
	f := &g.u.Funcs[fi]
	if f.Body == nil || f.AddrTaken || f.Spawned || g.isExtern(f) || g.nakedFn(f) || g.funcSection(f.Item) != "" ||
		f.Symbol == symTaskStartHook || g.codegenCalled(fi) || g.u.tailItems()[f.Item] || g.valueFns()[fi] {
		return false
	}
	switch k := g.items.Get(f.Item).Key; {
	case k == keyFromCNeeded || k == keyRunFromC, k == types.ItemKey(g.cfg.RootModule, "Main"), !g.cfg.Main.IsZero() && k == g.cfg.Main:
		return false
	}
	return true
}

// valueFns is the set of Funcs whose address some pack's IR takes: every
// function a const reloc, an RvFnRef or an asm sym operand of any pack
// names (walkPackConsts, the walk PackFacts keys a pack's relocs by).
// mono marks most of them AddrTaken, but not all: a section const or var
// is emitted whether or not a body reads it, and an IoTable entry no body
// reads waits in mono unmarked while a direct call brings its function in.
// Memoized under idxMu while Funcs keeps its length.
func (g *packGen) valueFns() map[int]bool {
	u := g.u
	u.idxMu.Lock()
	if u.valsN == len(u.Funcs)+1 {
		s := u.vals
		u.idxMu.Unlock()
		return s
	}
	u.idxMu.Unlock()
	s := map[int]bool{}
	for p := range u.Packs {
		walkPackConsts(u, p, g.env, *g.cfg, func(types.TypeID) {}, func(f types.FnRef) {
			if fi, ok := g.findFunc(f.Item, f.Args); ok {
				s[fi] = true
			}
		})
	}
	u.idxMu.Lock()
	u.vals, u.valsN = s, len(u.Funcs)+1
	u.idxMu.Unlock()
	return s
}
