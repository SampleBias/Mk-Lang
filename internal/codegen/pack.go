// Why: the build caches and links object packs one by one, so each pack's
// IR must be a module of its own and byte-identical across runs.
//
// What: emitPack (one llvm.Module per pack), packName and bodySpan.
//
// How: emitPack declares every member, emits the export thunks and each
// function under source.Guard so an ICE names its function, then finishes the
// pack's tables and function rows. The module's source_filename is derived from
// the pack's first member and is never an absolute path.

package codegen

import (
	"fmt"
	"io"
	"sort"

	"mk/internal/llvm"
	"mk/internal/source"
)

func emitPack(u *Unit, p int, env Env, cfg *Config, w io.Writer) (Report, error) {
	var rep Report
	if p < 0 || p >= len(u.Packs) {
		return rep, fmt.Errorf("codegen: pack %d of %d", p, len(u.Packs))
	}
	name := packName(u, p)
	m := llvm.NewModule(name, cfg.triple(), cfg.DataLayout)
	g := newPackGen(u, p, env, cfg, m)
	g.debugUnit(name)
	g.profileSummary()
	if err := guard(name, source.Span{}, g.defineHeaders); err != nil {
		return rep, err
	}
	if err := guard(name, source.Span{}, func() {
		for _, t := range g.cthunks {
			g.emitThunk(t.fi, g.defs[t.fi], t.lk, t.vis, t.dso) // exports.go
		}
	}); err != nil {
		return rep, err
	}
	for _, fi := range u.Packs[p] {
		f := &u.Funcs[fi]
		if err := guard(f.Symbol, bodySpan(f), func() { g.emitFunc(fi) }); err != nil {
			return rep, err
		}
	}
	var werr error
	if err := guard(name, source.Span{}, func() {
		// finish first: a var global's initializer may make a wrapper
		// (an intrinsic as a value), which needs its row.
		g.finish()
		g.importHint() // importhint.go: no row, the linker drops it
		g.funcRows()
		_, werr = m.WriteTo(w)
	}); err != nil {
		return rep, err
	}
	rep.Stale = g.stale
	sort.Strings(rep.Stale)
	return rep, werr
}

// packName is the module's source_filename: never an absolute path.
// The driver's pack name is not in Unit, so it is derived from the first
// member's symbol, which is stable.
func packName(u *Unit, p int) string {
	if len(u.Packs[p]) == 0 {
		return fmt.Sprintf("pack%d", p)
	}
	return u.Funcs[u.Packs[p][0]].Symbol
}

func bodySpan(f *Func) source.Span {
	if f.Body == nil || len(f.Body.Spans) == 0 {
		return source.Span{}
	}
	return f.Body.Spans[0]
}
