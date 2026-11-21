// Why: std.rt's process and terminal API (pipes, spawning with a directory,
// job or pseudo-console, killing a group, raw console mode) needs Windows
// calls a plain spawn does not. They live in a file of their own so a
// program links them, and imports CreatePipe and the job-object calls, only
// when it names std.rt's functions (std/rt/proc.mk); proc.mk's spawn stays
// what the IoTable's spawn entry links.
//
// What: Pipe, PipeAttach, PipeLend, SpawnEx, ChildSignal, GroupEnd,
// GroupClose, TryReap, PtyOpen, PtyResize, PtyClose, TermOpen, TermMode,
// TermRecords and the reaper set (ReapSetOpen, ReapSetAdd, ReapSetWait).
//
// How:
// - Pipe: CreatePipe, both ends synchronous and not inheritable, so an end
//   lent to a child is a plain inheritable duplicate (Rust's Command makes
//   the child's ends synchronous for the same reason); PipeAttach is false
//   and PipeLend lends an end as it is.
// - SpawnEx: proc.mk's program search and quoting (Line, the batch-file
//   rule included), then CreateProcessW with STARTUPINFOEXW and an
//   attribute list: the handle list (only the listed handles are
//   inherited), for a group the job list (PROC_THREAD_ATTRIBUTE_JOB_LIST,
//   so the child is in its job before its first instruction), for a
//   terminal the pseudo-console; the environment block
//   (CREATE_UNICODE_ENVIRONMENT) and the directory. A group's job is
//   CreateJobObjectW's with no limit set: no kill-on-close.
// - ChildSignal, GroupEnd: TerminateJobObject or TerminateProcess for a
//   kill; no other signal can be delivered (Unsupported).
// - PtyOpen, PtyResize, PtyClose: two anonymous pipes and
//   CreatePseudoConsole, ResizePseudoConsole and ClosePseudoConsole, found
//   at run time in kernel32 (Windows 10 1809; Unsupported before).
// - TermOpen, TermMode: CONIN$ and CONOUT$; GetConsoleMode and
//   SetConsoleMode.
// Errors are MapError's (sys.mk) with call.mk's FailCall rows.

import std.core
import std.raw

const AttrJobList: uint = 0x0002_000D
const AttrPseudoConsole: uint = 0x0002_0016
// PgHandleList is SpawnEx's handle list (three streams and PassMax named
// handles), then the job list's one handle: 544 bytes after
// PgName's 64 units, before PgQuoted.
const PgHandleList: int = 133440

// ReOpenAny opens the disk file h names again as a new synchronous file
// object at position 0 (ReOpenFile), with the access h was granted and
// never more (NtQueryObject's ObjectBasicInformation.GrantedAccess, masked
// to the data, attribute, extended-attribute, read-control and
// synchronize rights): a read-only handle's copy is read-only, an
// append-only one's append-only, though the file's ACL would allow more.
// value the new handle, not inheritable. Other when ntdll's NtQueryObject
// cannot be found or answers an error. O(1): three system calls besides
// ntdll's lookup; allocates nothing.
ReOpenAny(h: uint): OsResult {
    var granted: u32 = 0
    unsafe "ntdll; the name is a NUL-terminated constant; info and ret are this frame's" {
        var name: [16]u16 = [0; 16]
        _ = Wide("ntdll.dll", mut name[..])
        m := GetModuleHandleW(raw.Data(name[..]))
        p := if m != 0 { GetProcAddress(m, raw.StrData("NtQueryObject\u{0}")) } else { raw.Null[u8]() }
        if raw.IsNull(p) {
            return FailWith(core.SysErr.Other)
        }
        var a := u64(raw.Addr(p))
        f := raw.Read(raw.PtrCast[u64, fn(uint, u32, *u8, u32, *u32): i32](raw.Of(&a)))
        // PUBLIC_OBJECT_BASIC_INFORMATION: Attributes, GrantedAccess, then
        // counts and reserved words, 56 bytes.
        var info: [14]u32 = [0; 14]
        var ret: u32 = 0
        if f(h, ObjectBasicInformation, raw.PtrCast[u32, u8](raw.DataMut(mut info[..])), 56, raw.OfMut(mut ret)) != 0 {
            return FailWith(core.SysErr.Other)
        }
        granted = info[1]
    }
    return ReOpen(h, granted & ReOpenRights)
}

// ObjectBasicInformation is OBJECT_INFORMATION_CLASS 0.
const ObjectBasicInformation: u32 = 0
// ReOpenRights: FILE_READ_DATA, FILE_WRITE_DATA, FILE_APPEND_DATA,
// FILE_READ_EA, FILE_WRITE_EA, FILE_READ_ATTRIBUTES, FILE_WRITE_ATTRIBUTES,
// READ_CONTROL and SYNCHRONIZE; DELETE, WRITE_DAC, WRITE_OWNER and
// FILE_EXECUTE are never given on.
const ReOpenRights: u32 = 0x0012_019F

ReOpen(h: uint, access: u32): OsResult {
    unsafe "kernel32; passes only values and a handle" {
        n := ReOpenFile(h, access, ShareAll, 0)
        if n == InvalidHandle {
            return LastError()
        }
        return Ok(Trunc[i64](n))
    }
}

// AppendHandles adds MK_HANDLES=<h>,<h>,... at the end of the
// environment block out, which ends with an empty entry and has room for
// 12 units and 21 per handle more.
AppendHandles(out: *u16, hs: []uint) {
    var o := 0
    unsafe "out ends with an empty entry (one NUL) and has the room above" {
        for raw.Read(raw.Offset(out, o)) != 0 {
            for raw.Read(raw.Offset(out, o)) != 0 {
                o += 1
            }
            o += 1
        }
        PutUnits(raw.Offset(out, o), "MK_HANDLES=")
        var k := o + 11
        for i, h in hs {
            if i > 0 {
                raw.Write(raw.Offset(out, k), 44)
                k += 1
            }
            k = PutDecimal(out, k, u64(h))
        }
        raw.Write(raw.Offset(out, k), 0)
        raw.Write(raw.Offset(out, k + 1), 0)
    }
}
const ShareReadWrite: u32 = 3
// Console input modes: processed, line, echo, window (the condition for
// WINDOW_BUFFER_SIZE_EVENT records, std.term's resizes), virtual-terminal
// input.
const InProcessed: u32 = 1
const InLine: u32 = 2
const InEcho: u32 = 4
const InWindow: u32 = 8
const InVt: u32 = 0x200
// Console output modes: processed, virtual-terminal processing, no
// newline auto return (so "\n" moves down only, as Linux's raw mode has
// OPOST off).
const OutProcessed: u32 = 1
const OutVt: u32 = 4
const OutNoAutoReturn: u32 = 8
// ConModeBytes: the saved input mode, then the output mode, each a u32.
const ConModeBytes: int = 8

/// Pipe makes an anonymous pipe: value the read end, os the write end,
/// both synchronous and not inheritable, with a buffer of PipeBytes
/// (proc.mk's 64 KiB, Linux's default pipe capacity; 0 would give the
/// system's one page), so a writer can be 64 KiB ahead of the reader before
/// a write waits for it. O(1): one system call.
export Pipe(): OsResult {
    var r: uint = 0
    var w: uint = 0
    unsafe "kernel32; r and w are this frame's" {
        if CreatePipe(raw.OfMut(mut r), raw.OfMut(mut w), raw.Null[u8](), PipeBytes) == 0 {
            return LastCall()
        }
    }
    return OsResult(value: Trunc[i64](r), err: core.SysErr.None, os: Trunc[i64](w))
}

/// PipeAttach has std/sys/linux's contract; on Windows it is false: an end
/// Pipe made stays synchronous, a pool end. A port needs an
/// overlapped pipe, a child's ReadFile and WriteFile pass no OVERLAPPED,
/// and neither end of a named pipe can be opened again as a synchronous
/// handle on the same instance (ReOpenFile of the client end connects a
/// new instance, of the server end is denied). O(1); allocates nothing.
export PipeAttach(fd: uint): bool {
    return false
}

/// PipeLend has std/sys/linux's contract; on Windows an end is lent as it
/// is (value fd, os 0), since PipeAttach never attaches one. O(1);
/// allocates nothing.
export PipeLend(fd: uint): OsResult {
    return OsResult(value: Trunc[i64](fd), err: core.SysErr.None, os: 0)
}

// Fold is u with ASCII letters upper-cased (environment names compare
// ignoring case, as SetEnvironmentVariableW's lookup does for ASCII).
Fold(u: u16): u16 {
    return if u >= 97 && u <= 122 { u - 32 } else { u }
}

// SameName: the NUL-terminated entries a and b have the same name, the units
// before the first '=' past the first unit (so "=C:=C:\x" is named "=C:").
SameName(a: *u16, b: *u16): bool {
    var i := 0
    unsafe "a and b are NUL-terminated entries: a NUL ends the compare" {
        for {
            x := raw.Read(raw.Offset(a, i))
            y := raw.Read(raw.Offset(b, i))
            xe := x == 0 || (x == 61 && i > 0)
            ye := y == 0 || (y == 61 && i > 0)
            if xe || ye {
                return xe && ye
            }
            if Fold(x) != Fold(y) {
                return false
            }
            i += 1
        }
    }
}

// Unshadow drops each of the first o units' entries whose name a later one
// of them has (names compared as SameName does, ignoring case), moving the
// rest down in order: the last entry of a name wins, as Go's os/exec
// (dedupEnv) and Rust's Command give it. The units kept. O(entries squared
// times a name's units); allocates nothing.
Unshadow(out: *u16, o: int): int {
    var r := 0
    var w := 0
    unsafe "out[0..o] holds NUL-terminated entries; w <= r, so the copy moves down" {
        for r < o {
            var e := r
            for raw.Read(raw.Offset(out, e)) != 0 {
                e += 1
            }
            var later := e + 1
            var shadowed := false
            for later < o && !shadowed {
                shadowed = SameName(raw.Offset(out, later), raw.Offset(out, r))
                for raw.Read(raw.Offset(out, later)) != 0 {
                    later += 1
                }
                later += 1
            }
            if !shadowed {
                for k in 0..(e - r + 1) {
                    raw.Write(raw.Offset(out, w + k), raw.Read(raw.Offset(out, r + k)))
                }
                w += e - r + 1
            }
            r = e + 1
        }
    }
    return w
}

// EnvBlock writes the child's environment block to out (cap units): for
// mode IoEnvReplace or IoEnvExtend the entries of env first (when env names
// a variable twice, the later entry wins), then for
// IoEnvInherit or IoEnvExtend each entry of this process's block parent
// (null for none) that is not MK_INHERIT's and, for IoEnvExtend, not named
// by one of env's. The units written, ending with the block's two NULs, or
// -1 when an entry holds a NUL or the block does not fit. O(units of env
// and parent, plus entries of parent times entries of env for IoEnvExtend).
EnvBlock(parent: *u16, env: []str, mode: u32, out: *u16, cap: int): int {
    var o := 0
    unsafe "out has cap units; parent ends with two NULs" {
        if mode != core.IoEnvInherit {
            for e in env {
                n := Wide(e, raw.SliceMut(raw.Offset(out, o), cap - 2 - o))
                if n < 0 {
                    return -1
                }
                o += n
            }
            o = Unshadow(out, o)
        }
        given := o
        if mode != core.IoEnvReplace && !raw.IsNull(parent) {
            var i := 0
            for raw.Read(raw.Offset(parent, i)) != 0 {
                p := raw.Offset(parent, i)
                var j := i
                for raw.Read(raw.Offset(parent, j)) != 0 {
                    j += 1
                }
                var keep := !IsInheritName(p)
                if keep && mode == core.IoEnvExtend {
                    var g := 0
                    for g < given && keep {
                        if SameName(raw.Offset(out, g), p) {
                            keep = false
                        }
                        for raw.Read(raw.Offset(out, g)) != 0 {
                            g += 1
                        }
                        g += 1
                    }
                }
                if keep {
                    if o + (j - i + 1) > cap - 2 {
                        return -1
                    }
                    raw.Copy(raw.Offset(out, o), p, j - i + 1)
                    o += j - i + 1
                }
                i = j + 1
            }
        }
        // The block's end: an empty entry, and a second NUL for an empty
        // block.
        raw.Write(raw.Offset(out, o), 0)
        raw.Write(raw.Offset(out, o + 1), 0)
        return o + 2
    }
}

/// SpawnEx starts args[0] (found and quoted as proc.mk's Spawn does) as s
/// says (core.IoSpawnReq): src[i] the OS handle of a lent stream
/// (IoStdHandle), pty a pseudo-console (0 none), which replaces the three
/// streams. env is used as s.env_mode says, dir (empty: this process's) is
/// the child's working directory. value the process handle, os the
/// process id; s.pipes the parent's pipe ends (overlapped named pipes on
/// the port, proc.mk's ChildStream), s.pid, s.job the group's job (0
/// without a group). Every handle made for the child is closed before it
/// returns, and the parent's pipe ends and the job too on a failure. A NUL
/// in an argument, an entry or dir, an empty program, or an unknown stream
/// mode: InvalidPath or Other, with no child. O(bytes of args, env and the
/// environment, plus entries of env times the environment's for
/// IoEnvExtend); the pages it maps are given back before it returns.
///
/// named are the OS handles of the handles the Command names, at
/// most PassMax (else Other), and reopen's bit i says named[i] is a disk
/// file opened for overlapped I/O: the child gets an inheritable
/// duplicate of each (for a reopen bit, a new synchronous file object at
/// position 0, ReOpenFile's, with the access named[i] was granted and no
/// more, ReOpenAny), on the handle list beside its
/// streams, and MK_HANDLES=<value>,... in its environment (this process's
/// own MK_HANDLES and MK_INHERIT are never passed on).
export SpawnEx(sp: *u8, args: []str, env: []str, dir: str, s: mut core.IoSpawnReq, src: [3]uint, pty: uint,
    named: []uint, reopen: u64): OsResult {
    if args.Len() == 0 || args[0].Len() == 0 {
        return FailWith(core.SysErr.InvalidPath)
    }
    if named.Len() > PassMax {
        return FailWith(core.SysErr.Other)
    }
    var qbytes := 2
    for a in args {
        for b in a.AsBytes() {
            if b == 0 {
                return FailWith(core.SysErr.InvalidPath)
            }
        }
        qbytes += a.Len() * 2 + 3
    }
    if qbytes > MaxCmdUnits * 4 {
        return Fail(206)
    }
    var given := 0
    for e in env {
        given += e.Len() + 1
    }
    for i in 0..3 {
        if s.stdio[i] > core.IoStdJoin || (s.stdio[i] == core.IoStdJoin && i != 2) {
            return FailWith(core.SysErr.Other)
        }
    }
    if s.env_mode > core.IoEnvExtend {
        return FailWith(core.SysErr.Other)
    }
    var envp := raw.Null[u16]()
    var env_units := 0
    if s.env_mode != core.IoEnvReplace {
        unsafe "kernel32; the block ends with two NULs" {
            envp = GetEnvironmentStringsW()
            if raw.IsNull(envp) {
                return LastError()
            }
            for raw.Read(raw.Offset(envp, env_units)) != 0 {
                for raw.Read(raw.Offset(envp, env_units)) != 0 {
                    env_units += 1
                }
                env_units += 1
            }
        }
    }
    // MK_HANDLES=, then at most 20 digits and a comma per named handle.
    cap := env_units + given + 4 + 12 + named.Len() * 21
    cmd_off := PgQuoted + PageRound(qbytes)
    env_off := cmd_off + PageRound((qbytes + 1) * 2)
    dir_off := env_off + PageRound(cap * 2)
    total := PageRound(dir_off + (dir.Len() + 1) * 2)
    pg := Reserve(total, 4096)
    if raw.IsNull(pg) || !Commit(pg, total) {
        FreeMap(pg, total)
        if !raw.IsNull(envp) {
            unsafe "kernel32; envp is the block GetEnvironmentStringsW returned, freed once here" {
                _ = FreeEnvironmentStringsW(envp)
            }
        }
        return FailWith(core.SysErr.Other)
    }
    var r := Ok(0)
    // The pages are fresh (Reserve and Commit above): the OS zeroes them,
    // so they are not filled again (at least 135 KB a spawn).
    unsafe "the call's pages, zeroed by the OS at their commit" {
        n := EnvBlock(envp, env, s.env_mode, raw.PtrCast[u8, u16](raw.Offset(pg, env_off)), cap)
        if !raw.IsNull(envp) {
            _ = FreeEnvironmentStringsW(envp)
        }
        var wdir := raw.Null[u16]()
        if n < 0 {
            r = FailWith(core.SysErr.InvalidPath)
        } else if dir.Len() > 0 {
            wdir = raw.PtrCast[u8, u16](raw.Offset(pg, dir_off))
            if Wide(dir, raw.SliceMut(wdir, dir.Len() + 1)) < 0 {
                r = FailWith(core.SysErr.InvalidPath)
            }
        }
        if r.err == core.SysErr.None {
            var cmd := raw.Null[u16]()
            var bm := raw.Null[u8]()
            var bsz := 0
            r = Line(pg, args, cmd_off, mut cmd, mut bm, mut bsz)
            if r.err == core.SysErr.None {
                r = HandlesEx(pg, cmd, raw.PtrCast[u8, u16](raw.Offset(pg, env_off)), wdir, s, src, pty, named,
                    reopen)
            }
            if bsz > 0 {
                Release(bm, bsz)
            }
        }
    }
    Release(pg, total)
    return r
}

// HandlesEx makes the child's handles and its job for SpawnEx and calls
// CreateEx; every handle it made for the child is closed before it
// returns, and the parent's pipe ends and the job too on a failure. The
// named handles' inheritable copies go on the list after the streams', and
// their values into env's MK_HANDLES entry.
HandlesEx(pg: *u8, cmd: *u16, env: *u16, dir: *u16, s: mut core.IoSpawnReq, src: [3]uint, pty: uint, named: []uint,
    reopen: u64): OsResult {
    unsafe "the call's pages hold every region named by the Pg offsets" {
        sa := raw.Offset(pg, PgSa)
        Put32(sa, 0, 24)
        Put32(sa, 16, 1)
        var theirs: [3]uint = [0; 3]
        var mine: [3]uint = [0; 3]
        var kids: [64]uint = [0; 64]
        var r := Ok(0)
        for i, h in named {
            if reopen & (u64(1) << Trunc[u64](i)) != 0 {
                r = ReOpenAny(h)
                if r.err == core.SysErr.None {
                    kids[i] = Trunc[uint](r.value)
                    if SetHandleInformation(kids[i], HandleFlagInherit, HandleFlagInherit) == 0 {
                        r = LastError()
                    }
                }
            } else {
                var d: uint = 0
                if DuplicateHandle(GetCurrentProcess(), h, GetCurrentProcess(), raw.OfMut(mut d), 0, 1,
                    DuplicateSameAccess) == 0 {
                    r = LastError()
                } else {
                    kids[i] = d
                }
            }
            if r.err != core.SysErr.None {
                break
            }
        }
        if r.err == core.SysErr.None && named.Len() > 0 {
            AppendHandles(env, kids[0..named.Len()])
        }
        if r.err == core.SysErr.None && pty == 0 {
            for i in 0..3 {
                mode := s.stdio[i]
                if mode == core.IoStdHandle || mode == core.IoStdJoin {
                    from := if mode == core.IoStdHandle { src[i] } else { theirs[1] }
                    if from != 0 {
                        var d: uint = 0
                        if DuplicateHandle(GetCurrentProcess(), from, GetCurrentProcess(), raw.OfMut(mut d), 0, 1,
                            DuplicateSameAccess) == 0 {
                            r = LastError()
                        } else {
                            theirs[i] = d
                        }
                    }
                } else {
                    r = ChildStream(i, mode, sa, raw.PtrCast[u8, u16](raw.Offset(pg, PgName)), mut theirs[i],
                        mut mine[i])
                }
                if r.err != core.SysErr.None {
                    break
                }
            }
        }
        var job: uint = 0
        if r.err == core.SysErr.None && s.group {
            job = CreateJobObjectW(raw.Null[u8](), raw.Null[u16]())
            if job == 0 {
                r = LastError()
            }
        }
        if r.err == core.SysErr.None {
            r = CreateEx(pg, cmd, env, dir, theirs, job, pty, kids[0..named.Len()])
        }
        for i in 0..3 {
            if theirs[i] != 0 {
                _ = CloseHandle(theirs[i])
            }
        }
        for k in kids {
            if k != 0 {
                _ = CloseHandle(k)
            }
        }
        if r.err != core.SysErr.None {
            for i in 0..3 {
                if mine[i] != 0 {
                    _ = CloseHandle(mine[i])
                }
            }
            if job != 0 {
                _ = CloseHandle(job)
            }
            return r
        }
        s.pipes = mine
        s.pid = r.os
        s.job = job
        return r
    }
}

// CreateEx fills the attribute list (the non-zero handles of theirs and
// kids, the job, the pseudo-console) and calls CreateProcessW; value the
// process handle (the thread's is closed at once), os the process id.
CreateEx(pg: *u8, cmd: *u16, env: *u16, dir: *u16, theirs: [3]uint, job: uint, pty: uint, kids: []uint): OsResult {
    unsafe "pg is the call's committed pages, laid out for si, pi and the lists" {
        app := raw.PtrCast[u8, u16](raw.Offset(pg, PgApp))
        si := raw.Offset(pg, PgSi)
        pi := raw.Offset(pg, PgPi)
        list := raw.Offset(pg, PgHandleList)
        var n := 0
        for i in 0..3 {
            if theirs[i] != 0 {
                Put64(list, n * 8, u64(theirs[i]))
                n += 1
            }
        }
        for k in kids {
            Put64(list, n * 8, u64(k))
            n += 1
        }
        // The job list's one handle sits after the handle list's room
        // (three streams and PassMax named handles).
        jl := raw.Offset(list, (3 + PassMax) * 8)
        Put64(jl, 0, u64(job))
        var count: u32 = 0
        if n > 0 {
            count += 1
        }
        if job != 0 {
            count += 1
        }
        if pty != 0 {
            count += 1
        }
        Put32(si, 0, 112)
        // With a pseudo-console the standard handles are left null, so the
        // child's streams are the console's, never this process's.
        Put32(si, 60, StartfUseStdHandles)
        if pty == 0 {
            Put64(si, 80, u64(theirs[0]))
            Put64(si, 88, u64(theirs[1]))
            Put64(si, 96, u64(theirs[2]))
        }
        var attr := raw.Null[u8]()
        if count > 0 {
            attr = raw.Offset(pg, PgAttr)
            var size: uint = Trunc[uint](PgAttrBytes)
            if InitializeProcThreadAttributeList(attr, count, 0, raw.OfMut(mut size)) == 0 {
                return LastError()
            }
            var ok := true
            if n > 0 {
                ok = UpdateProcThreadAttribute(attr, 0, AttrHandleList, list, Trunc[uint](n * 8), raw.Null[u8](),
                    raw.Null[uint]()) != 0
            }
            if ok && job != 0 {
                ok = UpdateProcThreadAttribute(attr, 0, AttrJobList, jl, 8, raw.Null[u8](), raw.Null[uint]()) != 0
            }
            if ok && pty != 0 {
                ok = UpdateProcThreadAttribute(attr, 0, AttrPseudoConsole, raw.FromAddr[u8](pty), 8, raw.Null[u8](),
                    raw.Null[uint]()) != 0
            }
            if !ok {
                e := LastCall()
                DeleteProcThreadAttributeList(attr)
                return e
            }
            Put64(si, 104, u64(raw.Addr(attr)))
        }
        inherits: i32 = if n > 0 { 1 } else { 0 }
        created := CreateProcessW(app, cmd, raw.Null[u8](), raw.Null[u8](), inherits,
            ExtendedStartupInfo | UnicodeEnvironment, env, dir, si, pi)
        r := if created == 0 { LastCall() } else { Ok(0) }
        if count > 0 {
            DeleteProcThreadAttributeList(attr)
        }
        if created == 0 {
            return r
        }
        _ = CloseHandle(Trunc[uint](Get64(pi, 8)))
        return OsResult(value: Trunc[i64](Get64(pi, 0)), err: core.SysErr.None, os: i64(Get32(pi, 16)))
    }
}

/// ChildSignal sends sig (Linux's number) to the process h, or with group
/// to every process of its job: only 9 (kill) can be delivered, by
/// TerminateJobObject or TerminateProcess (exit code 1); any other signal
/// is Unsupported. A process already ended is success.
/// O(1): one system call; allocates nothing.
export ChildSignal(h: uint, group: bool, pid: i64, job: uint, sig: u32): OsResult {
    if sig != 9 {
        return OsResult(value: 0, err: core.SysErr.Unsupported, os: 0)
    }
    if group && job != 0 {
        unsafe "kernel32; job is the caller's live job handle" {
            if TerminateJobObject(job, 1) == 0 {
                return LastCall()
            }
        }
        return Ok(0)
    }
    return ProcKill(h)
}

/// GroupEnd ends what remains of the job's processes and closes the job
/// (the leader's wait has returned). O(1): two system calls.
export GroupEnd(job: uint): OsResult {
    unsafe "kernel32; job is the caller's job handle, not used after this" {
        var r := Ok(0)
        if TerminateJobObject(job, 1) == 0 {
            r = LastCall()
        }
        _ = CloseHandle(job)
        return r
    }
}

/// GroupClose closes the job handle and leaves its processes running (a
/// detached child; the job has no kill-on-close). O(1).
export GroupClose(job: uint) {
    unsafe "kernel32; job is the caller's job handle, not used after this" {
        _ = CloseHandle(job)
    }
}

/// TryReap is Linux's reaper of detached children: Windows has no zombie
/// to reap, so always true. O(1).
export TryReap(h: uint): bool {
    return true
}

// ConPty is the kernel32 entry named by name, found at run time; null on
// a Windows before 10 1809.
ConPty(name: str): *u8 {
    var k32: [16]u16 = [0; 16]
    _ = Wide("kernel32.dll", mut k32[..])
    unsafe "kernel32; name is a NUL-terminated constant" {
        m := GetModuleHandleW(raw.Data(k32[..]))
        if m == 0 {
            return raw.Null[u8]()
        }
        return GetProcAddress(m, raw.StrData(name))
    }
}

// Coord packs cols and rows as a COORD (X low, Y high), which the x64
// convention passes by value in a register; Other outside 1 to 32,767.
Coord(cols: int, rows: int): i64 {
    if cols < 1 || cols > 32767 || rows < 1 || rows > 32767 {
        return -1
    }
    return i64(cols) | (i64(rows) << 16)
}

// HrFail maps an HRESULT: a Win32 code (facility 7) as MapError does, any
// other failure Other with the HRESULT in os.
HrFail(hr: i32): OsResult {
    u := Trunc[u32](i64(hr))
    if u & 0xFFFF_0000 == 0x8007_0000 {
        return FailCall(u & 0xFFFF)
    }
    return OsResult(value: 0, err: core.SysErr.Other, os: i64(u))
}

/// PtyOpen makes a pseudo-console of cols by rows over two anonymous
/// pipes: value the console (HPCON), w.a the read end of its output, w.b
/// the write end of its input (both synchronous); the console's own ends
/// are closed here, as CreatePseudoConsole duplicated them. Unsupported
/// before Windows 10 1809. O(1) system calls; every handle is closed on
/// every failure.
export PtyOpen(cols: int, rows: int, w: mut core.IoWords): OsResult {
    c := Coord(cols, rows)
    if c < 0 {
        return FailWith(core.SysErr.Other)
    }
    f := ConPty("CreatePseudoConsole\u{0}")
    if raw.IsNull(f) {
        return OsResult(value: 0, err: core.SysErr.Unsupported, os: 0)
    }
    var in_r: uint = 0
    var in_w: uint = 0
    var out_r: uint = 0
    var out_w: uint = 0
    unsafe "kernel32; the entry's type is its documented signature" {
        if CreatePipe(raw.OfMut(mut in_r), raw.OfMut(mut in_w), raw.Null[u8](), 0) == 0 {
            return LastCall()
        }
        if CreatePipe(raw.OfMut(mut out_r), raw.OfMut(mut out_w), raw.Null[u8](), 0) == 0 {
            r := LastCall()
            _ = CloseHandle(in_r)
            _ = CloseHandle(in_w)
            return r
        }
        var fw := u64(raw.Addr(f))
        create := raw.Read(raw.PtrCast[u64, fn(u32, uint, uint, u32, *uint): i32](raw.OfMut(mut fw)))
        var hpc: uint = 0
        hr := create(Trunc[u32](c), in_r, out_w, 0, raw.OfMut(mut hpc))
        _ = CloseHandle(in_r)
        _ = CloseHandle(out_w)
        if hr < 0 {
            _ = CloseHandle(in_w)
            _ = CloseHandle(out_r)
            return HrFail(hr)
        }
        w.a = Trunc[i64](out_r)
        w.b = Trunc[i64](in_w)
        return Ok(Trunc[i64](hpc))
    }
}

/// PtyResize sets the pseudo-console hpc's size. O(1).
export PtyResize(hpc: uint, cols: int, rows: int): OsResult {
    c := Coord(cols, rows)
    if c < 0 {
        return FailWith(core.SysErr.Other)
    }
    f := ConPty("ResizePseudoConsole\u{0}")
    if raw.IsNull(f) {
        return OsResult(value: 0, err: core.SysErr.Unsupported, os: 0)
    }
    unsafe "kernel32; the entry's type is its documented signature" {
        var fw := u64(raw.Addr(f))
        resize := raw.Read(raw.PtrCast[u64, fn(uint, u32): i32](raw.OfMut(mut fw)))
        hr := resize(hpc, Trunc[u32](c))
        return if hr < 0 { HrFail(hr) } else { Ok(0) }
    }
}

/// PtyClose closes the pseudo-console hpc; its child processes are ended
/// by the console. Called after both pipe ends are closed, so it never
/// waits for an unread output pipe. O(1).
export PtyClose(hpc: uint) {
    f := ConPty("ClosePseudoConsole\u{0}")
    if raw.IsNull(f) {
        return
    }
    unsafe "kernel32; the entry's type is its documented signature" {
        var fw := u64(raw.Addr(f))
        close := raw.Read(raw.PtrCast[u64, fn(uint)](raw.OfMut(mut fw)))
        close(hpc)
    }
}

/// TermOpen opens the console of this process: value CONIN$, os CONOUT$
/// (both read-write, shared). No console is NotFound (the OS's 2 or 6).
/// O(1): two system calls; the first is closed when the second fails.
export TermOpen(): OsResult {
    var cin: [8]u16 = [0; 8]
    var cout: [8]u16 = [0; 8]
    _ = Wide("CONIN$", mut cin[..])
    _ = Wide("CONOUT$", mut cout[..])
    unsafe "kernel32; the names are NUL-terminated" {
        i := CreateFileW(raw.Data(cin[..]), GenericRead | GenericWrite, ShareReadWrite, raw.Null[u8](), OpenExisting, 0,
            0)
        if i == InvalidHandle {
            return NoConsole()
        }
        o := CreateFileW(raw.Data(cout[..]), GenericRead | GenericWrite, ShareReadWrite, raw.Null[u8](), OpenExisting,
            0, 0)
        if o == InvalidHandle {
            r := NoConsole()
            _ = CloseHandle(i)
            return r
        }
        return OsResult(value: Trunc[i64](i), err: core.SysErr.None, os: Trunc[i64](o))
    }
}

// NoConsole is the last error, with 2 and 6 (no console to open) NotFound.
NoConsole(): OsResult {
    unsafe "kernel32" {
        e := GetLastError()
        if e == 6 {
            return OsResult(value: 0, err: core.SysErr.NotFound, os: 6)
        }
        return Fail(e)
    }
}

/// TermMode reads the console modes of the input h and the output out into
/// old (input then output, 8 bytes) and sets mode: core.IoTermSaved sets
/// saved back, IoTermRaw turns echo, line and processed input off and
/// virtual-terminal and window input on (a classic console sends resize
/// records only with the latter), and
/// virtual-terminal processing and no-newline-auto-return on for the
/// output; IoTermNoEcho turns echo off and line and processed input on;
/// IoTermGet sets nothing. A console without virtual-terminal input (87)
/// is Unsupported. When the output's set fails the input's mode is put
/// back. value 8. O(1): four system calls at most, two for IoTermGet.
export TermMode(h: uint, out: uint, mode: i64, saved: []u8, old: mut []u8): OsResult {
    if old.Len() < ConModeBytes || mode < 0 || mode > core.IoTermGet ||
        (mode == core.IoTermSaved && saved.Len() < ConModeBytes) {
        return FailWith(core.SysErr.Other)
    }
    var mi: u32 = 0
    var mo: u32 = 0
    unsafe "kernel32; mi and mo are this frame's" {
        if GetConsoleMode(h, raw.OfMut(mut mi)) == 0 || GetConsoleMode(out, raw.OfMut(mut mo)) == 0 {
            return LastCall()
        }
        for k in 0..4 {
            old[k] = Trunc[u8](mi >> (k * 8))
            old[4 + k] = Trunc[u8](mo >> (k * 8))
        }
        if mode == core.IoTermGet {
            return Ok(i64(ConModeBytes))
        }
        var ni := mi
        var no := mo
        if mode == core.IoTermSaved {
            ni = u32(saved[0]) | (u32(saved[1]) << 8) | (u32(saved[2]) << 16) | (u32(saved[3]) << 24)
            no = u32(saved[4]) | (u32(saved[5]) << 8) | (u32(saved[6]) << 16) | (u32(saved[7]) << 24)
        } else if mode == core.IoTermRaw {
            ni = (mi & ~(InProcessed | InLine | InEcho)) | InVt | InWindow
            no = mo | OutProcessed | OutVt | OutNoAutoReturn
        } else {
            ni = (mi & ~InEcho) | InLine | InProcessed
        }
        if SetConsoleMode(h, ni) == 0 {
            e := GetLastError()
            return if e == 87 { OsResult(value: 0, err: core.SysErr.Unsupported, os: 87) } else { Fail(e) }
        }
        if no != mo && SetConsoleMode(out, no) == 0 {
            e := GetLastError()
            _ = SetConsoleMode(h, mi)
            return if e == 87 { OsResult(value: 0, err: core.SysErr.Unsupported, os: 87) } else { Fail(e) }
        }
    }
    return Ok(i64(ConModeBytes))
}

/// TermRecords reads the console input records h holds now, without
/// waiting: out gets whole INPUT_RECORDs (20 bytes each: key events with
/// their UTF-16 unit, which virtual-terminal input makes the escape
/// sequences Linux reads as bytes; window-size events), value their bytes,
/// 0 when none is queued. An out shorter than one record is Other.
/// O(records); allocates nothing.
export TermRecords(h: uint, out: mut []u8): OsResult {
    cap := out.Len() / 20
    if cap < 1 {
        return FailWith(core.SysErr.Other)
    }
    var n: u32 = 0
    unsafe "kernel32; out holds cap records" {
        if GetNumberOfConsoleInputEvents(h, raw.OfMut(mut n)) == 0 {
            return LastCall()
        }
        if n == 0 {
            return Ok(0)
        }
        want := if int(n) < cap { n } else { Trunc[u32](cap) }
        var got: u32 = 0
        if ReadConsoleInputW(h, raw.DataMut(out), want, raw.OfMut(mut got)) == 0 {
            return LastCall()
        }
        return Ok(i64(got) * 20)
    }
}

/// ReapSetOpen, ReapSetAdd and ReapSetWait are Linux's reaper set of
/// detached children: Windows has no zombie to reap, std.rt never calls
/// them here, and each answers Unsupported. O(1); allocates nothing.
export ReapSetOpen(): OsResult {
    return FailWith(core.SysErr.Unsupported)
}

/// ReapSetAdd: see ReapSetOpen. Unsupported.
export ReapSetAdd(set: uint, h: uint, tag: u64, again: bool): OsResult {
    return FailWith(core.SysErr.Unsupported)
}

/// ReapSetWait: see ReapSetOpen. Unsupported.
export ReapSetWait(set: uint, tags: mut []u64): OsResult {
    return FailWith(core.SysErr.Unsupported)
}
