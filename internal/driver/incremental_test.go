// Why: the object cache, the build ID and the ThinLTO directory make warm
// builds fast; they are only correct if every build gives the bytes a cold,
// uncached build gives.
// What: TestObjectCacheWarmBuild, TestNoCacheEqualsCached,
// TestReleaseLinkUsesThinLTOCache, TestDeterminismGOMAXPROCS,
// TestCacheVerifyShadowMode and TestConcurrentBuildsShareCache.
// How: builds packages cold, warm, with --no-cache, at different
// GOMAXPROCS and -j, and concurrently, and compares objects, executables
// and --json reports byte for byte.

package driver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	"mk/internal/cache"
	"mk/internal/source"
	"mk/internal/toolchain"
	"mk/internal/types"
)

// These tests cover the object and import-library cache, the build ID, the
// ThinLTO directory, --no-cache and determinism, for the "Recompiles (dev)"
// column of doc/compiler.md's invalidation table.

// incPkg is a package of two modules: lib with two exported functions, and
// the bin root app, which calls both.
func incPkg(libF string) map[string]string {
	return map[string]string{
		"pkg.mk": manifest(map[string]string{"bins": `[pkg.Bin(name: "app", root: "app", link_script: "", cpu: "", subsystem: "console")]`}),
		"lib/lib.mk": "export F(x: int): int {\n    return " + libF + "\n}\n\n" +
			"export G(x: int): int {\n    return x * 3\n}\n",
		"app/app.mk": "import lib\n\nexport Main(sys: &System): int {\n    return lib.F(2) + lib.G(1)\n}\n",
	}
}

// incBuild is one mk build of dir into out with cfg's cache settings; it
// returns the result, the executable's bytes and the bytes of every object
// the link read, in link order. An OnLink hook cfg already has runs first.
func incBuild(t *testing.T, cfg Config, out string) (Result, []byte, [][]byte) {
	t.Helper()
	var objs [][]byte
	prev := cfg.TestHooks.OnLink
	cfg.TestHooks.OnLink = func(j toolchain.LinkJob) {
		if prev != nil {
			prev(j)
		}
		for _, o := range j.Objects {
			b, err := os.ReadFile(o)
			if err != nil {
				t.Errorf("object %s: %v", o, err)
			}
			objs = append(objs, b)
		}
	}
	s, err := NewSession(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r, paths, err := s.Build(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Report.Diags) > 0 || len(paths) != 1 {
		skipKnownBlockers(t, r)
		t.Fatalf("build: %d paths\n%s", len(paths), r.Report.Text(r.Sources))
	}
	exe, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	return r, exe, objs
}

// TestObjectCacheWarmBuild: a cold build compiles every pack; a warm build
// of the unchanged package, in a new Session, compiles none and links the
// same bytes; a body edit of one leaf function recompiles exactly one pack
// (the invalidation table's "Recompiles" column), and its executable equals
// a cold build of the edited source into a fresh cache; a different build
// ID recompiles everything. Both targets: a Linux build is a cross
// build and runs nothing.
func TestObjectCacheWarmBuild(t *testing.T) {
	tools := needTools(t)
	for _, target := range []types.Target{types.X86_64Windows, types.X86_64Linux} {
		t.Run(target.Name, func(t *testing.T) {
			dir := writeTree(t, incPkg("x + 1"))
			cacheDir := filepath.Join(t.TempDir(), "cache")
			out := t.TempDir()
			cfg := Config{Root: dir, Target: target, Tools: tools, CacheDir: cacheDir}
			cold, coldExe, _ := incBuild(t, cfg, filepath.Join(out, "cold"))
			if len(cold.Trace.PacksCompiled) == 0 || len(cold.Trace.PacksFromCache) != 0 || !cold.Trace.Linked {
				t.Fatalf("cold: compiled %v, from cache %v, linked %v", cold.Trace.PacksCompiled, cold.Trace.PacksFromCache, cold.Trace.Linked)
			}
			warm, warmExe, _ := incBuild(t, cfg, filepath.Join(out, "warm"))
			if len(warm.Trace.PacksCompiled) != 0 {
				t.Errorf("warm build with no edit compiled %v", warm.Trace.PacksCompiled)
			}
			if !slices.Equal(warm.Trace.PacksFromCache, cold.Trace.PacksCompiled) {
				t.Errorf("warm from cache %v, want the cold build's %v", warm.Trace.PacksFromCache, cold.Trace.PacksCompiled)
			}
			if !bytes.Equal(coldExe, warmExe) {
				t.Error("warm executable differs from the cold one")
			}

			// Row 3 of invalidation_test.go: a non-generic body edit in lib.
			edited := incPkg("x + 2")
			if err := os.WriteFile(filepath.Join(dir, "lib", "lib.mk"), []byte(edited["lib/lib.mk"]), 0o644); err != nil {
				t.Fatal(err)
			}
			edit, editExe, _ := incBuild(t, cfg, filepath.Join(out, "edit"))
			if len(edit.Trace.PacksCompiled) != 1 {
				t.Errorf("body edit compiled %v, want exactly one pack", edit.Trace.PacksCompiled)
			}
			fresh := Config{Root: writeTree(t, edited), Target: target, Tools: tools, CacheDir: filepath.Join(t.TempDir(), "c2")}
			_, freshExe, _ := incBuild(t, fresh, filepath.Join(out, "fresh"))
			if !bytes.Equal(editExe, freshExe) {
				t.Error("warm build after the edit differs from a cold build of the edited source")
			}
			again, _, _ := incBuild(t, cfg, filepath.Join(out, "again"))
			if len(again.Trace.PacksCompiled) != 0 {
				t.Errorf("third build with no edit compiled %v (the edit's object was not stored)", again.Trace.PacksCompiled)
			}

			// Row 15 of invalidation_test.go: another compiler.
			other := cfg
			other.BuildID = source.HashString("test", "another compiler")
			ver, _, _ := incBuild(t, other, filepath.Join(out, "ver"))
			if len(ver.Trace.PacksFromCache) != 0 || !slices.Equal(ver.Trace.PacksCompiled, again.Trace.PacksFromCache) {
				t.Errorf("new build ID: compiled %v, from cache %v; want every pack compiled", ver.Trace.PacksCompiled, ver.Trace.PacksFromCache)
			}
		})
	}
}

// TestNoCacheEqualsCached: --no-cache writes nothing to the
// cache directory and gives the bytes of a cached build, cold and warm.
func TestNoCacheEqualsCached(t *testing.T) {
	tools := needTools(t)
	for _, target := range []types.Target{types.X86_64Windows, types.X86_64Linux} {
		t.Run(target.Name, func(t *testing.T) {
			dir := writeTree(t, incPkg("x + 1"))
			cacheDir := filepath.Join(t.TempDir(), "cache")
			out := t.TempDir()
			cfg := Config{Root: dir, Target: target, Tools: tools, CacheDir: cacheDir}
			_, cold, coldObjs := incBuild(t, cfg, filepath.Join(out, "cold"))
			// A warm build whose link key hits links nothing, so
			// the cache's copy of the executable goes first and the warm
			// build links the cached objects; the reuse is compared too.
			_, reuse, _ := incBuild(t, cfg, filepath.Join(out, "reuse"))
			if err := os.RemoveAll(cache.DirPath(cacheDir, cache.BinDir)); err != nil {
				t.Fatal(err)
			}
			_, warm, warmObjs := incBuild(t, cfg, filepath.Join(out, "warm"))
			none := cfg
			none.NoCache = true
			none.CacheDir = filepath.Join(t.TempDir(), "unused")
			r, nc, ncObjs := incBuild(t, none, filepath.Join(out, "nocache"))
			if !bytes.Equal(cold, nc) || !bytes.Equal(warm, nc) || !bytes.Equal(reuse, nc) {
				t.Error("--no-cache executable differs from the cached builds'")
			}
			if !objsEqual(coldObjs, ncObjs) || !objsEqual(warmObjs, ncObjs) {
				t.Error("--no-cache objects differ from the cached builds'")
			}
			if len(r.Trace.PacksFromCache) != 0 {
				t.Errorf("--no-cache read packs %v from a cache", r.Trace.PacksFromCache)
			}
			if _, err := os.Stat(none.CacheDir); !os.IsNotExist(err) {
				t.Errorf("--no-cache created its cache directory: %v", err)
			}
		})
	}
}

// TestReleaseLinkUsesThinLTOCache: a release link passes the store's thinlto
// directory, inside the cache root, with a quarter of the budget as its
// pruning bound; --no-cache passes none.
func TestReleaseLinkUsesThinLTOCache(t *testing.T) {
	tools := needTools(t)
	dir := writeTree(t, incPkg("x + 1"))
	cacheDir := filepath.Join(t.TempDir(), "cache")
	for _, noCache := range []bool{false, true} {
		var job toolchain.LinkJob
		s, err := NewSession(Config{Root: dir, Profile: types.Release, Tools: tools, CacheDir: cacheDir, NoCache: noCache,
			TestHooks: TestHooks{OnLink: func(j toolchain.LinkJob) { job = j }}})
		if err != nil {
			t.Fatal(err)
		}
		r, _, err := s.Build(filepath.Join(t.TempDir(), "app.exe"))
		if err != nil || len(r.Report.Diags) > 0 {
			skipKnownBlockers(t, r)
			t.Fatalf("release build: %v\n%s", err, r.Report.Text(r.Sources))
		}
		if noCache {
			if job.LTOCacheDir != "" {
				t.Errorf("--no-cache: LTOCacheDir %q, want none", job.LTOCacheDir)
			}
			continue
		}
		rel, err := filepath.Rel(cacheDir, job.LTOCacheDir)
		if err != nil || strings.HasPrefix(rel, "..") || filepath.Base(job.LTOCacheDir) != "thinlto" {
			t.Errorf("LTOCacheDir %q is not the store's thinlto directory under %s", job.LTOCacheDir, cacheDir)
		}
		if job.LTOCacheSize <= 0 || job.LTOCacheSize > 1<<30 {
			t.Errorf("LTOCacheSize %d, want a quarter of the budget", job.LTOCacheSize)
		}
	}
}

// objsEqual compares two object lists.
func objsEqual(a, b [][]byte) bool {
	return slices.EqualFunc(a, b, bytes.Equal)
}

// TestDeterminismGOMAXPROCS:
// tests/programs/fizz.mk and the two-module package, built with --no-cache
// at GOMAXPROCS 1 and -j 1, then at GOMAXPROCS 8 and -j 8, into different
// directories, give byte-identical objects, executables and --json reports;
// so does a warm build from a cache filled at GOMAXPROCS 1. Dev and release,
// both targets (cross builds; nothing runs).
//
// A build at one worker compiles the whole of std on one core, so only
// fizz, x86_64-windows, release starts at GOMAXPROCS 1; the other seven
// combinations start at 2, which still interleaves the workers, and are
// compared with 8 the same way. With MK_SLOW=1 every combination starts
// at 1.
func TestDeterminismGOMAXPROCS(t *testing.T) {
	tools := needTools(t)
	fizz, err := filepath.Abs(filepath.Join("..", "..", "tests", "programs", "fizz.mk"))
	if err != nil {
		t.Fatal(err)
	}
	roots := map[string]string{"fizz": fizz, "pkg": writeTree(t, incPkg("x + 1"))}
	prev := runtime.GOMAXPROCS(0)
	defer runtime.GOMAXPROCS(prev)
	for _, name := range []string{"fizz", "pkg"} {
		for _, target := range []types.Target{types.X86_64Windows, types.X86_64Linux} {
			for _, prof := range []types.Profile{types.Dev, types.Release} {
				low := 2
				if os.Getenv("MK_SLOW") == "1" || name == "fizz" && target.Name == types.X86_64Windows.Name && prof == types.Release {
					low = 1
				}
				t.Run(fmt.Sprintf("%s/%s/%v", name, target.Name, prof), func(t *testing.T) {
					type run struct {
						exe  []byte
						objs [][]byte
						json []byte
						lls  map[string]string
					}
					build := func(procs int, cfg Config) run {
						runtime.GOMAXPROCS(procs)
						defer runtime.GOMAXPROCS(prev)
						cfg.Root, cfg.Target, cfg.Profile, cfg.Tools, cfg.Jobs = roots[name], target, prof, tools, procs
						var mu sync.Mutex
						lls := map[string]string{}
						cfg.TestHooks.OnLL = func(n, ll string) { mu.Lock(); lls[n] = ll; mu.Unlock() }
						r, exe, objs := incBuild(t, cfg, filepath.Join(t.TempDir(), "out"+exeSuffix(target)))
						var buf bytes.Buffer
						if err := r.WriteReport(&buf, true); err != nil {
							t.Fatal(err)
						}
						if !json.Valid(buf.Bytes()) {
							t.Fatalf("report is not JSON: %s", buf.Bytes())
						}
						return run{exe, objs, buf.Bytes(), lls}
					}
					one := build(low, Config{NoCache: true})
					eight := build(8, Config{NoCache: true})
					cacheDir := filepath.Join(t.TempDir(), "cache")
					build(low, Config{CacheDir: cacheDir})
					// The cache's copy of the executable goes, so
					// the warm build links the cached objects; the next
					// one's link key hits, and it links nothing.
					if err := os.RemoveAll(cache.DirPath(cacheDir, cache.BinDir)); err != nil {
						t.Fatal(err)
					}
					warm := build(8, Config{CacheDir: cacheDir})
					reuse := build(8, Config{CacheDir: cacheDir})
					if len(warm.objs) == 0 || len(reuse.objs) != 0 {
						t.Errorf("warm build linked %d objects, the reuse %d; want a link, then none", len(warm.objs), len(reuse.objs))
					}
					reuse.objs = one.objs
					for label, other := range map[string]run{"GOMAXPROCS 8": eight, "warm": warm, "link reuse": reuse} {
						if !bytes.Equal(one.exe, other.exe) {
							t.Errorf("%s: executable differs", label)
						}
						if !objsEqual(one.objs, other.objs) {
							t.Errorf("%s: objects differ", label)
						}
						if !maps.Equal(one.lls, other.lls) || len(one.lls) == 0 {
							t.Errorf("%s: .ll files differ (%d and %d packs)", label, len(one.lls), len(other.lls))
						}
						if !bytes.Equal(one.json, other.json) {
							t.Errorf("%s: --json differs:\n%s\n%s", label, one.json, other.json)
						}
					}
				})
			}
		}
	}
}

// TestCacheVerifyShadowMode: with MK_CACHE_VERIFY=1 a planted stale object
// (same size, other bytes, so the sidecar still verifies) is detected as
// E0000 where "cache verify", the build goes on with the recomputed object,
// and the executable equals the cold one. A clean cache verifies with no
// diagnostic.
func TestCacheVerifyShadowMode(t *testing.T) {
	tools := needTools(t)
	dir := writeTree(t, incPkg("x + 1"))
	cacheDir := filepath.Join(t.TempDir(), "cache")
	out := t.TempDir()
	cfg := Config{Root: dir, Tools: tools, CacheDir: cacheDir}
	_, coldExe, _ := incBuild(t, cfg, filepath.Join(out, "cold.exe"))
	verify := cfg
	verify.CacheVerify = true
	r, _, _ := incBuild(t, verify, filepath.Join(out, "clean.exe"))
	if len(r.Report.Diags) != 0 || len(r.Trace.PacksCompiled) != 0 {
		t.Fatalf("verify over a clean cache: %v, compiled %v", codes(r), r.Trace.PacksCompiled)
	}
	var objs []string
	filepath.WalkDir(filepath.Join(cacheDir, "v1", "obj"), func(p string, d os.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() && filepath.Ext(p) != ".sum" {
			objs = append(objs, p)
		}
		return nil
	})
	if len(objs) == 0 {
		t.Fatal("no stored object under v1/obj")
	}
	slices.Sort(objs)
	b, err := os.ReadFile(objs[0])
	if err != nil {
		t.Fatal(err)
	}
	orig := bytes.Clone(b)
	b[len(b)/2] ^= 0xff
	if err := os.WriteFile(objs[0], b, 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := NewSession(Config{Root: dir, Tools: tools, CacheDir: cacheDir, CacheVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	r, paths, err := s.Build(filepath.Join(out, "stale.exe"))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Report.Diags) != 1 || fact(r.Report.Diags[0].Diag, "where") != "cache verify" || !strings.HasPrefix(fact(r.Report.Diags[0].Diag, "what"), "obj ") {
		t.Fatalf("stale object under verify: %s", r.Report.Text(r.Sources))
	}
	if ExitStatus(r.Report) != 3 || len(paths) != 1 {
		t.Fatalf("exit %d, %d paths", ExitStatus(r.Report), len(paths))
	}
	exe, _ := os.ReadFile(paths[0])
	if !bytes.Equal(exe, coldExe) {
		t.Error("the build under verify did not use the recomputed object")
	}
	// The stale entry was replaced, read directly: which pack's object
	// sorts first changes with the build ID, and a stale copy of some of
	// them still links the cold executable, so the executable alone cannot
	// show a missing repair.
	if got, err := os.ReadFile(objs[0]); err != nil || !bytes.Equal(got, orig) {
		t.Errorf("the stale object %s was not replaced by the recomputed one (%v)", filepath.Base(objs[0]), err)
	}
	// A plain warm build then links the right bytes and compiles nothing.
	// The stored links go first, so that build links the stored objects
	// rather than copying a stored executable out.
	if err := os.RemoveAll(cache.DirPath(cacheDir, cache.BinDir)); err != nil {
		t.Fatal(err)
	}
	r, after, _ := incBuild(t, cfg, filepath.Join(out, "after.exe"))
	if !r.Trace.Linked || !bytes.Equal(after, coldExe) || len(r.Trace.PacksCompiled) != 0 {
		t.Errorf("after the verify: linked %v, executable equal %v, compiled %v", r.Trace.Linked, bytes.Equal(after, coldExe), r.Trace.PacksCompiled)
	}
}

// TestConcurrentBuildsShareCache: four builds of one package into one
// cache at once (each cold build of std holds about 2 GB, so eight ran a
// 16 GB runner out of memory) all succeed with byte-identical
// executables, and a build after them compiles nothing.
func TestConcurrentBuildsShareCache(t *testing.T) {
	tools := needTools(t)
	dir := writeTree(t, incPkg("x + 1"))
	cacheDir := filepath.Join(t.TempDir(), "cache")
	out := t.TempDir()
	exes := make([][]byte, 4)
	errs := make([]string, 4)
	done := make(chan int)
	for i := range exes {
		go func() {
			defer func() { done <- i }()
			s, err := NewSession(Config{Root: dir, Tools: tools, CacheDir: cacheDir, Jobs: 2})
			if err != nil {
				errs[i] = err.Error()
				return
			}
			r, paths, err := s.Build(filepath.Join(out, fmt.Sprintf("app%d.exe", i)))
			if err != nil || len(r.Report.Diags) > 0 || len(paths) != 1 {
				errs[i] = fmt.Sprintf("%v %s", err, r.Report.Text(r.Sources))
				return
			}
			exes[i], _ = os.ReadFile(paths[0])
		}()
	}
	for range exes {
		<-done
	}
	for i := range exes {
		if errs[i] != "" {
			t.Fatalf("build %d: %s", i, errs[i])
		}
		if !bytes.Equal(exes[i], exes[0]) || len(exes[i]) == 0 {
			t.Errorf("build %d: executable differs", i)
		}
	}
	r, _, _ := incBuild(t, Config{Root: dir, Tools: tools, CacheDir: cacheDir}, filepath.Join(out, "after.exe"))
	if len(r.Trace.PacksCompiled) != 0 {
		t.Errorf("after the concurrent builds: compiled %v", r.Trace.PacksCompiled)
	}
}
