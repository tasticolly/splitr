package daemon

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tasticolly/splitr/internal/config"
	"github.com/tasticolly/splitr/internal/netmon"
	"github.com/tasticolly/splitr/internal/pfctl/pftest"
	"github.com/tasticolly/splitr/internal/protect"
	"github.com/tasticolly/splitr/internal/shuttle"
)

// requireNonRoot страхует от порчи системы: ApplyProtection пишет в
// protect.AnchorFile по пути, зашитому в прод-коде константой. Под обычным
// пользователем запись просто не удаётся (это тестам и нужно), а под root
// тест перезаписал бы настоящий /etc/pf.anchors/splitr.
func requireNonRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skipf("под root тест перезаписал бы системный файл %s", protect.AnchorFile)
	}
}

// --- тестовые двойники ------------------------------------------------------

// fakeTunnel заменяет *shuttle.Runner: ничего не запускает, только считает вызовы.
type fakeTunnel struct {
	mu sync.Mutex

	snap    shuttle.Snapshot
	running bool

	started     []string // имена профилей, с которыми звали Start
	stops       int
	marks       int
	killForeign int

	startErr, stopErr, killErr error
}

func newFakeTunnel() *fakeTunnel {
	return &fakeTunnel{snap: shuttle.Snapshot{State: shuttle.StateDown}}
}

func (f *fakeTunnel) Start(cfg config.Config, name string, p config.Profile) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = append(f.started, name)
	if f.startErr != nil {
		return f.startErr
	}
	f.running = true
	f.snap = shuttle.Snapshot{State: shuttle.StateStarting, Profile: name, PID: 4242, Since: time.Now()}
	return nil
}

func (f *fakeTunnel) Stop(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stops++
	if f.stopErr != nil {
		return f.stopErr
	}
	f.running = false
	f.snap = shuttle.Snapshot{State: shuttle.StateDown, Profile: f.snap.Profile}
	return nil
}

func (f *fakeTunnel) Snapshot() shuttle.Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snap
}

func (f *fakeTunnel) SetAction(a shuttle.ActionRequired) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snap.Action = a
}

func (f *fakeTunnel) ClearAction() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snap.Action = shuttle.ActionRequired{}
}

func (f *fakeTunnel) MarkUp() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.marks++
	if f.snap.State == shuttle.StateStarting {
		f.snap.State = shuttle.StateUp
	}
}

func (f *fakeTunnel) Running() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.running
}

func (f *fakeTunnel) KillForeign() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.killForeign++
	return f.killErr
}

func (f *fakeTunnel) setSnapshot(s shuttle.Snapshot) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snap = s
}

func (f *fakeTunnel) counters() (starts []string, stops, marks, kills int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.started...), f.stops, f.marks, f.killForeign
}

// fakeOps заменяет системные побочные действия: ssh на удалённый хост и DNS.
type fakeOps struct {
	mu sync.Mutex

	remote  []string // "хост: команда"
	scripts []string
	flushes int

	remoteErr, scriptErr, flushErr error

	// Проверка достижимости хоста перед подъёмом туннеля.
	reachChecks int
	reachArgs   []string
	reachOut    []byte
	reachErr    error
}

func (o *fakeOps) RunRemote(sshArgs []string, remote, command string) ([]byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.remote = append(o.remote, remote+": "+command)
	return nil, o.remoteErr
}

// Reachable по умолчанию отвечает «хост доступен»: тесты, которым это
// неважно, не должны спотыкаться о проверку перед подъёмом туннеля.
func (o *fakeOps) Reachable(sshArgs []string, remote string) ([]byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.reachChecks++
	o.reachArgs = sshArgs
	return o.reachOut, o.reachErr
}

func (o *fakeOps) UpdateDNS(script string) ([]byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.scripts = append(o.scripts, script)
	return []byte("готово"), o.scriptErr
}

func (o *fakeOps) FlushDNSCache() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.flushes++
	return o.flushErr
}

func (o *fakeOps) snapshot() (remote, scripts []string, flushes int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.remote...), append([]string(nil), o.scripts...), o.flushes
}

// syncBuffer — журнал демона, безопасный при параллельной записи.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// testConfig — конфиг, на котором работает большинство тестов демона.
func testConfig() config.Config {
	cfg := config.Default()
	cfg.Subnets = []string{"10.0.0.0/9", "203.0.113.0/24"}
	cfg.Excludes = []string{"192.168.1.0/24"}
	cfg.Protection.Allow = []string{"192.168.1.0/24"}
	// Профиль по умолчанию берёт глобальный список сетей, второй —
	// переопределяет его своим: так проверяется, что правила пересобираются
	// под конкретный профиль.
	cfg.DefaultProfile = "alpha"
	cfg.Profiles = map[string]config.Profile{
		"alpha": {Remote: "user@alpha"},
		"beta":  {Remote: "user@beta", Subnets: []string{"172.16.0.0/12"}},
	}
	cfg.DNS = config.DNS{}
	cfg.Daemon.HTTPAddr = ""
	cfg.Daemon.SocketPath = ""
	// Пустой путь отключает файл состояния: тесты не должны писать
	// в /usr/local/var/run. Там, где состояние проверяется, путь задаётся явно.
	cfg.Daemon.StateFile = ""
	return cfg
}

// newTestDaemon собирает демон на фейковых зависимостях.
func newTestDaemon(t *testing.T, cfg config.Config) (*Daemon, *pftest.Fake, *fakeTunnel, *fakeOps) {
	t.Helper()
	pf := pftest.New()
	tun := newFakeTunnel()
	ops := &fakeOps{}
	d := NewWithDeps(cfg, filepath.Join(t.TempDir(), "config.yaml"), io.Discard, pf, tun, ops)
	// Файл якоря уводится во временный каталог: боевой путь /etc/pf.anchors
	// недоступен на запись, и без подмены тесты проверяли бы только отказ.
	d.anchorFile = filepath.Join(t.TempDir(), "splitr.anchor")
	// По умолчанию считаем, что процесс sshuttle в системе есть: тесты,
	// изображающие живой туннель, задают только якоря pf. Проверки
	// осиротевших якорей подменяют этот шов явно.
	d.shuttleRunning = func(string) (bool, error) { return true, nil }
	return d, pf, tun, ops
}

// installed приводит pf в состояние «Ð·Ð°ÑÐ¸ÑÐ° установлен и работает».
func installed(t *testing.T, d *Daemon, pf *pftest.Fake) {
	t.Helper()
	rs, err := d.ruleset()
	if err != nil {
		t.Fatalf("собрать правила: %v", err)
	}
	if err := rs.Apply(pf); err != nil {
		t.Fatalf("загрузить правила: %v", err)
	}
	pf.LinkAnchor(protect.Anchor)
}

// --- Status -----------------------------------------------------------------

func TestStatusReportsProtectionMode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		mode       config.ProtectionMode
		enabled    bool
		strictMode bool
		want       string
	}{
		{name: "включён в режиме all", mode: config.ModeAll, enabled: true, want: "all"},
		{name: "включён в режиме public", mode: config.ModePublic, enabled: true, want: "public"},
		{name: "выключен", mode: config.ModeAll, want: "off"},
		{name: "strict перекрывает политику", mode: config.ModeAll, enabled: true, strictMode: true, want: "strict"},
		{name: "strict перекрывает даже выключенную защиту", mode: config.ModeAll, strictMode: true, want: "strict"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := testConfig()
			cfg.Protection.Mode = tt.mode
			cfg.Protection.Enabled = tt.enabled
			d, pf, _, _ := newTestDaemon(t, cfg)
			d.strictMode = tt.strictMode
			installed(t, d, pf)

			if got := d.Status().Protection; got != tt.want {
				t.Fatalf("Protection = %q, ожидалось %q", got, tt.want)
			}
		})
	}
}

// Blocking означает «трафик реально режется прямо сейчас».
func TestStatusBlocking(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		enabled  bool
		loaded   bool
		linked   bool
		tunnelUp bool
		want     bool
	}{
		{name: "правила загружены, якорь подключён, туннеля нет", enabled: true, loaded: true, linked: true, want: true},
		{name: "живой туннель перебивает блокировку", enabled: true, loaded: true, linked: true, tunnelUp: true},
		{name: "якорь не подключён к pf.conf", enabled: true, loaded: true},
		{name: "правила смыло", enabled: true, linked: true},
		{name: "Ð·Ð°ÑÐ¸ÑÐ° выключен", loaded: true, linked: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := testConfig()
			cfg.Protection.Enabled = tt.enabled
			d, pf, _, _ := newTestDaemon(t, cfg)
			if tt.loaded {
				rs := protect.Build(cfg, cfg.Profiles["alpha"], false)
				if err := rs.Apply(pf); err != nil {
					t.Fatalf("Apply: %v", err)
				}
			}
			if tt.linked {
				pf.LinkAnchor(protect.Anchor)
			}
			if tt.tunnelUp {
				pf.SetSshuttleAnchors("sshuttle-12300")
			}

			st := d.Status()
			if st.Blocking != tt.want {
				t.Fatalf("Blocking = %v, ожидалось %v (loaded=%v linked=%v anchors=%v ks=%q)",
					st.Blocking, tt.want, st.AnchorLoaded, st.AnchorLinked, st.SshuttleAnchs, st.Protection)
			}
			if st.AnchorLoaded != tt.loaded || st.AnchorLinked != tt.linked {
				t.Fatalf("AnchorLoaded=%v AnchorLinked=%v, ожидалось %v/%v", st.AnchorLoaded, st.AnchorLinked, tt.loaded, tt.linked)
			}
		})
	}
}

func TestStatusReflectsTunnelSnapshot(t *testing.T) {
	t.Parallel()

	d, pf, tun, _ := newTestDaemon(t, testConfig())
	installed(t, d, pf)
	pf.SetSshuttleAnchors("sshuttle-12300")
	since := time.Now().Add(-time.Minute)
	tun.setSnapshot(shuttle.Snapshot{
		State: shuttle.StateUp, Profile: "beta", PID: 777, Since: since, LastError: "было дело",
	})

	st := d.Status()
	if st.Tunnel != "up" || st.Profile != "beta" || st.PID != 777 || !st.Since.Equal(since) || st.LastError != "было дело" {
		t.Fatalf("статус собран неверно: %+v", st)
	}
	if strings.Join(st.SshuttleAnchs, ",") != "sshuttle-12300" {
		t.Fatalf("якоря sshuttle = %v", st.SshuttleAnchs)
	}
	if !st.PFEnabled {
		t.Fatal("PFEnabled должен быть true")
	}
}

// Пока туннель не поднимали, показывается выбранный профиль, а не пустая строка.
func TestStatusFallsBackToActiveProfile(t *testing.T) {
	t.Parallel()

	d, _, _, _ := newTestDaemon(t, testConfig())
	if got := d.Status().Profile; got != "alpha" {
		t.Fatalf("Profile = %q, ожидался профиль по умолчанию visa", got)
	}
}

func TestStatusListsBlockedAndAllowedNets(t *testing.T) {
	t.Parallel()

	d, _, _, _ := newTestDaemon(t, testConfig())
	st := d.Status()
	if strings.Join(st.BlockedNets, ",") != "10.0.0.0/9,203.0.113.0/24" {
		t.Fatalf("BlockedNets = %v", st.BlockedNets)
	}
	if strings.Join(st.AllowedNets, ",") != "192.168.1.0/24" {
		t.Fatalf("AllowedNets = %v", st.AllowedNets)
	}
}

// Ошибки pf не должны ронять статус — он обязан отдаваться всегда.
func TestStatusSurvivesPFErrors(t *testing.T) {
	t.Parallel()

	d, pf, _, _ := newTestDaemon(t, testConfig())
	boom := errors.New("pf недоступен")
	for _, m := range []string{
		pftest.MethodEnabled, pftest.MethodAnchorRules,
		pftest.MethodMainRules, pftest.MethodSshuttleAnchors,
	} {
		pf.Fail(m, boom)
	}

	st := d.Status()
	if st.PFEnabled || st.AnchorLoaded || st.AnchorLinked || st.Blocking {
		t.Fatalf("при недоступном pf ничего не должно считаться работающим: %+v", st)
	}
	if st.ConfigPath == "" {
		t.Fatal("путь к конфигу должен присутствовать всегда")
	}
}

// Если активный профиль исчез из конфига, список сетей просто пуст.
func TestStatusWithUnknownActiveProfile(t *testing.T) {
	t.Parallel()

	d, _, _, _ := newTestDaemon(t, testConfig())
	d.activeName = "нет-такого"
	st := d.Status()
	if len(st.BlockedNets) != 0 {
		t.Fatalf("BlockedNets = %v, ожидался пустой список", st.BlockedNets)
	}
}

func TestStatusIncludesWatchdogWarnings(t *testing.T) {
	t.Parallel()

	d, pf, _, _ := newTestDaemon(t, testConfig())
	pf.Fail(pftest.MethodSshuttleAnchors, errors.New("pfctl не отвечает"))
	d.tick(false)

	warnings := d.Status().Warnings
	if len(warnings) == 0 || !strings.Contains(strings.Join(warnings, " "), "pfctl не отвечает") {
		t.Fatalf("предупреждения = %v, ожидалась жалоба на pfctl", warnings)
	}
}

func TestMarshalStatusProducesJSON(t *testing.T) {
	t.Parallel()

	d, pf, _, _ := newTestDaemon(t, testConfig())
	installed(t, d, pf)
	body, err := MarshalStatus(d.Status())
	if err != nil {
		t.Fatalf("MarshalStatus: %v", err)
	}
	for _, want := range []string{`"protection": "all"`, `"blocking": true`, `"blocked_nets"`} {
		if !strings.Contains(string(body), want) {
			t.Errorf("в JSON нет %q:\n%s", want, body)
		}
	}
}

// --- ruleset ----------------------------------------------------------------

func TestRulesetIsEmptyWhenProtectionDisabled(t *testing.T) {
	t.Parallel()

	cfg := testConfig()
	cfg.Protection.Enabled = false
	d, _, _, _ := newTestDaemon(t, cfg)

	rs, err := d.ruleset()
	if err != nil {
		t.Fatalf("ruleset: %v", err)
	}
	if !rs.Empty() {
		t.Fatalf("выключенный Ð·Ð°ÑÐ¸ÑÐ° не должен ничего резать: %+v", rs)
	}
}

// Strict поднимает блокировку даже при выключенном Ð·Ð°ÑÐ¸ÑÐ°.
func TestRulesetStrictOverridesDisabledProtection(t *testing.T) {
	t.Parallel()

	cfg := testConfig()
	cfg.Protection.Enabled = false
	d, _, _, _ := newTestDaemon(t, cfg)
	d.strictMode = true

	rs, err := d.ruleset()
	if err != nil {
		t.Fatalf("ruleset: %v", err)
	}
	if rs.Empty() || !rs.Strict {
		t.Fatalf("в panic-режиме ожидалась безусловная блокировка: %+v", rs)
	}
}

// Правила считаются под активный профиль: у профиля может быть свой список сетей.
func TestRulesetUsesActiveProfileSubnets(t *testing.T) {
	t.Parallel()

	d, _, _, _ := newTestDaemon(t, testConfig())
	d.activeName = "beta"

	rs, err := d.ruleset()
	if err != nil {
		t.Fatalf("ruleset: %v", err)
	}
	if strings.Join(rs.Block, ",") != "172.16.0.0/12" {
		t.Fatalf("Block = %v, ожидались сети профиля pc", rs.Block)
	}
}

func TestRulesetFailsOnUnknownProfile(t *testing.T) {
	t.Parallel()

	d, _, _, _ := newTestDaemon(t, testConfig())
	d.activeName = "нет-такого"
	if _, err := d.ruleset(); err == nil {
		t.Fatal("ожидалась ошибка про неизвестный профиль")
	}
}

// --- сторож (tick) ----------------------------------------------------------

// Якорь sshuttle в pf — единственный признак живого туннеля.
func TestTickDetectsTunnelByAnchor(t *testing.T) {
	t.Parallel()

	d, pf, tun, _ := newTestDaemon(t, testConfig())
	installed(t, d, pf)
	pf.SetSshuttleAnchors("sshuttle-12300")

	if up := d.tick(false); !up {
		t.Fatal("при живом якоре sshuttle туннель должен считаться поднятым")
	}
	if _, _, marks, _ := tun.counters(); marks == 0 {
		t.Fatal("сторож обязан подтвердить подъём туннеля через MarkUp")
	}
}

func TestTickWithoutAnchorsReportsTunnelDown(t *testing.T) {
	t.Parallel()

	d, pf, tun, _ := newTestDaemon(t, testConfig())
	installed(t, d, pf)

	if up := d.tick(false); up {
		t.Fatal("без якорей sshuttle туннель поднятым не считается")
	}
	if _, _, marks, _ := tun.counters(); marks != 0 {
		t.Fatal("MarkUp не должен вызываться при опущенном туннеле")
	}
}

// Потеря туннеля обязана сбросить живые состояния — иначе открытые
// соединения переживут падение и продолжат светить трафик.
func TestTickKillsStatesWhenTunnelLost(t *testing.T) {
	t.Parallel()

	d, pf, _, _ := newTestDaemon(t, testConfig())
	installed(t, d, pf)

	if up := d.tick(true); up {
		t.Fatal("туннель пропал, ожидалось false")
	}
	if strings.Join(pf.KilledStates(), ",") != "10.0.0.0/9,203.0.113.0/24" {
		t.Fatalf("сброшены состояния %v, ожидались все блокируемые сети", pf.KilledStates())
	}
}

func TestTickRespectsKillStatesOption(t *testing.T) {
	t.Parallel()

	cfg := testConfig()
	cfg.Protection.KillStates = false
	d, pf, _, _ := newTestDaemon(t, cfg)
	installed(t, d, pf)

	d.tick(true)
	if len(pf.KilledStates()) != 0 {
		t.Fatalf("при kill_states=false состояния трогать нельзя: %v", pf.KilledStates())
	}
}

func TestTickDoesNotKillStatesWhenTunnelStaysDown(t *testing.T) {
	t.Parallel()

	d, pf, _, _ := newTestDaemon(t, testConfig())
	installed(t, d, pf)

	d.tick(false)
	if len(pf.KilledStates()) != 0 {
		t.Fatalf("состояния сбрасываются только в момент потери туннеля: %v", pf.KilledStates())
	}
}

// Чужой `pfctl -F all` смывает правила якоря — сторож обязан их вернуть.
func TestTickRestoresFlushedAnchorRules(t *testing.T) {
	t.Parallel()

	d, pf, _, _ := newTestDaemon(t, testConfig())
	installed(t, d, pf)
	pf.FlushAll()

	d.tick(false)

	rules := pf.AnchorText(protect.Anchor)
	if !strings.Contains(rules, "block drop out") {
		t.Fatalf("правила якоря не восстановлены:\n%s", rules)
	}
	if len(pf.Reloads()) != 0 {
		t.Fatalf("перезагружать pf.conf было незачем: %v", pf.Reloads())
	}
}

// Уже загруженные правила переписывать не надо: лишний LoadAnchor — лишний риск.
func TestTickLeavesLoadedAnchorAlone(t *testing.T) {
	t.Parallel()

	d, pf, _, _ := newTestDaemon(t, testConfig())
	installed(t, d, pf)
	before := pf.CallCount(pftest.MethodLoadAnchor)

	d.tick(false)

	if got := pf.CallCount(pftest.MethodLoadAnchor); got != before {
		t.Fatalf("якорь перезагружен без нужды: было %d вызовов, стало %d", before, got)
	}
}

// Перезагрузка pf.conf снесла бы якоря sshuttle, поэтому при живом туннеле
// сторож только предупреждает.
func TestTickRefusesToReloadPfConfWhileTunnelIsUp(t *testing.T) {
	t.Parallel()

	d, pf, _, _ := newTestDaemon(t, testConfig())
	installed(t, d, pf)
	pf.UnlinkAnchor(protect.Anchor)
	pf.SetSshuttleAnchors("sshuttle-12300")

	d.tick(true)

	if len(pf.Reloads()) != 0 {
		t.Fatalf("pf.conf перезагружен при живом туннеле — туннель бы порвался: %v", pf.Reloads())
	}
	warnings := strings.Join(d.Status().Warnings, " ")
	if !strings.Contains(warnings, "repair deferred") {
		t.Fatalf("предупреждения = %q, ожидалось сообщение об отложенной починке", warnings)
	}
}

// Когда туннеля нет, отвалившийся якорь чинится перезагрузкой pf.conf.
func TestTickReloadsPfConfWhenAnchorUnlinkedAndTunnelDown(t *testing.T) {
	t.Parallel()

	d, pf, _, _ := newTestDaemon(t, testConfig())
	installed(t, d, pf)
	pf.UnlinkAnchor(protect.Anchor)

	d.tick(false)

	if strings.Join(pf.Reloads(), ",") != protect.PfConf {
		t.Fatalf("перезагрузки = %v, ожидалась одна перезагрузка %s", pf.Reloads(), protect.PfConf)
	}
	if !strings.Contains(pf.AnchorText(protect.Anchor), "block drop out") {
		t.Fatal("после перезагрузки pf.conf правила должны быть загружены заново")
	}
	if len(d.Status().Warnings) != 0 {
		t.Fatalf("починка удалась, предупреждений быть не должно: %v", d.Status().Warnings)
	}
}

func TestTickReportsReloadFailure(t *testing.T) {
	t.Parallel()

	d, pf, _, _ := newTestDaemon(t, testConfig())
	installed(t, d, pf)
	pf.UnlinkAnchor(protect.Anchor)
	pf.Fail(pftest.MethodReloadMain, errors.New("pf.conf не разобрать"))

	d.tick(false)

	if !strings.Contains(strings.Join(d.Status().Warnings, " "), "pf.conf не разобрать") {
		t.Fatalf("предупреждения = %v, ожидалась ошибка перезагрузки", d.Status().Warnings)
	}
}

// При выключенном Ð·Ð°ÑÐ¸ÑÐ° чинить в якоре нечего.
func TestTickSkipsAnchorRepairWhenNothingToBlock(t *testing.T) {
	t.Parallel()

	cfg := testConfig()
	cfg.Protection.Enabled = false
	d, pf, _, _ := newTestDaemon(t, cfg)

	d.tick(false)

	if n := pf.CallCount(pftest.MethodLoadAnchor); n != 0 {
		t.Fatalf("LoadAnchor вызван %d раз, ожидалось ноль", n)
	}
	if len(pf.Reloads()) != 0 {
		t.Fatalf("перезагрузок быть не должно: %v", pf.Reloads())
	}
}

// Выключенный кем-то pf сторож включает обратно.
func TestTickReenablesPF(t *testing.T) {
	requireNonRoot(t)
	t.Parallel()

	d, pf, _, _ := newTestDaemon(t, testConfig())
	installed(t, d, pf)
	pf.SetEnabled(false)

	d.tick(false)

	if pf.CallCount(pftest.MethodEnable) != 1 {
		t.Fatalf("pf должен быть включён заново, вызовы: %v", pf.Calls())
	}
	if on, _ := pf.Enabled(); !on {
		t.Fatal("после починки pf обязан быть включён")
	}
	if warnings := d.Status().Warnings; len(warnings) != 0 {
		t.Fatalf("починка прошла успешно, предупреждений быть не должно: %v", warnings)
	}
}

// Недоступный файл якоря — не повод оставлять pf со старыми правилами:
// правила обязаны примениться, а о непереживающей перезагрузку записи
// человек должен узнать из предупреждений.
func TestApplyProtectionWarnsButAppliesWhenAnchorFileUnwritable(t *testing.T) {
	requireNonRoot(t)
	t.Parallel()

	d, pf, _, _ := newTestDaemon(t, testConfig())
	// Каталог вместо файла: запись гарантированно провалится, а создание
	// родительского каталога — нет, то есть проверяется именно запись.
	d.anchorFile = t.TempDir()

	if err := d.ApplyProtection(); err != nil {
		t.Fatalf("применение правил не должно падать из-за файла: %v", err)
	}
	if rules, _ := pf.AnchorRules(protect.Anchor); !strings.Contains(rules, "block drop out") {
		t.Fatalf("правила обязаны быть в pf, получено %q", rules)
	}
	if warnings := d.Status().Warnings; len(warnings) == 0 ||
		!strings.Contains(strings.Join(warnings, " "), "will not survive a reboot") {
		t.Fatalf("предупреждения = %v, ожидалась жалоба на запись файла якоря", warnings)
	}
}

func TestTickReportsPFEnableFailure(t *testing.T) {
	t.Parallel()

	d, pf, _, _ := newTestDaemon(t, testConfig())
	installed(t, d, pf)
	pf.SetEnabled(false)
	pf.Fail(pftest.MethodEnable, errors.New("нет прав"))

	d.tick(false)

	if !strings.Contains(strings.Join(d.Status().Warnings, " "), "enable pf") {
		t.Fatalf("предупреждения = %v, ожидалась ошибка включения pf", d.Status().Warnings)
	}
}

func TestTickReportsAnchorReadFailure(t *testing.T) {
	t.Parallel()

	d, pf, _, _ := newTestDaemon(t, testConfig())
	pf.Fail(pftest.MethodAnchorRules, errors.New("якорь не читается"))

	d.tick(false)

	if !strings.Contains(strings.Join(d.Status().Warnings, " "), "якорь не читается") {
		t.Fatalf("предупреждения = %v", d.Status().Warnings)
	}
}

// Предупреждения не копятся: каждый тик собирает их заново.
func TestTickClearsStaleWarnings(t *testing.T) {
	t.Parallel()

	d, pf, _, _ := newTestDaemon(t, testConfig())
	installed(t, d, pf)
	pf.Fail(pftest.MethodSshuttleAnchors, errors.New("временный сбой"))
	d.tick(false)
	if len(d.Status().Warnings) == 0 {
		t.Fatal("ожидалось предупреждение")
	}

	pf.Fail(pftest.MethodSshuttleAnchors, nil)
	d.tick(false)
	if got := d.Status().Warnings; len(got) != 0 {
		t.Fatalf("устаревшие предупреждения не сброшены: %v", got)
	}
}

// При autoreconnect сторож сам пытается поднять туннель.
func TestTickAutoreconnectAttemptsUp(t *testing.T) {
	requireNonRoot(t)
	t.Parallel()

	cfg := testConfig()
	cfg.Daemon.Autoreconnect = true
	d, pf, tun, _ := newTestDaemon(t, cfg)
	installed(t, d, pf)

	d.tick(false)

	if _, _, _, kills := tun.counters(); kills == 0 {
		t.Fatal("автопереподключение должно было начать подъём туннеля")
	}
}

func TestTickWithoutAutoreconnectDoesNotStartTunnel(t *testing.T) {
	t.Parallel()

	d, pf, tun, _ := newTestDaemon(t, testConfig())
	installed(t, d, pf)

	d.tick(false)

	if starts, _, _, kills := tun.counters(); len(starts) != 0 || kills != 0 {
		t.Fatalf("без autoreconnect туннель трогать нельзя: старты %v, KillForeign %d", starts, kills)
	}
}

// --- Up / Down / Reload / переключатели -------------------------------------

func TestUpRejectsUnknownProfile(t *testing.T) {
	t.Parallel()

	d, _, tun, _ := newTestDaemon(t, testConfig())
	if err := d.Up("нет-такого"); err == nil {
		t.Fatal("ожидалась ошибка про неизвестный профиль")
	}
	if starts, _, _, kills := tun.counters(); len(starts) != 0 || kills != 0 {
		t.Fatalf("при неизвестном профиле трогать систему нельзя: старты %v, KillForeign %d", starts, kills)
	}
}

// Перед подъёмом туннеля выполняются удалённые команды профиля и работа с DNS.
func TestUpRunsPreparationSteps(t *testing.T) {
	requireNonRoot(t)
	t.Parallel()

	cfg := testConfig()
	cfg.DNS = config.DNS{UpdateScript: "/usr/local/bin/update_dns.sh", FlushCache: true}
	cfg.Profiles["beta"] = config.Profile{Remote: "user@beta", PreKillRemote: []string{"pkill -f my-agent.py"}}
	d, _, tun, ops := newTestDaemon(t, cfg)

	// ApplyProtection упрётся в системный файл якоря — до Start дело не дойдёт,
	// но подготовительные шаги обязаны отработать.
	_ = d.Up("beta")

	remote, scripts, flushes := ops.snapshot()
	if strings.Join(remote, ";") != "user@beta: pkill -f my-agent.py" {
		t.Fatalf("удалённые команды = %v", remote)
	}
	if strings.Join(scripts, ";") != "/usr/local/bin/update_dns.sh" {
		t.Fatalf("вызовы update_dns = %v", scripts)
	}
	if flushes != 1 {
		t.Fatalf("кэш DNS сброшен %d раз, ожидался один", flushes)
	}
	if _, _, _, kills := tun.counters(); kills != 1 {
		t.Fatalf("чужие sshuttle должны гаситься ровно один раз, получено %d", kills)
	}
	if got := d.Status().Profile; got != "beta" {
		t.Fatalf("активный профиль = %q, ожидался pc", got)
	}
}

// Down гасит туннель, но блокировку оставляет — в этом весь смысл Ð·Ð°ÑÐ¸ÑÐ°.
func TestDownStopsTunnelAndKeepsRules(t *testing.T) {
	t.Parallel()

	d, pf, tun, _ := newTestDaemon(t, testConfig())
	installed(t, d, pf)
	rulesBefore := pf.AnchorText(protect.Anchor)

	if err := d.Down(context.Background()); err != nil {
		t.Fatalf("Down: %v", err)
	}

	_, stops, _, kills := tun.counters()
	if stops != 1 || kills != 1 {
		t.Fatalf("Stop вызван %d раз, KillForeign %d — ожидалось по одному", stops, kills)
	}
	if pf.AnchorText(protect.Anchor) != rulesBefore {
		t.Fatal("после Down правила блокировки обязаны остаться на месте")
	}
	if strings.Join(pf.KilledStates(), ",") != "10.0.0.0/9,203.0.113.0/24" {
		t.Fatalf("состояния = %v, ожидался сброс по всем блокируемым сетям", pf.KilledStates())
	}
}

func TestDownPropagatesStopError(t *testing.T) {
	t.Parallel()

	d, _, tun, _ := newTestDaemon(t, testConfig())
	tun.stopErr = errors.New("процесс не гаснет")

	if err := d.Down(context.Background()); err == nil {
		t.Fatal("ошибка остановки туннеля должна возвращаться наружу")
	}
	if _, _, _, kills := tun.counters(); kills != 0 {
		t.Fatal("после неудачной остановки чужие процессы гасить рано")
	}
}

func TestSetStrictSwitchesMode(t *testing.T) {
	requireNonRoot(t)
	t.Parallel()

	d, pf, _, _ := newTestDaemon(t, testConfig())
	installed(t, d, pf)

	// Ошибка записи системного файла якоря ожидаема, состояние всё равно меняется.
	_ = d.SetStrict(true)
	if got := d.Status().Protection; got != "strict" {
		t.Fatalf("Protection = %q, ожидалось panic", got)
	}
	rs, err := d.ruleset()
	if err != nil {
		t.Fatalf("ruleset: %v", err)
	}
	if !rs.Strict || !strings.Contains(string(rs.Rules()), "quick") {
		t.Fatal("в panic-режиме правила обязаны быть quick")
	}

	_ = d.SetStrict(false)
	if got := d.Status().Protection; got != "all" {
		t.Fatalf("Protection = %q, ожидался возврат в режим all", got)
	}
}

func TestSetEnabledTogglesProtection(t *testing.T) {
	requireNonRoot(t)
	t.Parallel()

	d, pf, _, _ := newTestDaemon(t, testConfig())
	installed(t, d, pf)

	_ = d.SetEnabled(false)
	if got := d.Status().Protection; got != "off" {
		t.Fatalf("Protection = %q, ожидалось off", got)
	}
	if rs, _ := d.ruleset(); !rs.Empty() {
		t.Fatalf("при выключенном Ð·Ð°ÑÐ¸ÑÐ° резать нечего: %+v", rs)
	}

	_ = d.SetEnabled(true)
	if got := d.Status().Protection; got != "all" {
		t.Fatalf("Protection = %q, ожидалось all", got)
	}
}

func TestReloadPicksUpNewConfig(t *testing.T) {
	requireNonRoot(t)
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
default_profile: beta
subnets:
  - 11.0.0.0/8
profiles:
  beta:
    remote: user@beta
`), 0o600); err != nil {
		t.Fatalf("записать конфиг: %v", err)
	}

	pf := pftest.New()
	d := NewWithDeps(testConfig(), path, io.Discard, pf, newFakeTunnel(), &fakeOps{})

	_ = d.Reload() // ошибка записи системного файла якоря ожидаема

	cfg := d.Config()
	if strings.Join(cfg.Subnets, ",") != "11.0.0.0/8" {
		t.Fatalf("subnets = %v, конфиг не перечитан", cfg.Subnets)
	}
	// Профиля visa в новом конфиге нет — активным обязан стать профиль по умолчанию.
	if got := d.Status().Profile; got != "beta" {
		t.Fatalf("активный профиль = %q, ожидался pc", got)
	}
}

func TestReloadKeepsOldConfigOnError(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("субнеты: [нет]\n"), 0o600); err != nil {
		t.Fatalf("записать конфиг: %v", err)
	}
	pf := pftest.New()
	d := NewWithDeps(testConfig(), path, io.Discard, pf, newFakeTunnel(), &fakeOps{})

	if err := d.Reload(); err == nil {
		t.Fatal("битый конфиг должен приводить к ошибке")
	}
	if strings.Join(d.Config().Subnets, ",") != "10.0.0.0/9,203.0.113.0/24" {
		t.Fatalf("после неудачного Reload конфиг обязан остаться прежним: %v", d.Config().Subnets)
	}
}

func TestReloadKeepsActiveProfileIfItStillExists(t *testing.T) {
	requireNonRoot(t)
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
default_profile: alpha
subnets:
  - 11.0.0.0/8
profiles:
  alpha:
    remote: user@alpha
  beta:
    remote: user@beta
`), 0o600); err != nil {
		t.Fatalf("записать конфиг: %v", err)
	}
	pf := pftest.New()
	d := NewWithDeps(testConfig(), path, io.Discard, pf, newFakeTunnel(), &fakeOps{})
	d.activeName = "beta"

	_ = d.Reload()

	if got := d.Status().Profile; got != "beta" {
		t.Fatalf("активный профиль = %q, ожидалось сохранение beta", got)
	}
}

// ApplyProtection пишет в protect.AnchorFile — путь зашит в прод-коде
// константой, подменить его тесту нечем. Проверяем хотя бы, что при отказе
// записи демон честно возвращает ошибку и не молчит.
func TestApplyProtectionReportsAnchorFileFailure(t *testing.T) {
	requireNonRoot(t)
	t.Parallel()

	d, pf, _, _ := newTestDaemon(t, testConfig())
	err := d.ApplyProtection()
	if err == nil {
		t.Skipf("файл %s оказался доступен на запись — проверка неприменима", protect.AnchorFile)
	}
	if !strings.Contains(err.Error(), protect.AnchorFile) {
		t.Fatalf("ошибка = %v, ожидалось упоминание %s", err, protect.AnchorFile)
	}
	if n := pf.CallCount(pftest.MethodLoadAnchor); n != 0 {
		t.Fatalf("при неудачной записи файла правила в pf грузить рано, вызовов %d", n)
	}
}

func TestLogfWritesToProvidedWriter(t *testing.T) {
	t.Parallel()

	log := &syncBuffer{}
	d := NewWithDeps(testConfig(), "конфиг.yaml", log, pftest.New(), newFakeTunnel(), &fakeOps{})
	d.logf("проверка %d", 42)

	if !strings.Contains(log.String(), "проверка 42") {
		t.Fatalf("журнал = %q", log.String())
	}
}

func TestConfigReturnsCurrentConfig(t *testing.T) {
	t.Parallel()

	cfg := testConfig()
	d, _, _, _ := newTestDaemon(t, cfg)
	if d.Config().DefaultProfile != cfg.DefaultProfile {
		t.Fatal("Config() должен отдавать текущий конфиг")
	}
}

// --- сверка загруженных правил ----------------------------------------------

// rulesDrifted — сторожевая проверка «в якоре лежит не то»: побайтового
// сравнения не выйдет, pfctl печатает правила в своей канонической форме.
func TestRulesDrifted(t *testing.T) {
	t.Parallel()

	normal := protect.Ruleset{Block: []string{"10.0.0.0/9"}, Pass: []string{"192.168.1.0/24"}}
	normalLoaded := "block drop out on ! lo0 inet from any to <splitr_block>\n" +
		"pass out on ! lo0 inet from any to <splitr_pass>\n"

	tests := []struct {
		name   string
		loaded string
		rs     protect.Ruleset
		want   bool
		reason string
	}{
		{
			name:   "всё на месте",
			loaded: normalLoaded,
			rs:     normal,
		},
		{
			name:   "якорь пуст",
			rs:     normal,
			want:   true,
			reason: "anchor is empty",
		},
		{
			name:   "якорь из одних пробелов",
			loaded: "   \n\n",
			rs:     normal,
			want:   true,
			reason: "anchor is empty",
		},
		{
			name:   "пропало правило блокировки",
			loaded: "pass out on ! lo0 inet from any to <splitr_pass>\n",
			rs:     normal,
			want:   true,
			reason: "0 block rules instead of 1",
		},
		{
			name:   "не хватает правила блокировки DNS",
			loaded: normalLoaded,
			rs:     protect.Ruleset{Block: []string{"10.0.0.0/9"}, DNSServers: []string{"10.0.0.1"}, Pass: []string{"192.168.1.0/24"}},
			want:   true,
			reason: "1 block rules instead of 2",
		},
		{
			name:   "в panic-режиме потерялся quick",
			loaded: normalLoaded,
			rs:     protect.Ruleset{Block: []string{"10.0.0.0/9"}, Pass: []string{"192.168.1.0/24"}, Strict: true},
			want:   true,
			reason: "quick=false with strict=true",
		},
		{
			name:   "quick остался от снятого panic-режима",
			loaded: "block drop out quick on ! lo0 inet from any to <splitr_block>\npass out quick on ! lo0 inet from any to <splitr_pass>\n",
			rs:     normal,
			want:   true,
			reason: "quick=true with strict=false",
		},
		{
			name:   "пропало правило-исключение",
			loaded: "block drop out on ! lo0 inet from any to <splitr_block>\n",
			rs:     normal,
			want:   true,
			reason: "pass rule presence does not match",
		},
		{
			name:   "исключений нет и не должно быть",
			loaded: "block drop out on ! lo0 inet from any to <splitr_block>\n",
			rs:     protect.Ruleset{Block: []string{"10.0.0.0/9"}},
		},
		{
			name:   "panic-правила совпадают",
			loaded: "pass out quick on ! lo0 inet from any to <splitr_pass>\nblock drop out quick on ! lo0 inet from any to <splitr_block>\n",
			rs:     protect.Ruleset{Block: []string{"10.0.0.0/9"}, Pass: []string{"192.168.1.0/24"}, Strict: true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, why := rulesDrifted(tt.loaded, tt.rs)
			if got != tt.want {
				t.Fatalf("rulesDrifted = %v (%s), ожидалось %v", got, why, tt.want)
			}
			if tt.want && why != tt.reason {
				t.Fatalf("причина = %q, ожидалось %q", why, tt.reason)
			}
		})
	}
}

// Расхождение правил с ожидаемыми сторож чинит перезагрузкой якоря.
func TestTickReloadsDriftedAnchor(t *testing.T) {
	t.Parallel()

	d, pf, _, _ := newTestDaemon(t, testConfig())
	installed(t, d, pf)
	// Кто-то подменил содержимое якоря: блокировки в нём больше нет.
	if err := pf.LoadAnchor(protect.Anchor, []byte("pass out on ! lo0 inet from any to <splitr_pass>\n")); err != nil {
		t.Fatalf("LoadAnchor: %v", err)
	}

	d.tick(false)

	if !strings.Contains(pf.AnchorText(protect.Anchor), "block drop out") {
		t.Fatalf("правила не восстановлены:\n%s", pf.AnchorText(protect.Anchor))
	}
}

// --- сохранённое состояние --------------------------------------------------

// stateConfig — конфиг с файлом состояния во временном каталоге.
func stateConfig(t *testing.T) (config.Config, string) {
	t.Helper()
	cfg := testConfig()
	path := filepath.Join(t.TempDir(), "splitr.state.json")
	cfg.Daemon.StateFile = path
	return cfg, path
}

// Снятая руками защита обязана пережить перезапуск демона:
// иначе после перезагрузки она молча вернётся, а человек об этом не узнает.
func TestSaveAndRestoreState(t *testing.T) {
	requireNonRoot(t)
	t.Parallel()

	cfg, statePath := stateConfig(t)
	d, _, _, _ := newTestDaemon(t, cfg)
	d.activeName = "beta"
	_ = d.SetStrict(true)
	_ = d.SetEnabled(false)

	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("файл состояния не создан: %v", err)
	}

	// Новый демон с тем же конфигом обязан поднять сохранённые переключатели.
	restored, _, _, _ := newTestDaemon(t, cfg)
	restored.restoreState()

	if restored.activeName != "beta" {
		t.Errorf("активный профиль = %q, ожидался pc", restored.activeName)
	}
	if !restored.strictMode {
		t.Error("panic-режим не восстановлен")
	}
	if restored.Config().Protection.Enabled {
		t.Error("выключенный Ð·Ð°ÑÐ¸ÑÐ° не должен включаться сам")
	}
}

func TestRestoreStateIgnoresMissingAndBrokenFile(t *testing.T) {
	t.Parallel()

	t.Run("файла нет", func(t *testing.T) {
		t.Parallel()
		cfg, _ := stateConfig(t)
		d, _, _, _ := newTestDaemon(t, cfg)
		d.restoreState()
		if d.activeName != "alpha" || d.strictMode {
			t.Fatalf("состояние без файла должно остаться дефолтным: профиль %q, panic %v", d.activeName, d.strictMode)
		}
	})

	t.Run("файл повреждён", func(t *testing.T) {
		t.Parallel()
		cfg, path := stateConfig(t)
		if err := os.WriteFile(path, []byte("{не json"), 0o600); err != nil {
			t.Fatalf("записать состояние: %v", err)
		}
		log := &syncBuffer{}
		d := NewWithDeps(cfg, "конфиг.yaml", log, pftest.New(), newFakeTunnel(), &fakeOps{})
		d.restoreState()
		if !strings.Contains(log.String(), "is corrupt") {
			t.Fatalf("в журнале нет жалобы на повреждённое состояние:\n%s", log.String())
		}
		if d.strictMode {
			t.Fatal("из повреждённого файла ничего браться не должно")
		}
	})
}

// Профиль, которого больше нет в конфиге, восстанавливать нельзя.
func TestRestoreStateSkipsUnknownProfile(t *testing.T) {
	t.Parallel()

	cfg, path := stateConfig(t)
	if err := os.WriteFile(path, []byte(`{"active_profile":"нет-такого","strict_mode":true}`), 0o600); err != nil {
		t.Fatalf("записать состояние: %v", err)
	}
	d, _, _, _ := newTestDaemon(t, cfg)
	d.restoreState()

	if d.activeName != "alpha" {
		t.Fatalf("активный профиль = %q, ожидался alpha из конфига", d.activeName)
	}
	if !d.strictMode {
		t.Fatal("panic-режим восстановить всё же следовало")
	}
}

// Режим, выбранный через UI, тоже переживает перезапуск.
func TestRestoreStateAppliesSavedMode(t *testing.T) {
	t.Parallel()

	cfg, path := stateConfig(t)
	if err := os.WriteFile(path, []byte(`{"active_profile":"alpha","mode":"public"}`), 0o600); err != nil {
		t.Fatalf("записать состояние: %v", err)
	}
	d, _, _, _ := newTestDaemon(t, cfg)
	d.restoreState()

	if got := d.Config().Protection.Mode; got != config.ModePublic {
		t.Fatalf("режим = %q, ожидался public", got)
	}
}

// Токен pf из прошлого запуска нужен, чтобы отпустить старую ссылку
// уже после взятия новой — иначе pf выключится вместе с блокировкой.
func TestRestoreStateKeepsPreviousPFToken(t *testing.T) {
	t.Parallel()

	cfg, path := stateConfig(t)
	if err := os.WriteFile(path, []byte(`{"active_profile":"alpha","pf_token":"12345"}`), 0o600); err != nil {
		t.Fatalf("записать состояние: %v", err)
	}
	d, _, _, _ := newTestDaemon(t, cfg)
	d.restoreState()

	if d.previousPFToken != "12345" {
		t.Fatalf("previousPFToken = %q, ожидалось 12345", d.previousPFToken)
	}
	token, err := LoadPFToken(path)
	if err != nil || token != "12345" {
		t.Fatalf("LoadPFToken = %q, %v", token, err)
	}
}

func TestLoadPFTokenErrors(t *testing.T) {
	t.Parallel()

	if _, err := LoadPFToken(filepath.Join(t.TempDir(), "нет-файла")); err == nil {
		t.Fatal("отсутствующий файл состояния должен приводить к ошибке")
	}
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("не json"), 0o600); err != nil {
		t.Fatalf("записать состояние: %v", err)
	}
	if _, err := LoadPFToken(path); err == nil {
		t.Fatal("повреждённый файл состояния должен приводить к ошибке")
	}
}

// Пустой state_file полностью отключает работу с состоянием.
func TestStateFileDisabled(t *testing.T) {
	t.Parallel()

	d, _, _, _ := newTestDaemon(t, testConfig())
	d.saveState()
	d.restoreState()
	if d.strictMode {
		t.Fatal("без файла состояния ничего меняться не должно")
	}
}

// --- смена режима на лету ---------------------------------------------------

func TestSetMode(t *testing.T) {
	requireNonRoot(t)
	t.Parallel()

	t.Run("режим меняется и попадает в статус", func(t *testing.T) {
		t.Parallel()
		d, pf, _, _ := newTestDaemon(t, testConfig())
		installed(t, d, pf)

		_ = d.SetMode(config.ModePublic)

		if got := d.Config().Protection.Mode; got != config.ModePublic {
			t.Fatalf("режим = %q, ожидался public", got)
		}
		rs, err := d.ruleset()
		if err != nil {
			t.Fatalf("ruleset: %v", err)
		}
		if strings.Join(rs.Block, ",") != "203.0.113.0/24" {
			t.Fatalf("Block = %v, ожидались только публичные сети", rs.Block)
		}
	})

	t.Run("неизвестный режим отвергается", func(t *testing.T) {
		t.Parallel()
		d, _, _, _ := newTestDaemon(t, testConfig())
		if err := d.SetMode("иногда"); err == nil {
			t.Fatal("ожидалась ошибка про недопустимый режим")
		}
		if d.Config().Protection.Mode != config.ModeAll {
			t.Fatal("режим не должен меняться при ошибке")
		}
	})

	t.Run("custom без списка сетей отвергается", func(t *testing.T) {
		t.Parallel()
		d, _, _, _ := newTestDaemon(t, testConfig())
		err := d.SetMode(config.ModeCustom)
		if err == nil || !strings.Contains(err.Error(), "protection.block") {
			t.Fatalf("ошибка = %v, ожидалась жалоба на пустой protect.block", err)
		}
		if d.Config().Protection.Mode != config.ModeAll {
			t.Fatal("режим не должен меняться при ошибке")
		}
	})
}

// --- запись конфига через API -----------------------------------------------

func TestWriteConfig(t *testing.T) {
	requireNonRoot(t)
	t.Parallel()

	const original = `
default_profile: alpha
subnets:
  - 10.0.0.0/9
profiles:
  alpha:
    remote: user@alpha
`

	t.Run("корректный конфиг записывается и перечитывается", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
			t.Fatalf("записать конфиг: %v", err)
		}
		d := NewWithDeps(testConfig(), path, io.Discard, pftest.New(), newFakeTunnel(), &fakeOps{})

		_ = d.WriteConfig([]byte(`
default_profile: alpha
subnets:
  - 11.0.0.0/8
profiles:
  alpha:
    remote: user@alpha
`))

		if strings.Join(d.Config().Subnets, ",") != "11.0.0.0/8" {
			t.Fatalf("subnets = %v, конфиг не перечитан", d.Config().Subnets)
		}
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("прочитать конфиг: %v", err)
		}
		if !strings.Contains(string(body), "11.0.0.0/8") {
			t.Fatalf("на диске остался старый конфиг:\n%s", body)
		}
	})

	t.Run("кривой конфиг не подменяет рабочий", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
			t.Fatalf("записать конфиг: %v", err)
		}
		d := NewWithDeps(testConfig(), path, io.Discard, pftest.New(), newFakeTunnel(), &fakeOps{})

		err := d.WriteConfig([]byte("protection:\n  mode: иногда\nprofiles:\n  visa:\n    remote: u@h\n"))
		if err == nil || !strings.Contains(err.Error(), "rejected") {
			t.Fatalf("ошибка = %v, ожидался отказ", err)
		}
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("прочитать конфиг: %v", err)
		}
		if string(body) != original {
			t.Fatalf("рабочий конфиг испорчен:\n%s", body)
		}
		if _, err := os.Stat(path + ".new"); !os.IsNotExist(err) {
			t.Fatal("временный файл конфига должен убираться за собой")
		}
	})
}

// --- туннель, поднятый мимо демона ------------------------------------------

// Якоря sshuttle есть, а своего процесса у демона нет: туннель подняли руками
// или старым скриптом. Показывать «down» нельзя — человек решит, что защита
// работает, хотя трафик как раз идёт в туннель.
func TestStatusReportsExternalTunnel(t *testing.T) {
	t.Parallel()

	d, pf, _, _ := newTestDaemon(t, testConfig())
	installed(t, d, pf)
	pf.SetSshuttleAnchors("sshuttle-12300")

	st := d.Status()
	if st.Tunnel != "external" || !st.External {
		t.Fatalf("статус = %+v, ожидался внешний туннель", st)
	}
	if st.Blocking {
		t.Fatal("при живом туннеле блокировка не действует")
	}
}

// Свой туннель внешним не считается.
func TestStatusOwnTunnelIsNotExternal(t *testing.T) {
	t.Parallel()

	d, pf, tun, _ := newTestDaemon(t, testConfig())
	installed(t, d, pf)
	pf.SetSshuttleAnchors("sshuttle-12300")
	tun.setSnapshot(shuttle.Snapshot{State: shuttle.StateUp, Profile: "alpha", PID: 4242})

	st := d.Status()
	if st.External || st.Tunnel != "up" {
		t.Fatalf("статус = %+v, ожидался собственный туннель", st)
	}
}

func TestStatusCarriesVersionAndLogFile(t *testing.T) {
	t.Parallel()

	cfg := testConfig()
	cfg.Daemon.LogFile = "/tmp/splitr-test.log"
	d, _, _, _ := newTestDaemon(t, cfg)

	st := d.Status()
	if st.Version != Version {
		t.Fatalf("Version = %q, ожидалось %q", st.Version, Version)
	}
	if st.Mode != "all" {
		t.Fatalf("Mode = %q, ожидалось all", st.Mode)
	}
	if st.LogFile != "/tmp/splitr-test.log" {
		t.Fatalf("LogFile = %q", st.LogFile)
	}
	if st.StartedAt.IsZero() {
		t.Fatal("время старта должно быть проставлено")
	}
}

// --- осиротевшие якоря sshuttle ---------------------------------------------

// Якорь sshuttle без единого процесса sshuttle — это мусор от туннеля,
// убитого по SIGKILL. Принимая его за живой туннель, сторож переставал чинить
// главный набор правил, и защита не включалась вообще.
func TestTickCleansOrphanedSshuttleAnchors(t *testing.T) {
	requireNonRoot(t)
	t.Parallel()

	d, pf, _, _ := newTestDaemon(t, testConfig())
	installed(t, d, pf)
	pf.SetSshuttleAnchors("sshuttle-12299", "sshuttle-12300")
	d.shuttleRunning = func(string) (bool, error) { return false, nil }

	if up := d.tick(false); up {
		t.Fatal("якоря без процессов не должны считаться поднятым туннелем")
	}
	if anchors, _ := d.liveSshuttleAnchors(); len(anchors) != 0 {
		t.Fatalf("осиротевшие якоря обязаны быть очищены, остались значимыми: %v", anchors)
	}
}

// Живой процесс sshuttle — повод не трогать его якоря, даже если туннель
// подняли мимо splitr.
func TestTickKeepsAnchorsOfLiveExternalTunnel(t *testing.T) {
	requireNonRoot(t)
	t.Parallel()

	d, pf, _, _ := newTestDaemon(t, testConfig())
	installed(t, d, pf)
	pf.SetSshuttleAnchors("sshuttle-12300")

	if up := d.tick(false); !up {
		t.Fatal("живой внешний туннель обязан считаться поднятым")
	}
	if anchors, _ := d.liveSshuttleAnchors(); len(anchors) != 1 {
		t.Fatalf("якоря живого туннеля трогать нельзя, стало: %v", anchors)
	}
}

// Если проверить процессы не удалось, якоря трогать нельзя: снести якоря
// работающего туннеля дороже, чем лишний раз перестраховаться.
func TestTickKeepsAnchorsWhenProcessCheckFails(t *testing.T) {
	requireNonRoot(t)
	t.Parallel()

	d, pf, _, _ := newTestDaemon(t, testConfig())
	installed(t, d, pf)
	pf.SetSshuttleAnchors("sshuttle-12300")
	d.shuttleRunning = func(string) (bool, error) { return false, errors.New("ps недоступен") }

	if up := d.tick(false); !up {
		t.Fatal("при неизвестном состоянии процессов туннель считается живым")
	}
	if anchors, _ := d.liveSshuttleAnchors(); len(anchors) != 1 {
		t.Fatalf("якоря трогать было нельзя, стало: %v", anchors)
	}
	if warnings := d.Status().Warnings; len(warnings) == 0 {
		t.Fatal("о неудачной проверке процессов надо предупредить")
	}
}

// Пустой якорь sshuttle — оболочка от давно умершего туннеля: pf не удаляет
// якорь из дерева после очистки, и `pfctl -s Anchors` показывает его вечно.
// Перебить блокировку он не может, а демон однажды залип на нём навсегда.
func TestGhostAnchorWithoutRulesIsNotATunnel(t *testing.T) {
	requireNonRoot(t)
	t.Parallel()

	d, pf, _, _ := newTestDaemon(t, testConfig())
	installed(t, d, pf)
	pf.SetGhostSshuttleAnchors("sshuttle-12300")
	// Хозяина у оболочки нет — только это и делает её мусором.
	d.shuttleRunning = func(string) (bool, error) { return false, nil }

	if up := d.tick(false); up {
		t.Fatal("пустой якорь без процесса не должен считаться поднятым туннелем")
	}
	if st := d.Status(); !st.Blocking {
		t.Fatalf("при пустом якоре блокировка обязана действовать, статус: %+v", st)
	}
}

// Обратный случай: якорь пуст, но процесс sshuttle жив. Правила туннеля могут
// быть не видны нам по любой причине, а снести якорь живого туннеля дороже,
// чем лишний раз перестраховаться.
func TestGhostAnchorWithLiveProcessIsATunnel(t *testing.T) {
	requireNonRoot(t)
	t.Parallel()

	d, pf, _, _ := newTestDaemon(t, testConfig())
	installed(t, d, pf)
	pf.SetGhostSshuttleAnchors("sshuttle-12300")

	if up := d.tick(false); !up {
		t.Fatal("при живом процессе sshuttle якорь обязан считаться туннелем")
	}
	if st := d.Status(); len(st.SshuttleAnchs) != 1 {
		t.Fatalf("якоря живого туннеля обязаны быть видны в статусе: %+v", st.SshuttleAnchs)
	}
}

// --- автопереподключение ----------------------------------------------------

// После неудачи следующая попытка отодвигается: без этого демон долбился бы
// в недоступный хост каждый тик сторожа.
func TestReconnectBacksOffAfterFailure(t *testing.T) {
	requireNonRoot(t)
	t.Parallel()

	cfg := testConfig()
	cfg.Daemon.Autoreconnect = true
	cfg.Daemon.ReconnectDelay = time.Minute
	d, _, tun, _ := newTestDaemon(t, cfg)
	// Подъём должен провалиться — иначе проверять отступ после неудачи не на чем.
	tun.startErr = errors.New("хост недоступен")

	d.reconnect()
	_, _, _, killsAfterFirst := tun.counters()
	if killsAfterFirst != 1 {
		t.Fatalf("первая попытка должна была состояться, KillForeign %d", killsAfterFirst)
	}
	if d.reconnectFails != 1 {
		t.Fatalf("счётчик неудач = %d, ожидался 1", d.reconnectFails)
	}

	d.reconnect()
	if _, _, _, kills := tun.counters(); kills != killsAfterFirst {
		t.Fatalf("вторая попытка не должна была случиться раньше срока, KillForeign %d", kills)
	}
	if d.reconnectFails != 1 {
		t.Fatalf("счётчик неудач вырос без попытки: %d", d.reconnectFails)
	}
}

// Задержка растёт, но не бесконечно.
func TestReconnectBackoffIsCapped(t *testing.T) {
	requireNonRoot(t)
	t.Parallel()

	cfg := testConfig()
	cfg.Daemon.ReconnectDelay = time.Minute
	d, _, _, _ := newTestDaemon(t, cfg)

	for i := 0; i < 8; i++ {
		d.reconnectAt = time.Time{}
		d.reconnect()
	}
	if got := time.Until(d.reconnectAt); got > 5*time.Minute+time.Second {
		t.Fatalf("задержка выросла до %s, ожидался потолок в 5 минут", got)
	}
}

// --- Run --------------------------------------------------------------------

// Демон намеренно НЕ отпускает ссылку на pf при остановке: иначе перезапуск
// службы снимал бы блокировку на несколько секунд. Зато он отпускает ссылку
// прошлого запуска — уже после того, как взял новую.
func TestRunTakesNewPFTokenBeforeReleasingOld(t *testing.T) {
	requireNonRoot(t)
	t.Parallel()

	cfg, statePath := stateConfig(t)
	cfg.Daemon.WatchdogInterval = 10 * time.Millisecond
	d, pf, tun, _ := newTestDaemon(t, cfg)

	// Ссылка «прошлого запуска».
	old, err := pf.Enable()
	if err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if err := os.WriteFile(statePath, []byte(`{"active_profile":"alpha","pf_token":"`+old+`"}`), 0o600); err != nil {
		t.Fatalf("записать состояние: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()
	if err := d.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if pf.LiveTokens() != 1 {
		t.Fatalf("живых ссылок на pf %d, ожидалась ровно одна — своя", pf.LiveTokens())
	}
	if on, _ := pf.Enabled(); !on {
		t.Fatal("pf обязан остаться включённым после остановки демона")
	}
	if _, stops, _, _ := tun.counters(); stops != 1 {
		t.Fatalf("при остановке туннель должен гаситься, Stop вызван %d раз", stops)
	}
}

func TestRunFailsWhenPFCannotBeEnabled(t *testing.T) {
	t.Parallel()

	d, pf, _, _ := newTestDaemon(t, testConfig())
	pf.Fail(pftest.MethodEnable, errors.New("нет прав"))

	err := d.Run(t.Context())
	if err == nil || !strings.Contains(err.Error(), "enable pf") {
		t.Fatalf("ошибка = %v, ожидался отказ включения pf", err)
	}
}

// --- события сети и пробуждение из сна ---------------------------------------

// Смена сети обязана вызывать ту же проверку, что и тик сторожа, но сразу,
// а не через несколько секунд.
func TestNetworkEventRunsWatchdogPass(t *testing.T) {
	t.Parallel()

	d, pf, _, _ := newTestDaemon(t, testConfig())
	installed(t, d, pf)
	pf.FlushAll()

	if up := d.onNetworkEvent(netmon.Event{Reason: "изменились маршруты"}, false); up {
		t.Fatal("без якорей sshuttle туннель поднятым не считается")
	}
	if text := pf.AnchorText(protect.Anchor); !strings.Contains(text, "block drop out") {
		t.Fatalf("правила якоря не восстановлены по событию сети: %q", text)
	}
}

// Пробуждение из сна сбрасывает накопленную задержку переподключения:
// её копили неудачи в сети, которой больше нет.
func TestWakeResetsReconnectBackoff(t *testing.T) {
	t.Parallel()

	d, pf, _, _ := newTestDaemon(t, testConfig())
	installed(t, d, pf)

	d.mu.Lock()
	d.reconnectAt = time.Now().Add(time.Hour)
	d.reconnectFails = 5
	d.mu.Unlock()

	d.onNetworkEvent(netmon.Event{Reason: "пробуждение из сна", Wake: true}, false)

	d.mu.RLock()
	at, fails := d.reconnectAt, d.reconnectFails
	d.mu.RUnlock()
	if fails != 0 || at.After(time.Now()) {
		t.Fatalf("после сна ждать нельзя: попыток %d, следующая в %s", fails, at)
	}
}

// Состояния pf переживают сон: соединение, открытое в прошлой сети, пойдёт
// напрямую мимо блокировки, пока его состояние живо.
func TestWakeFlushesStatesWhenTunnelDown(t *testing.T) {
	t.Parallel()

	d, pf, _, _ := newTestDaemon(t, testConfig())
	installed(t, d, pf)

	d.onNetworkEvent(netmon.Event{Reason: "пробуждение из сна", Wake: true}, false)

	if strings.Join(pf.KilledStates(), ",") != "10.0.0.0/9,203.0.113.0/24" {
		t.Fatalf("состояния = %v, ожидался сброс по всем блокируемым сетям", pf.KilledStates())
	}
}

// При живом туннеле состояния трогать нельзя: сброс оборвал бы соединения,
// которые как раз идут через туннель.
func TestWakeKeepsStatesWhenTunnelUp(t *testing.T) {
	t.Parallel()

	d, pf, _, _ := newTestDaemon(t, testConfig())
	installed(t, d, pf)
	pf.SetSshuttleAnchors("sshuttle-12300")

	d.onNetworkEvent(netmon.Event{Reason: "пробуждение из сна", Wake: true}, true)

	if len(pf.KilledStates()) != 0 {
		t.Fatalf("при живом туннеле состояния трогать нельзя: %v", pf.KilledStates())
	}
}

// Обычная смена сети состояний не трогает: это делает либо потеря туннеля,
// либо пробуждение.
func TestNetworkEventWithoutWakeKeepsStates(t *testing.T) {
	t.Parallel()

	d, pf, _, _ := newTestDaemon(t, testConfig())
	installed(t, d, pf)

	d.onNetworkEvent(netmon.Event{Reason: "изменились адреса интерфейсов"}, false)

	if len(pf.KilledStates()) != 0 {
		t.Fatalf("состояния сброшены без повода: %v", pf.KilledStates())
	}
}

// --- сброс состояний при ужесточении защиты ----------------------------------

// Strict обещает оборвать трафик немедленно. Без сброса состояний pf уже
// открытые соединения продолжали бы идти: пакет по живому состоянию правила
// не перечитывает.
func TestStrictFlushesLiveStates(t *testing.T) {
	t.Parallel()

	d, pf, _, _ := newTestDaemon(t, testConfig())
	installed(t, d, pf)
	pf.SetSshuttleAnchors("sshuttle-12300")

	if err := d.SetStrict(true); err != nil {
		t.Fatalf("panic: %v", err)
	}
	if len(pf.KilledStates()) == 0 {
		t.Fatal("strict обязан оборвать живые соединения к защищаемым сетям")
	}
}

// Настройка kill_states описывает штатную потерю туннеля; panic жмут тогда,
// когда трафик надо оборвать прямо сейчас, и спрашивать её незачем.
func TestStrictFlushesStatesEvenWithKillStatesOff(t *testing.T) {
	t.Parallel()

	cfg := testConfig()
	cfg.Protection.KillStates = false
	d, pf, _, _ := newTestDaemon(t, cfg)
	installed(t, d, pf)

	if err := d.SetStrict(true); err != nil {
		t.Fatalf("panic: %v", err)
	}
	if len(pf.KilledStates()) == 0 {
		t.Fatal("panic обязан рвать состояния независимо от kill_states")
	}
}

// Снятие panic ничего не рвёт: это ослабление защиты, а не ужесточение.
func TestUnsetStrictKeepsStates(t *testing.T) {
	t.Parallel()

	d, pf, _, _ := newTestDaemon(t, testConfig())
	installed(t, d, pf)
	if err := d.SetStrict(false); err != nil {
		t.Fatalf("снять panic: %v", err)
	}
	if len(pf.KilledStates()) != 0 {
		t.Fatalf("снятие panic не должно рвать соединения: %v", pf.KilledStates())
	}
}

// Включение Ð·Ð°ÑÐ¸ÑÐ° — тоже ужесточение: старые соединения обязаны умереть.
func TestEnablingProtectionFlushesStates(t *testing.T) {
	t.Parallel()

	cfg := testConfig()
	cfg.Protection.Enabled = false
	d, pf, _, _ := newTestDaemon(t, cfg)
	installed(t, d, pf)

	if err := d.SetEnabled(true); err != nil {
		t.Fatalf("включить Ð·Ð°ÑÐ¸ÑÐ°: %v", err)
	}
	if len(pf.KilledStates()) == 0 {
		t.Fatal("включение защиты обязано оборвать открытые соединения")
	}
}

// Выключение защиты состояний не трогает.
func TestDisablingProtectionKeepsStates(t *testing.T) {
	t.Parallel()

	d, pf, _, _ := newTestDaemon(t, testConfig())
	installed(t, d, pf)

	if err := d.SetEnabled(false); err != nil {
		t.Fatalf("выключить Ð·Ð°ÑÐ¸ÑÐ°: %v", err)
	}
	if len(pf.KilledStates()) != 0 {
		t.Fatalf("снятие защиты не должно рвать соединения: %v", pf.KilledStates())
	}
}

// При живом туннеле включение защиты состояний не трогает: блокировка его
// всё равно не касается, а сброс оборвал бы работающие соединения.
func TestEnablingProtectionKeepsStatesWhenTunnelUp(t *testing.T) {
	t.Parallel()

	cfg := testConfig()
	cfg.Protection.Enabled = false
	d, pf, _, _ := newTestDaemon(t, cfg)
	installed(t, d, pf)
	pf.SetSshuttleAnchors("sshuttle-12300")

	if err := d.SetEnabled(true); err != nil {
		t.Fatalf("включить Ð·Ð°ÑÐ¸ÑÐ°: %v", err)
	}
	if len(pf.KilledStates()) != 0 {
		t.Fatalf("при живом туннеле состояния трогать нельзя: %v", pf.KilledStates())
	}
}

// --- проверка достижимости хоста перед подъёмом туннеля ---------------------

// Проверка обязана ходить тем же ssh, что и туннель. Голый ssh без ключа
// профиля упирается в «Permission denied» и врёт, что хост недоступен —
// подъём останавливался бы там, где всё в порядке.
func TestReachabilityUsesProfileSSHCommand(t *testing.T) {
	requireNonRoot(t)
	t.Parallel()

	cfg := testConfig()
	cfg.Sshuttle.SSHOptions = []string{"ServerAliveInterval=7"}
	p := cfg.Profiles["alpha"]
	p.SSHKey = "/home/u/.ssh/id_test"
	cfg.Profiles["alpha"] = p

	d, _, _, ops := newTestDaemon(t, cfg)
	if err := d.Up("alpha"); err != nil {
		t.Fatalf("Up: %v", err)
	}

	joined := strings.Join(ops.reachArgs, " ")
	if !strings.Contains(joined, "-i /home/u/.ssh/id_test") {
		t.Fatalf("ключ профиля не попал в проверку достижимости: %q", joined)
	}
	if !strings.Contains(joined, "ServerAliveInterval=7") {
		t.Fatalf("опции ssh не попали в проверку достижимости: %q", joined)
	}
}

// Недоступный хост обязан останавливать подъём до того, как мы погасим
// посторонние процессы и перепишем системный DNS. Раньше всё это делалось
// впустую, sshuttle стартовал и молча умирал, а человек видел «failed»
// без причины и жал Connect снова и снова.
func TestUpStopsBeforeSideEffectsWhenRemoteIsUnreachable(t *testing.T) {
	requireNonRoot(t)
	t.Parallel()

	d, _, tun, ops := newTestDaemon(t, testConfig())
	ops.reachErr = errors.New("exit status 255")
	ops.reachOut = []byte("ssh: connect to host port 22: Operation timed out")

	err := d.Up("alpha")
	if err == nil {
		t.Fatal("подъём к недоступному хосту обязан вернуть ошибку")
	}
	if !strings.Contains(err.Error(), "not reachable") {
		t.Fatalf("текст ошибки = %q, ожидалось про недоступность", err)
	}
	if starts, _, _, kills := tun.counters(); len(starts) != 0 || kills != 0 {
		t.Fatalf("до проверки достижимости трогать процессы нельзя: старты %v, kills %d", starts, kills)
	}
	if scripts, _, flushes := ops.snapshot(); len(scripts) != 0 || flushes != 0 {
		t.Fatalf("системный DNS трогать было нельзя: скрипты %v, сбросы %d", scripts, flushes)
	}
}

// Ссылку на повторный вход нельзя терять: без неё отказ необъясним,
// а починить его нечем. Она обязана дойти до статуса и до текста ошибки.
func TestUpSurfacesReAuthURL(t *testing.T) {
	requireNonRoot(t)
	t.Parallel()

	const url = "https://login.tailscale.com/a/1234567890ab"
	d, _, _, ops := newTestDaemon(t, testConfig())
	ops.reachErr = errors.New("exit status 255")
	ops.reachOut = []byte("# Tailscale SSH requires you to authenticate:\n" + url + "\n")

	err := d.Up("alpha")
	if err == nil || !strings.Contains(err.Error(), url) {
		t.Fatalf("ошибка = %v, в ней ожидалась ссылка %s", err, url)
	}
	action := d.Status().ActionRequired
	if action == nil || action.URL != url {
		t.Fatalf("ссылка не попала в статус: %+v", action)
	}
}

// Доступный хост снимает прежнее требование входа: иначе оно висело бы
// в интерфейсе и после того, как человек уже вошёл.
func TestUpClearsActionWhenRemoteIsReachable(t *testing.T) {
	requireNonRoot(t)
	t.Parallel()

	d, _, tun, ops := newTestDaemon(t, testConfig())
	tun.SetAction(shuttle.AuthAction("https://login.tailscale.com/a/stale"))

	if err := d.Up("alpha"); err != nil {
		t.Fatalf("Up: %v", err)
	}
	if ops.reachChecks == 0 {
		t.Fatal("достижимость хоста обязана проверяться перед подъёмом")
	}
	if action := d.Status().ActionRequired; action != nil {
		t.Fatalf("устаревшее требование входа осталось: %+v", action)
	}
}
