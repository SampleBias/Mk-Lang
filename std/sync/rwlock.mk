// Why: data read by many tasks and written rarely should not serialise its
// readers behind one Mutex.
//
// What: sync.RwLock[T] and ReadGuard[T]: New, Lock (excludes everyone and
// returns the prelude Guard[T]), LockRead (excludes only Lock), Into, and
// ReadGuard's Ref and Unlock.
//
// How: a writer-preferring lock over a Mutex that holds the value, with one
// state word: the active reader count and two writer bits (P and W).

// Algorithm: a writer-preferring reader-writer lock over a Mutex, after
// Go's sync.RWMutex (writers serialised by a Mutex; a pending-writer mark
// in the reader count turns new readers away; the writer waits for the
// active readers to drain). The value lives inside the Mutex, so the write
// lock is that Mutex's own Guard, and Guard.Unlock is the whole write
// unlock. The state word rs holds:
//   bits 0 to 29  the active reader count (RwCount);
//   bit 30, P     the writer is parked on rs, waiting for the count to drain;
//   bit 31, W     a writer holds the mutex, or held it last.
// LockRead: Add(1), in when it read W clear; else it takes the count back
// (an exit, which may wake the writer) and takes the mutex
// (FIFO behind the writers, with Mutex's starvation handoff), and, holding
// it, clear W and P, which are stale because no writer holds the mutex, add
// one reader, and unlock. Lock: take the mutex, set W, and wait on rs until
// the count is 0 (P is set before each wait; the reader whose unlock makes
// the count 0 with P set wakes the writer). The write unlock leaves W set:
// the first reader after it clears it through the mutex, one uncontended
// mutex round trip per writer phase. Unlike Go, readers that arrive during
// a write wait in the mutex's queue rather than on a reader semaphore, so
// the write unlock needs no wake of its own.
//
// Invariants (the blocks of tests_rwlock.mk check each, and say which):
//   RW1 W and P change only while the mutex is held, except a reader's
//       count change, which keeps them.
//   RW2 While a writer holds a Guard, W is set and no reader holds a
//       ReadGuard: no reader can enter (the fast path's Add must read W
//       clear; the slow path needs the mutex). A fast reader that read W
//       set adds to the count only until it takes it back.
//   RW3 No lost wakeup: the writer waits on rs only while it reads a value
//       with P set and a non-zero count; the reader whose unlock takes the
//       count to 0 reads P in the same read-modify-write and wakes.
//   RW4 No starvation: new readers stop at W, so the count drains; readers
//       and writers queue in the mutex's FIFO, whose starvation handoff
//       bounds the wait.
//
// Memory ordering (raw.Atomic is seq_cst only). Reader enter: the Cas that
// adds one reads W clear, which the slow reader's clearing RMW wrote after
// it took the mutex (acquiring the last writer's critical section from its
// unlock), so the writes of the last writer happen before the read; the
// RMW chain on rs carries that edge to every later fast reader. Reader
// exit: its Add is a release of its reads; the writer's Load that reads a
// count of 0 acquires them, so no read overlaps the write that follows.

import std.core
import std.raw

/// A lock over a value of type T that many readers or one writer hold.
/// Lock excludes everyone and returns the prelude's Guard; LockRead
/// excludes only Lock and returns a ReadGuard. Writers are not starved: a
/// waiting writer turns new readers away, and the readers and writers that
/// wait queue on one Mutex, served in order, with the same handoff to a
/// waiter that has waited too long. It allocates nothing. Send when T is
/// Send, and Share when T is Send and Share.
export struct RwLock[T] {
    m: Mutex[T]
    rs: raw.Atomic[u32]
}

/// A held read lock of an RwLock, giving shared access to its value.
/// Linear: Unlock ends it, as Guard.Unlock ends a write lock, so the lock
/// cannot be forgotten. It borrows the RwLock while it lives, and no
/// writer can enter until it is unlocked.
export linear view struct ReadGuard[T] {
    l: *RwLock[T]
    io: Io
}

// Readers on many tasks share &T, so T must be Share as well as Send.
unsafe impl[T: Send] Send for RwLock[T] {}
unsafe impl[T: Send + Share] Share for RwLock[T] {}

const RwW: u32 = 0x8000_0000
const RwP: u32 = 0x4000_0000
const RwCount: u32 = 0x3FFF_FFFF

/// An unlocked RwLock holding x. It allocates nothing, so it can
/// initialize a `var` global.
export RwLock[T].New(x: T): RwLock[T] {
    return RwLock[T](m: Mutex[T].New(x), rs: raw.Atomic[u32].New(0))
}

/// Takes the write lock, waiting for the writer before it and for the
/// readers inside to leave, and returns the Guard; Guard.Unlock releases
/// it. Readers that arrive while a writer waits or holds the lock wait
/// behind it. Canceled, without the lock, when the scope is cancelled at
/// the call or while it waits. O(1) besides the wait; allocates nothing.
export RwLock[T].Lock(&self, io: Io): Canceled!Guard[T] {
    g := try self.m.Lock(io)
    rs := &self.rs
    // Announce: from here no reader enters by the fast path (RW2).
    for {
        // Edge: the Cas that sets W is ordered after the readers' Adds (and
        // slow-path Cas) whose count it reads.
        s := rs.Load()
        if rs.Cas(s, s | RwW) {
            break
        }
    }
    for {
        // Edge: a Load that sees the count reach 0 reads the last reader's
        // exit Add, so the readers' accesses happen before the writer's.
        s := rs.Load()
        if (s & RwCount) == 0 {
            // Acquire edge: this Load reads the last reader's exit Add.
            if (s & RwP) != 0 {
                RwClear(rs, RwP)
            }
            return g
        }
        if (s & RwP) == 0 && !rs.Cas(s, s | RwP) {
            continue
        }
        if core.SyncWait(io, rs, s | RwP) {
            // Cancelled: let readers in again and give the mutex back.
            RwClear(rs, RwW | RwP)
            g.Unlock()
            fail Canceled
        }
    }
}

// RwClear clears bits in rs, keeping the count. Only the mutex holder
// calls it (RW1).
RwClear(rs: &raw.Atomic[u32], bits: u32) {
    for {
        // Edge: the Cas on the value it read; ordered against every reader's
        // Cas (RW1).
        s := rs.Load()
        if rs.Cas(s, s & ~bits) {
            return
        }
    }
}

/// Takes a read lock, beside any other readers, and returns the
/// ReadGuard. It waits only while a writer holds or waits for the lock.
/// A cancellation point first, as Mutex.Lock is: in a cancelled scope it
/// returns Canceled at once, and a cancel while it waits returns Canceled
/// without the lock. More than 2^30 - 1 readers at once panics with code
/// 1. O(1) besides the wait; allocates nothing.
export RwLock[T].LockRead(&self, io: Io): Canceled!ReadGuard[T] {
    // The cancellation point's fast test inline (core.CancelClear: a native
    // Io while no scope is cancelled); when it fails, RwReadSlow makes the
    // full check first, then the Add.
    //
    // Fast path, as Go's RLock: one fetch-and-add, which never fails and
    // never retries (a Load and Cas loop would retry once for every reader
    // that changed the count in between, one more cache-line round trip
    // each), then one test: W clear and the count not already full (W is
    // the top bit, the count the low 30). Acquire edge: the Add that reads W
    // clear reads the RMW chain on rs from the slow reader that cleared W
    // under the mutex, so the last writer's critical section happens before
    // this reader's reads. Otherwise RwReadSlow goes on from what the Add
    // read.
    if core.CancelClear(io) {
        old := self.rs.Add(1)
        if (old & (RwW | RwCount)) < RwCount {
            return ReadGuard[T](l: raw.Of(self), io: io)
        }
        try RwReadSlow(self, io, old, true)
    } else {
        try RwReadSlow(self, io, 0, false)
    }
    return ReadGuard[T](l: raw.Of(self), io: io)
}

// RwReadSlow is LockRead past its fast path, out of line so that the fast
// path (the cancellation test, the Add and one test of what it read) is
// small enough to inline into a caller's loop. added says LockRead's Add was
// made and read old0 (W set or a full count); else the cancellation test
// failed, and the full check and the Add are made here. It returns holding
// a read count, or panics, or fails Canceled holding none. Both of
// LockRead's branches call it, so it is not a callee whose one call the
// release build's small-function rule counts into LockRead's size.
RwReadSlow[T](l: &RwLock[T], io: Io, old0: u32, added: bool): Canceled!void {
    rs := &l.rs
    var old := old0
    if !added {
        try core.SyncCancel(io)
        // Acquire edge: as LockRead's Add.
        old = rs.Add(1)
        if (old & (RwW | RwCount)) < RwCount {
            return
        }
    }
    if (old & RwW) == 0 {
        // The count was full: this Add carried into P.
        RwExit(rs, io)
        raw.Fail(1, "RwLock: too many readers")
    }
    // W is set: this reader is not in. It takes its count back as a
    // reader's exit does, so when that takes the count to 0 with P set
    // the parked writer is woken (RW3); RW2 counts only readers that
    // returned a ReadGuard.
    RwExit(rs, io)
    // A writer holds the mutex, waits for it, or held it last: queue on it.
    g := try l.m.Lock(io)
    for {
        // Edge: the Cas from a value with W and P stale is the reader's acquire
        // of the count (RW1).
        s := rs.Load()
        if (s & RwCount) == RwCount {
            g.Unlock()
            raw.Fail(1, "RwLock: too many readers")
        }
        // W and P are stale: no writer holds the mutex (RW1).
        if rs.Cas(s, (s & RwCount) + 1) {
            break
        }
    }
    g.Unlock()
}

/// The value. It takes the RwLock by value, so no guard can be holding it.
/// O(1); allocates nothing.
export RwLock[T].Into(self): T {
    RwLock(m, rs: _) := self
    return m.Into()
}

// RwAt is the RwLock a ReadGuard holds by raw pointer.
RwAt[T](l: *RwLock[T]): &RwLock[T] {
    unsafe "l is the live RwLock whose ReadGuard holds it: the guard's loan keeps it alive" {
        return &*l
    }
}

/// The value, shared with the other readers. No writer holds a Guard while
/// this ReadGuard lives. O(1).
export ReadGuard[T].Ref(&self): &T {
    return core.MutexPeek(&RwAt(self.l).m)
}

/// Releases this read lock. When it is the last reader out and a writer
/// waits, the writer is woken; that writer's Lock sees every read made
/// under this lock as done. Not a cancellation point. O(1); allocates
/// nothing.
export ReadGuard[T].Unlock(self) {
    ReadGuard(l, io) := self
    RwExit(&RwAt(l).rs, io)
}

// RwExit takes one reader's count back; the exit that takes the count to
// 0 with P set wakes the parked writer (RW3: it reads P in the same RMW).
// The Add is the release edge named at ReadGuard.Unlock.
RwExit(rs: &raw.Atomic[u32], io: Io) {
    old := rs.Add(0xFFFF_FFFF)
    if (old & RwCount) == 1 && (old & RwP) != 0 {
        core.SyncWake(io, rs, false)
    }
}
