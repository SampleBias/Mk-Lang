// Why: the parser's one-step paths (Parser.PathRun, IsPlainDomain,
// SerializeSame, ParsePlain) skip work, so they must give what the full
// steps give.
// What: TestStdUrl_PathModel, TestStdUrl_HostModel, TestStdUrl_SameAsFull
// and TestStdUrl_PlainForm.
// How: random paths and hosts against Go models of the URL Standard's path
// state and host parser; SerializeSame and ParsePlain against Serialize.

package std

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

// urlPathSetModel is whether the URL Standard's path percent-encode set holds
// byte c: the C0 control set (C0 controls and every byte above 0x7E), space,
// '"', '#', '<', '>', '?', '^', '`', '{' and '}'.
func urlPathSetModel(c byte) bool {
	return c < 0x20 || c > 0x7E || strings.IndexByte(" \"#<>?^`{}", c) >= 0
}

func urlIsSingleDotModel(s string) bool {
	return s == "." || strings.EqualFold(s, "%2e")
}

func urlIsDoubleDotModel(s string) bool {
	switch strings.ToLower(s) {
	case "..", ".%2e", "%2e.", "%2e%2e":
		return true
	}
	return false
}

// urlPathModel is the path state (not a file URL, no state override) run
// on in from the segment list start: the pathname serialized.
func urlPathModel(special bool, start []string, in string) string {
	segs := append([]string(nil), start...)
	var buf strings.Builder
	for i := 0; ; i++ {
		eof := i >= len(in)
		var c byte
		if !eof {
			c = in[i]
		}
		if eof || c == '/' || special && c == '\\' || c == '?' || c == '#' {
			slash := !eof && (c == '/' || special && c == '\\')
			b := buf.String()
			buf.Reset()
			switch {
			case urlIsDoubleDotModel(b):
				if len(segs) > 0 {
					segs = segs[:len(segs)-1]
				}
				if !slash {
					segs = append(segs, "")
				}
			case urlIsSingleDotModel(b):
				if !slash {
					segs = append(segs, "")
				}
			default:
				var e strings.Builder
				for k := 0; k < len(b); k++ {
					if urlPathSetModel(b[k]) {
						fmt.Fprintf(&e, "%%%02X", b[k])
					} else {
						e.WriteByte(b[k])
					}
				}
				segs = append(segs, e.String())
			}
			if !slash {
				break
			}
			continue
		}
		buf.WriteByte(c)
	}
	var out strings.Builder
	for _, s := range segs {
		out.WriteString("/")
		out.WriteString(s)
	}
	return out.String()
}

// urlPathPieces are what random paths are made of: plain text, every dot
// segment spelling, separators, bytes the path set encodes (a C0 control,
// space, DEL, '^', '{', a two-byte and a three-byte scalar), '%' escapes
// that are and are not dots, and the bytes that end a path.
var urlPathPieces = []string{
	"a", "b", "seg", "~x-_", ".", "..", "%2e", "%2E", ".%2e", "%2e.", "%2E%2e", "...", ".a", "a.",
	"/", "/", "/", "//", "\\", "\x01", " ", "\x7f", "^", "{", "é", "€", "%41", "%", "%2", "%2f",
	"?", "#", "?q", "#f",
}

func urlRandomPath(r *urlRng) string {
	var b strings.Builder
	n := r.intn(9)
	for i := 0; i < n; i++ {
		b.WriteString(urlPathPieces[r.intn(len(urlPathPieces))])
	}
	return b.String()
}

// TestStdUrl_PathModel: 6,000 random paths through url.Parse, as an absolute
// special URL's, an absolute non-special URL's with a host, and relative to
// a special base with three segments, each pathname against urlPathModel.
func TestStdUrl_PathModel(t *testing.T) {
	r := urlRng{s: 3}
	type pathCase struct{ record, want string }
	var cases []pathCase
	for i := 0; i < 2000; i++ {
		p := urlRandomPath(&r)
		// "#e" keeps a trailing space or control from being trimmed.
		cases = append(cases,
			pathCase{"parse " + urlHex("http://h/"+p+"#e") + " -", urlPathModel(true, nil, p)},
			pathCase{"parse " + urlHex("foo://h/"+p+"#e") + " -", urlPathModel(false, nil, p)})
		// Relative to a base: a first byte that is not '/', '\', '?' or '#'
		// clones the base's path, drops its last segment and enters the
		// path state.
		q := "z" + p
		cases = append(cases, pathCase{"parse " + urlHex(q+"#e") + " " + urlHex("http://h/a/b/c"), urlPathModel(true, []string{"a", "b"}, q)})
	}
	var records []string
	for _, c := range cases {
		records = append(records, c.record)
	}
	for _, mode := range urlModes {
		answers := urlAsk(t, mode, records)
		failed := 0
		for i, c := range cases {
			u, ok := urlRead(t, answers[i])
			if !ok {
				t.Errorf("%s: %s: %s, want pathname %q", mode, c.record, answers[i], c.want)
				failed++
			} else if u.path != c.want || !u.reparses {
				t.Errorf("%s: %s: pathname %q (reparses %v), want %q", mode, c.record, u.path, u.reparses, c.want)
				failed++
			}
			if failed > 20 {
				t.Fatalf("%s: too many failures", mode)
			}
		}
	}
}

// urlIpv4NumberModel is the IPv4 number parser: the value (saturated past
// 2^32) and whether it parsed.
func urlIpv4NumberModel(s string) (uint64, bool) {
	if s == "" {
		return 0, false
	}
	radix := uint64(10)
	switch {
	case len(s) >= 2 && s[0] == '0' && (s[1] == 'x' || s[1] == 'X'):
		s, radix = s[2:], 16
	case len(s) >= 2 && s[0] == '0':
		s, radix = s[1:], 8
	}
	var v uint64
	for i := 0; i < len(s); i++ {
		d, err := strconv.ParseUint(s[i:i+1], 16, 8)
		if err != nil || d >= radix {
			return 0, false
		}
		if v <= 1<<32 {
			v = v*radix + d
		}
	}
	return min(v, 1<<32+1), true
}

// urlHostDecodesToAscii is whether in's percent-decoding is ASCII, the
// hosts urlHostModel covers (the others go through UTS 46).
func urlHostDecodesToAscii(in string) bool {
	for i := 0; i+2 < len(in); i++ {
		if in[i] == '%' {
			if v, err := strconv.ParseUint(in[i+1:i+3], 16, 8); err == nil && v >= 0x80 {
				return false
			}
		}
	}
	return true
}

// urlHostModel is the host parser on a special URL's ASCII host whose
// escapes decode to ASCII: the hostname serialized, or "" for failure.
func urlHostModel(in string) string {
	var d strings.Builder
	for i := 0; i < len(in); i++ {
		if in[i] == '%' && i+2 < len(in) {
			if v, err := strconv.ParseUint(in[i+1:i+3], 16, 8); err == nil {
				d.WriteByte(byte(v))
				i += 2
				continue
			}
		}
		d.WriteByte(in[i])
	}
	domain := strings.ToLower(d.String())
	if domain == "" || strings.ContainsAny(domain, "\x00\t\n\r #/:<>?@[\\]^|%\x7f") {
		return ""
	}
	for i := 0; i < len(domain); i++ {
		if domain[i] < 0x20 {
			return ""
		}
	}
	// The ends-in-a-number checker.
	labels := strings.Split(domain, ".")
	if labels[len(labels)-1] == "" && len(labels) > 1 {
		labels = labels[:len(labels)-1]
	}
	last := labels[len(labels)-1]
	number := last != "" && strings.Trim(last, "0123456789") == ""
	if len(last) >= 2 && last[0] == '0' && last[1] == 'x' && strings.Trim(last[2:], "0123456789abcdef") == "" {
		number = true
	}
	if !number {
		return domain
	}
	// The IPv4 parser.
	if len(labels) > 4 {
		return ""
	}
	var ip uint64
	for i, l := range labels {
		n, ok := urlIpv4NumberModel(l)
		if !ok {
			return ""
		}
		if i < len(labels)-1 {
			if n > 255 {
				return ""
			}
			ip += n << (8 * (3 - i))
			continue
		}
		if n >= 1<<(8*(5-len(labels))) {
			return ""
		}
		ip += n
	}
	return fmt.Sprintf("%d.%d.%d.%d", ip>>24, ip>>16&255, ip>>8&255, ip&255)
}

// urlHostPieces build random hosts: letters of both cases, digits, hex and
// octal prefixes, dots, '-' and '_', escapes of ASCII, and forbidden domain
// code points.
var urlHostPieces = []string{
	"a", "b", "z", "Z", "Ex", "f", "0", "1", "9", "255", "256", "0x", "0X", "0xff", "07", "08", "4294967295",
	".", ".", "..", "-", "_", "%41", "%2e", "%30", "%7a", "%25", "%", "%zz", "%20", "*", "~", "<", "^", "|", "\x01",
}

// TestStdUrl_HostModel: 8,000 random ASCII hosts through url.Parse as an
// http URL's, each hostname or failure against urlHostModel.
func TestStdUrl_HostModel(t *testing.T) {
	r := urlRng{s: 5}
	var records, hosts []string
	for len(hosts) < 8000 {
		var b strings.Builder
		n := 1 + r.intn(6)
		for k := 0; k < n; k++ {
			b.WriteString(urlHostPieces[r.intn(len(urlHostPieces))])
		}
		h := b.String()
		if !urlHostDecodesToAscii(h) {
			continue
		}
		hosts = append(hosts, h)
		records = append(records, "parse "+urlHex("http://"+h+"/p")+" -")
	}
	for _, mode := range urlModes {
		answers := urlAsk(t, mode, records)
		failed := 0
		for i, h := range hosts {
			want := urlHostModel(h)
			u, ok := urlRead(t, answers[i])
			switch {
			case want == "" && ok:
				t.Errorf("%s: host %q: parsed to %q, want failure", mode, h, u.hostname)
				failed++
			case want != "" && !ok:
				t.Errorf("%s: host %q: %s, want %q", mode, h, answers[i], want)
				failed++
			case ok && u.hostname != want:
				t.Errorf("%s: host %q: got %q, want %q", mode, h, u.hostname, want)
				failed++
			}
			if failed > 20 {
				t.Fatalf("%s: too many failures", mode)
			}
		}
	}
}

// urlTabbed is s with a tab after its first scalar that is not a C0 control
// or space (one that step 1 keeps), or false when s has none.
func urlTabbed(s string) (string, bool) {
	for i, r := range s {
		if r > 0x20 {
			k := i + utf8.RuneLen(r)
			return s[:k] + "\t" + s[k:], true
		}
	}
	return "", false
}

// TestStdUrl_SameAsFull: urltestdata.json's inputs and expected hrefs, the
// bench's URL shapes and the path test's random paths, each parsed with no
// base as given (SerializeSame where it applies) and with a tab added
// (Serialize always), and the two answers compared: the same URL, the same
// error, a BadUrl offset past the tab one later (urlSameAnswersTab).
func TestStdUrl_SameAsFull(t *testing.T) {
	var inputs []string
	for _, c := range urlTestData(t) {
		inputs = append(inputs, c.Input)
		if c.Href != "" {
			inputs = append(inputs, c.Href)
		}
	}
	r := urlRng{s: 11}
	schemes := []string{"http", "https", "ws", "wss", "ftp", "foo", "HTTP", "mailto", "file"}
	hosts := []string{"example.com", "a.b.c.org", "EXAMPLE.com", "1.2.3.4", "[::1]", "", "h", "xn--a", "0x7f.1", "a%41.com"}
	ports := []string{"", ":8080", ":80", ":443", ":0", ":", ":08080", ":65535"}
	for i := 0; i < 3000; i++ {
		s := schemes[r.intn(len(schemes))] + ":"
		switch r.intn(4) {
		case 0:
			s += "//" + hosts[r.intn(len(hosts))] + ports[r.intn(len(ports))]
		case 1:
			s += "//" + hosts[r.intn(len(hosts))]
		case 2:
			s += "/"
		}
		s += urlRandomPath(&r)
		inputs = append(inputs, s)
	}
	var records []string
	var kept []string
	for _, in := range inputs {
		tab, ok := urlTabbed(in)
		if !ok {
			continue
		}
		kept = append(kept, in)
		records = append(records, "parse "+urlHex(in)+" -", "parse "+urlHex(tab)+" -")
	}
	for _, mode := range urlModes {
		answers := urlAsk(t, mode, records)
		failed := 0
		for i, in := range kept {
			x, y := answers[2*i], answers[2*i+1]
			if !urlSameAnswersTab(x, y, in) {
				t.Errorf("%s: %q:\n as given %s\n tabbed   %s", mode, in, x, y)
				failed++
			}
			if failed > 20 {
				t.Fatalf("%s: too many failures", mode)
			}
		}
	}
}

// TestStdUrl_PlainForm: ParsePlain takes the inputs it is for (the bench's
// shapes among them) and refuses each kind of input it must leave to the
// state machine, and on every case Parse answers as it does with a tab
// added, which ParsePlain never takes.
func TestStdUrl_PlainForm(t *testing.T) {
	cases := []struct {
		input string
		plain bool
	}{
		{"http://example.com/", true},
		{"https://a.b.c.org:8443/x/y/?q=1&r=#frag", true},
		{"HTTP://Example.COM/p", true},
		{"Ws://h:3000/", true},
		{"wss://10.1.2.3:3000/a", true},
		{"ftp://h/~a-b_c/", true},
		{"ws://h:0/", true},
		{"http://h:65535/", true},
		{"http://h/?a#b#c", true},
		{"http://h/p?q?r", true},
		{"http://a..b/", true},
		{"http://h//x", true},
		{"http://xn--a.com/", true},
		{"http://h/a|b", true},
		{"http://h", false},
		{"http://h?q", false},
		{"http://h#f", false},
		{"http://h:/", false},
		{"http://h:80/", false},
		{"https://h:443/", false},
		{"ftp://h:21/", false},
		{"http://h:0080/", false},
		{"http://h:08080/", false},
		{"http://h:65536/", false},
		{"http://h:123456/", false},
		{"http://u@h/", false},
		{"http://u:p@h/", false},
		{"http://h:8080@g/", false},
		{"http://[::1]/", false},
		{"http://h%41/", false},
		{"http://h\\x", false},
		{"http:\\\\h/", false},
		{"http:/h/", false},
		{"http:h/", false},
		{"http:///h/", false},
		{"file:///x", false},
		{"sc://h/", false},
		{"httpx://h/", false},
		{"h://h/", false},
		{"http://0x7f.1/", false},
		{"http://1.2.3/", false},
		{"http://01.2.3.4/", false},
		{"http://1.2.3.4./", false},
		{"http://256.1.1.1/", false},
		{"http://a.1/", false},
		{"http://h/a/../b", false},
		{"http://h/./b", false},
		{"http://h/%2e/", false},
		{"http://h/a/..", false},
		{"http://h/a/%2E%2e", false},
		{"http://h/a%2E%2e", true},
		{"http://h/a b", false},
		{"http://h/a\t", false},
		{" http://h/", false},
		{"http://h/ ", false},
		{"http://h/\u00e9", false},
		{"http://\u00e9.com/", false},
		{"http://h/a^b", false},
		{"http://h/?a b", false},
		{"http://h/?'", false},
		{"http://h/#a b", false},
		{"http://h/#`", false},
	}
	var records []string
	for _, c := range cases {
		tab, ok := urlTabbed(c.input)
		if !ok {
			t.Fatalf("%q: no tab place", c.input)
		}
		records = append(records, "plain "+urlHex(c.input), "parse "+urlHex(c.input)+" -", "parse "+urlHex(tab)+" -")
	}
	for _, mode := range urlModes {
		answers := urlAsk(t, mode, records)
		for i, c := range cases {
			if want := fmt.Sprintf("ok %v", c.plain); answers[3*i] != want {
				t.Errorf("%s: %q: ParsePlain %s, want %s", mode, c.input, answers[3*i], want)
			}
			if x, y := answers[3*i+1], answers[3*i+2]; !urlSameAnswersTab(x, y, c.input) {
				t.Errorf("%s: %q:\n as given %s\n tabbed   %s", mode, c.input, x, y)
			}
		}
	}
}
