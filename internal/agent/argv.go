package agent

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

var placeholderRe = regexp.MustCompile(`\{(in|out):([A-Za-z0-9._-]+)\}|\{workdir\}`)

// SubstituteArgs replaces {in:NAME}, {out:NAME} and {workdir}. Unknown input
// or output names are an error so typos fail loudly instead of running with a
// literal placeholder.
func SubstituteArgs(args []string, workdir string, inputs, outputs map[string]bool) ([]string, error) {
	out := make([]string, len(args))
	for i, a := range args {
		var err error
		out[i] = placeholderRe.ReplaceAllStringFunc(a, func(m string) string {
			if m == "{workdir}" {
				return workdir
			}
			sub := placeholderRe.FindStringSubmatch(m)
			kind, name := sub[1], sub[2]
			if kind == "in" {
				if !inputs[name] {
					err = fmt.Errorf("arg %d references unknown input %q", i, name)
				}
				return filepath.Join(workdir, "in", name)
			}
			if !outputs[name] {
				err = fmt.Errorf("arg %d references unknown output %q", i, name)
			}
			return filepath.Join(workdir, "out", name)
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// BuildArgv composes the final command line: priority wrapper, optional
// sandbox, then the allowlisted runtime prefix and the task's arguments.
func BuildArgv(goos, priority string, sandbox bool, workdir string, runtime []string, args []string) []string {
	var argv []string
	switch {
	case goos == "darwin" && priority == "background":
		argv = append(argv, "/usr/sbin/taskpolicy", "-b")
	case goos == "darwin" && (priority == "utility" || priority == "adaptive-utility"):
		argv = append(argv, "/usr/sbin/taskpolicy", "-c", "utility", "/usr/bin/nice", "-n", "10")
	case goos == "darwin" && priority == "adaptive-background":
		// Utility clamp plus darwin background: background wins now, and
		// lifting it later (setGroupBackground) leaves the task at utility.
		argv = append(argv, "/usr/sbin/taskpolicy", "-c", "utility", "-b", "/usr/bin/nice", "-n", "10")
	case goos == "linux" && priority != "none":
		argv = append(argv, "/usr/bin/nice", "-n", "19")
	}
	if goos == "darwin" && sandbox {
		argv = append(argv, "/usr/bin/sandbox-exec", "-p", SandboxProfile(workdir))
	}
	argv = append(argv, runtime...)
	return append(argv, args...)
}

// SandboxProfile allows everything except writes outside the work dir and a
// few device files. Network stays allowed (tasks may stream from URLs).
func SandboxProfile(workdir string) string {
	q := func(s string) string {
		return `"` + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `"`, `\"`) + `"`
	}
	return strings.Join([]string{
		"(version 1)",
		"(allow default)",
		"(deny file-write*)",
		"(allow file-write* (subpath " + q(workdir) + ")",
		"  (literal \"/dev/null\") (literal \"/dev/zero\") (literal \"/dev/dtracehelper\")",
		"  (regex #\"^/dev/fd/\") (regex #\"^/private/var/folders/\"))",
	}, "\n")
}

var envKeyRe = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,127}$`)

// TaskEnv builds a minimal environment. Tasks cannot override PATH, HOME,
// TMPDIR, UMM_* or dynamic-loader variables.
func TaskEnv(workdir, taskID string, attempt int, agentEnv, taskEnv map[string]string) ([]string, error) {
	env := map[string]string{
		"PATH":        "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin",
		"HOME":        workdir,
		"TMPDIR":      filepath.Join(workdir, "tmp"),
		"LANG":        "en_US.UTF-8",
		"UMM_WORKDIR": workdir,
		"UMM_IN":      filepath.Join(workdir, "in"),
		"UMM_OUT":     filepath.Join(workdir, "out"),
		"UMM_TASK_ID": taskID,
		"UMM_ATTEMPT": fmt.Sprint(attempt),
	}
	for k, v := range agentEnv {
		env[k] = v
	}
	for k, v := range taskEnv {
		if !envKeyRe.MatchString(k) {
			return nil, fmt.Errorf("invalid env name %q", k)
		}
		switch {
		case k == "PATH", k == "HOME", k == "TMPDIR", strings.HasPrefix(k, "UMM_"),
			strings.HasPrefix(k, "DYLD_"), strings.HasPrefix(k, "LD_"):
			return nil, fmt.Errorf("task may not set %s", k)
		}
		env[k] = v
	}
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out, nil
}
