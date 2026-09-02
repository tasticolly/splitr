package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/tasticolly/splitr/internal/config"
	"github.com/tasticolly/splitr/internal/daemon"
	"github.com/tasticolly/splitr/internal/pfctl"
	"github.com/tasticolly/splitr/internal/protect"
	"github.com/tasticolly/splitr/internal/sysinstall"
)

const (
	// LaunchDaemonLabel — идентификатор службы в launchd.
	LaunchDaemonLabel = sysinstall.LaunchDaemonLabel
	launchDaemonPlist = sysinstall.PlistPath
	installedBinary   = sysinstall.BinaryPath
)

// printf печатает ход установки на экран — этим sysinstall и отличает
// установку руками от обновления через демона, которое пишет в журнал.
func printf(format string, args ...any) { fmt.Printf(format+"\n", args...) }

// Install ставит бинарь, конфиг, якорь pf и службу launchd. Требует root.
func Install(configPath string, embeddedConfig []byte) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("root required: sudo splitr install")
	}
	if configPath == "" {
		configPath = config.DefaultPath
	}

	for _, dir := range []string{
		filepath.Dir(configPath),
		filepath.Dir(protect.AnchorFile),
		"/usr/local/bin", "/usr/local/var/log", "/usr/local/var/run",
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}

	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		if err := os.WriteFile(configPath, embeddedConfig, 0o644); err != nil {
			return fmt.Errorf("write config: %w", err)
		}
		fmt.Printf("config %s created\n", configPath)
	} else {
		fmt.Printf("config %s already exists, left as is\n", configPath)
	}

	// Путь к исходникам записывается до установки бинаря: иначе кнопка
	// обновления не появится, и человек останется с командой в терминале.
	if err := ensureUpdateSource(configPath); err != nil {
		fmt.Println("could not record the update source:", err)
	}

	if err := installSelf(); err != nil {
		return err
	}
	if err := sysinstall.PatchPfConf(printf); err != nil {
		return err
	}
	if err := sysinstall.WritePlist(configPath); err != nil {
		return err
	}

	if err := sysinstall.RestartService(printf); err != nil {
		return err
	}
	if err := waitForSocket(configPath, 15*time.Second); err != nil {
		return err
	}
	fmt.Println("service", LaunchDaemonLabel, "started")
	return nil
}

// Uninstall снимает службу и правки pf.conf, оставляя конфиг на месте.
func Uninstall(configPath string) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("root required: sudo splitr uninstall")
	}
	if configPath == "" {
		configPath = config.DefaultPath
	}
	_ = exec.Command("/bin/launchctl", "bootout", "system/"+LaunchDaemonLabel).Run()
	_ = os.Remove(launchDaemonPlist)

	if err := sysinstall.UnpatchPfConf(); err != nil {
		return err
	}

	// Демон намеренно держал pf включённым и после своей остановки, чтобы
	// блокировка не пропадала на время перезапуска. Снятие продукта —
	// единственный момент, когда эту ссылку нужно отпустить.
	cfg, err := config.Load(configPath)
	if err != nil {
		cfg = config.Default()
	}
	if token, err := daemon.LoadPFToken(cfg.Daemon.StateFile); err == nil && token != "" {
		if err := pfctl.New().Release(token); err != nil {
			fmt.Println("could not release the pf reference:", err)
		} else {
			fmt.Println("pf reference released")
		}
		_ = os.Remove(cfg.Daemon.StateFile)
	}
	if err := os.WriteFile(protect.AnchorFile, []byte("# splitr removed\n"), 0o644); err != nil {
		return err
	}

	// Снять правила с диска мало: в ядре они остаются загруженными, и машина
	// после «удаления» продолжает резать защищаемые сети без единого способа
	// это выключить — демона-то уже нет. Поэтому чистим якорь сразу.
	pf := pfctl.New()
	if err := pf.FlushAnchor(protect.Anchor); err != nil {
		fmt.Println("could not flush the pf anchor:", err)
		fmt.Println("do it by hand: sudo pfctl -a", protect.Anchor, "-F all")
	} else {
		fmt.Println("block rules removed from pf")
	}
	// Перезагрузка главного набора убирает и сам вызов якоря. Она сносит
	// динамические якоря, поэтому при живом туннеле её лучше не делать.
	if anchors, err := pf.SshuttleAnchors(); err == nil && len(anchors) > 0 {
		fmt.Println("sshuttle anchors present, leaving the main ruleset alone")
		fmt.Println("once the tunnel is down: sudo pfctl -f", protect.PfConf)
	} else if err := pf.ReloadMain(protect.PfConf); err != nil {
		fmt.Println("reloading", protect.PfConf, "failed:", err)
	}
	fmt.Println("service removed, pf.conf restored, protection off; config and binary left in place")
	return nil
}

func serviceLoaded() bool { return sysinstall.ServiceLoaded() }

// waitForSocket ждёт, пока демон поднимет управляющий сокет:
// без этого сразу после install команда status упирается в отсутствующий файл.
func waitForSocket(configPath string, timeout time.Duration) error {
	socket := config.Default().Daemon.SocketPath
	if cfg, err := config.Load(configPath); err == nil && cfg.Daemon.SocketPath != "" {
		socket = cfg.Daemon.SocketPath
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(socket); err == nil {
			fmt.Println("control socket ready:", socket)
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("the daemon did not create socket %s within %s; see /usr/local/var/log/splitr.err.log", socket, timeout)
}

// installSelf кладёт на место запущенный сейчас бинарь.
func installSelf() error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	self, err = filepath.EvalSymlinks(self)
	if err != nil {
		return err
	}
	return sysinstall.InstallBinary(self, installedBinary, printf)
}
