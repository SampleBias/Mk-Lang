// Why: std.mem's allocators are reached only through std.core's
// AllocBytes, ResizeBytes and FreeBytes, which user code cannot call.
// What: the fixture module memfix, written into a copy of std, and the Go
// models of Fixed and Counting that the std.mem tests compare runs with.
// How: memfix drives the allocators directly; splitmix generates operations.

package std

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// memFixModule is std/memfix/memfix.mk in the copy. Each Run* function
// executes an op list (see memOp) and prints one line per op. Offsets are
// relative to the buffer's start; the first line is the buffer's address
// modulo 4096, so the model can compute alignment padding.
const memFixModule = `// Fixture of the std-mem2 tests (std/std-mem_allocharness_test.go).

import std.core
import std.mem
import std.raw

struct Blk {
    p: *u8
    size: int
    align: int
}

const MaxBlk = 512

export Skew(buf: []u8): int {
    return Trunc[int](raw.Addr(raw.Data(buf)) & 4095)
}

Off(base: *u8, p: *u8): int {
    return Trunc[int](raw.Addr(p) -% raw.Addr(base))
}

// RunFixed runs ops over a Fixed on buf: 0 size align alloc, 1 i new
// resize, 2 i free, 3 reset, and two that pass a block's pointer with other
// numbers than its own and keep it live: 4 i size align free, 5 i old new
// resize; 6 allocates the whole buffer past its first 8-byte boundary at
// alignment 1. i is taken modulo the live blocks.
export RunFixed(w: mut Writer, buf: mut []u8, ops: []int): WriteError!void {
    base := raw.DataMut(buf)
    whole := buf.Len() - Trunc[int]((0 -% raw.Addr(base)) & 7)
    try Print(w, "skew {s}\n", (s: Trunc[int](raw.Addr(base) & 4095)))
    var f := mem.Fixed.New(buf)
    var blks: [512]Blk = [Blk(p: raw.Null[u8](), size: 0, align: 1); 512]
    var n := 0
    var i := 0
    for i < ops.Len() {
        k := ops[i]
        if k == 3 {
            f.Reset()
            n = 0
            try Print(w, "z\n")
            i += 1
            continue
        }
        a := f.Alloc()
        if k == 0 || k == 6 {
            var size := whole
            var align := 1
            var step := 1
            if k == 0 {
                size = ops[i + 1]
                align = ops[i + 2]
                step = 3
            }
            p := core.AllocBytes(a, size, align)
            if raw.IsNull(p) {
                try Print(w, "a null\n")
            } else {
                try Print(w, "a {o}\n", (o: Off(base, p)))
                if n < MaxBlk {
                    blks[n] = Blk(p: p, size: size, align: align)
                    n += 1
                }
            }
            i += step
        } else if k == 1 {
            if n == 0 {
                try Print(w, "r -\n")
            } else {
                j := ops[i + 1] % n
                b := blks[j]
                ok := core.ResizeBytes(a, b.p, b.size, ops[i + 2], b.align)
                if ok {
                    blks[j] = Blk(p: b.p, size: ops[i + 2], align: b.align)
                }
                try Print(w, "r {o} {ok}\n", (o: Off(base, b.p), ok: ok))
            }
            i += 3
        } else if k == 4 {
            if n == 0 {
                try Print(w, "x -\n")
            } else {
                b := blks[ops[i + 1] % n]
                core.FreeBytes(a, b.p, ops[i + 2], ops[i + 3])
                try Print(w, "x {o}\n", (o: Off(base, b.p)))
            }
            i += 4
        } else if k == 5 {
            if n == 0 {
                try Print(w, "y -\n")
            } else {
                j := ops[i + 1] % n
                b := blks[j]
                ok := core.ResizeBytes(a, b.p, ops[i + 2], ops[i + 3], b.align)
                if ok {
                    blks[j] = Blk(p: b.p, size: ops[i + 3], align: b.align)
                }
                try Print(w, "y {o} {ok}\n", (o: Off(base, b.p), ok: ok))
            }
            i += 4
        } else {
            if n == 0 {
                try Print(w, "f -\n")
            } else {
                j := ops[i + 1] % n
                b := blks[j]
                core.FreeBytes(a, b.p, b.size, b.align)
                blks[j] = blks[n - 1]
                n -= 1
                try Print(w, "f {o}\n", (o: Off(base, b.p)))
            }
            i += 2
        }
    }
}

// RunCounting runs the same ops through a Counting over inner (reset is a
// no-op line) and prints the result and the three counts after each op.
export RunCounting(w: mut Writer, inner: &Alloc, ops: []int): WriteError!void {
    c := mem.Counting.New(inner)
    var blks: [512]Blk = [Blk(p: raw.Null[u8](), size: 0, align: 1); 512]
    var n := 0
    var i := 0
    for i < ops.Len() {
        k := ops[i]
        a := c.Alloc()
        if k == 3 {
            try Print(w, "z")
            i += 1
        } else if k == 0 {
            p := core.AllocBytes(a, ops[i + 1], ops[i + 2])
            if raw.IsNull(p) {
                try Print(w, "a null")
            } else {
                try Print(w, "a ok")
                if n < MaxBlk {
                    blks[n] = Blk(p: p, size: ops[i + 1], align: ops[i + 2])
                    n += 1
                }
            }
            i += 3
        } else if k == 1 {
            if n == 0 {
                try Print(w, "r -")
            } else {
                j := ops[i + 1] % n
                b := blks[j]
                ok := core.ResizeBytes(a, b.p, b.size, ops[i + 2], b.align)
                if ok {
                    blks[j] = Blk(p: b.p, size: ops[i + 2], align: b.align)
                }
                try Print(w, "r {ok}", (ok: ok))
            }
            i += 3
        } else {
            if n == 0 {
                try Print(w, "f -")
            } else {
                j := ops[i + 1] % n
                b := blks[j]
                core.FreeBytes(a, b.p, b.size, b.align)
                blks[j] = blks[n - 1]
                n -= 1
                try Print(w, "f")
            }
            i += 2
        }
        try Print(w, " b={b} l={l} p={p}\n", (b: c.Bytes(), l: c.Live(), p: c.Peak()))
    }
    // Give back what is still live, so the test heap reports no leak.
    for n > 0 {
        n -= 1
        core.FreeBytes(c.Alloc(), blks[n].p, blks[n].size, blks[n].align)
    }
    try Print(w, "end b={b} l={l} p={p}\n", (b: c.Bytes(), l: c.Live(), p: c.Peak()))
}

// SameOwner reports whether two calls of Alloc on one value, and two calls
// of mem.Null, return one owner.
export SameOwner(buf: mut []u8, inner: &Alloc): bool {
    f := mem.Fixed.New(buf)
    c := mem.Counting.New(inner)
    return raw.Same(raw.Of(f.Alloc()), raw.Of(f.Alloc())) && raw.Same(raw.Of(c.Alloc()), raw.Of(c.Alloc())) && raw.Same(raw.Of(mem.Null()), raw.Of(mem.Null()))
}

// FreeNull frees, in this module, a HeapList the caller made with
// mem.Null() in its own module: HeapList.Free's owner check (code 12)
// panics unless both modules' mem.Null() are one owner.
export FreeNull(xs: HeapList[int]) {
    xs.Free(mem.Null())
}
`

// memBuild copies std with the memfix module, writes src as main.mk and
// builds it with flags; a missing toolchain skips, as buildNative does.
func memBuild(t *testing.T, src string, flags ...string) string {
	t.Helper()
	root := t.TempDir()
	stdDir := memCopyStd(t, root)
	if err := os.WriteFile(filepath.Join(root, "main.mk"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(root, "main")
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	args := append([]string{"build"}, flags...)
	args = append(args, "-o", exe, "main.mk")
	out, status := mkCmd(t, root, stdDir, args...)
	if status != 0 {
		if strings.Contains(out, "no usable LLVM") {
			t.Skipf("needs the LLVM toolchain: %s", out)
		}
		t.Fatalf("mk build: exit %d\n%s", status, out)
	}
	return exe
}

// memCheck runs mk check over memCopyStd's std on src and returns the
// output and exit status.
func memCheck(t *testing.T, src string) (string, int) {
	t.Helper()
	root := t.TempDir()
	stdDir := memCopyStd(t, root)
	if err := os.WriteFile(filepath.Join(root, "main.mk"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	return mkCmd(t, root, stdDir, "check", "--max-diagnostics", "0", "main.mk")
}

// memStdKeep are the top-level directories of std these tests build over:
// the modules std.mem needs, plus std/mem and the fixture. The rest of std
// (std/coll, ...) is left out of the copy, so a half-written file elsewhere
// cannot fail these tests; std.mem imports none of it. The gate's own runs (TestStdChecksClean,
// internal/stdtest) still build the whole of std.
var memStdKeep = map[string]bool{
	"atomic": true, "core": true, "corpus": true, "derive": true, "fs": true, "math": true, "net": true,
	"pkg": true, "proc": true, "raw": true, "rt": true, "sync": true, "sys": true, "task": true, "time": true,
	"mem": true, "memfix": true,
	// pkg.mk selects free/sys for the freestanding target, std.rt imports
	// std.simd, and std.sync's test blocks import std.sim, which imports
	// std.hash.
	"free": true, "simd": true, "sim": true, "hash": true,
	// std.core imports std.dyn.erased (core_conn.mk) and std.simd's
	// kernel imports std.mmio (wipe.mk).
	"dyn": true, "mmio": true,
	// std.core and std.simd import std.target.
	"target": true,
	// pkg.mk selects std.linux's directory on every target, std.sim's
	// test blocks import std.runtime, whose test blocks import std.vm.
	"linux": true, "runtime": true, "vm": true,
	// pkg.mk selects std.sys.lc's directory (lc/other, lc/macos) on every
	// target.
	"lc": true,
}

// unsafeList is the unsafe_modules list of pkg.mk.
var unsafeList = regexp.MustCompile(`unsafe_modules: \[([^\]]*)\]`)

// memCopyStd copies std with the memfix fixture into root/std, drops every
// top-level module directory memStdKeep does not name, and drops their
// entries from pkg.mk's unsafe_modules (the manifest refuses a listed
// directory that does not exist).
func memCopyStd(t *testing.T, root string) string {
	t.Helper()
	stdDir := copyStd(t, root, map[string]string{"memfix/memfix.mk": memFixModule})
	ents, err := os.ReadDir(stdDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if e.IsDir() && !memStdKeep[e.Name()] {
			if err := os.RemoveAll(filepath.Join(stdDir, e.Name())); err != nil {
				t.Fatal(err)
			}
		}
	}
	manifest := filepath.Join(stdDir, "pkg.mk")
	b, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	m := unsafeList.FindSubmatchIndex(b)
	if m == nil {
		t.Fatal("std/pkg.mk has no unsafe_modules list")
	}
	var keep []string
	for _, ent := range strings.Split(string(b[m[2]:m[3]]), ",") {
		ent = strings.TrimSpace(ent)
		dir := strings.SplitN(strings.Trim(ent, `"`), "/", 2)[0]
		if ent != "" && memStdKeep[dir] {
			keep = append(keep, ent)
		}
	}
	out := string(b[:m[2]]) + strings.Join(keep, ", ") + string(b[m[3]:])
	if err := os.WriteFile(manifest, []byte(out), 0o644); err != nil {
		t.Fatal(err)
	}
	return stdDir
}

// splitmix is splitmix64 (Steele, Lea and Flood 2014), the generator of the
// benchmark rules in tests/bench/README.md.
type splitmix uint64

func (s *splitmix) next() uint64 {
	*s += 0x9e3779b97f4a7c15
	z := uint64(*s)
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

func (s *splitmix) below(n int) int { return int(s.next() % uint64(n)) }

// memOps is a random op list in RunFixed's encoding: allocations of 0 to
// maxSize bytes at alignments 1 to 4096 (mostly small), resizes, frees and
// rare resets.
func memOps(seed uint64, count, maxSize int, resets bool) []int {
	s := splitmix(seed)
	aligns := []int{1, 1, 2, 4, 8, 8, 8, 16, 32, 64, 4096}
	var ops []int
	for range count {
		switch r := s.below(100); {
		case r < 50:
			ops = append(ops, 0, s.below(maxSize+1), aligns[s.below(len(aligns))])
		case r < 70:
			ops = append(ops, 1, s.below(1<<20), s.below(maxSize*2+1))
		case r < 97 || !resets:
			ops = append(ops, 2, s.below(1<<20))
		default:
			ops = append(ops, 3)
		}
	}
	return ops
}

// memWildOps is memOps with requests no container makes mixed in: bad sizes and alignments, and resizes and frees of a live block
// with a negative old size or one larger than any buffer here, which must
// change nothing. (A plausible wrong size breaks the AllocTable contract,
// and what the allocator then does is not defined, so none is drawn.)
func memWildOps(seed uint64, count, maxSize int, resets bool) []int {
	s := splitmix(seed)
	base := memOps(seed^0x5bd1e995, count, maxSize, resets)
	bad := []int{-1, 0, 3, 6, 12, -8, 1 << 31, 1 << 40, 1 << 30}
	var ops []int
	for i := 0; i < len(base); {
		n := map[int]int{0: 3, 1: 3, 2: 2, 3: 1}[base[i]]
		ops = append(ops, base[i:i+n]...)
		i += n
		switch r := s.below(100); {
		case r < 6:
			ops = append(ops, 0, s.below(maxSize+1), bad[s.below(len(bad))])
		case r < 8:
			ops = append(ops, 0, -1-s.below(1<<20), 8)
		case r < 12:
			ops = append(ops, 5, s.below(1<<20), memWildSize(&s), s.below(maxSize*2+1))
		case r < 16:
			ops = append(ops, 4, s.below(1<<20), memWildSize(&s), 1<<s.below(13))
		}
	}
	return ops
}

// memWildSize is a size no block of a test buffer can have: negative, or
// past every buffer (64 KiB).
func memWildSize(s *splitmix) int {
	if s.below(2) == 0 {
		return -1 - s.below(1000)
	}
	return 1<<17 + s.below(1000)
}

// fixedModel is the Go model of mem.Fixed (mem.mk's header: base moved up
// to the buffer's first 8-byte boundary, upward bump in 8-byte granules,
// the last block resized and freed in place with the padding before it, a
// wide block's header in its padding, any block shrinks). Offsets are from
// that moved base; lead is where it lies in the buffer (newFixedModel), and
// a buffer with no boundary has size -8. hdr is what the buffer holds at
// each header slot (never written slots read 0, the fixtures' buffers are
// zeroed); no fixture writes into a block, so headers are the only bytes
// that change.
type fixedModel struct {
	lead, skew, size, used int
	hdr                    map[int]int
}

// newFixedModel is the model of Fixed.New over n bytes whose address is
// skew modulo 4096.
func newFixedModel(skew, n int) *fixedModel {
	lead := (-skew) & 7
	if lead > n {
		return &fixedModel{skew: skew, size: -8}
	}
	return &fixedModel{lead: lead, skew: skew + lead, size: n - lead}
}

// round is what used becomes after a last block ending at x: the next
// multiple of 8.
func (m *fixedModel) round(x int) int { return (x + 7) &^ 7 }

func memAlignOK(a int) bool { return a >= 1 && a <= 1<<30 && a&(a-1) == 0 }

// alloc returns the block's offset, and the header slot's (-1: none).
func (m *fixedModel) allocHdr(size, align int) (off, hdr int, ok bool) {
	if size < 0 || !memAlignOK(align) {
		return 0, -1, false
	}
	lo := m.used
	if align > 8 && lo != 0 {
		lo += 8
	}
	off = lo
	if align > 8 {
		off = lo + (-(m.skew + lo))&(align-1)
	}
	if size > m.size-off {
		return 0, -1, false
	}
	hdr = -1
	if align > 8 && off >= 8 {
		if m.hdr == nil {
			m.hdr = map[int]int{}
		}
		hdr = off - 8
		m.hdr[hdr] = m.used
	}
	m.used = m.round(off + size)
	return off, hdr, true
}

func (m *fixedModel) alloc(size, align int) (int, bool) {
	off, _, ok := m.allocHdr(size, align)
	return off, ok
}

// last reports whether the n bytes at off are this buffer's last block; ok
// is false when they are not below used at all.
func (m *fixedModel) last(off, n int) (last, ok bool) {
	if off < 0 || off > m.used || n < 0 || n > m.used-off {
		return false, false
	}
	return m.round(off+n) == m.used, true
}

func (m *fixedModel) resize(off, old, new int) bool {
	if old < 0 || new < 0 {
		return false
	}
	last, ok := m.last(off, old)
	switch {
	case !ok:
		return false
	case !last:
		return new <= old
	case new > m.size-off:
		return false
	}
	m.used = m.round(off + new)
	return true
}

func (m *fixedModel) free(off, size, align int) {
	if size < 0 {
		return
	}
	if last, ok := m.last(off, size); !ok || !last {
		return
	}
	switch {
	case align <= 8:
		m.used = off &^ 7
	case off < 8:
		m.used = 0
	case off > m.size:
		m.used = off &^ 7
	default:
		h := m.hdr[(off-8)&^7]
		if h >= 0 && h <= off-8 && off-8-h < align && h&7 == 0 {
			m.used = h
		} else {
			m.used = off &^ 7
		}
	}
}

// memBlk is a live block of a run; hdr is its header slot's offset, or -1.
type memBlk struct{ off, size, align, hdr int }

// memOverlap reports whether [a, a+n) and [b, b+k) share a byte.
func memOverlap(a, n, b, k int) bool { return n > 0 && k > 0 && a < b+k && b < a+n }

// fixedOracle checks, independently of the model, that the block at index
// j of blks (just handed out, or just grown in place) is aligned, inside
// the buffer, and shares no byte with another live block or with another
// live block's header, and that its own header shares none with a block.
func fixedOracle(t *testing.T, skew, size int, blks []memBlk, j int) {
	t.Helper()
	x := blks[j]
	if (skew+x.off)%x.align != 0 || x.off < 0 || x.off+x.size > size {
		t.Fatalf("model block at %d size %d align %d skew %d breaks alignment or bounds", x.off, x.size, x.align, skew)
	}
	if x.hdr >= 0 && (x.hdr+8 > x.off || (skew+x.hdr)%8 != 0) {
		t.Fatalf("model header of block %d at %d is not 8 aligned bytes in its padding", x.off, x.hdr)
	}
	for k, o := range blks {
		if k == j {
			continue
		}
		if memOverlap(x.off, x.size, o.off, o.size) {
			t.Fatalf("model block %d+%d overlaps live %d+%d", x.off, x.size, o.off, o.size)
		}
		if o.hdr >= 0 && memOverlap(x.off, x.size, o.hdr, 8) {
			t.Fatalf("model block %d+%d overlaps the header at %d of live block %d", x.off, x.size, o.hdr, o.off)
		}
		if x.hdr >= 0 && memOverlap(x.hdr, 8, o.off, o.size) {
			t.Fatalf("model header at %d of block %d overlaps live block %d+%d", x.hdr, x.off, o.off, o.size)
		}
	}
}

// fixedExpect runs ops on the model with the buffer's skew and returns the
// lines RunFixed must print, checking each block with fixedOracle.
func fixedExpect(t *testing.T, skew, size int, ops []int) string {
	t.Helper()
	m := newFixedModel(skew, size)
	var b strings.Builder
	fmt.Fprintf(&b, "skew %d\n", skew)
	var blks []memBlk
	for i := 0; i < len(ops); {
		switch ops[i] {
		case 3:
			m.used = 0
			blks = blks[:0]
			b.WriteString("z\n")
			i++
		case 0, 6:
			sz, al, n := m.size, 1, 1
			if ops[i] == 0 {
				sz, al, n = ops[i+1], ops[i+2], 3
			}
			off, hdr, ok := m.allocHdr(sz, al)
			if !ok {
				b.WriteString("a null\n")
			} else {
				fmt.Fprintf(&b, "a %d\n", m.lead+off)
				if len(blks) < 512 {
					blks = append(blks, memBlk{off, sz, al, hdr})
					fixedOracle(t, m.skew, m.size, blks, len(blks)-1)
				}
			}
			i += n
		case 1, 5:
			op, n := "r", 3
			if ops[i] == 5 {
				op, n = "y", 4
			}
			if len(blks) == 0 {
				fmt.Fprintf(&b, "%s -\n", op)
			} else {
				j := ops[i+1] % len(blks)
				old, new := blks[j].size, ops[i+2]
				if n == 4 {
					old, new = ops[i+2], ops[i+3]
				}
				ok := m.resize(blks[j].off, old, new)
				if ok {
					blks[j].size = new
					fixedOracle(t, m.skew, m.size, blks, j)
				}
				fmt.Fprintf(&b, "%s %d %t\n", op, m.lead+blks[j].off, ok)
			}
			i += n
		case 4:
			if len(blks) == 0 {
				b.WriteString("x -\n")
			} else {
				x := blks[ops[i+1]%len(blks)]
				m.free(x.off, ops[i+2], ops[i+3])
				fmt.Fprintf(&b, "x %d\n", m.lead+x.off)
			}
			i += 4
		default:
			if len(blks) == 0 {
				b.WriteString("f -\n")
			} else {
				j := ops[i+1] % len(blks)
				x := blks[j]
				m.free(x.off, x.size, x.align)
				blks[j] = blks[len(blks)-1]
				blks = blks[:len(blks)-1]
				fmt.Fprintf(&b, "f %d\n", m.lead+x.off)
			}
			i += 2
		}
	}
	return b.String()
}

// countingExpect is the model of RunCounting over an inner allocator that
// grants or refuses as the Mk run printed (got): it predicts the counts from
// the results, and the results themselves when inner is a fixedModel
// (fixed != nil) or Null (null).
func countingExpect(t *testing.T, ops []int, got string, fixed *fixedModel, null bool) string {
	t.Helper()
	lines := strings.Split(got, "\n")
	var b strings.Builder
	var bytes, live, peak int
	var blks []memBlk
	line := 0
	result := func() string {
		if line >= len(lines) {
			return ""
		}
		return lines[line]
	}
	for i := 0; i < len(ops); line++ {
		switch ops[i] {
		case 3:
			b.WriteString("z")
			i++
		case 0:
			sz, al := ops[i+1], ops[i+2]
			ok := strings.HasPrefix(result(), "a ok")
			off := 0
			switch {
			case null:
				ok = false
			case fixed != nil:
				off, ok = fixed.alloc(sz, al)
			}
			if ok {
				b.WriteString("a ok")
				bytes += sz
				live++
				if len(blks) < 512 {
					blks = append(blks, memBlk{off, sz, al, -1})
				}
			} else {
				b.WriteString("a null")
			}
			i += 3
		case 1:
			if len(blks) == 0 {
				b.WriteString("r -")
			} else {
				j := ops[i+1] % len(blks)
				ok := strings.HasPrefix(result(), "r true")
				switch {
				case null:
					ok = false
				case fixed != nil:
					ok = fixed.resize(blks[j].off, blks[j].size, ops[i+2])
				}
				if ok {
					bytes += ops[i+2] - blks[j].size
					blks[j].size = ops[i+2]
				}
				fmt.Fprintf(&b, "r %t", ok)
			}
			i += 3
		default:
			if len(blks) == 0 {
				b.WriteString("f -")
			} else {
				j := ops[i+1] % len(blks)
				x := blks[j]
				if fixed != nil {
					fixed.free(x.off, x.size, x.align)
				}
				bytes -= x.size
				live--
				blks[j] = blks[len(blks)-1]
				blks = blks[:len(blks)-1]
				b.WriteString("f")
			}
			i += 2
		}
		peak = max(peak, bytes)
		fmt.Fprintf(&b, " b=%d l=%d p=%d\n", bytes, live, peak)
	}
	fmt.Fprintf(&b, "end b=0 l=0 p=%d\n", peak)
	return b.String()
}

// mkInts is a Mk array literal of xs, or a typed empty one.
func mkInts(xs []int) string {
	if len(xs) == 0 {
		return "[0; 0]"
	}
	parts := make([]string, len(xs))
	for i, x := range xs {
		parts[i] = strconv.Itoa(x)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}
