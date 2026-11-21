// Why: containers release what they replace or drop; a missed release leaks
// and a doubled one corrupts the heap.
// What: test blocks for Replace on Map, HeapMap and TextMap, TextMap.Set,
// HeapText.Set, Clear and Len, and FreeEmpty and FreeEach on HeapList,
// HeapMap, TextMap and Chan; linear elements go through FreeEach and Replace.
// How: the counting heap fails a block that leaves heap memory live (exit
// 102). Helper names start with Tr, because every file of std/core is one
// module.

linear struct TrTok {
    n: int
}

TrClose(t: TrTok): int {
    TrTok(n) := t
    return n
}

// TrNone is whether o is none; a value in it is closed.
TrNone(o: ?TrTok): bool {
    if t := o {
        _ = TrClose(t)
        return false
    }
    return true
}

// TrSum keeps a running sum a capturing literal can add to.
struct TrSum {
    v: Cell[int]
}

test "HeapText.Set replaces in place, grows past the capacity, and Clear keeps the buffer" (sys: &System) {
    a := sys.heap.Alloc()
    var t := HeapText.New(a)
    onerr t.Free(a)
    Assert(t.Len() == 0 && t.Str() == "")
    try t.Set(a, "")
    Assert(t.Len() == 0)
    try t.Set(a, "abc")
    Assert(t.Str() == "abc" && t.Len() == 3)
    try t.Set(a, "x")
    Assert(t.Str() == "x" && t.Len() == 1)
    try t.Set(a, "a much longer text than any before it, to force growth")
    Assert(t.Len() == 54 && t.Str().StartsWith("a much"))
    t.Clear()
    Assert(t.Len() == 0 && t.Str() == "")
    try t.Push(a, "é")
    try t.Set(a, "")
    Assert(t.Len() == 0)
    digits := "0123456789"
    for i in 0..200 {
        try t.Set(a, digits[..i % 11])
        Assert(t.Len() == i % 11)
    }
    t.Free(a)
}

test "HeapMap.Replace hands back the old value, linear values included" (sys: &System) {
    a := sys.heap.Alloc()
    var m := HeapMap[int, TrTok].New(a)
    for i in 0..100 {
        Assert(TrNone(m.Replace(a, i, TrTok(n: i)).Must()))
    }
    var back := 0
    for i in 0..100 {
        if o := m.Replace(a, i, TrTok(n: 1000 + i)).Must() {
            back += TrClose(o)
        }
    }
    Assert(back == 4950 && m.Len() == 100)
    s := TrSum(v: Cell[int].New(0))
    m.FreeEach(a, fn(k: int, v: TrTok) { s.v.Store(s.v.Load() + k + TrClose(v)) })
    Assert(s.v.Load() == 4950 + 100_000 + 4950)
}

test "HeapMap.Set and Ensure on present keys keep one entry" (sys: &System) {
    a := sys.heap.Alloc()
    var m := HeapMap[int, int].New(a)
    onerr m.Free(a)
    for i in 0..50 {
        try m.Set(a, i % 7, i)
    }
    Assert(m.Len() == 7 && *m.Get(&3).Must() == 45)
    v := try m.Ensure(a, 3, -1)
    Assert(*v == 45)
    m.Free(a)
}

test "TextMap.Set and Replace keep or copy each new key once and free them all" (sys: &System) {
    a := sys.heap.Alloc()
    var m := TextMap[TrTok].New(a)
    keys := ["", "a", "bb", "ccc", "a much longer key than the others"]
    for k in keys {
        Assert(TrNone(m.Replace(a, k, TrTok(n: k.Len())).Must()))
    }
    var sum := 0
    for k in keys {
        sum += TrClose(m.Replace(a, k, TrTok(n: 0)).Must().Must())
    }
    Assert(sum == 39 && m.Len() == 5)
    s := TrSum(v: Cell[int].New(0))
    m.FreeEach(a, fn(v: TrTok) { s.v.Store(s.v.Load() + 1 + TrClose(v)) })
    Assert(s.v.Load() == 5)
    var p := TextMap[int].New(a)
    onerr p.Free(a)
    try p.Set(a, "k", 1)
    try p.Set(a, "k", 2)
    try p.Set(a, "", 3)
    Assert(p.Len() == 2 && *p.Get("k").Must() == 2 && *p.Get("").Must() == 3)
    p.Free(a)
}

test "Map.Replace gives none for a new key and the old value for a present one" (sys: &System) {
    var m := Map[str, int].New(sys.arena)
    Assert((try m.Replace("x", 1)).IsNone())
    Assert((try m.Replace("x", 2)).Must() == 1)
    Assert(*m.Get(&"x").Must() == 2 && m.Len() == 1)
}

test "FreeEach consumes every linear element of a HeapList, in order" (sys: &System) {
    a := sys.heap.Alloc()
    var xs := HeapList[TrTok].New(a)
    for i in 0..1000 {
        xs.Push(a, TrTok(n: i)).Must()
    }
    s := TrSum(v: Cell[int].New(0))
    xs.FreeEach(a, fn(t: TrTok) {
        n := TrClose(t)
        Assert(n == s.v.Load())
        s.v.Store(n + 1)
    })
    Assert(s.v.Load() == 1000)
}

test "FreeEmpty frees an emptied container of linear elements" (sys: &System) {
    a := sys.heap.Alloc()
    var xs := HeapList[TrTok].New(a)
    xs.Push(a, TrTok(n: 1)).Must()
    Assert(TrClose(xs.Pop().Must()) == 1)
    xs.FreeEmpty(a)
    var m := HeapMap[int, TrTok].New(a)
    Assert(TrNone(m.Replace(a, 1, TrTok(n: 2)).Must()))
    Assert(TrClose(m.Remove(&1).Must()) == 2)
    m.FreeEmpty(a)
    var t := TextMap[TrTok].New(a)
    Assert(TrNone(t.Replace(a, "k", TrTok(n: 3)).Must()))
    Assert(TrClose(t.Remove(a, "k").Must()) == 3)
    t.FreeEmpty(a)
}

test "HeapList.FreeEmpty of a non-empty list panics [exit 101: HeapList.FreeEmpty of a non-empty list]" (sys: &System) {
    a := sys.heap.Alloc()
    var xs := HeapList[int].New(a)
    xs.Push(a, 1) else {
    }
    xs.FreeEmpty(a)
}

test "HeapMap.FreeEmpty of a non-empty map panics [exit 101: HeapMap.FreeEmpty of a non-empty map]" (sys: &System) {
    a := sys.heap.Alloc()
    var m := HeapMap[int, int].New(a)
    m.Set(a, 1, 1) else {
    }
    m.FreeEmpty(a)
}

test "TextMap.FreeEmpty of a non-empty map panics [exit 101: TextMap.FreeEmpty of a non-empty map]" (sys: &System) {
    a := sys.heap.Alloc()
    var m := TextMap[int].New(a)
    m.Set(a, "k", 1) else {
    }
    m.FreeEmpty(a)
}

test "Chan.FreeEach releases buffered linear values, oldest first, and FreeEmpty an emptied channel" (sys: &System) {
    a := sys.heap.Alloc()
    c := try Chan[TrTok].New(a, 8)
    for i in 0..5 {
        c.Send(sys.io, TrTok(n: i)).Must()
    }
    s := TrSum(v: Cell[int].New(0))
    c.FreeEach(a, fn(t: TrTok) {
        n := TrClose(t)
        Assert(n == s.v.Load())
        s.v.Store(n + 1)
    })
    Assert(s.v.Load() == 5)
    d := try Chan[TrTok].New(a, 2)
    d.Send(sys.io, TrTok(n: 9)).Must()
    v := d.Recv(sys.io).Must()
    Assert(TrClose(v) == 9)
    d.FreeEmpty(a)
}

test "Chan.FreeEmpty of a channel holding values panics [exit 101: Chan.FreeEmpty of a channel holding values]" (sys: &System) {
    a := sys.heap.Alloc()
    c := Chan[int].New(a, 2) else {
        return
    }
    c.Send(sys.io, 1) else {
    }
    c.FreeEmpty(a)
}

test "Chan.FreeEach over a wrapped ring releases the buffered values oldest first" (sys: &System) {
    a := sys.heap.Alloc()
    c := try Chan[TrTok].New(a, 4)
    // Two rounds of three sends and three receives leave the head at 2, so
    // values 6, 7 and 8 sit in slots 2, 3 and 0 and the walk wraps.
    var next := 0
    var want := 0
    for _ in 0..2 {
        for _ in 0..3 {
            c.Send(sys.io, TrTok(n: next)).Must()
            next += 1
        }
        for _ in 0..3 {
            Assert(TrClose(c.Recv(sys.io).Must()) == want)
            want += 1
        }
    }
    for _ in 0..3 {
        c.Send(sys.io, TrTok(n: next)).Must()
        next += 1
    }
    s := TrSum(v: Cell[int].New(6))
    c.FreeEach(a, fn(t: TrTok) {
        n := TrClose(t)
        Assert(n == s.v.Load())
        s.v.Store(n + 1)
    })
    Assert(s.v.Load() == 9)
}
