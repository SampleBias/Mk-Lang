// Why: a reader-writer lock fails as a torn value, a starved writer or a
// lost wake-up, mostly under cancellation and odd interleavings.
// What: the Conc-RwLock blocks; the invariants RW1 to RW4 they name are rwlock.mk's.
// How: native tasks at 1 and 8 carriers and seeded std.sim schedules, run by
// `mk test --std=std.sync` (std/std-missing_blocks_test.go).

import std.atomic
import std.core
import std.sim

test "Conc-RwLock write, two readers at once, Into" (sys: &System) {
    l := RwLock[i64].New(1)
    var g := l.Lock(sys.io).Must()
    *g.Mut() = 2
    g.Unlock()
    r1 := l.LockRead(sys.io).Must()
    r2 := l.LockRead(sys.io).Must()
    Assert(*r1.Ref() == 2 && *r2.Ref() == 2)
    r1.Unlock()
    r2.Unlock()
    Assert(l.Into() == 2)
}

struct RwPair {
    a: i64
    b: i64
}

RwWriter(l: &RwLock[RwPair], io: Io, n: int, bad: &atomic.AtomicInt) {
    for i in 0..n {
        var g := l.Lock(io) else {
            _ = bad.Add(1)
            return
        }
        p := g.Mut()
        if p.a != p.b {
            _ = bad.Add(1)
        }
        p.a += 1
        if i % 3 == 0 {
            io.Yield() else {}
        }
        p.b += 1
        g.Unlock()
    }
}

RwReader(l: &RwLock[RwPair], io: Io, n: int, bad: &atomic.AtomicInt) {
    for i in 0..n {
        r := l.LockRead(io) else {
            _ = bad.Add(1)
            return
        }
        p := r.Ref()
        if p.a != p.b {
            _ = bad.Add(1)
        }
        if i % 5 == 0 {
            io.Yield() else {}
        }
        if p.a != p.b {
            _ = bad.Add(1)
        }
        r.Unlock()
    }
}

// RW2: no reader sees a write in progress, and every write lands once.
test "Conc-RwLock 8 writers and 8 readers keep the pair equal" (sys: &System) {
    l := RwLock[RwPair].New(RwPair(a: 0, b: 0))
    bad := atomic.AtomicInt.New(0)
    scope s := sys.io.Scope() {
        for _ in 0..8 {
            s.go RwWriter(&l, sys.io, 500, &bad)
            s.go RwReader(&l, sys.io, 2000, &bad)
        }
    }
    Assert(bad.Load() == 0)
    p := l.Into()
    Assert(p.a == 4000 && p.b == 4000)
}

RwMs(n: i64): core.Duration {
    return core.DurationOf(n * 1_000_000)
}

RwWriteOne(l: &RwLock[i64], io: Io, done: &atomic.AtomicInt) {
    var g := l.Lock(io) else {
        done.Store(-1)
        return
    }
    *g.Mut() = 1
    g.Unlock()
    done.Store(1)
}

RwReadSees(l: &RwLock[i64], io: Io, seen: &atomic.AtomicInt) {
    r := l.LockRead(io) else {
        seen.Store(-1)
        return
    }
    seen.Store(*r.Ref() + 10)
    r.Unlock()
}

// RW4: a writer waiting for a reader turns a later reader away, so the
// later reader sees the write.
test "Conc-RwLock a waiting writer goes before a later reader" (sys: &System) {
    l := RwLock[i64].New(0)
    done := atomic.AtomicInt.New(0)
    seen := atomic.AtomicInt.New(0)
    r1 := l.LockRead(sys.io).Must()
    scope s := sys.io.Scope() {
        s.go RwWriteOne(&l, sys.io, &done)
        // Wait until the writer has announced itself (W) and is
        // waiting for r1 to leave (P), instead of hoping 10 ms is enough:
        // a reader spawned before W is set enters by the fast path. From
        // here every assertion holds whatever the schedule: the writer
        // cannot finish while r1 is held, the reader cannot enter while W
        // is set (it queues on the mutex the writer holds), and after r1
        // leaves the writer goes first (a late reader still finds W set,
        // which the write unlock leaves, and takes the mutex after it).
        RwWaitBits(&l, sys.io, RwW | RwP)
        s.go RwReadSees(&l, sys.io, &seen)
        sys.io.Sleep(RwMs(10)) else {}
        Assert(done.Load() == 0)
        Assert(seen.Load() == 0)
        r1.Unlock()
    }
    Assert(done.Load() == 1)
    Assert(seen.Load() == 11)
}

// RwWaitBits yields until every bit of bits is set in l's state word (the
// test reads the private word: it is in the module).
RwWaitBits(l: &RwLock[i64], io: Io, bits: u32) {
    for (l.rs.Load() & bits) != bits {
        io.NoCancel().Yield() else {}
    }
}

// A writer cancelled while it waits for readers leaves the lock usable:
// new readers enter at once, and a later writer gets it.
test "Conc-RwLock a writer cancelled while readers hold it" (sys: &System) {
    l := RwLock[i64].New(0)
    done := atomic.AtomicInt.New(0)
    r1 := l.LockRead(sys.io).Must()
    scope s := sys.io.Scope() {
        s.go RwWriteOne(&l, sys.io, &done)
        // The writer is in the drain wait (P set) when it is cancelled.
        RwWaitBits(&l, sys.io, RwW | RwP)
        s.Cancel()
    }
    Assert((l.rs.Load() & (RwW | RwP)) == 0)
    Assert(done.Load() == -1)
    r2 := l.LockRead(sys.io).Must()
    Assert(*r2.Ref() == 0)
    r2.Unlock()
    r1.Unlock()
    var g := l.Lock(sys.io).Must()
    *g.Mut() = 5
    g.Unlock()
    r3 := l.LockRead(sys.io).Must()
    Assert(*r3.Ref() == 5)
    r3.Unlock()
}

// A reader queued behind a writer is cancelled: Canceled, and the lock is
// unchanged.
test "Conc-RwLock a reader cancelled behind a writer" (sys: &System) {
    l := RwLock[i64].New(0)
    seen := atomic.AtomicInt.New(0)
    var g := l.Lock(sys.io).Must()
    scope s := sys.io.Scope() {
        s.go RwReadSees(&l, sys.io, &seen)
        sys.io.Sleep(RwMs(10)) else {}
        s.Cancel()
    }
    Assert(seen.Load() == -1)
    g.Unlock()
    r := l.LockRead(sys.io).Must()
    r.Unlock()
}

RwTryRead(l: &RwLock[i64], io: Io): int {
    r := l.LockRead(io) else {
        return 1
    }
    r.Unlock()
    return 0
}

RwTryWrite(l: &RwLock[i64], io: Io): int {
    g := l.Lock(io) else {
        return 1
    }
    g.Unlock()
    return 0
}

test "Conc-RwLock a cancelled scope is Canceled even when free" (sys: &System) {
    l := RwLock[i64].New(0)
    var a := 0
    var b := 0
    var c := 0
    scope s := sys.io.Scope() {
        s.Cancel()
        a = RwTryRead(&l, sys.io)
        b = RwTryWrite(&l, sys.io)
        c = RwTryRead(&l, sys.io.NoCancel()) + RwTryWrite(&l, sys.io.NoCancel())
    }
    Assert(a == 1 && b == 1 && c == 0)
}

// Lock's first await, a writer queued on
// the mutex behind another writer, is cancelled while four readers queue
// behind it. The cancelled writer answers Canceled without the lock, the
// readers then enter after the first writer, and a later writer gets it.
RwReadAdd(l: &RwLock[i64], io: Io, sum: &atomic.AtomicInt) {
    r := l.LockRead(io) else {
        _ = sum.Add(1_000_000)
        return
    }
    _ = sum.Add(*r.Ref() + 10)
    r.Unlock()
}

test "Conc-RwLock a writer cancelled behind a writer, readers behind it" (sys: &System) {
    l := RwLock[i64].New(0)
    done := atomic.AtomicInt.New(0)
    sum := atomic.AtomicInt.New(0)
    var g := l.Lock(sys.io).Must()
    scope r := sys.io.Scope() {
        scope s := sys.io.Scope() {
            // B can only wait on the mutex: A holds it for the whole block.
            s.go RwWriteOne(&l, sys.io, &done)
            sys.io.Sleep(RwMs(5)) else {}
            for _ in 0..4 {
                r.go RwReadAdd(&l, sys.io, &sum)
            }
            sys.io.Sleep(RwMs(5)) else {}
            s.Cancel()
        }
        // s joined B, so B has answered.
        Assert(done.Load() == -1)
        Assert(sum.Load() == 0)
        *g.Mut() = 3
        g.Unlock()
    }
    Assert(sum.Load() == 4 * 13)
    Assert(RwTryWrite(&l, sys.io) == 0)
    Assert(RwTryRead(&l, sys.io) == 0)
}

// Seeded schedules under std.sim. Two readers hold the
// lock across a yield while a writer arrives; a third reader comes after.
// Sim switches tasks only at Io calls (the PCT choice), so this
// fixes the order of the lock's waits and wakes per seed (RW3: the last
// reader's exit wakes the parked writer, whatever the order), not the
// instruction-level window between the writer's Cas(P) and its wait, which
// only the native stress runs reach.
RwSimReader(l: &RwLock[i64], io: Io, sum: &atomic.AtomicInt) {
    r := l.LockRead(io) else {
        _ = sum.Add(1_000_000)
        return
    }
    io.Yield() else {}
    _ = sum.Add(*r.Ref() + 10)
    io.Yield() else {}
    r.Unlock()
}

RwSimWriter(l: &RwLock[i64], io: Io, done: &atomic.AtomicInt) {
    io.Yield() else {}
    var g := l.Lock(io) else {
        done.Store(-1)
        return
    }
    *g.Mut() += 1
    io.Yield() else {}
    g.Unlock()
    done.Store(1)
}

test "Conc-RwLock RW3 under Sim schedules, seeds 1 to 256" (sys: &System) {
    for seed in 1..257 {
        var sm := try sim.Sim.New(sys.arena, Cast[u64](seed))
        io := sm.Io()
        l := RwLock[i64].New(0)
        done := atomic.AtomicInt.New(0)
        sum := atomic.AtomicInt.New(0)
        scope sc := io.Scope() {
            sc.go RwSimReader(&l, io, &sum)
            sc.go RwSimReader(&l, io, &sum)
            sc.go RwSimWriter(&l, io, &done)
            sc.go RwSimReader(&l, io, &sum)
        }
        sm.Free()
        Assert(done.Load() == 1)
        // Each reader saw 0 or the one write: 30 to 33, never a failure.
        s := sum.Load()
        Assert(s >= 30 && s <= 33)
        Assert(l.Into() == 1)
    }
}

// RwHoldCancelled keeps a cancelled scope alive on its own task until stop
// is set, so no native cancellation test of the process takes the fast
// answer (core.CancelClear is false) and every LockRead takes RwReadSlow's
// full check before its Add.
RwHoldCancelled(io: Io, on: &atomic.AtomicInt, stop: &atomic.AtomicInt) {
    scope c := io.Scope() {
        c.Cancel()
        on.Store(1)
        for stop.Load() == 0 {
            io.NoCancel().Yield() else {}
        }
    }
}

// RW2 with LockRead off its fast path: while another task's scope is
// cancelled, readers and writers of scopes that are not still keep the pair
// equal and every write lands once.
test "Conc-RwLock 4 writers and 8 readers keep the pair equal while another scope is cancelled" (sys: &System) {
    l := RwLock[RwPair].New(RwPair(a: 0, b: 0))
    bad := atomic.AtomicInt.New(0)
    on := atomic.AtomicInt.New(0)
    stop := atomic.AtomicInt.New(0)
    var slow := false
    scope s := sys.io.Scope() {
        s.go RwHoldCancelled(sys.io, &on, &stop)
        for on.Load() == 0 {
            sys.io.Yield() else {}
        }
        slow = !core.CancelClear(sys.io)
        scope w := sys.io.Scope() {
            for _ in 0..4 {
                w.go RwWriter(&l, sys.io, 300, &bad)
            }
            for _ in 0..8 {
                w.go RwReader(&l, sys.io, 1000, &bad)
            }
        }
        stop.Store(1)
    }
    Assert(slow)
    Assert(bad.Load() == 0)
    p := l.Into()
    Assert(p.a == 1200 && p.b == 1200)
}

// LockRead's fast path: with no writer ever and no scope cancelled, two
// readers at once and one after them each take the read lock with the Add
// alone. std-sync_inline_test.go runs this block over a scratch std in
// which RwReadSlow panics on entry.
test "Conc-RwLock readers with no writer and no scope cancelled stay off RwReadSlow" (sys: &System) {
    l := RwLock[i64].New(5)
    Assert(core.CancelClear(sys.io))
    r1 := l.LockRead(sys.io).Must()
    r2 := l.LockRead(sys.io).Must()
    Assert(l.rs.Load() == 2 && *r1.Ref() == 5 && *r2.Ref() == 5)
    r1.Unlock()
    r2.Unlock()
    r3 := l.LockRead(sys.io).Must()
    r3.Unlock()
    Assert(l.rs.Load() == 0)
    Assert(l.Into() == 5)
}
