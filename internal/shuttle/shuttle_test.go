package shuttle

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tasticolly/splitr/internal/config"
)

// syncBuffer — журнал, в который безопасно пишут и Runner, и его дочерний процесс.
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

// baseConfig — конфиг, от которого пляшут тесты аргументов.
// configFor возвращает базовый конфиг с заданным путём к sshuttle.
// Путь обязан лежать в конфиге: раннер берёт его оттуда при каждом запуске,
// иначе тест незаметно запустил бы настоящий sshuttle из PATH.
func configFor(path string) config.Config {
	cfg := baseConfig()
	cfg.Sshuttle.Path = path
	return cfg
}

func baseConfig() config.Config {
	cfg := config.Default()
	cfg.Subnets = []string{"10.0.0.0/9", "203.0.113.0/24"}
	cfg.Excludes = []string{"192.168.1.0/24"}
	return cfg
}

func TestNewRunnerFallsBackToBareBinaryName(t *testing.T) {
	t.Parallel()

	r := NewRunner(config.Sshuttle{}, io.Discard)
	if r.sshuttlePath != "sshuttle" {
		t.Fatalf("путь к бинарю = %q, ожидался sshuttle из PATH", r.sshuttlePath)
	}
	if snap := r.Snapshot(); snap.State != StateDown {
		t.Fatalf("свежий Runner должен быть в состоянии down, получено %q", snap.State)
	}
}

func TestArgsForFullProfile(t *testing.T) {
	t.Parallel()

	r := NewRunner(config.Sshuttle{Path: "/opt/homebrew/bin/sshuttle"}, io.Discard)

	cfg := baseConfig()
	cfg.Sshuttle.SSHOptions = []string{"ServerAliveInterval=15", "TCPKeepAlive=yes"}
	cfg.Sshuttle.ExtraArgs = []string{"--listen", "127.0.0.1:12300"}

	p := config.Profile{
		Remote:     "user@host",
		SSHKey:     "/home/u/.ssh/id_ed25519",
		KnownHosts: "/home/u/.ssh/known_hosts",
		DNS:        true,
	}
	got := r.Args(cfg, p)
	want := []string{
		"-r", "user@host",
		"--ssh-cmd", "ssh -i /home/u/.ssh/id_ed25519 -o UserKnownHostsFile=/home/u/.ssh/known_hosts -o ServerAliveInterval=15 -o TCPKeepAlive=yes",
		"--dns",
		"--listen", "127.0.0.1:12300",
		"-x", "192.168.1.0/24",
		"10.0.0.0/9", "203.0.113.0/24",
	}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("Args =\n%q\nожидалось\n%q", got, want)
	}
}

func TestSSHCmdVariants(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		options []string
		profile config.Profile
		want    string
	}{
		{
			name:    "профиль без ключа и known_hosts",
			profile: config.Profile{Remote: "user@host"},
			want:    "ssh",
		},
		{
			name:    "только ключ",
			profile: config.Profile{Remote: "user@host", SSHKey: "/k"},
			want:    "ssh -i /k",
		},
		{
			name:    "только known_hosts",
			profile: config.Profile{Remote: "user@host", KnownHosts: "/kh"},
			want:    "ssh -o UserKnownHostsFile=/kh",
		},
		{
			name:    "ключ, known_hosts и опции ssh",
			options: []string{"TCPKeepAlive=yes"},
			profile: config.Profile{Remote: "user@host", SSHKey: "/k", KnownHosts: "/kh"},
			want:    "ssh -i /k -o UserKnownHostsFile=/kh -o TCPKeepAlive=yes",
		},
		{
			name:    "опции ssh без ключа",
			options: []string{"ServerAliveInterval=15"},
			profile: config.Profile{Remote: "user@host"},
			want:    "ssh -o ServerAliveInterval=15",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := NewRunner(config.Sshuttle{Path: "/bin/true"}, io.Discard)
			if got := sshCmd(config.Sshuttle{SSHOptions: tt.options}, tt.profile); got != tt.want {
				t.Fatalf("--ssh-cmd = %q, ожидалось %q", got, tt.want)
			}
			cfg := baseConfig()
			cfg.Sshuttle.SSHOptions = tt.options
			args := r.Args(cfg, tt.profile)
			if args[2] != "--ssh-cmd" || args[3] != tt.want {
				t.Fatalf("строка --ssh-cmd не попала в аргументы: %q", args)
			}
		})
	}
}

func TestArgsDNSFlagOnlyWhenProfileAsksForIt(t *testing.T) {
	t.Parallel()

	r := NewRunner(config.Sshuttle{Path: "/bin/true"}, io.Discard)

	withDNS := strings.Join(r.Args(baseConfig(), config.Profile{Remote: "u@h", DNS: true}), " ")
	if !strings.Contains(withDNS, "--dns") {
		t.Fatalf("при dns:true ожидался --dns: %s", withDNS)
	}
	withoutDNS := strings.Join(r.Args(baseConfig(), config.Profile{Remote: "u@h"}), " ")
	if strings.Contains(withoutDNS, "--dns") {
		t.Fatalf("при dns:false --dns недопустим: %s", withoutDNS)
	}
}

// Все -x обязаны стоять до списка сетей: иначе sshuttle примет исключение за сеть.
func TestArgsExcludesComeBeforeSubnets(t *testing.T) {
	t.Parallel()

	cfg := baseConfig()
	cfg.Excludes = []string{"192.168.1.0/24", "192.168.2.0/24"}
	r := NewRunner(config.Sshuttle{Path: "/bin/true"}, io.Discard)
	args := r.Args(cfg, config.Profile{Remote: "u@h", Excludes: []string{"10.10.0.0/16"}})

	lastExclude, firstSubnet := -1, len(args)
	for i, a := range args {
		if a == "-x" {
			lastExclude = i + 1
		}
	}
	for i, a := range args {
		if a == "10.0.0.0/9" {
			firstSubnet = i
			break
		}
	}
	if lastExclude < 0 || lastExclude > firstSubnet {
		t.Fatalf("исключения обязаны идти до сетей: %q", args)
	}
	joined := strings.Join(args, " ")
	for _, ex := range []string{"-x 192.168.1.0/24", "-x 192.168.2.0/24", "-x 10.10.0.0/16"} {
		if !strings.Contains(joined, ex) {
			t.Fatalf("нет %q в аргументах: %s", ex, joined)
		}
	}
	if !strings.HasSuffix(joined, "10.0.0.0/9 203.0.113.0/24") {
		t.Fatalf("сети обязаны замыкать команду: %s", joined)
	}
}

// Профильные subnets полностью заменяют глобальные.
func TestArgsProfileSubnetsOverrideGlobal(t *testing.T) {
	t.Parallel()

	r := NewRunner(config.Sshuttle{Path: "/bin/true"}, io.Discard)
	args := r.Args(baseConfig(), config.Profile{Remote: "u@h", Subnets: []string{"172.16.0.0/12"}})
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "10.0.0.0/9") {
		t.Fatalf("глобальные сети не должны попадать в команду: %s", joined)
	}
	if !strings.HasSuffix(joined, "172.16.0.0/12") {
		t.Fatalf("ожидалась профильная сеть в конце: %s", joined)
	}
}

func TestArgsWithoutExcludes(t *testing.T) {
	t.Parallel()

	cfg := baseConfig()
	cfg.Excludes = nil
	args := NewRunner(config.Sshuttle{Path: "/bin/true"}, io.Discard).Args(cfg, config.Profile{Remote: "u@h"})
	if strings.Contains(strings.Join(args, " "), "-x") {
		t.Fatalf("без исключений -x быть не должно: %q", args)
	}
}

// Настройки sshuttle обязаны браться из свежего конфига, а не из снимка,
// сделанного при создании раннера: иначе правка extra_args вступала бы в силу
// только после перезапуска демона, причём молча.
func TestArgsFollowReloadedConfig(t *testing.T) {
	t.Parallel()

	r := NewRunner(config.Sshuttle{Path: "/bin/true"}, io.Discard)

	cfg := baseConfig()
	cfg.Sshuttle.ExtraArgs = []string{"--latency-buffer-size", "262144"}
	cfg.Sshuttle.SSHOptions = []string{"ServerAliveInterval=42"}

	args := r.Args(cfg, config.Profile{Remote: "user@host", DNS: true})
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--latency-buffer-size 262144") {
		t.Fatalf("extra_args из конфига не попали в аргументы: %q", joined)
	}
	if !strings.Contains(joined, "ServerAliveInterval=42") {
		t.Fatalf("ssh_options из конфига не попали в аргументы: %q", joined)
	}
	// Порядок важен: sshuttle ждёт свои флаги до списка сетей.
	if strings.Index(joined, "--latency-buffer-size") > strings.Index(joined, " -x ") {
		t.Fatalf("флаги обязаны идти до исключений и сетей: %q", joined)
	}
}

// --- жизненный цикл процесса ------------------------------------------------
//
// Настоящий sshuttle не запускается никогда: вместо него подставляется
// безобидный shell-скрипт во временном каталоге.

// fakeSshuttle кладёт скрипт-заглушку и отдаёт путь к нему.
func fakeSshuttle(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sshuttle-stub")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
		t.Fatalf("создать заглушку sshuttle: %v", err)
	}
	return path
}

// waitFor крутится, пока условие не выполнится или не истечёт запас времени.
// Запас щедрый нарочно: тесты запускают процессы, а на загруженной машине
// планировщик может задержать их на заметное время.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("не дождались: %s", what)
}

func TestStartAndStopManageProcess(t *testing.T) {
	t.Parallel()

	log := &syncBuffer{}
	r := NewRunner(config.Sshuttle{Path: fakeSshuttle(t, "sleep 30")}, log)

	if err := r.Start(configFor(fakeSshuttle(t, "sleep 30")), "visa", config.Profile{Remote: "u@h"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = r.Stop(context.Background()) })

	snap := r.Snapshot()
	if snap.State != StateStarting {
		t.Fatalf("сразу после запуска ожидалось состояние starting, получено %q", snap.State)
	}
	if snap.Profile != "visa" {
		t.Fatalf("профиль = %q", snap.Profile)
	}
	if snap.PID <= 0 {
		t.Fatalf("ожидался PID процесса, получено %d", snap.PID)
	}
	if snap.Since.IsZero() {
		t.Fatal("время старта не проставлено")
	}
	if !r.Running() {
		t.Fatal("Running() должен быть true при живом процессе")
	}

	// Сторож переводит состояние в up, когда в pf появляется якорь sshuttle.
	r.MarkUp()
	if got := r.Snapshot().State; got != StateUp {
		t.Fatalf("после MarkUp ожидалось up, получено %q", got)
	}

	if err := r.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	waitFor(t, "процесс завершится", func() bool { return !r.Running() })
	if !strings.Contains(log.String(), "starting:") {
		t.Fatalf("в журнале нет записи о запуске:\n%s", log.String())
	}
}

func TestStartRejectsSecondTunnel(t *testing.T) {
	t.Parallel()

	r := NewRunner(config.Sshuttle{Path: fakeSshuttle(t, "sleep 30")}, io.Discard)
	if err := r.Start(configFor(fakeSshuttle(t, "sleep 30")), "visa", config.Profile{Remote: "u@h"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = r.Stop(context.Background()) })

	err := r.Start(configFor(fakeSshuttle(t, "sleep 30")), "pc", config.Profile{Remote: "u@h2"})
	if err == nil {
		t.Fatal("второй Start при живом туннеле должен возвращать ошибку")
	}
	if !strings.Contains(err.Error(), "visa") {
		t.Fatalf("ошибка = %v, ожидалось упоминание активного профиля", err)
	}
	if got := r.Snapshot().Profile; got != "visa" {
		t.Fatalf("профиль подменился на %q", got)
	}
}

func TestStartWithMissingBinaryMarksFailed(t *testing.T) {
	t.Parallel()

	r := NewRunner(config.Sshuttle{Path: filepath.Join(t.TempDir(), "нет-такого")}, io.Discard)
	err := r.Start(configFor(filepath.Join(t.TempDir(), "нет-такого")), "visa", config.Profile{Remote: "u@h"})
	if err == nil {
		t.Fatal("запуск несуществующего бинаря должен вернуть ошибку")
	}
	snap := r.Snapshot()
	if snap.State != StateFailed {
		t.Fatalf("состояние = %q, ожидалось failed", snap.State)
	}
	if snap.LastError == "" {
		t.Fatal("ожидалось описание последней ошибки")
	}
	if r.Running() {
		t.Fatal("Running() должен быть false")
	}
}

// Процесс, упавший сам, обязан оставить состояние failed и текст ошибки.
func TestProcessExitWithErrorMarksFailed(t *testing.T) {
	t.Parallel()

	r := NewRunner(config.Sshuttle{Path: fakeSshuttle(t, "exit 3")}, io.Discard)
	if err := r.Start(configFor(fakeSshuttle(t, "exit 3")), "visa", config.Profile{Remote: "u@h"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, "процесс упадёт", func() bool { return r.Snapshot().State == StateFailed })
	if r.Snapshot().LastError == "" {
		t.Fatal("ожидался текст ошибки завершения")
	}
	if r.Running() {
		t.Fatal("после завершения процесса Running() должен быть false")
	}
}

// Штатное завершение процесса возвращает состояние в down.
func TestProcessCleanExitMarksDown(t *testing.T) {
	t.Parallel()

	r := NewRunner(config.Sshuttle{Path: fakeSshuttle(t, "exit 0")}, io.Discard)
	if err := r.Start(configFor(fakeSshuttle(t, "exit 0")), "visa", config.Profile{Remote: "u@h"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, "процесс завершится", func() bool { return r.Snapshot().State == StateDown })
	if got := r.Snapshot().LastError; got != "" {
		t.Fatalf("при штатном выходе ошибки быть не должно: %q", got)
	}
}

// После падения туннеля Start обязан снова сработать.
func TestStartAfterFailureIsAllowed(t *testing.T) {
	t.Parallel()

	r := NewRunner(config.Sshuttle{Path: fakeSshuttle(t, "exit 3")}, io.Discard)
	if err := r.Start(configFor(fakeSshuttle(t, "exit 3")), "visa", config.Profile{Remote: "u@h"}); err != nil {
		t.Fatalf("первый Start: %v", err)
	}
	waitFor(t, "первый процесс упадёт", func() bool { return r.Snapshot().State == StateFailed })
	if err := r.Start(configFor(fakeSshuttle(t, "exit 3")), "visa", config.Profile{Remote: "u@h"}); err != nil {
		t.Fatalf("повторный Start после падения: %v", err)
	}
	waitFor(t, "второй процесс упадёт", func() bool { return r.Snapshot().State == StateFailed })
}

func TestStopWithoutRunningProcessIsNoop(t *testing.T) {
	t.Parallel()

	r := NewRunner(config.Sshuttle{Path: "/bin/true"}, io.Discard)
	if err := r.Stop(context.Background()); err != nil {
		t.Fatalf("Stop без процесса: %v", err)
	}
}

// Процесс, игнорирующий SIGTERM, добивается по отмене контекста.
func TestStopKillsProcessIgnoringSIGTERM(t *testing.T) {
	t.Parallel()

	r := NewRunner(config.Sshuttle{Path: fakeSshuttle(t, "trap '' TERM; sleep 30")}, io.Discard)
	if err := r.Start(configFor(fakeSshuttle(t, "trap '' TERM; sleep 30")), "visa", config.Profile{Remote: "u@h"}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := r.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	waitFor(t, "процесс будет убит", func() bool { return !r.Running() })
}

// MarkUp работает только из состояния starting: поднимать down или failed нечего.
func TestMarkUpOnlyPromotesStartingState(t *testing.T) {
	t.Parallel()

	r := NewRunner(config.Sshuttle{Path: "/bin/true"}, io.Discard)
	r.MarkUp()
	if got := r.Snapshot().State; got != StateDown {
		t.Fatalf("состояние = %q, MarkUp не должен поднимать выключенный туннель", got)
	}
}

// Точечный перехват: в туннель уходят только запросы к перечисленным
// резолверам, весь остальной DNS остаётся на прямом пути.
func TestArgsCapturesOnlyTheListedResolvers(t *testing.T) {
	cfg := config.Default()
	p := config.Profile{Remote: "user@host", DNSServers: []string{"10.73.16.4", "10.73.0.23"}}

	args := strings.Join(NewRunner(cfg.Sshuttle, io.Discard).Args(cfg, p), " ")

	if !strings.Contains(args, "--ns-hosts=10.73.16.4,10.73.0.23") {
		t.Errorf("в аргументах нет точечного перехвата: %s", args)
	}
	// --dns увёл бы в туннель вообще всё и обессмыслил бы список.
	if strings.Contains(args, " --dns") {
		t.Errorf("--dns не должен появляться рядом с dns_servers: %s", args)
	}
}

func TestArgsCapturesEverythingWithPlainDNS(t *testing.T) {
	cfg := config.Default()
	args := strings.Join(NewRunner(cfg.Sshuttle, io.Discard).Args(cfg, config.Profile{Remote: "user@host", DNS: true}), " ")

	if !strings.Contains(args, "--dns") {
		t.Errorf("ожидался --dns: %s", args)
	}
	if strings.Contains(args, "--ns-hosts") {
		t.Errorf("точечного перехвата не просили: %s", args)
	}
}

func TestArgsLeavesDNSAloneByDefault(t *testing.T) {
	cfg := config.Default()
	args := strings.Join(NewRunner(cfg.Sshuttle, io.Discard).Args(cfg, config.Profile{Remote: "user@host"}), " ")

	if strings.Contains(args, "--dns") || strings.Contains(args, "--ns-hosts") {
		t.Errorf("DNS трогать не просили: %s", args)
	}
}
