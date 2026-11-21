// Why: an allocator's entries are reached only through its table, a const
// std.core.AllocTable, so ThinLTO never imports them into a caller's pack:
// its importer follows the call edges of the summary, and a table's
// function pointers are references, not calls. After import LLVM still
// proves the table and turns the call of its entry into a direct one (the
// table comes in as a read-only constant, and the stores of an inlined
// mem.Fixed.Alloc fold into the loads of Alloc.Bytes), but the entry is
// only declared in that module, so the direct call stays a call: two calls
// and two returns per Box in the fixed-alloc benchmark, which
// std.heap.FixedBufferAllocator in Zig inlines.
//
// What: importHints (the entries a pack's hint names), allocTablesOf,
// isAllocTable and importHint (the pack's mk.importhint function).
//
// How: in a release build, a pack whose function takes the address of a
// const AllocTable (&T, RvGlobalRef), or reaches one that does by direct
// calls of functions that carry the small or the cross-pack sole-call mark
// (smallfn.go: each definition is imported and inlined into its caller, so
// the table becomes a constant there: mem.Fixed.Alloc stores &FixedTable),
// gets one internal function, mk.importhint,
// that calls once each entry of those tables defined in another pack, each
// argument the zero value of its type. A call is an edge of the pack's summary, so ThinLTO
// imports the entry's body into the pack, under its usual instruction
// limit, and a table call LLVM turns direct there can inline.
//
// The hint is noinline and optnone, so the inliner's cost model takes
// nothing into it (an alwaysinline entry is still inlined there, which is
// harmless: the hint is never kept), and it is in @llvm.compiler.used, so the optimizer keeps it, ThinLTO's dead stripping
// included; nothing calls it, so the linker drops it (/OPT:REF,
// --gc-sections) with no FuncTable row, the way an unreferenced function's
// row goes (functable.go), and it never runs. It changes no call site: a
// table call stays indirect unless LLVM proves the table, and becomes the
// direct call LLVM would make anyway; only whether that call can inline
// changes. So the trigger may be loose without risk to the program's
// meaning: a hint that names an entry LLVM never meets costs build time
// only, as an imported body nothing calls is dropped.
//
// No hint is emitted outside release, on macOS (a release Mach-O link
// passes -dead_strip, which should drop it, but no macOS build is linked
// and run here), for a library (a host linker may keep it) or under
// --profile size (imports are inlining). No pack with a member a
// constant-time module reaches (smallSet.ctReach) gets one either: the
// imports go into the whole pack, and those functions' machine code is
// read jump by jump against a recorded reading. In a build, packs split
// by owner module, so std.crypto and std.simd.kernel never share one with
// a trigger anyway.
//
// The hint is a function of the unit's MIR (bodies of other packs, the
// marks): PackFacts hashes its entries, and walkPackConsts gives their
// declarations' types and tail-forward bits like a const's function
// relocation's.

package codegen

import (
	"slices"

	"mk/internal/llvm"
	"mk/internal/mir"
	"mk/internal/types"
)

var keyAllocTable = types.ItemKey("std.core", "AllocTable")

// isAllocTable reports whether t is the named type std.core.AllocTable.
func (g *packGen) isAllocTable(t types.TypeID) bool {
	ty := g.tt.Get(t)
	return ty.Kind == types.KNamed && g.items.Get(ty.Item).Key == keyAllocTable
}

// allocTablesOf adds to seen the AllocTable consts whose address b takes,
// in body order, appending each new one to out.
func (g *packGen) allocTablesOf(b *mir.Body, seen map[types.ConstID]bool, out []types.ConstID) []types.ConstID {
	if b == nil {
		return out
	}
	for i := range b.Blocks {
		for _, s := range b.Blocks[i].Stmts {
			rv := &s.Rvalue
			if s.Kind != mir.StAssign || rv.Kind != mir.RvGlobalRef {
				continue
			}
			d, ok := g.items.Get(rv.Item).Decl.(*types.ConstDecl)
			if !ok || !d.HasValue || seen[d.Value] || !g.isAllocTable(d.Type) {
				continue
			}
			seen[d.Value] = true
			out = append(out, d.Value)
		}
	}
	return out
}

// importHints is the Funcs indexes, ascending, of the entries pack p's
// hint calls (the file comment); nil when the pack gets none.
func (g *packGen) importHints() []int {
	cfg := g.cfg
	if !cfg.Profile.Optimized() || cfg.Library || cfg.Size || cfg.Target.OS == types.OSMacos {
		return nil
	}
	s := g.smallSetOf()
	for _, fi := range g.u.Packs[g.p] {
		if fi >= len(s.ctReach) || s.ctReach[fi] {
			return nil // the imports would reach a constant-time function
		}
	}
	seen := map[types.ConstID]bool{}
	var tables []types.ConstID
	// Each member, and every function its direct calls reach through
	// marked callees, whose bodies inline into it.
	visited := map[int]bool{}
	var work []int
	for _, fi := range g.u.Packs[g.p] {
		if g.u.Funcs[fi].Body != nil && !visited[fi] {
			visited[fi] = true
			work = append(work, fi)
		}
		for len(work) > 0 {
			v := work[len(work)-1]
			work = work[:len(work)-1]
			tables = g.allocTablesOf(g.u.Funcs[v].Body, seen, tables)
			for _, j := range s.calls[v] {
				if (s.small[j] || s.cross[j]) && !s.ctReach[j] && !visited[j] {
					visited[j] = true
					work = append(work, j)
				}
			}
		}
	}
	var out []int
	for _, c := range tables {
		for _, rl := range g.env.Consts().Get(c).Relocs {
			if rl.Kind != types.RelocFn || g.inlineIntrinsic(rl.Fn.Item) {
				continue
			}
			e, ok := g.findFunc(rl.Fn.Item, rl.Fn.Args)
			if !ok {
				continue
			}
			f := &g.u.Funcs[e]
			if f.Pack == g.p || f.Local || f.Body == nil || slices.Contains(out, e) {
				continue
			}
			out = append(out, e)
		}
	}
	slices.Sort(out)
	return out
}

// importHint defines the pack's mk.importhint when importHints names an
// entry: one call of each, with zero arguments, then ret.
func (g *packGen) importHint() {
	es := g.importHints()
	if len(es) == 0 {
		return
	}
	m := g.m
	fn := m.AddFunction("mk.importhint", llvm.FuncSig{Linkage: llvm.Internal, Ret: m.Void(),
		FnAttrs: g.fnAttrs(types.Void).Flag("noinline").Flag("optnone")})
	b := fn.NewBlock("entry")
	for _, e := range es {
		d := g.funcDecl(e)
		sig := d.Sig()
		args := make([]llvm.Value, len(sig.Params))
		for i, t := range sig.Params {
			args[i] = m.ZeroInit(t)
		}
		b.Call(d.Type(), d.Ref(), args, nil, llvm.Attrs{}, llvm.Attrs{})
	}
	b.Ret(nil)
	m.CompilerUsed(m.FuncRef(fn))
}
