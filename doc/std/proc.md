# std.proc

`std.proc` starts and controls other programs. Run one to the end and keep its output, start one with pipes to talk to it, give it handles, put it on a pseudo-terminal, and stop it. It also serves the program itself. It waits for a signal such as Ctrl+C, ends with an exit code, takes handles its parent passed, and runs copies of itself as supervised workers. It runs on Linux and Windows, and under a simulated `Io` from `std.sim`; the sections below give each difference. For the controlling terminal (raw mode, keys), use [`std.term`](term.md).

Read the [library guide](README.md) first.

```mk
// verdict: parse
import std.proc
```

## Contents

- [The main pieces](#the-main-pieces)
- [Starting a program](#starting-a-program)
- [Children, pipes and waiting](#children-pipes-and-waiting)
- [Process groups](#process-groups)
- [Pseudo-terminals](#pseudo-terminals)
- [Handles for a child](#handles-for-a-child)
- [Signals and exit](#signals-and-exit)
- [Supervised workers](#supervised-workers)
- [Errors](#errors)
- [Cancellation](#cancellation)
- [Memory, threads and cost](#memory-threads-and-cost)
- [Under a simulated Io](#under-a-simulated-io)
- [Examples](#examples): [run a program](#run-a-program), [feed a child and read its output](#feed-a-child-and-read-its-output), [stop a child at a deadline](#stop-a-child-at-a-deadline), [handle Ctrl+C](#handle-ctrlc)
- [API](#api)

## The main pieces

| Task | Use |
|---|---|
| Run a program to the end and keep its output | `Run`, which returns an `Output` |
| Start a program and talk to it | `Spawn` with a `Command` (start from `DefaultCommand`) |
| Choose where its standard streams go | `Command.stdin`, `stdout`, `stderr`: a `Stdio` |
| Set its environment and directory | `Command.env` (an `Env`), `Command.dir` |
| Wait for it, or let it go | `Child.Wait`, which returns a `Status`; `Child.Detach` |
| Stop it | `Child.Signal`, `Child.Kill` |
| Connect programs to each other | `Pipe`, which returns a `PipeReader` and a `PipeWriter` |
| Run it on a terminal | `Pty.Open`, then `Command.terminal` |
| Give it other handles | `Command.handles` (each a `Pass`); the child calls `Inherited` |
| React to Ctrl+C and other signals | `WaitSignal` with a `Signal` |
| End this program with a code | `Exit` |
| Run copies of a server that restart | `Supervise`, `IsWorker` |

Starting, waiting, reading and writing take an `io: Io` ([`Io`](README.md#4-io-and-the-system-handle)). `Supervise` and `IsWorker` take `sys`. `Signal`, `Kill`, `Detach`, the `Close` calls and `Exit` take no `Io`.

See also the cookbook recipes [Run a program and read its output](../cookbook.md#run-a-program-and-read-its-output) and [Talk to a running child, build a pipeline, and kill on timeout](../cookbook.md#talk-to-a-running-child-build-a-pipeline-and-kill-on-timeout). For signals and workers, see [Shut down on a signal](../cookbook.md#shut-down-on-a-signal) and [Restart workers that panic](../cookbook.md#restart-workers-that-panic).

## Starting a program

`Run` and `Spawn` take the program and its arguments as a list of strings, `args`. No shell runs: each argument reaches the program as it is, with no splitting, quoting or wildcards. To run a shell command line, run the shell (`["sh", "-c", line]`, or `["cmd", "/c", line]` on Windows).

- `args[0]` with a separator is a path. A relative path is taken in `Command.dir` when it is given, else in the working directory.
- `args[0]` with no separator is a name, searched in `PATH` only (with `PATHEXT` on Windows). It is never searched in the current directory, not even through an empty or `.` entry of `PATH`. On Windows the path found replaces `args[0]` on the child's command line.
- On Windows the arguments are quoted so that the child's `CommandLineToArgvW` gives back the same list. A `.bat` or `.cmd` program runs through `cmd.exe`. `Spawn` quotes and escapes each argument for `cmd.exe`. It refuses with `Os(0)` an argument that holds a CR or LF, and a script path that holds a quote or ends in a backslash.
- `Command.env` is an `Env`: `Inherit` this process's environment, `Replace` it with a list of `NAME=value` entries, or `Extend` it with such a list, each entry replacing the variable of its name or adding it. With `Replace` nothing is added: on Windows pass `SYSTEMROOT` when the child uses Winsock or CryptoAPI. When a name appears twice, the later entry wins. On Windows names compare ignoring case.
- A NUL byte in an argument, an environment entry or the directory is `Os(0)`. A `Command.dir` that names no directory is `NotFound`.

`Command` is a plain record of settings that `Spawn` reads and never keeps. Start from `DefaultCommand` and assign the fields you change.

## Children, pipes and waiting

`Spawn` returns a `Child` and, for each standard stream whose `Stdio` is `Pipe`, the parent's end of that pipe; the others are `none`. All of them are **linear** values you own: the compiler rejects a path through your code that drops one. End a `Child` with `Wait` or `Detach`, and each pipe end with `Close`. `Run` does all of this for you.

| `Stdio` | The child's stream is |
|---|---|
| `Inherit` | This process's own stream |
| `Null` | The null device |
| `Pipe` | A new pipe; `Spawn` returns the parent's end |
| `File(f)` | A duplicate of an open file's handle |
| `Reader(r)` | A duplicate of a `Pipe` read end (stdin only) |
| `Writer(w)` | A duplicate of a `Pipe` write end (stdout or stderr only) |
| `JoinStdout` | The same stream as stdout (stderr only) |

Any other combination is `Os(0)`.

- Every handle std opens is close-on-exec, so a child gets only what its `Command` gives it. `Spawn` duplicates what it is lent into the child, and the parent keeps its own end. Close a lent write end before reading the read end, or the read never sees the end of input.
- A `Pipe` stream is an OS pipe on both targets, so the child can open it again by name (`/dev/stdin`, `/proc/self/fd/0` on Linux).
- A write to a pipe whose read ends are all closed is `Os(32)` on Linux and `Os(232)` on Windows.
- A pipe made by `proc.Pipe` holds 64 KiB that nobody has read yet on Windows, as a Linux pipe does by default; a `WriteAll` waits for the reader only past that.
- `Wait` reads no pipe. Read each of the child's pipes while it runs, or a child that fills a pipe blocks forever. A child that reads its input to the end waits forever while you keep its `stdin` open.
- `Wait` consumes the child only when it returns a `Status`. On `Canceled` or any other error you still own the child: wait again, `Kill` it and then `Wait`, or `Detach` it.
- `Status.code` is the exit code: 0 to 255 on Linux, the 32-bit exit code on Windows. It is `none` exactly when a signal ended the child (Linux), and `Status.signal` then names the signal when `Signal` has a member for it. A child ended by `Kill` is SIGKILL on Linux, so both fields are `none`; on Windows it exits with code 1.
- `Child.Pid` is for display. `Wait`, `Signal` and `Kill` use the OS handle of the process, so they never reach another process that reuses the number.
- `Detach` lets the child run on. The runtime collects its exit status when it ends, so no zombie is left.

## Process groups

With `Command.group`, the child leads a group of its own: a process group on Linux, a job object without kill-on-close on Windows.

- `Signal` and `Kill` reach every process still in the group.
- `Wait` kills what remains of the group before it returns.
- `Detach` leaves the group running.
- A process that leaves the group is outside it.
- On Linux the group is not the terminal's foreground group, so Ctrl+C at the terminal does not reach the child. Forward it with `Child.Signal`, as the cookbook's [Talk to a running child](../cookbook.md#talk-to-a-running-child-build-a-pipeline-and-kill-on-timeout) does. On Windows a child that shares this process's console gets Ctrl+C itself, grouped or not; a child on a `Pty` has a console of its own.

## Pseudo-terminals

A `Pty` is a pseudo-terminal: `/dev/ptmx` on Linux, a pseudo-console on Windows (from Windows 10 1809; earlier it is `Unsupported`). Setting `Command.terminal` makes it the child's three standard streams and its controlling terminal; the three `Stdio` fields must then be `Inherit`. What the children write is read with `Pty.Read`, and `Pty.WriteAll` is their input. A size outside 1 to 32,767 is `Os(0)`.

- `Pty.Read` returns the end (0) once the children are gone. On Linux that is when no process holds the terminal open. On Windows it is once every child that `Spawn` started on the `Pty` has been waited for, so a detached child keeps it open.
- On Windows the bytes read are the pseudo-console's rendering of the screen, not the child's own bytes. When `Pty.Read` reaches the end, the pseudo-console is closed, and whatever still runs on it ends. A later `Spawn` on the `Pty` gets a new pseudo-console of the `Pty`'s last size, behind the same `Pty`. Unread output of the earlier children is dropped. No task may `Read` or `WriteAll` the `Pty` while that `Spawn` runs. On Linux the terminal takes a new child as it is.

## Handles for a child

A child gets its three standard streams and the handles its `Command.handles` names, and no other handle of its parent. There are at most 64 handles, each a `Pass`: a file, a connected stream, a listener or a pipe end.

- On Linux the handles are the child's descriptors 3, 4 and on, in order. On Windows they are inheritable duplicates on its handle list. A disk file arrives as a new synchronous file object at position 0, with the access the parent's handle had and no more.
- The child's environment variable `MK_HANDLES` lists them, and `proc.Inherited` takes them, once, as `net.Passed` values; a pipe end arrives as a file. No child inherits its parent's `MK_HANDLES`, and a `Command.env` entry that names it is `Os(0)`.
- The runtime makes the handles close-on-exec at start-up, so they reach a grandchild only through its own `Command`.
- Start-up checks the list before the runtime opens a handle of its own. On Linux it must be exactly 3, 4 and on, each open and not close-on-exec. On Windows the values must be distinct multiples of 4, each open, inheritable and not a standard handle. The kernel ignores a handle's low two bits, so another value would name a handle twice. A list that fails makes `Inherited` fail with `Os(22)`, and no handle is taken or touched. Such a list may come from a program that is not an Mk `Spawn`.
- A socket handed over is shared between the processes, as [passing handles](net.md#unix-domain-sockets-and-passing-handles) describes.
- On Windows a connected stream or a Unix-domain listener among the handles is `Unsupported`. A stream's overlapped I/O would complete in the parent, and the child cannot read an AF_UNIX socket's family. A TCP listener works, through the same shared accept that `Supervise`'s workers use.

## Signals and exit

`WaitSignal(io, s)` waits until the process receives `s`. Only a signal that arrives after the call starts counts. Once a program waits for a signal, that signal's default action is gone for the rest of the process. From then on, Ctrl+C does not end a program that waits for `Interrupt`.

| `Signal` | Linux | Windows |
|---|---|---|
| `Interrupt` | SIGINT | Ctrl+C |
| `Terminate` | SIGTERM | Ctrl+Break, console close, logoff, shutdown |
| `Hangup` | SIGHUP | never arrives |

`Child.Signal` sends a signal to a child on Linux. Windows can send no signal but a kill to another process, so there it fails with `Unsupported`.

`Exit(code)` flushes the standard streams and ends the process at once. Other tasks are not joined, and no `defer` runs. Returning from `Main` is the usual way to end; `Exit` is for a specific exit code.

## Supervised workers

`Supervise(sys, workers, &l, worker)` runs `workers` copies of this program that share the listener `l`, and keeps them running. It starts each copy as a new process, never forking a process whose tasks are running.

- A worker runs `Main` from the start, so the code before `Supervise` runs again in every worker; keep it to setup that can repeat. `IsWorker(sys)` is true in a worker and false in the parent, for code that belongs to the parent only.
- In a worker, `net.Listen` returns the inherited listener ([streams and listeners](net.md#streams-and-listeners)), and `Supervise` runs `worker` once and returns its result.
- In the parent, a worker that exits for any reason, a panic included, is started again, with a backoff after its third restart within a minute. On cancellation `Supervise` returns `Canceled` once every worker has exited.
- Workers inherit the parent's standard streams, so their output and panic reports reach them. What a worker has buffered and not written is lost when it is stopped.

## Errors

Every function fails with members of one error set, `proc.Error`. The doc text of each function says which members it returns and when.

| Member | Meaning |
|---|---|
| `NotFound` | No program at `args[0]`, `args` or `args[0]` empty, or `Command.dir` names no directory |
| `Denied` | The OS refused to start the program |
| `TooLarge` | `Run`'s output passed its limit |
| `Os(code)` | Any other OS failure, with the OS's code; 0 for an argument the call refuses (see each function) |
| `Unsupported` | This target or this `Io` cannot do it |
| `Canceled` | The call's scope was cancelled (from the prelude) |
| `OutOfMemory` | The memory for the call could not be had (from the prelude) |

`proc.Error` is open: it may gain members ([errors](README.md#5-errors)).

When a function of yours that takes a `Child` or pipe end by value fails, the caller still owns the value ([fallible calls](../reference.md#fallible-calls-consume-only-on-success)). The deadline example below relies on this.

## Cancellation

A call that can wait has `Canceled` in its error set and fails with it when its task's scope is cancelled.

- `Child.Wait`, `WaitSignal`, `PipeReader.Read` and `Pty.Read` consume nothing when cancelled: call them again.
- `Spawn` is cancelled only before the child starts, with nothing started. A child that has started is always returned.
- `Supervise`'s parent returns `Canceled` once every worker has exited.
- `Run` is cancelled part way: a prefix of its input was fed, and the child ran in part before the cancel killed and reaped it.
- `PipeWriter.WriteAll` and `Pty.WriteAll` may have written a prefix of the data.
- `Close`, `Kill`, `Signal` and `Detach` are not cancellation points.

See [cancellation](README.md#6-cancellation) in the library guide.

## Memory, threads and cost

- `Run` returns the output in the arena you pass (`from a`). While the child runs, each stream also holds up to the limit plus one byte in arenas on the process heap, freed before `Run` returns.
- `Spawn` allocates nothing in `a` on Linux. On Windows, for a name searched in `PATH` or a relative path taken in `Command.dir`, it takes a sub-arena of `a`. The sub-arena holds the argument list and the candidate path, and is freed before `Spawn` returns. The records of a child, a pipe end and a `Pty` come from the process heap; when they cannot, the call is `OutOfMemory`.
- Waiting for a child, or on a pipe end that `Spawn` returned, holds no thread on either target: both wait on the runtime's poller. A wait on an end made by `proc.Pipe` holds no thread on Linux, and an end lent to a child is given to it in blocking mode. On Windows, and for a `Pty`, such a wait holds a thread of its own, beyond the 64 that run other blocking calls. The runtime keeps at most 1,024 threads in all and keeps each one it starts. Past about that many waits, less the threads other blocking calls hold, further blocking calls queue behind them.

## Under a simulated Io

Under a `Sim`'s `Io`, a spawned child runs no program, so a test of code that starts programs runs the same on every target:

- The child copies its standard input to its standard output at the `Sim`'s pace; a `Pty` child echoes what is written to the `Pty`.
- It exits with code 0 when its input ends, and at once on `Kill`, reported as the target reports a kill.
- Its pipes are `Sim` streams of 64 KiB that end when it ends. `Pipe` gives a `Sim` stream pair, and a stream lent to a child reads as `Null`.
- `Spawn` ignores the arguments, environment, directory and handles, except that a program named `missing` is `NotFound`.
- `WaitSignal` never sees a signal. It returns `Canceled` when its scope is cancelled or its deadline passes, or when the run has nothing else to do.
- `Inherited` returns 0. A process, pipe or terminal under a `Recorder` or `Replayer` is `Unsupported`.

## Examples

The first three programs run as written with `mk run`.

### Run a program

```mk
// verdict: ok
import std.proc

// Shell is the argument list that runs line in the system's shell.
Shell(line: str): [3]str {
    if Target.os == Os.Windows {
        return ["cmd", "/c", line]
    }
    return ["sh", "-c", line]
}

export Main(sys: &System): proc.Error|WriteError!void {
    args := Shell("echo hello&& exit 3")
    out := try proc.Run(sys.arena, sys.io, args[..], b"", 1 << 16) // at most 64 KiB of output
    try sys.Print("exit code {code}, {n} bytes on stderr\n", (code: out.code, n: out.stderr.Len()))
    var w := sys.Stdout()
    try w.Bytes(out.stdout) // the child's output, in sys.arena
}
```

```text
$ mk run run.mk
exit code 3, 0 bytes on stderr
hello
```

- `Run` uses this process's environment and working directory, and captures stderr too. To set them, use `Spawn` with a `Command`.
- An empty input (`b""`) gives the child the null device as standard input.
- Output past the limit, both streams together, kills the child and fails with `TooLarge`. On every failure after the start, the child has exited before `Run` returns.
- A cancel kills the child at once, with no gentler signal first.

### Feed a child and read its output

```mk
// verdict: ok
import std.proc

// Sort feeds input to the system's sort program and copies its output to
// out, without carriage returns.
Sort(a: &Arena, io: Io, input: []u8, out: mut Writer): proc.Error|WriteError!proc.Status {
    var c := proc.DefaultCommand
    args := ["sort"]
    c.args = args[..]
    c.stdin = proc.Stdio.Pipe
    c.stdout = proc.Stdio.Pipe
    (child, stdin, stdout, stderr) := try proc.Spawn(a, io, &c)
    onerr child.Detach() // let it go if a later line fails
    if e := stderr { // none here: stderr is inherited, not a pipe
        e.Close()
    }
    var w := stdin.Must() // the pipe ends are ours to close
    var r := stdout.Must()
    onerr r.Close()
    w.WriteAll(io, input) else e {
        w.Close()
        fail e
    }
    w.Close() // end of input: sort starts writing
    var buf: [512]u8 = [0; 512]
    for {
        n := try r.Read(io, mut buf[..]) // 0 at the end
        if n == 0 {
            break
        }
        var from := 0
        for i in 0..n {
            if buf[i] == 13 { // a carriage return, which sort writes on Windows
                try out.Bytes(buf[from..i])
                from = i + 1
            }
        }
        try out.Bytes(buf[from..n])
    }
    r.Close()
    return try child.Wait(io) // consumes the child
}

export Main(sys: &System): proc.Error|WriteError!void {
    var out := sys.Stdout()
    st := try Sort(sys.arena, sys.io, b"pears\napples\nplums\n", mut out)
    if code := st.code { // none when a signal ended the child (Linux)
        try sys.Print("sort exited with code {code}\n", (code,))
    }
}
```

```text
$ mk run sort.mk
apples
pears
plums
sort exited with code 0
```

- The input here fits in the pipe, so one task can write it all and then read. For a large input, write from another task while this one reads, as the cookbook's [Talk to a running child](../cookbook.md#talk-to-a-running-child-build-a-pipeline-and-kill-on-timeout) does.
- `onerr` lines run only when a later line fails, so on success each value is ended once, by `Close` and `Wait`.

### Stop a child at a deadline

```mk
// verdict: ok
import std.proc
import std.time

// WaitOrKill waits for child for at most limit, then kills it. It reports
// whether the child was killed.
WaitOrKill(io: Io, child: proc.Child, limit: time.Duration): proc.Error!bool {
    scope s := io.Scope() {
        s.CancelAfter(limit)
        _ = child.Wait(io) else e { // on any error we still own child
            if !s.Expired() {
                fail e // a failed call leaves child with the caller
            }
            try child.Kill()
            _ = try child.Wait(io.NoCancel()) // the kill ends it at once
            return true
        }
        return false
    }
}

export Main(sys: &System): proc.Error|WriteError!void {
    var c := proc.DefaultCommand
    args := ["sort"]
    c.args = args[..]
    c.stdin = proc.Stdio.Pipe // sort reads until its input ends
    (child, stdin, stdout, stderr) := try proc.Spawn(sys.arena, sys.io, &c)
    onerr child.Detach() // if a later line fails, let the child run on
    if r := stdout { // none: only a Pipe stream returns an end
        r.Close()
    }
    if r := stderr {
        r.Close()
    }
    w := stdin.Must()
    onerr w.Close()
    killed := try WaitOrKill(sys.io, child, time.Millis(200)) // stdin is still open
    w.Close()
    try sys.Print("killed at the deadline: {killed}\n", (killed,))
}
```

```text
$ mk run deadline.mk
killed at the deadline: true
```

- `sort` waits for the end of its input, which never comes while `stdin` is open, so the deadline cuts `Wait` with `Canceled`.
- `io.NoCancel()` lets the last `Wait` run in a scope that is already cancelled.
- Set `c.group = true` when the child starts children of its own, so `Kill` reaches them too.

### Handle Ctrl+C

```mk
// verdict: ok
import std.proc

// Main counts Ctrl+C presses; the third one ends the program with code 130.
export Main(sys: &System): Canceled|WriteError!void {
    for n in 1..4 {
        try proc.WaitSignal(sys.io, proc.Signal.Interrupt) // from here on, Ctrl+C does not end the process
        try sys.Print("interrupt {n}\n", (n,))
    }
    try sys.Print("bye\n", ())
    proc.Exit(130) // flushes stdout and stderr, then ends at once
}
```

This program waits for keys pressed at a terminal, so it has no output to show here. Pressing Ctrl+C three times prints `interrupt 1` to `interrupt 3` and `bye`.

## API

Generated from the module's doc comments by `mk doc --std std.proc` ([reading the API](README.md#reading-an-api-section)).

<!-- api:begin -->
Stability: Stable.

### `Child`

```
export linear struct Child
```

A running child process, and its process group when it was started
with Command.group. Linear: end it with Wait or Detach. Send and Share,
so one task can Signal it while another reads its pipes.

### `Child.Detach`

```
export Child.Detach(self)
```

Gives the child up without waiting for it. The child and its group run
on, and the runtime collects the child's exit status when it ends, so
no zombie process is left behind. Infallible and not cancellable.

O(1). On Linux the first Detach in a process starts one thread, with
a 1 MiB stack reservation, and one epoll set, kept until exit, that
collect detached children as they end; if the thread cannot start, a
child is collected at the next Spawn or Detach. On Windows nothing
remains once the child's handles are closed.

### `Child.Kill`

```
export Child.Kill(&self): Error!void
```

- Errors: Error (Canceled, OutOfMemory, Denied, NotFound, Os(code: i64), TooLarge, Unsupported)

Ends the child at once, or, with Command.group, every process still in
its group. Killing a child that has already ended succeeds; any failure
is the OS's. Call Wait afterwards: the Child is still owned.

On Linux this is SIGKILL. On Windows it is TerminateProcess with exit
code 1, or the end of the group's job object. Not a cancellation point;
O(1); allocates nothing.

### `Child.Pid`

```
export Child.Pid(&self): i64
```

The child's OS process id, for display and for other programs. Wait,
Signal and Kill use the OS handle of the process, not this number, so
they never reach another process that reuses the id. O(1); allocates
nothing.

### `Child.Signal`

```
export Child.Signal(&self, s: Signal): Error!void
```

- Errors: Error (Canceled, OutOfMemory, Denied, NotFound, Os(code: i64), TooLarge, Unsupported)

Sends s to the child, or, with Command.group, to every process still in
its group (a process that left the group is not reached). Sending to a
child that has already ended succeeds.

Unsupported on Windows, which can send no signal but a kill to another
process; any other failure is the OS's. Not a cancellation point; O(1);
allocates nothing.

### `Child.Wait`

```
export Child.Wait(self, io: Io): Error!Status
```

- Errors: Error (Canceled, OutOfMemory, Denied, NotFound, Os(code: i64), TooLarge, Unsupported)

Waits until the child ends, and returns how it ended. When the child
leads a group, every process still in the group is killed before Wait
returns. The kill runs while the leader is not yet reaped, so it cannot
reach another group that reuses the id.

On success the child is consumed. On Canceled, or any other error, the
caller still owns it, running and not yet waited for: call Wait again,
Kill it and then Wait, or Detach it.

Wait reads no pipe. Read each of the child's pipes in a task before or
while waiting, or a child that fills a pipe blocks forever; a child
that reads its input to the end waits forever while its stdin is open.

Waiting holds no thread. O(1) besides the wait; allocates nothing.

### `Command`

```
export struct Command
```

How Spawn starts a child: the program and its arguments, environment,
directory, standard streams and other handles. Start from
DefaultCommand and assign the fields to change. Plain: it holds
references and slices, which Spawn reads and never keeps.

Fields:

- `args: []str`: The program, args[0], and its arguments. A name with no separator is
  searched in this process's PATH only, never in the current
  directory (with PATHEXT on Windows).
- `env: Env`: The child's environment; a name given twice takes the later entry.
- `dir: ?str`: The child's working directory; none (or empty) keeps this process's.
- `stdin: Stdio`: Where stdin, stdout and stderr go.
- `stdout: Stdio`: Where the child's standard output goes.
- `stderr: Stdio`: Where the child's standard error goes.
- `group: bool`: Start the child as the leader of its own group: a process group on
  Linux, a job object without kill-on-close on Windows. Signal and
  Kill reach every process still in the group, and Wait kills what
  remains of it when the leader ends.
- `terminal: ?&Pty`: A pseudo-terminal that becomes the child's three standard streams
  and its controlling terminal; the three streams must then be
  Inherit. Provisional.
- `handles: []Pass`: The handles the child gets beside its three streams, at most 64, in
  order; it gets no other handle of this process. The child takes them
  with proc.Inherited.

### `DefaultCommand`

```
export const DefaultCommand: Command
```

No arguments, the environment inherited, this process's directory, the
three streams inherited, no group, no terminal, no other handle.

### `Denied`

```
export error Denied
```

The OS refused to start the program.

### `Env`

```
export enum Env
```

The environment a child gets.

Variants:

- `Inherit`: This process's environment.
- `Replace(vars: []str)`: Exactly vars, each NAME=value. Nothing is added: on Windows, pass
  SYSTEMROOT when the child uses Winsock or CryptoAPI, which need it.
- `Extend(vars: []str)`: This process's environment with vars (each NAME=value) applied: each
  replaces the variable of its name, or is added. On Windows names
  compare ignoring case.

### `Error`

```
export open type Error = NotFound | Denied | TooLarge | Os | Canceled | OutOfMemory | Unsupported
```

Every way a std.proc operation fails.

### `Exit`

```
export Exit(code: int): never
```

Flushes the standard streams, then ends the process at once with exit
code code. Other tasks are not joined, and nothing else runs.

A code outside i32 panics with code 5, since the OS keeps a 32-bit
status and would report a different code; the panic still flushes the
streams. O(bytes buffered in the streams); allocates nothing.

### `Inherited`

```
export Inherited(a: &Alloc, io: Io, into: mut HeapList[net.Passed]): Error!int
```

- Errors: Error (Canceled, OutOfMemory, Denied, NotFound, Os(code: i64), TooLarge, Unsupported)

Takes the handles this process's parent passed in Command.handles,
pushes them onto into in the order the parent named them, and returns
how many. Each is typed by what the OS says it is (a net.Passed; a pipe
end arrives as a File).

Only the first call in a process takes them. Every later call, a
process that was passed nothing (no MK_HANDLES in its environment), and
a call under a simulated Io return 0. The handles are already
close-on-exec (not inheritable on Windows) when the program starts, so
a program this process starts gets none of them unless its own Command
names them.

Errors:
- Os(code: 22) when MK_HANDLES fails the check made at start-up, before
  the runtime opens any handle of its own. On Linux the list must be
  exactly 3, 4 and on, each open and not close-on-exec; on Windows the
  values must be distinct, open, inheritable and not a standard handle;
  on both, none may be one MK_INHERIT names. So an MK_HANDLES passed on
  by a program that is not an Mk Spawn, without its handles, takes
  nothing and touches no handle.
- Os(code) for a value that names no open handle (EBADF, 9, on Linux;
  ERROR_INVALID_HANDLE, 6, on Windows).
- Unsupported for a socket that is not a stream socket (on Windows, any
  socket but a listener), or a handle the OS cannot type.
- OutOfMemory when into cannot grow, or the process heap cannot give a
  handle's record.

On an error, the handles already taken are closed and the rest are left
as they are. O(handles) plus a scan of the environment; allocates a
runtime record per handle and what into's Push takes from a.

### `IsWorker`

```
export IsWorker(sys: &System): bool
```

Whether this process is a worker that Supervise started. A program
uses it to run the code before its Supervise call in the parent only.
Read from the environment as the process started. O(environment size);
allocates nothing.

### `NotFound`

```
export error NotFound
```

The program or its directory was not found: no program at args[0],
args or args[0] is empty, or dir names no directory (Command.dir is
missing or not a directory).

### `Os`

```
export error Os(code: i64)
```

Any other OS failure, with the OS's error code. The code is 0 for an
argument out of range: a NUL in an argument, an environment entry or the
directory; a disposition a stream cannot take; a batch-file argument
that cmd.exe cannot be given safely. It is also 0 when the runtime
reports an impossible result.

### `Output`

```
export struct Output
```

What a child wrote and how it ended. code is the exit code: 0 to 255 on
Linux, or minus the signal when a signal ended the child; on Windows,
the exit code as a u32. stdout and stderr hold everything the child
wrote to each stream.

Fields:

- `code: int`: The exit code: 0 to 255 on Linux, or minus the signal that ended the
  child; on Windows the exit code as a u32.
- `stdout: []u8`: Everything the child wrote to standard output.
- `stderr: []u8`: Everything the child wrote to standard error.

### `Pass`

```
export open enum Pass
```

A handle a Command lends its child beside the three standard streams:
Spawn duplicates it into the child, and the parent keeps its own. The
child takes it with proc.Inherited, as a net.Passed of the kind the OS
says it is; a pipe end arrives as a File. Plain.

Variants:

- `File(f: &fs.File)`: An open file's handle.
- `Stream(s: &net.Stream)`: A connected stream, TCP or Unix-domain. Unsupported on Windows, where
  the socket's I/O would still complete in this process.
- `Listener(l: &net.Listener)`: A listener; the child accepts on it beside this process. A
  Unix-domain listener is Unsupported on Windows.
- `Reader(r: &PipeReader)`: The read end of a pipe made by proc.Pipe.
- `Writer(w: &PipeWriter)`: The write end of a pipe made by proc.Pipe.

### `Pipe`

```
export Pipe(io: Io): Error!(r: PipeReader, w: PipeWriter)
```

- Errors: Error (Canceled, OutOfMemory, Denied, NotFound, Os(code: i64), TooLarge, Unsupported)

A new pipe: what is written to w is read from r, in order. No child
inherits either end unless a Command lends it (Stdio.Reader,
Stdio.Writer, Pass.Reader, Pass.Writer).

OutOfMemory when the process heap cannot give a handle record;
otherwise the OS's error as its proc.Error member (reaching the
process's handle limit is Os). Not a cancellation point. O(1);
allocates two handle records from the process heap, nothing in an
arena.

### `PipeReader`

```
export linear struct PipeReader
```

The read end of a pipe. Read from it, or wrap it in a Reader, then
Close it. Linear. Send and Share: two tasks may read one end, each read
taking what is there. Owns one OS handle and no memory.

### `PipeReader.Close`

```
export PipeReader.Close(self)
```

Closes this end: once every read end is closed, a write to the pipe
fails. Infallible and not cancellable. O(1); allocates nothing.

### `PipeReader.Read`

```
export PipeReader.Read(&self, io: Io, buf: mut []u8): Error!int
```

- Errors: Error (Canceled, OutOfMemory, Denied, NotFound, Os(code: i64), TooLarge, Unsupported)

Reads what the pipe holds into buf, waiting until there is something,
and returns the number of bytes read: at least 1, or 0 at the end of
input (once every write end, the parent's and each child's, is closed)
and for an empty buf. One read, which may fill less than buf. O(n);
allocates nothing.

Canceled when the task's scope is cancelled; nothing is consumed, so
the same call again reads every byte. Os(0) when the runtime reports a
count outside buf.

A waiting Read holds no thread on Linux, nor on Windows for an end
Spawn returned. On Windows an end made by Pipe holds a thread of its
own, outside the 64 that run blocking calls, and up to about
1,024 may wait at once, less the threads other blocking calls hold;
past that, the process's other blocking calls queue behind them.

### `PipeReader.Reader`

```
export PipeReader.Reader(&self, io: Io, buf: mut []u8): Reader from self, buf
```

- Flows: The result borrows from self and buf.

A Reader for this end that buffers in buf. Each fill is one Read, and
any error becomes the Reader's ReadError. Bytes it buffered that the
caller did not read are lost when it is dropped. O(1); allocates
nothing.

### `PipeWriter`

```
export linear struct PipeWriter
```

The write end of a pipe. Write to it with WriteAll, or wrap it in a
Writer, then Close it; the reader sees the close as the end of input.
Linear. Send and Share: two tasks' WriteAlls on one end never
interleave, since each holds the end's write lock for all of its data,
as net.Conn does. Owns one OS handle and no memory.

### `PipeWriter.Close`

```
export PipeWriter.Close(self)
```

Closes this end: once every write end is closed, a Read of the pipe
returns 0. Infallible and not cancellable. O(1); allocates nothing.

### `PipeWriter.WriteAll`

```
export PipeWriter.WriteAll(&self, io: Io, data: []u8): Error!void
```

- Errors: Error (Canceled, OutOfMemory, Denied, NotFound, Os(code: i64), TooLarge, Unsupported)

Writes every byte of data, waiting while the pipe is full. It holds the
end's write lock throughout, so another task's WriteAll on the same end
comes wholly before or after it.

Once every read end is closed: Os(32) on Linux (EPIPE), Os(232) on
Windows (ERROR_NO_DATA). Canceled when the task's scope is cancelled,
either while waiting for the lock, with nothing written, or after an
unknown prefix of data reached the pipe. Os(0) when a write makes no
progress or the runtime reports a count past data.

O(n) plus the wait for the lock; allocates nothing. A waiting write
holds threads as a waiting Read does.

### `PipeWriter.Writer`

```
export PipeWriter.Writer(&self, io: Io, buf: mut []u8): Writer from self, buf
```

- Flows: The result borrows from self and buf.

A Writer for this end that buffers in buf. Each flush of the buffer is
one WriteAll, so another task's WriteAll falls between two flushes,
never inside one. Any error becomes the Writer's WriteError. Flush it
before dropping it. O(1); allocates nothing.

### `Pty`

```
export linear struct Pty
```

The controlling side of a pseudo-terminal: what children on the
terminal write is read here, and what is written here is their input.
Give it to a child with Command.terminal. Linear: Close it. Send and
Share: two tasks' WriteAlls never interleave, since each holds the
Pty's write lock for all of its data. Owns OS handles and no memory.

On Windows, a Spawn on the Pty after every earlier child was waited for
opens a new pseudo-console and closes the old handles: no task may Read
or WriteAll the Pty while such a Spawn runs.

Provisional.

### `Pty.Close`

```
export Pty.Close(self)
```

Closes the terminal. On Linux the processes still on it see a hang-up;
on Windows the pseudo-console is closed, which ends them. Infallible
and not cancellable. O(1); allocates nothing and frees the handle
records.

On Windows the pseudo-console's close runs on a thread of its own, so
Close does not wait for a free thread among those that run blocking
calls; only when that thread cannot start does the close wait for one.

### `Pty.Open`

```
export Pty.Open(io: Io, cols: int, rows: int): Error!Pty
```

- Errors: Error (Canceled, OutOfMemory, Denied, NotFound, Os(code: i64), TooLarge, Unsupported)

A new pseudo-terminal of cols columns by rows rows.

Os(0) for a size outside 1 to 32,767. Unsupported before Windows 10
1809 and under a Recorder or Replayer. OutOfMemory when the process
heap cannot give a handle record; the OS's error otherwise. Not a
cancellation point. O(1); allocates one or two handle records from the
process heap.

### `Pty.Read`

```
export Pty.Read(&self, io: Io, buf: mut []u8): Error!int
```

- Errors: Error (Canceled, OutOfMemory, Denied, NotFound, Os(code: i64), TooLarge, Unsupported)

Reads what the children on this terminal wrote into buf, waiting until
there is something, and returns the number of bytes read: at least 1,
or 0 at the end. One read, which may fill less than buf. O(n);
allocates nothing.

On Linux the end comes once every child has closed its side of the
terminal. On Windows it comes once every child started on this terminal
has been waited for and its last output read, so a child that is
detached or never waited for holds the end off until Close.

Canceled when the task's scope is cancelled; nothing is consumed, so
the same call again reads every byte.

On Windows the bytes are the pseudo-console's rendering of the screen,
not the child's own bytes, and no Read may run beside a Spawn that
opens a new pseudo-console (see Pty). A waiting Read holds a thread of
its own, outside the 64 that run blocking calls; waiting Reads hold up
the process's other blocking calls only past about 1,024 threads in
all.

### `Pty.Resize`

```
export Pty.Resize(&self, io: Io, cols: int, rows: int): Error!void
```

- Errors: Error (Canceled, OutOfMemory, Denied, NotFound, Os(code: i64), TooLarge, Unsupported)

Sets the terminal's size. A child on it sees SIGWINCH on Linux and a
resize event on Windows. Os(0) for a size outside 1 to 32,767. Not a
cancellation point; O(1); allocates nothing.

### `Pty.WriteAll`

```
export Pty.WriteAll(&self, io: Io, data: []u8): Error!void
```

- Errors: Error (Canceled, OutOfMemory, Denied, NotFound, Os(code: i64), TooLarge, Unsupported)

Writes every byte of data as the terminal's input. It holds the Pty's
write lock throughout, so another task's WriteAll comes wholly before
or after it.

Canceled when the task's scope is cancelled, either while waiting for
the lock, with nothing written, or after an unknown prefix of data was
written. Os(0) when a write makes no progress; the OS's error
otherwise. O(n) plus the wait for the lock; allocates nothing. A
waiting write holds a thread as a waiting Read does.

### `Run`

```
export Run(a: &Arena, io: Io, args: []str, input: []u8, limit: int): Error!Output from a
```

- Flows: The result borrows from a.
- Errors: Error (Canceled, OutOfMemory, Denied, NotFound, Os(code: i64), TooLarge, Unsupported)

Runs a program to the end: args[0] with the arguments args[1..], with
input written to its standard input, which is then closed. The child
gets this process's environment and working directory. Once the child
has exited, returns
its exit code and everything it wrote to standard output and standard
error. args[0] is a path, or a name searched in PATH. An empty input
gives the child the null device as standard input.

Errors: NotFound for an empty args or args[0]; TooLarge when the output
passes limit bytes, both streams together, which kills the child at
once; otherwise Spawn's errors and the OS's. A negative limit panics
(code 8). On Canceled, TooLarge or any other failure after the start,
the child is killed and waited for before Run returns. Under
io.NoCancel(), a failure still ends Run once the dead child's
pipes close; only a grandchild that holds one open can delay it.

O(output) time; allocates the output in a, copying each byte once.
While the child runs, each stream also holds up to limit + 1 bytes in
arenas on the process heap, not from a, freed before Run returns. On
Windows a name searched in PATH takes Spawn's search memory as well
(see Spawn), freed once the child has started.

### `Signal`

```
export open enum Signal
```

A signal a program can wait for with WaitSignal, or send to a child
with Child.Signal.

On Windows, Interrupt is Ctrl+C, Terminate is Ctrl+Break, a console
close, a logoff or a shutdown, and Hangup never arrives. Windows cannot
send any of them to a child: Child.Signal fails with Unsupported.

Variants:

- `Interrupt`: SIGINT on Linux; Ctrl+C on Windows.
- `Terminate`: SIGTERM on Linux; Ctrl+Break, a console close, a logoff or a shutdown on
  Windows.
- `Hangup`: SIGHUP on Linux; it never arrives on Windows.

### `Spawn`

```
export Spawn(a: &Arena, io: Io, c: &Command): Error!(child: Child, stdin: ?PipeWriter, stdout: ?PipeReader, stderr: ?PipeReader)
```

- Errors: Error (Canceled, OutOfMemory, Denied, NotFound, Os(code: i64), TooLarge, Unsupported)

Starts the program c.args[0] with the arguments c.args, set up as c
says. Returns the Child, and the parent's end of each standard stream
whose Stdio is Pipe (none for the others); all are the caller's linear
values. No shell runs: the arguments reach the program as a list.

A file or pipe end lent through c is duplicated into the child, and the
parent keeps its own: close a lent write end before reading the read
end, or the read never sees the end of input. The child gets its three
standard streams and c.handles, and no other handle of this process. On
Linux the handles are its descriptors 3, 4 and on; on Windows they are
inheritable duplicates, a disk file becoming a new synchronous file
object at position 0. A socket among them is shared from then on: closing either
process's copy releases only that copy, with no shutdown, and the peer
sees the end when the last copy closes.

A relative path in args[0] that contains a separator is taken in c.dir
when c.dir is given, on both targets.

Errors:
- NotFound when args or args[0] is empty, no program is found, or c.dir
  names no directory.
- Denied when the OS refuses to start the program.
- Os(0) for a NUL in an argument (args[0] included), an environment
  entry or the directory; an environment entry MK_HANDLES=, which only
  Spawn writes; a Stdio its stream cannot take; a stream that is not
  Inherit beside a terminal; more than 64 handles; a handle with no OS
  handle behind it (a simulated Io's); a pipe end that another Spawn
  returned, as a Stdio or a handle (lend the ends of a proc.Pipe
  instead); a .bat or .cmd program with an argument cmd.exe cannot be
  given safely (a CR or LF).
- Unsupported under a Recorder or Replayer, and on Windows for a
  Pass.Stream or a Unix-domain Pass.Listener.
- Canceled when the task's scope is cancelled before the child starts,
  also while the start waits for one of the 64 threads that run
  blocking calls (a waiting Read of a proc.Pipe end holds none of
  them); a deadline cuts that wait too.
- OutOfMemory when the memory for the call cannot be had, or the
  process heap cannot hold the record of a started child, which is then
  killed and waited for.
- Any other failure is the OS's. On Linux that includes the error of
  opening a lent proc.Pipe end again through /proc/self/fd (ENOENT
  after a chroot, EACCES after a uid change, EMFILE).

Apart from that OutOfMemory, a child that has started is always
returned: a cancel does not separate it from its caller.

O(bytes of args, the environment and PATH), plus on Windows one stat
per PATH entry and extension and one read of the current directory for
a drive-relative args[0] beside a dir with no drive, and one stat of
dir after a failure when dir is given. It allocates nothing in a on
Linux. On Windows, for a name searched in PATH or a relative path taken
in dir, it takes a sub-arena of a (its first chunk from a's heap),
freed before Spawn returns, holding
the argument list and either one candidate path (the longest PATH
entry, the name and the longest PATHEXT extension, plus 2 bytes) or dir
and args[0] plus 1 byte and, for a drive-relative args[0], the current
directory's path. The records of the child and its pipe ends come from
the process heap.

Under a simulated Io the child is simulated: it runs no program, copies
its stdin to its stdout, and exits with code 0 when its input ends. The
arguments, environment, directory and handles are ignored, except that
a program named "missing" is NotFound.

### `Status`

```
export struct Status
```

How a child ended.

code is the exit code: 0 to 255 on Linux, the 32-bit exit code on
Windows (1 after a Kill). It is none exactly when a signal ended the
child on Linux. signal is that signal when Signal has a member for it;
SIGKILL has none, so a Linux child ended by Kill has both fields none.

Fields:

- `code: ?int`: The exit code; none exactly when a signal ended the child on Linux.
- `signal: ?Signal`: The signal that ended the child, when Signal has a member for it.

### `Stdio`

```
export enum Stdio
```

Where one of a child's standard streams goes.

Variants:

- `Inherit`: The parent's own stream.
- `Null`: The null device.
- `Pipe`: A new pipe, whose parent end Spawn returns. The child's end is a
  pipe on both targets; on Linux the child may open it again by name
  (/dev/stdin, /dev/stdout, /proc/self/fd/N).
- `File(f: &fs.File)`: A duplicate of an open file's handle. The child reads or writes at
  the OS's position of the file, which std.fs's own calls never move:
  the start for a file std.fs opened, and the end at each write for
  one opened with Append.
- `Reader(r: &PipeReader)`: A duplicate of the read end of a pipe made by proc.Pipe; stdin only.
- `Writer(w: &PipeWriter)`: A duplicate of the write end of a pipe made by proc.Pipe; stdout or
  stderr only.
- `JoinStdout`: stderr only: the same stream as stdout.

### `Supervise`

```
export Supervise[E](sys: &System, workers: int, l: &net.Listener, const worker: fn(&System, &net.Listener): E!void): E|Error!void
```

- Errors: E|Error (E, Canceled, OutOfMemory, Denied, NotFound, Os(code: i64), TooLarge, Unsupported)

Runs worker in workers processes that share the listener l, and keeps
them running. This program is started again as each worker process,
with l inherited. In a worker, Supervise runs worker once and returns
its result, and net.Listen returns the inherited listener.

In the parent, a worker that exits for any reason, a panic included, is
started again, with a backoff after its third restart within a minute.
The parent returns Canceled once it is cancelled and every worker has
exited, or the error of the first start. workers < 1 panics (code 8).

The parent's cost is O(workers) runtime calls per round of restarts,
plus the backoff's sleeps; it allocates its argument list in
sys.arena.

### `TooLarge`

```
export error TooLarge
```

A child's output passed the limit given to Run.

### `Unsupported`

```
export error Unsupported
```

Something this target or this Io cannot do: a signal Windows cannot
deliver, a pseudo-console before Windows 10 1809, or a process, pipe or
terminal under a Recorder or Replayer.

### `WaitSignal`

```
export WaitSignal(io: Io, s: Signal): Canceled!void
```

- Errors: Canceled

Waits until the process receives s. Only a signal that arrives after
the call starts counts; an earlier one is not seen.

The first wait for s turns off s's default action for the rest of the
process: a program that waits for Interrupt is not ended by Ctrl+C
from then on.

Canceled when the task's scope is cancelled. Panics with code 8 when
io cannot watch s at all. One runtime call; allocates nothing.

<!-- api:end -->
