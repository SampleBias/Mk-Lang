// Why: packs are compiled and cached apart, so a call into another pack
// must name the callee by a symbol that depends only on stable keys, and
// declare it with the right signature.
//
// What: symIndex and unitIndex (instances by item and arguments), argsKey,
// findFunc and funcIndex, funcDecl (a callee's declaration or definition),
// fnRef (a function's address) and isExtern.
//
// How: the driver hands codegen mono's symbols; codegen finds a callee's
// instance by its item and generic arguments (with the caller's cpu set for
// a feature variant) and declares it once per pack.

package codegen

import (
	"strconv"

	"mk/internal/llvm"
	"mk/internal/source"
	"mk/internal/types"
)

// symIndex maps (item, args) to a Funcs index. The key encodes IDs, which
// are stable within one build; it is never printed or hashed.
type symIndex struct {
	byKey map[string]int
}

func argsKey(item types.ItemID, args []types.GenericArg) string {
	b := strconv.AppendUint(nil, uint64(item), 10)
	for _, a := range args {
		b = append(b, '|')
		if a.IsConst {
			b = append(b, 'c')
			b = strconv.AppendUint(b, uint64(a.Const.Kind), 10)
			b = append(b, ',')
			b = strconv.AppendUint(b, uint64(a.Const.Param), 10)
			b = append(b, ',')
			b = strconv.AppendUint(b, uint64(a.Const.Value), 10)
			b = append(b, ',')
			b = strconv.AppendUint(b, uint64(a.Const.Type), 10)
		} else {
			b = strconv.AppendUint(b, uint64(a.Type), 10)
		}
	}
	return string(b)
}

func unitIndex(u *Unit) *symIndex {
	ix := &symIndex{byKey: make(map[string]int, len(u.Funcs))}
	for i := range u.Funcs {
		f := &u.Funcs[i]
		if extra := keyCPU(f.Key); len(extra) > 0 {
			// A variant inheriting features is found only by
			// callIndex, with the caller's set; the plain key stays the
			// baseline instance's.
			ix.byKey[cpuIndexKey(f.Item, f.Args, extra)] = i
			continue
		}
		ix.byKey[argsKey(f.Item, f.Args)] = i
	}
	return ix
}

// findFunc is the Funcs index of item's instance with args, if the unit
// has it: Unit.Lookup, else the unit's shared exact index.
func (g *packGen) findFunc(item types.ItemID, args []types.GenericArg) (int, bool) {
	if g.u.Lookup != nil {
		return g.u.Lookup(item, args)
	}
	i, ok := g.syms.byKey[argsKey(item, args)]
	return i, ok
}

// funcIndex is the Funcs index of item's instance with args.
func (g *packGen) funcIndex(item types.ItemID, args []types.GenericArg) int {
	if i, ok := g.findFunc(item, args); ok {
		return i
	}
	it := g.items.Get(item)
	source.ICEf(source.Span{}, "codegen: no instance of %s with %d arguments in the unit", it.Key, len(args))
	return -1
}

// funcDecl is the LLVM function of Funcs[fi] in this pack: its definition
// when the pack holds it, else a declaration with the same lowered
// signature (hidden on ELF, dso_local on COFF; an extern by its C name with
// default visibility).
func (g *packGen) funcDecl(fi int) *llvm.Function {
	if f, ok := g.defs[fi]; ok {
		return f
	}
	f := &g.u.Funcs[fi]
	if f.Local && f.Body != nil {
		source.ICEf(source.Span{}, "codegen: %s is Local to pack %d but referenced from pack %d", f.Symbol, f.Pack, g.p)
	}
	if f.Body == nil {
		// The support-pack bodies (CallMain, ErrorName, FuncTable,
		// ProfileTable) are functions of the support pack.
		if fd, ok := g.items.Get(f.Item).Decl.(*types.FuncDecl); ok && fd.Intrinsic && !supportBodies[g.items.Get(f.Item).Key.String()] {
			source.ICEf(source.Span{}, "codegen: intrinsic %s used as a function", g.items.Get(f.Item).Key)
		}
	}
	ls := g.funcSigOf(fi)
	lk, vis, dso := g.linkage(f)
	attrs := llvm.Attrs{}.Flag("nounwind")
	if ls.result == types.Never {
		attrs = attrs.Flag("noreturn")
	}
	sig := g.funcSig(ls, lk, vis, dso, attrs)
	if g.isExtern(f) {
		// Two externs of one C name may disagree on its type, or name a
		// routine codegen calls itself (memcmp): each gets a view of the
		// one declaration with its own type.
		return g.m.DeclareAs(f.Symbol, sig)
	}
	return g.m.Declare(f.Symbol, sig)
}

// fnRef is the address of item's instance with args (RvFnRef, RelocFn); an
// intrinsic's is its wrapper's, whether or not the unit has a Func
// for it, compiled with the features of fi, the function taking the value
// (-1 for a const's relocation).
func (g *packGen) fnRef(item types.ItemID, args []types.GenericArg, fi int) llvm.Value {
	if g.inlineIntrinsic(item) {
		return g.m.FuncRef(g.wrapper(item, args, fi).fn)
	}
	callee := g.funcIndex(item, args)
	if th, ok := g.cthunkFn[callee]; ok {
		// A thunked export of this pack: its value is the C name, the
		// thunk, not the body (exports.go).
		return g.m.FuncRef(th)
	}
	if g.internalABI(callee) {
		// A fn value is a C function pointer: mono marks its function
		// AddrTaken, which keeps the C ABI.
		source.ICEf(source.Span{}, "codegen: %s is a value but has the internal ABI (not AddrTaken)", g.u.Funcs[callee].Symbol)
	}
	return g.m.FuncRef(g.funcDecl(callee))
}

// isExtern reports an extern or export extern, whose symbol is a C name.
func (g *packGen) isExtern(f *Func) bool {
	fd, ok := g.items.Get(f.Item).Decl.(*types.FuncDecl)
	return ok && fd.Sig.Extern
}
