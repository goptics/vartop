package cmd

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"unicode/utf16"
)

// The login service is a per-user service of the OS that runs `vartop
// daemon run` at login and again after a crash, but not after a deliberate
// stop: the daemon exits 0 then. Each OS has its supervisor in
// service_<os>.go; the files they register are rendered here, so that every
// OS's is tested everywhere.

// supervisor is the OS's login service for the daemon.
type supervisor interface {
	// Installed reports whether the service is registered.
	Installed() bool
	// Install registers the service to run exe with env, or rewrites it,
	// and (re)starts it: it is the one call that starts the service's
	// daemon, and leaves one daemon running the new definition.
	Install(exe string, env map[string]string) error
	// Uninstall stops the service's daemon and removes the service.
	Uninstall() error
	// Stop stops the service's daemon and cancels any restart of it the
	// service manager has planned (after a kill, say), until the next
	// login or Install.
	Stop() error
}

// runner runs an OS command: systemctl, launchctl or schtasks.
type runner func(name string, args ...string) error

// runCommand is the runner of the real commands: what a command printed
// goes in its error.
func runCommand(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, bytes.TrimSpace(out))
	}
	return nil
}

// noService is the supervisor where there is no login service: never
// installed, and every other call fails with err.
type noService struct{ err error }

func (noService) Installed() bool                           { return false }
func (s noService) Install(string, map[string]string) error { return s.err }
func (s noService) Uninstall() error                        { return s.err }
func (s noService) Stop() error                             { return s.err }

const (
	serviceUnit  = "vartop.service"     // the systemd user unit
	serviceLabel = "dev.goptics.vartop" // the LaunchAgent's label
	serviceTask  = "vartop daemon"      // the Windows scheduled task
)

// serviceDescription says what the service is, where the OS shows it.
const serviceDescription = "vartop daemon: collects the configured varmq servers into vartop's history"

// serviceAllow are the variables the login service keeps from the
// environment of the vartop that installs or rewrites it: what collection
// depends on (the config and data dirs, Kubernetes and cloud credential
// plugins, proxies), nothing secret.
var serviceAllow = []string{
	"PATH", "HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "VARTOP_DATA_DIR", "KUBECONFIG",
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy",
	"AWS_PROFILE", "AWS_REGION", "AWS_DEFAULT_REGION", "AWS_CONFIG_FILE", "AWS_SHARED_CREDENTIALS_FILE",
	"CLOUDSDK_CONFIG", "CLOUDSDK_ACTIVE_CONFIG_NAME", "GOOGLE_APPLICATION_CREDENTIALS", "AZURE_CONFIG_DIR",
}

// secretName reports whether a variable's name says it holds a key, a
// secret or a token: such a variable is never saved in a service file.
func secretName(name string) bool {
	n := strings.ToUpper(name)
	return strings.Contains(n, "KEY") || strings.Contains(n, "SECRET") || strings.Contains(n, "TOKEN")
}

// serviceAlways are the variables the login service always sets, empty
// when vartop has them unset, so that the service manager's own values
// never stand in for them: the config path and KUBECONFIG are compared
// with the UI's, and the UIs read the data dir the daemon writes.
var serviceAlways = []string{"KUBECONFIG", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "VARTOP_DATA_DIR"}

// serviceEnv picks from environ (as os.Environ gives it) what the login
// service keeps: the variables of serviceAllow set to a value, never one
// that secretName flags, and those of serviceAlways in any case. Windows
// names match in any case.
func serviceEnv(environ []string) map[string]string {
	env := make(map[string]string)
	for _, name := range serviceAlways {
		env[name] = ""
	}
	for _, kv := range environ {
		name, value, ok := strings.Cut(kv, "=")
		if !ok || value == "" || secretName(name) {
			continue
		}
		for _, allowed := range serviceAllow {
			if name == allowed || runtime.GOOS == "windows" && strings.EqualFold(name, allowed) {
				env[allowed] = value
			}
		}
	}
	return env
}

// systemdUnit is the systemd user unit that runs exe as the daemon with
// env.
func systemdUnit(exe string, env map[string]string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[Unit]\nDescription=%s\n\n[Service]\n", serviceDescription)
	fmt.Fprintf(&b, "ExecStart=%s daemon run\n", systemdQuote(exe, true))
	for _, name := range slices.Sorted(maps.Keys(env)) {
		fmt.Fprintf(&b, "Environment=%s\n", systemdQuote(name+"="+env[name], false))
	}
	b.WriteString("Restart=on-failure\nRestartSec=5\n\n[Install]\nWantedBy=default.target\n")
	return b.String()
}

// systemdQuote quotes s as one word of a unit file: backslashes, quotes
// and newlines escaped C-style, % doubled (specifiers) and, in a command
// line, $ too (variables).
func systemdQuote(s string, command bool) string {
	r := []string{`\`, `\\`, `"`, `\"`, "\n", `\n`, "%", "%%"}
	if command {
		r = append(r, "$", "$$")
	}
	return `"` + strings.NewReplacer(r...).Replace(s) + `"`
}

// launchdPlist is the LaunchAgent that runs exe as the daemon with env.
func launchdPlist(exe string, env map[string]string) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + serviceLabel + `</string>
	<key>ProgramArguments</key>
	<array>
`)
	for _, arg := range []string{exe, "daemon", "run"} {
		fmt.Fprintf(&b, "\t\t<string>%s</string>\n", xmlText(arg))
	}
	b.WriteString("\t</array>\n\t<key>EnvironmentVariables</key>\n\t<dict>\n")
	for _, name := range slices.Sorted(maps.Keys(env)) {
		fmt.Fprintf(&b, "\t\t<key>%s</key>\n\t\t<string>%s</string>\n", xmlText(name), xmlText(env[name]))
	}
	b.WriteString(`	</dict>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<dict>
		<key>SuccessfulExit</key>
		<false/>
	</dict>
	<key>ThrottleInterval</key>
	<integer>10</integer>
</dict>
</plist>
`)
	return b.String()
}

// windowsTaskXML is the scheduled task that runs exe as the daemon with env,
// at the logon of the user installing it. A task has no environment of its
// own: `daemon run --env NAME=VALUE` sets each variable first.
func windowsTaskXML(exe string, env map[string]string) string {
	args := []string{"daemon", "run"}
	for _, name := range slices.Sorted(maps.Keys(env)) {
		args = append(args, "--env", windowsQuote(name+"="+env[name]))
	}
	var user string
	if u := windowsUser(); u != "" {
		user = "\n      <UserId>" + xmlText(u) + "</UserId>"
	}
	return `<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo>
    <Description>` + xmlText(serviceDescription) + `</Description>
  </RegistrationInfo>
  <Triggers>
    <LogonTrigger>
      <Enabled>true</Enabled>` + user + `
    </LogonTrigger>
  </Triggers>
  <Principals>
    <Principal id="Author">` + user + `
      <LogonType>InteractiveToken</LogonType>
      <RunLevel>LeastPrivilege</RunLevel>
    </Principal>
  </Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
    <RestartOnFailure>
      <Interval>PT1M</Interval>
      <Count>999</Count>
    </RestartOnFailure>
  </Settings>
  <Actions Context="Author">
    <Exec>
      <Command>` + xmlText(exe) + `</Command>
      <Arguments>` + xmlText(strings.Join(args, " ")) + `</Arguments>
    </Exec>
  </Actions>
</Task>
`
}

// windowsUser is the Windows user running vartop, as DOMAIN\name; "" when
// unknown.
func windowsUser() string {
	domain, name := os.Getenv("USERDOMAIN"), os.Getenv("USERNAME")
	switch {
	case name == "":
		return ""
	case domain == "":
		return name
	}
	return domain + `\` + name
}

// windowsQuote quotes s as one argument of a Windows command line: in
// double quotes, with the backslashes before a quote (the closing one too)
// doubled and the quote escaped.
func windowsQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	slashes := 0
	for _, c := range s {
		switch c {
		case '\\':
			slashes++
		case '"':
			b.WriteString(strings.Repeat(`\`, slashes+1))
			slashes = 0
		default:
			slashes = 0
		}
		b.WriteRune(c)
	}
	b.WriteString(strings.Repeat(`\`, slashes))
	b.WriteByte('"')
	return b.String()
}

// writeServiceFile writes a service file at path, 0600 (its variables may
// hold a proxy's credentials) even over an older one of another mode.
func writeServiceFile(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600) // WriteFile keeps the mode of a file there
}

// installServiceFile writes a login service's file at path (see
// writeServiceFile), then has the service manager take it (register).
// Should either fail, a file that was not there before is undone —
// through uninstall once the manager may have taken some of it — so that
// Installed, which looks for the file, does not report a service the
// manager never took (R32): vartop then spawns its daemon itself. A file
// that was there before is kept: the manager knows it from an install
// that worked.
func installServiceFile(path, content string, register, uninstall func() error) error {
	_, err := os.Stat(path)
	created := errors.Is(err, fs.ErrNotExist)
	if err := writeServiceFile(path, content); err != nil {
		if created {
			_ = os.Remove(path)
		}
		return err
	}
	if err := register(); err != nil {
		if created {
			_ = uninstall() // the manager may be unreachable: its part of the undo fails too
		}
		return err
	}
	return nil
}

// xmlText escapes s for XML text or an attribute value.
func xmlText(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s)) // a strings.Builder never fails
	return b.String()
}

// utf16File is s as a UTF-16 (little-endian) file with a byte order mark:
// what schtasks reads a task's XML from.
func utf16File(s string) []byte {
	b := []byte{0xff, 0xfe}
	for _, u := range utf16.Encode([]rune(s)) {
		b = append(b, byte(u), byte(u>>8))
	}
	return b
}
