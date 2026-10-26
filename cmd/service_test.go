package cmd

import (
	"flag"
	"maps"
	"os"
	"path/filepath"
	"slices"
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
	}
	if !maps.Equal(got, want) {
		t.Errorf("serviceEnv() = %v, want %v", got, want)
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

// goldenEnv is an environment with every kind of character the service
// files must escape.
var goldenEnv = map[string]string{
	"PATH":       `/opt/my tools/bin:/usr/bin`,
	"HOME":       `/home/u`,
	"KUBECONFIG": `/home/u/.kube/a "b"%c$d<e>&f`,
}

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
	winEnv := map[string]string{
		"PATH":       `C:\Program Files\Git\bin;C:\Windows\`,
		"KUBECONFIG": `C:\Users\u\.kube\a "b".yaml`,
	}
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
