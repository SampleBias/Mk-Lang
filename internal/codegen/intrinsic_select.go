// Why: a binary search or a branch-free partition picks its next index or
// source by a comparison whose outcome is random, so it must be a
// conditional move; x86's cmov converter turns a select in a loop into a
// branch when the next load depends on it, which on random keys is
// mispredicted half the time.
//
// What: selectUnpredictable, the lowering of std.raw.Select (std/raw/raw.mk).
//
// How: a select marked !unpredictable, the IR of Rust's
// core::hint::select_unpredictable and of Zig's @branchHint(.unpredictable),
// which the converter leaves a cmov. A scalar T selects the value; an
// aggregate T selects the address of a or b and copies from it; a zero-size
// T emits nothing.

package codegen

import (
	"mk/internal/llvm"
	"mk/internal/mir"
	"mk/internal/source"
	"mk/internal/types"
)

// selectUnpredictable stores args[1] into Dest when args[0] holds, else
// args[2], with no branch, and continues at Target.
func (fg *fnGen) selectUnpredictable(t *mir.Terminator, args []value, destT types.TypeID) {
	m := fg.g.m
	if len(args) != 3 || args[0].kind != rScalar || args[1].kind != args[2].kind {
		source.ICEf(fg.spanOf(t.Span), "codegen: intrinsic std.raw.Select takes (bool, T, T) (intrinsicSigs: %s)", intrinsicSigs["std.raw.Select"])
	}
	c := args[0].v
	if c.Ty.Bits() != 1 {
		c = fg.cur.ICmp(llvm.INE, c, m.ConstInt(c.Ty, 0))
	}
	hint := m.Metadata().Unpredictable()
	switch args[1].kind {
	case rScalar:
		d, _ := fg.placeAddr(t.Dest)
		fg.store(d, value{t: destT, kind: rScalar, v: fg.cur.Select(c, args[1].v, args[2].v, hint)})
	case rAgg:
		a, b := args[1].a, args[2].a
		d, _ := fg.placeAddr(t.Dest)
		fg.store(d, value{t: destT, kind: rAgg, a: addr{v: fg.cur.Select(c, a.v, b.v, hint), align: min(a.align, b.align)}})
	}
	fg.cur.Br(fg.blocks[t.Target])
}
