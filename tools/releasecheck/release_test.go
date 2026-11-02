package releasecheck

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

func TestWorkflowRunBlocksDoNotInterpolateUntrustedInputs(t *testing.T) {
	paths, err := filepath.Glob("../../.github/workflows/*.yml")
	if err != nil {
		t.Fatal(err)
	}
	untrusted := regexp.MustCompile(`\$\{\{\s*(inputs\.|github\.event\.|github\.head_ref|github\.ref_name)`)
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		runIndent := -1
		for i, line := range strings.Split(string(data), "\n") {
			trimmed := strings.TrimLeft(line, " ")
			indent := len(line) - len(trimmed)
			if trimmed != "" && indent <= runIndent {
				runIndent = -1
			}
			value := strings.TrimPrefix(trimmed, "- ")
			if strings.HasPrefix(value, "run:") {
				runIndent = indent
			}
			if runIndent >= 0 && untrusted.MatchString(line) {
				t.Errorf("%s:%d: pass untrusted values through env", path, i+1)
			}
		}
	}
}

// Exercise the installer with local shell fixtures. No network or plugin state is used.
func TestInstallerRequiresMatchingChecksums(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("installer is a POSIX shell script")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"missing", "wrong-asset", "mismatch"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, "bin")
			if err := os.Mkdir(bin, 0700); err != nil {
				t.Fatal(err)
			}
			// curl writes only local fixture bytes; every archive is a valid tarball.
			payload := filepath.Join(dir, "payload")
			if err := os.Mkdir(payload, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(payload, "entire-brain"), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
				t.Fatal(err)
			}
			archive := filepath.Join(dir, "archive.tar.gz")
			if out, err := exec.Command("tar", "-czf", archive, "-C", payload, "entire-brain").CombinedOutput(); err != nil {
				t.Fatalf("tar: %v: %s", err, out)
			}
			curl := `#!/bin/sh
out=""
url=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    -o|--output) out="$2"; shift 2 ;;
    https://*) url="$1"; shift ;;
    *) shift ;;
  esac
done
case "$url" in
  */checksums.txt)
    case "$FIXTURE_MODE" in
      missing) exit 22 ;;
      wrong-asset) printf '%064d  other.tar.gz\n' 0 > "$out" ;;
      mismatch) printf '%064d  %s\n' 0 "$FIXTURE_ASSET" > "$out" ;;
    esac ;;
  *.tar.gz) cp "$FIXTURE_ARCHIVE" "$out" ;;
  *) exit 23 ;;
esac
`
			if err := os.WriteFile(filepath.Join(bin, "curl"), []byte(curl), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(bin, "entire"), []byte("#!/bin/sh\nexit 99\n"), 0700); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("bash", filepath.Join(root, "scripts/get-brain.sh"), "--version", "v9.9.9")
			cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "FIXTURE_MODE="+mode, "FIXTURE_ARCHIVE="+archive, "FIXTURE_ASSET=entire-brain_9.9.9_"+runtime.GOOS+"_"+runtime.GOARCH+".tar.gz", "ENTIRE_BRAIN_INSTALL_DIR="+filepath.Join(dir, "install"))
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("unverified install succeeded: %s", out)
			}
			asset := "entire-brain_9.9.9_" + runtime.GOOS + "_" + runtime.GOARCH + ".tar.gz"
			want := map[string]string{
				"missing":     "could not fetch checksums.txt for v9.9.9",
				"wrong-asset": "checksums.txt has no entry for " + asset,
				"mismatch":    "checksum mismatch for " + asset,
			}[mode]
			if !strings.Contains(string(out), want) {
				t.Fatalf("missing specific diagnostic %q: %s", want, out)
			}
			if _, err := os.Stat(filepath.Join(dir, "install", "entire-brain")); !os.IsNotExist(err) {
				t.Fatalf("installed unverified binary: %v", err)
			}
		})
	}
}

func TestNoPythonTooling(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", ".context", "node_modules", ".worktrees":
				return filepath.SkipDir
			}
			if entry.Name() == ".venv" || entry.Name() == "venv" || entry.Name() == "__pycache__" {
				t.Errorf("Python environment: %s", path)
				return filepath.SkipDir
			}
			return nil
		}
		ext, name := strings.ToLower(filepath.Ext(path)), strings.ToLower(entry.Name())
		manifest := name == "pyproject.toml" || name == "pipfile" || name == "pipfile.lock" || name == "poetry.lock" || name == "uv.lock" || (strings.HasPrefix(name, "requirements") && ext == ".txt")
		if ext == ".py" || ext == ".pyc" || ext == ".pyo" || name == "pyvenv.cfg" || manifest {
			t.Errorf("Python file: %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestInstallerHelpWorksFromStandardInput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("installer is a POSIX shell script")
	}
	data, err := os.ReadFile("../../scripts/get-brain.sh")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "-s", "--", "--help")
	cmd.Stdin = strings.NewReader(string(data))
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "--nightly") {
		t.Fatalf("piped installer help: %v: %s", err, out)
	}
}

func TestReleaseJobsValidateVersionShape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("release version guards run in bash")
	}
	data, err := os.ReadFile("../../.github/workflows/release.yml")
	if err != nil {
		t.Fatal(err)
	}
	source := string(data)
	jobs := regexp.MustCompile(`(?m)^  ([\w-]+):$`).FindAllStringSubmatchIndex(source, -1)
	guard := regexp.MustCompile(`\[\[ ! "\$version" =~ (\S+) \]\]`)
	seen := 0
	for i, match := range jobs {
		name := source[match[2]:match[3]]
		if name != "build" && name != "publish" {
			continue
		}
		seen++
		end := len(source)
		if i+1 < len(jobs) {
			end = jobs[i+1][0]
		}
		patterns := guard.FindAllStringSubmatch(source[match[1]:end], -1)
		if len(patterns) != 1 {
			t.Fatalf("%s: expected one version guard, got %d", name, len(patterns))
		}
		for want, values := range map[string][]string{
			"ACCEPT": {"v0.1.0", "v1.2.3", "v10.20.30", "v1.2.3-nightly.202609192059.abc1234", "v0.0.0-dev.f9838c0", "v0.1.0+build.5"},
			"REJECT": {"0.1.0", "v1.2", "v1", "", "v1.2.3.4", "v1.2.3-", "v1.2.3; rm -rf /", "v1.2.3$(id)", "v1.2.3 && echo pwned", "v1.2.3`id`", "../../etc/passwd", "v1.2.3\nv9.9.9"},
		} {
			for _, value := range values {
				cmd := exec.Command("bash", "-c", `if [[ "$1" =~ $VERSION_PATTERN ]]; then echo ACCEPT; else echo REJECT; fi`, "bash", value)
				cmd.Env = append(os.Environ(), "VERSION_PATTERN="+patterns[0][1])
				out, err := cmd.CombinedOutput()
				if err != nil || strings.TrimSpace(string(out)) != want {
					t.Fatalf("%s version %q: got %s, want %s: %v", name, value, out, want, err)
				}
			}
		}
	}
	if seen != 2 {
		t.Fatalf("expected build and publish jobs, got %d", seen)
	}
}
