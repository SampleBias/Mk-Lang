// Why: the heap's size classes, huge blocks, in-place resize and failure
// paths must hold exactly, since every allocation in std rests on them.
// What: single-carrier tests RT-HeapClasses, RT-HeapHuge, RT-HeapHugeGrow,
// RT-HeapHugeGrowModel, RT-HeapHugeBudget,
// RT-HeapResizeInPlace, RT-HeapOOM, RT-F16-HeapZero, RT-Heap-ClassIndex,
// RT-Heap-RunIn and RT-Heap-PathEdges, run by mk test over std.
// How: the computed class and run search are checked against the first,
// linear versions (ClassIndexScan, RunScan) for every size and alignment. A
// commit-limit failure is not tested here: std.sys does not expose the
// limit.

import std.raw
import std.sys

test "RT-HeapClasses RT-F9-HeapClassCount the 32 classes follow the rule of 3.8" {
    var k := 0
    for i in 1..9 {
        Assert(Classes[k] == 16 * i)
        k += 1
    }
    var base := 128
    for _ in 0..6 {
        for j in 1..5 {
            Assert(Classes[k] == base + base / 4 * j)
            k += 1
        }
        base = base * 2
    }
    Assert(k == 32)
    Assert(Classes.Len() == 32)
    Assert(Classes[31] == SmallMax)
    var hs: HeapState = HeapState(lock: raw.Atomic[u32].New(0), segs: raw.Null[SegHeader](),
        lists: [raw.Null[PageMeta](); 32], mags: raw.Null[Mag](), empty: 0, over: raw.Atomic[u32].New(0),
        idle_head: raw.Null[SegHeader](), idle_tail: raw.Null[SegHeader](), spare: raw.Atomic[int].New(0))
    Assert(hs.lists.Len() == 32)
    // A power-of-two class is aligned to its size; every align up to 4 KiB
    // is honoured.
    for c in 0..32 {
        n := Classes[c]
        if n & (n - 1) == 0 {
            p := HeapAlloc(n, 1)
            Assert(raw.Addr(p) % Trunc[uint](n) == 0)
            HeapFree(p, n, 1)
        }
    }
    var a := 1
    for a <= 4096 {
        p := HeapAlloc(24, a)
        Assert(raw.Addr(p) % Trunc[uint](a) == 0)
        HeapFree(p, 24, a)
        q := HeapAlloc(20_000, a)
        Assert(raw.Addr(q) % Trunc[uint](a) == 0)
        HeapFree(q, 20_000, a)
        a = a * 2
    }
}

test "RT-HeapHuge a block over 1 MiB is its own reservation" {
    // The block's capacity is its size rounded up to 64 KiB pages.
    n := (3 << 20) + 100
    p := HeapAlloc(n, 8)
    Assert(Real(p))
    unsafe "p is a fresh block of n bytes" {
        raw.Write(p, 1)
        raw.Write(raw.Offset(p, n - 1), 2)
        Assert(raw.Read(p) == 1 && raw.Read(raw.Offset(p, n - 1)) == 2)
    }
    Assert(HeapUsable(p, 8) == (3 << 20) + PageBytes)
    Assert(HeapResize(p, n, (3 << 20) + PageBytes, 8))
    HeapFree(p, n, 8)
    // align 4 MiB and above: the header sits just below the block.
    for align in [SegBytes, 8 << 20] {
        q := HeapAlloc(100, align)
        Assert(Real(q))
        Assert(raw.Addr(q) % Trunc[uint](align) == 0)
        Assert(HeapResize(q, 100, 4096, align))
        HeapFree(q, 100, align)
    }
}

test "RT-HeapHugeGrow a huge block grows in place by committing its reservation" (env: &System) {
    // The reservation spans HugeSpare times the first pages (lead and
    // capacity): growing past the capacity commits more of it, keeps the
    // bytes, and reads zero in the new part; one byte past the reservation
    // is refused and changes nothing.
    // Where the address space is limited the budget is 0 (RT-HeapHugeBudget
    // covers that), so there is nothing to grow into.
    if sys.AddressLimited() {
        env.Print("skip: the address space is limited (RLIMIT_AS), so huge blocks reserve no spare\n", ()) else {}
        return
    }
    n := (3 << 20) + 100
    p := HeapAlloc(n, 8)
    c := HeapUsable(p, 8)
    Assert(c == (3 << 20) + PageBytes)
    limit := HugeSpare * (c + PageBytes) - PageBytes
    unsafe "p is a block of c bytes, then of each size HeapResize grants" {
        raw.Write(p, 7)
        raw.Write(raw.Offset(p, n - 1), 9)
        Assert(HeapResize(p, n, c + 1, 8))
        Assert(HeapUsable(p, 8) == c + PageBytes)
        Assert(raw.Read(raw.Offset(p, c)) == 0)
        Assert(HeapResize(p, c + 1, 8 * c, 8))
        Assert(HeapUsable(p, 8) == 8 * c)
        Assert(raw.Read(p) == 7 && raw.Read(raw.Offset(p, n - 1)) == 9)
        Assert(raw.Read(raw.Offset(p, 8 * c - 1)) == 0)
        raw.Write(raw.Offset(p, 8 * c - 1), 5)
        Assert(!HeapResize(p, 8 * c, limit + 1, 8))
        Assert(HeapUsable(p, 8) == 8 * c)
        Assert(HeapResize(p, 8 * c, limit, 8))
        Assert(HeapUsable(p, 8) == limit)
        raw.Write(raw.Offset(p, limit - 1), 3)
        Assert(raw.Read(raw.Offset(p, 8 * c - 1)) == 5 && raw.Read(raw.Offset(p, limit - 1)) == 3)
        // A shrink stays in place and keeps the committed capacity.
        Assert(HeapResize(p, limit, n, 8))
        Assert(HeapUsable(p, 8) == limit)
    }
    HeapFree(p, n, 8)
    // align 4 MiB and above: HeaderOf reads the header below the block,
    // which a growth updates with the one at the reservation's base.
    for align in [SegBytes, 8 << 20] {
        q := HeapAlloc(100, align)
        Assert(HeapUsable(q, align) == PageBytes)
        Assert(HeapResize(q, 100, 1 << 20, align))
        Assert(HeapUsable(q, align) == 1 << 20)
        unsafe "q is a block of 1 MiB now" {
            raw.Write(raw.Offset(q, (1 << 20) - 1), 1)
            Assert(raw.Read(raw.Offset(q, (1 << 20) - 1)) == 1)
        }
        Assert(!HeapResize(q, 1 << 20, HugeSpare * (align + PageBytes) - align + 1, align))
        HeapFree(q, 1 << 20, align)
    }
}

test "RT-HeapResizeInPlace a block grows within its class or span only" {
    p := HeapAlloc(20, 8)
    Assert(HeapResize(p, 20, 32, 8))
    Assert(!HeapResize(p, 32, 33, 8))
    Assert(HeapResize(p, 32, 1, 8))
    HeapFree(p, 1, 8)
    q := HeapAlloc(9000, 8)
    Assert(HeapResize(q, 9000, PageBytes, 8))
    Assert(!HeapResize(q, PageBytes, PageBytes + 1, 8))
    HeapFree(q, PageBytes, 8)
}

test "RT-HeapOOM RT-F18-HeapOOMAddressSpace a 2^50-byte request is null" {
    Assert(raw.IsNull(HeapAlloc(1 << 50, 8)))
    Assert(raw.IsNull(HeapAlloc(1 << 50, 8 << 20)))
}

test "RT-F16-HeapZero size 0 is a dangling pointer below 4 MiB" {
    p := HeapAlloc(0, 8)
    Assert(raw.Addr(p) == 8)
    HeapFree(p, 0, 8)
    Assert(!HeapResize(p, 0, 16, 8))
    Assert(HeapResize(p, 0, 0, 8))
    // At 4 MiB alignment there is no dangling pointer: a real block.
    q := HeapAlloc(0, 8 << 20)
    Assert(Real(q))
    Assert(raw.Addr(q) % Trunc[uint](8 << 20) == 0)
    HeapFree(q, 0, 8 << 20)
    // A real block resized in place to 0 and freed with size 0 is freed:
    // the next allocation of its class gets it back. 7168 is a class no
    // other code of a test uses.
    a := HeapAlloc(7168, 8)
    Assert(HeapResize(a, 7168, 0, 8))
    HeapFree(a, 0, 8)
    b := HeapAlloc(7168, 8)
    Assert(raw.Same(a, b))
    HeapFree(b, 7168, 8)
}

// ClassIndexScan is the first ClassIndex, kept as the reference: the first class that
// holds n and is a multiple of align.
ClassIndexScan(n: int, align: int): int {
    for i in 0..32 {
        c := Classes[i]
        if c >= n && c % align == 0 {
            return i
        }
    }
    return 31
}

test "RT-Heap-ClassIndex the computed class equals the scan for every size and alignment" {
    // n from 1 to SmallMax, align every power of two to 4096 (HeapAlloc
    // passes n >= align), the 106,496 inputs the small path can see.
    var align := 1
    for align <= 4096 {
        var n := align
        for n <= SmallMax {
            Assert(ClassIndex(n, align) == ClassIndexScan(n, align))
            n += 1
        }
        align *= 2
    }
}

test "RT-Heap-SmallClassOf the fast path's class equals ClassIndex for every small size and every align to 16" {
    for n in 1..SmallMax + 1 {
        c := SmallClassOf(n)
        Assert(c == ClassIndex(n, 1) && c == ClassIndexScan(n, 1))
        // HeapAlloc's fast path takes SmallClassOf(size) where the slow
        // path takes ClassIndex(max(size, align), align), for each
        // power-of-two align of 16 or less.
        var a := 1
        for a <= 16 {
            m := if n > a { n } else { a }
            Assert(ClassIndex(m, a) == c && ClassIndexScan(m, a) == c)
            a *= 2
        }
    }
}

// MtCache is a private shared cache for the magazine order blocks: no
// segment and no page, so its first page is fresh and nothing else
// allocates from it. Their paths never reach the process's idle list
// (IdleLink, IdleUnlink and EmptyAdd go through HS()): the one page comes
// from a new segment, which is not idle, and no free there empties the
// page. Each block releases the segment at its end.
MtCache(): HeapState {
    return HeapState(lock: raw.Atomic[u32].New(0), segs: raw.Null[SegHeader](), lists: [raw.Null[PageMeta](); 32],
        mags: raw.Null[Mag](), empty: 0, over: raw.Atomic[u32].New(0), idle_head: raw.Null[SegHeader](),
        idle_tail: raw.Null[SegHeader](), spare: raw.Atomic[int].New(0))
}

// MtRelease releases a private cache's one segment, whose blocks are not
// used past the call. Outside the blocks: the runtime harness
// (std/rt-sched_harness_test.go) rewrites each block into a function whose
// sys parameter would hide the std.sys import.
MtRelease(s: *SegHeader) {
    unsafe "the private cache's one segment, whose blocks are not used past here" {
        sys.Release(raw.PtrCast[SegHeader, u8](s), SegBytes)
    }
}

// MtClass is the class the order blocks use: 64 bytes, whose magazine
// holds 16 blocks (MagOf's cap).
const MtClass: int = 3

test "RT-HeapMag-RefillOrder a magazine's refills hand out a fresh page's blocks in rising address order" {
    var hs := MtCache()
    var mag := Mag(n: 0, cap: 16, items: [raw.Null[u8](); 16])
    first := MagAlloc(raw.OfMut(mut hs), raw.OfMut(mut mag), MtClass)
    Assert(!raw.IsNull(first) && raw.Addr(first) % Trunc[uint](PageBytes) == 0)
    // 40 blocks over five refills (each hands out its first block, then 8
    // pops): each is the block after the last, as SmallAlloc gave them,
    // never the refill's last block first.
    for k in 1..40 {
        p := MagAlloc(raw.OfMut(mut hs), raw.OfMut(mut mag), MtClass)
        Assert(raw.Addr(p) == raw.Addr(first) + Trunc[uint](k * Classes[MtClass]))
    }
    MtRelease(hs.segs)
}

test "RT-HeapMag-DrainOrder a full magazine gives back its newer half in the order it was freed, then the freed block" {
    var hs := MtCache()
    var mag := Mag(n: 0, cap: 16, items: [raw.Null[u8](); 16])
    // 17 blocks of one fresh page; the cache is private, so no lock.
    var bs: [17]*u8 = [raw.Null[u8](); 17]
    for k in 0..17 {
        bs[k] = SmallAlloc(raw.OfMut(mut hs), MtClass)
        Assert(!raw.IsNull(bs[k]))
    }
    for k in 0..17 {
        MagFree(raw.OfMut(mut hs), raw.OfMut(mut mag), bs[k])
    }
    // The 17th free found the magazine full: blocks 8 to 15 went back to
    // the page in that order, then block 16; blocks 0 to 7 stay.
    Assert(mag.n == 8)
    for k in 0..8 {
        Assert(raw.Same(mag.items[k], bs[k]))
    }
    // The page's free list pops the last free first: 16, 15, ..., 8; then
    // the page's next unused block.
    for k in 0..9 {
        Assert(raw.Same(SmallAlloc(raw.OfMut(mut hs), MtClass), bs[16 - k]))
    }
    Assert(raw.Addr(SmallAlloc(raw.OfMut(mut hs), MtClass)) == raw.Addr(bs[16]) + Trunc[uint](Classes[MtClass]))
    MtRelease(hs.segs)
}

// RunScan is the first run search, kept as the reference: the lowest i >= 0 with bits i
// to i + k - 1 of f set, or -1.
RunScan(f: u64, k: int): int {
    want: u64 = (Trunc[u64](1) << Trunc[u64](k)) - 1
    var i := 0
    for i + k <= 64 {
        if ((f >> Trunc[u64](i)) & want) == want {
            return i
        }
        i += 1
    }
    return -1
}

test "RT-Heap-RunIn the word-parallel run search equals the scan" {
    // 200,000 masks from xorshift64 thinned to every density (AND of 0 to 3
    // draws), plus the edges (0, all ones, one bit, a run touching bit 63),
    // for k from 1 to 16.
    var x: u64 = 0x9E37_79B9_7F4A_7C15
    for t in 0..200_000 {
        var f: u64 = ~Trunc[u64](0)
        for _ in 0..(t % 4) {
            x ^= x << 13
            x ^= x >> 7
            x ^= x << 17
            f &= x
        }
        if t == 1 {
            f = 0
        } else if t == 2 {
            f = Trunc[u64](1) << 63
        } else if t == 3 {
            f = 0xFFFF_0000_0000_0000
        } else if t == 4 {
            f = 0xFFFF_FFFF_FFFF_FFFE
        }
        for k in 1..17 {
            Assert(RunIn(f, k) == RunScan(f, k))
        }
    }
}

// HbCheck allocates size bytes at align, checks the alignment, writes the
// first and the last byte and reads them back, grows the block in place to
// size (always allowed) and frees it. False when the heap returned null.
HbCheck(size: int, align: int): bool {
    p := HeapAlloc(size, align)
    if raw.IsNull(p) {
        return false
    }
    Assert(Real(p))
    Assert(raw.Addr(p) % Trunc[uint](align) == 0)
    unsafe "p is a fresh block of at least size bytes" {
        raw.Write(p, 0xA5)
        raw.Write(raw.Offset(p, size - 1), 0x5A)
        // size 1: one byte, the last write wins.
        Assert((size == 1 || raw.Read(p) == 0xA5) && raw.Read(raw.Offset(p, size - 1)) == 0x5A)
    }
    Assert(HeapResize(p, size, size, align))
    HeapFree(p, size, align)
    return true
}

test "RT-Heap-PathEdges every size at the small, large and huge boundaries, and the huge alignments below 4 MiB" {
    // The edges of HeapAlloc's three paths, each
    // at every alignment it can take, 50 rounds so a block freed on one
    // round is reused on the next (a header written outside its block, or
    // a page freed while in use, shows as a wrong byte or a fault).
    for _ in 0..50 {
        for size in [1, 15, 16, 17, 128, 129, SmallMax - 1, SmallMax, SmallMax + 1, PageBytes, PageBytes + 1,
            LargeMax - 1, LargeMax, LargeMax + 1] {
            var align := 1
            for align <= 4096 {
                Assert(HbCheck(size, align))
                align *= 2
            }
        }
        // Huge by alignment only (4 KiB < align < 4 MiB): the header at the
        // reservation's 4 MiB-aligned base, the block at base + max(64 KiB,
        // align), found again by masking (HeaderOf).
        for align in [8192, PageBytes, PageBytes * 2, 1 << 20, 2 << 20] {
            for size in [1, 100, PageBytes, (3 << 20) + 1] {
                Assert(HbCheck(size, align))
            }
        }
    }
}

// HgModel is a huge block as the RT-HeapHugeGrowModel test models it: its
// address (null for an empty slot), alignment, size, committed capacity
// and the most it may grow to, the bytes past its lead that its
// reservation spans.
struct HgModel {
    p: *u8
    align: int
    size: int
    cap: int
    limit: int
}

// HgPages is n rounded up to whole 64 KiB pages.
HgPages(n: int): int {
    return (n + PageBytes - 1) / PageBytes * PageBytes
}

// HgTag is the byte a model block holds at offset at, from its slot and
// the offset, so a byte a growth or a shrink moved or lost shows.
HgTag(slot: int, at: int): u8 {
    return Trunc[u8](slot * 31 + (at >> 16) + 1)
}

// HgMark writes the tags at the first byte of each 64 KiB page of [0, to)
// of the block at p, and at its last byte.
HgMark(p: *u8, slot: int, to: int) {
    var at := 0
    for at < to {
        unsafe "[0, to) lies inside the block, which the model knows" {
            raw.Write(raw.Offset(p, at), HgTag(slot, at))
        }
        at += PageBytes
    }
    if to > 0 {
        unsafe "to - 1 is the block's last byte" {
            raw.Write(raw.Offset(p, to - 1), HgTag(slot, to - 1))
        }
    }
}

// HgCheck reads back the tags HgMark wrote over [0, to), and the last
// byte's when to is the size it was written with (last).
HgCheck(p: *u8, slot: int, to: int, last: bool) {
    var at := 0
    for at < to {
        unsafe "[0, to) lies inside the block" {
            Assert(raw.Read(raw.Offset(p, at)) == HgTag(slot, at))
        }
        at += PageBytes
    }
    if last && to > 0 {
        unsafe "to - 1 is the block's last byte" {
            Assert(raw.Read(raw.Offset(p, to - 1)) == HgTag(slot, to - 1))
        }
    }
}

test "RT-HeapHugeGrowModel random allocations, resizes and frees of huge blocks agree with a model" (env: &System) {
    // Four slots, 400 seeded steps: allocate a huge block (by size, or by
    // an alignment past 4 KiB, of 1 MiB, where the lead is 16 pages and
    // the two headers are apart, or of 4 MiB), resize it to a
    // random size, now and then just around the end of its reservation,
    // or free it. The model predicts every resize's answer and the
    // capacity after it (HeapUsable: the size rounded to pages once it
    // passes the old one, as the lead is whole pages), and the bytes
    // written are read back after each resize: a growth keeps them and
    // reads zero past the old capacity, and a refused one changes nothing.
    if sys.AddressLimited() {
        env.Print("skip: the address space is limited (RLIMIT_AS), so huge blocks reserve no spare\n", ()) else {}
        return
    }
    // Every free gives its block's share of the spare budget back, from
    // the header HeaderOf finds at each alignment.
    hs := HS()
    var before := 0
    unsafe "the RtState page; this test runs on one task" {
        before = (*hs).spare.Load()
    }
    var m: [4]HgModel = [HgModel(p: raw.Null[u8](), align: 8, size: 0, cap: 0, limit: 0); 4]
    aligns := [8, 8192, 1 << 20, SegBytes]
    var x: u64 = 0x9E3779B97F4A7C15
    for _ in 0..400 {
        x ^= x << 13
        x ^= x >> 7
        x ^= x << 17
        slot := Trunc[int](x & 3)
        r := Trunc[int]((x >> 8) & 0xFFFF_FFFF)
        if raw.IsNull(m[slot].p) {
            align := aligns[(r >> 4) & 3]
            var size := LargeMax + 1 + (r >> 6) % (512 << 10)
            if align > 4096 {
                size = 1 + (r >> 6) % (256 << 10)
            }
            p := HeapAlloc(size, align)
            Assert(Real(p) && raw.Addr(p) % Trunc[uint](align) == 0)
            var lead := PageBytes
            if align > lead {
                lead = align
            }
            total := HgPages(lead + size)
            m[slot] = HgModel(p: p, align: align, size: size, cap: total - lead, limit: HugeSpare * total - lead)
            Assert(HeapUsable(p, align) == m[slot].cap)
            HgMark(p, slot, size)
            continue
        }
        b := m[slot]
        if (r & 7) == 0 {
            HgCheck(b.p, slot, b.size, true)
            HeapFree(b.p, b.size, b.align)
            m[slot].p = raw.Null[u8]()
            continue
        }
        var new := 1 + (r >> 3) % (b.limit / 4)
        if (r & 7) == 1 {
            new = 1 + (r >> 3) % b.size
        } else if (r & 7) == 2 {
            new = b.limit - PageBytes + (r >> 3) % (2 * PageBytes)
        }
        ok := HeapResize(b.p, b.size, new, b.align)
        Assert(ok == (new <= b.limit))
        if !ok {
            Assert(HeapUsable(b.p, b.align) == b.cap)
            HgCheck(b.p, slot, b.size, true)
            continue
        }
        var cap := b.cap
        if new > cap {
            cap = HgPages(new)
            unsafe "b.cap < new: the first byte the growth committed" {
                Assert(raw.Read(raw.Offset(b.p, b.cap)) == 0)
            }
        }
        Assert(HeapUsable(b.p, b.align) == cap)
        if new < b.size {
            HgCheck(b.p, slot, new, false)
        } else {
            HgCheck(b.p, slot, b.size, true)
        }
        HgMark(b.p, slot, new)
        m[slot].size = new
        m[slot].cap = cap
    }
    for i in 0..4 {
        if !raw.IsNull(m[i].p) {
            HgCheck(m[i].p, i, m[i].size, true)
            HeapFree(m[i].p, m[i].size, m[i].align)
        }
    }
    unsafe "the RtState page" {
        Assert((*hs).spare.Load() == before)
    }
}

test "RT-HeapHugeBudget a huge block reserves spare only while the budget lasts, and gives it back" {
    // The budget as HeapInit left it, then emptied, as for a limited
    // address space: a block gets no spare, so it cannot grow past its
    // pages, and its free gives nothing back. With a budget of one block's
    // spare, the first block takes it all, the second gets none, and the
    // frees return the budget to where it was.
    hs := HS()
    var before := 0
    unsafe "the RtState page; this test runs on one task" {
        before = (*hs).spare.Load()
        (*hs).spare.Store(0)
    }
    n := (2 << 20) + 1
    p := HeapAlloc(n, 8)
    c := HeapUsable(p, 8)
    Assert(!HeapResize(p, n, c + 1, 8))
    HeapFree(p, n, 8)
    total := c + PageBytes
    unsafe "the RtState page" {
        Assert((*hs).spare.Load() == 0)
        (*hs).spare.Store(total * (HugeSpare - 1))
    }
    a := HeapAlloc(n, 8)
    b := HeapAlloc(n, 8)
    unsafe "the RtState page" {
        Assert((*hs).spare.Load() == 0)
    }
    Assert(HeapResize(a, n, 2 * c, 8))
    Assert(!HeapResize(b, n, c + 1, 8))
    HeapFree(b, n, 8)
    HeapFree(a, 2 * c, 8)
    unsafe "the RtState page" {
        Assert((*hs).spare.Load() == total * (HugeSpare - 1))
        (*hs).spare.Store(before)
    }
}
