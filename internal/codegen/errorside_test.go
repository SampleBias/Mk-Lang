// Why: the error side of E!T is not always laid out as E's error-set value
// (T = i64 puts the error payload at offset 8 in Small!i64 and at 4 in the
// set), so it cannot always be viewed in place.
// What: TestErrorSideCopy and TestErrorSideWriteICE.
// How: in that case the SelErr view is a copy, the error edge of a call
// copies per error, AggErr builds per error, and a write through the view
// is an ICE.

package codegen

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"mk/internal/mir"
	"mk/internal/types"
)

// r1Bodies:
//
//	MkErr(n: u8): Small!i64 { return Small(n) }
//	EdgePay(n: u8): u8 { _ := MkErr(n) else e { return e.Small.0 }; return 0 }
//	ViewPay(x: Small!i64): u8 { return x.err.Small.0 }   (x's error side read through SelErr)
func r1Bodies(e *testEnv) ([]*mir.Body, types.TypeID, types.TypeID, types.ItemID) {
	small := e.errItem("Small", types.U8)
	set := e.tt.ErrorSet([]types.ErrorMember{{Item: small}})
	st := e.tt.ErrorSetType(set)
	ut := e.tt.ErrorUnion(set, types.I64)

	mk := e.body(e.fn("MkErr", []types.TypeID{types.U8}, types.I64, set))
	el := mk.local(st)
	mk.assign(place(el), mir.Rvalue{Kind: mir.RvAggregate, Agg: mir.AggError, Item: small, Ops: mk.b.AddOperands(mir.Copy(place(1)))}, mk.at(2, 3))
	mk.assign(place(0), mir.Rvalue{Kind: mir.RvAggregate, Agg: mir.AggErr, Ops: mk.b.AddOperands(mir.Move(place(el)))}, mk.at(2, 3))
	mk.ret(mk.at(2, 3))

	ep := e.body(e.fn("EdgePay", []types.TypeID{types.U8}, types.U8, 0))
	v, er := ep.local(types.I64), ep.local(st)
	okB, errB := ep.newBlock(), ep.newBlock()
	ep.term(mir.Terminator{Kind: mir.TermCall, Callee: direct(mk.item), Args: ep.b.AddArgs(mir.Arg{Operand: mir.Copy(place(1))}),
		Dest: place(v), Target: okB, HasErr: true, Err: mir.ErrorEdge{Kind: mir.EdgeElse, Block: errB, Error: er},
		Spawn: mir.NoLocal, Span: ep.at(4, 8)})
	ep.cur = errB
	ep.assign(place(0), use(mir.Copy(ep.b.Place(er, mir.Projection{Kind: mir.ProjDowncast, Sel: mir.SelCase, A: uint32(small), Type: st},
		mir.Projection{Kind: mir.ProjField, A: 0, Type: types.U8}))), ep.at(4, 30))
	ep.ret(ep.at(4, 30))
	ep.cur = okB
	ep.assign(place(0), use(mir.Imm(types.U8, 0)), ep.at(5, 3))
	ep.ret(ep.at(5, 3))

	vp := e.body(e.fn("ViewPay", []types.TypeID{ut}, types.U8, 0))
	vp.assign(place(0), use(mir.Copy(vp.b.Place(1, mir.Projection{Kind: mir.ProjDowncast, Sel: mir.SelErr, Type: st},
		mir.Projection{Kind: mir.ProjDowncast, Sel: mir.SelCase, A: uint32(small), Type: st},
		mir.Projection{Kind: mir.ProjField, A: 0, Type: types.U8}))), vp.at(7, 3))
	vp.ret(vp.at(7, 3))
	return []*mir.Body{mk.b, ep.b, vp.b}, ut, st, small
}

func TestErrorSideCopy(t *testing.T) {
	e := newTestEnv()
	bodies, ut, st, _ := r1Bodies(e)
	if lu, ls := e.Layout(ut), e.Layout(st); lu.Variants[1].Fields[0].Offset == ls.Variants[0].Fields[0].Offset {
		t.Fatalf("the test layouts agree (%d); R-1's copy is not exercised", ls.Variants[0].Fields[0].Offset)
	}
	for _, target := range types.Targets() {
		ll := emit(t, e, unitOf(e, bodies), target, types.Dev)
		clangC(t, target, t.TempDir(), "r1", ll, "-O2")
	}
	for _, target := range runTargets {
		for _, opt := range []string{"-O0", "-O2"} {
			t.Run(target.Name+opt, func(t *testing.T) {
				if _, why := hostRunner(target); why != "" {
					skipRunner(t, why)
				}
				ll := emit(t, e, cCallable(unitOf(e, bodies)), target, types.Dev) // the harness calls them
				dir := t.TempDir()
				osIR, kernel := harnessOS(target)
				// ViewPay's argument: code of Small at 0, payload 77 at 8.
				code := types.ErrorCode(types.ItemKey("main", "Small"))
				view := "  %r1 = call i8 @mk.main.ViewPay(i64 " + itoa(int(code)) + ", i64 77)\n"
				decl := "declare i8 @mk.main.ViewPay(i64, i64)\n"
				if target.OS == types.OSWindows {
					view = "  %x = alloca [16 x i8], align 8\n  store i32 " + itoa(int(code)) + ", ptr %x, align 8\n" +
						"  %xp = getelementptr inbounds i8, ptr %x, i64 8\n  store i8 77, ptr %xp, align 8\n" +
						"  %r1 = call i8 @mk.main.ViewPay(ptr %x)\n"
					decl = "declare i8 @mk.main.ViewPay(ptr)\n"
				}
				// A u8 argument or result carries zeroext where C extends one
				// EdgePay(u8): u8 and ViewPay's u8 result.
				edge := "declare i8 @mk.main.EdgePay(i8)\n"
				edgeCall := "  %r0 = call i8 @mk.main.EdgePay(i8 42)\n"
				if cExtends(target) {
					edge = "declare zeroext i8 @mk.main.EdgePay(i8 zeroext)\n"
					edgeCall = "  %r0 = call zeroext i8 @mk.main.EdgePay(i8 zeroext 42)\n"
					decl = strings.Replace(decl, "declare i8", "declare zeroext i8", 1)
					view = strings.Replace(view, "call i8", "call zeroext i8", 1)
				}
				h := osIR + harnessRT + decl + edge + `
define void @"mk.std.task.Start"(ptr %sp) noreturn nounwind {
` + edgeCall + `  %w0 = zext i8 %r0 to i64
  call void @h_num(i64 %w0)
  call void @h_write(ptr @s.nl, i64 1)
` + view + `  %w1 = zext i8 %r1 to i64
  call void @h_num(i64 %w1)
  call void @h_write(ptr @s.nl, i64 1)
  call void @h_exit(i32 0)
  unreachable
}
`
				objs := []string{clangC(t, target, dir, "support", supportLL(t, target), "-O2"),
					clangC(t, target, dir, "pack", ll, opt), clangC(t, target, dir, "harness", h, "-O0")}
				out, code2 := runExe(t, target, linkExe(t, target, dir, objs, linkOpts{kernel: kernel}), "")
				if code2 != 0 || out != "42\n77\n" {
					t.Fatalf("exit %d, output %q; want 42 and 77", code2, out)
				}
			})
		}
	}
}

// TestErrorSideWriteICE: a write through a SelErr view that is a copy is
// an ICE, never a silent write to the temporary.
func TestErrorSideWriteICE(t *testing.T) {
	e := newTestEnv()
	_, ut, st, small := r1Bodies(e)
	w := e.body(e.fn("WriteView", []types.TypeID{e.tt.Mut(ut)}, types.Void, 0))
	w.assign(w.b.Place(1, mir.Projection{Kind: mir.ProjDeref, Type: ut}, mir.Projection{Kind: mir.ProjDowncast, Sel: mir.SelErr, Type: st},
		mir.Projection{Kind: mir.ProjDowncast, Sel: mir.SelCase, A: uint32(small), Type: st},
		mir.Projection{Kind: mir.ProjField, A: 0, Type: types.U8}), use(mir.Imm(types.U8, 1)), w.at(9, 3))
	w.ret(w.at(9, 3))
	var b bytes.Buffer
	_, err := EmitPack(unitOf(e, []*mir.Body{w.b}), 0, e, Config{Target: types.X86_64Linux}, &b)
	var ie *InternalError
	if !errors.As(err, &ie) || !strings.Contains(ie.IE.What, "which is not laid out as") {
		t.Fatalf("err = %v, want the ICE of a write to an error side laid out otherwise", err)
	}
}
