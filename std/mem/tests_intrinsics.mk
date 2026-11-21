// Why: intrinsics are lowered by codegen and run by the evaluator; both
// paths must agree.
// What: tests of std.mem's OffsetOf and Prefetch (intrinsics.mk), std.raw's
// AtomicCopy, AtomicFill, FnFromAddr and Select, and std.atomic's Pause.
// std.runtime's Cycles is tested in std/runtime/tests_cycles.mk, since
// std.mem builds on freestanding and std.runtime does not.
// How: run by `mk test --std=std.mem`: each runs natively, and the
// consts run the same functions in the evaluator
// (internal/eval/intrinsic_machine.go).

import std.atomic
import std.raw

struct OffRec {
    a: u8
    b: u64
    c: u16
}

const OffA: int = OffsetOf[OffRec]("a")
const OffB: int = OffsetOf[OffRec]("b")
const OffC: int = OffsetOf[OffRec]("c")

// AtomicElems fills words 1 and 2 with 7, copies words 0 and 1 up one
// (an overlap memmove copies backward), then words 2 and 3 down two (no
// overlap), each element whole and unordered.
AtomicElems(): [6]u32 {
    var w: [6]u32 = [1, 2, 3, 4, 5, 6]
    unsafe "w is a local array of six words" {
        p := raw.PtrCast[[6]u32, u32](raw.OfMut(mut w))
        raw.AtomicFill[u32](raw.Offset(p, 1), 7, 2)
        raw.AtomicCopy[u32](raw.Offset(p, 1), p, 2)
        raw.AtomicCopy[u32](raw.Offset(p, 4), raw.Offset(p, 2), 2)
        raw.AtomicCopy[u32](raw.Offset(p, 2), raw.Offset(p, 3), 2)
    }
    return w
}

const Elems: [6]u32 = AtomicElems()

align(4) struct AtomicPair {
    a: u16
    b: u16
}

// AtomicFloats fills with a float and a plain pair, any plain T of 1, 2, 4
// or 8 bytes aligned to its size (an atomic needs its natural
// alignment; AtomicPair is align(4)).
AtomicFloats(): bool {
    var f: [3]f64 = [0.0, 0.0, 0.0]
    var q: [3]AtomicPair = [AtomicPair(a: 0, b: 0), AtomicPair(a: 0, b: 0), AtomicPair(a: 0, b: 0)]
    unsafe "f and q are local arrays of three elements" {
        raw.AtomicFill[f64](raw.PtrCast[[3]f64, f64](raw.OfMut(mut f)), -2.5, 3)
        pq := raw.PtrCast[[3]AtomicPair, AtomicPair](raw.OfMut(mut q))
        raw.AtomicFill[AtomicPair](pq, AtomicPair(a: 9, b: 65535), 2)
        raw.AtomicCopy[AtomicPair](raw.Offset(pq, 2), pq, 1)
    }
    return f[0] == -2.5 && f[2] == -2.5 && q[2].a == 9 && q[2].b == 65535 && q[1].a == 9
}

const EvalFloats: bool = AtomicFloats()

test "OffsetOf reads the layout" {
    // Each field's address, taken natively, less the record's: the
    // offsets codegen laid out, against the evaluator's by name.
    r := OffRec(a: 1, b: 2, c: 3)
    var da := 0
    var db := 0
    var dc := 0
    unsafe "addresses of a local's fields, read only as integers" {
        base := raw.Addr(raw.Of(&r))
        da = Cast[int](raw.Addr(raw.Of(&r.a)) - base)
        db = Cast[int](raw.Addr(raw.Of(&r.b)) - base)
        dc = Cast[int](raw.Addr(raw.Of(&r.c)) - base)
    }
    Assert(OffA == da && OffB == db && OffC == dc)
    Assert(OffA != OffB && OffB != OffC && OffA != OffC)
}

test "AtomicFill and AtomicCopy, natively and in the evaluator" {
    // Fill: [1 7 7 4 5 6]; up one, backward: [1 1 7 4 5 6]; down two:
    // [1 1 7 4 7 4]; 3, 4 down one: [1 1 4 7 7 4].
    want: [6]u32 = [1, 1, 4, 7, 7, 4]
    n := AtomicElems()
    for i in 0..6 {
        Assert(Elems[i] == want[i])
        Assert(n[i] == want[i])
    }
    Assert(EvalFloats)
    Assert(AtomicFloats())
}

test "OffsetOf at run time, Prefetch and Pause" {
    r := OffRec(a: 1, b: 2, c: 3)
    Assert(OffsetOf[OffRec]("b") == OffB)
    Prefetch[OffRec](&r, false)
    Prefetch[OffRec](&r, true)
    var w := false
    for _ in 0..3 {
        Prefetch[OffRec](&r, w)
        w = !w
    }
    atomic.Pause()
}

ReturnSeven(): int {
    return 7
}

test "FnFromAddr calls a function at an address" {
    f := ReturnSeven
    var g: fn(): int = ReturnSeven
    unsafe "f's bits are its function's address" {
        a := raw.Read(raw.PtrCast[fn(): int, uint](raw.Of(&f)))
        g = raw.FnFromAddr[fn(): int](a)
    }
    Assert(g() == 7)
}

struct SelRec {
    a: u8
    b: u64
    c: u16
}

// Selects folds raw.Select over scalars, a pointer and a record, for both
// conditions; SelectsAt is the same at compile time.
Selects(): u64 {
    var acc: u64 = 0
    x: u64 = 5
    y: u64 = 9
    for i in 0..4 {
        c := i % 2 == 0
        acc = acc *% 31 +% raw.Select(c, x, y)
        acc = acc *% 31 +% Cast[u64](raw.Select(c, -3, 40) + 3)
        bv: u64 = if raw.Select(c, true, false) { 1 } else { 2 }
        acc = acc *% 31 +% bv
        r := raw.Select(c, SelRec(a: 1, b: 2, c: 3), SelRec(a: 4, b: 5, c: 6))
        acc = acc *% 31 +% Cast[u64](r.a) + r.b + Cast[u64](r.c)
        p := raw.Select(c, raw.Of(&x), raw.Of(&y))
        pv: u64 = if raw.Same(p, raw.Of(&x)) { 7 } else { 8 }
        acc = acc *% 31 +% pv
        acc = acc *% 31 +% Cast[u64](raw.Select(c, 1.5, 2.5) * 2.0)
    }
    return acc
}

const SelectsAt: u64 = Selects()

test "Select picks a or b, at run time and at compile time" {
    Assert(raw.Select(true, 1, 2) == 1 && raw.Select(false, 1, 2) == 2)
    r := raw.Select(false, SelRec(a: 1, b: 2, c: 3), SelRec(a: 4, b: 5, c: 6))
    Assert(r.a == 4 && r.b == 5 && r.c == 6)
    Assert(Selects() == SelectsAt)
}
