// Why: a warm or edit build generates IR only for packs whose inputs
// changed, because each pack is keyed before emission. The soundness claim
// is "equal key means equal object", and a missed input links a stale one.
// What: pkFiles, pkEdits and the pack-key tests: one edit per input
// codegen reads, release-only inlining marks that cross packs, and the
// forced and unkeyed paths.
// How: each edit must miss exactly the packs that read the input, and every
// build is followed by an MK_CACHE_VERIFY build of the same tree, which
// emits and compiles every pack, checks each memo entry against the
// emitted IR, and must report nothing and link the same bytes.

package driver

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	"mk/internal/cache"
	"mk/internal/source"
	"mk/internal/types"
)

// pkFiles is the program of the pre-emission key tests: three modules, so three
// packs of user code at least (a pack never crosses a module). lib.Size
// names only shape.Outer, never shape.Inner; lib.Tag reads an i8 enum;
// lib.Helper is called once, from lib.Wrap in its own pack (release marks
// that call alwaysinline, solecall.go); lib.Counter is defined in lib's
// pack and read only by app.
func pkFiles() map[string]string {
	return map[string]string{
		"pkg.mk": manifest(map[string]string{"bins": `[pkg.Bin(name: "app", root: "app", link_script: "", cpu: "", subsystem: "console")]`}),
		"shape/shape.mk": "export struct Inner {\n    export a: int\n    export b: int\n}\n\n" +
			"export struct Outer {\n    export inner: Inner\n    export n: int\n}\n\n" +
			"export enum Color: i8 {\n    Red = 1\n    Green = -2\n}\n\n" +
			"export MakeOuter(n: int): Outer {\n    return Outer(inner: Inner(a: n, b: n), n: n)\n}\n",
		"lib/lib.mk": "import shape\n\nexport var Counter: int = 5\n\n" +
			"export F(x: int): int {\n    return x + 1\n}\n\n" +
			"export Size(o: &shape.Outer): int {\n    return o.n\n}\n\n" +
			"export Helper(x: int): int {\n    return x * 7 + 3\n}\n\n" +
			"export Wrap(x: int): int {\n    return Helper(x) + 1\n}\n\n" +
			"export Tag(c: shape.Color): int {\n    return int(c)\n}\n",
		"app/app.mk": "import lib\nimport shape\n\nexport Main(sys: &System): int {\n    o := shape.MakeOuter(3)\n" +
			"    return lib.F(2) + lib.Size(&o) + lib.Wrap(1) + lib.Tag(shape.Color.Green) + lib.Counter - 26\n}\n",
	}
}

// pkTarget is the host's target, so the tests run natively in WSL too
// (the Linux pass).
func pkTarget() types.Target {
	if runtime.GOOS == "windows" {
		return types.X86_64Windows
	}
	return types.X86_64Linux
}

// pkRun is one build: its pack events, the executable and the trace.
type pkRun struct {
	events []PackEvent
	exe    []byte
	r      Result
}

// emittedMods is the set of module paths whose packs were emitted (by the
// first member's key text, "mod.Name..."), "support" for the support pack.
func (p pkRun) emittedMods() []string {
	var out []string
	for _, e := range p.events {
		if !e.Emitted {
			continue
		}
		m := "support"
		if e.First != "" {
			m, _, _ = strings.Cut(e.First, ".")
		}
		out = append(out, m)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// pkBuild is one mk build of cfg into out, recording the pack events.
func pkBuild(t *testing.T, cfg Config, out string) pkRun {
	t.Helper()
	var mu sync.Mutex
	var run pkRun
	cfg.TestHooks.OnPack = func(e PackEvent) {
		mu.Lock()
		run.events = append(run.events, e)
		mu.Unlock()
	}
	run.r, run.exe, _ = incBuild(t, cfg, out)
	if !slices.Equal(run.r.Trace.PacksEmitted, func() []string {
		var n []string
		for _, e := range run.events {
			if e.Emitted {
				n = append(n, e.Name)
			}
		}
		slices.Sort(n)
		return n
	}()) {
		t.Errorf("Trace.PacksEmitted %v disagrees with the pack events", run.r.Trace.PacksEmitted)
	}
	return run
}

// pkVerify builds cfg's tree again under MK_CACHE_VERIFY: every pack is
// emitted and compiled, each memo entry is checked against its IR (a
// mismatch is an E0000 diagnostic, which incBuild fails on), and the
// executable must have want's bytes.
func pkVerify(t *testing.T, cfg Config, out string, want []byte) {
	t.Helper()
	cfg.CacheVerify = true
	v := pkBuild(t, cfg, out)
	for _, e := range v.events {
		if !e.Emitted {
			t.Errorf("verify build: pack %s (%s) not emitted", e.Name, e.First)
		}
	}
	if !bytes.Equal(v.exe, want) {
		t.Error("the verify build's executable differs from the build before it: an object was reused stale")
	}
}

// pkWrite replaces files of the tree at dir.
func pkWrite(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for p, text := range files {
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(p)), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// pkEdit is one edit of pkFiles: the files it rewrites (by replacing old
// with new), the modules whose packs must be emitted, and, when exact,
// that no other user module's pack is.
type pkEdit struct {
	name  string
	file  string
	old   string
	new   string
	more  map[string][2]string // further replacements in other files
	want  []string
	exact bool
}

// pkEdits are the inputs a pack's IR reads, one per edit.
var pkEdits = []pkEdit{
	{name: "body", file: "lib/lib.mk", old: "return x + 1", new: "return x + 2", want: []string{"lib"}, exact: true},
	// A position-only edit: the overflow check's site row and the line
	// table of lib's functions move, nothing else does.
	{name: "position", file: "lib/lib.mk", old: "import shape\n", new: "import shape\n\n\n", want: []string{"lib"}, exact: true},
	// Inner changes, size and alignment kept; lib.Size names only Outer,
	// whose own layout is unchanged: the deep layout part misses it.
	{name: "nested-layout", file: "shape/shape.mk", old: "    export b: int\n}", new: "    export b: i32\n    export c: i32\n}",
		more: map[string][2]string{"shape/shape.mk": {"Inner(a: n, b: n)", "Inner(a: n, b: 1, c: 2)"}}, want: []string{"lib", "shape"}},
	// The tag's signedness is in the declaration, not the layout.
	{name: "enum-tag", file: "shape/shape.mk", old: "Color: i8 {\n    Red = 1\n    Green = -2", new: "Color: u8 {\n    Red = 1\n    Green = 2",
		want: []string{"lib"}},
	// A global's initial value: lib's pack defines it, and no lib body
	// reads it.
	{name: "global-value", file: "lib/lib.mk", old: "Counter: int = 5", new: "Counter: int = 6", want: []string{"lib"}},
	// A second call of Helper, from another pack: in dev the solecall
	// rule does not run, so lib's key does not move.
	{name: "callsite-dev", file: "app/app.mk", old: "lib.Counter - 26", new: "lib.Counter + lib.Helper(0) - 29", want: []string{"app"}, exact: true},
}

// apply returns base with e applied.
func (e pkEdit) apply(t *testing.T, base map[string]string) map[string]string {
	t.Helper()
	out := map[string]string{}
	rep := func(f, old, new string) {
		src := base[f]
		if v, ok := out[f]; ok {
			src = v
		}
		if !strings.Contains(src, old) {
			t.Fatalf("edit %s: %q not in %s", e.name, old, f)
		}
		out[f] = strings.Replace(src, old, new, 1)
	}
	rep(e.file, e.old, e.new)
	for f, r := range e.more {
		rep(f, r[0], r[1])
	}
	return out
}

// TestEditsMissTheirPacks: a cold build keys and emits every pack; a
// warm build with no change emits only the support pack and links the
// same bytes; then each edit of pkEdits, applied alone to the base tree,
// emits the packs that read the edited input (only those, for an exact
// edit), and its MK_CACHE_VERIFY build reports no stale memo and links
// the same bytes; reverting it emits only the support pack again. Dev,
// on the host's target.
func TestEditsMissTheirPacks(t *testing.T) {
	tools := needTools(t)
	base := pkFiles()
	dir := writeTree(t, base)
	out := t.TempDir()
	cfg := Config{Root: dir, Target: pkTarget(), Profile: types.Dev, Tools: tools,
		CacheDir: filepath.Join(t.TempDir(), "cache")}
	exe := func(n string) string { return filepath.Join(out, n+".exe") }
	cold := pkBuild(t, cfg, exe("cold"))
	for _, e := range cold.events {
		if !e.Emitted || e.HasPre != (e.Name != "support") {
			t.Errorf("cold build: pack %s (%s) emitted %v, keyed %v", e.Name, e.First, e.Emitted, e.HasPre)
		}
	}
	if got := cold.emittedMods(); !slices.Contains(got, "lib") || !slices.Contains(got, "app") || !slices.Contains(got, "shape") {
		t.Fatalf("cold build emitted %v, want lib, app and shape among them", got)
	}
	warm := pkBuild(t, cfg, exe("warm"))
	if got := warm.emittedMods(); !slices.Equal(got, []string{"support"}) {
		t.Errorf("warm build with no change emitted %v, want the support pack only", got)
	}
	if !bytes.Equal(warm.exe, cold.exe) {
		t.Error("the warm build's executable differs from the cold build's")
	}
	pkVerify(t, cfg, exe("warm-verify"), warm.exe)
	for _, e := range pkEdits {
		t.Run(e.name, func(t *testing.T) {
			pkWrite(t, dir, e.apply(t, base))
			defer pkWrite(t, dir, base)
			ed := pkBuild(t, cfg, exe(e.name))
			got := ed.emittedMods()
			for _, m := range e.want {
				if !slices.Contains(got, m) {
					t.Errorf("emitted %v, want %s among them", got, m)
				}
			}
			if e.exact && !slices.Equal(got, append(slices.Clone(e.want), "support")) {
				t.Errorf("emitted %v, want exactly %v and the support pack", got, e.want)
			}
			pkVerify(t, cfg, exe(e.name+"-verify"), ed.exe)
			pkWrite(t, dir, base)
			back := pkBuild(t, cfg, exe(e.name+"-back"))
			if got := back.emittedMods(); !slices.Equal(got, []string{"support"}) {
				t.Errorf("after reverting: emitted %v, want the support pack only", got)
			}
			if !bytes.Equal(back.exe, cold.exe) {
				t.Error("after reverting: the executable differs from the cold build's")
			}
		})
	}
	// A target fact: another CPU misses every pack.
	t.Run("cpu", func(t *testing.T) {
		c := cfg
		c.CPU = "x86-64-v3"
		r := pkBuild(t, c, exe("cpu"))
		for _, e := range r.events {
			if !e.Emitted {
				t.Errorf("--cpu x86-64-v3: pack %s (%s) not emitted", e.Name, e.First)
			}
		}
		pkVerify(t, c, exe("cpu-verify"), r.exe)
	})
}

// TestReleaseCallSiteInAnotherPack: in release, lib.Wrap's
// call of lib.Helper is the only one, so it is marked alwaysinline. A
// second call from app, with lib's source unchanged, removes the mark: the
// key of lib's pack must move with it.
func TestReleaseCallSiteInAnotherPack(t *testing.T) {
	tools := needTools(t)
	base := pkFiles()
	dir := writeTree(t, base)
	out := t.TempDir()
	cfg := Config{Root: dir, Target: pkTarget(), Profile: types.Release, Tools: tools,
		CacheDir: filepath.Join(t.TempDir(), "cache")}
	pkBuild(t, cfg, filepath.Join(out, "cold.exe"))
	e := pkEdit{name: "callsite-release", file: "app/app.mk", old: "lib.Counter - 26", new: "lib.Counter + lib.Helper(0) - 29"}
	pkWrite(t, dir, e.apply(t, base))
	ed := pkBuild(t, cfg, filepath.Join(out, "edit.exe"))
	if got := ed.emittedMods(); !slices.Contains(got, "lib") || !slices.Contains(got, "app") {
		t.Errorf("emitted %v, want lib (its solecall mark moved) and app", got)
	}
	pkVerify(t, cfg, filepath.Join(out, "verify.exe"), ed.exe)
}

// TestReleaseSmallCalleeInAnotherPack: in release, app.G calls lib.Mix
// twice and both are small (codegen's smallfn.go), so G's definition carries
// alwaysinline. Mix grows, still small, until G's effect (its own operations
// plus Mix's per call) is over the bound: app's source is unchanged, but its
// IR is not (G loses the mark, and Main, which counted G, gains it), so
// app's pack key must move with lib's body. Without the small-function facts
// in PackFacts app's pack hits its stale memo, which the MK_CACHE_VERIFY
// build reports.
func TestReleaseSmallCalleeInAnotherPack(t *testing.T) {
	tools := needTools(t)
	base := pkFiles()
	base["lib/lib.mk"] += "\nexport Mix(x: int): int {\n    return x ^ (x >> 3)\n}\n"
	app := base["app/app.mk"]
	app = strings.Replace(app, "import shape\n", "import shape\n\nG(x: int): int {\n    return lib.Mix(x) +% lib.Mix(x +% 1)\n}\n", 1)
	app = strings.Replace(app, "lib.Counter - 26", "lib.Counter + G(1) - G(1) - 26", 1)
	base["app/app.mk"] = app
	dir := writeTree(t, base)
	out := t.TempDir()
	cfg := Config{Root: dir, Target: pkTarget(), Profile: types.Release, Tools: tools,
		CacheDir: filepath.Join(t.TempDir(), "cache")}
	pkBuild(t, cfg, filepath.Join(out, "cold.exe"))
	e := pkEdit{name: "small-callee", file: "lib/lib.mk", old: "return x ^ (x >> 3)", new: "return x ^ (x >> 3) ^ (x << 5) ^ (x >> 7) ^ (x << 9)"}
	pkWrite(t, dir, e.apply(t, base))
	ed := pkBuild(t, cfg, filepath.Join(out, "edit.exe"))
	if got := ed.emittedMods(); !slices.Contains(got, "lib") || !slices.Contains(got, "app") {
		t.Errorf("emitted %v, want lib (Mix's body) and app (G's and Main's small marks moved)", got)
	}
	pkVerify(t, cfg, filepath.Join(out, "verify.exe"), ed.exe)
}

// TestReleaseCrossSoleCallerInAnotherPack: in release, lib.Long, past
// the small-function bound, has one call, from app's Main, so its definition
// carries the cross-pack sole-call mark (codegen's smallfn.go). A second
// call in app, with lib's source unchanged, removes the mark: lib's pack key
// must move with it. Without the marks in PackFacts lib's pack hits its
// stale memo, which the MK_CACHE_VERIFY build reports.
func TestReleaseCrossSoleCallerInAnotherPack(t *testing.T) {
	tools := needTools(t)
	base := pkFiles()
	var long strings.Builder
	long.WriteString("\nexport Long(x: int): int {\n    var y := x\n")
	for i := range 12 {
		fmt.Fprintf(&long, "    y = y ^ (y >> %d)\n", i+1)
	}
	long.WriteString("    return y\n}\n")
	base["lib/lib.mk"] += long.String()
	base["app/app.mk"] = strings.Replace(base["app/app.mk"], "lib.Counter - 26", "lib.Counter + lib.Long(0) - 26", 1)
	dir := writeTree(t, base)
	out := t.TempDir()
	cfg := Config{Root: dir, Target: pkTarget(), Profile: types.Release, Tools: tools,
		CacheDir: filepath.Join(t.TempDir(), "cache")}
	pkBuild(t, cfg, filepath.Join(out, "cold.exe"))
	e := pkEdit{name: "cross-sole", file: "app/app.mk", old: "lib.Long(0) - 26", new: "lib.Long(0) + lib.Long(0) - 26"}
	pkWrite(t, dir, e.apply(t, base))
	ed := pkBuild(t, cfg, filepath.Join(out, "edit.exe"))
	if got := ed.emittedMods(); !slices.Contains(got, "lib") || !slices.Contains(got, "app") {
		t.Errorf("emitted %v, want lib (Long's cross-pack mark moved) and app", got)
	}
	pkVerify(t, cfg, filepath.Join(out, "verify.exe"), ed.exe)
}

// TestReleaseCalleeEffectLoopInAnotherPack: app's Loop calls app.Step once, and
// Step calls lib.Advance once. Advance is past the small bound, so it
// takes the cross-pack sole-call mark; while its effect has no loop,
// Loop's call of Step carries the call-site alwaysinline of the sole-call
// rule. The edit gives Advance a loop: no member of app changes its own
// marks, but Step's effect now has a loop, so Loop's call of Step loses
// its mark and app's IR changes. app's pack key must move; without the
// per-call facts PackFacts writes (the callee's marks and effect) it hits
// its stale memo, which the MK_CACHE_VERIFY build reports. The edit also
// changes Advance's operation count; codegen's
// TestPackFactsCalleeLoop moves the loop alone.
func TestReleaseCalleeEffectLoopInAnotherPack(t *testing.T) {
	tools := needTools(t)
	base := pkFiles()
	var adv strings.Builder
	adv.WriteString("\nexport Advance(x: int): int {\n    var y := x\n")
	for i := range 12 {
		fmt.Fprintf(&adv, "    y = y ^ (y >> %d)\n", i+1)
	}
	adv.WriteString("    return y\n}\n")
	base["lib/lib.mk"] += adv.String()
	app := base["app/app.mk"]
	app = strings.Replace(app, "import shape\n", "import shape\n\nStep(x: int): int {\n    return lib.Advance(x) +% 1\n}\n\n"+
		"Loop(n: int): int {\n    var s := 0\n    for i in 0..n {\n        s = s +% Step(i)\n    }\n    return s\n}\n", 1)
	app = strings.Replace(app, "lib.Counter - 26", "lib.Counter + Loop(0) - 26", 1)
	base["app/app.mk"] = app
	dir := writeTree(t, base)
	out := t.TempDir()
	cfg := Config{Root: dir, Target: pkTarget(), Profile: types.Release, Tools: tools,
		CacheDir: filepath.Join(t.TempDir(), "cache")}
	pkBuild(t, cfg, filepath.Join(out, "cold.exe"))
	e := pkEdit{name: "callee-loop", file: "lib/lib.mk", old: "    return y\n}\n",
		new: "    for _ in 0..3 {\n        y = y ^ (y >> 1)\n    }\n    return y\n}\n"}
	pkWrite(t, dir, e.apply(t, base))
	ed := pkBuild(t, cfg, filepath.Join(out, "edit.exe"))
	if got := ed.emittedMods(); !slices.Contains(got, "lib") || !slices.Contains(got, "app") {
		t.Errorf("emitted %v, want lib (Advance's body) and app (Loop's call of Step lost its mark)", got)
	}
	pkVerify(t, cfg, filepath.Join(out, "verify.exe"), ed.exe)
}

// TestReleaseCalleeBodyLoopInAnotherPack: app's Loop calls app.M once, M
// calls lib.J once, and the thin J calls lib2.Y and lib.Z once each, Z (not
// thin) calling the looping lib.Q. J takes the cross-pack sole-call mark,
// and while Y has no loop none ends up in J's body, so Loop's call of M
// carries the sole-call mark. The edit gives Y a loop at the same
// operation count: J's operations, its effect's loop (Z counts Q's) and
// its marks stay as they were, but Y's loop now ends up in the thin J, so
// M carries it and Loop's call of M loses its mark. app's pack key must
// move; without the per-call loop bit PackFacts writes (codegen's
// smallLoops) it hits its stale memo, which the MK_CACHE_VERIFY build
// reports. Codegen's TestPackFactsCalleeBodyLoop moves the same bit.
func TestReleaseCalleeBodyLoopInAnotherPack(t *testing.T) {
	tools := needTools(t)
	y := func(loop bool) string {
		var b strings.Builder
		b.WriteString("export Y(x: int): int {\n    var y := x\n")
		first, last := 1, 18
		if loop {
			b.WriteString("    for _ in 0..3 {\n        y = y ^ (y >> 1)\n    }\n")
			first, last = 2, 16
		}
		for i := first; i <= last; i++ {
			fmt.Fprintf(&b, "    y = y ^ (y >> %d)\n", i)
		}
		b.WriteString("    return y\n}\n")
		return b.String()
	}
	base := map[string]string{
		"pkg.mk": manifest(map[string]string{"bins": `[pkg.Bin(name: "app", root: "app", link_script: "", cpu: "", subsystem: "console")]`}),
		"app/app.mk": "import lib\n\nM(x: int): int {\n    return lib.J(x)\n}\n\n" +
			"Loop(n: int): int {\n    var s := 0\n    for i in 0..n {\n        s = s +% M(i)\n    }\n    return s\n}\n\n" +
			"export Main(sys: &System): WriteError!void {\n    try sys.Print(\"{v}\\n\", (v: Loop(sys.args.Len() + 4)))\n}\n",
		"lib/lib.mk": "import lib2\n\nexport Q(x: int): int {\n    var s := x\n    for i in 0..x {\n        s = s +% i\n    }\n    return s\n}\n\n" +
			"Z(x: int): int {\n    var r := x\n    if x > 1 {\n        r = r +% 1\n    }\n    if x > 2 {\n        r = r +% 2\n    }\n" +
			"    if x > 3 {\n        r = r +% 3\n    }\n    return Q(r)\n}\n\n" +
			"export J(x: int): int {\n    return Z(lib2.Y(x))\n}\n",
		"lib2/lib2.mk": y(false),
	}
	dir := writeTree(t, base)
	out := t.TempDir()
	cfg := Config{Root: dir, Target: pkTarget(), Profile: types.Release, Tools: tools,
		CacheDir: filepath.Join(t.TempDir(), "cache")}
	pkBuild(t, cfg, filepath.Join(out, "cold.exe"))
	pkWrite(t, dir, map[string]string{"lib2/lib2.mk": y(true)})
	ed := pkBuild(t, cfg, filepath.Join(out, "edit.exe"))
	if got := ed.emittedMods(); !slices.Contains(got, "lib2") || !slices.Contains(got, "app") {
		t.Errorf("emitted %v, want lib2 (Y's body) and app (Loop's call of M lost its mark)", got)
	}
	pkVerify(t, cfg, filepath.Join(out, "verify.exe"), ed.exe)
}

// TestStdEditMissesEveryPack: under MK_STD (Config.StdDir) a std
// file's change is not in the build ID; the front cache's context folds
// every std file's hash, and the pack keys hold that context, so every
// pack misses, user packs included.
func TestStdEditMissesEveryPack(t *testing.T) {
	tools := needTools(t)
	std := filepath.Join(t.TempDir(), "std")
	if err := os.CopyFS(std, os.DirFS(filepath.Join("..", "..", "std"))); err != nil {
		t.Fatal(err)
	}
	dir := writeTree(t, pkFiles())
	out := t.TempDir()
	cfg := Config{Root: dir, Target: pkTarget(), Profile: types.Dev, Tools: tools, StdDir: std,
		CacheDir: filepath.Join(t.TempDir(), "cache")}
	pkBuild(t, cfg, filepath.Join(out, "cold.exe"))
	f := filepath.Join(std, "core", "coll_list.mk")
	src, err := os.ReadFile(f)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f, append([]byte("// test edit\n"), src...), 0o644); err != nil {
		t.Fatal(err)
	}
	ed := pkBuild(t, cfg, filepath.Join(out, "edit.exe"))
	for _, e := range ed.events {
		if !e.Emitted {
			t.Errorf("after a std edit: pack %s (%s) not emitted", e.Name, e.First)
		}
	}
	pkVerify(t, cfg, filepath.Join(out, "verify.exe"), ed.exe)
}

// TestForcedAndUnkeyed: the OnLL hook (as --emit-ll uses it)
// makes every pack emit on a warm build; --debug forms no pre-emission
// key (codegen.PackFacts refuses it), so it emits every pack.
func TestForcedAndUnkeyed(t *testing.T) {
	tools := needTools(t)
	dir := writeTree(t, pkFiles())
	out := t.TempDir()
	cfg := Config{Root: dir, Target: pkTarget(), Profile: types.Dev, Tools: tools,
		CacheDir: filepath.Join(t.TempDir(), "cache")}
	pkBuild(t, cfg, filepath.Join(out, "cold.exe"))
	ll := cfg
	n := 0
	var mu sync.Mutex
	ll.TestHooks.OnLL = func(string, string) { mu.Lock(); n++; mu.Unlock() }
	r := pkBuild(t, ll, filepath.Join(out, "ll.exe"))
	for _, e := range r.events {
		if !e.Emitted || e.HasPre {
			t.Errorf("with OnLL: pack %s emitted %v, keyed %v; want emitted and unkeyed", e.Name, e.Emitted, e.HasPre)
		}
	}
	if n != len(r.events) {
		t.Errorf("OnLL saw %d packs of %d", n, len(r.events))
	}
	dbg := cfg
	dbg.Debug = true
	for i := range 2 {
		r := pkBuild(t, dbg, filepath.Join(out, "debug.exe"))
		for _, e := range r.events {
			if !e.Emitted || e.HasPre {
				t.Errorf("--debug build %d: pack %s emitted %v, keyed %v; want emitted and unkeyed", i, e.Name, e.Emitted, e.HasPre)
			}
		}
	}
}

// TestWrongMemoIsCaught: a memo entry that names another pack's
// object (a stale key, which MK_CACHE_VERIFY exists to catch) is reported by
// MK_CACHE_VERIFY as E0000 "cache verify ... prekey" and replaced, so the
// next plain build links the right bytes. A memo that names no stored
// object is a miss: the pack is emitted.
func TestWrongMemoIsCaught(t *testing.T) {
	tools := needTools(t)
	dir := writeTree(t, pkFiles())
	out := t.TempDir()
	cacheDir := filepath.Join(t.TempDir(), "cache")
	cfg := Config{Root: dir, Target: pkTarget(), Profile: types.Dev, Tools: tools, CacheDir: cacheDir}
	cold := pkBuild(t, cfg, filepath.Join(out, "cold.exe"))
	var keyed []PackEvent
	for _, e := range cold.events {
		if e.HasPre {
			keyed = append(keyed, e)
		}
	}
	if len(keyed) < 2 {
		t.Fatalf("cold build keyed %d packs, want 2 at least", len(keyed))
	}
	slices.SortFunc(keyed, func(a, b PackEvent) int { return strings.Compare(a.Name, b.Name) })
	victim, other := keyed[0], keyed[1]
	st, err := cache.Open(cacheDir, cache.Options{})
	if err != nil {
		t.Fatal(err)
	}
	// A memo naming a key with no object: a miss.
	memoPut(st, victim.Pre, source.HashString("mk.test.d787", "absent"))
	r := pkBuild(t, cfg, filepath.Join(out, "absent.exe"))
	for _, e := range r.events {
		if e.Name == victim.Name && !e.Emitted {
			t.Errorf("a memo naming no object: pack %s not emitted", e.Name)
		}
	}
	if !bytes.Equal(r.exe, cold.exe) {
		t.Error("after a memo naming no object: the executable differs")
	}
	// A memo naming another pack's object: only verify can see it.
	memoPut(st, victim.Pre, other.Obj)
	vcfg := cfg
	vcfg.CacheVerify = true
	s, err := NewSession(vcfg)
	if err != nil {
		t.Fatal(err)
	}
	vr, _, err := s.Build(filepath.Join(out, "verify.exe"))
	if err != nil {
		t.Fatal(err)
	}
	text := vr.Report.Text(vr.Sources)
	if !strings.Contains(text, "E0000") || !strings.Contains(text, "cache verify") || !strings.Contains(text, "prekey "+victim.Pre.String()) {
		t.Errorf("MK_CACHE_VERIFY with a wrong memo reported:\n%s\nwant E0000 cache verify naming prekey %s", text, victim.Pre)
	}
	after := pkBuild(t, cfg, filepath.Join(out, "after.exe"))
	if !bytes.Equal(after.exe, cold.exe) {
		t.Error("after the verify build replaced the memo: the executable differs from the cold build's")
	}
	if got, ok := memoGet(st, victim.Pre); !ok || got != victim.Obj {
		t.Errorf("memo of %s after verify: %v %v, want %v", victim.Name, got, ok, victim.Obj)
	}
}
