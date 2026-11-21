// Why: Mk's asm statement, naked functions and the section clause on
// functions all lower to LLVM inline assembly or function attributes, and
// LLVM's constraint strings and template syntax differ from Mk's.
//
// What: asmStmt (one call of an inline-asm value per StAsm), asmTemplate,
// the register class and modifier tables, clobberABI, emitNaked and
// nakedAttrs, and sectionOf.
//
// How: constraints go in the order outputs, inputs, clobbers, with an
// inout's input tied to its output. A naked function is the asm call and
// unreachable, with no prologue. Only a pure asm gets memory(none) or
// memory(read); any other keeps a write to inaccessible memory, so LLVM
// never deletes it.

package codegen

import (
	"strconv"
	"strings"

	"mk/internal/llvm"
	"mk/internal/mir"
	"mk/internal/source"
	"mk/internal/types"
)

// asmClasses is the LLVM constraint of each register class.
var asmClasses = map[string]string{
	"reg": "r", "reg_byte": "q", "xmm_reg": "x", "ymm_reg": "x", "zmm_reg": "v", "kreg": "^Yk",
	"vreg": "w", // aarch64's v0 to v31
}

// asmModifiers maps a template modifier to LLVM's x86 operand modifier
// (r 64-bit, e 32-bit, x 16-bit, l low byte, y ymm, z zmm).
var asmModifiers = map[string]string{"r": "q", "e": "k", "x": "w", "l": "b", "y": "t", "z": "g"}

// asmModifiersArm64 are aarch64's template modifiers, LLVM's own names: w
// and x a general register's 32- and 64-bit names, b, h, s, d and q a
// vector register's 8- to 128-bit scalar names.
var asmModifiersArm64 = map[string]string{"w": "w", "x": "x", "b": "b", "h": "h", "s": "s", "d": "d", "q": "q"}

// clobberABI is the caller-saved set of clobber_abi("C") per C ABI:
// sysv64 on Linux and freestanding (k0 to k7 included: a C callee may
// change an AVX-512 mask register), win64 on Windows.
func clobberABI(win bool) []string {
	var out []string
	if win {
		out = append(out, "rax", "rcx", "rdx", "r8", "r9", "r10", "r11")
		for i := 0; i <= 5; i++ {
			out = append(out, "xmm"+strconv.Itoa(i))
		}
		return out
	}
	out = append(out, "rax", "rcx", "rdx", "rsi", "rdi", "r8", "r9", "r10", "r11")
	for i := 0; i <= 15; i++ {
		out = append(out, "xmm"+strconv.Itoa(i))
	}
	out = append(out, "st")
	for i := 1; i <= 7; i++ {
		out = append(out, "st("+strconv.Itoa(i)+")")
	}
	for i := 0; i <= 7; i++ {
		out = append(out, "mm"+strconv.Itoa(i))
	}
	for i := 0; i <= 7; i++ {
		out = append(out, "k"+strconv.Itoa(i)) // the AVX-512 masks
	}
	return out
}

// clobberABIArm64 is AAPCS64's caller-saved set: x0 to x17 and x30 (the
// link register a call overwrites), and q0 to q31 whole. A callee keeps
// only the low halves of v8 to v15, so the whole registers are clobbered;
// x18, the platform register, is never named. The x and q registers become
// discarded lateouts, as x86_64's do, so an input may still be given one;
// the link register, which LLVM allocates to no operand, a clobber.
func clobberABIArm64() []string {
	var out []string
	for i := 0; i <= 17; i++ {
		out = append(out, "x"+strconv.Itoa(i))
	}
	out = append(out, "x30")
	for i := 0; i <= 31; i++ {
		out = append(out, "q"+strconv.Itoa(i))
	}
	return out
}

// arm64Family is the 64-bit (or q) register an aarch64 register name is
// part of: w5 is x5, v3, d3 and s3 are q3, lr is x30; any other name is
// returned lowered.
func arm64Family(r string) string {
	r = strings.ToLower(r)
	if r == "lr" {
		return "x30"
	}
	if len(r) >= 2 && r[1] >= '0' && r[1] <= '9' {
		switch r[0] {
		case 'w':
			return "x" + r[1:]
		case 'v', 'd', 's':
			return "q" + r[1:]
		}
	}
	return r
}

// hasFeature reports whether function fi is compiled with feature: its
// cpu set, inherited features included, or the target's baseline.
func (g *packGen) hasFeature(fi int, feature string) bool {
	for _, f := range types.CPUBaseline(g.cfg.Target) {
		if f == feature {
			return true
		}
	}
	for _, f := range g.cpuOf(fi) {
		if f == feature {
			return true
		}
	}
	return false
}

// regFamily is the 64-bit (or xmm) register an explicit register name is
// part of, so an output in eax keeps clobber_abi from clobbering rax.
func regFamily(r string) string {
	r = strings.ToLower(r)
	switch r {
	case "eax", "ax", "al", "ah":
		return "rax"
	case "ecx", "cx", "cl", "ch":
		return "rcx"
	case "edx", "dx", "dl", "dh":
		return "rdx"
	case "esi", "si", "sil":
		return "rsi"
	case "edi", "di", "dil":
		return "rdi"
	}
	if strings.HasPrefix(r, "r") && len(r) >= 2 && r[1] >= '0' && r[1] <= '9' {
		return strings.TrimRight(r, "dwb")
	}
	if strings.HasPrefix(r, "ymm") || strings.HasPrefix(r, "zmm") {
		return "xmm" + r[3:]
	}
	return r
}

func asmReads(k mir.AsmOperandKind) bool {
	return k == mir.AsmIn || k == mir.AsmInOut || k == mir.AsmInLateOut
}

func asmWrites(k mir.AsmOperandKind) bool {
	return k == mir.AsmOut || k == mir.AsmLateOut || k == mir.AsmInOut || k == mir.AsmInLateOut
}

// asmConstraint is a direction operand's register constraint, without the
// output prefix.
func asmConstraint(op *mir.AsmOperand) string {
	if op.Explicit {
		return "{" + op.Reg + "}"
	}
	if c, ok := asmClasses[op.Reg]; ok {
		return c
	}
	return "r"
}

// asmVec reports whether t is a std.simd Vec or Mask (an aggregate of lanes)
// and gives its LLVM vector type: <N x T> for a Vec, <N x iW> for a Mask
// (its memory form, lanes of all zeros or all ones).
func (g *packGen) asmVec(t types.TypeID) (llvm.Type, bool) {
	ty := g.tt.Get(t)
	if ty.Kind != types.KNamed || len(ty.Args) != 2 || ty.Args[0].IsConst {
		return llvm.Type{}, false
	}
	key := g.items.Get(ty.Item).Key.String()
	if key != "std.simd.Vec" && key != "std.simd.Mask" {
		return llvm.Type{}, false
	}
	lane := ty.Args[0].Type
	size, _ := g.sizeAlign(t)
	ls, _ := g.sizeAlign(lane)
	if ls == 0 {
		return llvm.Type{}, false
	}
	n := int(size / ls)
	if key == "std.simd.Mask" {
		return g.m.Vector(n, g.m.I(int(ls)*8)), true
	}
	return g.m.Vector(n, g.reprOf(lane).ssa), true
}

// asmType is an operand's LLVM type: a scalar's SSA type (bool as i8), a
// vector's <N x T>.
func (g *packGen) asmType(t types.TypeID) llvm.Type {
	if vt, ok := g.asmVec(t); ok {
		return vt
	}
	r := g.reprOf(t)
	if r.class == sBool {
		return r.mem
	}
	return r.ssa
}

// asmIn is an input operand's value at its asm type.
func (fg *fnGen) asmIn(o mir.Operand, t types.TypeID) llvm.Value {
	g := fg.g
	v := fg.operand(o)
	if vt, ok := g.asmVec(t); ok {
		a := fg.addrOf(v)
		_, al := g.sizeAlign(g.tt.Get(t).Args[0].Type)
		return fg.cur.Load(vt, a.v, max(al, 1))
	}
	r := g.reprOf(t)
	if r.class == sBool && v.v.Ty.Bits() == 1 {
		return fg.cur.Cast(llvm.ZExt, v.v, r.mem)
	}
	return v.v
}

// asmOut stores an output value into its place.
func (fg *fnGen) asmOut(p mir.Place, t types.TypeID, x llvm.Value) {
	g := fg.g
	d, _ := fg.placeAddr(p)
	if _, ok := g.asmVec(t); ok {
		_, al := g.sizeAlign(g.tt.Get(t).Args[0].Type)
		fg.cur.Store(x, d.v, max(al, 1))
		return
	}
	fg.storeScalar(d, g.reprOf(t), x)
}

// asmSymbol is the link name a sym operand names, kept in @llvm.used: a
// function's instance symbol, a var's global, an extern var's own name.
func (g *packGen) asmSymbol(item types.ItemID, at source.Span) string {
	it := g.items.Get(item)
	switch d := it.Decl.(type) {
	case *types.FuncDecl:
		fi := g.funcIndex(item, nil)
		if g.internalABI(fi) {
			source.ICEf(at, "codegen: asm names %s, which has the internal ABI (not AddrTaken)", g.u.Funcs[fi].Symbol)
		}
		fn := g.funcDecl(fi)
		g.m.Used(g.m.FuncRef(fn))
		return fn.Name()
	case *types.GlobalDecl:
		v := g.varGlobal(item, d)
		g.m.Used(v)
		if d.Extern {
			return symbolName(it.Key)
		}
		return "mk." + it.Key.String()
	}
	source.ICEf(at, "codegen: sym operand %s is no function or var", it.Key)
	return ""
}

// asmStmt emits a. Outputs come first in the constraint list, then
// inputs (an inout's input tied to its output), then clobbers: clobber_abi's
// set less the registers written, the flags unless preserves_flags, memory
// unless nomem or readonly. A discarded output in an explicit register is a
// clobber of it. The template's {i} and {i:m} become ${n} and ${n:m'} for
// the operand's LLVM number n; a sym operand's hole is its symbol's name.
// A noreturn asm's block ends in the unreachable of its MIR terminator.
func (fg *fnGen) asmStmt(a *mir.Asm) {
	g, m := fg.g, fg.g.m
	at := fg.spanOf(a.Span)
	var outCons, inCons, clobbers []string
	var outTys []llvm.Type
	var ins []llvm.Value
	type outSlot struct {
		place mir.Place
		t     types.TypeID
		keep  bool
	}
	var outs []outSlot
	num := make([]int, len(a.Operands))
	syms := make([]string, len(a.Operands))
	written := map[string]bool{}
	for i := range a.Operands {
		op := &a.Operands[i]
		num[i] = -1
		if !asmWrites(op.Kind) {
			continue
		}
		if op.Explicit {
			written[regFamily(op.Reg)] = true
			written[arm64Family(op.Reg)] = true
		}
		if op.Out.Local == mir.NoLocal && op.Explicit && !asmReads(op.Kind) {
			clobbers = append(clobbers, "~{"+op.Reg+"}")
			continue
		}
		c := "="
		if op.Kind == mir.AsmOut || op.Kind == mir.AsmInOut {
			c += "&" // early clobber: it may not share an input's register
		}
		num[i] = len(outCons)
		outCons = append(outCons, c+asmConstraint(op))
		outTys = append(outTys, g.asmType(op.Type))
		outs = append(outs, outSlot{place: op.Out, t: op.Type, keep: op.Out.Local != mir.NoLocal})
	}
	if a.ClobberABI != "" {
		// A caller-saved register no operand writes is a discarded lateout
		// ={reg}, not ~{reg}: LLVM allocates no input to a clobbered
		// register, and on sysv64 every xmm register is caller-saved, so an
		// xmm_reg input next to clobber_abi("C") would find none (Rust's
		// asm! lowers clobber_abi the same way). x87 and MMX stay clobbers,
		// and so do the mask registers k0 to k7 where the function's set
		// lacks avx512f: no kreg operand can be there (mono's E1104
		// feature), and LLVM has no mask register type to give a lateout.
		kOut := fg.fi >= 0 && g.hasFeature(fg.fi, "avx512f")
		arm := g.cfg.Target.Arch == types.ArchArm64
		set := clobberABI(g.cfg.Target.OS == types.OSWindows)
		if arm {
			set = clobberABIArm64()
		}
		for _, r := range set {
			switch {
			case arm && written[arm64Family(r)]:
			case arm && r == "x30":
				clobbers = append(clobbers, "~{lr}") // LLVM allocates no operand to the link register
			case arm && r[0] == 'q':
				outCons, outTys = append(outCons, "={"+r+"}"), append(outTys, m.Vector(2, m.I(64)))
				outs = append(outs, outSlot{})
			case arm:
				outCons, outTys = append(outCons, "={"+r+"}"), append(outTys, m.I(64))
				outs = append(outs, outSlot{})
			case written[regFamily(r)]:
			case strings.HasPrefix(r, "st") || strings.HasPrefix(r, "mm"):
				clobbers = append(clobbers, "~{"+r+"}")
			case r[0] == 'k' && !kOut:
				clobbers = append(clobbers, "~{"+r+"}")
			case r[0] == 'k':
				outCons, outTys = append(outCons, "={"+r+"}"), append(outTys, m.I(16))
				outs = append(outs, outSlot{})
			case strings.HasPrefix(r, "xmm"):
				outCons, outTys = append(outCons, "={"+r+"}"), append(outTys, m.Double())
				outs = append(outs, outSlot{})
			default:
				outCons, outTys = append(outCons, "={"+r+"}"), append(outTys, m.I(64))
				outs = append(outs, outSlot{})
			}
		}
	}
	nOut := len(outCons)
	for i := range a.Operands {
		op := &a.Operands[i]
		switch op.Kind {
		case mir.AsmIn:
			num[i] = nOut + len(inCons)
			inCons = append(inCons, asmConstraint(op))
			ins = append(ins, fg.asmIn(op.In, op.Type))
		case mir.AsmInOut, mir.AsmInLateOut:
			inCons = append(inCons, strconv.Itoa(num[i]))
			ins = append(ins, fg.asmIn(op.In, op.Type))
		case mir.AsmSym:
			syms[i] = g.asmSymbol(op.Sym, at)
		}
	}
	if a.Options&mir.AsmPreservesFlags == 0 {
		if g.cfg.Target.Arch == types.ArchArm64 {
			clobbers = append(clobbers, "~{cc}")
		} else {
			clobbers = append(clobbers, "~{dirflag}", "~{fpsr}", "~{flags}")
		}
	}
	if a.Options&(mir.AsmNomem|mir.AsmReadonly) == 0 {
		clobbers = append(clobbers, "~{memory}")
	}
	cons := strings.Join(append(append(outCons, inCons...), clobbers...), ",")

	var ret llvm.Type
	switch len(outTys) {
	case 0:
		ret = m.Void()
	case 1:
		ret = outTys[0]
	default:
		ret = m.Struct(false, outTys...)
	}
	inTys := make([]llvm.Type, len(ins))
	for i, v := range ins {
		inTys[i] = v.Ty
	}
	// nomem and readonly say what the asm does to Mk's memory, not that it
	// has no other effect: memory(none) or memory(read) on a sideeffect asm
	// lets LLVM delete it when its results are unused (an input-only
	// `out dx, al`, `lidt` or `cli; hlt` vanished on freestanding). As
	// rustc does (rustc_codegen_llvm asm.rs), only a pure asm
	// gets them; any other keeps a write to inaccessible memory, which
	// orders it without making Mk's memory clobbered.
	pure := a.Options&mir.AsmPure != 0
	attrs := llvm.Attrs{}.Flag("nounwind")
	switch {
	case a.Options&mir.AsmNomem != 0 && pure:
		attrs = attrs.Flag("memory(none)")
	case a.Options&mir.AsmNomem != 0:
		attrs = attrs.Flag("memory(inaccessiblemem: readwrite)")
	case a.Options&mir.AsmReadonly != 0 && pure:
		attrs = attrs.Flag("memory(read)")
	case a.Options&mir.AsmReadonly != 0:
		attrs = attrs.Flag("memory(read, inaccessiblemem: readwrite)")
	}
	if a.Options&mir.AsmNoreturn != 0 {
		attrs = attrs.Flag("noreturn")
	}
	fl := llvm.AsmFlags{SideEffect: a.Options&mir.AsmPure == 0, AlignStack: a.Options&mir.AsmNostack == 0,
		Intel: a.Options&mir.AsmAtt == 0 && g.cfg.Target.Arch == types.ArchX64}
	mods := asmModifiers
	if g.cfg.Target.Arch == types.ArchArm64 {
		mods = asmModifiersArm64
	}
	res := fg.cur.InlineAsmDialect(m.FuncType(ret, inTys, false), asmTemplate(a, num, syms, mods), cons, fl, ins, attrs)
	for k, o := range outs {
		if !o.keep {
			continue
		}
		x := res
		if len(outTys) > 1 {
			x = fg.cur.ExtractValue(res, k)
		}
		fg.asmOut(o.place, o.t, x)
	}
}

// asmTemplate is a's template in LLVM's form: {i} is ${n},
// {i:m} is ${n:m'}, a sym operand's hole its symbol, {{ and }} braces, and
// $ is $$. With options(raw) only $ is escaped.
func asmTemplate(a *mir.Asm, num []int, syms []string, mods map[string]string) string {
	t := a.Template
	var sb strings.Builder
	raw := a.Options&mir.AsmRaw != 0
	for i := 0; i < len(t); i++ {
		c := t[i]
		switch {
		case c == '$':
			sb.WriteString("$$")
		case raw:
			sb.WriteByte(c)
		case (c == '{' || c == '}') && i+1 < len(t) && t[i+1] == c:
			sb.WriteByte(c)
			i++
		case c == '{':
			j := strings.IndexByte(t[i+1:], '}')
			if j < 0 {
				sb.WriteString(t[i:])
				return sb.String()
			}
			hole := t[i+1 : i+1+j]
			idx, mod, _ := strings.Cut(hole, ":")
			k, err := strconv.Atoi(idx)
			switch {
			case err != nil || k < 0 || k >= len(num):
				sb.WriteString("{" + hole + "}")
			case syms[k] != "":
				sb.WriteString(syms[k])
			case num[k] < 0:
				sb.WriteString("{" + hole + "}")
			default:
				sb.WriteString("${" + strconv.Itoa(num[k]))
				if m := mods[mod]; m != "" {
					sb.WriteString(":" + m)
				}
				sb.WriteByte('}')
			}
			i += j + 1
		default:
			sb.WriteByte(c)
		}
	}
	return sb.String()
}

// nakedFn reports whether f is a naked function.
func (g *packGen) nakedFn(f *Func) bool {
	fd, ok := g.items.Get(f.Item).Decl.(*types.FuncDecl)
	return ok && fd.Naked
}

// nakedAttrs are a naked function's attributes: naked, noinline,
// nounwind, the build's CPU and noredzone on freestanding; no stack
// probe, no unwind table, no PGO.
func (g *packGen) nakedAttrs() llvm.Attrs {
	return noRedZone(cpuAttrs(llvm.Attrs{}.Flag("naked").Flag("noinline").Flag("nounwind"), g.cfg.CPU), g.cfg.Target)
}

// emitNaked emits a naked function: an entry block holding its asm
// call and unreachable, with no allocas, no parameter stores, no counters
// and no debug records. Its MIR is the StAsm and TermUnreachable (lower);
// its asm takes only const and sym operands (check).
func (g *packGen) emitNaked(fi int) {
	f := &g.u.Funcs[fi]
	fg := &fnGen{g: g, f: f, fi: fi, b: f.Body, fn: g.defs[fi], sig: g.funcSigOf(fi)}
	fg.entry = fg.fn.NewBlock("entry")
	fg.cur = fg.entry
	for i := range f.Body.Blocks {
		for si := range f.Body.Blocks[i].Stmts {
			if s := &f.Body.Blocks[i].Stmts[si]; s.Kind == mir.StAsm {
				fg.span = s.Span
				fg.asmStmt(&f.Body.Asms[s.Asm])
			}
		}
	}
	fg.cur.Unreachable()
}

// funcSection is a function's section clause, "" for none.
func (g *packGen) funcSection(item types.ItemID) string {
	if fd, ok := g.items.Get(item).Decl.(*types.FuncDecl); ok {
		return fd.Section
	}
	return ""
}

// sectionOf places a function with a section clause in its section and in
// @llvm.used, so neither the optimizer nor --gc-sections drops it.
func (g *packGen) sectionOf(f *Func, fn *llvm.Function) {
	if s := g.funcSection(f.Item); s != "" {
		fn.SetSection(s)
		g.m.Used(g.m.FuncRef(fn))
	}
}
