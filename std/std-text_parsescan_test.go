// Why: str.ParseInt and str.ParseFloat's fraction read digits eight at a time and
// count nothing per byte while at most 19 digit bytes fit a u64, and fall
// back to a digit-by-digit reader past that; each boundary of those paths
// is a place for an off-by-one.
// What: TestParseScanMatchesGo, ParseFloat and ParseInt on texts built
// around those boundaries, against Go's strconv, natively and at compile
// time.
// How: digit runs of 0 to 24 bytes on each side of the point with leading
// and trailing zeros, a foreign byte at each offset of an 8-byte word,
// exponents, and integers at the i64 and u64 limits. strconv is exact on
// these texts (all are short and their exponents modest).

package std

import (
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"testing"
)

// scanForeign are bytes (and two UTF-8 scalars) that end a digit run or
// must be refused: just below and above the digits, the point, exponent
// letters, signs, and multibyte text.
var scanForeign = []string{"/", ":", ".", "e", "E", "+", "-", "_", " ", "x", "\x00", "é", "€"}

// scanDigits is n random digits; mode makes the first or the last half
// zeros, which leave the value unchanged but count as digit bytes.
func scanDigits(r *rand.Rand, n int) string {
	var b strings.Builder
	mode := r.Intn(4)
	for i := 0; i < n; i++ {
		c := byte('0' + r.Intn(10))
		if mode == 1 && i < n/2 || mode == 2 && i >= n/2 {
			c = '0'
		}
		b.WriteByte(c)
	}
	return b.String()
}

// scanFloatCorpus is n float texts around the scanner's boundaries.
func scanFloatCorpus(r *rand.Rand, n int) []string {
	var out []string
	for len(out) < n {
		var s strings.Builder
		switch r.Intn(3) {
		case 1:
			s.WriteByte('-')
		case 2:
			if r.Intn(4) == 0 {
				s.WriteByte('+')
			}
		}
		s.WriteString(scanDigits(r, r.Intn(25)))
		if r.Intn(4) != 0 {
			s.WriteByte('.')
			s.WriteString(scanDigits(r, r.Intn(25)))
		}
		if r.Intn(3) == 0 {
			s.WriteString("eE"[r.Intn(2):][:1])
			if k := r.Intn(3); k > 0 {
				s.WriteString("+-"[k-1 : k])
			}
			s.WriteString(strconv.Itoa(r.Intn(400)))
		}
		t := s.String()
		if r.Intn(4) == 0 && t != "" {
			// A foreign byte at any place, often inside a word of digits.
			k := r.Intn(len(t))
			t = t[:k] + scanForeign[r.Intn(len(scanForeign))] + t[k+1:]
		}
		out = append(out, t)
	}
	// A foreign byte at each offset of the first and second words after a
	// point.
	for _, f := range scanForeign {
		for k := 0; k < 17; k++ {
			d := []byte("0.12345678901234567")
			out = append(out, string(d[:2+k])+f+string(d[3+k:]), "123"+string(d[1:2+k])+f)
		}
	}
	out = append(out, "9999999999999999999", "9999999999999999999.", ".9999999999999999999",
		"18446744073709551615", "18446744073709551616", "1844674407370955161.6", "0.1844674407370955161",
		"00000000000000000001", "1.0000000000000000000", "123456.78901234567", "99999999.99999999",
		"0.00000000", "00000000.00000000", "1.2345678e-5", "12345678.", ".12345678", "1234567812345678",
		".1234567812345678", "1.12345678123456781", "1.123456781234567812")
	return out
}

// scanIntCorpus is n integer texts around ParseInt's boundaries.
func scanIntCorpus(r *rand.Rand, n int) []string {
	out := []string{"9223372036854775807", "9223372036854775808", "-9223372036854775808",
		"-9223372036854775809", "+9223372036854775807", "09223372036854775807", "-09223372036854775808",
		"0009223372036854775807", "9999999999999999999", "18446744073709551615", "18446744073709551616",
		"-18446744073709551616", "10000000000000000000", "1000000000000000000", "00000000000000000000",
		"-0000000000000000000000000", "12345678", "123456781", "1234567812345678", "+", "-", ""}
	for len(out) < n {
		var s strings.Builder
		if k := r.Intn(4); k > 1 {
			s.WriteString("+-"[k-2 : k-1])
		}
		if r.Intn(4) == 0 {
			s.WriteString(strings.Repeat("0", r.Intn(6)))
		}
		if r.Intn(2) == 0 {
			s.WriteString(strconv.FormatUint(r.Uint64()>>uint(r.Intn(64)), 10))
		} else {
			s.WriteString(scanDigits(r, 1+r.Intn(24)))
		}
		t := s.String()
		if r.Intn(5) == 0 {
			k := r.Intn(len(t))
			t = t[:k] + scanForeign[r.Intn(len(scanForeign))] + t[k+1:]
		}
		out = append(out, t)
	}
	return out
}

// goParseInt is ParseInt's oracle: strconv's base-10 grammar is the same
// ([+-] then digits).
func goParseInt(s string) (int64, bool) {
	v, err := strconv.ParseInt(s, 10, 64)
	return v, err == nil
}

// intFixture checks ParseInt over texts at compile time.
func intFixture(name string, texts []string) string {
	var in, ok, vals []string
	for _, s := range texts {
		v, good := goParseInt(s)
		in = append(in, mkString(s))
		ok = append(ok, strconv.FormatBool(good))
		vals = append(vals, fmt.Sprintf("%d", uint64(v)))
	}
	n := len(texts)
	return fmt.Sprintf(`const %[1]sIn: [%[2]d]str = [%[3]s]
const %[1]sOk: [%[2]d]bool = [%[4]s]
const %[1]sVal: [%[2]d]u64 = [%[5]s]

%[1]sOne(k: int): int {
    strs := &%[1]sIn
    ok := &%[1]sOk
    vals := &%[1]sVal
    v := strs[k].ParseInt() else {
        if ok[k] {
            return 1
        }
        return 0
    }
    if !ok[k] || Trunc[u64](v) != vals[k] {
        return 1
    }
    return 0
}

%[1]sCheck(): int {
    var bad := 0
    for k in 0..%[2]d {
        bad += %[1]sOne(k)
    }
    return bad
}

const %[1]sCheckOk = [0; 1][%[1]sCheck()]
`, name, n, strings.Join(in, ", "), strings.Join(ok, ", "), strings.Join(vals, ", "))
}

// TestParseScanMatchesGo checks ParseFloat and ParseInt against strconv:
// 400 random texts of each at compile time and 300,000 floats and 100,000
// integers natively at release, plus the fixed edge texts, one result per line of stdin.
func TestParseScanMatchesGo(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewSource(11))
	var cases []parseCase
	for _, s := range scanFloatCorpus(r, 400) {
		cases = append(cases, goParse(s))
	}
	expectClean(t, "s_scan_float", parseFixture("Sf", cases))
	expectClean(t, "s_scan_int", intFixture("Si", scanIntCorpus(r, 400)))

	floats := scanFloatCorpus(rand.New(rand.NewSource(12)), 300000)
	ints := scanIntCorpus(rand.New(rand.NewSource(13)), 100000)
	var in, want strings.Builder
	for _, s := range floats {
		in.WriteString("f" + s + "\n")
		if c := goParse(s); c.ok {
			fmt.Fprintf(&want, "%x\n", c.bits)
		} else {
			want.WriteString("err\n")
		}
	}
	for _, s := range ints {
		in.WriteString("i" + s + "\n")
		if v, ok := goParseInt(s); ok {
			fmt.Fprintf(&want, "%d\n", v)
		} else {
			want.WriteString("err\n")
		}
	}
	src := `import std.zzbits

export Main(sys: &System): ReadError|OutOfMemory|WriteError!void {
    var out := sys.Stdout()
    var inp := sys.Stdin()
    for {
        l := try inp.Line(sys.arena)
        s := l else {
            break
        }
        t := s[1..]
        if s.Byte(0) == 0x69 {
            v := t.ParseInt() else {
                try Print(mut out, "err\n", ())
                continue
            }
            try Print(mut out, "{v}\n", (v))
            continue
        }
        v := t.ParseFloat() else {
            try Print(mut out, "err\n", ())
            continue
        }
        b := zzbits.F64Bits(v)
        try Print(mut out, "{b:x}\n", (b))
    }
    try out.Flush()
}
`
	res := runNative(t, buildWithModule(t, "zzbits/bits.mk", bitsModule, src, "--release"), in.String())
	if res.code != 0 {
		t.Fatalf("exit %d\n%s", res.code, res.stderr)
	}
	compareLines(t, res.stdout, want.String())
}
