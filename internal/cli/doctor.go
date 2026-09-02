package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/tasticolly/splitr/internal/config"
	"github.com/tasticolly/splitr/internal/protect"
)

// checkState — исход одной проверки.
type checkState int

const (
	checkOK checkState = iota
	checkWarn
	checkFail
)

type check struct {
	name   string
	state  checkState
	detail string
	hint   string
}

// Doctor проверяет установку целиком и объясняет, что чинить.
// Возвращает ошибку, если хоть одна проверка провалилась.
func Doctor(configPath string) error {
	checks := []check{
		checkBinary(),
		checkConfigFile(configPath),
		checkPfConf(),
		checkAnchorFile(),
		checkService(),
	}

	cfg, cfgErr := config.Load(configPath)
	if cfgErr == nil {
		checks = append(checks, checkSshuttleBinary(cfg), checkProfileKeys(cfg))
		checks = append(checks, checkDaemon(cfg)...)
	}

	var failed int
	for _, c := range checks {
		mark := map[checkState]string{checkOK: "✓", checkWarn: "!", checkFail: "✗"}[c.state]
		fmt.Printf("%s %-32s %s\n", mark, c.name, c.detail)
		if c.hint != "" && c.state != checkOK {
			fmt.Printf("  → %s\n", c.hint)
		}
		if c.state == checkFail {
			failed++
		}
	}
	if failed > 0 {
		return fmt.Errorf("checks failed: %d", failed)
	}
	fmt.Println("\neverything is in place")
	return nil
}

func checkBinary() check {
	info, err := os.Stat(installedBinary)
	if err != nil {
		return check{"binary", checkFail, installedBinary + " not found", "sudo splitr install"}
	}
	if info.Mode().Perm()&0o111 == 0 {
		return check{"binary", checkFail, installedBinary + " is not executable", "sudo splitr install"}
	}
	return check{"binary", checkOK, installedBinary, ""}
}

func checkConfigFile(path string) check {
	if _, err := config.Load(path); err != nil {
		return check{"config", checkFail, err.Error(), "splitr validate " + path}
	}
	return check{"config", checkOK, path, ""}
}

// checkPfConf проверяет главное: без вызова якоря из /etc/pf.conf
// правила блокировки лежат мёртвым грузом и не вычисляются вообще.
func checkPfConf() check {
	raw, err := os.ReadFile(protect.PfConf)
	if err != nil {
		return check{"pf.conf", checkFail, err.Error(), "sudo splitr install"}
	}
	if !strings.Contains(string(raw), `anchor "`+protect.Anchor+`"`) {
		return check{"pf.conf", checkFail, "no anchor call for " + protect.Anchor, "sudo splitr install"}
	}
	if !strings.Contains(string(raw), protect.AnchorFile) {
		return check{"pf.conf", checkWarn, "no `load anchor` — rules will not survive a reboot", "sudo splitr install"}
	}
	return check{"pf.conf", checkOK, "anchor linked and loaded at boot", ""}
}

func checkAnchorFile() check {
	raw, err := os.ReadFile(protect.AnchorFile)
	if err != nil {
		return check{"anchor file", checkFail, err.Error(), "sudo splitr install"}
	}
	if !strings.Contains(string(raw), "block drop out") {
		return check{"anchor file", checkWarn, "no block rules", "splitr protect on"}
	}
	return check{"anchor file", checkOK, protect.AnchorFile, ""}
}

func checkService() check {
	if _, err := os.Stat(launchDaemonPlist); err != nil {
		return check{"launchd service", checkFail, launchDaemonPlist + " is missing", "sudo splitr install"}
	}
	if !serviceLoaded() {
		return check{"launchd service", checkFail, "not loaded", "sudo launchctl bootstrap system " + launchDaemonPlist}
	}
	return check{"launchd service", checkOK, LaunchDaemonLabel, ""}
}

func checkSshuttleBinary(cfg config.Config) check {
	path := cfg.Sshuttle.Path
	if path == "" {
		path = "sshuttle"
	}
	if !strings.Contains(path, "/") {
		resolved, err := exec.LookPath(path)
		if err != nil {
			return check{"sshuttle", checkFail, path + " not found in PATH", "brew install sshuttle"}
		}
		path = resolved
	}
	if _, err := os.Stat(path); err != nil {
		return check{"sshuttle", checkFail, err.Error(), "brew install sshuttle"}
	}
	return check{"sshuttle", checkOK, path, ""}
}

func checkProfileKeys(cfg config.Config) check {
	var missing []string
	for name, p := range cfg.Profiles {
		if p.SSHKey == "" {
			continue
		}
		if _, err := os.Stat(p.SSHKey); err != nil {
			missing = append(missing, fmt.Sprintf("%s: %s", name, p.SSHKey))
		}
	}
	if len(missing) > 0 {
		return check{"profile ssh keys", checkWarn, strings.Join(missing, ", "), "check the ssh_key paths in the config"}
	}
	return check{"profile ssh keys", checkOK, fmt.Sprintf("profiles: %d", len(cfg.Profiles)), ""}
}

func checkDaemon(cfg config.Config) []check {
	socket := cfg.Daemon.SocketPath
	if _, err := os.Stat(socket); err != nil {
		return []check{{"daemon", checkFail, "no socket at " + socket,
			"sudo launchctl kickstart -k system/" + LaunchDaemonLabel + "; log: /usr/local/var/log/splitr.err.log"}}
	}

	data, err := NewClient(socket).Get("/status")
	if err != nil {
		return []check{{"daemon", checkFail, err.Error(), "sudo launchctl kickstart -k system/" + LaunchDaemonLabel}}
	}

	var st struct {
		PFEnabled    bool     `json:"pf_enabled"`
		AnchorLoaded bool     `json:"anchor_loaded"`
		AnchorLinked bool     `json:"anchor_linked"`
		Protection   string   `json:"protection"`
		Blocking     bool     `json:"blocking"`
		Tunnel       string   `json:"tunnel"`
		BlockedNets  []string `json:"blocked_nets"`
		Warnings     []string `json:"warnings"`
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return []check{{"daemon", checkFail, "unreadable response: " + err.Error(), ""}}
	}

	out := []check{{"daemon", checkOK, "responding on " + socket, ""}}

	if st.PFEnabled {
		out = append(out, check{"pf enabled", checkOK, "yes", ""})
	} else {
		out = append(out, check{"pf enabled", checkFail, "no", "the daemon should enable pf itself; check the log"})
	}

	switch {
	case st.Protection == "off":
		out = append(out, check{"protection", checkWarn, "off — protected routes are reachable", "splitr protect on"})
	case !st.AnchorLoaded:
		out = append(out, check{"protection", checkFail, "rules are not loaded into the anchor", "splitr reload"})
	case !st.AnchorLinked:
		out = append(out, check{"protection", checkFail, "anchor is not linked into the main ruleset",
			"sudo pfctl -f /etc/pf.conf (only while the tunnel is down)"})
	default:
		out = append(out, check{"protection", checkOK,
			fmt.Sprintf("%s, routes: %d", st.Protection, len(st.BlockedNets)), ""})
	}

	state := "protected routes are dropped"
	if !st.Blocking {
		state = "protected routes are reachable (tunnel is up or protection is off)"
	}
	out = append(out, check{"current state", checkOK, fmt.Sprintf("tunnel %s; %s", st.Tunnel, state), ""})

	for _, w := range st.Warnings {
		out = append(out, check{"daemon warning", checkWarn, w, ""})
	}
	return out
}
