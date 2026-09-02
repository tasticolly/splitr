// Package daemon — фоновый процесс splitr: сторож pf, менеджер туннеля и API.
package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/tasticolly/splitr/internal/config"
	"github.com/tasticolly/splitr/internal/netmon"
	"github.com/tasticolly/splitr/internal/pfctl"
	"github.com/tasticolly/splitr/internal/protect"
	"github.com/tasticolly/splitr/internal/shuttle"
	"github.com/tasticolly/splitr/internal/sysinstall"
	"github.com/tasticolly/splitr/internal/update"
)

// Version подставляется при сборке через -ldflags.
var Version = "dev"

// Daemon связывает конфиг, Ð·Ð°ÑÐ¸ÑÐ° и процесс sshuttle.
type Daemon struct {
	configPath string
	log        io.Writer
	runner     Tunnel
	pf         pfctl.Controller
	ops        SystemOps

	mu         sync.RWMutex
	cfg        config.Config
	activeName string
	strictMode bool
	warnings   []string
	// fileWarning живёт отдельно от warnings: список сторожа
	// перезаписывается каждый тик и затёр бы жалобу на запись файла якоря.
	fileWarning string

	// Переподключение отодвигается по нарастающей: без этого сторож
	// дёргал бы sshuttle и update_dns каждые несколько секунд подряд.
	reconnectAt    time.Time
	reconnectFails int

	// dnsBackup is the resolver list update_script displaced, kept so the
	// redirect can be undone; dnsRedirected says whether one is outstanding.
	dnsBackup     []string
	dnsRedirected bool

	// pfToken и previousPFToken живут в горутине Run и не требуют мьютекса.
	pfToken         string
	previousPFToken string

	// Состояние обновления под своим замком: проверка ходит в git, и держать
	// на это время общий mu значило бы подвесить статус и весь API.
	updateMu   sync.Mutex
	lastUpdate update.State
	updateAt   time.Time

	// Швы для тестов: пути и внешние команды, которые нельзя дёргать
	// на машине разработчика. Тесты лежат в этом же пакете и подменяют их
	// напрямую, поэтому экспортировать сеттеры не нужно.
	anchorFile     string
	startedAt      time.Time
	version        string
	installedPath  string
	restartDelay   time.Duration
	checkUpdate    func(repoPath, installed string) update.State
	binaryVersion  func(path string) (string, error)
	finishInstall  func(configPath string) error
	exit           func(code int)
	dial           func(ctx context.Context, network, address string) (net.Conn, error)
	pflogUp        func() error
	shuttleRunning func(binary string) (bool, error)
	blockedCmd     func(ctx context.Context) *exec.Cmd
}

// New создаёт демон с уже загруженным конфигом и боевым pf.
func New(cfg config.Config, configPath string, log io.Writer) *Daemon {
	return NewWithDeps(cfg, configPath, log, pfctl.New(), shuttle.NewRunner(cfg.Sshuttle, log), execOps{})
}

// NewWithDeps позволяет подменить зависимости — этим пользуются тесты.
func NewWithDeps(cfg config.Config, configPath string, log io.Writer, pf pfctl.Controller, tunnel Tunnel, ops SystemOps) *Daemon {
	d := &Daemon{
		configPath: configPath,
		log:        log,
		cfg:        cfg,
		activeName: cfg.DefaultProfile,
		runner:     tunnel,
		pf:         pf,
		ops:        ops,
		anchorFile: protect.AnchorFilePath(),
		startedAt:  time.Now(),
	}
	d.dial = func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
	d.pflogUp = d.ensurePflog
	d.shuttleRunning = shuttle.Running
	d.blockedCmd = blockedTcpdump
	d.version = Version
	d.installedPath = sysinstall.BinaryPath
	d.restartDelay = 300 * time.Millisecond
	d.checkUpdate = update.NewChecker().State
	d.binaryVersion = binaryVersion
	// Плист переписывается на случай, если у новой версии изменился запуск,
	// а pf.conf патчится, если вставки там почему-то не оказалось. Обе
	// операции идемпотентны — повторное обновление ничего не ломает.
	d.finishInstall = func(configPath string) error {
		if err := sysinstall.WritePlist(configPath); err != nil {
			return err
		}
		return sysinstall.PatchPfConf(d.logf)
	}
	d.exit = os.Exit
	return d
}

func (d *Daemon) logf(format string, args ...any) {
	fmt.Fprintf(d.log, "[%s] "+format+"\n", append([]any{time.Now().Format(time.RFC3339)}, args...)...)
}

// Run держит Ð·Ð°ÑÐ¸ÑÐ° применённым и следит за туннелем до отмены контекста.
func (d *Daemon) Run(ctx context.Context) error {
	d.restoreState()

	token, err := d.pf.Enable()
	if err != nil {
		return fmt.Errorf("enable pf: %w", err)
	}
	d.pfToken = token
	d.logf("pf enabled, token %s", token)

	// Новая ссылка взята — теперь можно отпустить ссылку прошлого запуска.
	// Порядок важен: наоборот pf успел бы выключиться, а вместе с ним
	// пропала бы вся блокировка на время перезапуска службы.
	if prev := d.previousPFToken; prev != "" && prev != token {
		if err := d.pf.Release(prev); err != nil {
			d.logf("could not release the previous pf reference (%s): %v", prev, err)
		}
	}
	d.saveState()

	// Ссылка на pf намеренно НЕ освобождается при остановке демона:
	// иначе выключение службы снимало бы блокировку. Отпускает её uninstall.

	if err := d.ApplyProtection(); err != nil {
		d.logf("initial protection apply failed: %v", err)
	}

	d.mu.RLock()
	interval := d.cfg.Daemon.WatchdogInterval
	d.mu.RUnlock()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// События ядра — ускоритель, а не замена тикеру. Тикер ловит то, о чём
	// ядро не рассказывает: чужой `pfctl -F all`, смерть sshuttle, ручную
	// правку правил. Убрать его значило бы разменять надёжность на изящество.
	events := d.netEvents(ctx)

	tunnelWasUp := false
	for {
		select {
		case <-ctx.Done():
			d.logf("daemon stopping; pf stays enabled, block rules stay in place")
			return d.runner.Stop(context.Background())
		case ev := <-events:
			tunnelWasUp = d.onNetworkEvent(ev, tunnelWasUp)
		case <-ticker.C:
			tunnelWasUp = d.tick(tunnelWasUp)
		}
	}
}

// netEvents подписывается на события сети и сна.
// Отдельный метод — шов для тестов: подписка лезет в ядро, а тесты сторожа
// должны оставаться без сети и без таймингов.
func (d *Daemon) netEvents(ctx context.Context) <-chan netmon.Event {
	return netmon.Start(ctx, d.logf, netmon.Options{}).C
}

// onNetworkEvent — внеочередная проверка после смены сети или пробуждения.
//
// Именно здесь мы вели себя хуже всего: после смены Wi-Fi или закрытия крышки
// туннель мёртв, а демон узнавал об этом лишь очередным тиком и только затем
// начинал отсчитывать нарастающую задержку переподключения — то есть в самый
// нужный момент ждал дольше всего.
func (d *Daemon) onNetworkEvent(ev netmon.Event, wasUp bool) bool {
	d.logf("%s — out-of-band state check", ev.Reason)

	if ev.Wake {
		// Задержка переподключения копилась из-за прошлых неудач, но после сна
		// причина этих неудач (сети не было) заведомо неактуальна.
		d.mu.Lock()
		d.reconnectAt = time.Time{}
		d.reconnectFails = 0
		d.mu.Unlock()
	}

	up := d.tick(wasUp)

	// Состояния pf переживают и сон, и смену сети: соединение, открытое в
	// прошлой сети, продолжит идти напрямую мимо блокировки, пока его состояние
	// живо. При поднятом туннеле их трогать нельзя — умрёт сам туннель.
	if ev.Wake && !up {
		d.onTunnelLost()
	}
	return up
}

// tick — одна итерация сторожа; возвращает актуальное «туннель поднят».
func (d *Daemon) tick(wasUp bool) bool {
	var warns []string

	tunnelUp, err := d.tunnelAlive()
	if err != nil {
		warns = append(warns, err.Error())
	}
	if tunnelUp {
		d.runner.MarkUp()
	}

	if wasUp && !tunnelUp {
		d.logf("tunnel is gone — protection now drops protected routes")
		d.onTunnelLost()
	}

	// A DNS redirect with no sshuttle behind it is undone here rather than only
	// on the up-to-down edge above, because the edge never fires for the case
	// that caused the bug: a tunnel that never came up at all. update_script
	// had already pointed the resolver at 127.0.0.1, sshuttle then failed to
	// reach the host and exited, and since the tunnel was never up there was no
	// transition to react to. The condition is the running process, not the
	// tunnel being up, so that the seconds sshuttle spends connecting are not
	// mistaken for a failure and do not pull the resolver out from under it.
	if !d.runner.Running() {
		d.restoreDNS()
	}

	if err := d.ensurePF(); err != nil {
		warns = append(warns, err.Error())
	}
	if err := d.ensureAnchor(tunnelUp); err != nil {
		warns = append(warns, err.Error())
	}

	d.mu.Lock()
	d.warnings = warns
	autoreconnect := d.cfg.Daemon.Autoreconnect
	d.mu.Unlock()

	if autoreconnect && !tunnelUp && !d.runner.Running() {
		d.reconnect()
	}

	// Проверка новой версии живёт здесь, а не отдельным таймером: кеш всё
	// равно не пустит её в git чаще, чем раз в check_interval, зато состояние
	// готово к моменту, когда приложение спросит статус.
	d.UpdateState()
	return tunnelUp
}

// liveSshuttleAnchors оставляет только якоря sshuttle, в которых реально есть
// правила.
//
// Настоящий pf не удаляет якорь из дерева после очистки: `pfctl -s Anchors`
// продолжает показывать пустую оболочку от давно умершего туннеля. Перебить
// нашу блокировку такой якорь не может — в нём нет ни одного правила, — а вот
// демона он вводил в заблуждение навсегда.
func (d *Daemon) liveSshuttleAnchors() ([]string, error) {
	names, err := d.pf.SshuttleAnchors()
	if err != nil {
		return nil, fmt.Errorf("could not read pf anchors: %w", err)
	}

	if len(names) == 0 {
		return nil, nil
	}

	var withRules []string
	for _, name := range names {
		rules, err := d.pf.AnchorRules(name)
		if err != nil {
			// Не смогли заглянуть внутрь — считаем якорь значимым.
			withRules = append(withRules, name)
			continue
		}
		if strings.TrimSpace(rules) != "" {
			withRules = append(withRules, name)
		}
	}
	if len(withRules) > 0 {
		return withRules, nil
	}

	// Все якоря пусты — и это тот единственный случай, когда по одному только
	// pf решить нельзя. Пустая оболочка остаётся и от давно умершего туннеля,
	// и у живого, чьи правила нам почему-то не видны. Спрашиваем систему:
	// хозяин у якоря есть — значит трогать его нельзя, хозяина нет — значит
	// это мусор, и защита обязана действовать.
	running, err := d.shuttleRunning(d.Config().Sshuttle.Path)
	if err != nil || running {
		return names, nil
	}
	return nil, nil
}

// tunnelAlive отвечает, поднят ли туннель на самом деле, и попутно убирает
// осиротевшие якоря.
//
// Одного имени якоря `sshuttle-*` мало: sshuttle, убитый по SIGKILL или
// оборвавшийся вместе с ssh, оставляет якорь с живыми правилами. Демон
// принимал такой мусор за живой туннель, а значит переставал чинить главный
// набор правил («чтобы не порвать туннель») — и защита не включалась вообще,
// молча и до перезагрузки.
func (d *Daemon) tunnelAlive() (bool, error) {
	anchors, err := d.liveSshuttleAnchors()
	if err != nil {
		return false, err
	}
	if len(anchors) == 0 {
		return false, nil
	}
	if d.runner.Running() {
		return true, nil
	}

	running, err := d.shuttleRunning(d.Config().Sshuttle.Path)
	if err != nil {
		// Не смогли проверить — считаем туннель живым: лишняя осторожность
		// дешевле, чем снести якоря работающего туннеля.
		return true, err
	}
	if running {
		return true, nil
	}

	d.logf("sshuttle anchors (%s) are orphaned: no sshuttle process, removing them", strings.Join(anchors, ", "))
	for _, anchor := range anchors {
		if err := d.pf.FlushAnchor(anchor); err != nil {
			return true, fmt.Errorf("remove orphaned anchor %s: %w", anchor, err)
		}
	}
	return false, nil
}

// reconnect поднимает туннель заново, отодвигая следующую попытку
// с нарастающей задержкой после каждой неудачи.
func (d *Daemon) reconnect() {
	d.mu.Lock()
	if time.Now().Before(d.reconnectAt) {
		d.mu.Unlock()
		return
	}
	base := d.cfg.Daemon.ReconnectDelay
	fails := d.reconnectFails
	d.mu.Unlock()

	if base <= 0 {
		base = 5 * time.Second
	}

	err := d.Up("")

	d.mu.Lock()
	defer d.mu.Unlock()
	if err == nil {
		d.reconnectFails = 0
		d.reconnectAt = time.Now().Add(base)
		return
	}
	d.reconnectFails = fails + 1
	backoff := base << min(d.reconnectFails, 5)
	if maxBackoff := 5 * time.Minute; backoff > maxBackoff {
		backoff = maxBackoff
	}
	d.reconnectAt = time.Now().Add(backoff)
	d.logf("autoreconnect failed (attempt %d): %v; next try in %s", d.reconnectFails, err, backoff)
}

func (d *Daemon) onTunnelLost() {
	// The DNS redirect is undone before anything else, and unconditionally.
	//
	// This is the bug it fixes: update_script points the system resolver at
	// 127.0.0.1, where sshuttle answers DNS through the tunnel. The redirect
	// used to be applied on the way up and never taken back, so when sshuttle
	// died - a dropped tunnel, an unreachable host, a crash - the resolver was
	// left aimed at a port nobody was listening on. Every name lookup on the
	// machine then had to wait out a timeout before falling back to the second
	// resolver, which looks exactly like "the network is broken" and has
	// nothing to do with the tunnel that failed.
	d.restoreDNS()

	d.mu.RLock()
	killStates := d.cfg.Protection.KillStates
	d.mu.RUnlock()
	if !killStates {
		return
	}
	d.flushBlockedStates()
}

// backupDNS records the current resolvers so the redirect can be undone.
// A failure here is logged and not fatal: refusing to bring the tunnel up
// because the resolvers could not be read would trade a small problem for a
// bigger one. The redirect is still marked, so the restore path will at least
// clear it back to the DHCP-provided servers.
func (d *Daemon) backupDNS() {
	servers, err := d.ops.SnapshotDNS()
	if err != nil {
		d.logf("could not record the current resolvers, the restore will fall back to DHCP: %v", err)
	}
	d.mu.Lock()
	d.dnsBackup = servers
	d.dnsRedirected = true
	d.mu.Unlock()
	d.saveState()
}

// restoreDNS puts the resolvers back and forgets the backup. It is a no-op
// unless a redirect is actually outstanding, so the repeated calls the watchdog
// makes while the tunnel is down do not keep rewriting the system settings.
func (d *Daemon) restoreDNS() {
	d.mu.Lock()
	redirected := d.dnsRedirected
	servers := d.dnsBackup
	d.dnsRedirected = false
	d.dnsBackup = nil
	d.mu.Unlock()
	if !redirected {
		return
	}

	if err := d.ops.RestoreDNS(servers); err != nil {
		d.logf("could not restore the resolvers: %v", err)
	} else if len(servers) == 0 {
		d.logf("resolvers restored to the ones DHCP hands out")
	} else {
		d.logf("resolvers restored: %s", strings.Join(servers, ", "))
	}

	d.mu.RLock()
	flush := d.cfg.DNS.FlushCache
	d.mu.RUnlock()
	if flush {
		if err := d.ops.FlushDNSCache(); err != nil {
			d.logf("dns cache flush: %v", err)
		}
	}
	d.saveState()
}

// flushBlockedStates убивает живые состояния pf к блокируемым сетям.
//
// Новое правило блокировки само по себе не рвёт уже установленные соединения:
// pf пропускает пакет по записи в таблице состояний, не заглядывая в правила
// заново. То есть без этого сброса свежая блокировка действует только на новые
// соединения, а те, что уже открыты, продолжают спокойно светить трафиком.
func (d *Daemon) flushBlockedStates() {
	rs, err := d.ruleset()
	if err != nil {
		d.logf("could not build the ruleset: %v", err)
		return
	}
	if err := rs.KillStates(d.pf); err != nil {
		d.logf("%v", err)
	}
}

func (d *Daemon) ensurePF() error {
	enabled, err := d.pf.Enabled()
	if err != nil {
		return fmt.Errorf("check pf state: %w", err)
	}
	if enabled {
		return nil
	}
	d.logf("pf turned out to be disabled, enabling it again")
	token, err := d.pf.Enable()
	if err != nil {
		return fmt.Errorf("enable pf: %w", err)
	}
	d.pfToken = token
	return d.ApplyProtection()
}

// ensureAnchor чинит якорь, если его правила потерялись
// (например, после чужого `pfctl -F all` или перезагрузки pf.conf).
func (d *Daemon) ensureAnchor(tunnelUp bool) error {
	rs, err := d.ruleset()
	if err != nil {
		return err
	}
	if rs.Empty() {
		return nil
	}

	loaded, err := d.pf.AnchorRules(protect.Anchor)
	if err != nil {
		return fmt.Errorf("read anchor %s: %w", protect.Anchor, err)
	}
	if drifted, why := rulesDrifted(loaded, rs); drifted {
		d.logf("anchor %s drifted from the expected rules (%s), reloading", protect.Anchor, why)
		if err := rs.Apply(d.pf); err != nil {
			return err
		}
	}

	referenced, err := pfctl.AnchorReferenced(d.pf, protect.Anchor)
	if err != nil {
		return err
	}
	if referenced {
		return nil
	}
	if tunnelUp {
		// Перезагрузка pf.conf снесла бы якоря sshuttle и порвала туннель.
		return fmt.Errorf("anchor %s is not linked into the main ruleset; repair deferred until the tunnel goes down", protect.Anchor)
	}
	d.logf("anchor %s is not linked, reloading %s", protect.Anchor, protect.PfConf)
	if err := d.pf.ReloadMain(protect.PfConf); err != nil {
		return err
	}
	return rs.Apply(d.pf)
}

// rulesDrifted сравнивает то, что реально загружено в якорь, с тем, что должно
// быть. Побайтово сверять нельзя: pfctl печатает правила в своей канонической
// форме, отличной от той, что мы подаём на вход. Поэтому проверяются признаки,
// потеря которых означает потерю защиты.
func rulesDrifted(loaded string, rs protect.Ruleset) (bool, string) {
	if strings.TrimSpace(loaded) == "" {
		return true, "anchor is empty"
	}
	wantBlocks := 0
	if len(rs.Block) > 0 {
		wantBlocks++
	}
	if len(rs.DNSServers) > 0 {
		wantBlocks++
	}
	if got := strings.Count(loaded, "block drop out"); got != wantBlocks {
		return true, fmt.Sprintf("%d block rules instead of %d", got, wantBlocks)
	}
	if hasQuick := strings.Contains(loaded, "quick"); hasQuick != rs.Strict {
		return true, fmt.Sprintf("quick=%v with strict=%v", hasQuick, rs.Strict)
	}
	if wantPass := len(rs.Pass) > 0; wantPass != strings.Contains(loaded, "pass out") {
		return true, "pass rule presence does not match"
	}
	return false, ""
}

func (d *Daemon) ruleset() (protect.Ruleset, error) {
	d.mu.RLock()
	cfg, name, strictMode := d.cfg, d.activeName, d.strictMode
	d.mu.RUnlock()

	if !cfg.Protection.Enabled && !strictMode {
		return protect.Ruleset{}, nil
	}
	_, p, err := cfg.Profile(name)
	if err != nil {
		return protect.Ruleset{}, err
	}
	return protect.Build(cfg, p, strictMode), nil
}

// ApplyProtection пересобирает и загружает правила блокировки.
//
// Порядок важен: сначала правила уходят в ядро, и только потом на диск.
// В ядре живёт защита прямо сейчас, а файл нужен лишь для восстановления
// при загрузке системы — поэтому неудачная запись файла не должна оставлять
// pf со старыми правилами, пока демон считает, что применил новые.
func (d *Daemon) ApplyProtection() error {
	rs, err := d.ruleset()
	if err != nil {
		return err
	}
	if err := d.pflogUp(); err != nil {
		d.logf("drop logging: %v", err)
	}

	if rs.Empty() {
		if err := d.pf.FlushAnchor(protect.Anchor); err != nil {
			return err
		}
	} else if err := rs.Apply(d.pf); err != nil {
		return err
	}

	err = d.writeAnchorFile(rs)
	d.mu.Lock()
	if err != nil {
		d.fileWarning = err.Error()
	} else {
		d.fileWarning = ""
	}
	d.mu.Unlock()
	if err != nil {
		d.logf("%v", err)
	}
	return nil
}

func (d *Daemon) writeAnchorFile(rs protect.Ruleset) error {
	path := d.anchorFile
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create the anchors directory: %w", err)
	}
	if err := os.WriteFile(path, rs.Rules(), 0o644); err != nil {
		return fmt.Errorf("rules applied, but writing %s failed — they will not survive a reboot: %w", path, err)
	}
	return nil
}

// checkRemote убеждается, что до хоста вообще можно дойти по ssh.
//
// Самый частый отказ здесь — не сеть, а требование заново войти: Tailscale SSH
// раз в несколько часов просит подтверждения через браузер, печатает ссылку и
// закрывает соединение с кодом 255. Ссылку надо донести до человека, а не
// потерять: без неё отказ выглядит необъяснимым, а починить его нечем.
func (d *Daemon) checkRemote(cfg config.Config, p config.Profile) error {
	remote := p.Remote
	out, err := d.ops.Reachable(shuttle.SSHCommand(cfg.Sshuttle, p), remote)
	if err == nil {
		d.runner.ClearAction()
		return nil
	}

	text := strings.TrimSpace(string(out))
	if url := shuttle.DetectAuthURL(text); url != "" {
		d.runner.SetAction(shuttle.AuthAction(url))
		d.logf("%s requires re-authentication, open: %s", remote, url)
		return fmt.Errorf("%s requires re-authentication: %s", remote, url)
	}

	d.logf("%s is not reachable over ssh: %v: %s", remote, err, text)
	return fmt.Errorf("%s is not reachable over ssh: %s", remote, firstLine(text))
}

// firstLine оставляет от вывода ssh первую содержательную строку:
// в интерфейс не нужно тащить многострочную диагностику.
func firstLine(text string) string {
	for _, line := range strings.Split(text, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return "no output"
}

// Up поднимает туннель по имени профиля (пустое имя — профиль по умолчанию).
func (d *Daemon) Up(profileName string) error {
	d.mu.Lock()
	cfg := d.cfg
	d.mu.Unlock()

	name, p, err := cfg.Profile(profileName)
	if err != nil {
		return err
	}

	// Проверка «уже поднят» обязана идти до гашения посторонних процессов:
	// иначе повторный up сначала убивал бы собственный живой туннель.
	if snap := d.runner.Snapshot(); d.runner.Running() {
		if snap.Profile == name {
			d.logf("tunnel is already up with profile %s, nothing to do", name)
			return nil
		}
		d.logf("profile change %s -> %s, taking the current tunnel down", snap.Profile, name)
		if err := d.runner.Stop(context.Background()); err != nil {
			return fmt.Errorf("take the previous tunnel down: %w", err)
		}
	}

	// Проверка достижимости идёт до всего остального. Раньше при недоступном
	// хосте мы всё равно гасили посторонние процессы, переписывали системный
	// DNS и запускали sshuttle, который через несколько секунд молча умирал.
	// Человек видел «tunnel failed» без причины и жал Connect снова и снова.
	if err := d.checkRemote(cfg, p); err != nil {
		return err
	}

	if err := d.runner.KillForeign(); err != nil {
		d.logf("%v", err)
	}
	if err := d.prepareDNS(cfg, p); err != nil {
		d.logf("dns setup: %v", err)
	}

	d.mu.Lock()
	d.activeName = name
	d.mu.Unlock()

	d.saveState()

	// Правила пересобираются под профиль до старта: у профиля может быть свой список сетей.
	if err := d.ApplyProtection(); err != nil {
		return fmt.Errorf("apply protection: %w", err)
	}
	return d.runner.Start(cfg, name, p)
}

func (d *Daemon) prepareDNS(cfg config.Config, p config.Profile) error {
	for _, cmdline := range p.PreKillRemote {
		out, err := d.ops.RunRemote(shuttle.SSHCommand(cfg.Sshuttle, p), p.Remote, cmdline)
		if err != nil {
			d.logf("remote command %q on %s: %v: %s", cmdline, p.Remote, err, strings.TrimSpace(string(out)))
		}
	}
	if cfg.DNS.UpdateScript != "" {
		// The resolvers are recorded before the script runs, not after: once it
		// has pointed the system at 127.0.0.1 there is nothing left to read.
		d.backupDNS()
		out, err := d.ops.UpdateDNS(cfg.DNS.UpdateScript)
		d.logf("update_dns: %s", strings.TrimSpace(string(out)))
		if err != nil {
			return fmt.Errorf("%s: %w", cfg.DNS.UpdateScript, err)
		}
	}
	if cfg.DNS.FlushCache {
		if err := d.ops.FlushDNSCache(); err != nil {
			d.logf("dns cache flush: %v", err)
		}
	}
	return nil
}

// Down опускает туннель. Правила блокировки остаются загруженными.
func (d *Daemon) Down(ctx context.Context) error {
	if err := d.runner.Stop(ctx); err != nil {
		return err
	}
	if err := d.runner.KillForeign(); err != nil {
		d.logf("%v", err)
	}
	d.onTunnelLost()
	return nil
}

// SetStrict включает или выключает безусловную блокировку.
func (d *Daemon) SetStrict(on bool) error {
	d.mu.Lock()
	previous := d.strictMode
	d.strictMode = on
	d.mu.Unlock()

	// Если применить не удалось, состояние откатывается: иначе статус
	// показывал бы режим, которого в pf нет, и человек считал бы себя
	// защищённым, не будучи защищённым.
	if err := d.ApplyProtection(); err != nil {
		d.mu.Lock()
		d.strictMode = previous
		d.mu.Unlock()
		return err
	}
	if on {
		// Strict обещает перекрыть защищаемые сети немедленно — а значит и уже
		// открытые соединения тоже. Настройка kill_states тут не спрашивается
		// намеренно: она про штатную потерю туннеля, а panic жмут тогда,
		// когда трафик нужно оборвать прямо сейчас, ценой живого туннеля.
		d.flushBlockedStates()
	}
	d.saveState()
	return nil
}

// SetMode переключает режим блокировки на лету.
// Выбор запоминается и переживает перезапуск демона.
func (d *Daemon) SetMode(mode config.ProtectionMode) error {
	switch mode {
	case config.ModeAll, config.ModePublic, config.ModeCustom, config.ModeOff:
	default:
		return fmt.Errorf("policy must be all|public|custom|off, got %q", mode)
	}

	d.mu.Lock()
	if mode == config.ModeCustom && len(d.cfg.Protection.Block) == 0 {
		d.mu.Unlock()
		return fmt.Errorf("policy custom requires a non-empty protection.block in the config")
	}
	previous := d.cfg.Protection.Mode
	d.cfg.Protection.Mode = mode
	d.mu.Unlock()

	if err := d.ApplyProtection(); err != nil {
		d.mu.Lock()
		d.cfg.Protection.Mode = previous
		d.mu.Unlock()
		return err
	}
	d.saveState()
	return nil
}

// WriteConfig проверяет и атомарно записывает новый конфиг, затем перечитывает его.
// Кривой конфиг не должен оставить демона без рабочего файла, поэтому запись
// идёт через временный файл, а разбор проверяется до подмены.
func (d *Daemon) WriteConfig(raw []byte) error {
	tmp := d.configPath + ".new"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return fmt.Errorf("write the temporary config: %w", err)
	}
	defer os.Remove(tmp)

	if _, err := config.Load(tmp); err != nil {
		return fmt.Errorf("config rejected: %w", err)
	}
	if err := os.Rename(tmp, d.configPath); err != nil {
		return fmt.Errorf("replace the config: %w", err)
	}
	d.logf("config rewritten via the api")
	return d.Reload()
}

// SetEnabled включает или выключает Ð·Ð°ÑÐ¸ÑÐ° целиком.
func (d *Daemon) SetEnabled(on bool) error {
	d.mu.Lock()
	previous := d.cfg.Protection.Enabled
	d.cfg.Protection.Enabled = on
	d.mu.Unlock()

	if err := d.ApplyProtection(); err != nil {
		d.mu.Lock()
		d.cfg.Protection.Enabled = previous
		d.mu.Unlock()
		return err
	}
	// Включение защиты — тоже ужесточение, и старые состояния переживают его
	// так же, как переживают падение туннеля. При живом туннеле состояния не
	// трогаем: блокировка его всё равно не касается, а сброс оборвал бы
	// работающие через него соединения.
	if on && !previous {
		if anchors, err := d.liveSshuttleAnchors(); err == nil && len(anchors) == 0 {
			d.onTunnelLost()
		}
	}
	d.saveState()
	return nil
}

// Reload перечитывает конфиг с диска и переприменяет правила.
func (d *Daemon) Reload() error {
	cfg, err := config.Load(d.configPath)
	if err != nil {
		return err
	}
	d.mu.Lock()
	d.cfg = cfg
	if _, ok := cfg.Profiles[d.activeName]; !ok {
		d.activeName = cfg.DefaultProfile
	}
	d.mu.Unlock()
	d.logf("config re-read from %s", d.configPath)
	return d.ApplyProtection()
}

// Config отдаёт текущий конфиг.
func (d *Daemon) Config() config.Config {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.cfg
}

// Status собирает снимок состояния.
func (d *Daemon) Status() Status {
	snap := d.runner.Snapshot()

	d.mu.RLock()
	cfg, name, strictMode := d.cfg, d.activeName, d.strictMode
	warnings := append([]string{}, d.warnings...)
	if d.fileWarning != "" {
		warnings = append(warnings, d.fileWarning)
	}
	d.mu.RUnlock()

	profile := snap.Profile
	if profile == "" {
		profile = name
	}
	st := Status{
		Tunnel:     string(snap.State),
		Profile:    profile,
		PID:        snap.PID,
		Since:      snap.Since,
		LastError:  snap.LastError,
		ConfigPath: d.configPath,
		Warnings:   warnings,
		Mode:       string(cfg.Protection.Mode),
		Version:    Version,
		StartedAt:  d.startedAt,
		LogFile:    cfg.Daemon.LogFile,
		Update:     d.UpdateState(),
	}
	// Требование войти по ссылке приходит из вывода sshuttle и живёт в снимке
	// туннеля. В статус оно попадает только когда действительно есть: иначе
	// приложению пришлось бы отличать «пусто» от «ничего не нужно».
	if !snap.Action.Empty() {
		action := snap.Action
		st.ActionRequired = &action
	}

	st.PFEnabled, _ = d.pf.Enabled()
	st.AnchorLoaded, st.AnchorLinked, _ = protect.Installed(d.pf)
	st.SshuttleAnchs, _ = d.liveSshuttleAnchors()
	// Туннель могли поднять мимо splitr — старым скриптом или руками.
	// Молча показывать «down» при живых якорях sshuttle нельзя: человек решит,
	// что защита работает, хотя трафик как раз идёт в туннель.
	if len(st.SshuttleAnchs) > 0 && snap.PID == 0 {
		if running, err := d.shuttleRunning(cfg.Sshuttle.Path); err == nil && running {
			st.Tunnel = "external"
			st.External = true
		} else {
			// Якоря есть, а процесса нет — это мусор, сторож уберёт его
			// ближайшим тиком. Показывать «туннель поднят» нельзя.
			st.Tunnel = "stale-anchors"
		}
	}

	if rs, err := d.ruleset(); err == nil {
		st.BlockedNets = rs.Block
		st.AllowedNets = rs.Pass
	}

	switch {
	case strictMode:
		st.Protection = "strict"
	case !cfg.Protection.Enabled:
		st.Protection = "off"
	default:
		st.Protection = string(cfg.Protection.Mode)
	}
	// Блокировка реально действует, когда правила загружены, якорь подключён
	// и туннеля (который их перебивает) нет.
	st.Blocking = st.AnchorLoaded && st.AnchorLinked && len(st.SshuttleAnchs) == 0 && st.Protection != "off"
	return st
}

// MarshalStatus сериализует статус для API и CLI.
func MarshalStatus(s Status) ([]byte, error) { return json.MarshalIndent(s, "", "  ") }
