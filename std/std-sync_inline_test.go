// Why: Mutex.Lock and MutexUnlockWord take their slow paths' first step
// inline, so a lock word with W set must not send a Lock or an Unlock
// through MutexLockSlow or MutexUnlockSlow, and the cancellation fast test
// must stay inline.
// What: TestSTDSYNC_LockWithWStaysOffSlowPaths and
// TestSTDSYNC_CancelFastTestInline.
// How: as TestSTDSYNC_CloseLockBranchesRun does, a scratch copy of std
// turns a branch into a panic, and a planted schedule of
// core/tests_sync.mk must (or must not) reach it. Starts no wsl.exe.

package std

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// syncProbe turns the text old of std file into new in a scratch copy.
type syncProbe struct{ file, old, new string }

// syncSlowEntryProbes make the entry of each of the lock word's slow paths
// a panic naming it.
var syncSlowEntryProbes = []syncProbe{
	{"core/sync_mutex.mk", "MutexLockSlow(w: &raw.Atomic[u64], io: Io, clock: bool): Canceled!void {\n",
		"MutexLockSlow(w: &raw.Atomic[u64], io: Io, clock: bool): Canceled!void {\n    raw.Fail(99, \"probe: MutexLockSlow\")\n"},
	{"core/sync_mutex.mk", "MutexUnlockSlow(w: &raw.Atomic[u64], io: Io) {\n",
		"MutexUnlockSlow(w: &raw.Atomic[u64], io: Io) {\n    raw.Fail(99, \"probe: MutexUnlockSlow\")\n"},
}

// syncProbedBlock runs the std test blocks of module whose names contain
// block, dev and release, on one carrier, over a copy of std with probes
// applied, and returns each profile's output and exit status.
func syncProbedBlock(t *testing.T, module, block string, probes []syncProbe) map[string]string {
	t.Helper()
	root := t.TempDir()
	stdDir := copyStd(t, root, nil)
	for _, p := range probes {
		f := filepath.Join(stdDir, filepath.FromSlash(p.file))
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		s := strings.ReplaceAll(string(b), "\r\n", "\n")
		if strings.Count(s, p.old) != 1 {
			t.Fatalf("probe: %q is not in %s exactly once", p.old, p.file)
		}
		if err := os.WriteFile(f, []byte(strings.Replace(s, p.old, p.new, 1)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("MK_CACHE", filepath.Join(root, "cache"))
	t.Setenv("MK_CARRIERS", "1")
	outs := map[string]string{}
	for _, profile := range []string{"dev", "release"} {
		args := []string{"test", "--std=" + module, "-run", block}
		if profile == "release" {
			args = append(args, "--release")
		}
		out, status := mkCmd(t, root, stdDir, args...)
		if strings.Contains(out, "no usable LLVM") {
			t.Skipf("needs the LLVM toolchain: %s", out)
		}
		outs[profile] = fmt.Sprintf("%s\n(status %d)", out, status)
	}
	return outs
}

// TestSTDSYNC_LockWithWStaysOffSlowPaths: tests_sync.mk's block "with W
// set, Lock takes a free lock and Unlock clears L, ..." over a std whose
// MutexLockSlow and MutexUnlockSlow panic on entry, dev and release: it
// passes, so with W set (nobody queued, or a head that is not starving
// queued) a Lock and an Unlock each stay off the slow path. Before the
// first steps were inline, Lock's Cas(0, L) and Unlock's Cas(L, 0) failed
// on such a word, and the block panicked in MutexLockSlow.
func TestSTDSYNC_LockWithWStaysOffSlowPaths(t *testing.T) {
	const name = "with W set, Lock takes a free lock and Unlock clears L, each with one Cas, off the slow paths"
	for profile, out := range syncProbedBlock(t, "std.core", name, syncSlowEntryProbes) {
		if !strings.Contains(out, fmt.Sprintf("PASS std.core %q", name)) {
			t.Errorf("%s: the block did not pass with the slow paths a panic\n%s", profile, out)
		}
	}
}

// syncCancelFastProbe makes core's CancelPoint panic when it is entered while
// its own fast test (CancelClear: a native Io, no scope cancelled) would have
// answered, that is, when a caller could have made the test inline.
var syncCancelFastProbe = []syncProbe{
	{"core/sync_mutex.mk", "CancelPoint(io: Io): Canceled!void {\n",
		"CancelPoint(io: Io): Canceled!void {\n    if IsNative(io) && Native.live.Load() == 0 {\n        raw.Fail(99, \"probe: CancelPoint with nothing cancelled\")\n    }\n"},
}

// TestSTDSYNC_CancelFastTestInline: the blocks "with no scope cancelled,
// Lock, Send and Recv check for cancellation without entering CancelPoint"
// (std.core) and "Wait-Fast with no scope cancelled, ..." (std.sync) pass
// over a std whose CancelPoint panics when entered while its fast test
// would have answered, dev and release. Before the test was inline at those
// sites, each called CancelPoint (or SyncCancel, which calls it) first.
func TestSTDSYNC_CancelFastTestInline(t *testing.T) {
	for _, b := range []struct{ module, name string }{
		{"std.core", "with no scope cancelled, Lock, Send and Recv check for cancellation without entering CancelPoint"},
		{"std.sync", "Wait-Fast with no scope cancelled, the waits check for cancellation without entering CancelPoint"},
	} {
		for profile, out := range syncProbedBlock(t, b.module, b.name, syncCancelFastProbe) {
			if !strings.Contains(out, fmt.Sprintf("PASS %s %q", b.module, b.name)) {
				t.Errorf("%s %s: the block did not pass with the probe\n%s", b.module, profile, out)
			}
		}
	}
}

// syncReadSlowProbe makes std.sync's RwReadSlow panic on entry.
var syncReadSlowProbe = []syncProbe{
	{"sync/rwlock.mk", "RwReadSlow[T](l: &RwLock[T], io: Io, old0: u32, added: bool): Canceled!void {\n",
		"RwReadSlow[T](l: &RwLock[T], io: Io, old0: u32, added: bool): Canceled!void {\n    raw.Fail(99, \"probe: RwReadSlow\")\n"},
}

// TestSTDSYNC_LockReadFastStaysOffSlowPath: tests_rwlock.mk's block
// "Conc-RwLock readers with no writer and no scope cancelled stay off
// RwReadSlow" passes over a std whose RwReadSlow panics on entry, dev and
// release, so LockRead's fast path (CancelClear, the Add and one compare)
// decides on its own; and a block that reads after a write (the stale W
// sends the first reader through RwReadSlow) fails with the probe, so the
// probe is live.
func TestSTDSYNC_LockReadFastStaysOffSlowPath(t *testing.T) {
	const fast = "Conc-RwLock readers with no writer and no scope cancelled stay off RwReadSlow"
	for profile, out := range syncProbedBlock(t, "std.sync", fast, syncReadSlowProbe) {
		if !strings.Contains(out, fmt.Sprintf("PASS %s %q", "std.sync", fast)) {
			t.Errorf("%s: the block did not pass with the probe\n%s", profile, out)
		}
	}
	const slow = "Conc-RwLock write, two readers at once, Into"
	for profile, out := range syncProbedBlock(t, "std.sync", slow, syncReadSlowProbe) {
		if !strings.Contains(out, "probe: RwReadSlow") {
			t.Errorf("%s: the probe did not fire after a write\n%s", profile, out)
		}
	}
}

// syncRwFullSrc is TestSTDSYNC_RwLockFullCountPanics's blocks: a LockRead
// on a word preset to a full reader count, with W clear or set, on the
// fast path or (slow) with another task's scope cancelled, so that
// CancelClear is false and RwReadSlow makes the Add. Each must panic with
// code 1. They are added to a scratch copy of tests_rwlock.mk, since a
// panicking block would fail `mk test --std=std.sync` (TestSTDSYNC_TestBlocks).
const syncRwFullSrc = `RwFullPanics(sys: &System, w0: u32, slow: bool) {
    l := RwLock[i64].New(0)
    on := atomic.AtomicInt.New(0)
    stop := atomic.AtomicInt.New(0)
    scope s := sys.io.Scope() {
        if slow {
            s.go RwHoldCancelled(sys.io, &on, &stop)
            for on.Load() == 0 {
                sys.io.Yield() else {}
            }
            Assert(!core.CancelClear(sys.io))
        }
        l.rs.Store(w0)
        r := l.LockRead(sys.io).Must()
        r.Unlock()
        stop.Store(1)
    }
}

test "RwFull count fast" (sys: &System) {
    RwFullPanics(sys, RwCount, false)
}

test "RwFull count and W fast" (sys: &System) {
    RwFullPanics(sys, RwW | RwCount, false)
}

test "RwFull count slow" (sys: &System) {
    RwFullPanics(sys, RwCount, true)
}

test "RwFull count and W slow" (sys: &System) {
    RwFullPanics(sys, RwW | RwCount, true)
}

`

// TestSTDSYNC_RwLockFullCountPanics: LockRead documents a panic with code 1
// past 2^30 - 1 readers. Each of syncRwFullSrc's four blocks, dev and
// release, fails with that panic's message, so neither compare of the
// bound (LockRead's and RwReadSlow's, W clear or set) can admit a full
// count: turning either from < to <= makes a block pass.
func TestSTDSYNC_RwLockFullCountPanics(t *testing.T) {
	anchor := "RwHoldCancelled(io: Io, on: &atomic.AtomicInt, stop: &atomic.AtomicInt) {\n"
	probes := []syncProbe{{"sync/tests_rwlock.mk", anchor, syncRwFullSrc + anchor}}
	for _, name := range []string{"RwFull count fast", "RwFull count and W fast", "RwFull count slow", "RwFull count and W slow"} {
		for profile, out := range syncProbedBlock(t, "std.sync", name, probes) {
			if !strings.Contains(out, "RwLock: too many readers") || strings.Contains(out, fmt.Sprintf("PASS std.sync %q", name)) || strings.Contains(out, "(status 0)") {
				t.Errorf("%s %s: the block did not panic with the full-count message\n%s", name, profile, out)
			}
		}
	}
}
