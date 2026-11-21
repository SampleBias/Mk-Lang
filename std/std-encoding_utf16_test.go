// Why: utf16.mk decodes UTF-16 by vector blocks (eight units with no surrogate, written as UTF-8 by
// two 16-byte shuffles while 32 output bytes remain), and encodes it by vector steps too
// (std.simd.kernel's Utf8To16Step);
// both paths must equal the scalar ones.
// What: every mix of one-, two- and three-byte scalars in blocks, every input length, and Decoder.Push
// split at every byte (TestStdEncoding_Utf16Blocks); every mix of one- to four-byte scalars encoded
// (TestStdEncoding_Utf16EncodeBlocks).
// How: the WHATWG model is the oracle, as in TestStdEncoding_DecodeAdversarial.

package std

import (
	"strings"
	"testing"
	"unicode/utf16"
)

// encUnits is n UTF-16 units drawn by kind: "1" ASCII, "2" U+0080 to
// U+07FF, "3" U+0800 to U+FFFF outside the surrogates, "s" a surrogate
// pair (two units), "l" a lone surrogate.
func encUnits(r *encRng, n int, kinds string) []uint16 {
	var u []uint16
	for len(u) < n {
		switch kinds[r.intn(len(kinds))] {
		case '1':
			u = append(u, uint16(r.intn(0x80)))
		case '2':
			u = append(u, uint16(0x80+r.intn(0x780)))
		case '3':
			c := 0x800 + r.intn(0xF800)
			if c >= 0xD800 && c <= 0xDFFF {
				c = 0xFFFF - (c - 0xD800)
			}
			u = append(u, uint16(c))
		case 's':
			a, b := utf16.EncodeRune(rune(0x10000 + r.intn(0x100000)))
			u = append(u, uint16(a), uint16(b))
		case 'l':
			u = append(u, uint16(0xD800+r.intn(0x800)))
		}
	}
	return u
}

// encUnitBytes is u as UTF-16LE or BE bytes.
func encUnitBytes(u []uint16, be bool) []byte {
	b := make([]byte, 0, 2*len(u))
	for _, x := range u {
		if be {
			b = append(b, byte(x>>8), byte(x))
		} else {
			b = append(b, byte(x), byte(x>>8))
		}
	}
	return b
}

func TestStdEncoding_Utf16Blocks(t *testing.T) {
	models := encModels(t)
	byName := map[string]encModel{}
	for _, m := range models {
		byName[m.name] = m
	}
	r := &encRng{s: 31}
	var cases []encCase
	for _, name := range []string{"UTF-16LE", "UTF-16BE"} {
		m := byName[name]
		be := name == "UTF-16BE"
		// Every length from 0 to 80 units, so the input ends at every
		// offset of a block and the output's last 32 bytes fall anywhere
		// in one, for all-three-byte text (the most output a block makes),
		// all-two-byte text, and mixes.
		for _, kinds := range []string{"3", "2", "123", "13", "12", "1233", "123s", "123l"} {
			for n := 0; n <= 80; n++ {
				b := encUnitBytes(encUnits(r, n, kinds), be)
				cases = append(cases, decCase(m, b), decsCase(m, b))
				if n%7 == 0 {
					cases = append(cases, pushCase(m, b, encCuts(r, len(b), 1+r.intn(40))))
				}
			}
		}
		// Every mix of the three kinds in one block: the 3^8 patterns of
		// eight units, sampled, each after an ASCII prefix of 0 to 7 units
		// so the block starts at each unit offset, with text after it.
		for k := 0; k < 600; k++ {
			pre := encUnits(r, r.intn(8), "1")
			block := encUnits(r, 8, "123")
			post := encUnits(r, r.intn(40), "123")
			u := append(append(pre, block...), post...)
			b := encUnitBytes(u, be)
			cases = append(cases, decCase(m, b))
			if k%10 == 0 {
				cases = append(cases, pushCase(m, b, encCuts(r, len(b), 1+r.intn(17))))
			}
		}
		// An odd byte at the end, and long mixed text split at every byte
		// count up to 33.
		for n := 30; n < 40; n++ {
			b := encUnitBytes(encUnits(r, n, "123"), be)
			cases = append(cases, decCase(m, append(b, 0x41)), decsCase(m, append(b, 0x41)))
		}
		long := encUnitBytes(encUnits(r, 3000, "1233s"), be)
		for c := 1; c <= 33; c++ {
			var cuts []int
			for left := len(long); left > 0; left -= c {
				cuts = append(cuts, min(c, left))
			}
			cases = append(cases, pushCase(m, long, cuts))
		}
		cases = append(cases, decCase(m, long), decsCase(m, long))
	}
	encRunAll(t, cases)
}

// encScalars is n scalars as UTF-8, drawn by kind: "1" ASCII, "2" U+0080 to
// U+07FF, "3" U+0800 to U+FFFF outside the surrogates, "4" U+10000 and up.
func encScalars(r *encRng, n int, kinds string) string {
	var sb strings.Builder
	for k := 0; k < n; k++ {
		switch kinds[r.intn(len(kinds))] {
		case '1':
			sb.WriteByte(byte(r.intn(0x80)))
		case '2':
			sb.WriteRune(rune(0x80 + r.intn(0x780)))
		case '3':
			c := 0x800 + r.intn(0xF800)
			if c >= 0xD800 && c <= 0xDFFF {
				c = 0xFFFF - (c - 0xD800)
			}
			sb.WriteRune(rune(c))
		case '4':
			sb.WriteRune(rune(0x10000 + r.intn(0x100000)))
		}
	}
	return sb.String()
}

// TestStdEncoding_Utf16EncodeBlocks: Encode to UTF-16LE and BE writes
// sixteen ASCII bytes, or up to twelve bytes of any mix of one- to
// three-byte scalars, a vector step (std.simd.kernel's Utf8To16Step), and that path
// must equal the scalar one. Every length from 0 to 60 scalars of each mix,
// so the last 16 bytes of input and of output fall at every offset of a
// step, every sequence of scalar lengths that covers a step's 13 bytes
// (utf16StepSeqs), and 3,000 long texts of every mix, against the WHATWG
// model.
func TestStdEncoding_Utf16EncodeBlocks(t *testing.T) {
	models := encModels(t)
	byName := map[string]encModel{}
	for _, m := range models {
		byName[m.name] = m
	}
	r := &encRng{s: 47}
	var cases []encCase
	mixes := []string{"1", "2", "3", "12", "13", "23", "123", "1123", "1234", "124", "34", "11112", "11113"}
	for _, name := range []string{"UTF-16LE", "UTF-16BE"} {
		m := byName[name]
		for _, kinds := range mixes {
			for n := 0; n <= 60; n++ {
				cases = append(cases, encTextCase(m, encScalars(r, n, kinds)))
			}
		}
		for _, s := range utf16StepSeqs(r) {
			cases = append(cases, encTextCase(m, s))
		}
		for k := 0; k < 1500; k++ {
			kinds := mixes[r.intn(len(mixes))]
			cases = append(cases, encTextCase(m, encScalars(r, r.intn(16), "1")+encScalars(r, 16+r.intn(200), kinds)))
		}
	}
	encRunAll(t, cases)
}

// utf16StepSeqs is every sequence of scalar lengths 1 to 4 that covers the
// first 13 bytes of a vector step (the step reads the leads of bytes 0 to
// 12), each as text: 16 ASCII bytes, the scalars (drawn by r), 16 ASCII
// bytes. So every 12-bit mask a valid text can show is met.
func utf16StepSeqs(r *encRng) []string {
	kinds := "1234"
	var out []string
	var walk func(seq []byte, sum int)
	walk = func(seq []byte, sum int) {
		if sum >= 13 {
			var sb strings.Builder
			sb.WriteString("0123456789abcdef")
			for _, k := range seq {
				sb.WriteString(encScalars(r, 1, string(k)))
			}
			sb.WriteString("ghijklmnopqrstuv")
			out = append(out, sb.String())
			return
		}
		for z := 1; z <= 4; z++ {
			walk(append(seq, kinds[z-1]), sum+z)
		}
	}
	walk(nil, 0)
	return out
}
