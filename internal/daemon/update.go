package daemon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/tasticolly/splitr/internal/config"
	"github.com/tasticolly/splitr/internal/sysinstall"
	"github.com/tasticolly/splitr/internal/update"
)

// UpdateState отдаёт состояние обновления, спрашивая git не чаще
// update.check_interval.
//
// Кеш обязателен: статус опрашивает приложение в строке меню, и без него
// каждая отрисовка меню запускала бы процесс git от root.
func (d *Daemon) UpdateState() update.State {
	cfg := d.Config()
	interval := cfg.Update.CheckInterval
	if interval <= 0 {
		interval = config.Default().Update.CheckInterval
	}

	d.updateMu.Lock()
	defer d.updateMu.Unlock()
	// Смена repo_path в конфиге обесценивает кеш: он про другой репозиторий.
	fresh := !d.updateAt.IsZero() && time.Since(d.updateAt) < interval
	if fresh && d.lastUpdate.RepoPath == cfg.Update.RepoPath {
		return d.lastUpdate
	}
	d.lastUpdate = d.checkUpdate(cfg.Update.RepoPath, d.version)
	d.updateAt = time.Now()
	return d.lastUpdate
}

// RefreshUpdate спрашивает git заново, не глядя на кеш.
// Нужен перед самой установкой: ставится то, что в репозитории прямо сейчас.
func (d *Daemon) RefreshUpdate() update.State {
	d.updateMu.Lock()
	d.updateAt = time.Time{}
	d.updateMu.Unlock()
	return d.UpdateState()
}

// ApplyUpdate ставит на место бинарь, собранный в репозитории.
//
// Репозиторий берётся ИЗ КОНФИГА, а не из запроса: иначе всякий, кто дотянулся
// до управляющего сокета, подсунул бы произвольный бинарь, который launchd
// тут же запустит от root.
//
// Сборки здесь нет намеренно: собирает приложение от имени пользователя.
// Собери демон сам — и в репозитории появились бы артефакты, принадлежащие
// root, после чего обычная `make build` перестала бы работать.
func (d *Daemon) ApplyUpdate() (update.State, error) {
	st := d.RefreshUpdate()
	if !st.Available {
		reason := st.Reason
		if reason == "" {
			reason = "there is no newer version"
		}
		return st, fmt.Errorf("nothing to install: %s", reason)
	}

	src := filepath.Join(st.RepoPath, "bin", "splitr")
	info, err := os.Stat(src)
	if err != nil {
		return st, fmt.Errorf("%s is missing; build it first: make update", src)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return st, fmt.Errorf("%s is not an executable file", src)
	}
	// Сравнение по времени правки отсекает самый частый случай: тег в
	// репозитории новый, а бинарь остался от прошлой сборки. Поставить его
	// значило бы объявить обновление выполненным, ничего не изменив.
	if installed, err := os.Stat(d.installedPath); err == nil && !info.ModTime().After(installed.ModTime()) {
		return st, fmt.Errorf("%s is older than the installed %s; build the new version first: make update", src, d.installedPath)
	}

	// Бинарь спрашивается о версии до подмены. Это разом проверяет и что он
	// вообще запускается: подменить работающий демон на неработающий — значит
	// остаться и без защиты, и без способа её вернуть.
	got, err := d.binaryVersion(src)
	if err != nil {
		return st, fmt.Errorf("%s does not run: %w", src, err)
	}
	if got != st.Latest {
		// Two different failures used to share one message, and the advice
		// only fitted one of them. A binary left over from an older tag does
		// need rebuilding. A binary built from commits made after the tag
		// cannot be fixed that way: `git describe` will keep appending the
		// distance to the tag, so "make update" lands the user right back
		// here. That case needs a tag, not another build.
		if aheadOfTag(got, st.Latest) {
			return st, fmt.Errorf("%s was built from a checkout that sits after the latest tag: it reports %s while the tag is %s. Rebuilding will not change that; tag the new version first with make release V=..., or check out %s", src, got, st.Latest, st.Latest)
		}
		return st, fmt.Errorf("%s reports version %s, but the latest tag is %s; rebuild it: make update", src, got, st.Latest)
	}

	if err := sysinstall.InstallBinary(src, d.installedPath, d.logf); err != nil {
		return st, err
	}
	if err := d.finishInstall(d.configPath); err != nil {
		return st, fmt.Errorf("binary installed, but finishing the installation failed: %w", err)
	}
	d.logf("updated from %s to %s", st.Installed, st.Latest)
	return st, nil
}

// restart завершает процесс, чтобы launchd (KeepAlive) поднял новую версию.
//
// Выход отложен и вынесен в отдельную горутину: ответ на POST /update обязан
// дойти до клиента, иначе приложение увидит оборванное соединение вместо «ок».
// Правила pf при этом остаются загруженными, а ссылка на pf — не отпущенной,
// как и при любой другой остановке демона: перезапуск не должен снимать защиту
// даже на секунду.
func (d *Daemon) restart() {
	go func() {
		time.Sleep(d.restartDelay)
		d.logf("restarting to run the new version; pf rules stay loaded")
		d.exit(0)
	}()
}

// binaryVersion спрашивает у собранного бинаря его версию.
func binaryVersion(path string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, path, "--version").Output()
	if err != nil {
		return "", err
	}
	// Вывод — «splitr v0.2.0»; версия всегда последним словом.
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return "", fmt.Errorf("it printed nothing in response to --version")
	}
	return fields[len(fields)-1], nil
}

// aheadOfTag tells the two version mismatches apart. `git describe --tags`
// spells a checkout that has moved past its tag as "<tag>-<commits>-g<hash>",
// optionally with "-dirty", so the tag being a prefix followed by a dash is
// what distinguishes "newer than the tag" from "some other version entirely".
func aheadOfTag(version, tag string) bool {
	return tag != "" && strings.HasPrefix(version, tag+"-")
}
