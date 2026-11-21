// Why: thousands of URL cases need one build of a driver, not one build each.
// What: the Mk driver over every function of std.url, its builds urlMain and
// urlCutBuild, urlCutSeam, and the helpers that feed it records.
// How: built once per mode with mk build and fed one record a line on standard
// input, one answer line each. MK_URL_BASE names a std snapshot to build over.

package std

import (
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// urlDriverHead is the drivers' shared part: hex in and out.
const urlDriverHead = `
Nib(c: u8): u8 {
    if c >= b'a' {
        return c - 87
    }
    return c - 48
}

Unhex(a: &Arena, s: str): OutOfMemory![]u8 from a {
    b := s.AsBytes()
    out := try a.Slice[u8](b.Len() / 2)
    for i in 0..out.Len() {
        out[i] = (Nib(b[2 * i]) << 4) | Nib(b[2 * i + 1])
    }
    return out[..]
}

const Digits: str = "0123456789abcdef"

HexOf(a: &Arena, b: []u8): OutOfMemory!str from a {
    d := Digits.AsBytes()
    out := try a.Slice[u8](2 * b.Len())
    for i in 0..b.Len() {
        out[2 * i] = d[int(b[i] >> 4)]
        out[2 * i + 1] = d[int(b[i] & 15)]
    }
    return str.From(out[..]).Must()
}

H(a: &Arena, s: str): OutOfMemory!str from a {
    return try HexOf(a, s.AsBytes())
}

TextOf(a: &Arena, s: str): OutOfMemory!str from a {
    b := try Unhex(a, s)
    return str.From(b) else { "<not utf-8>" }
}
`

// urlDriver is the Mk side. Every argument is lower-case hex or a short
// name ("-" for none); every answer is "ok ..." or "err <the error as {e}
// prints it>".
const urlDriver = `import std.encoding
import std.url
` + urlDriverHead + `
PartOf(name: str): url.Part {
    return match name {
        "protocol" { url.Part.Protocol }
        "username" { url.Part.Username }
        "password" { url.Part.Password }
        "host" { url.Part.Host }
        "hostname" { url.Part.Hostname }
        "port" { url.Part.Port }
        "pathname" { url.Part.Pathname }
        "search" { url.Part.Search }
        "hash" { url.Part.Hash }
        _ { url.Part.Href }
    }
}

// HostOf is Host() as a kind letter and its payload.
HostOf(a: &Arena, u: &url.Url): OutOfMemory!str from a {
    return match u.Host() {
        .Domain(name) { try a.Fmt("D:{h}", (h: try H(a, name))) }
        .Opaque(text) { try a.Fmt("O:{h}", (h: try H(a, text))) }
        .Ipv4(addr) { try a.Fmt("4:{addr}", (addr)) }
        .Ipv6(addr) { try a.Fmt("6:{h}", (h: try HexOf(a, addr[..]))) }
        .Empty { "E:" }
    }
}

// Show is every accessor of u, each hex, and Format's text, and whether
// href parses back to itself.
Show(a: &Arena into a, u: &url.Url): OutOfMemory!str from a {
    port := if p := u.Port() { try a.Fmt("{p}", (p)) } else { "-" }
    var t := Text.New(a)
    var w := t.Writer()
    Print(mut w, "{u}", (u)) else {}
    // Parse keeps views of its input in a, so href is copied there first.
    again := url.Parse(a, try a.Dup(u.Href()), none) else e {
        return try a.Fmt("bad reparse {e}", (e))
    }
    same := again.Href() == u.Href()
    return try a.Fmt("ok {href} {origin} {scheme} {user} {pass} {host} {port} {path} {query} {frag} {kind} {format} {same}", (href: try H(a, u.Href()), origin: try H(a, try u.Origin(a)), scheme: try H(a, u.Scheme()), user: try H(a, u.Username()), pass: try H(a, u.Password()), host: try H(a, u.Hostname()), port, path: try H(a, u.Path()), query: try H(a, u.Query()), frag: try H(a, u.Fragment()), kind: try HostOf(a, u), format: try H(a, t.Str()), same))
}

Case(a: &Arena into a, line: str): OutOfMemory!str from a {
    var parts := line.Split(" ")
    op := parts.Next() else { "" }
    x := parts.Next() else { "" }
    y := parts.Next() else { "" }
    z := parts.Next() else { "" }
    w4 := parts.Next() else { "" }
    w5 := parts.Next() else { "" }
    match op {
        "parse" {
            input := try TextOf(a, x)
            if y == "-" {
                u := url.Parse(a, input, none) else e {
                    return try a.Fmt("err {e}", (e))
                }
                return try Show(a, &u)
            }
            base := url.Parse(a, try TextOf(a, y), none) else e {
                return try a.Fmt("err base {e}", (e))
            }
            u := url.Parse(a, input, some(&base)) else e {
                return try a.Fmt("err {e}", (e))
            }
            return try Show(a, &u)
        }
        "rep" {
            // prefix, unit repeated count times, suffix: inputs past the
            // 64 KiB line of standard input.
            count := Cast[int](z.ParseInt() else { 0 })
            unit := try TextOf(a, y)
            var t := Text.New(a)
            try t.Push(try TextOf(a, x))
            for _ in 0..count {
                try t.Push(unit)
            }
            try t.Push(try TextOf(a, w4))
            // Parse keeps views of its input in a: the text goes there.
            input := try a.Dup(t.Str())
            if w5 == "-" {
                u := url.Parse(a, input, none) else e {
                    return try a.Fmt("err {e}", (e))
                }
                return try Show(a, &u)
            }
            base := url.Parse(a, try TextOf(a, w5), none) else e {
                return try a.Fmt("err base {e}", (e))
            }
            u := url.Parse(a, input, some(&base)) else e {
                return try a.Fmt("err {e}", (e))
            }
            return try Show(a, &u)
        }
        "pbr" {
            // Whether Parse of x against y built y's record (url-05).
            base := url.Parse(a, try TextOf(a, y), none) else e {
                return try a.Fmt("err base {e}", (e))
            }
            read := url.FxParseBaseRead(a, try TextOf(a, x), &base) else e {
                return try a.Fmt("err {e}", (e))
            }
            return try a.Fmt("ok {read}", (read))
        }
        "plain" {
            // Whether Parse takes x by its one-pass form (ParsePlain).
            plain := try url.FxPlain(a, try TextOf(a, x))
            return try a.Fmt("ok {plain}", (plain))
        }
        "set" {
            u := url.Parse(a, try TextOf(a, x), none) else e {
                return try a.Fmt("err href {e}", (e))
            }
            v := u.Set(a, PartOf(y), try TextOf(a, z)) else e {
                return try a.Fmt("err {e}", (e))
            }
            return try Show(a, &v)
        }
        "pq" {
            pairs := try url.ParseQuery(a, try TextOf(a, x))
            var t := Text.New(a)
            try t.Push("ok")
            for (name, value) in pairs {
                try t.Push(try a.Fmt(" {n}={v}", (n: try H(a, name), v: try H(a, value))))
            }
            return try a.Dup(t.Str())
        }
        "fq" {
            var pairs := List[(name: str, value: str)].New(a)
            if x != "-" {
                for item in x.Split(",") {
                    var nv := item.Split("=")
                    name := nv.Next() else { "" }
                    value := nv.Next() else { "" }
                    try pairs.Push((name: try TextOf(a, name), value: try TextOf(a, value)))
                }
            }
            return try a.Fmt("ok {h}", (h: try H(a, try url.FormatQuery(a, pairs.Done()[..]))))
        }
        "pcte" {
            // Percent-encode after encoding with the special-query set (the
            // URL Standard 1.3) for a single-byte encoding: each scalar
            // encoded alone, an unmappable one as %26%23N%3B.
            enc := encoding.Lookup(x) else e {
                return try a.Fmt("err {e}", (e))
            }
            input := try TextOf(a, y)
            var t := Text.New(a)
            var at := 0
            for c in input.Scalars() {
                n := if c < '\u{80}' { 1 } else if c < '\u{800}' { 2 } else if c < '\u{10000}' { 3 } else { 4 }
                one := input[at..at + n]
                at += n
                bytes := enc.Encode(a, one) else {
                    try t.Push(try a.Fmt("%26%23{n}%3B", (n: u32(c))))
                    continue
                }
                try t.Push(try url.FxEncodeSpecialQuery(a, bytes))
            }
            return try a.Fmt("ok {h}", (h: try H(a, t.Str())))
        }
        "enc" {
            return try a.Fmt("ok {h}", (h: try H(a, try url.FxEncode(a, try Unhex(a, y), x))))
        }
        "props" {
            lo := Cast[int](x.ParseInt() else { 0 })
            count := Cast[int](y.ParseInt() else { 0 })
            return try a.Fmt("ok {h}", (h: try url.FxProps(a, lo, count)))
        }
        _ {
            return "bad op"
        }
    }
}

export Main(sys: &System): ReadError|WriteError|OutOfMemory!void {
    var r := sys.Stdin()
    var out := sys.Stdout()
    var arena := try Arena.New(sys.heap)
    defer arena.Free()
    for {
        line := try r.Line(&arena) else {
            break
        }
        answer := try Case(&arena, line)
        try out.Str(answer)
        try out.Str("\n")
        arena.Reset()
    }
    try out.Flush()
}
`

// urlFixture is written to std/url in the driver's copy of std: it exports
// the percent-encoder, so a test can drive the percent-encode sets
// directly. Nothing here is part of std.
const urlFixture = `// Fixture of the std-url tests: private functions, exported.

import std.rt

export FxEncode(a: &Arena, s: []u8, set: str): OutOfMemory!str from a {
    bit := match set {
        "c0" { InC0 }
        "fragment" { InFragment }
        "query" { InQuery }
        "specialquery" { InSpecialQuery }
        "path" { InPath }
        _ { InUserinfo }
    }
    out := try Encode(a, s, bit)
    return try a.Dup(rt.StrFrom(out).Must())
}

// FxParseBaseRead is whether Parse of input against base built the base's
// record, a copy of base in a (url-05).
export FxParseBaseRead(a: &Arena, input: str into a, base: &Url): Error|OutOfMemory!bool {
    var read := false
    _ = try ParseWith(a, input, some(base), mut read)
    return read
}

// FxPlain is whether ParsePlain takes input.
export FxPlain(a: &Arena, input: str into a): OutOfMemory!bool {
    u := try ParsePlain(a, input)
    return u.IsSome()
}

export FxEncodeSpecialQuery(a: &Arena, s: []u8): OutOfMemory!str from a {
    return try FxEncode(a, s, "specialquery")
}

// FxProps is, for each code point from lo, its PropData byte and whether
// it is a Virama, as four hex digits; a surrogate is "0000".
export FxProps(a: &Arena, lo: int, count: int): OutOfMemory!str from a {
    digits := "0123456789abcdef".AsBytes()
    out := try a.Slice[u8](4 * count)
    for k in 0..count {
        cp := lo + k
        var b: u8 = 0
        var v: u8 = 0
        if cp < 0xD800 || cp > 0xDFFF {
            c := Cast[char](Cast[u32](cp))
            b = BidiJoining(c)
            v = if IsVirama(c) { 1 } else { 0 }
        }
        out[4 * k] = digits[int(b >> 4)]
        out[4 * k + 1] = digits[int(b & 15)]
        out[4 * k + 2] = b'0'
        out[4 * k + 3] = digits[int(v)]
    }
    return rt.StrFrom(out[..]).Must()
}
`

// urlBuild names one driver build: its driver program, the fixture files
// written into its std/url copy, and whether that copy is IDNA's cut form
// (std/url/idna_*.mk removed and idna.mk replaced by urlCutSeam).
type urlBuild struct {
	key      string
	driver   string
	fixtures map[string]string
	cut      bool
}

// urlMain is the driver over the core and the props tables.
var urlMain = urlBuild{key: "main", driver: urlDriver, fixtures: map[string]string{"url/tests_fixture.mk": urlFixture}}

// urlCutBuild is urlMain over std.url as it is with IDNA cut.
var urlCutBuild = urlBuild{key: "cut", driver: urlDriver, fixtures: map[string]string{"url/tests_fixture.mk": urlFixture}, cut: true}

// urlCutSeam is std/url/idna.mk in IDNA's cut form: a domain with a
// non-ASCII code point, or an ASCII label starting "xn--" in any case, is
// refused with IdnaUnsupported. Cutting IDNA deletes std/url/idna_*.mk and
// std/std-url_idna_*_test.go and writes this text as std/url/idna.mk;
// TestStdUrl_IdnaCutForm builds and runs that form.
const urlCutSeam = `// Why: the domain parser needs UTS 46 for non-ASCII domains; this is the
// build without IDNA, which refuses them instead of mapping them.
//
// What: IdnaAsciiAllowed and IdnaToAscii, the two calls host.mk makes into
// UTS 46 (section 3.3 of the URL Standard), in their refusal form.
//
// How: a domain with a non-ASCII code point, or an ASCII label starting
// "xn--" in any case, is refused with IdnaUnsupported, so no Punycode label
// reaches a resolver or a certificate name check unchecked.

// IdnaAsciiAllowed is whether an ASCII domain may be used as it is: true
// unless one of its labels starts "xn--", which needs UTS 46's check.
IdnaAsciiAllowed(domain: []u8): bool {
    var start := 0
    for k in 0..domain.Len() + 1 {
        if k < domain.Len() && domain[k] != b'.' {
            continue
        }
        label := domain[start..k]
        if label.Len() >= 4 && label[0] | 0x20 == b'x' && label[1] | 0x20 == b'n' && label[2] == b'-' && label[3] == b'-' {
            return false
        }
        start = k + 1
    }
    return true
}

// IdnaToAscii is UTS 46's ToASCII as the domain parser runs it, on a
// domain that is valid UTF-8 and not ASCII. Without IDNA it fails with
// IdnaUnsupported.
IdnaToAscii(a: &Arena, domain: str): Error|OutOfMemory![]u8 from a {
    _ = a
    _ = domain
    fail IdnaUnsupported
}
`

// urlBuilt holds one driver executable per build and mode.
var urlBuilt struct {
	sync.Mutex
	exe  map[string]string
	err  map[string]string
	fail map[string]string
}

// urlStdDir builds the std directory a test's mk uses: the live tree, or
// MK_URL_BASE with this tree's std/url; both with b's fixtures, and in
// IDNA's cut form when b asks for it.
func urlStdDir(t *testing.T, root string, b urlBuild) string {
	t.Helper()
	var stdDir string
	base := os.Getenv("MK_URL_BASE")
	if base == "" {
		stdDir = copyStd(t, root, b.fixtures)
	} else {
		stdDir = filepath.Join(root, "std")
		if err := os.CopyFS(stdDir, os.DirFS(base)); err != nil {
			t.Fatal(err)
		}
		if err := os.RemoveAll(filepath.Join(stdDir, "url")); err != nil {
			t.Fatal(err)
		}
		if err := os.RemoveAll(filepath.Join(stdDir, ".mk-cache")); err != nil {
			t.Fatal(err)
		}
		if err := os.CopyFS(filepath.Join(stdDir, "url"), os.DirFS("url")); err != nil {
			t.Fatal(err)
		}
		for p, text := range b.fixtures {
			if err := os.WriteFile(filepath.Join(stdDir, filepath.FromSlash(p)), []byte(text), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	if b.cut {
		idna, err := filepath.Glob(filepath.Join(stdDir, "url", "idna_*.mk"))
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range idna {
			if err := os.Remove(f); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(stdDir, "url", "idna.mk"), []byte(urlCutSeam), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return stdDir
}

// urlExe builds urlMain's driver for mode ("dev" or "release").
func urlExe(t testing.TB, mode string) string {
	t.Helper()
	return urlExeOf(t, urlMain, mode)
}

// urlExeOf builds b's driver for mode once per test binary and returns its
// path; a missing toolchain skips the test.
func urlExeOf(t testing.TB, b urlBuild, mode string) string {
	t.Helper()
	urlBuilt.Lock()
	defer urlBuilt.Unlock()
	if urlBuilt.exe == nil {
		urlBuilt.exe = map[string]string{}
		urlBuilt.err = map[string]string{}
		urlBuilt.fail = map[string]string{}
	}
	key := b.key + "-" + mode
	if exe, ok := urlBuilt.exe[key]; ok {
		return exe
	}
	if msg, ok := urlBuilt.fail[key]; ok {
		t.Fatal(msg)
	}
	if msg, ok := urlBuilt.err[key]; ok {
		t.Skip(msg)
	}
	tt, ok := t.(*testing.T)
	if !ok {
		t.Fatal("urlExeOf: the first build needs a *testing.T")
	}
	// A driver lives as long as its test binary, which no t.TempDir does, so
	// each build leaves its std copy behind: remove those older than two
	// hours.
	if old, _ := filepath.Glob(filepath.Join(os.TempDir(), "mk-url-*")); len(old) > 0 {
		for _, d := range old {
			if fi, err := os.Stat(d); err == nil && time.Since(fi.ModTime()) > 2*time.Hour {
				os.RemoveAll(d)
			}
		}
	}
	dir, err := packageTempDir("mk-url-" + key)
	if err != nil {
		t.Fatal(err)
	}
	stdDir := urlStdDir(tt, dir, b)
	if err := os.WriteFile(filepath.Join(dir, "main.mk"), []byte(b.driver), 0o644); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(dir, "main")
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	args := []string{"build"}
	if mode == "release" {
		args = append(args, "--release")
	}
	args = append(args, "-o", exe, "main.mk")
	out, status := mkCmd(tt, dir, stdDir, args...)
	if status != 0 {
		if strings.Contains(out, "no usable LLVM") {
			urlBuilt.err[key] = "needs the LLVM toolchain: " + out
			t.Skip(urlBuilt.err[key])
		}
		// Kept, so a fuzz target's seeds do not rebuild a broken driver
		// once each.
		urlBuilt.fail[key] = fmt.Sprintf("mk build of the url driver (%s): exit %d\n%s", key, status, out)
		t.Fatal(urlBuilt.fail[key])
	}
	urlBuilt.exe[key] = exe
	return exe
}

// urlModes are the builds every differential runs.
var urlModes = []string{"dev", "release"}

// urlAsk sends records to urlMain's driver for mode.
func urlAsk(t *testing.T, mode string, records []string) []string {
	t.Helper()
	return urlAskOf(t, urlMain, mode, records)
}

// urlAskOf sends records to b's driver for mode in batches and returns its
// answers, one per record.
func urlAskOf(t *testing.T, b urlBuild, mode string, records []string) []string {
	t.Helper()
	exe := urlExeOf(t, b, mode)
	var answers []string
	const batch = 4000
	for start := 0; start < len(records); start += batch {
		end := min(start+batch, len(records))
		var in strings.Builder
		for _, r := range records[start:end] {
			in.WriteString(r)
			in.WriteString("\n")
		}
		r := runNative(t, exe, in.String())
		if r.code != 0 {
			t.Fatalf("url driver (%s %s) exit %d\nstderr: %s\nlast answers: %s", b.key, mode, r.code, r.stderr, urlTail(r.stdout))
		}
		got := strings.Split(strings.TrimSuffix(strings.ReplaceAll(r.stdout, "\r\n", "\n"), "\n"), "\n")
		if len(got) != end-start {
			t.Fatalf("url driver (%s %s): %d answers for %d records\nstderr: %s", b.key, mode, len(got), end-start, r.stderr)
		}
		answers = append(answers, got...)
	}
	return answers
}

func urlTail(s string) string {
	if len(s) > 1000 {
		return "..." + s[len(s)-1000:]
	}
	return s
}

// urlHex is the driver's hex of s.
func urlHex(s string) string { return hex.EncodeToString([]byte(s)) }

// urlUnhex is the text the driver's hex h spells.
func urlUnhex(t *testing.T, h string) string {
	t.Helper()
	b, err := hex.DecodeString(h)
	if err != nil {
		t.Fatalf("bad hex from the driver: %q", h)
	}
	return string(b)
}

// urlShown is one URL as the driver's Show answers it.
type urlShown struct {
	href, origin, scheme, username, password, hostname, port, path, query, fragment string
	kind, format                                                                    string
	reparses                                                                        bool
}

// urlRead reads an "ok ..." answer of Show.
func urlRead(t *testing.T, answer string) (urlShown, bool) {
	t.Helper()
	f := strings.Fields(answer)
	if len(f) == 0 || f[0] != "ok" {
		return urlShown{}, false
	}
	// Empty hex fields vanish under Fields: split on single spaces.
	f = strings.Split(answer, " ")
	if len(f) != 14 {
		t.Fatalf("bad answer: %q", answer)
	}
	return urlShown{
		href: urlUnhex(t, f[1]), origin: urlUnhex(t, f[2]), scheme: urlUnhex(t, f[3]),
		username: urlUnhex(t, f[4]), password: urlUnhex(t, f[5]), hostname: urlUnhex(t, f[6]),
		port: f[7], path: urlUnhex(t, f[8]), query: urlUnhex(t, f[9]), fragment: urlUnhex(t, f[10]),
		kind: f[11], format: urlUnhex(t, f[12]), reparses: f[13] == "true",
	}, true
}

// search and hash are the URL Standard's getters over Query and Fragment.
func (u urlShown) search() string {
	if u.query == "" {
		return ""
	}
	return "?" + u.query
}

func (u urlShown) hash() string {
	if u.fragment == "" {
		return ""
	}
	return "#" + u.fragment
}

func (u urlShown) host() string {
	if u.port == "-" {
		return u.hostname
	}
	return u.hostname + ":" + u.port
}

func (u urlShown) portText() string {
	if u.port == "-" {
		return ""
	}
	return u.port
}

// urlRng is splitmix64, so a failing case can be named by its seed.
type urlRng struct{ s uint64 }

func (r *urlRng) next() uint64 {
	r.s += 0x9E3779B97F4A7C15
	z := r.s
	z = (z ^ z>>30) * 0xBF58476D1CE4E5B9
	z = (z ^ z>>27) * 0x94D049BB133111EB
	return z ^ z>>31
}

func (r *urlRng) intn(n int) int { return int(r.next() % uint64(n)) }

// urlCheck compares one field and returns a message when they differ.
func urlCheck(name, got, want string) string {
	if got == want {
		return ""
	}
	return fmt.Sprintf("%s: got %q, want %q", name, got, want)
}
