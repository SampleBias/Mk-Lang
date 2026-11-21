// Why: Win64 passes every aggregate that is not 1, 2, 4 or 8 bytes by
// reference and returns it through sret, so a slice, a str or a ?int
// crosses each call through memory: the caller's two 8-byte stores and the
// callee's 16-byte load of them miss store-to-load forwarding. A Mk
// function no C code can reach needs no C convention for them.
//
// What: LowerInternal, the lowering of a function only Mk direct calls
// reach.
//
// How: Lower, except that on x86_64 Windows a parameter or result of 9 to
// 16 bytes whose two eightbytes System V would classify INTEGER travels as
// System V passes it: Direct, in two integer registers (LLVM's Win64
// lowering takes the parts in argument order and returns two in RAX and
// RDX). Every other target already passes such a value so; there it is
// Lower.

package abi

import "mk/internal/types"

// LowerInternal lowers a non-variadic signature of a function that only Mk
// direct calls reach: never an extern, an export, a fn value, a callback or
// a function another language or the support code calls by symbol. On
// x86_64 Windows a two-eightbyte INTEGER aggregate of 9 to 16 bytes is
// Direct (internalPair); elsewhere it is Lower.
func LowerInternal(tt *types.Table, layout LayoutOf, target types.Target, params []types.TypeID, result types.TypeID) Sig {
	sig := Lower(tt, layout, target, params, len(params), false, result)
	if target.Arch != types.ArchX64 || target.OS != types.OSWindows {
		return sig
	}
	v := newView(tt, layout, target)
	if sl, ok := internalPair(v, result, sig.Ret); ok {
		sig.Ret = sl
	}
	for i, p := range params {
		if sl, ok := internalPair(v, p, sig.Params[i]); ok {
			sig.Params[i] = sl
		}
	}
	return sig
}

// internalPair is the Direct slot of an Indirect Win64 slot sl of type t
// when t is an aggregate of 9 to 16 bytes, aligned to at most 8, whose
// leaves are integers and pointers of at most 8 bytes at aligned offsets:
// System V's two INTEGER eightbytes.
func internalPair(v *view, t types.TypeID, sl Slot) (Slot, bool) {
	if sl.Kind != Indirect || sl.Size <= 8 || sl.Size > 16 || sl.Align > 8 {
		return Slot{}, false
	}
	if _, ok := v.scalarOf(t); ok {
		return Slot{}, false
	}
	fl := v.flatten(t)
	if fl.unaligned {
		return Slot{}, false
	}
	for _, l := range fl.leaves {
		if l.class == leafFloat || l.size > 8 {
			return Slot{}, false
		}
	}
	c := sysvClassify(v, t)
	if c.memory || c.needSSE != 0 || len(c.slot.Parts) != 2 {
		return Slot{}, false
	}
	return c.slot, true
}
