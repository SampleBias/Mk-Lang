// Why: text.ToUtf16 converts runs of two- and three-byte scalars with vector
// steps (Utf8Two16 in std/text/utf.mk, std.simd.kernel's Utf8To16Step)
// whose edges are easy to get wrong.
// What: TestStd_ToUtf16VectorSteps, text.ToUtf16 against unicode/utf16.Encode.
// How: texts built to cross every step's edges, compared unit for unit by an
// FNV-1a digest, then text.FromUtf16 back to the input, dev and release.

package std

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf16"
)

func std10Utf16Digest(units []uint16) uint64 {
	h := uint64(0xCBF29CE484222325)
	for _, u := range units {
		h = (h ^ uint64(u)) * 0x100000001B3
	}
	return h
}

func std10Utf16Inputs() []string {
	two := []string{"α", "ж", "ש", "ق", "é", "ß", "߿", "\u0080"}
	three := []string{"中", "あ", "ह", "ࠀ", "�", "퟿", "", "￿", "€"}
	breaks := []string{"a", " ", "é", "中", "😀", "\U0010FFFF"}
	var in []string
	for _, set := range [][]string{two, three} {
		for n := 0; n <= 40; n++ {
			var run strings.Builder
			for k := 0; k < n; k++ {
				run.WriteString(set[k%len(set)])
			}
			r := run.String()
			in = append(in, r)
			rs := []rune(r)
			for p := 0; p <= len(rs); p++ {
				for _, b := range breaks {
					in = append(in, string(rs[:p])+b+string(rs[p:]))
				}
			}
			for off := 0; off < 16; off++ {
				in = append(in, strings.Repeat("x", off)+r+"yz")
			}
		}
	}
	// Every sequence of scalar lengths over the 13 bytes Utf8To16Step reads.
	in = append(in, utf16StepSeqs(&encRng{s: 53})...)
	rnd := rand.New(rand.NewSource(45))
	pool := append(append(append([]string{}, two...), three...), breaks...)
	for k := 0; k < 4000; k++ {
		n := rnd.Intn(3000)
		var b strings.Builder
		for b.Len() < n {
			// Long runs of one class, so the vector steps fire often.
			set := pool
			switch rnd.Intn(3) {
			case 0:
				set = two
			case 1:
				set = three
			}
			for m := rnd.Intn(30); m >= 0; m-- {
				b.WriteString(set[rnd.Intn(len(set))])
			}
		}
		in = append(in, b.String())
	}
	return in
}

const std10Utf16Program = `import std.text

export Main(sys: &System): ReadError|WriteError|OutOfMemory|Utf8Error!void {
    var w := sys.Stdout()
    var inp := sys.Stdin()
    var ar := try Arena.New(sys.heap)
    defer ar.Free()
    for true {
        lo := try inp.Line(&ar)
        line := lo else {
            break
        }
        s := try str.From(try Unhex(&ar, line))
        u := try text.ToUtf16(&ar, s)
        var h: u64 = 0xCBF29CE484222325
        for x in u {
            h = (h ^ Cast[u64](x)) *% 0x100000001B3
        }
        back := try text.FromUtf16(&ar, u)
        try Print(mut w, "{n} {h} {r}\n", (n: u.Len(), h, r: back == s))
        ar.Reset()
    }
    try w.Flush()
}

Unhex(a: &Arena, h: str): OutOfMemory![]u8 from a {
    b := h.AsBytes()
    out := try a.Slice[u8](b.Len() / 2)
    for i in 0..b.Len() / 2 {
        out[i] = Nib(b[2 * i]) * 16 + Nib(b[2 * i + 1])
    }
    return out[..]
}

Nib(c: u8): u8 {
    if c >= 0x61 {
        return c - 0x61 + 10
    }
    return c - 0x30
}
`

// TestStd_ToUtf16VectorSteps: text.ToUtf16 against unicode/utf16.Encode,
// unit for unit (an FNV-1a digest of the units and their count), and
// text.FromUtf16 of the result back to the input, dev and release. The texts
// cross every step's edges: runs of two-byte and of three-byte scalars of
// every length from 0 to 40, each broken at every position by an ASCII byte,
// another length, or a four-byte scalar; the same runs at every start offset
// 0 to 15 behind ASCII; runs that end within the last 16 bytes of the text,
// where the steps must not run; every sequence of scalar lengths over the
// 13 bytes Utf8To16Step reads (utf16StepSeqs); and 4,000 random mixes of
// every length to 3,000 bytes.
func TestStd_ToUtf16VectorSteps(t *testing.T) {
	in := std10Utf16Inputs()
	var feed bytes.Buffer
	for _, s := range in {
		fmt.Fprintf(&feed, "%x\n", s)
	}
	for _, prof := range []string{"dev", "release"} {
		t.Run(prof, func(t *testing.T) {
			var flags []string
			if prof == "release" {
				flags = append(flags, "--release")
			}
			exe := buildNative(t, nil, std10Utf16Program, flags...)
			inFile := filepath.Join(t.TempDir(), "in.txt")
			if err := os.WriteFile(inFile, feed.Bytes(), 0o644); err != nil {
				t.Fatal(err)
			}
			f, err := os.Open(inFile)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			ctx, cancel := context.WithTimeout(context.Background(), std10RunTimeout)
			defer cancel()
			cmd := exec.CommandContext(ctx, exe)
			cmd.Stdin = f
			var out, errb bytes.Buffer
			cmd.Stdout, cmd.Stderr = &out, &errb
			if err := cmd.Run(); err != nil {
				t.Fatalf("%v\n%s", err, errb.String())
			}
			got := strings.Split(strings.TrimSpace(out.String()), "\n")
			if len(got) != len(in) {
				t.Fatalf("%d answers for %d inputs", len(got), len(in))
			}
			bad := 0
			for i, s := range in {
				u := utf16.Encode([]rune(s))
				want := fmt.Sprintf("%d %d true", len(u), std10Utf16Digest(u))
				if strings.TrimRight(got[i], "\r") != want {
					bad++
					if bad <= 20 {
						t.Errorf("%q: got %q, want %q", s, got[i], want)
					}
				}
			}
			if bad > 0 {
				t.Fatalf("%d of %d inputs differ", bad, len(in))
			}
			t.Logf("%s: %d texts equal to unicode/utf16, and back", prof, len(in))
		})
	}
}
