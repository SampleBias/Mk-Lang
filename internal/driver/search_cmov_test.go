// Why: Slice.Search over random keys is only as fast as Rust's and Zig's
// binary searches when each probe's choice is a conditional move; LLVM's x86
// cmov converter turns a plain select in that loop into a branch, which
// random keys mispredict half the time.
// What: TestSearchProbeIsCmov.
// How: builds a release program that searches a u64 slice for x86_64-linux
// and checks, in the disassembly of mk.main.Main (where Search inlines),
// that the first instruction after the probe's compare of a slice element
// that reads the flags is a cmov, not a jump.

package driver

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"mk/internal/types"
)

const searchCmovMain = `Less(e: &u64, k: &u64): bool {
    return *e < *k
}

export Main(sys: &System): OutOfMemory|WriteError!void {
    n := sys.args.Len() * 1000
    xs := try sys.arena.Slice[u64](n)
    for i in 0..n {
        xs[i] = Cast[u64](2 * i)
    }
    k := Cast[u64](sys.args.Len() * 777)
    i := xs.Search(&k, Less)
    try sys.Print("{i}\n", (i))
}
`

// TestSearchProbeIsCmov: in a release x86_64-linux build of a u64 search,
// mk.main.Main holds a compare of a register with an element of the slice
// (an indexed memory operand) whose result is first read by a cmov:
// Search's probe, whose choice raw.Select keeps branch-free. Moves and lea
// between the two, which leave the flags alone, are passed over.
func TestSearchProbeIsCmov(t *testing.T) {
	tools := needTools(t)
	exe := "llvm-objdump"
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	objdump := filepath.Join(filepath.Dir(tools.Clang), exe)
	if _, err := os.Stat(objdump); err != nil {
		notRun(t, "no %s next to clang: the disassembly check is not run", objdump)
	}
	path := mkh144Build(t, searchCmovMain, types.X86_64Linux, types.Release, t.TempDir(), "search")
	out, err := exec.Command(objdump, "-d", "--no-show-raw-insn", "--disassemble-symbols=mk.main.Main", path).CombinedOutput()
	if err != nil {
		t.Fatalf("llvm-objdump: %v\n%s", err, out)
	}
	probe := regexp.MustCompile(`^cmpq\s+%r\w+, -?(0x[0-9a-f]+)?\(%r\w+,%r\w+,8\)$`)
	var ins []string
	for _, line := range strings.Split(string(out), "\n") {
		if f := strings.SplitN(line, "\t", 2); len(f) == 2 && strings.HasSuffix(strings.TrimSpace(f[0]), ":") {
			ins = append(ins, strings.Join(strings.Fields(f[1]), " "))
		}
	}
	if len(ins) == 0 {
		t.Fatalf("llvm-objdump found no mk.main.Main in %s:\n%s", path, out)
	}
	var seen []string
	for i, in := range ins {
		if !probe.MatchString(in) {
			continue
		}
		j := i + 1
		for j < len(ins) && (strings.HasPrefix(ins[j], "mov") || strings.HasPrefix(ins[j], "lea")) {
			j++
		}
		if j == len(ins) {
			break
		}
		seen = append(seen, in+" / "+ins[j])
		if strings.HasPrefix(ins[j], "cmov") {
			return
		}
	}
	t.Fatalf("no compare of a slice element in mk.main.Main is first read by a cmov; the compares and their readers:\n%s", strings.Join(seen, "\n"))
}
