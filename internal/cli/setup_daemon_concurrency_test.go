package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// pathVaryingSpec is fixedDaemonSpec with a different inherited search path and
// nothing else changed. Two shells on one machine differ exactly this much.
func pathVaryingSpec(path string) daemonSpec {
	spec := fixedDaemonSpec()
	env := make(map[string]string, len(spec.Env))
	for key, value := range spec.Env {
		env[key] = value
	}
	env["PATH"] = path
	spec.Env = env
	return spec
}

func writeUnit(t *testing.T, plan daemonPlan) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(plan.UnitPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plan.UnitPath, []byte(plan.Contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The installed watcher is ONE machine-wide service and `setup` repairs a unit
// it reads as stale by unloading and reloading it -- killing an in-flight
// refresh. The unit carries the installing shell's PATH because a launchd job
// inherits almost nothing, so before this fix installing any tool, opening a
// different terminal, or a shell rc that prepended a directory twice made the
// bytes differ and every later `setup` tore the live watcher down and restarted
// it, while `status` told the user their running daemon was stale.
func TestDaemonUnitStaysCurrentWhenOnlyThePathChanged(t *testing.T) {
	t.Parallel()
	for _, goos := range []string{"darwin", "linux"} {
		t.Run(goos, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			installed, err := planBrainWatchDaemon(goos, "/Users/demo", "", dir, pathVaryingSpec("/usr/bin:/bin"))
			if err != nil {
				t.Fatalf("plan: %v", err)
			}
			writeUnit(t, installed)

			// Same machine, same daemon, a shell that has since gained a tool.
			current, err := planBrainWatchDaemon(goos, "/Users/demo", "", dir, pathVaryingSpec("/opt/newtool/bin:/usr/bin:/bin:/usr/bin"))
			if err != nil {
				t.Fatalf("plan: %v", err)
			}
			current.NoRegister = true
			if installed.Contents == current.Contents {
				t.Fatal("the two units must differ byte-for-byte, or this test proves nothing")
			}
			state := inspectDaemon(context.Background(), nil, current)
			if !state.Installed {
				t.Fatalf("the unit is on disk at %s", current.UnitPath)
			}
			if !state.Current {
				t.Fatalf("a changed PATH must not make the running watcher stale;\n--- installed ---\n%s\n--- rendered ---\n%s",
					installed.Contents, current.Contents)
			}
		})
	}
}

// The masking has to be narrow. Everything that decides what the daemon DOES
// still has to read as drift, or a genuinely stale unit would never be
// repaired.
func TestDaemonUnitIsStaleWhenSomethingMaterialChanged(t *testing.T) {
	t.Parallel()
	base := pathVaryingSpec("/usr/bin:/bin")
	cases := map[string]func(*daemonSpec){
		"binary":      func(s *daemonSpec) { s.Binary = "/opt/entire/bin/entire-brain-next" },
		"args":        func(s *daemonSpec) { s.Args = append(append([]string{}, s.Args...), "--jobs", "4") },
		"log path":    func(s *daemonSpec) { s.LogPath = "/var/state/entire/logs/other.log" },
		"working dir": func(s *daemonSpec) { s.WorkingDir = "/opt/elsewhere" },
		"plugin dir": func(s *daemonSpec) {
			env := map[string]string{}
			for k, v := range s.Env {
				env[k] = v
			}
			env["ENTIRE_PLUGIN_DATA_DIR"] = "/var/data/other"
			s.Env = env
		},
		"path removed": func(s *daemonSpec) {
			env := map[string]string{}
			for k, v := range s.Env {
				if k != "PATH" {
					env[k] = v
				}
			}
			s.Env = env
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			installed, err := planBrainWatchDaemon("darwin", "/Users/demo", "", dir, base)
			if err != nil {
				t.Fatal(err)
			}
			writeUnit(t, installed)
			changed := base
			mutate(&changed)
			current, err := planBrainWatchDaemon("darwin", "/Users/demo", "", dir, changed)
			if err != nil {
				t.Fatal(err)
			}
			current.NoRegister = true
			if state := inspectDaemon(context.Background(), nil, current); state.Current {
				t.Fatalf("a changed %s must be repaired, not masked", name)
			}
		})
	}
}

// `setup` decides what to do about the service with inspect() and then acts
// with install(), and install() on darwin is `launchctl unload` followed by
// `launchctl load` with the service DOWN in between. Two repos onboarding at
// once is the ordinary case; unserialized, one run's load lands between the
// other's unload and load and the loser reports "daemon install failed" for a
// watcher that is in fact running.
func TestConcurrentSetupsDoNotInterleaveTheDaemonRegistration(t *testing.T) {
	f := newSetupTestFixture(t)
	plan, err := brainWatchDaemonPlanFor("darwin", f.opts, defaultSetupOptions())
	if err != nil {
		t.Fatalf("plan: %v", err)
	}

	type event struct {
		racer int
		what  string
	}
	var mu sync.Mutex
	var events []event
	record := func(racer int, what string) {
		mu.Lock()
		events = append(events, event{racer, what})
		mu.Unlock()
	}
	// Yield hard instead of sleeping: with the region unguarded this is enough
	// for another racer's install to land inside it on any multi-core runner,
	// and with the region guarded it is bounded and fast.
	yield := func() {
		for i := 0; i < 2000; i++ {
			runtime.Gosched()
		}
	}
	stepsFor := func(racer int) setupSteps {
		observed := false
		return setupSteps{
			inspect: func(context.Context, daemonPlan) daemonState {
				if !observed {
					observed = true
					record(racer, "inspect")
					yield()
				}
				return daemonState{Manager: plan.Manager, Label: plan.Label, UnitPath: plan.UnitPath}
			},
			install: func(context.Context, daemonPlan) error {
				// The unload/load pair: the window the service is down.
				record(racer, "install")
				yield()
				return nil
			},
		}
	}

	const racers = 8
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(racer int) {
			defer wg.Done()
			<-start
			report := setupReport{}
			runSetupDaemon(context.Background(), newRefreshProgress(io.Discard), f.env, stepsFor(racer), plan, &report, "entire-brain")
		}(i)
	}
	close(start)
	wg.Wait()

	counts := make(map[event]int)
	for _, event := range events {
		counts[event]++
	}
	for racer := 0; racer < racers; racer++ {
		for _, kind := range []string{"inspect", "install"} {
			if counts[event{racer, kind}] != 1 {
				t.Fatalf("racer %d %s count=%d, want 1; events=%+v", racer, kind, counts[event{racer, kind}], events)
			}
		}
	}
	// Each racer observes the service and then changes it. Another racer
	// changing it in between is the interleaving that leaves one `setup`
	// reporting "daemon install failed" for a watcher that is running.
	for i, begin := range events {
		if begin.what != "inspect" {
			continue
		}
		for _, later := range events[i+1:] {
			if later.racer == begin.racer {
				break
			}
			if later.what == "install" {
				t.Fatalf("racer %d installed between racer %d's observation and its own install; the daemon phase must be serialized machine-wide\nevents: %+v",
					later.racer, begin.racer, events)
			}
		}
	}
}

// Registration lock callers serialize their critical sections.
func TestDaemonRegistrationLockSerializesCallers(t *testing.T) {
	f := newSetupTestFixture(t)
	var inside atomic.Int32
	var overlaps atomic.Int32
	const racers = 8
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			err := withDaemonRegistrationLock(f.env, func() error {
				if inside.Add(1) != 1 {
					overlaps.Add(1)
				}
				defer inside.Add(-1)
				return nil
			})
			if err != nil {
				t.Errorf("registration lock: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()
	if overlaps.Load() != 0 {
		t.Fatalf("the machine-level daemon lock let %d callers in at once", overlaps.Load())
	}
}

// A state directory that cannot be resolved must not stop a machine installing
// its watcher: the lock is a safety rail, not a prerequisite.
func TestDaemonRegistrationLockDegradesWhenTheStateDirIsUnusable(t *testing.T) {
	t.Parallel()
	// A state dir that resolvePluginDirs rejects outright, so the lock cannot
	// even be located.
	unusable := EntireEnv{PluginStateDir: filepath.Join("not", "absolute")}
	if _, err := setupWatchPlanPath(unusable); err == nil {
		t.Fatal("this fixture must be unresolvable, or the test proves nothing")
	}
	ran := false
	if err := withDaemonRegistrationLock(unusable, func() error { ran = true; return nil }); err != nil {
		t.Fatalf("an unusable state dir must degrade, not fail: %v", err)
	}
	if !ran {
		t.Fatal("the install must still run without the lock")
	}
}

// The installed watcher is a KeepAlive service that prints several lines per
// tick per repo, forever, into a file nothing rotated -- not launchd, not
// systemd, and not this program, which caps every OTHER log it writes.
func TestDaemonLogIsRotatedInPlaceWhenItGrowsPastTheCap(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "watch.log")
	old := strings.Repeat("[watch] no change; nothing to do\n", 200)
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	// The service manager opened this file with O_APPEND before the process
	// started and holds the descriptor for the life of the job.
	held, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()

	if err := rotateDaemonLogIfLarge(path, int64(len(old))-1); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if info, statErr := os.Stat(path); statErr != nil || info.Size() != 0 {
		t.Fatalf("the log must be truncated in place; size=%v err=%v", info, statErr)
	}
	archived, err := os.ReadFile(path + daemonLogArchiveSuffix)
	if err != nil {
		t.Fatalf("read archive: %v", err)
	}
	if string(archived) != old {
		t.Fatal("the archive must carry the rotated-out lines verbatim")
	}

	// Renaming instead of truncating would leave the service writing to the
	// renamed inode forever and the live log permanently empty.
	if _, err := held.WriteString("[watch] after rotation\n"); err != nil {
		t.Fatalf("append through the pre-existing descriptor: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != "[watch] after rotation\n" {
		t.Fatalf("the held O_APPEND descriptor must keep writing to the live log at offset 0, got %q", after)
	}
}

// Rotation below the cap, and on anything that is not a plain file, must be a
// no-op: a watcher run by hand in a terminal must never have its output touched.
func TestDaemonLogRotationLeavesSmallAndNonRegularFilesAlone(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "watch.log")
	if err := os.WriteFile(path, []byte("short\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := rotateDaemonLogIfLarge(path, 1024); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + daemonLogArchiveSuffix); !os.IsNotExist(err) {
		t.Fatal("a log below the cap must not be rotated")
	}
	if err := rotateDaemonLogIfLarge(filepath.Join(dir, "absent.log"), 1); err != nil {
		t.Fatalf("a missing log is not an error: %v", err)
	}
	link := filepath.Join(dir, "linked.log")
	if err := os.Symlink(path, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := rotateDaemonLogIfLarge(link, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(link + daemonLogArchiveSuffix); !os.IsNotExist(err) {
		t.Fatal("a symlinked log path must be left alone")
	}
}

// Only the process whose stdout IS the unit's log may truncate it.
func TestDaemonLogMaintainerIsNilForAWatcherThatIsNotTheService(t *testing.T) {
	f := newSetupTestFixture(t)
	if maintain := daemonLogMaintainer(f.env); maintain != nil {
		t.Fatal("a watcher whose stdout is not the daemon log must not rotate it")
	}
}

// The pass that most needs bounding is the one that finds NOTHING to do: an
// empty or unreadable plan logs a line every interval forever, and before this
// the upkeep would never have run because no workspace pass ever did.
func TestSupervisedWatchMaintainsItsLogEvenWithNothingToWatch(t *testing.T) {
	f := newSetupTestFixture(t)
	base := defaultWatchOptions()
	base.once = true
	maintained := 0
	passes := 0
	if err := supervisedWatchLoop(context.Background(), io.Discard, f.env, base,
		func(string, watchCommandOptions, *int) error { passes++; return nil },
		func() { maintained++ }); err != nil {
		t.Fatalf("supervised loop: %v", err)
	}
	if passes != 0 {
		t.Fatalf("nothing is registered, but %d workspace passes ran", passes)
	}
	if maintained != 1 {
		t.Fatalf("the daemon must do its own upkeep on every outer pass; ran %d times", maintained)
	}
}

// A pid and a timestamp have been stamped on every lock acquisition since the
// package existed and nothing ever read them, so a contended brain said only
// which file it could not lock -- the one thing the caller already knew -- and
// withheld the only thing that identifies the culprit.
func TestLockTimeoutNamesTheProcessHoldingTheLock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "write.lock")
	held, err := acquireFileLock(path, "brain_locked", time.Second)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer func() { _ = held.Close() }()

	// A second acquisition from anywhere -- another goroutine here, another
	// process in production -- must be told who is in the way.
	done := make(chan error, 1)
	go func() {
		_, blockedErr := acquireFileLock(path, "brain_locked", 20*time.Millisecond)
		done <- blockedErr
	}()
	blocked := <-done
	if !errors.Is(blocked, errFileLockTimeout) {
		t.Fatalf("expected a lock timeout, got %v", blocked)
	}
	if !strings.Contains(blocked.Error(), fmt.Sprintf("pid %d", os.Getpid())) {
		t.Fatalf("the timeout must name the holder; got %q", blocked.Error())
	}
	if !strings.Contains(blocked.Error(), path) {
		t.Fatalf("the timeout must still name the lock; got %q", blocked.Error())
	}
}

// The regression #259 caught on Windows shard 3, pinned as a property rather
// than a message: the owner metadata must be readable BY ANOTHER READER WHILE
// THE LOCK IS HELD, which is the only moment it is worth reading.
//
// Windows byte-range locks are mandatory, so while a holder exists a read that
// overlaps the locked byte fails with ERROR_LOCK_VIOLATION. Keeping the
// metadata at offset 0 of the lock file therefore made it unreadable on exactly
// the platform and exactly the instant that mattered, while POSIX's advisory
// flock let every other shard pass. This runs on every platform -- a skip on
// the one platform where the behaviour differs is how the defect hid in the
// first place.
func TestLockOwnerMetadataIsReadableWhileTheLockIsHeld(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "write.lock")
	held, err := acquireFileLock(path, "brain_locked", time.Second)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer func() { _ = held.Close() }()

	if got := describeFileLockHolder(path); !strings.Contains(got, fmt.Sprintf("pid %d", os.Getpid())) {
		t.Fatalf("the holder must be readable while the lock is held; got %q", got)
	}
	// And it must not be INSIDE the locked file -- that is the arrangement that
	// cannot be read on Windows. Stat rather than read: stat inspects metadata
	// and never touches the locked byte range.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat lock: %v", err)
	}
	if info.Size() != 0 {
		t.Fatalf("the lock file must stay empty; owner metadata belongs in the sidecar, found %d bytes", info.Size())
	}
	if _, err := os.Stat(path + fileLockOwnerSuffix); err != nil {
		t.Fatalf("the owner sidecar must exist beside the lock: %v", err)
	}
}

// The message must never be worse than the one it replaces: metadata naming a
// process that is gone describes an innocent bystander, because flock is
// released by the kernel the moment its holder dies.
func TestLockHolderDescriptionIgnoresUnusableMetadata(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cases := map[string]string{
		"dead holder": "pid 2147483646\ncreated_at=2020-01-01T00:00:00Z\n",
		"no pid":      "created_at=2020-01-01T00:00:00Z\n",
		"garbage":     "\x00\x01\x02not a lock file at all",
		"empty":       "",
	}
	for name, contents := range cases {
		path := filepath.Join(dir, strings.ReplaceAll(name, " ", "-")+".lock")
		if err := os.WriteFile(path+fileLockOwnerSuffix, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := describeFileLockHolder(path); got != "" {
			t.Fatalf("%s must describe no holder, got %q", name, got)
		}
	}
	if got := describeFileLockHolder(filepath.Join(dir, "absent.lock")); got != "" {
		t.Fatalf("a missing lock file must describe no holder, got %q", got)
	}
}

// Masking PATH out of the drift decision must not freeze the daemon's inherited
// search path at whatever the very first install happened to see. A re-run
// leaves the SERVICE alone and still brings the FILE up to date, so the current
// PATH is picked up at the next natural restart.
func TestARerunRefreshesTheUnitFileWithoutTouchingTheService(t *testing.T) {
	f := newSetupTestFixture(t)
	plan, err := brainWatchDaemonPlanFor("darwin", f.opts, defaultSetupOptions())
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	stale := plan
	stale.Contents = strings.Replace(plan.Contents, "<key>RunAtLoad</key>", "<key>PATH-was-different</key>\n  <string>x</string>\n  <key>RunAtLoad</key>", 1)
	if stale.Contents == plan.Contents {
		t.Fatal("the fixture must differ from the plan")
	}
	writeUnit(t, stale)

	installs := 0
	steps := setupSteps{
		inspect: func(context.Context, daemonPlan) daemonState {
			// What inspectDaemon reports once PATH is masked: installed, current,
			// running -- the branch that must not reload.
			return daemonState{Manager: plan.Manager, Label: plan.Label, UnitPath: plan.UnitPath, Installed: true, Current: true, Running: true}
		},
		install: func(context.Context, daemonPlan) error { installs++; return nil },
	}
	report := setupReport{}
	runSetupDaemon(context.Background(), newRefreshProgress(io.Discard), f.env, steps, plan, &report, "entire-brain")

	if installs != 0 {
		t.Fatalf("a running, current watcher must never be reloaded; install ran %d times", installs)
	}
	onDisk, err := os.ReadFile(plan.UnitPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(onDisk) != plan.Contents {
		t.Fatal("the unit file must be brought up to date so the next restart picks up the current environment")
	}
}
