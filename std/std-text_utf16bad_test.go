// Why: text.ToUtf16 and the UTF-16 encoders assume valid UTF-8, but unsafe
// code can make a str that is not (raw.Str); their vector steps must then
// still finish, with garbage or a bounds panic as the scalar loops give,
// and never loop.
// What: TestSTDTEXT_Utf16InvalidEnds.
// How: a std.core fixture builds the invalid str, a bridge module runs
// ToUtf16, Utf16Le.Encode or Utf16Be.Encode over it, and each run must
// exit (0, or 101 for a panic) within a time bound.

package std

import (
	"context"
	"fmt"
	"os/exec"
	"testing"
	"time"
)

const ubCore = `import std.raw

// UbBad is 40 bytes of b as a str, valid UTF-8 or not.
export UbBad(b: []u8): str from b {
    if b.Len() < 40 {
        raw.Fail(2, "UbBad: short")
    }
    unsafe "a test fixture: a str over 40 bytes of b, invalid UTF-8 on purpose" {
        return raw.Str(raw.Data(b), 40)
    }
}
`

const ubModule = `import std.core
import std.text
import std.encoding

// Run converts case k with ToUtf16 (which 0), Utf16Le.Encode (1) or
// Utf16Be.Encode (2): a run of continuation bytes, a three-byte scalar with
// an extra continuation, a truncated four-byte scalar, a lone lead,
// continuations inside ASCII, a four-byte lead before bytes above F4; then
// ASCII to 40 bytes.
export Run(sys: &System, k: int, which: int): OutOfMemory|encoding.Error|WriteError!void {
    var buf := try sys.arena.Slice[u8](64)
    for j in 0..64 {
        buf[j] = 0x61
    }
    if k == 0 {
        for j in 0..20 {
            buf[j] = 0x80
        }
    } else if k == 1 {
        buf[0] = 0xE4
        buf[1] = 0xB8
        buf[2] = 0xAD
        buf[3] = 0xAD
    } else if k == 2 {
        buf[0] = 0xF0
        buf[1] = 0x9F
        buf[2] = 0x98
    } else if k == 3 {
        buf[0] = 0xC3
    } else if k == 4 {
        for j in 5..9 {
            buf[j] = 0x80
        }
    } else if k == 5 {
        buf[3] = 0xF0
        buf[4] = 0x80
        for j in 5..20 {
            buf[j] = 0xF5
        }
    }
    s := core.UbBad(buf[..])
    var a := try Arena.New(sys.heap)
    defer a.Free()
    var n := 0
    if which == 0 {
        n = (try text.ToUtf16(&a, s)).Len()
    } else if which == 1 {
        n = (try encoding.Utf16Le.Encode(&a, s)).Len()
    } else {
        n = (try encoding.Utf16Be.Encode(&a, s)).Len()
    }
    try sys.Print("case {k} {which} {n}\n", (k, which, n))
}
`

const ubMain = `import std.encoding
import std.utf16bad

export Main(sys: &System): OutOfMemory|ParseError|encoding.Error|WriteError!void {
    try utf16bad.Run(sys, Cast[int](try sys.args[1].ParseInt()), Cast[int](try sys.args[2].ParseInt()))
}
`

// ubOverride replaces std files for a red run of the test; empty normally.
var ubOverride = map[string]string{}

// TestSTDTEXT_Utf16InvalidEnds: over each invalid str, ToUtf16 and the
// UTF-16LE and BE encoders each end (exit 0, or 101 for a bounds panic)
// within 20 seconds, dev and release.
func TestSTDTEXT_Utf16InvalidEnds(t *testing.T) {
	t.Parallel()
	fx := map[string]string{"core/zz_utf16bad.mk": ubCore, "utf16bad/utf16bad.mk": ubModule}
	for k, v := range ubOverride {
		fx[k] = v
	}
	for _, flags := range c2Flags {
		t.Run(c2FlagName(flags), func(t *testing.T) {
			t.Parallel()
			exe := buildNative(t, fx, ubMain, flags...)
			for c := 0; c < 18; c++ {
				k, which := c/3, c%3
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				out, err := exec.CommandContext(ctx, exe, fmt.Sprint(k), fmt.Sprint(which)).CombinedOutput()
				timedOut := ctx.Err() != nil
				cancel()
				if timedOut {
					t.Errorf("case %d/%d: still running after 20 s (a vector step that never advances)", k, which)
					continue
				}
				code := 0
				if ee, ok := err.(*exec.ExitError); ok {
					code = ee.ExitCode()
				} else if err != nil {
					t.Fatal(err)
				}
				if code != 0 && code != 101 {
					t.Errorf("case %d/%d: exit %d\n%s", k, which, code, out)
				}
				t.Logf("case %d/%d: exit %d: %.120s", k, which, code, out)
			}
		})
	}
}
