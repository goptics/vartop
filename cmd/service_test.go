package cmd

import (
	"encoding/xml"
	"flag"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the golden files under testdata")

func TestServiceEnvAllowList(t *testing.T) {
	environ := []string{
		"PATH=/usr/local/bin:/usr/bin",
		"HOME=/home/u",
		"KUBECONFIG=/home/u/.kube/config",
		"AWS_PROFILE=prod",
		"HTTPS_PROXY=http://proxy:3128",
		"no_proxy=localhost",
		"AWS_ACCESS_KEY_ID=AKIA0000",
		"AWS_SECRET_ACCESS_KEY=s3cr3t",
		"AWS_SESSION_TOKEN=t0k3n",
		"GITHUB_TOKEN=ghp_0000",
		"EDITOR=vim",
		"XDG_CONFIG_HOME=", // set but empty: as good as unset
		"MALFORMED",
	}
	got := serviceEnv(environ)
	want := map[string]string{
		"PATH":        "/usr/local/bin:/usr/bin",
		"HOME":        "/home/u",
		"KUBECONFIG":  "/home/u/.kube/config",
		"AWS_PROFILE": "prod",
		"HTTPS_PROXY": "http://proxy:3128",
		"no_proxy":    "localhost",
		// Always written, empty when unset: a value of the service
		// manager's own must not take their place.
		"XDG_CONFIG_HOME": "",
		"XDG_CACHE_HOME":  "",
		"VARTOP_DATA_DIR": "",
	}
	if !maps.Equal(got, want) {
		t.Errorf("serviceEnv() = %v, want %v", got, want)
	}
	unset := map[string]string{"KUBECONFIG": "", "XDG_CONFIG_HOME": "", "XDG_CACHE_HOME": "", "VARTOP_DATA_DIR": ""}
	if got := serviceEnv(nil); !maps.Equal(got, unset) {
		t.Errorf("serviceEnv(nothing set) = %v, want %v", got, unset)
	}
	for _, name := range serviceAllow {
		if secretName(name) {
			t.Errorf("allow-list entry %s names a secret", name)
		}
	}
	for _, name := range []string{"AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "GITHUB_TOKEN", "AWS_ACCESS_KEY_ID", "api_key"} {
		if !secretName(name) {
			t.Errorf("secretName(%q) = false, want true", name)
		}
	}
}

// goldenEnv is a service environment with every kind of character the
// service files must escape, and the names written empty when unset.
var goldenEnv = serviceEnv([]string{
	`PATH=/opt/my tools/bin:/usr/bin`,
	`HOME=/home/u`,
	`KUBECONFIG=/home/u/.kube/a "b"%c$d<e>&f`,
})

// golden compares got with testdata/name, or rewrites it with -update.
func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run the test with -update to create it)", err)
	}
	if got != string(want) {
		t.Errorf("%s differs from the golden file:\n--- got\n%s\n--- want\n%s", name, got, want)
	}
}

func TestServiceFilesGolden(t *testing.T) {
	t.Setenv("USERDOMAIN", "DESKTOP")
	t.Setenv("USERNAME", "u")
	golden(t, "vartop.service", systemdUnit("/opt/vartop dir/vartop", goldenEnv))
	golden(t, "dev.goptics.vartop.plist", launchdPlist("/opt/vartop dir/vartop", goldenEnv))
	winEnv := serviceEnv([]string{
		`PATH=C:\Program Files\Git\bin;C:\Windows\`,
		`KUBECONFIG=C:\Users\u\.kube\a "b".yaml`,
	})
	golden(t, "vartop-task.xml", windowsTaskXML(`C:\Program Files\vartop\vartop.exe`, winEnv))
}

// TestWindowsArgsRoundTrip: the arguments the task passes parse back,
// by the Windows rules, into the exact NAME=VALUE pairs.
func TestWindowsArgsRoundTrip(t *testing.T) {
	for _, s := range []string{`PATH=C:\a b\;C:\c\`, `X="q" \"`, `Y=`, `Z=\\server\share\`} {
		if got := splitWindowsArgs(windowsQuote(s)); !slices.Equal(got, []string{s}) {
			t.Errorf("windowsQuote(%q) parses back as %q", s, got)
		}
	}
}

// TestWindowsEnvRoundTrip: the task's --env pairs, applied by `daemon
// run`, give the daemon the UI's environment: a name the UI has unset is
// unset in the daemon, whatever the logon environment holds. What it sets
// in this process is put back after it.
func TestWindowsEnvRoundTrip(t *testing.T) {
	names := []string{"PATH", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "VARTOP_DATA_DIR", "KUBECONFIG", "AWS_PROFILE"}
	before := make(map[string]string)
	for _, name := range names {
		if v, set := os.LookupEnv(name); set {
			before[name] = v
		}
	}
	t.Run("roundtrip", windowsEnvRoundTrip)
	for _, name := range names {
		v, set := os.LookupEnv(name)
		if want, wasSet := before[name]; set != wasSet || v != want {
			t.Errorf("%s = %q (set %v) after the test, want %q (set %v): applyEnv's change leaked", name, v, set, want, wasSet)
		}
	}
}

func windowsEnvRoundTrip(t *testing.T) {
	// applyEnv sets PATH and unsets the serviceAlways names the UI has
	// empty: t.Setenv puts each back once the test ends.
	for _, name := range []string{"PATH", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "VARTOP_DATA_DIR"} {
		t.Setenv(name, os.Getenv(name))
	}
	t.Setenv("KUBECONFIG", "/from/the/logon/environment")
	t.Setenv("AWS_PROFILE", "logon")
	env := serviceEnv([]string{`PATH=C:\bin`, `AWS_PROFILE=ui profile`})
	task := windowsTaskXML(`C:\vartop.exe`, env)
	start, end := strings.Index(task, "<Arguments>"), strings.Index(task, "</Arguments>")
	var arguments string
	if err := xml.Unmarshal([]byte("<a>"+task[start+len("<Arguments>"):end]+"</a>"), &arguments); err != nil {
		t.Fatal(err)
	}
	args := splitWindowsArgs(arguments)
	var pairs []string
	for i, a := range args {
		if a == "--env" && i+1 < len(args) {
			pairs = append(pairs, args[i+1])
		}
	}
	if err := applyEnv(pairs); err != nil {
		t.Fatal(err)
	}
	if v, set := os.LookupEnv("KUBECONFIG"); set {
		t.Errorf("KUBECONFIG = %q in the daemon, want it unset", v)
	}
	if got := os.Getenv("AWS_PROFILE"); got != "ui profile" {
		t.Errorf("AWS_PROFILE = %q in the daemon, want the UI's", got)
	}
}

// splitWindowsArgs parses a command line as Windows programs (and Go's
// runtime) do.
func splitWindowsArgs(cmd string) []string {
	var args []string
	var arg []rune
	inQuote, have := false, false
	rs := []rune(cmd)
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		switch {
		case c == '\\':
			n := 0
			for i < len(rs) && rs[i] == '\\' {
				n++
				i++
			}
			if i < len(rs) && rs[i] == '"' {
				arg = append(arg, []rune(repeat('\\', n/2))...)
				if n%2 == 1 {
					arg = append(arg, '"')
				} else {
					inQuote = !inQuote
				}
			} else {
				arg = append(arg, []rune(repeat('\\', n))...)
				i--
			}
			have = true
		case c == '"':
			inQuote, have = !inQuote, true
		case (c == ' ' || c == '\t') && !inQuote:
			if have {
				args = append(args, string(arg))
				arg, have = nil, false
			}
		default:
			arg, have = append(arg, c), true
		}
	}
	if have {
		args = append(args, string(arg))
	}
	return args
}

func repeat(c rune, n int) string {
	s := make([]rune, n)
	for i := range s {
		s[i] = c
	}
	return string(s)
}
