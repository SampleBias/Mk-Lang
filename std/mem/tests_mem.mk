// Why: Fixed, Null and Counting serve every container in std; a wrong
// count, offset or rollback corrupts memory far from the cause.
// What: tests of the allocators through the containers, as user code uses
// them; the table entries themselves are judged by std's Go differential
// tests against a model.
// How: run by `mk test --std=std.mem` with the counting test heap: a block
// that leaves heap memory live fails with exit 102. Helper names start with
// Tm.

import std.raw

// TmLead is how far b's first 8-byte address boundary lies past its start:
// a Fixed never hands out the bytes before it, and a [N]u8 local
// may sit at any address, so a block that fills a buffer exactly slices
// one from there.
TmLead(b: []u8): int {
    return Trunc[int]((0 -% raw.Addr(raw.Data(b))) & 7)
}

struct TmPair {
    a: int
    b: i64
}

test "Fixed: a growing HeapList fills the buffer exactly, in place" {
    var buf: [264]u8 = [0; 264]
    lead := TmLead(buf[..])
    f := Fixed.New(mut buf[lead..lead + 256])
    var xs := HeapList[i64].New(f.Alloc())
    var oom := -1
    for i in 0..40 {
        if oom < 0 {
            xs.Push(f.Alloc(), Cast[i64](i)) else {
                oom = i
            }
        }
    }
    // 4, 8, 16 and 32 elements, each grown in place: 32 * 8 bytes is the
    // whole buffer, and the 33rd push fails.
    Assert(oom == 32 && xs.Len() == 32)
    var sum: i64 = 0
    for x in xs.Items() {
        sum += x
    }
    Assert(sum == 496)
    xs.Free(f.Alloc())
}

test "Fixed: LIFO frees and Reset reuse the buffer; refused pushes leave the list as it was" {
    var buf: [72]u8 = [0; 72]
    lead := TmLead(buf[..])
    var f := Fixed.New(mut buf[lead..lead + 64])
    for round in 0..3 {
        a := Box[TmPair].New(f.Alloc(), TmPair(a: round, b: 1)) else {
            Assert(false)
            return
        }
        b := Box[TmPair].New(f.Alloc(), TmPair(a: round, b: 2)) else {
            a.Free(f.Alloc())
            Assert(false)
            return
        }
        b.Free(f.Alloc())
        a.Free(f.Alloc())
        f.Reset()
    }
    // 4 then 8 elements in place fill the 64 bytes: two pushes of ten fail.
    var xs := HeapList[i64].New(f.Alloc())
    var refused := 0
    for i in 0..10 {
        xs.Push(f.Alloc(), Cast[i64](i)) else {
            refused += 1
        }
    }
    Assert(refused == 2 && xs.Len() == 8)
    var s: i64 = 0
    for x in xs.Items() {
        s += x
    }
    Assert(s == 28)
    xs.Free(f.Alloc())
    f.Reset()
}

test "Fixed over an empty buffer refuses every allocation" {
    var buf: [1]u8 = [0; 1]
    f := Fixed.New(mut buf[0..0])
    var xs := HeapList[u8].New(f.Alloc())
    var failed := false
    xs.Push(f.Alloc(), 1) else {
        failed = true
    }
    Assert(failed && xs.Len() == 0)
    xs.Free(f.Alloc())
}

test "Counting over the heap: Bytes, Live and Peak, zero after Free" (sys: &System) {
    c := Counting.New(sys.heap.Alloc())
    Assert(c.Bytes() == 0 && c.Live() == 0 && c.Peak() == 0)
    b := try Box[TmPair].New(c.Alloc(), TmPair(a: 1, b: 2))
    Assert(c.Bytes() == 16 && c.Live() == 1 && c.Peak() == 16)
    var xs := HeapList[int].New(c.Alloc())
    for i in 0..100 {
        xs.Push(c.Alloc(), i) else {
            Assert(false)
        }
    }
    Assert(c.Live() == 2 && c.Bytes() == 16 + 8 * 128)
    xs.Free(c.Alloc())
    b.Free(c.Alloc())
    // The leak report a test block writes: nothing allocated through c is
    // still live.
    Assert(c.Live() == 0 && c.Bytes() == 0)
    Assert(c.Peak() >= 16 + 8 * 128)
}

test "Counting sees a leak through a HeapMap left unfreed, then freed" (sys: &System) {
    c := Counting.New(sys.heap.Alloc())
    var m := HeapMap[int, int].New(c.Alloc())
    for i in 0..1_000 {
        m.Set(c.Alloc(), i, i * i) else {
            Assert(false)
        }
    }
    Assert(c.Live() > 0 && c.Bytes() > 0)
    m.Free(c.Alloc())
    Assert(c.Live() == 0 && c.Bytes() == 0)
}

test "Counting over a Fixed counts what the Fixed grants" {
    var buf: [136]u8 = [0; 136]
    lead := TmLead(buf[..])
    f := Fixed.New(mut buf[lead..lead + 128])
    c := Counting.New(f.Alloc())
    var xs := HeapList[i64].New(c.Alloc())
    var oom := -1
    for i in 0..20 {
        if oom < 0 {
            xs.Push(c.Alloc(), 1) else {
                oom = i
            }
        }
    }
    Assert(oom == 16 && c.Live() == 1 && c.Bytes() == 128 && c.Peak() == 128)
    xs.Free(c.Alloc())
    Assert(c.Live() == 0 && c.Bytes() == 0)
}

test "Null refuses every allocation, and nothing leaks" {
    var xs := HeapList[int].New(Null())
    var failed := 0
    for i in 0..10 {
        xs.Push(Null(), i) else {
            failed += 1
        }
    }
    Assert(failed == 10 && xs.Len() == 0)
    xs.Free(Null())
    b := Box[int].New(Null(), 5) else {
        return
    }
    b.Free(Null())
    Assert(false)
}

test "Fixed: LIFO rounds of differently aligned Boxes never run the buffer out" {
    var buf: [4104]u8 = [0; 4104]
    lead := TmLead(buf[..])
    f := Fixed.New(mut buf[lead..lead + 4096])
    var bad := -1
    for i in 0..100_000 {
        if bad < 0 {
            a := Box[u8].New(f.Alloc(), 1) else {
                bad = i
                continue
            }
            b := Box[u64].New(f.Alloc(), 2) else {
                a.Free(f.Alloc())
                bad = i
                continue
            }
            c := Box[u8].New(f.Alloc(), 3) else {
                b.Free(f.Alloc())
                a.Free(f.Alloc())
                bad = i
                continue
            }
            d := Box[TmPair].New(f.Alloc(), TmPair(a: i, b: 4)) else {
                c.Free(f.Alloc())
                b.Free(f.Alloc())
                a.Free(f.Alloc())
                bad = i
                continue
            }
            d.Free(f.Alloc())
            c.Free(f.Alloc())
            b.Free(f.Alloc())
            a.Free(f.Alloc())
        }
    }
    Assert(bad < 0)
    // Everything came back: one list fills the whole buffer in place.
    var xs := HeapList[u8].New(f.Alloc())
    var refused := -1
    for i in 0..5_000 {
        if refused < 0 {
            xs.Push(f.Alloc(), 1) else {
                refused = i
            }
        }
    }
    Assert(refused == 4096 && xs.Len() == 4096)
    xs.Free(f.Alloc())
}

test "Fixed: a HeapList grows in place again after a temporary of higher alignment is freed" {
    var buf: [72]u8 = [0; 72]
    lead := TmLead(buf[..])
    f := Fixed.New(mut buf[lead..lead + 64])
    var xs := HeapList[u8].New(f.Alloc())
    xs.Push(f.Alloc(), 0) else {
        Assert(false)
    }
    t := Box[u64].New(f.Alloc(), 7) else {
        xs.Free(f.Alloc())
        Assert(false)
        return
    }
    t.Free(f.Alloc())
    var refused := -1
    for i in 1..100 {
        if refused < 0 {
            xs.Push(f.Alloc(), Trunc[u8](i)) else {
                refused = i
            }
        }
    }
    // 4, 8, 16, 32 and 64 bytes, every step in place: a copy would need a
    // second buffer that 64 bytes cannot hold.
    Assert(refused == 64 && xs.Len() == 64)
    xs.Free(f.Alloc())
}

test "HeapText over a Fixed fills the buffer in place; a refused push keeps the text" {
    var buf: [72]u8 = [0; 72]
    lead := TmLead(buf[..])
    f := Fixed.New(mut buf[lead..lead + 64])
    var t := HeapText.New(f.Alloc())
    var refused := -1
    for i in 0..40 {
        if refused < 0 {
            t.Push(f.Alloc(), "abcd") else {
                refused = i
            }
        }
    }
    // Capacities 4, 8, 16, 32 and 64, each grown in place: sixteen pushes
    // fill the 64 bytes, and the seventeenth is refused.
    Assert(refused == 16)
    Assert(t.Str() == "abcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcd")
    var again := false
    t.Push(f.Alloc(), "e") else {
        again = true
    }
    Assert(again && t.Str().Len() == 64)
    t.Free(f.Alloc())
}

test "TextMap over a Counting over a Fixed: Live and Bytes after each step, zero after Free" {
    var buf: [4104]u8 = [0; 4104]
    lead := TmLead(buf[..])
    f := Fixed.New(mut buf[lead..lead + 4096])
    c := Counting.New(f.Alloc())
    var m := TextMap[int].New(c.Alloc())
    Assert(c.Live() == 0 && c.Bytes() == 0)
    _ = m.Ensure(c.Alloc(), "alpha", 1) else {
        Assert(false)
    }
    // The table's arrays: a key of up to 15 bytes is kept in its slot.
    l1 := c.Live()
    b1 := c.Bytes()
    Assert(l1 >= 2 && b1 > 0)
    _ = m.Ensure(c.Alloc(), "beta", 2) else {
        Assert(false)
    }
    Assert(c.Live() == l1 && c.Bytes() == b1)
    // A key of 16 bytes or more is copied, once.
    long := "a sixteen-byte k"
    _ = m.Ensure(c.Alloc(), long, 5) else {
        Assert(false)
    }
    Assert(c.Live() == l1 + 1 && c.Bytes() == b1 + 16)
    // A present key copies nothing.
    _ = m.Ensure(c.Alloc(), "beta", 3) else {
        Assert(false)
    }
    _ = m.Ensure(c.Alloc(), long, 6) else {
        Assert(false)
    }
    Assert(c.Live() == l1 + 1 && c.Bytes() == b1 + 16 && m.Len() == 3)
    Assert(m.Remove(c.Alloc(), long).Must() == 5)
    Assert(c.Live() == l1 && c.Bytes() == b1 && m.Len() == 2)
    _ = m.Remove(c.Alloc(), "alpha")
    Assert(c.Live() == l1 && c.Bytes() == b1 && m.Len() == 1)
    // The empty key has no copy.
    _ = m.Ensure(c.Alloc(), "", 4) else {
        Assert(false)
    }
    Assert(c.Live() == l1 && c.Bytes() == b1 && m.Len() == 2)
    if v := m.Get("beta") {
        Assert(*v == 2)
    } else {
        Assert(false)
    }
    m.Free(c.Alloc())
    Assert(c.Live() == 0 && c.Bytes() == 0 && c.Peak() >= b1 + 16)
}

// TmFillMap sets i to i * 3 from 0 up in a HeapMap over a, until a Set is
// refused, and returns how many it holds then; the map is freed.
TmFillMap(a: &Alloc): int {
    var m := HeapMap[int, int].New(a)
    var n := 0
    for n < 100_000 {
        m.Set(a, n, n * 3) else {
            break
        }
        n += 1
    }
    // A refused Set leaves the map as it was.
    Assert(m.Len() == n)
    for i in 0..n {
        if v := m.Get(&i) {
            Assert(*v == i * 3)
        } else {
            Assert(false)
        }
    }
    m.Free(a)
    return n
}

test "HeapMap over a Fixed grows until refused; Free, then Reset gives every byte back" {
    var buf: [4104]u8 = [0; 4104]
    lead := TmLead(buf[..])
    var f := Fixed.New(mut buf[lead..lead + 4096])
    // Each growth frees an old table that is not the last block, so its
    // bytes stay until Reset.
    n1 := TmFillMap(f.Alloc())
    Assert(n1 > 0 && n1 < 4096 / 16)
    // The last table came back with Free, the stale ones did not: a list
    // grown in place from there stops short of the whole buffer.
    Assert(TmFillList(f.Alloc()) < 4096)
    f.Reset()
    // After Reset every byte is free: the list takes all 4096, and the map
    // grows as far as the first time.
    Assert(TmFillList(f.Alloc()) == 4096)
    n2 := TmFillMap(f.Alloc())
    Assert(n2 == n1)
    f.Reset()
}

// TmFillList pushes bytes into a HeapList over a until a push is refused,
// frees it and returns how many it held.
TmFillList(a: &Alloc): int {
    var xs := HeapList[u8].New(a)
    var n := 0
    for n < 100_000 {
        xs.Push(a, 1) else {
            break
        }
        n += 1
    }
    xs.Free(a)
    return n
}
