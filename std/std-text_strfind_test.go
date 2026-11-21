// Why: FindFrom's substring search has four paths (the 16- and 32-lane pair
// kernels, their one-by-one and overlapping tails, and Two-Way past the
// verification budget) that must all find what a naive search finds.
// What: TestSTDTEXT_StrFindDiff, each path against a naive search in Mk,
// dev and release.
// How: seeded haystacks over 1 to 4 letters (many candidates, periodic
// needles that trip the budget), 26 letters and all 256 byte values, at
// every offset of a 64-byte block, needles of 0 to 70 bytes planted near
// the end or drawn at random, from every start (every 7th on long
// haystacks, plus the last 80). The kernels are called directly, so the
// 16-lane one runs on long haystacks on any CPU; the counts of budget trips
// and of matches show that every path ran.

package std

import (
	"fmt"
	"strings"
	"testing"
)

const sfFixture = `// Test fixture (std/std-text_strfind_test.go): FindFrom's paths against a
// naive search.
import std.simd
` + svRngMk + `
SfNaive(b: []u8, p: []u8, from: int): int {
    m := p.Len()
    var i := from
    for i + m <= b.Len() {
        var k := 0
        for k < m && b[i + k] == p[k] {
            k += 1
        }
        if k == m {
            return i
        }
        i += 1
    }
    return -1
}

// SfDecode finishes a kernel result as FindFrom does: -2 - i goes on with
// Two-Way from i.
SfDecode(b: []u8, p: []u8, r: int): int {
    if r >= -1 {
        return r
    }
    if k := TwoWay(b, p, -2 - r) {
        return k
    }
    return -1
}

export SfRun(sys: &System, seed: u64): OutOfMemory|Utf8Error|WriteError!void {
    var out := sys.Stdout()
    var rng := SvRng(x: seed)
    avx2 := simd.Cpu().Has("avx2")
    var buf := try sys.arena.Slice[u8](6200)
    var pb := try sys.arena.Slice[u8](80)
    var cases := 0
    var found := 0
    var bad := 0
    var trips := 0
    var wide := 0
    for c in 0..2400 {
        var n := SvR(mut rng, 300)
        if c % 40 == 0 {
            n = 2000 + SvR(mut rng, 4000)
        }
        off := SvR(mut rng, 64)
        mode := c % 6
        letters := 1 + SvR(mut rng, 4)
        for i in 0..n {
            var x := 0x61 + SvR(mut rng, letters)
            if mode == 3 {
                x = 0x61 + SvR(mut rng, 26)
            } else if mode == 4 {
                x = SvR(mut rng, 256)
            } else if mode == 5 && SvR(mut rng, 64) != 0 {
                x = 0x61
            }
            buf[off + i] = Trunc[u8](x)
        }
        m := SvR(mut rng, 71)
        if n >= m && SvR(mut rng, 3) != 0 {
            // Planted: often within the last 80 bytes.
            var at := SvR(mut rng, n - m + 1)
            if SvR(mut rng, 2) == 0 {
                at = n - m - SvR(mut rng, IntMin(80, n - m + 1))
                if at < 0 {
                    at = 0
                }
            }
            for k in 0..m {
                pb[k] = buf[off + at + k]
            }
            if m > 0 && SvR(mut rng, 4) == 0 {
                // A near miss: one byte changed.
                k := SvR(mut rng, m)
                pb[k] = pb[k] ^ Trunc[u8](1 + SvR(mut rng, 2))
            }
        } else {
            for k in 0..m {
                var x := 0x61 + SvR(mut rng, letters)
                if mode == 4 {
                    x = SvR(mut rng, 256)
                }
                pb[k] = Trunc[u8](x)
            }
        }
        if mode == 5 && m >= 2 {
            // aaa...ab and baa...a over a text of a's.
            for k in 0..m {
                pb[k] = 0x61
            }
            if c % 12 == 5 {
                pb[m - 1] = 0x62
            } else {
                pb[0] = 0x62
            }
        }
        b := buf[off..off + n]
        p := pb[0..m]
        var step := 1
        if n > 400 {
            step = 7
        }
        var from := 0
        for from <= n {
            cases += 1
            want := SfNaive(b, p, from)
            if want >= 0 {
                found += 1
            }
            if mode != 4 {
                s := try str.From(b)
                t := try str.From(p)
                var got := -1
                if k := FindFrom(s, t, from) {
                    got = k
                }
                if got != want {
                    bad += 1
                }
            }
            if m >= 2 && m <= n - from {
                r := PairFindN[16](b, p, FindPair(p), from)
                if r < -1 {
                    trips += 1
                }
                if SfDecode(b, p, r) != want {
                    bad += 1
                }
                if avx2 {
                    wide += 1
                    unsafe "Cpu() reports avx2, which PairFindAvx2's clause names" {
                        if SfDecode(b, p, PairFindAvx2(b, p, FindPair(p), from)) != want {
                            bad += 1
                        }
                    }
                }
                var tw := -1
                if k := TwoWay(b, p, from) {
                    tw = k
                }
                if tw != want {
                    bad += 1
                }
            }
            if from < n - 80 && from + step > n - 80 {
                from = n - 80
            } else if from >= n - 80 {
                from += 1
            } else {
                from += step
            }
        }
    }
    try Print(mut out, "strfind {cases} found {found} bad {bad} trips {trips} wide {wide}\n", (cases, found, bad, trips, wide))
    try out.Flush()
}
`

// sfModule carries the fixture out of std.core, which a program cannot
// import.
const sfModule = `import std.core

export Run(sys: &System): OutOfMemory|Utf8Error|WriteError!void {
    try core.SfRun(sys, 0x9E3779B97F4A7C15)
}
`

const sfMain = `import std.strfindfix

export Main(sys: &System): OutOfMemory|Utf8Error|WriteError!void {
    try strfindfix.Run(sys)
}
`

// TestSTDTEXT_StrFindDiff is FindFrom, both pair kernels and Two-Way
// against a naive search, dev and release.
func TestSTDTEXT_StrFindDiff(t *testing.T) {
	t.Parallel()
	fx := map[string]string{"core/zz_strfind.mk": sfFixture, "strfindfix/strfindfix.mk": sfModule}
	for _, flags := range c2Flags {
		t.Run(c2FlagName(flags), func(t *testing.T) {
			t.Parallel()
			r := runNative(t, buildNative(t, fx, sfMain, flags...), "")
			if r.code != 0 {
				t.Fatalf("exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
			}
			var cases, found, bad, trips, wide int
			out := strings.ReplaceAll(r.stdout, "\r\n", "\n")
			if _, err := fmt.Sscanf(out, "strfind %d found %d bad %d trips %d wide %d\n", &cases, &found, &bad, &trips, &wide); err != nil {
				t.Fatalf("output: %v\n%s", err, out)
			}
			t.Logf("strfind %d cases, %d found, %d budget trips, %d avx2 runs", cases, found, trips, wide)
			if bad != 0 || cases < 100000 || found < cases/10 || found > cases-cases/10 || trips < 100 {
				t.Fatalf("strfind: %d cases, %d found, %d differ, %d budget trips", cases, found, bad, trips)
			}
		})
	}
}
