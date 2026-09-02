// Package shuttle запускает и контролирует процесс sshuttle.
package shuttle

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/tasticolly/splitr/internal/config"
)

// State — текущее состояние туннеля.
type State string

const (
	// StateDown — туннель не поднят.
	StateDown State = "down"
	// StateStarting — процесс запущен, но якорь pf ещё не появился.
	StateStarting State = "starting"
	// StateUp — туннель работает.
	StateUp State = "up"
	// StateFailed — последняя попытка подъёма завершилась ошибкой.
	StateFailed State = "failed"
)

// Runner владеет процессом sshuttle.
type Runner struct {
	sshuttlePath string
	log          io.Writer

	mu      sync.Mutex
	cmd     *exec.Cmd
	profile string
	state   State
	lastErr string
	since   time.Time
	exited  chan struct{}
	// action — то, чего туннель ждёт от человека (например, войти по ссылке
	// Tailscale). Живёт рядом с состоянием, потому что узнаётся из того же
	// вывода процесса и снимается вместе с ним.
	action ActionRequired
}

// NewRunner собирает Runner из настроек sshuttle.
func NewRunner(s config.Sshuttle, log io.Writer) *Runner {
	return &Runner{
		sshuttlePath: sshuttleBinary(s),
		log:          log,
		state:        StateDown,
	}
}

// sshuttleBinary возвращает путь к sshuttle, подставляя имя из PATH,
// если путь в конфиге не задан.
func sshuttleBinary(s config.Sshuttle) string {
	if s.Path == "" {
		return "sshuttle"
	}
	return s.Path
}

// Args собирает командную строку sshuttle для профиля.
//
// Настройки sshuttle берутся из переданного конфига, а не из полей раннера:
// раннер живёт всё время работы демона, а конфиг перечитывается на ходу.
// Раньше правка extra_args или ssh_options вступала в силу только после
// перезапуска демона, причём молча — конфиг показывал новое значение,
// а в командной строке оставалось старое.
func (r *Runner) Args(cfg config.Config, p config.Profile) []string {
	args := []string{"-r", p.Remote, "--ssh-cmd", sshCmd(cfg.Sshuttle, p)}
	if p.DNS {
		args = append(args, "--dns")
	}
	args = append(args, cfg.Sshuttle.ExtraArgs...)
	for _, ex := range cfg.ExcludedSubnets(p) {
		args = append(args, "-x", ex)
	}
	return append(args, cfg.RoutedSubnets(p)...)
}

// SSHCommand собирает вызов ssh для профиля.
//
// Тем же самым вызовом обязаны пользоваться все, кто ходит на удалённый хост:
// проверка достижимости голым `ssh`, без ключа и known_hosts профиля,
// упирается в «Permission denied» и врёт, что хост недоступен.
func SSHCommand(s config.Sshuttle, p config.Profile) []string {
	parts := []string{"ssh"}
	if p.SSHKey != "" {
		parts = append(parts, "-i", p.SSHKey)
	}
	if p.KnownHosts != "" {
		parts = append(parts, "-o", "UserKnownHostsFile="+p.KnownHosts)
	}
	for _, opt := range s.SSHOptions {
		parts = append(parts, "-o", opt)
	}
	return parts
}

func sshCmd(s config.Sshuttle, p config.Profile) string {
	return strings.Join(SSHCommand(s, p), " ")
}

// Start поднимает туннель. Возвращает ошибку, если он уже запущен.
func (r *Runner) Start(cfg config.Config, name string, p config.Profile) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cmd != nil && r.cmd.Process != nil && r.state != StateDown && r.state != StateFailed {
		return fmt.Errorf("the tunnel is already up (profile %s)", r.profile)
	}

	args := r.Args(cfg, p)
	// Путь запоминается для KillForeign: тот отличает свои процессы от чужих
	// по исполняемому файлу, и знать он должен актуальный.
	r.sshuttlePath = sshuttleBinary(cfg.Sshuttle)
	fmt.Fprintf(r.log, "[%s] starting: %s %s\n", time.Now().Format(time.RFC3339), r.sshuttlePath, strings.Join(args, " "))

	cmd := exec.Command(r.sshuttlePath, args...)
	// Вывод идёт в журнал как раньше и попутно читается построчно: только там
	// видно требование заново пройти аутентификацию, и без этого отказ
	// выглядит как молчание.
	out := &outputWatcher{dst: r.log, note: r.noteOutput}
	cmd.Stdout = out
	cmd.Stderr = out
	// Своя группа процессов, чтобы гасить sshuttle вместе с его ssh-потомками.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		r.state = StateFailed
		r.lastErr = err.Error()
		return fmt.Errorf("start sshuttle: %w", err)
	}

	r.cmd = cmd
	r.profile = name
	r.state = StateStarting
	r.since = time.Now()
	r.lastErr = ""
	// Новая попытка отменяет требование от прошлой: оно могло быть выполнено.
	r.action = ActionRequired{}
	exited := make(chan struct{})
	r.exited = exited

	go func() {
		err := cmd.Wait()
		// Последняя строка могла остаться без перевода строки — а именно ею
		// ssh и печатает ссылку на вход перед тем, как оборваться.
		out.flush()
		r.mu.Lock()
		if err != nil {
			r.state = StateFailed
			r.lastErr = err.Error()
		} else {
			r.state = StateDown
		}
		r.cmd = nil
		r.mu.Unlock()
		close(exited)
		fmt.Fprintf(r.log, "[%s] sshuttle exited: %v\n", time.Now().Format(time.RFC3339), err)
	}()
	return nil
}

// Stop гасит процесс: сначала мягко, затем принудительно.
func (r *Runner) Stop(ctx context.Context) error {
	r.mu.Lock()
	cmd, exited := r.cmd, r.exited
	// Туннель опускают намеренно — значит требование войти больше не висит,
	// и снять его нужно до всякого выхода из метода: процесс мог уже умереть,
	// а требование от него — остаться.
	r.action = ActionRequired{}
	r.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return nil
	}

	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		pgid = cmd.Process.Pid
	}
	_ = syscall.Kill(-pgid, syscall.SIGTERM)

	select {
	case <-exited:
		return nil
	case <-time.After(5 * time.Second):
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
	case <-ctx.Done():
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
	}
	select {
	case <-exited:
	case <-time.After(3 * time.Second):
		return fmt.Errorf("sshuttle did not exit after SIGKILL")
	}
	return nil
}

// Snapshot — наблюдаемое состояние туннеля.
type Snapshot struct {
	State     State
	Profile   string
	PID       int
	Since     time.Time
	LastError string
	// Action — что нужно сделать человеку, чтобы туннель поднялся.
	Action ActionRequired
}

// SetAction сообщает, что человек должен что-то сделать руками, чтобы
// туннель поднялся: например, заново войти по ссылке.
func (r *Runner) SetAction(a ActionRequired) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.action = a
}

// ClearAction снимает требование: до хоста снова можно дойти.
func (r *Runner) ClearAction() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.action = ActionRequired{}
}

// Snapshot возвращает наблюдаемое состояние туннеля.
func (r *Runner) Snapshot() Snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	snap := Snapshot{State: r.state, Profile: r.profile, Since: r.since, LastError: r.lastErr, Action: r.action}
	if r.cmd != nil && r.cmd.Process != nil {
		snap.PID = r.cmd.Process.Pid
	}
	return snap
}

// MarkUp переводит состояние в StateUp — вызывается сторожем, когда в pf появился якорь sshuttle.
func (r *Runner) MarkUp() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state == StateStarting {
		r.state = StateUp
	}
	// Туннель поднялся — значит войти уже не нужно, чем бы дело ни кончилось
	// в прошлый раз.
	r.action = ActionRequired{}
}

// noteOutput разбирает очередную строку вывода sshuttle.
func (r *Runner) noteOutput(line string) {
	url := DetectAuthURL(line)
	if url == "" {
		return
	}
	r.mu.Lock()
	already := r.action.URL == url
	r.action = AuthAction(url)
	r.mu.Unlock()
	if already {
		return
	}
	// Отдельная строка в журнале: иначе ссылку пришлось бы выискивать
	// в обычном выводе sshuttle.
	fmt.Fprintf(r.log, "[%s] the tunnel host requires re-authentication, open: %s\n",
		time.Now().Format(time.RFC3339), url)
}

// Running сообщает, жив ли управляемый процесс.
func (r *Runner) Running() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cmd != nil && r.cmd.Process != nil
}

// Швы для тестов: перебор и убийство процессов нельзя гонять по-настоящему
// на машине разработчика. Тесты лежат в этом же пакете и подменяют их.
var (
	listProcesses = func() (string, error) {
		out, err := exec.Command("/bin/ps", "-axo", "pid=,command=").Output()
		return string(out), err
	}
	killProcess  = syscall.Kill
	processGroup = syscall.Getpgid
)

// Running сообщает, есть ли в системе хоть один живой процесс sshuttle.
//
// Нужно, чтобы отличить работающий туннель от осиротевшего якоря pf:
// sshuttle, убитый по SIGKILL, оставляет свой якорь в pf, и по одному только
// имени якоря живой туннель от мусора не отличить.
func Running(binary string) (bool, error) {
	out, err := listProcesses()
	if err != nil {
		return false, fmt.Errorf("list processes: %w", err)
	}
	for _, line := range strings.Split(out, "\n") {
		pid, command, ok := parsePsLine(line)
		if !ok || pid == os.Getpid() {
			continue
		}
		if isSshuttleCommand(command, binary) {
			return true, nil
		}
	}
	return false, nil
}

// KillForeign гасит чужие процессы sshuttle, не трогая собственный туннель.
func (r *Runner) KillForeign() error {
	r.mu.Lock()
	ownPGID := 0
	if r.cmd != nil && r.cmd.Process != nil {
		if pgid, err := syscall.Getpgid(r.cmd.Process.Pid); err == nil {
			ownPGID = pgid
		}
	}
	binary := r.sshuttlePath
	r.mu.Unlock()
	return KillForeign(binary, ownPGID)
}

// KillForeign гасит sshuttle, запущенные мимо splitr: иначе они держат
// свои pf-якоря и незаметно снимают блокировку.
//
// Процессы из группы ownPGID считаются своими и не трогаются — sshuttle
// порождает потомков (ssh, вспомогательный процесс фаервола), и убийство их
// как «чужих» порвало бы наш же туннель. ownPGID = 0 означает «своих нет».
func KillForeign(binary string, ownPGID int) error {
	out, err := listProcesses()
	if err != nil {
		return fmt.Errorf("list processes: %w", err)
	}

	var killed []string
	for _, line := range strings.Split(out, "\n") {
		pid, command, ok := parsePsLine(line)
		if !ok || pid == os.Getpid() || !isSshuttleCommand(command, binary) {
			continue
		}
		if ownPGID != 0 {
			if pgid, err := processGroup(pid); err == nil && pgid == ownPGID {
				continue
			}
		}
		if err := killProcess(pid, syscall.SIGTERM); err == nil {
			killed = append(killed, strconv.Itoa(pid))
		}
	}
	if len(killed) > 0 {
		return fmt.Errorf("killed stray sshuttle processes: %s", strings.Join(killed, ", "))
	}
	return nil
}

func parsePsLine(line string) (pid int, command string, ok bool) {
	line = strings.TrimSpace(line)
	first, rest, found := strings.Cut(line, " ")
	if !found {
		return 0, "", false
	}
	pid, err := strconv.Atoi(first)
	if err != nil {
		return 0, "", false
	}
	return pid, strings.TrimSpace(rest), true
}

// isSshuttleCommand отличает настоящий sshuttle от чего угодно со словом
// «sshuttle» в командной строке — редактора с открытым конфигом, скрипта-обёртки,
// самого splitr. Смотрим только на исполняемый файл и на аргумент
// интерпретатора: sshuttle — это python-скрипт, и запускают его либо напрямую,
// либо как `python3 /путь/sshuttle`.
func isSshuttleCommand(command, binary string) bool {
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return false
	}
	candidates := fields[:1]
	if len(fields) > 1 && strings.Contains(filepath.Base(fields[0]), "python") {
		candidates = fields[:2]
	}
	for _, c := range candidates {
		base := filepath.Base(c)
		if base == "sshuttle" || (binary != "" && c == binary) {
			return true
		}
	}
	return false
}
