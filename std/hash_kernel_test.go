// Why: hash.Crc32 and hash.Sha512 choose a SIMD kernel or a portable path by
// CPU, and both paths must give the same results on any machine.
// What: TestHash_Crc32* and TestHash_Sha512* (Crc32Fold, Sha512Blocks),
// and TestHash_Linux.
// How: a fixture masks the CPU test so the portable path runs anywhere; a
// control std with one path broken shows which path ran.

package std

import (
	"crypto/sha512"
	"fmt"
	"hash/crc32"
	"strings"
	"testing"
)

// h11CrcOff masks the kernel's pclmul test, so CrcRun takes the table.
var h11CrcOff = []sdForce{{"simd/kernel/crc32.mk", `id.Has("pclmul")`, "false", 1}}

// h11CrcBreakKernel breaks the fold (the register enters it xored with
// 1) on both its paths, so a checksum of 64 bytes or more is right only
// when the table ran.
var h11CrcBreakKernel = append(append([]sdForce{}, h11CrcBreakNarrow...), h11CrcBreakWide...)

// h11CrcBreakNarrow breaks only the 128-bit fold (Crc32Ni), and
// h11CrcBreakWide only the AVX-512 one (Crc32Wide).
var h11CrcBreakNarrow = []sdForce{{"simd/kernel/crc32.mk", "[u64(c), 0]", "[u64(c) ^ 1, 0]", 1}}
var h11CrcBreakWide = []sdForce{{"simd/kernel/crc32.mk", "[u64(c), 0, 0, 0, 0, 0, 0, 0]", "[u64(c) ^ 1, 0, 0, 0, 0, 0, 0, 0]", 1}}

// h11CrcNoWide masks the kernel's avx512f test, so every fold takes the
// 128-bit path.
var h11CrcNoWide = []sdForce{{"simd/kernel/crc32.mk", `id.Has("avx512f")`, "false", 1}}

// h11CrcLens are the one-shot and stream lengths: every length to 300 (the
// kernel's 64-byte threshold, its four-way loop, its 16-byte loop and the
// table's tail), then block edges and longer inputs.
var h11CrcLens = append(s8Range(0, 300), 447, 448, 511, 512, 513, 1000, 4095, 4096, 4097, 65543)

// h11CrcEval are the lengths of h2ConstText checksummed at compile time.
var h11CrcEval = []int{0, 9, 63, 64, 65, 128, 200, len(h2ConstText)}

// h11CrcProgram prints whether simd.Cpu() has pclmul and aes, and whether
// the wide fold can run (also avx512f and vpclmulqdq on x86_64), then per
// length of h11CrcEval the compile-time Crc32 of h2ConstText's prefix
// ("E"), then per length of h11CrcLens the one-shot Crc32 ("L") and a
// Crc32State fed in seeded random pieces ("S") of splitmix64 bytes, at an
// offset of n % 8 so the kernel's loads are unaligned too.
func h11CrcProgram() string {
	var b strings.Builder
	b.WriteString(strings.Replace(h2Prelude, "import std.hash\n", "import std.hash\nimport std.simd\n", 1))
	fmt.Fprintf(&b, "\nconst Text = %q\n", h2ConstText)
	for i, n := range h11CrcEval {
		fmt.Fprintf(&b, "const E%d: u32 = hash.Crc32(Text.AsBytes()[0..%d])\n", i, n)
	}
	fmt.Fprintf(&b, "const Lens: [%d]int = %s\n", len(h11CrcLens), mkIntArray(h11CrcLens))
	fmt.Fprintf(&b, "const Pieces: [%d]int = %s\n", len(h2Pieces), mkIntArray(h2Pieces))
	b.WriteString(`
export Main(sys: &System): WriteError|OutOfMemory!void {
    var out := sys.Stdout()
    clmul := simd.Cpu().Has("pclmul") && simd.Cpu().Has("aes")
    wide := clmul && Target.arch == Arch.X64 && simd.Cpu().Has("avx512f") && simd.Cpu().Has("vpclmulqdq")
    try Print(mut out, "cpu {c} wide {w}\n", (c: clmul, w: wide))
`)
	for i := range h11CrcEval {
		fmt.Fprintf(&b, "    try Print(mut out, \"E {x}\\n\", (x: E%d))\n", i)
	}
	b.WriteString(`    var r := Rng(s: 1)
    buf := try sys.arena.Slice[u8](65543 + 8)
    Fill(mut r, buf)
    var sr := Rng(s: 7)
    for k in 0..Lens.Len() {
        n := Lens[k]
        off := n % 8
        m := buf[off..off + n]
        try Print(mut out, "L {x}\n", (x: hash.Crc32(m)))
        var s := hash.Crc32State.New()
        var i := 0
        for i < n {
            var p := Pieces[Trunc[int](sr.Next() % 16)]
            if p > n - i {
                p = n - i
            }
            s.Bytes(m[i..i + p])
            i += p
        }
        try Print(mut out, "S {x}\n", (x: s.Sum()))
    }
}
`)
	return b.String()
}

// h11CrcWant is the oracle's output for h11CrcProgram after its cpu line.
func h11CrcWant() []string {
	var lines []string
	for _, n := range h11CrcEval {
		lines = append(lines, fmt.Sprintf("E %d", crc32.ChecksumIEEE([]byte(h2ConstText[:n]))))
	}
	r := hmSplitmix{1}
	buf := make([]byte, 65543+8)
	r.fill(buf)
	sr := hmSplitmix{7}
	for _, n := range h11CrcLens {
		off := n % 8
		d := crc32.ChecksumIEEE(buf[off : off+n])
		for i := 0; i < n; {
			i += min(h2Pieces[sr.next()%16], n-i)
		}
		lines = append(lines, fmt.Sprintf("L %d", d), fmt.Sprintf("S %d", d))
	}
	return lines
}

// h11CrcRun builds and runs h11CrcProgram over std with forces, and
// returns whether the CPU has pclmul and aes and the lines after the cpu
// line.
func h11CrcRun(t *testing.T, forces []sdForce, flags ...string) (bool, []string) {
	t.Helper()
	has, _, lines := h11CrcRunWide(t, forces, flags...)
	return has, lines
}

// h11CrcRunWide is h11CrcRun that also returns whether the wide fold can
// run.
func h11CrcRunWide(t *testing.T, forces []sdForce, flags ...string) (bool, bool, []string) {
	t.Helper()
	got := h2Run(t, sdFixtures(t, forces), h11CrcProgram(), flags...)
	lines := strings.Split(strings.TrimSuffix(got, "\n"), "\n")
	if len(lines) == 0 || !strings.HasPrefix(lines[0], "cpu ") {
		t.Fatalf("no cpu line:\n%.2000s", got)
	}
	return strings.HasPrefix(lines[0], "cpu true"), strings.HasSuffix(lines[0], "wide true"), lines[1:]
}

// TestHash_Crc32PathsMatchGo: hash.Crc32 and Crc32State, as built (the
// kernel where the CPU has pclmul and aes, its wide fold from 256 bytes
// where it also has avx512f and vpclmulqdq), with the wide fold masked
// (the 128-bit fold) and with the CPU test masked (the table on any
// machine), at dev and release, equal hash/crc32's IEEE
// checksums, and so do the compile-time checksums (the evaluator, whose
// Cpu() is the baseline, so the table). "123456789" gives 0xCBF43926.
func TestHash_Crc32PathsMatchGo(t *testing.T) {
	t.Parallel()
	if got := crc32.ChecksumIEEE([]byte("123456789")); got != 0xCBF43926 {
		t.Fatalf("oracle check value %#x", got)
	}
	want := h11CrcWant()
	for _, c := range []struct {
		name   string
		forces []sdForce
		flags  []string
	}{
		{"dispatch-dev", nil, nil},
		{"dispatch-release", nil, []string{"--release"}},
		{"masked-dev", h11CrcOff, nil},
		{"masked-release", h11CrcOff, []string{"--release"}},
		{"narrow-dev", h11CrcNoWide, nil},
		{"narrow-release", h11CrcNoWide, []string{"--release"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			has, got := h11CrcRun(t, c.forces, c.flags...)
			t.Logf("simd.Cpu() has pclmul and aes = %v", has)
			diffLines(t, strings.Join(got, "\n"), strings.Join(want, "\n"))
		})
	}
}

// TestHash_Crc32KernelRuns: with the fold broken, a build on a CPU with
// pclmul and aes gives a wrong checksum for every input of 64 bytes or
// more (every such input went through the kernel) and right ones below 64
// and at compile time (the evaluator's Cpu() is the baseline); masked,
// every checksum is right (the table ran everywhere). So the dispatch
// takes the kernel exactly where it should and the mask reaches it. A
// stream line is wrong when some piece of its input reached 64 bytes,
// which the oracle does not track, so only the one-shot lines are read
// per length.
func TestHash_Crc32KernelRuns(t *testing.T) {
	t.Parallel()
	want := h11CrcWant()
	for _, flags := range [][]string{nil, {"--release"}} {
		name := "dev"
		if flags != nil {
			name = "release"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, masked := h11CrcRun(t, append(append([]sdForce{}, h11CrcOff...), h11CrcBreakKernel...), flags...)
			diffLines(t, strings.Join(masked, "\n"), strings.Join(want, "\n"))
			has, got := h11CrcRun(t, h11CrcBreakKernel, flags...)
			if len(got) != len(want) {
				t.Fatalf("%d lines, want %d", len(got), len(want))
			}
			for i := range h11CrcEval {
				if got[i] != want[i] {
					t.Errorf("compile time with the fold broken: %q, want %q (the evaluator's Cpu() is the baseline)", got[i], want[i])
				}
			}
			if !has {
				t.Skip("this CPU lacks pclmul or aes: the kernel cannot run; the masked and compile-time halves passed")
			}
			wrong, right := 0, 0
			for k, n := range h11CrcLens {
				i := len(h11CrcEval) + 2*k
				if (got[i] != want[i]) != (n >= 64) {
					t.Errorf("length %d with the fold broken: %q against %q; want wrong exactly from 64 bytes", n, got[i], want[i])
				}
				if n >= 64 {
					wrong++
				} else {
					right++
				}
			}
			t.Logf("one-shot lines through the kernel: %d, through the table: %d", wrong, right)
		})
	}
}

// TestHash_Crc32WideFoldRuns: with only the 128-bit fold broken, a build
// on a CPU with avx512f and vpclmulqdq gives wrong one-shot checksums
// exactly from 64 to 255 bytes and right ones from 256 (the wide fold);
// with only the wide fold broken, wrong ones exactly from 256 bytes. So
// the wide fold runs, and only where it should.
func TestHash_Crc32WideFoldRuns(t *testing.T) {
	t.Parallel()
	want := h11CrcWant()
	for _, c := range []struct {
		name   string
		forces []sdForce
		lo, hi int
	}{
		{"narrow-broken", h11CrcBreakNarrow, 64, 256},
		{"wide-broken", h11CrcBreakWide, 256, 1 << 30},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			_, wide, got := h11CrcRunWide(t, c.forces, "--release")
			if len(got) != len(want) {
				t.Fatalf("%d lines, want %d", len(got), len(want))
			}
			if !wide {
				t.Skip("this CPU lacks avx512f or vpclmulqdq: the wide fold cannot run")
			}
			for k, n := range h11CrcLens {
				i := len(h11CrcEval) + 2*k
				if (got[i] != want[i]) != (n >= c.lo && n < c.hi) {
					t.Errorf("length %d: %q against %q; want wrong exactly from %d to below %d bytes", n, got[i], want[i], c.lo, c.hi)
				}
			}
		})
	}
}

// h11ShaOff masks the SHA-512 kernel's avx2 test, so Sha512Blocks runs the
// portable loop.
var h11ShaOff = []sdForce{{"simd/kernel/sha512.mk", `c.Has("avx2")`, "false", 1}}

// h11ShaBreakPortable breaks std.hash's portable SHA-512 loop (the first
// chaining word gains 1 per block), so a digest is right only when every
// block went through the kernel. The first word, not the last, so that
// SHA-384's 48-byte digest shows it after one block too.
var h11ShaBreakPortable = []sdForce{{"hash/sha512.mk", "h[0] = h[0] +% a\n", "h[0] = h[0] +% a +% 1\n", 1}}

// h11ShaLens are the one-shot and stream lengths: every length to 300,
// then block edges (111, 112 and 239, 240 are Sum's one- and two-block
// paddings) and longer messages.
var h11ShaLens = append(s8Range(0, 300), 383, 384, 895, 896, 1000, 4096, 4097, 65543)

// h11ShaProgram prints whether simd.Cpu() has avx2 and bmi2, then per
// length of h2KernelEval the compile-time Sha512 of h2ConstText's prefix
// ("E"), then per length of h11ShaLens the one-shot Sha512 ("L"), a
// Sha512State fed in seeded random pieces ("S") and the one-shot Sha384
// ("M") of splitmix64 bytes.
func h11ShaProgram() string {
	var b strings.Builder
	b.WriteString(strings.Replace(h2Prelude, "import std.hash\n", "import std.hash\nimport std.simd\n", 1))
	fmt.Fprintf(&b, "\nconst Text = %q\n", h2ConstText)
	for i, n := range h2KernelEval {
		fmt.Fprintf(&b, "const E%d: [64]u8 = hash.Sha512(Text.AsBytes()[0..%d])\n", i, n)
	}
	fmt.Fprintf(&b, "const Lens: [%d]int = %s\n", len(h11ShaLens), mkIntArray(h11ShaLens))
	fmt.Fprintf(&b, "const Pieces: [%d]int = %s\n", len(h2Pieces), mkIntArray(h2Pieces))
	b.WriteString(`
export Main(sys: &System): WriteError|OutOfMemory!void {
    var out := sys.Stdout()
    has := simd.Cpu().Has("avx2") && simd.Cpu().Has("bmi2")
    try Print(mut out, "cpu {h}\n", (h: has))
`)
	for i := range h2KernelEval {
		fmt.Fprintf(&b, "    try Print(mut out, \"E \")\n    try Hex(mut out, E%d[..])\n    try Print(mut out, \"\\n\")\n", i)
	}
	b.WriteString(`    var r := Rng(s: 1)
    buf := try sys.arena.Slice[u8](65543 + 8)
    Fill(mut r, buf)
    var sr := Rng(s: 7)
    for k in 0..Lens.Len() {
        n := Lens[k]
        off := n % 8
        m := buf[off..off + n]
        try Print(mut out, "L ")
        try Hex(mut out, hash.Sha512(m)[..])
        try Print(mut out, "\n")
        var s := hash.Sha512State.New()
        var i := 0
        for i < n {
            var p := Pieces[Trunc[int](sr.Next() % 16)]
            if p > n - i {
                p = n - i
            }
            s.Bytes(m[i..i + p])
            i += p
        }
        try Print(mut out, "S ")
        try Hex(mut out, s.Sum()[..])
        try Print(mut out, "\nM ")
        try Hex(mut out, hash.Sha384(m)[..])
        try Print(mut out, "\n")
    }
}
`)
	return b.String()
}

// h11ShaWant is the oracle's output for h11ShaProgram after its cpu line.
func h11ShaWant() []string {
	var lines []string
	for _, n := range h2KernelEval {
		lines = append(lines, fmt.Sprintf("E %x", sha512.Sum512([]byte(h2ConstText[:n]))))
	}
	r := hmSplitmix{1}
	buf := make([]byte, 65543+8)
	r.fill(buf)
	sr := hmSplitmix{7}
	for _, n := range h11ShaLens {
		off := n % 8
		m := buf[off : off+n]
		d := sha512.Sum512(m)
		for i := 0; i < n; {
			i += min(h2Pieces[sr.next()%16], n-i)
		}
		lines = append(lines, fmt.Sprintf("L %x", d), fmt.Sprintf("S %x", d), fmt.Sprintf("M %x", sha512.Sum384(m)))
	}
	return lines
}

// TestHash_Sha512PathsMatchGo: hash.Sha512, Sha512State and Sha384, as
// built (the kernel where the CPU has avx2 and bmi2) and with the CPU
// test masked (the portable loop on any machine), at dev and release,
// equal crypto/sha512; the compile-time digests (the evaluator, where
// Cpu() is the baseline, so the portable loop) too.
func TestHash_Sha512PathsMatchGo(t *testing.T) {
	t.Parallel()
	want := h11ShaWant()
	for _, c := range []struct {
		name   string
		forces []sdForce
		flags  []string
	}{
		{"dispatch-dev", nil, nil},
		{"dispatch-release", nil, []string{"--release"}},
		{"masked-dev", h11ShaOff, nil},
		{"masked-release", h11ShaOff, []string{"--release"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			has, got := h2KernelRunProgram(t, h11ShaProgram(), c.forces, c.flags...)
			t.Logf("simd.Cpu() has avx2 and bmi2 = %v", has)
			diffLines(t, strings.Join(got, "\n"), strings.Join(want, "\n"))
		})
	}
}

// TestHash_Sha512KernelRunsEveryBlock: with the portable loop broken, a
// build on a CPU with avx2 and bmi2 still gives crypto/sha512's digests
// at run time (every block of Bytes and Sum, Sha384's too, went through
// the kernel) and wrong ones at compile time (the evaluator's Cpu() is
// the baseline); masked, every digest is wrong (the portable loop ran).
func TestHash_Sha512KernelRunsEveryBlock(t *testing.T) {
	t.Parallel()
	want := h11ShaWant()
	total := h2Total(want)
	for _, flags := range [][]string{nil, {"--release"}} {
		name := "dev"
		if flags != nil {
			name = "release"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, masked := h2KernelRunProgram(t, h11ShaProgram(), append(append([]sdForce{}, h11ShaOff...), h11ShaBreakPortable...), flags...)
			if bad := h2Mismatches(t, masked, want); bad["E"] != total["E"] || bad["L"] != total["L"] || bad["S"] != total["S"] || bad["M"] != total["M"] {
				t.Fatalf("masked with the portable loop broken: %v wrong of %v; want all (the portable loop ran nowhere?)", bad, total)
			}
			has, got := h2KernelRunProgram(t, h11ShaProgram(), h11ShaBreakPortable, flags...)
			bad := h2Mismatches(t, got, want)
			if bad["E"] != total["E"] {
				t.Fatalf("compile time with the portable loop broken: %d of %d wrong; want all (the evaluator's Cpu() is the baseline)", bad["E"], total["E"])
			}
			if !has {
				t.Skip("this CPU lacks avx2 or bmi2: the kernel cannot run; the masked and compile-time halves passed")
			}
			if bad["L"] != 0 || bad["S"] != 0 || bad["M"] != 0 {
				t.Fatalf("CPU has avx2 and bmi2, portable loop broken: %v wrong; want none (a block missed the kernel)", bad)
			}
		})
	}
}

// h11LinuxLines builds src over std with forces for x86_64-linux with
// flags, runs it through wsl.exe, and returns whether its cpu line says
// true and the lines after it.
func h11LinuxLines(t *testing.T, src string, forces []sdForce, flags ...string) (bool, []string) {
	t.Helper()
	exe := buildNative(t, sdFixtures(t, forces), src, append([]string{"--target", "x86_64-linux"}, flags...)...)
	out, errOut, status := runWSL(t, exe, nil)
	if status != 0 {
		t.Fatalf("exit %d\nstdout:\n%.2000s\nstderr:\n%s", status, out, errOut)
	}
	lines := strings.Split(strings.TrimSuffix(strings.ReplaceAll(out, "\r\n", "\n"), "\n"), "\n")
	if len(lines) == 0 || !strings.HasPrefix(lines[0], "cpu ") {
		t.Fatalf("no cpu line:\n%.2000s", out)
	}
	return lines[0] == "cpu true", lines[1:]
}

// TestHash_Linux: the CRC-32 and SHA-512 programs on x86_64-linux, as
// built and masked, at dev and release, equal their oracles; with the
// portable SHA-512 loop broken the run-time digests stay right where the
// CPU has avx2 and bmi2, and with the fold broken the CRC lines from 64
// bytes go wrong where it has pclmul and aes (the kernels ran there too).
// math.Sqrt's bits are checked there as well. Linux leaves run only with
// MK_LINUX=1, under tools/wsl-lock.ps1, with go test -p 1.
func TestHash_Linux(t *testing.T) {
	needWSL(t)
	crcWant, shaWant := h11CrcWant(), h11ShaWant()
	for _, flags := range [][]string{nil, {"--release"}} {
		name := "dev"
		if flags != nil {
			name = "release"
		}
		t.Run(name, func(t *testing.T) {
			for _, c := range []struct {
				name   string
				src    string
				forces []sdForce
				want   []string
			}{
				{"crc-dispatch", h11CrcProgram(), nil, crcWant},
				{"crc-masked", h11CrcProgram(), h11CrcOff, crcWant},
				{"sha-dispatch", h11ShaProgram(), nil, shaWant},
				{"sha-masked", h11ShaProgram(), h11ShaOff, shaWant},
			} {
				has, got := h11LinuxLines(t, c.src, c.forces, flags...)
				t.Logf("%s: cpu %v", c.name, has)
				diffLines(t, strings.Join(got, "\n"), strings.Join(c.want, "\n"))
			}
			has, got := h11LinuxLines(t, h11ShaProgram(), h11ShaBreakPortable, flags...)
			if has {
				if bad := h2Mismatches(t, got, shaWant); bad["L"] != 0 || bad["S"] != 0 || bad["M"] != 0 {
					t.Errorf("sha, portable loop broken: %v wrong; want none (a block missed the kernel)", bad)
				}
			} else {
				t.Log("sha: this CPU lacks avx2 or bmi2 under WSL; the kernel-runs half was not run")
			}
			has, got = h11LinuxLines(t, h11CrcProgram(), h11CrcBreakKernel, flags...)
			if len(got) != len(crcWant) {
				t.Fatalf("crc, fold broken: %d lines, want %d", len(got), len(crcWant))
			}
			if has {
				for k, n := range h11CrcLens {
					i := len(h11CrcEval) + 2*k
					if (got[i] != crcWant[i]) != (n >= 64) {
						t.Errorf("crc length %d with the fold broken: %q against %q; want wrong exactly from 64 bytes", n, got[i], crcWant[i])
					}
				}
			} else {
				t.Log("crc: this CPU lacks pclmul or aes under WSL; the kernel-runs half was not run")
			}
			out, errOut, status := runWSL(t, buildNative(t, nil, m11SqrtProgram(), append([]string{"--target", "x86_64-linux"}, flags...)...), nil)
			if status != 0 {
				t.Fatalf("sqrt: exit %d\n%s\n%s", status, out, errOut)
			}
			diffLines(t, strings.TrimSuffix(strings.ReplaceAll(out, "\r\n", "\n"), "\n"), m11SqrtWant())
		})
	}
}
