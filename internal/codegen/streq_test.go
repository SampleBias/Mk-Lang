// Why: str equality is common, and a memcmp call for strings of different
// lengths is wasted work.
// What: TestStrEqualBranchesOnLength (rvalue.go strCompare: == and != branch
// on the lengths and call memcmp only when they are equal) and
// TestStrEqualRun (results natively against Go's ==).
// How: against a literal, LLVM expands the constant-size memcmp into loads
// and compares, so no call is left at -O2 on x86_64.

package codegen

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"

	"mk/internal/mir"
	"mk/internal/types"
)

// strEqBodies are StrEq(a, b: str): bool, StrNe(a, b: str): bool and
// EqGet(s: str): bool { return s == "GET" }.
func strEqBodies(e *testEnv) []*mir.Body {
	g := e.body(e.fn("EqGet", []types.TypeID{types.Str}, types.Bool, 0))
	g.assign(place(0), bin(mir.Eq, mir.Copy(place(1)), strConst(e, "GET")), g.at(2, 5))
	g.ret(g.at(3, 1))
	return []*mir.Body{
		binFn(e, "StrEq", mir.Eq, types.Str, types.Bool, mir.Copy(place(2))),
		binFn(e, "StrNe", mir.Ne, types.Str, types.Bool, mir.Copy(place(2))),
		g.b,
	}
}

func TestStrEqualBranchesOnLength(t *testing.T) {
	e := newTestEnv()
	bodies := strEqBodies(e)
	ll := emit(t, e, unitOf(e, bodies), types.X86_64Linux, types.Dev)
	call := regexp.MustCompile(`(?m)^(x[0-9]+):\n(?:  .*\n)*?  %v[0-9]+ = call i32 @memcmp\(ptr %v[0-9]+, ptr %v[0-9]+, i64 (%v[0-9]+)\)`)
	for _, fn := range []string{"mk.main.StrEq", "mk.main.StrNe", "mk.main.EqGet"} {
		ir := funcIR(t, ll, fn)
		if strings.Contains(ir, "select") {
			t.Errorf("%s: a select is left (the memcmp size must not be a select of the lengths):\n%s", fn, ir)
		}
		m := call.FindStringSubmatch(ir)
		if m == nil {
			t.Fatalf("%s: memcmp is not called in a block of its own:\n%s", fn, ir)
		}
		// The block holding the call is reached only by the branch on the
		// lengths' equality.
		br := regexp.MustCompile(`br i1 (%v[0-9]+), label %` + m[1] + `, label %x[0-9]+`).FindStringSubmatch(ir)
		if br == nil || !regexp.MustCompile(regexp.QuoteMeta(br[1])+` = icmp eq i64 %v[0-9]+, %v[0-9]+`).MatchString(ir) {
			t.Errorf("%s: the memcmp block %s is not guarded by an equality of the lengths:\n%s", fn, m[1], ir)
		}
	}
	// Release, at -O2, in the machine code (ExpandMemCmp runs in the code
	// generator's IR passes): against the literal no memcmp call is left on
	// x86_64; every target still compiles.
	for _, target := range types.Targets() {
		dir := t.TempDir()
		out := clangC(t, target, dir, "streq", emit(t, e, unitOf(e, bodies), target, types.Release), "-O2 -S")
		b, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		if target.Arch != types.ArchX64 {
			continue
		}
		asm := string(b)
		// Mach-O prefixes '_' to every symbol.
		us := ""
		if target.OS == types.OSMacos {
			us = "_"
		}
		i := strings.Index(asm, "\n"+us+"mk.main.EqGet:")
		if i < 0 {
			t.Fatalf("%s: no mk.main.EqGet in the assembly", target.Name)
		}
		fn := asm[i:]
		if j := strings.Index(fn, "func_end"); j >= 0 {
			fn = fn[:j]
		}
		if regexp.MustCompile(`call[a-z]*\s+_?(memcmp|bcmp)\b`).MatchString(fn) {
			t.Errorf("%s: s == \"GET\" still calls memcmp at -O2:\n%s", target.Name, fn)
		}
		if !strings.Contains(fn, "$3,") { // the length test against the literal's 3
			t.Errorf("%s: no compare of the length with 3:\n%s", target.Name, fn)
		}
	}
}

// TestStrEqualRun runs StrEq, StrNe and EqGet natively, at -O0 and -O2,
// over equal, unequal-length and same-length unequal pairs.
func TestStrEqualRun(t *testing.T) {
	long := strings.Repeat("q", 1000)
	pairs := [][2]string{{"GET", "GET"}, {"GET", "GEX"}, {"GET", "GE"}, {"", ""}, {"", "a"}, {"POST", "post"},
		{"abcdefghij", "abcdefghij"}, {"abcdefghij", "abcdefghiJ"}, {"Jbcdefghij", "abcdefghij"},
		{long, long}, {long, long[:999] + "r"}, {long, long[:999]}}
	gets := []string{"GET", "GEX", "GE", "GETS", "", "get"}
	for _, target := range runTargets {
		for _, opt := range []string{"-O0", "-O2"} {
			t.Run(target.Name+opt, func(t *testing.T) {
				if _, why := hostRunner(target); why != "" {
					skipRunner(t, why)
				}
				win := target.OS == types.OSWindows
				var data, body, want strings.Builder
				n := 0
				// arg is the call operands of a str: by reference on Win64
				// (16 bytes), as two words on SysV.
				arg := func(s string) string {
					n++
					fmt.Fprintf(&data, "@d%d = private constant [%d x i8] c\"%s\", align 1\n", n, len(s), s)
					if win {
						fmt.Fprintf(&data, "@s%d = internal global { ptr, i64 } { ptr @d%d, i64 %d }, align 8\n", n, n, len(s))
						return fmt.Sprintf("ptr @s%d", n)
					}
					return fmt.Sprintf("ptr @d%d, i64 %d", n, len(s))
				}
				k := 0
				out := func(fn, args string, r bool) {
					fmt.Fprintf(&body, "  %%r%d = call i1 @mk.main.%s(%s)\n  %%w%d = zext i1 %%r%d to i64\n  call void @h_num(i64 %%w%d)\n  call void @h_write(ptr @s.nl, i64 1)\n",
						k, fn, args, k, k, k)
					fmt.Fprintf(&want, "%d\n", b2u(r))
					k++
				}
				for _, p := range pairs {
					out("StrEq", arg(p[0])+", "+arg(p[1]), p[0] == p[1])
					out("StrNe", arg(p[0])+", "+arg(p[1]), p[0] != p[1])
				}
				for _, s := range gets {
					out("EqGet", arg(s), s == "GET")
				}
				decls := "declare i1 @mk.main.StrEq(ptr, i64, ptr, i64)\ndeclare i1 @mk.main.StrNe(ptr, i64, ptr, i64)\ndeclare i1 @mk.main.EqGet(ptr, i64)\n"
				if win {
					decls = "declare i1 @mk.main.StrEq(ptr, ptr)\ndeclare i1 @mk.main.StrNe(ptr, ptr)\ndeclare i1 @mk.main.EqGet(ptr)\n"
				}
				e := newTestEnv()
				ll := emit(t, e, cCallable(unitOf(e, strEqBodies(e))), target, types.Dev) // the harness calls them
				dir := t.TempDir()
				osIR, kernel := harnessOS(target)
				h := osIR + harnessRT + data.String() + decls + `
define void @"mk.std.task.Start"(ptr %sp) noreturn nounwind {
` + body.String() + `  call void @h_exit(i32 0)
  unreachable
}
`
				objs := []string{clangC(t, target, dir, "support", supportLL(t, target), "-O2"),
					clangC(t, target, dir, "pack", ll, opt), clangC(t, target, dir, "harness", h, "-O0")}
				got, code := runExe(t, target, linkExe(t, target, dir, objs, linkOpts{kernel: kernel}), "")
				if code != 0 || got != want.String() {
					t.Fatalf("exit %d, output\n%s\nwant\n%s", code, got, want.String())
				}
			})
		}
	}
}
