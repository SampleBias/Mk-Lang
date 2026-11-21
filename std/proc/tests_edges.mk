// Why: process handling fails at its edges: cancels and deadlines mid-call,
// concurrent writers, inherited descriptors, path search, error mapping.
// What: edge-case test blocks of proc.Spawn, Child, Pipe and Pty, with helpers
// such as SpWaitCut, SpHold and SpPoolHold.
// How: a child is this test executable run again with SpChild's modes
// (tests_spawn.mk); run by std/std-proc_spawnblocks_test.go and TestStdIo_TestBlocks.

import std.core
import std.fs
import std.raw
import std.sys
import std.time

// Regression blocks, each named after the behaviour it pins. A block
// whose fix needs a runtime change not yet made runs its assertion
// only when MK_PROC3_BLOCKED is set
// (SpRunBlocked), so it can be shown to fail today and turned on with one
// line when the fix lands. The blocks whose names end "(blocked ...)" are
// guarded so.

// SpRunBlocked: MK_PROC3_BLOCKED is set and not empty.
SpRunBlocked(sys: &System): bool {
    v := sys.Env("MK_PROC3_BLOCKED") else {
        return false
    }
    return v.Len() > 0
}

// SpFoldPrefix: b starts with the bytes of s, ASCII letters compared
// ignoring case.
SpFoldPrefix(b: []u8, s: str): bool {
    sb := s.AsBytes()
    if sb.Len() > b.Len() {
        return false
    }
    for i in 0..sb.Len() {
        if Fold(b[i]) != Fold(sb[i]) {
            return false
        }
    }
    return true
}

// SpLastSep is the index of the last separator in b at or before i, or -1.
SpLastSep(b: []u8, i: int): int {
    var k := i
    for k >= 0 {
        if b[k] == 47 || (SpWin() && b[k] == 92) {
            return k
        }
        k -= 1
    }
    return -1
}

// SpStatOk: a stat of p (a pool job on both targets) answers a size above
// 0.
SpStatOk(io: Io, p: str): bool {
    info := fs.Stat(io, p) else {
        return false
    }
    return info.size > 0
}

// SpCloseFd0 closes descriptor 0 of this process (Linux; the lend block
// with descriptor 0 free). A function of its own, since sys is the
// System inside a block.
SpCloseFd0() {
    _ = sys.Close(0)
}

// SpThreads is the Threads: count of /proc/self/status (Linux), -1 when
// it cannot be read.
SpThreads(s: &System): int {
    text := fs.ReadText(s.arena, s.io, "/proc/self/status") else {
        return -1
    }
    b := text.AsBytes()
    key := "\nThreads:".AsBytes()
    var i := 0
    for i + key.Len() <= b.Len() {
        var hit := true
        for j in 0..key.Len() {
            if b[i + j] != key[j] {
                hit = false
                break
            }
        }
        if hit {
            var k := i + key.Len()
            for k < b.Len() && (b[k] == 32 || b[k] == 9) {
                k += 1
            }
            var n := 0
            var digits := 0
            for k < b.Len() && b[k] >= 48 && b[k] <= 57 {
                n = n * 10 + int(b[k] - 48)
                k += 1
                digits += 1
            }
            return if digits > 0 { n } else { -1 }
        }
        i += 1
    }
    return -1
}

// SpThreadWaiters is how many Reads of proc.Pipe ends wait at once in
// the Linux thread-count block, and SpThreadSlack how many threads the
// process may gain meanwhile (carriers start on demand, up to 16, and a
// few pool threads for the status reads): a wait job per Read would add
// SpThreadWaiters.
const SpThreadWaiters: int = 200
const SpThreadSlack: int = 100

test "Linux: 200 Reads waiting on proc.Pipe ends at once start no thread for their waits, under io_uring and epoll: the process gains fewer than 100 threads while they wait" (sys: &System) {
    // Fails when an end is not attached to the poller (sys.PipeAttach
    // false: a pool end, each waiting Read a wait job with a thread of its
    // own), which the 72-waiter block above cannot see on Linux, since
    // wait jobs keep the pool free there too.
    if SpWin() || Target.os == core.Os.Macos {
        return
    }
    io := sys.io
    before := SpThreads(sys)
    Assert(before > 0)
    done := raw.Atomic[u32].New(0)
    var during := -1
    var late: u32 = 0
    scope g := io.Scope() {
        for _ in 0..SpThreadWaiters {
            g.go SpHold(io, &done)
        }
        io.Sleep(time.Millis(500)) else {}
        during = SpThreads(sys)
        late = done.Load()
    }
    Assert(late == 0)
    Assert(during > 0)
    Assert(during - before < SpThreadSlack)
    Assert(done.Load() == u32(SpThreadWaiters))
}

// SpPipeWaiters is how many Reads of proc.Pipe ends wait at once in
// the 72-waiter block: more than the blocking pool's 64 threads.
const SpPipeWaiters: int = 72

test "72 Reads waiting on proc.Pipe ends at once, more than the blocking pool's 64 threads, leave the pool free: a file stat and a WriteAll and Read through another pipe made meanwhile complete within 2 s, before any waiting Read ends" (sys: &System) {
    io := sys.io
    done := raw.Atomic[u32].New(0)
    var stat := false
    var piped := false
    var late: u32 = 0
    scope g := io.Scope() {
        for _ in 0..SpPipeWaiters {
            g.go SpHold(io, &done)
        }
        io.Sleep(time.Millis(300)) else {}
        (r, w) := Pipe(io).Must()
        scope s := io.Scope() {
            // A stat is a pool job whatever the pipe ends are: were the
            // Reads holding the pool's 64 threads, it would queue past
            // this deadline (they wait 3 s).
            s.CancelAfter(time.Millis(2000))
            stat = SpStatOk(io, sys.args[0])
            var b: [1]u8 = [0]
            w.WriteAll(io, "x".AsBytes()) else {}
            n := r.Read(io, mut b[..]) else { 0 }
            piped = n == 1 && b[0] == 120
            late = done.Load()
        }
        r.Close()
        w.Close()
    }
    Assert(stat)
    Assert(piped)
    Assert(late == 0)
    Assert(done.Load() == u32(SpPipeWaiters))
}

test "a Linux child given Stdio.Pipe opens its stdin and stdout again by name (/dev/stdin, /dev/stdout), as an os/exec child does" (sys: &System) {
    // The child's end is a pipe, which open(2)
    // of /dev/stdin takes (a Unix socket's end was ENXIO).
    if SpWin() {
        return
    }
    io := sys.io
    var a: [3]str = ["/bin/sh", "-c", "cat /dev/stdin > /dev/stdout"]
    var c := DefaultCommand
    c.args = a[..]
    c.stdin = Stdio.Pipe
    c.stdout = Stdio.Pipe
    (child, stdin, stdout, stderr) := Spawn(sys.arena, io, &c).Must()
    SpCloseEnds(none, none, stderr)
    SpFeed(io, stdin.Must(), "hi\n".AsBytes())
    r := stdout.Must()
    var b: [64]u8 = [0; 64]
    var n := 0
    SpDrain(io, &r, mut b[..], mut n)
    r.Close()
    Assert(SpCode(child.Wait(io).Must()) == 0 && SpText(b[0..n], "hi\n"))
}

// SpProcState is the state letter of /proc/<pid>/stat, or 0 when the
// entry is gone.
SpProcState(sys: &System, pid: i64): u8 {
    var d: [24]u8 = [0; 24]
    var p: [64]u8 = [0; 64]
    digits := PtDigits(mut d, Trunc[int](pid)).AsBytes()
    (mut p[0..6]).CopyFrom("/proc/".AsBytes())
    (mut p[6..6 + digits.Len()]).CopyFrom(digits)
    k := 6 + digits.Len()
    (mut p[k..k + 5]).CopyFrom("/stat".AsBytes())
    path := str.From(p[0..k + 5]) else { "" }
    text := fs.ReadText(sys.arena, sys.io, path) else {
        return 0
    }
    b := text.AsBytes()
    var i := b.Len() - 1
    for i > 0 && b[i] != 41 {
        i -= 1
    }
    return if i + 2 < b.Len() { b[i + 2] } else { 63 }
}

test "a detached Linux child that ends is reaped with no later Spawn or Detach, so it is no zombie" (sys: &System) {
    if sys.args.Len() > 2 {
        SpChild(sys)
    }
    // The runtime's reaper thread reaps it; the
    // runtime once reaped only at the next Spawn or Detach.
    if SpWin() {
        return
    }
    io := sys.io
    var a: [6]str = [sys.args[0], sys.args[1], "gen", "0", "0", "0"]
    var c := DefaultCommand
    c.args = a[..]
    (child, stdin, stdout, stderr) := Spawn(sys.arena, io, &c).Must()
    SpCloseEnds(stdin, stdout, stderr)
    pid := child.Pid()
    child.Detach()
    io.Sleep(time.Millis(1000)) else {}
    Assert(SpProcState(sys, pid) != 90)
}

test "a relative program path with a separator is taken in Command.dir on both targets, as os/exec takes it" (sys: &System) {
    if sys.args.Len() > 2 {
        SpChild(sys)
    }
    exe := fs.Absolute(sys.arena, sys.io, sys.args[0]).Must()
    eb := exe.AsBytes()
    i := SpLastSep(eb, eb.Len() - 1)
    j := SpLastSep(eb, i - 1)
    Assert(j > 0)
    // dir is the executable's grandparent, args[0] the rest of its path.
    dir := str.From(eb[0..j]) else { "" }
    rel := str.From(eb[j + 1..]) else { "" }
    var a: [3]str = [rel, sys.args[1], "cwd"]
    var c := DefaultCommand
    c.args = a[..]
    c.stdout = Stdio.Pipe
    c.dir = some(dir)
    var b: [4096]u8 = [0; 4096]
    var n := SpOut(sys, &c, mut b[..])
    Assert(n > dir.Len() && SpFoldPrefix(b[0..n], dir))
    if SpWin() && eb.Len() > 3 && eb[1] == 58 {
        // Rooted with no drive (\dir\exe): on the drive of Command.dir.
        var rooted: [3]str = [str.From(eb[2..]) else { "" }, sys.args[1], "cwd"]
        c.args = rooted[..]
        n = SpOut(sys, &c, mut b[..])
        Assert(n > dir.Len() && SpFoldPrefix(b[0..n], dir))
        // On dir's drive with no root (C:rel): in dir.
        var dr: [1024]u8 = [0; 1024]
        dr[0] = eb[0]
        dr[1] = 58
        (mut dr[2..2 + rel.Len()]).CopyFrom(rel.AsBytes())
        var drel: [3]str = [str.From(dr[0..2 + rel.Len()]) else { "" }, sys.args[1], "cwd"]
        c.args = drel[..]
        n = SpOut(sys, &c, mut b[..])
        Assert(n > dir.Len() && SpFoldPrefix(b[0..n], dir))
    }
}

test "a NUL in args[0] is Os(0) on both targets, for a name searched in PATH and for a path" (sys: &System) {
    var c := DefaultCommand
    var one: [1]str = ["mk-proc-x\x00y"]
    c.args = one[..]
    Assert(SpIsOs(SpErr(sys, &c), 0))
    var two: [1]str = [if SpWin() { "whoami.exe\x00x" } else { "true\x00x" }]
    c.args = two[..]
    Assert(SpIsOs(SpErr(sys, &c), 0))
    var three: [1]str = [if SpWin() { "C:\\Windows\\System32\\whoami.exe\x00x" } else { "/bin/true\x00x" }]
    c.args = three[..]
    Assert(SpIsOs(SpErr(sys, &c), 0))
}

test "a missing Command.dir, or one that names a file, is NotFound on both targets, by path and by name" (sys: &System) {
    if sys.args.Len() > 2 {
        SpChild(sys)
    }
    exe := fs.Absolute(sys.arena, sys.io, sys.args[0]).Must()
    missing := if SpWin() { "C:\\mk-proc-no-such-dir-4f1c" } else { "/mk-proc-no-such-dir-4f1c" }
    var a: [3]str = [exe, sys.args[1], "cwd"]
    var c := DefaultCommand
    c.args = a[..]
    c.dir = some(missing)
    Assert(PtSame(PtName(SpErr(sys, &c)), "NotFound"))
    c.dir = some(exe)
    Assert(PtSame(PtName(SpErr(sys, &c)), "NotFound"))
    var sh: [1]str = [if SpWin() { "cmd.exe" } else { "sh" }]
    c.args = sh[..]
    c.dir = some(missing)
    Assert(PtSame(PtName(SpErr(sys, &c)), "NotFound"))
}

// SpSpawnCut spawns c in a scope cancelled after us microseconds (at once
// for 0): 0 when Spawn was Canceled, 1 when it returned the child, which
// is then waited for. Anything else fails the block.
SpSpawnCut(sys: &System, c: &Command, us: i64): int {
    io := sys.io
    scope s := io.Scope() {
        if us == 0 {
            s.Cancel()
        } else {
            s.CancelAfter(time.Micros(us))
        }
        (child, stdin, stdout, stderr) := Spawn(sys.arena, io, c) else e {
            Assert(PtSame(PtName(e), "Canceled"))
            return 0
        }
        SpCloseEnds(stdin, stdout, stderr)
        Assert(SpCode(child.Wait(io.NoCancel()).Must()) == 0)
    }
    return 1
}

test "Spawn in a scope cancelled at 0, 1, 50, 200 and 1000 us is Canceled with no child, or returns the child, and leaks no handle over 130 rounds" (sys: &System) {
    if sys.args.Len() > 2 {
        SpChild(sys)
    }
    var a: [6]str = [sys.args[0], sys.args[1], "gen", "0", "0", "0"]
    var c := DefaultCommand
    c.args = a[..]
    c.stdin = Stdio.Pipe
    c.stdout = Stdio.Pipe
    c.stderr = Stdio.Pipe
    var delays: [5]i64 = [0, 1, 50, 200, 1000]
    // Rounds 0 to 9 warm up; then two windows of 60 rounds are counted,
    // rounds 10 to 69 and 70 to 129, 12 rounds of each cut at 0 us. The
    // count also rises once by a constant the runtime makes once (2 in
    // 22 or 100 rounds on Windows, release, 16 carriers; 4 once on a
    // loaded machine). Beside the Command.handles blocks, the counts at rounds
    // 10, 40 and 70 in 10 full std.proc runs on Windows dev were 61 61 61
    // in 7, 61 62 62 in 1 and 50 61 61 in 2: one rise of 11, between rounds
    // 10 and 40, of handles not identified; each block runs in its own
    // process, so those blocks hold none of them. A one-time rise lands
    // in one window at most and a leak in both, so the smaller window's
    // rise is held to 8 (a leak per round is 60 in each, per cut round 12).
    var h0 := 0
    var h1 := 0
    var cut := 0
    for round in 0..130 {
        if round == 10 {
            h0 = PtHandles()
        }
        if round == 70 {
            h1 = PtHandles()
        }
        cut += 1 - SpSpawnCut(sys, &c, delays[round % 5])
    }
    // The rounds at 0 us are always cut.
    Assert(cut >= 26)
    h2 := PtHandles()
    if h2 >= 0 {
        Assert(h1 - h0 <= 8 || h2 - h1 <= 8)
    }
}

// SpLateWrite writes "abc" to w after ms milliseconds.
SpLateWrite(io: Io, w: &PipeWriter, ms: i64) {
    io.Sleep(time.Millis(ms)) else {}
    w.WriteAll(io, "abc".AsBytes()).Must()
}

// SpCutRead is a Read of r into b under a deadline of ms: the bytes read,
// 0 when the deadline cut it.
SpCutRead(io: Io, r: &PipeReader, b: mut []u8, ms: i64): int {
    var k := 0
    scope d := io.Scope() {
        d.CancelAfter(time.Millis(ms))
        k = r.Read(io, b) else e {
            Assert(PtSame(PtName(e), "Canceled"))
            0
        }
    }
    return k
}

test "a Read cut by deadlines of 0 to 6 ms while a task writes after 0 to 4 ms loses no byte over 35 rounds (repeatable)" (sys: &System) {
    io := sys.io
    for round in 0..35 {
        (r, w) := Pipe(io).Must()
        var got: [3]u8 = [0; 3]
        var k := 0
        scope s := io.Scope() {
            s.go SpLateWrite(io, &w, i64(round % 5))
            k = SpCutRead(io, &r, mut got[..], i64(round % 7))
        }
        for k < 3 {
            n := r.Read(io, mut got[k..]).Must()
            Assert(n > 0)
            k += n
        }
        Assert(SpText(got[..], "abc"))
        w.Close()
        r.Close()
    }
}

test "Pty.WriteAll to a child that never reads ends by its deadline, Canceled, and the Pty still closes (prefix)" (sys: &System) {
    if sys.args.Len() > 2 {
        SpChild(sys)
    }
    io := sys.io
    p := Pty.Open(io, 80, 24).Must()
    var a: [3]str = [sys.args[0], sys.args[1], "sleep"]
    var c := DefaultCommand
    c.args = a[..]
    c.terminal = some(&p)
    (child, stdin, stdout, stderr) := Spawn(sys.arena, io, &c).Must()
    SpCloseEnds(stdin, stdout, stderr)
    n := 4_000_000
    data := sys.arena.Slice[u8](n).Must()
    for i in 0..n {
        data[i] = if i % 64 == 63 { 13 } else { 97 }
    }
    var failed := false
    scope s := io.Scope() {
        s.CancelAfter(time.Millis(300))
        p.WriteAll(io, data[..]) else e {
            failed = PtSame(PtName(e), "Canceled") && s.Expired()
        }
    }
    child.Kill().Must()
    // The echo of what was written is left on p (SpWaitReading).
    _ = SpWaitReading(io, &p, child)
    p.Close()
    Assert(failed)
}

// SpPtyToEnd reads p under a deadline of ms until 0 or the deadline:
// (the bytes read, whether 0 came).
SpPtyToEnd(io: Io, p: &Pty, buf: mut []u8, ms: i64): (total: int, end: bool) {
    var total := 0
    var end := false
    scope s := io.Scope() {
        s.CancelAfter(time.Millis(ms))
        for !end {
            k := p.Read(io, buf) else e {
                Assert(PtSame(PtName(e), "Canceled") && s.Expired())
                return (total: total, end: false)
            }
            end = k == 0
            total += k
        }
    }
    return (total: total, end: end)
}

test "Pty.Read after its only child was waited for returns its output, then the end (0), on both targets" (sys: &System) {
    if sys.args.Len() > 2 {
        SpChild(sys)
    }
    // Not macOS: there the last close of a pty's peer, at the child's exit,
    // waits at most 60 ticks for the master to read and then discards the
    // output left (XNU's ptsclose and ttyclose), so a read after the wait
    // finds only the end.
    if Target.os == core.Os.Macos {
        return
    }
    io := sys.io
    p := Pty.Open(io, 80, 24).Must()
    var a: [6]str = [sys.args[0], sys.args[1], "gen", "5", "0", "0"]
    var c := DefaultCommand
    c.args = a[..]
    c.terminal = some(&p)
    (child, stdin, stdout, stderr) := Spawn(sys.arena, io, &c).Must()
    SpCloseEnds(stdin, stdout, stderr)
    Assert(SpCode(child.Wait(io).Must()) == 0)
    var b: [8192]u8 = [0; 8192]
    (total, end) := SpPtyToEnd(io, &p, mut b[..], 10000)
    p.Close()
    // On Windows the end comes once the console's last child has been
    // waited for, as Linux's EIO does.
    Assert(end && total >= 5)
}

test "a descriptor a Linux process holds without close-on-exec does not reach the child it spawns (close_range before exec)" (sys: &System) {
    if sys.args.Len() > 2 {
        SpChild(sys)
    }
    // Linux only, as its title says: the child counts its descriptors in
    // /proc/self/fd, which macOS does not have.
    if SpWin() || Target.os == core.Os.Macos {
        return
    }
    var f: [3]str = [sys.args[0], sys.args[1], "fds"]
    var c := DefaultCommand
    c.args = f[..]
    c.stdout = Stdio.Pipe
    var b0: [64]u8 = [0; 64]
    n0 := SpOut(sys, &c, mut b0[..])
    base := PtInt(str.From(b0[0..n0 - 1]) else { "0" })
    // The shell opens fd 7 without close-on-exec, which std never does,
    // and execs this program in mode fdsvia, which holds it.
    var s: [5]str = ["/bin/sh", "-c", "exec 7</dev/null; exec \"$0\" \"$1\" fdsvia", sys.args[0], sys.args[1]]
    c.args = s[..]
    var b: [64]u8 = [0; 64]
    n := SpOut(sys, &c, mut b[..])
    var nl := 0
    for nl < n && b[nl] != 10 {
        nl += 1
    }
    Assert(nl + 1 < n)
    own := PtInt(str.From(b[0..nl]) else { "0" })
    grand := PtInt(str.From(b[nl + 1..n - 1]) else { "0" })
    Assert(own == base + 1 && grand == base)
}

// SpCheckErr is the error SpawnCheck maps r to, or for a value v the
// marker Os(1,000,000 + v).
SpCheckErr(r: core.IoResult): Error {
    v := SpawnCheck(r) else e {
        return e
    }
    return Os(code: v + 1000000)
}

test "a spawn's Other with no OS code is OutOfMemory, Other with a code is Os with it, and DirFailure keeps OutOfMemory" (sys: &System) {
    Assert(PtSame(PtName(SpCheckErr(core.IoResult(value: 0, err: core.SysErr.Other, os: 0))), "OutOfMemory"))
    Assert(SpIsOs(SpCheckErr(core.IoResult(value: 0, err: core.SysErr.Other, os: 1455)), 1455))
    Assert(SpIsOs(SpCheckErr(core.IoResult(value: 0, err: core.SysErr.InvalidPath, os: 0)), 0))
    Assert(PtSame(PtName(SpCheckErr(core.IoResult(value: 0, err: core.SysErr.NotFound, os: 2))), "NotFound"))
    Assert(PtSame(PtName(SpCheckErr(core.IoResult(value: 0, err: core.SysErr.Canceled, os: 0))), "Canceled"))
    Assert(SpIsOs(SpCheckErr(core.IoResult(value: 7, err: core.SysErr.None, os: 0)), 1000007))
    // A failed start with a missing dir is NotFound, but not when
    // the failure was memory.
    Assert(PtSame(PtName(DirFailure(sys.io, "mk-proc-no-such-dir", OutOfMemory)), "OutOfMemory"))
    Assert(PtSame(PtName(DirFailure(sys.io, "mk-proc-no-such-dir", Os(code: 267))), "NotFound"))
}

test "Pty.Open and Resize refuse a size the runtime would answer Other for as Os(0), never OutOfMemory, and Pipe and Pty.Open still open" (sys: &System) {
    io := sys.io
    // The runtime's size check answers Other with no code, which
    // SpawnCheck reads as memory: SizeOk must refuse first, on both
    // targets, including sizes Linux's sys accepts (to 65,535).
    var bad: [5][2]int = [[0, 24], [80, 0], [32_768, 24], [80, 65_535], [65_536, 65_536]]
    for s in bad {
        e := Pty.Open(io, s[0], s[1]) else f {
            Assert(SpIsOs(f, 0))
            continue
        }
        e.Close()
        Assert(false)
    }
    p := Pty.Open(io, 80, 24).Must()
    for s in bad {
        p.Resize(io, s[0], s[1]) else f {
            Assert(SpIsOs(f, 0))
            continue
        }
        Assert(false)
    }
    p.Close()
    (r, w) := Pipe(io).Must()
    r.Close()
    w.Close()
}

// SpWaitCut is one Wait of child under a deadline of ms: its status, or
// the child back when the deadline cut the Wait (repeatable).
SpWaitCut(io: Io, child: Child, ms: i64): (st: ?Status, back: ?Child) {
    scope d := io.Scope() {
        d.CancelAfter(time.Millis(ms))
        st := child.Wait(io) else e {
            Assert(PtSame(PtName(e), "Canceled"))
            return (st: none, back: some(child))
        }
        return (st: some(st), back: none)
    }
}

// SpWaitCuts waits for child with each Wait under a deadline of 0 to 6
// ms, round after round, until one answers: its exit code and the Waits
// cut before it. At most 4,000 rounds.
SpWaitCuts(io: Io, child: Child, round: int, cuts: int): (code: int, cuts: int) {
    Assert(round < 4000)
    (st, back) := SpWaitCut(io, child, i64(round % 7))
    if c := back {
        Assert(st.IsNone())
        return SpWaitCuts(io, c, round + 1, cuts + 1)
    }
    s := st else {
        Panic("Wait gave neither a status nor the child")
    }
    return (code: SpCode(s), cuts: cuts)
}

test "Child.Wait cut by deadlines of 0 to 6 ms, round after round, keeps the child each time, and the last Wait gives its exit code, over 10 children (repeatable)" (sys: &System) {
    if sys.args.Len() > 2 {
        SpChild(sys)
    }
    io := sys.io
    var a: [6]str = [sys.args[0], sys.args[1], "gen", "0", "0", "7"]
    var c := DefaultCommand
    c.args = a[..]
    var total := 0
    for _ in 0..10 {
        (child, stdin, stdout, stderr) := Spawn(sys.arena, io, &c).Must()
        SpCloseEnds(stdin, stdout, stderr)
        (code, cuts) := SpWaitCuts(io, child, 0, 0)
        Assert(code == 7)
        total += cuts
    }
    // Round 0's deadline is 0 ms, and a child takes milliseconds to start
    // and end, so some Waits are cut.
    Assert(total > 0)
}

// SpPtyCutToEnd reads p to its end with each Read under a deadline of 0
// to 6 ms, round after round (at most 4,000): the bytes read.
SpPtyCutToEnd(io: Io, p: &Pty, buf: mut []u8): int {
    var total := 0
    for round in 0..4000 {
        var k := -1
        scope d := io.Scope() {
            d.CancelAfter(time.Millis(i64(round % 7)))
            k = p.Read(io, buf) else e {
                Assert(PtSame(PtName(e), "Canceled"))
                -1
            }
        }
        if k == 0 {
            return total
        }
        if k > 0 {
            total += k
        }
    }
    Panic("Pty.Read gave no end in 4,000 rounds")
}

// SpPtyPass reads p to its end: plainly in pass 0, cut by deadlines
// (SpPtyCutToEnd) in pass 1; the bytes read.
SpPtyPass(io: Io, p: &Pty, pass: int): int {
    var b: [256]u8 = [0; 256]
    if pass == 0 {
        (total, end) := SpPtyToEnd(io, p, mut b[..], 10000)
        Assert(end)
        return total
    }
    return SpPtyCutToEnd(io, p, mut b[..])
}

test "Pty.Read cut by deadlines of 0 to 6 ms, round after round, loses no byte: the end comes, and on Linux the same child's output has the same length read with and without cuts (repeatable)" (sys: &System) {
    if sys.args.Len() > 2 {
        SpChild(sys)
    }
    io := sys.io
    var a: [6]str = [sys.args[0], sys.args[1], "gen", "300", "0", "0"]
    var totals: [2]int = [0, 0]
    for pass in 0..2 {
        p := Pty.Open(io, 80, 24).Must()
        var c := DefaultCommand
        c.args = a[..]
        c.terminal = some(&p)
        (child, stdin, stdout, stderr) := Spawn(sys.arena, io, &c).Must()
        SpCloseEnds(stdin, stdout, stderr)
        // macOS reads before the wait: there the last close of a pty's
        // peer, at the child's exit, waits at most 60 ticks for the master
        // to read and then discards the output left (XNU's ptsclose).
        if Target.os == core.Os.Macos {
            totals[pass] = SpPtyPass(io, &p, pass)
            Assert(SpCode(child.Wait(io).Must()) == 0)
        } else {
            Assert(SpCode(child.Wait(io).Must()) == 0)
            totals[pass] = SpPtyPass(io, &p, pass)
        }
        p.Close()
    }
    Assert(totals[0] >= 300 && totals[1] >= 300)
    if !SpWin() {
        Assert(totals[0] == totals[1])
    }
}

// SpWriteAllOf is one WriteAll of data to w; a failure adds 1 to bad.
SpWriteAllOf(io: Io, w: &PipeWriter, data: []u8, bad: &raw.Atomic[u32]) {
    w.WriteAll(io, data) else {
        _ = bad.Add(1)
    }
}

// SpTwoRuns: b is n bytes of one of A and B, then n of the other.
SpTwoRuns(b: []u8, n: int): bool {
    if b.Len() != 2 * n || (b[0] != 65 && b[0] != 66) {
        return false
    }
    second: u8 = if b[0] == 65 { 66 } else { 65 }
    for i in 0..n {
        if b[i] != b[0] || b[n + i] != second {
            return false
        }
    }
    return true
}

test "two tasks' WriteAll of 1 MiB each to one PipeWriter never interleave, on a proc.Pipe end and on a Spawn-made stdin to a cat child, on both targets" (sys: &System) {
    if sys.args.Len() > 2 {
        SpChild(sys)
    }
    io := sys.io
    n := 1 << 20
    as := sys.arena.Slice[u8](n).Must()
    bs := sys.arena.Slice[u8](n).Must()
    for i in 0..n {
        as[i] = 65
        bs[i] = 66
    }
    got := sys.arena.Slice[u8](2 * n).Must()
    bad := raw.Atomic[u32].New(0)
    for _ in 0..3 {
        (r, w) := Pipe(io).Must()
        var k := 0
        scope g := io.Scope() {
            g.go SpWriteAllOf(io, &w, as[..], &bad)
            g.go SpWriteAllOf(io, &w, bs[..], &bad)
            SpDrain(io, &r, mut got[..], mut k)
        }
        w.Close()
        r.Close()
        Assert(bad.Load() == 0 && k == 2 * n && SpTwoRuns(got[..], n))
    }
    var a: [3]str = [sys.args[0], sys.args[1], "cat"]
    for _ in 0..3 {
        var c := DefaultCommand
        c.args = a[..]
        c.stdin = Stdio.Pipe
        c.stdout = Stdio.Pipe
        c.stderr = Stdio.Null
        (child, stdin, stdout, stderr) := Spawn(sys.arena, io, &c).Must()
        SpCloseEnds(none, none, stderr)
        w := stdin.Must()
        r := stdout.Must()
        var k := 0
        scope g := io.Scope() {
            g.go SpWriteAllOf(io, &w, as[..], &bad)
            g.go SpWriteAllOf(io, &w, bs[..], &bad)
            SpDrain(io, &r, mut got[..], mut k)
        }
        w.Close()
        var one: [1]u8 = [0]
        Assert(r.Read(io, mut one[..]).Must() == 0)
        r.Close()
        Assert(SpCode(child.Wait(io).Must()) == 0)
        Assert(bad.Load() == 0 && k == 2 * n && SpTwoRuns(got[..], n))
    }
}

// SpReadUntil reads r into buf from its start until the bytes read hold
// want, within ms: whether they do (false at the end, a full buf or the
// deadline).
SpReadUntil(io: Io, r: &PipeReader, buf: mut []u8, want: str, ms: i64): bool {
    var k := 0
    scope s := io.Scope() {
        s.CancelAfter(time.Millis(ms))
        for k < buf.Len() {
            n := r.Read(io, mut buf[k..]) else {
                return false
            }
            if n == 0 {
                return false
            }
            k += n
            if SpHas(buf[0..k], want) {
                return true
            }
        }
    }
    return false
}

// SpWaitWithin is child's status within ms; past it the child is killed
// and reaped and the block fails.
SpWaitWithin(io: Io, child: Child, ms: i64): Status {
    (st, back) := SpWaitCut(io, child, ms)
    if c := back {
        c.Kill() else {}
        _ = c.Wait(io.NoCancel()).Must()
        Panic("the child did not end in time")
    }
    return st else {
        Panic("Wait gave neither a status nor the child")
    }
}

test "Child.Signal sends Interrupt as SIGINT and Hangup as SIGHUP, and a child ended by SIGINT or SIGHUP waits as Signal.Interrupt or Signal.Hangup, with group false and true (Linux; both Unsupported on Windows)" (sys: &System) {
    if sys.args.Len() > 2 {
        SpChild(sys)
    }
    io := sys.io
    if SpWin() {
        var a: [3]str = [sys.args[0], sys.args[1], "sleep"]
        var c := DefaultCommand
        c.args = a[..]
        (child, stdin, stdout, stderr) := Spawn(sys.arena, io, &c).Must()
        SpCloseEnds(stdin, stdout, stderr)
        for sig in [Signal.Interrupt, Signal.Hangup] {
            var failed := false
            child.Signal(sig) else e {
                failed = PtSame(PtName(e), "Unsupported")
            }
            Assert(failed)
        }
        child.Kill().Must()
        Assert(SpCode(child.Wait(io).Must()) == 1)
        return
    }
    // SignalNumber against the kernel's numbering: the shell traps each
    // signal by its name and prints which one came.
    trap := "trap 'echo got:INT; exit 0' INT; trap 'echo got:HUP; exit 0' HUP; echo ready; while :; do sleep 0.05; done"
    var sh: [3]str = ["/bin/sh", "-c", trap]
    // StatusOf against it: the shell ends itself by the signal's name.
    var selfint: [3]str = ["/bin/sh", "-c", "kill -INT $$; sleep 5"]
    var selfhup: [3]str = ["/bin/sh", "-c", "kill -HUP $$; sleep 5"]
    for group in [false, true] {
        for which in 0..2 {
            sig := if which == 0 { Signal.Interrupt } else { Signal.Hangup }
            var c := DefaultCommand
            c.args = sh[..]
            c.stdin = Stdio.Null
            c.stdout = Stdio.Pipe
            c.group = group
            (child, stdin, stdout, stderr) := Spawn(sys.arena, io, &c).Must()
            SpCloseEnds(stdin, none, stderr)
            r := stdout.Must()
            var b: [256]u8 = [0; 256]
            Assert(SpReadUntil(io, &r, mut b[..], "ready", 10000))
            child.Signal(sig).Must()
            Assert(SpReadUntil(io, &r, mut b[..], if which == 0 { "got:INT" } else { "got:HUP" }, 10000))
            r.Close()
            Assert(SpCode(SpWaitWithin(io, child, 10000)) == 0)
            c.args = if which == 0 { selfint[..] } else { selfhup[..] }
            c.stdout = Stdio.Null
            (child: k, stdin: ki, stdout: ko, stderr: ke) := Spawn(sys.arena, io, &c).Must()
            SpCloseEnds(ki, ko, ke)
            st := SpWaitWithin(io, k, 10000)
            got := st.signal else { Signal.Terminate }
            Assert(st.code.IsNone() && got == sig)
        }
    }
}

test "WaitSignal cut by a deadline is Canceled with the scope expired, and the same call again is cut the same way (repeatable), on both targets" (sys: &System) {
    io := sys.io
    for _ in 0..3 {
        var cut := false
        scope s := io.Scope() {
            s.CancelAfter(time.Millis(20))
            WaitSignal(io, Signal.Hangup) else {
                cut = s.Expired()
            }
        }
        Assert(cut)
    }
}

// SpX1Read reads p under 20 ms deadlines until stop is set; an answer
// other than bytes, a cut or the end adds 1 to errs, and a marker byte
// (254, which only SpX1Churn's pipes carry) among the bytes 1 to bad.
SpX1Read(io: Io, p: &Pty, stop: &raw.Atomic[u32], errs: &raw.Atomic[u32], bad: &raw.Atomic[u32]) {
    var b: [4096]u8 = [0; 4096]
    for stop.Load() == 0 {
        n := SpX1ReadOnce(io, p, mut b[..])
        if n < 0 {
            _ = errs.Add(1)
        }
        m := if n > 0 { n } else { 0 }
        for i in 0..m {
            if b[i] == 254 {
                _ = bad.Add(1)
            }
        }
        if n == 0 {
            io.Sleep(time.Millis(1)) else {}
        }
    }
}

// SpX1ReadOnce is one Pty.Read under a 20 ms deadline: the bytes, 0 for a
// cut or the end, -1 for any other answer.
SpX1ReadOnce(io: Io, p: &Pty, b: mut []u8): int {
    scope s := io.Scope() {
        s.CancelAfter(time.Millis(20))
        n := p.Read(io, b) else e {
            return if PtSame(PtName(e), "Canceled") { 0 } else { -1 }
        }
        return n
    }
}

// SpX1Write writes "x" to p under 20 ms deadlines until stop is set; an
// invalid handle (Os(6)) adds 1 to bad (a closed console's input pipe may
// refuse a write, which is no fault).
SpX1Write(io: Io, p: &Pty, stop: &raw.Atomic[u32], bad: &raw.Atomic[u32]) {
    for stop.Load() == 0 {
        scope s := io.Scope() {
            s.CancelAfter(time.Millis(20))
            p.WriteAll(io, "x".AsBytes()) else e {
                if SpIsOs(e, 6) {
                    _ = bad.Add(1)
                }
            }
        }
        io.Sleep(time.Millis(1)) else {}
    }
}

// SpX1Churn makes, fills with markers and closes pipes until stop is set:
// another byte in one (the Pty's "x") adds 1 to bad.
SpX1Churn(io: Io, stop: &raw.Atomic[u32], bad: &raw.Atomic[u32]) {
    var mark: [64]u8 = [254; 64]
    var b: [256]u8 = [0; 256]
    for stop.Load() == 0 {
        (r, w) := Pipe(io).Must()
        w.WriteAll(io, mark[..]) else {}
        w.Close()
        var k := 0
        SpDrain(io, &r, mut b[..], mut k)
        r.Close()
        for i in 0..k {
            if b[i] != 254 {
                _ = bad.Add(1)
            }
        }
        if k != 64 {
            _ = bad.Add(1)
        }
    }
}

test "a Pty read and written by two tasks while a third Spawns on it, each child waited for so Windows reopens the console, never reaches a closed or reused handle (blocked on rt's ConsoleSwap)" (sys: &System) {
    if sys.args.Len() > 2 {
        SpChild(sys)
    }
    // Windows only: Linux's Pty keeps its one handle. Runs with
    // MK_PROC3_BLOCKED until rt stops closing the handles under a Read or
    // WriteAll.
    if !SpWin() || !SpRunBlocked(sys) {
        return
    }
    io := sys.io
    p := Pty.Open(io, 80, 24).Must()
    var a: [6]str = [sys.args[0], sys.args[1], "gen", "0", "0", "0"]
    stop := raw.Atomic[u32].New(0)
    // One counter per symptom, so the failing Assert names it.
    rerr := raw.Atomic[u32].New(0)
    rmark := raw.Atomic[u32].New(0)
    wbad := raw.Atomic[u32].New(0)
    churn := raw.Atomic[u32].New(0)
    spawns := raw.Atomic[u32].New(0)
    scope g := io.Scope() {
        // Four readers, not one: with
        // one the block failed 2 runs in 3, with four a probe
        // saw over 100 read errors in every run.
        for _ in 0..4 {
            g.go SpX1Read(io, &p, &stop, &rerr, &rmark)
        }
        g.go SpX1Write(io, &p, &stop, &wbad)
        g.go SpX1Churn(io, &stop, &churn)
        g.go SpX1Churn(io, &stop, &churn)
        for _ in 0..300 {
            var c := DefaultCommand
            c.args = a[..]
            c.terminal = some(&p)
            (child, stdin, stdout, stderr) := Spawn(sys.arena, io, &c) else {
                _ = spawns.Add(1)
                continue
            }
            SpCloseEnds(stdin, stdout, stderr)
            _ = child.Wait(io).Must()
        }
        stop.Store(1)
    }
    p.Close()
    Assert(spawns.Load() == 0)
    Assert(rmark.Load() == 0)
    Assert(churn.Load() == 0)
    Assert(wbad.Load() == 0)
    Assert(rerr.Load() == 0)
}

// SpHold Reads an empty proc.Pipe end under a 3 s deadline; done counts
// the holders whose Read has returned. A waiting Read holds none of the
// blocking pool's 64 threads.
SpHold(io: Io, done: &raw.Atomic[u32]) {
    (r, w) := Pipe(io).Must()
    var b: [16]u8 = [0; 16]
    scope t := io.Scope() {
        t.CancelAfter(time.Seconds(3))
        _ = r.Read(io, mut b[..]) else {}
    }
    _ = done.Add(1)
    r.Close()
    w.Close()
}

// SpNapJob is a pool call's job that sleeps 3 s on its pool thread, as a
// slow disk call would, and is not interrupted; a.res stays as made.
SpNapJob(a: *core.IoCallArgs) {
    sys.SleepNanos(3_000_000_000)
}

// SpPoolHold holds one of the blocking pool's 64 threads for 3 s with a
// pool call (core.IoRoutePool, not a wait job); done counts the
// holders whose call has returned.
SpPoolHold(io: Io, done: &raw.Atomic[u32]) {
    var a := core.IoCallArgs(op: core.IoOpSpawn, route: core.IoRoutePool, h: 0, os: 0, p: raw.Null[u8](), pn: 0,
        q: raw.Null[u8](), qn: 0, out: raw.Null[u8](), outn: 0, w: core.IoWords(a: 0, b: 0, c: 0, d: 0),
        res: core.IoResult(value: 0, err: core.SysErr.None, os: 0), job: SpNapJob, stop: raw.Null[u32]())
    _ = core.IoCall(io.NoCancel(), raw.OfMut(mut a))
    _ = done.Add(1)
}

// SpSpawnHeld spawns c under a 200 ms deadline: 0 when Spawn was
// Canceled, 1 when it returned the child (then waited for, uncancellably).
// late is set to the holders done counts as Spawn returns.
SpSpawnHeld(sys: &System, c: &Command, done: &raw.Atomic[u32], late: mut u32): int {
    io := sys.io
    scope d := io.Scope() {
        d.CancelAfter(time.Millis(200))
        (child, stdin, stdout, stderr) := Spawn(sys.arena, io, c) else e {
            *late = done.Load()
            Assert(PtSame(PtName(e), "Canceled"))
            return 0
        }
        *late = done.Load()
        SpCloseEnds(stdin, stdout, stderr)
        _ = child.Wait(io.NoCancel()).Must()
    }
    return 1
}

test "with the blocking pool's 64 threads held by pool calls, Pty.Close returns before any holder does (Windows), and a Spawn under a 200 ms deadline is Canceled before any holder returns (both targets)" (sys: &System) {
    if sys.args.Len() > 2 {
        SpChild(sys)
    }
    // A queued whole-route job is cut by its scope's cancel, and Windows
    // Pty.Close closes the pseudo-console on a thread of its own. The
    // holders are pool calls that sleep 3 s (SpPoolHold): a Read of a
    // proc.Pipe end holds none of the 64 threads,
    // so with Read holders the Spawn half would pass for the wrong reason
    // (the child starts at once). The holders' 3 s is the margin: Pty.Close
    // returns at once and Spawn at its 200 ms deadline, 300 ms after the
    // holders started. Only a fix that cuts the queued start passes, not
    // one that starts the child within the deadline.
    io := sys.io
    var late: u32 = 0
    if SpWin() {
        closed := raw.Atomic[u32].New(0)
        scope g2 := io.Scope() {
            for _ in 0..64 {
                g2.go SpPoolHold(io, &closed)
            }
            io.Sleep(time.Millis(300)) else {}
            p := Pty.Open(io, 80, 24).Must()
            p.Close()
            late = closed.Load()
        }
        Assert(late == 0)
    }
    var a: [6]str = [sys.args[0], sys.args[1], "gen", "0", "0", "0"]
    var c := DefaultCommand
    c.args = a[..]
    done := raw.Atomic[u32].New(0)
    var spawned := 0
    scope g := io.Scope() {
        for _ in 0..64 {
            g.go SpPoolHold(io, &done)
        }
        io.Sleep(time.Millis(300)) else {}
        spawned = SpSpawnHeld(sys, &c, &done, mut late)
    }
    // A spawn job that waited for a holder's thread would start the child
    // after the deadline: spawned 1 and late above 0.
    Assert(spawned == 0)
    Assert(late == 0)
}

test "Windows' PATH search of a name holds its sub-arena's first chunk, not a 96 KiB candidate buffer, and still finds the program" (sys: &System) {
    if !SpWin() {
        return
    }
    var x := sys.arena.Sub().Must()
    p := FindWindows(&x, sys.io, "cmd").Must()
    b := p.AsBytes()
    Assert(b.Len() > 8 && SpFoldPrefix(b[b.Len() - 8..], "\\cmd.exe"))
    held := core.ArenaHeld(&x)
    x.Free()
    // The old buffer alone was CandBytes (96 KiB); the new one is sized
    // from PATH and PATHEXT (CandLen), so on any PATH shorter than about
    // 90 KiB the sub-arena holds less than that (4 KiB, its first chunk,
    // on a typical Windows PATH).
    Assert(held < CandBytes)
}

// On Windows a Pipe holds 64 KiB written ahead of its reader (the size Pipe
// asks CreatePipe for), so a 32 KiB WriteAll with nobody reading returns at
// once: under a 2 s deadline it must not be cut, and the reader then gets
// every byte in order. Elsewhere the capacity is the system's (on Linux a
// default of 64 KiB, which can be changed; on macOS it grows from 16 KiB),
// so the block asserts nothing there.
test "proc.Pipe holds a 32 KiB WriteAll with no reader waiting on Windows, and the reader then gets every byte" (sys: &System) {
    if !SpWin() {
        return
    }
    io := sys.io
    (r, w) := Pipe(io).Must()
    var data: [32768]u8 = [0; 32768]
    for i in 0..data.Len() {
        data[i] = Trunc[u8](i * 7)
    }
    var wrote := false
    scope d := io.Scope() {
        d.CancelAfter(time.Millis(2000))
        var ok := true
        w.WriteAll(io, data[..]) else {
            ok = false
        }
        wrote = ok
    }
    Assert(wrote)
    w.Close()
    var got: [32768]u8 = [0; 32768]
    var n := 0
    for {
        k := r.Read(io, mut got[n..]).Must()
        if k == 0 {
            break
        }
        n += k
    }
    r.Close()
    Assert(n == data.Len())
    for i in 0..n {
        Assert(got[i] == data[i])
    }
}
