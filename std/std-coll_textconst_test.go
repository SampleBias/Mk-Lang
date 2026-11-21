// Why: a TextMap keeps short keys inline only natively; a compile-time
// evaluation keeps every key copied, so a map built in a const must give
// the same results and order as one built at run time.
//
// What: TestStdColl_TextMapConstMatchesRun.
//
// How: one program builds and walks a TextMap in four consts (fixed and
// keyed, short keys only and with keys of 16 bytes or more, NULs, a
// removal) and again at run time, and prints whether each pair agrees.

package std

import (
	"strings"
	"testing"
)

const textConstProgram = `// A TextMap built and walked in the evaluator and at run time.

TcMix(h: u64, x: u64): u64 {
    return (h ^ x) *% 1099511628211
}

TcWalk(m: &TextMap[int]): u64 {
    var h: u64 = 14695981039346656037
    for e in m.Iter() {
        h = TcMix(h, Trunc[u64](e.key.Len()))
        b := e.key.AsBytes()
        for j in 0..b.Len() {
            h = TcMix(h, u64(b[j]))
        }
        h = TcMix(h, Trunc[u64](*e.value))
    }
    return h
}

TcProbe(ar: &Arena, keyed: bool, long: bool): u64 {
    a := ar.Alloc()
    var m := if keyed { TextMap[int].NewKeyed(a, HashKey.Of(1, 2)) } else { TextMap[int].New(a) }
    keys: [6]str = ["", "a", "a\x00", "fifteen bytes!!", "\x00\x00", "fifteen bytes!?"]
    for i in 0..6 {
        m.Set(a, keys[i], i) else {
            m.Free(a)
            return 1
        }
    }
    if long {
        m.Set(a, "sixteen bytes!!!", 16) else {
            m.Free(a)
            return 2
        }
        m.Set(a, "a much longer key than sixteen bytes", 36) else {
            m.Free(a)
            return 3
        }
    }
    _ = m.Remove(a, "a")
    var h := TcWalk(&m)
    if v := m.Get("fifteen bytes!?") {
        h = TcMix(h, Trunc[u64](*v))
    }
    if v := m.Get("a much longer key than sixteen bytes") {
        h = TcMix(h, Trunc[u64](*v))
    }
    h = TcMix(h, Trunc[u64](m.Len()))
    m.Free(a)
    return h
}

const Short: u64 = TcProbe(EvalArena(), false, false)
const ShortKeyed: u64 = TcProbe(EvalArena(), true, false)
const Long: u64 = TcProbe(EvalArena(), false, true)
const LongKeyed: u64 = TcProbe(EvalArena(), true, true)

export Main(sys: &System): WriteError!void {
    var out := sys.Stdout()
    try Print(mut out, "short {b}\n", (b: Short == TcProbe(sys.arena, false, false) && Short > 3))
    try Print(mut out, "short-keyed {b}\n", (b: ShortKeyed == TcProbe(sys.arena, true, false) && ShortKeyed > 3))
    try Print(mut out, "long {b}\n", (b: Long == TcProbe(sys.arena, false, true) && Long > 3))
    try Print(mut out, "long-keyed {b}\n", (b: LongKeyed == TcProbe(sys.arena, true, true) && LongKeyed > 3))
}
`

// TestStdColl_TextMapConstMatchesRun: a TextMap built in a const, whose
// keys are all copied, gives the digest of its walk and lookups that the
// same map built at run time gives, with short and long keys, fixed and
// keyed, at dev and release.
func TestStdColl_TextMapConstMatchesRun(t *testing.T) {
	t.Parallel()
	want := "short true\nshort-keyed true\nlong true\nlong-keyed true"
	for _, flags := range [][]string{nil, {"--release"}} {
		got := strings.TrimSpace(collExec(t, collBuild(t, textConstProgram, flags...)))
		if got != want {
			t.Errorf("flags %v:\n%s\nwant:\n%s", flags, got, want)
		}
	}
}
