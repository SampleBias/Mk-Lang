// Why: s.go runs a call on another task through a thunk and an argument
// block, so the thunk's ABI and the block's layout must match exactly.
// What: the spawn golden (s.go of a direct callee, of a fallible callee into
// Scope[E], through a fn value; the scope intrinsics and StJoin),
// TestSpawnThunkABI (the thunk's declaration is abi.Lower of fn(*Io, *u8,
// *u8): bool, per target) and TestSpawnThunkRun.
// How: the run test links a stub std.task that runs the thunk at once and
// checks the argument block, the thunk's loads and the error it writes,
// natively on Windows and WSL Linux.

package codegen

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"mk/internal/mir"
	"mk/internal/types"
)

func init() {
	emitCases = append(emitCases, emitCase{"spawn", spawnCase})
}

// spawnStd is the std surface a spawn needs: Io, Scope, Limits, StackSize,
// Site, the scope intrinsics and the std.task entries of 18 2.2.
type spawnStd struct {
	io, scope, limits, stack types.TypeID
	ioScope, ioScopeOf       types.ItemID
	ioScopeWith, cancel      types.ItemID
	bodiless                 []types.ItemID
}

func newSpawnStd(e *testEnv) *spawnStd {
	s := &spawnStd{}
	s.io = e.structIn("std.core", "Io", e.tt.RawPtr(types.U8), types.Uint)
	arr := e.tt.Array(types.ConstArg{Kind: types.CAValue, Value: e.intConst(types.Int, 16)}, types.U64)
	s.scope = e.structIn("std.core", "Scope", arr)
	e.noFreeze[s.scope] = true // a Scope is not Freeze (check)
	sd := &types.EnumDecl{Variants: []types.VariantDecl{{Name: e.in.Intern("Default"), Discr: 0}, {Name: e.in.Intern("Large"), Discr: 1}}}
	s.stack = e.tt.Named(e.itemIn("std.core", types.ItemKey("std.core", "StackSize"), "StackSize", types.KindEnum, sd), nil)
	s.limits = e.structIn("std.core", "Limits", types.Int, s.stack)
	site := e.structIn("std.rt", "Site", types.Str, types.U32, types.U32)
	ref := e.tt.Ref(s.scope)
	p8 := e.tt.RawPtr(types.U8)
	ft := e.tt.Fn(types.FnType{Params: []types.FnParam{{Type: e.tt.RawPtr(s.io)}, {Type: p8}, {Type: p8}}, Result: types.Bool})
	s.bodiless = []types.ItemID{
		e.fnIn("std.task", "", "Enter", []types.TypeID{p8, s.io, types.Int, s.stack, types.Int, e.tt.Ref(site)}, types.Void, false),
		e.fnIn("std.task", "", "SpawnAlloc", []types.TypeID{ref, types.Int, types.Int, e.tt.Ref(site)}, p8, false),
		e.fnIn("std.task", "", "Spawn", []types.TypeID{ref, ft, p8}, types.Void, false),
		e.fnIn("std.task", "", "Join", []types.TypeID{ref, types.Bool}, types.Void, false),
		e.fnIn("std.task", "", "Cancel", []types.TypeID{ref}, types.Void, false),
	}
	s.ioScope = e.fnIn("std.core", "Io", "Scope", []types.TypeID{s.io}, s.scope, true)
	s.ioScopeOf = e.fnIn("std.core", "Io", "ScopeOf", []types.TypeID{s.io}, s.scope, true)
	s.ioScopeWith = e.fnIn("std.core", "Io", "ScopeWith", []types.TypeID{s.io, s.limits}, s.scope, true)
	s.cancel = e.fnIn("std.core", "Scope", "Cancel", []types.TypeID{ref}, types.Void, true)
	return s
}

// call is a TermCall from f's current block to a new block, which becomes
// current.
func (f *fnb) callTo(c mir.Callee, dest mir.Place, spawn mir.LocalID, sp mir.SpanID, args ...mir.Operand) {
	next := f.newBlock()
	var as []mir.Arg
	for _, a := range args {
		as = append(as, mir.Arg{Operand: a})
	}
	f.term(mir.Terminator{Kind: mir.TermCall, Callee: c, Args: f.b.AddArgs(as...), Dest: dest, Target: next,
		Spawn: spawn, Span: sp})
	f.cur = next
}

func direct(item types.ItemID) mir.Callee {
	return mir.Callee{Kind: mir.CallDirect, Fn: mir.FnRef{Item: item}}
}

// scopeLocal adds the LocalScope local of a scope statement.
func (f *fnb) scopeLocal(t types.TypeID) mir.LocalID {
	return f.b.AddLocal(mir.Local{Type: t, Kind: mir.LocalScope, Loop: mir.NoLoop})
}

func (f *fnb) join(s mir.LocalID, cancel bool, sp mir.SpanID) {
	blk := &f.b.Blocks[f.cur]
	blk.Stmts = append(blk.Stmts, mir.Statement{Kind: mir.StJoin, Local: s, Cancel: cancel, Span: sp})
}

// spawnBodies are the functions of the spawn golden and run test:
//
//	Work(io: Io, n: int) { PrintNum(n) }
//	Run(io: Io) { scope s := io.Scope() { s.go Work(io, 7) } }
//	Fail(n: int): TooBig!void { return TooBig(n) }
//	RunE(io: Io) { scope s := io.ScopeOf[TooBig]() { s.go Fail(9) } }
//	RunV(io: Io, f: fn(int)) { scope s := io.Scope() { s.go f(3) } }
//	RunW(io: Io, l: Limits) { scope s := io.ScopeWith(l) { s.Cancel() } } (joins cancelling)
func spawnBodies(e *testEnv) ([]*mir.Body, []types.ItemID, types.ErrorSetID) {
	s := newSpawnStd(e)
	pn := e.fn("PrintNum", []types.TypeID{types.Int}, types.Void, 0)
	e.items.Get(pn).Decl.(*types.FuncDecl).Sig.Extern = true
	e.items.Get(pn).Decl.(*types.FuncDecl).Bodiless = true

	w := e.body(e.fn("Work", []types.TypeID{s.io, types.Int}, types.Void, 0))
	w.callTo(direct(pn), place(0), mir.NoLocal, w.at(2, 3), mir.Copy(place(2)))
	w.ret(w.at(2, 3))

	r := e.body(e.fn("Run", []types.TypeID{s.io}, types.Void, 0))
	sl := r.scopeLocal(s.scope)
	v := r.local(types.Void)
	r.callTo(direct(s.ioScope), place(sl), mir.NoLocal, r.at(4, 8), mir.Copy(place(1)))
	r.callTo(direct(w.item), place(v), sl, r.at(4, 30), mir.Copy(place(1)), mir.Imm(types.Int, 7))
	r.join(sl, false, r.at(4, 40))
	r.ret(r.at(4, 40))

	tooBig := e.errItem("TooBig", types.Int)
	set := e.tt.ErrorSet([]types.ErrorMember{{Item: tooBig}})
	fl := e.body(e.fn("Fail", []types.TypeID{types.Int}, types.Void, set))
	el := fl.local(e.tt.ErrorSetType(set))
	fl.assign(place(el), mir.Rvalue{Kind: mir.RvAggregate, Agg: mir.AggError, Item: tooBig,
		Ops: fl.b.AddOperands(mir.Copy(place(1)))}, fl.at(6, 3))
	fl.assign(place(0), mir.Rvalue{Kind: mir.RvAggregate, Agg: mir.AggErr, Ops: fl.b.AddOperands(mir.Move(place(el)))}, fl.at(6, 3))
	fl.ret(fl.at(6, 3))

	re := e.body(e.fn("RunE", []types.TypeID{s.io}, types.Void, 0))
	scopeE := e.tt.Named(e.tt.Get(s.scope).Item, []types.GenericArg{types.TypeArg(e.tt.ErrorSetType(set))})
	e.noFreeze[scopeE] = true
	se := re.scopeLocal(scopeE)
	ve := re.local(types.Void)
	ga := re.b.AddGenericArgs(types.TypeArg(e.tt.ErrorSetType(set)))
	re.callTo(mir.Callee{Kind: mir.CallDirect, Fn: mir.FnRef{Item: s.ioScopeOf, Args: ga}}, place(se), mir.NoLocal, re.at(8, 8), mir.Copy(place(1)))
	re.callTo(direct(fl.item), place(ve), se, re.at(8, 30), mir.Imm(types.Int, 9))
	re.join(se, false, re.at(8, 40))
	re.ret(re.at(8, 40))

	fnT := e.tt.Fn(types.FnType{Params: []types.FnParam{{Type: types.Int}}, Result: types.Void})
	rv := e.body(e.fn("RunV", []types.TypeID{s.io, fnT}, types.Void, 0))
	sv := rv.scopeLocal(s.scope)
	vv := rv.local(types.Void)
	rv.callTo(direct(s.ioScope), place(sv), mir.NoLocal, rv.at(10, 8), mir.Copy(place(1)))
	rv.callTo(mir.Callee{Kind: mir.CallValue, Value: mir.Copy(place(2))}, place(vv), sv, rv.at(10, 30), mir.Imm(types.Int, 3))
	rv.join(sv, false, rv.at(10, 40))
	rv.ret(rv.at(10, 40))

	rw := e.body(e.fn("RunW", []types.TypeID{s.io, s.limits}, types.Void, 0))
	sw := rw.scopeLocal(s.scope)
	ref := rw.local(e.tt.Ref(s.scope))
	vw := rw.local(types.Void)
	rw.callTo(direct(s.ioScopeWith), place(sw), mir.NoLocal, rw.at(12, 8), mir.Copy(place(1)), mir.Copy(place(2)))
	rw.assign(place(ref), mir.Rvalue{Kind: mir.RvRef, Place: place(sw)}, rw.at(12, 30))
	rw.callTo(direct(s.cancel), place(vw), mir.NoLocal, rw.at(12, 30), mir.Copy(place(ref)))
	rw.join(sw, true, rw.at(12, 40))
	rw.ret(rw.at(12, 40))

	bodiless := append([]types.ItemID{pn}, s.bodiless...)
	return []*mir.Body{w.b, r.b, fl.b, re.b, rv.b, rw.b}, bodiless, set
}

func spawnCase(e *testEnv) ([]*mir.Body, []types.ItemID) {
	b, bl, _ := spawnBodies(e)
	return b, bl
}

// TestSpawnThunkABI: for each target, the printed declaration of a thunk
// equals the lowering of fn(*Io, *u8, *u8): bool as a CallValue through
// that type prints it.
func TestSpawnThunkABI(t *testing.T) {
	head := regexp.MustCompile(`(?m)^define internal (.*) @"?mk\.main\.Work\.go"?\(([^)]*)\)`)
	for _, target := range types.Targets() {
		t.Run(target.Name, func(t *testing.T) {
			e := newTestEnv()
			bodies, bodiless, _ := spawnBodies(e)
			// Call(f: fn(*Io, *u8, *u8): bool): bool { return f(null, null, null) }
			io := e.tt.Named(mustItem(t, e, "std.core.Io"), nil)
			p8 := e.tt.RawPtr(types.U8)
			ft := e.tt.Fn(types.FnType{Params: []types.FnParam{{Type: e.tt.RawPtr(io)}, {Type: p8}, {Type: p8}}, Result: types.Bool})
			c := e.body(e.fn("CallThunk", []types.TypeID{ft, e.tt.RawPtr(io), p8}, types.Bool, 0))
			c.callTo(mir.Callee{Kind: mir.CallValue, Value: mir.Copy(place(1))}, place(0), mir.NoLocal, c.at(20, 3),
				mir.Copy(place(2)), mir.Copy(place(3)), mir.Copy(place(3)))
			c.ret(c.at(20, 3))
			ll := emit(t, e, unitOf(e, append(bodies, c.b), bodiless...), target, types.Dev)
			m := head.FindStringSubmatch(ll)
			if m == nil {
				t.Fatalf("no thunk definition of mk.main.Work in\n%s", ll)
			}
			call := regexp.MustCompile(`call (.*) %v\d+\((.*)\) nounwind`).FindStringSubmatch(fnText(ll, "mk.main.CallThunk"))
			if call == nil {
				t.Fatal("no call through the fn value")
			}
			params := regexp.MustCompile(` %p\d+`).ReplaceAllString(m[2], "")
			args := regexp.MustCompile(` %v\d+`).ReplaceAllString(call[2], "")
			if m[1] != call[1] || params != args {
				t.Errorf("thunk %q (%q), CallValue %q (%q)", m[1], params, call[1], args)
			}
			if !strings.Contains(m[1], "i1") {
				t.Errorf("thunk result %q is not the i1 of bool", m[1])
			}
		})
	}
}

func mustItem(t *testing.T, e *testEnv, key string) types.ItemID {
	id, ok := e.items.ByKey(types.KeyFromString(key))
	if !ok {
		t.Fatalf("no item %s", key)
	}
	return id
}

// spawnStub is the stub std.task of TestSpawnThunkRun: Enter prints 100,
// limit, stack, err_size; SpawnAlloc prints 200, size, align and returns a
// static block; Spawn runs the thunk at once with err at the scope's
// err field (offset 128) and prints 300, its result, the error code and the
// i64 payload at err+8 (err is null when Enter's err_size was 0); Join prints 400 and cancel; Cancel prints 500.
func spawnStub(target types.Target) string {
	enter := `define void @mk.std.task.Enter(ptr %s, ptr %t, i64 %f, i64 %limit, i8 %stack, i64 %es, ptr %site) nounwind {`
	if target.OS == types.OSWindows {
		enter = `define void @mk.std.task.Enter(ptr %s, ptr %io, i64 %limit, i8 %stack, i64 %es, ptr %site) nounwind {`
	}
	return `
@blk = internal global [64 x i8] zeroinitializer, align 16
@errsize = internal global i64 0, align 8

define internal void @h_pn(i64 %v) nounwind {
  call void @h_num(i64 %v)
  call void @h_write(ptr @s.nl, i64 1)
  ret void
}

define void @PrintNum(i64 %n) nounwind {
  call void @h_pn(i64 %n)
  ret void
}

` + enter + `
  call void @h_pn(i64 100)
  call void @h_pn(i64 %limit)
  %st = zext i8 %stack to i64
  call void @h_pn(i64 %st)
  call void @h_pn(i64 %es)
  store i64 %es, ptr @errsize, align 8
  ret void
}

define ptr @mk.std.task.SpawnAlloc(ptr %s, i64 %size, i64 %align, ptr %site) nounwind {
  call void @h_pn(i64 200)
  call void @h_pn(i64 %size)
  call void @h_pn(i64 %align)
  ret ptr @blk
}

define void @mk.std.task.Spawn(ptr %s, ptr %thunk, ptr %b) nounwind {
  %es = load i64, ptr @errsize, align 8
  %has = icmp ne i64 %es, 0
  %at = getelementptr inbounds i8, ptr %s, i64 128
  %err = select i1 %has, ptr %at, ptr null
  br i1 %has, label %clear, label %call
clear:
  store i32 0, ptr %err, align 4
  br label %call
call:
  %r = call i1 %thunk(ptr null, ptr %b, ptr %err)
  call void @h_pn(i64 300)
  %r64 = zext i1 %r to i64
  call void @h_pn(i64 %r64)
  br i1 %r, label %failed, label %done
failed:
  %code = load i32, ptr %err, align 4
  %c64 = zext i32 %code to i64
  call void @h_pn(i64 %c64)
  %pp = getelementptr inbounds i8, ptr %err, i64 8
  %pay = load i64, ptr %pp, align 8
  call void @h_pn(i64 %pay)
  br label %done
done:
  ret void
}

define void @mk.std.task.Join(ptr %s, i1 zeroext %c) nounwind {
  call void @h_pn(i64 400)
  %c64 = zext i1 %c to i64
  call void @h_pn(i64 %c64)
  ret void
}

define void @mk.std.task.Cancel(ptr %s) nounwind {
  call void @h_pn(i64 500)
  ret void
}
`
}

// spawnDriver is the harness Start: Run, RunE, RunV with PrintNum, RunW
// with Limits{4, Large} and with Limits{0, Default}.
func spawnDriver(target types.Target) string {
	if target.OS == types.OSWindows {
		return `
declare void @mk.main.Run(ptr)
declare void @mk.main.RunE(ptr)
declare void @mk.main.RunV(ptr, ptr)
declare void @mk.main.RunW(ptr, ptr)

define void @"mk.std.task.Start"(ptr %sp) noreturn nounwind {
  %io = alloca [16 x i8], align 8
  call void @llvm.memset.p0.i64(ptr %io, i8 0, i64 16, i1 false)
  %c1 = alloca [16 x i8], align 8
  call void @llvm.memcpy.p0.p0.i64(ptr %c1, ptr %io, i64 16, i1 false)
  call void @mk.main.Run(ptr %c1)
  call void @llvm.memcpy.p0.p0.i64(ptr %c1, ptr %io, i64 16, i1 false)
  call void @mk.main.RunE(ptr %c1)
  call void @llvm.memcpy.p0.p0.i64(ptr %c1, ptr %io, i64 16, i1 false)
  call void @mk.main.RunV(ptr %c1, ptr @PrintNum)
  %l = alloca [16 x i8], align 8
  store i64 4, ptr %l, align 8
  %ls = getelementptr inbounds i8, ptr %l, i64 8
  store i8 1, ptr %ls, align 8
  call void @llvm.memcpy.p0.p0.i64(ptr %c1, ptr %io, i64 16, i1 false)
  call void @mk.main.RunW(ptr %c1, ptr %l)
  store i64 0, ptr %l, align 8
  store i8 0, ptr %ls, align 8
  call void @llvm.memcpy.p0.p0.i64(ptr %c1, ptr %io, i64 16, i1 false)
  call void @mk.main.RunW(ptr %c1, ptr %l)
  call void @h_exit(i32 0)
  unreachable
}

declare void @llvm.memset.p0.i64(ptr, i8, i64, i1)
declare void @llvm.memcpy.p0.p0.i64(ptr, ptr, i64, i1)
`
	}
	return `
declare void @mk.main.Run(ptr, i64)
declare void @mk.main.RunE(ptr, i64)
declare void @mk.main.RunV(ptr, i64, ptr)
declare void @mk.main.RunW(ptr, i64, i64, i64)

define void @"mk.std.task.Start"(ptr %sp) noreturn nounwind {
  call void @mk.main.Run(ptr null, i64 0)
  call void @mk.main.RunE(ptr null, i64 0)
  call void @mk.main.RunV(ptr null, i64 0, ptr @PrintNum)
  call void @mk.main.RunW(ptr null, i64 0, i64 4, i64 1)
  call void @mk.main.RunW(ptr null, i64 0, i64 0, i64 0)
  call void @h_exit(i32 0)
  unreachable
}
`
}

// TestSpawnThunkRun runs the spawn bodies natively with the stub std.task:
// the block holds Io then 7 at offsets 0 and 16 (24 bytes, align 8); the
// thunk of Fail writes TooBig(9) into Scope[TooBig]'s err (err_size 16) and
// returns true; a fn value travels in slot 0; ScopeWith passes l.tasks, or
// -1 for tasks < 1, and l.stack.
func TestSpawnThunkRun(t *testing.T) {
	for _, target := range runTargets {
		for _, opt := range []string{"-O0", "-O2"} {
			t.Run(target.Name+opt, func(t *testing.T) {
				if _, why := hostRunner(target); why != "" {
					skipRunner(t, why)
				}
				e := newTestEnv()
				bodies, bodiless, set := spawnBodies(e)
				ll := emit(t, e, cCallable(unitOf(e, bodies, bodiless...)), target, types.Dev) // the harness calls them
				dir := t.TempDir()
				osIR, kernel := harnessOS(target)
				h := osIR + harnessRT + spawnStub(target) + spawnDriver(target)
				objs := []string{
					clangC(t, target, dir, "support", supportLL(t, target), "-O2"),
					clangC(t, target, dir, "pack", ll, opt),
					clangC(t, target, dir, "harness", h, "-O0"),
				}
				exe := linkExe(t, target, dir, objs, linkOpts{kernel: kernel})
				out, code := runExe(t, target, exe, "")
				tooBig := types.ErrorCode(types.ItemKey("main", "TooBig"))
				_ = set
				lines := []string{
					"100", "0", "0", "0", "200", "24", "8", "7", "300", "0", "400", "0", // Run
					"100", "0", "0", "16", "200", "8", "8", "300", "1", fmt.Sprint(tooBig), "9", "400", "0", // RunE
					"100", "0", "0", "0", "200", "16", "8", "3", "300", "0", "400", "0", // RunV
					"100", "4", "1", "0", "500", "400", "1", // RunW{4, Large}
					"100", "-1", "0", "0", "500", "400", "1", // RunW{0, Default}
				}
				want := strings.Join(lines, "\n") + "\n"
				if code != 0 || out != want {
					t.Fatalf("exit %d, output\n%s\nwant\n%s", code, out, want)
				}
			})
		}
	}
}
