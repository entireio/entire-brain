package base

import (
	"os"
	"strings"
)

// EnvNoDefaultCurrentDirectoryInExePath is the Windows environment variable
// that, when set (to any value), stops cmd.exe and the Win32 process-creation
// search from looking for a bare program name in the current working directory
// before PATH. With it set, a bare name resolves from PATH only.
const EnvNoDefaultCurrentDirectoryInExePath = "NoDefaultCurrentDirectoryInExePath"

// PathOnlyExeLookupEnv returns env for a child process that runs inside a
// repository. On windows it carries NoDefaultCurrentDirectoryInExePath=1, so a
// bare command name the child (or anything it starts) runs resolves from PATH
// and never from the repository working directory. Elsewhere program lookup
// never consults the working directory, and env is returned unchanged.
//
// A nil env means "inherit the parent environment", as it does for
// exec.Cmd.Env: on windows the result starts from os.Environ(), and elsewhere
// nil is returned so the child still inherits exactly as before. Any inherited
// spelling of the variable is replaced rather than left to compete with ours;
// windows environment names are case-insensitive, so the match is too. The
// caller's slice is never modified.
//
// goos is a parameter, like preEditHookCommandFor's, so both branches are
// testable on any host.
func PathOnlyExeLookupEnv(goos string, env []string) []string {
	if goos != "windows" {
		return env
	}
	if env == nil {
		env = os.Environ()
	}
	out := make([]string, 0, len(env)+1)
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		if strings.EqualFold(key, EnvNoDefaultCurrentDirectoryInExePath) {
			continue
		}
		out = append(out, entry)
	}
	return append(out, EnvNoDefaultCurrentDirectoryInExePath+"=1")
}
