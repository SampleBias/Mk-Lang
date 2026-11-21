// Why: a container call refused for memory must change nothing and leak
// nothing.
// What: out-of-memory tests of HeapText.Set, TextMap.Set and Replace, and
// HeapMap.Replace.
// How: each runs over a Counting over a Fixed buffer, which refuses at its
// budget. Run by `mk test --std=std.mem`. Helper names start with To.

// ToAlpha gives distinct keys by length: ToAlpha[..i + 1].
const ToAlpha = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

test "HeapText.Set refused by the allocator keeps the text" {
    var buf: [56]u8 = [0; 56]
    lead := TmLead(buf[..])
    f := Fixed.New(mut buf[lead..lead + 48])
    c := Counting.New(f.Alloc())
    var t := HeapText.New(c.Alloc())
    t.Set(c.Alloc(), "keep") else {
        Assert(false)
    }
    live := c.Live()
    var refused := false
    t.Set(c.Alloc(), ToAlpha) else {
        refused = true
    }
    Assert(refused && t.Str() == "keep" && t.Len() == 4 && c.Live() == live)
    t.Set(c.Alloc(), "kept") else {
        Assert(false)
    }
    Assert(t.Str() == "kept")
    t.Free(c.Alloc())
    Assert(c.Live() == 0)
}

test "TextMap.Set and Replace refused by the allocator change nothing and leak no key copy" {
    var buf: [1032]u8 = [0; 1032]
    lead := TmLead(buf[..])
    f := Fixed.New(mut buf[lead..lead + 1024])
    c := Counting.New(f.Alloc())
    var m := TextMap[int].New(c.Alloc())
    var held := -1
    for i in 0..ToAlpha.Len() {
        live := c.Live()
        m.Set(c.Alloc(), ToAlpha[..i + 1], i) else {
            Assert(c.Live() == live && m.Len() == i)
            held = i
            break
        }
    }
    Assert(held > 0)
    var refused := false
    r := m.Replace(c.Alloc(), ToAlpha[..held + 1], 99) else {
        refused = true
        none
    }
    Assert(refused && r.IsNone() && m.Len() == held && m.Get(ToAlpha[..held + 1]).IsNone())
    // A present key needs no memory, so it is not refused.
    old := m.Replace(c.Alloc(), ToAlpha[..1], 7) else {
        Assert(false)
        none
    }
    Assert(old.Must() == 0 && *m.Get("a").Must() == 7)
    m.Set(c.Alloc(), ToAlpha[..2], 8) else {
        Assert(false)
    }
    Assert(*m.Get("ab").Must() == 8)
    m.Free(c.Alloc())
    Assert(c.Live() == 0)
}

test "HeapMap.Replace refused by the allocator changes nothing" {
    var buf: [1032]u8 = [0; 1032]
    lead := TmLead(buf[..])
    f := Fixed.New(mut buf[lead..lead + 1024])
    c := Counting.New(f.Alloc())
    var m := HeapMap[int, int].New(c.Alloc())
    var held := -1
    for i in 0..10_000 {
        _ = m.Replace(c.Alloc(), i, i * 3) else {
            Assert(m.Len() == i)
            held = i
            break
        }
    }
    Assert(held > 0 && m.Get(&held).IsNone())
    for i in 0..held {
        old := m.Replace(c.Alloc(), i, -i) else {
            Assert(false)
            none
        }
        Assert(old.Must() == i * 3)
    }
    m.Free(c.Alloc())
    Assert(c.Live() == 0)
}

// ToKeyLen is the length of ToRun's key i: 3, 20, 15 or 16 bytes in turn,
// so every second key is kept in its slot and every second one copied.
ToKeyLen(i: int): int {
    lens: [4]int = [3, 20, 15, 16]
    return lens[i % 4]
}

// ToRun sets 40 distinct keys, ToAlpha[i..i + ToKeyLen(i)], into a TextMap
// over a. A refused Set must change nothing: the count of bytes live at c,
// the length, and the key still absent. Returns how many Sets were taken;
// the map is freed.
ToRun(a: &Alloc, c: &Counting): int {
    var m := TextMap[int].New(a)
    var held := 0
    for i in 0..40 {
        k := ToAlpha[i..i + ToKeyLen(i)]
        live := c.Live()
        m.Set(a, k, i) else {
            Assert(c.Live() == live && m.Len() == held && m.Get(k).IsNone())
            continue
        }
        held += 1
        Assert(*m.Get(k).Must() == i)
    }
    Assert(m.Len() == held)
    m.Free(a)
    return held
}

test "TextMap.Set refused at each request, short and long keys: nothing changes and nothing leaks" (sys: &System) {
    c := Counting.New(sys.heap.Alloc())
    // A run that fails nothing: the 20 copied keys, then the table's two
    // arrays at capacity 8, 16, 32 and 64. A key of up to 15 bytes asks
    // for no memory.
    probe := Failing.New(c.Alloc(), -1)
    Assert(ToRun(probe.Alloc(), &c) == 40 && probe.Requests() == 20 + 8 && c.Live() == 0)
    var at := 0
    for at < 100 {
        f := Failing.New(c.Alloc(), at)
        Assert(ToRun(f.Alloc(), &c) >= 39 && c.Live() == 0)
        if f.Failed() == 0 {
            break
        }
        at += 1
    }
    Assert(at == 28)
}
