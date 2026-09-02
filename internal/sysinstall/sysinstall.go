// Package sysinstall — системная часть установки SplitR: бинарь в
// /usr/local/bin, плист launchd и вызов якоря в /etc/pf.conf.
//
// Вынесено из internal/cli, потому что ровно тем же самым занимается
// обновление через демона (POST /update), а демон не может импортировать cli:
// cli уже зависит от daemon, и вышел бы цикл импортов. Команда `splitr install`
// работает через эти же функции, поэтому расхождению между установкой
// и обновлением взяться неоткуда.
package sysinstall

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/tasticolly/splitr/internal/protect"
)

const (
	// LaunchDaemonLabel — идентификатор службы в launchd.
	LaunchDaemonLabel = "com.splitr.daemon"
	// PlistPath — файл описания службы.
	PlistPath = "/Library/LaunchDaemons/com.splitr.daemon.plist"
	// BinaryPath — куда ставится бинарь.
	BinaryPath = "/usr/local/bin/splitr"

	// PfConfMarkerStart и PfConfMarkerEnd ограничивают вставку в /etc/pf.conf,
	// чтобы её можно было убрать целиком и не тронуть чужие правила.
	PfConfMarkerStart = "# --- splitr begin ---"
	PfConfMarkerEnd   = "# --- splitr end ---"
)

// Logf — куда рассказывать о ходе работ: CLI печатает на экран,
// демон пишет в свой журнал. Пустое значение допустимо — тогда молча.
type Logf func(format string, args ...any)

func (l Logf) say(format string, args ...any) {
	if l != nil {
		l(format, args...)
	}
}

// InstallBinary кладёт бинарь из src в dst.
//
// Запись поверх работающего бинаря даёт ETXTBSY, поэтому пишем во временный
// файл рядом и переименовываем: rename в пределах одной файловой системы
// атомарен, и оборванное обновление не оставит на месте splitr огрызок.
func InstallBinary(src, dst string, log Logf) error {
	if src == dst {
		return nil
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	tmp := dst + ".new"
	if err := os.WriteFile(tmp, data, 0o755); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("install %s: %w", dst, err)
	}
	log.say("binary installed at %s", dst)
	return nil
}

// PatchPfConf добавляет вызов якоря splitr в главный набор правил pf.
// Без этой строки правила внутри якоря не вычисляются вообще.
func PatchPfConf(log Logf) error {
	raw, err := os.ReadFile(protect.PfConf)
	if err != nil {
		return fmt.Errorf("read %s: %w", protect.PfConf, err)
	}
	if strings.Contains(string(raw), PfConfMarkerStart) {
		log.say("%s is already patched", protect.PfConf)
		return nil
	}

	backup := fmt.Sprintf("%s.splitr-backup-%s", protect.PfConf, time.Now().Format("20060102-150405"))
	if err := os.WriteFile(backup, raw, 0o644); err != nil {
		return fmt.Errorf("create backup %s: %w", backup, err)
	}

	block := fmt.Sprintf("\n%s\nanchor \"%s\"\nload anchor \"%s\" from \"%s\"\n%s\n",
		PfConfMarkerStart, protect.Anchor, protect.Anchor, protect.AnchorFile, PfConfMarkerEnd)
	if err := os.WriteFile(protect.PfConf, append(raw, []byte(block)...), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", protect.PfConf, err)
	}
	if _, err := os.Stat(protect.AnchorFile); os.IsNotExist(err) {
		if err := os.WriteFile(protect.AnchorFile, []byte("# splitr: rules not generated yet\n"), 0o644); err != nil {
			return err
		}
	}
	log.say("%s patched (backup: %s)", protect.PfConf, backup)
	return nil
}

// UnpatchPfConf убирает вставку целиком.
func UnpatchPfConf() error {
	raw, err := os.ReadFile(protect.PfConf)
	if err != nil {
		return err
	}
	s := string(raw)
	start := strings.Index(s, PfConfMarkerStart)
	end := strings.Index(s, PfConfMarkerEnd)
	if start < 0 || end < 0 {
		return nil
	}
	cleaned := s[:start] + s[end+len(PfConfMarkerEnd)+1:]
	return os.WriteFile(protect.PfConf, []byte(strings.TrimRight(cleaned, "\n")+"\n"), 0o644)
}

// WritePlist описывает службу для launchd.
func WritePlist(configPath string) error {
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>%s</string>
  <key>ProgramArguments</key>
  <array>
    <string>%s</string>
    <string>daemon</string>
    <string>--config</string>
    <string>%s</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>ProcessType</key><string>Background</string>
  <key>StandardOutPath</key><string>/usr/local/var/log/splitr.out.log</string>
  <key>StandardErrorPath</key><string>/usr/local/var/log/splitr.err.log</string>
  <key>EnvironmentVariables</key>
  <dict><key>PATH</key><string>/usr/local/bin:/opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin</string></dict>
</dict>
</plist>
`, LaunchDaemonLabel, BinaryPath, configPath)
	if err := os.WriteFile(PlistPath, []byte(plist), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", PlistPath, err)
	}
	return os.Chown(PlistPath, 0, 0)
}

// ServiceLoaded отвечает, знает ли launchd о службе.
func ServiceLoaded() bool {
	return exec.Command("/bin/launchctl", "print", "system/"+LaunchDaemonLabel).Run() == nil
}

// RestartService перезапускает службу launchd.
// bootout асинхронный: пока служба не исчезла из домена, bootstrap падает
// с «Bootstrap failed: 5: Input/output error», поэтому ждём и повторяем.
func RestartService(log Logf) error {
	_ = exec.Command("/bin/launchctl", "bootout", "system/"+LaunchDaemonLabel).Run()
	if err := WaitServiceGone(10 * time.Second); err != nil {
		return err
	}

	var lastErr error
	for attempt := range 10 {
		out, err := exec.Command("/bin/launchctl", "bootstrap", "system", PlistPath).CombinedOutput()
		if err == nil {
			log.say("service %s started", LaunchDaemonLabel)
			return nil
		}
		lastErr = fmt.Errorf("launchctl bootstrap: %w: %s", err, strings.TrimSpace(string(out)))
		time.Sleep(time.Duration(attempt+1) * 300 * time.Millisecond)
	}
	return lastErr
}

// WaitServiceGone ждёт, пока launchd действительно выгрузит службу.
func WaitServiceGone(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !ServiceLoaded() {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("service %s did not unload within %s; try `sudo launchctl bootout system/%s` by hand",
		LaunchDaemonLabel, timeout, LaunchDaemonLabel)
}
