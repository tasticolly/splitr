//go:build docker_e2e

// Package e2e — сквозные тесты splitr, которые запускаются ВНУТРИ
// клиентского контейнера стенда (test/docker). На хосте они бесполезны:
// нужен root, подменённый /sbin/pfctl и вторая сеть за туннелем.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/tasticolly/splitr/internal/daemon"
)

const (
	binary     = "/usr/local/bin/splitr"
	configPath = "/etc/splitr/config.yaml"
	baseConfig = "/etc/splitr/config.base.yaml"
	socketPath = "/var/run/splitr.sock"
	tcpAddr    = "127.0.0.1:8787"

	pfctlStub  = "/sbin/pfctl"
	stubDir    = "/var/lib/pfstub"
	stubLog    = stubDir + "/calls.log"
	anchorFile = "/etc/pf.anchors/splitr"
	daemonLog  = "/var/log/splitr.log"

	targetAddr = "10.77.2.10:80"
	targetURL  = "http://10.77.2.10/"
	targetMark = "SPLITR-E2E-TARGET-OK"
	remoteAddr = "10.77.1.20:22"

	backNet  = "10.77.2.0/24"
	frontNet = "10.77.1.0/24"
)

// daemonProc — запущенный `splitr daemon`.
type daemonProc struct {
	cmd *exec.Cmd
	out *bytes.Buffer
}

var running *daemonProc

func TestMain(m *testing.M) {
	if os.Getenv("SPLITR_E2E") != "1" {
		fmt.Fprintln(os.Stderr, "e2e-тесты запускаются только внутри контейнера стенда (SPLITR_E2E=1)")
		os.Exit(1)
	}
	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "e2e-тесты требуют root внутри контейнера")
		os.Exit(1)
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "не читается конфиг стенда:", err)
		os.Exit(1)
	}
	if err := os.WriteFile(baseConfig, raw, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "не сохраняется эталонный конфиг:", err)
		os.Exit(1)
	}

	code := m.Run()

	stopDaemonQuiet()
	killAllSshuttle()
	os.Exit(code)
}

// --- управление демоном ------------------------------------------------------

// resetStand возвращает стенд в исходное состояние: демон перезапущен с
// эталонным конфигом, состояние стаба pfctl очищено, туннеля нет.
func resetStand(t *testing.T) {
	t.Helper()
	stopDaemonQuiet()
	killAllSshuttle()
	restoreConfig(t)
	resetStub(t)
	startDaemon(t)
}

func restoreConfig(t *testing.T) {
	t.Helper()
	raw, err := os.ReadFile(baseConfig)
	if err != nil {
		t.Fatalf("не прочитан эталонный конфиг %s: %v", baseConfig, err)
	}
	if err := os.WriteFile(configPath, raw, 0o644); err != nil {
		t.Fatalf("не восстановлен конфиг %s: %v", configPath, err)
	}
}

// resetStub стирает состояние стаба pfctl: pf «выключен», правил нет.
func resetStub(t *testing.T) {
	t.Helper()
	if err := os.RemoveAll(stubDir); err != nil {
		t.Fatalf("не очищен каталог стаба pfctl %s: %v", stubDir, err)
	}
	if err := os.MkdirAll(stubDir+"/anchors", 0o755); err != nil {
		t.Fatalf("не создан каталог стаба pfctl: %v", err)
	}
	_ = os.Remove(anchorFile)
}

func startDaemon(t *testing.T) {
	t.Helper()
	if running != nil {
		t.Fatalf("демон уже запущен — тест должен был сначала его остановить")
	}
	buf := &bytes.Buffer{}
	cmd := exec.Command(binary, "daemon", "--config", configPath)
	cmd.Stdout = buf
	cmd.Stderr = buf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("не запустился демон: %v", err)
	}
	running = &daemonProc{cmd: cmd, out: buf}

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if _, _, err := request(unixClient(), http.MethodGet, "/status", nil); err == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("демон не отозвался на %s за 20с; вывод демона:\n%s", socketPath, buf.String())
}

func stopDaemonQuiet() {
	if running == nil {
		return
	}
	pgid, err := syscall.Getpgid(running.cmd.Process.Pid)
	if err != nil {
		pgid = running.cmd.Process.Pid
	}
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	done := make(chan struct{})
	go func() { _ = running.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(8 * time.Second):
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		<-done
	}
	running = nil
	_ = os.Remove(socketPath)
}

// daemonOutput — то, что демон написал в свой лог с момента старта.
func daemonOutput() string {
	if running == nil {
		return ""
	}
	return running.out.String()
}

// --- HTTP-клиенты ------------------------------------------------------------

func unixClient() *http.Client {
	return &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
			},
		},
	}
}

func tcpClient() *http.Client {
	return &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "tcp", tcpAddr)
			},
		},
	}
}

// request выполняет запрос к API демона и возвращает код и тело.
func request(c *http.Client, method, path string, body any) (int, []byte, error) {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return 0, nil, err
		}
	}
	// Адрес настоящий, а не выдуманный: демон проверяет заголовок Host,
	// чтобы чужая веб-страница не могла дотянуться до API через перепривязку
	// DNS. Транспорт клиента всё равно ведёт куда надо — в сокет или в TCP.
	req, err := http.NewRequest(method, "http://"+tcpAddr+path, bytes.NewReader(payload))
	if err != nil {
		return 0, nil, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	return resp.StatusCode, data, err
}

// mustRequest падает, если запрос не выполнился или код ответа не тот.
func mustRequest(t *testing.T, c *http.Client, method, path string, body any, wantCode int) []byte {
	t.Helper()
	code, data, err := request(c, method, path, body)
	if err != nil {
		t.Fatalf("%s %s: запрос не выполнился: %v", method, path, err)
	}
	if code != wantCode {
		t.Fatalf("%s %s: код ответа %d, ожидался %d; тело: %s", method, path, code, wantCode, data)
	}
	return data
}

func status(t *testing.T) daemon.Status {
	t.Helper()
	return statusVia(t, unixClient())
}

func statusVia(t *testing.T, c *http.Client) daemon.Status {
	t.Helper()
	data := mustRequest(t, c, http.MethodGet, "/status", nil, http.StatusOK)
	var st daemon.Status
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatalf("статус не разобран как JSON: %v; тело: %s", err, data)
	}
	return st
}

// waitStatus ждёт, пока статус демона удовлетворит условию.
func waitStatus(t *testing.T, what string, timeout time.Duration, cond func(daemon.Status) bool) daemon.Status {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last daemon.Status
	for time.Now().Before(deadline) {
		last = status(t)
		if cond(last) {
			return last
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("не дождались за %s: %s\nпоследний статус: %s\nлог демона:\n%s",
		timeout, what, mustJSON(last), tailFile(daemonLog, 30))
	return last
}

func mustJSON(v any) string {
	data, _ := json.MarshalIndent(v, "", "  ")
	return string(data)
}

// --- сеть --------------------------------------------------------------------

// fetchTarget тянет страницу с target. Ошибка означает, что пути до net_back нет.
func fetchTarget(timeout time.Duration) (string, error) {
	c := &http.Client{Timeout: timeout}
	resp, err := c.Get(targetURL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	return string(data), err
}

// targetReachable сообщает, отвечает ли target ожидаемой строкой.
func targetReachable(timeout time.Duration) bool {
	body, err := fetchTarget(timeout)
	return err == nil && strings.Contains(body, targetMark)
}

// waitTarget ждёт, пока target станет доступен (или перестанет).
func waitTarget(t *testing.T, want bool, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if targetReachable(2*time.Second) == want {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	if want {
		body, err := fetchTarget(3 * time.Second)
		t.Fatalf("target %s должен был стать доступен через туннель за %s, но не стал (ответ %q, ошибка %v)",
			targetURL, timeout, body, err)
	}
	t.Fatalf("target %s должен был стать недоступен за %s, но всё ещё отвечает", targetURL, timeout)
}

func dialable(addr string, timeout time.Duration) bool {
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// --- процессы ----------------------------------------------------------------

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	_, err := os.Stat(fmt.Sprintf("/proc/%d", pid))
	return err == nil
}

func cmdline(pid int) string {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(strings.ReplaceAll(string(raw), "\x00", " "))
}

// killTunnel жёстко убивает процесс sshuttle снаружи — как это сделал бы
// пользователь или OOM killer. Бьём по группе, иначе выживают потомки sshuttle.
func killTunnel(t *testing.T, pid int) {
	t.Helper()
	pgid, err := syscall.Getpgid(pid)
	if err != nil {
		pgid = pid
	}
	if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil {
		t.Fatalf("не удалось убить группу процессов sshuttle (pgid %d): %v", pgid, err)
	}
}

func killAllSshuttle() {
	_ = exec.Command("/usr/bin/pkill", "-9", "-f", "sshuttle").Run()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if exec.Command("/usr/bin/pgrep", "-f", "sshuttle").Run() != nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func sshuttleRunning() bool {
	return exec.Command("/usr/bin/pgrep", "-f", "sshuttle").Run() == nil
}

// --- стаб pfctl --------------------------------------------------------------

// pfctl зовёт стаб напрямую — так тест эмулирует чужие действия с pf.
func pfctl(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command(pfctlStub, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("стаб pfctl %v: %v; вывод: %s", args, err, out)
	}
	return string(out)
}

// stubCalls возвращает журнал вызовов pfctl целиком.
func stubCalls(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(stubLog)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("не прочитан журнал вызовов стаба %s: %v", stubLog, err)
	}
	var lines []string
	for _, l := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
		if l != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

// stubCallsSince возвращает вызовы pfctl, случившиеся после отметки n.
func stubCallsSince(t *testing.T, n int) []string {
	t.Helper()
	all := stubCalls(t)
	if n > len(all) {
		return nil
	}
	return all[n:]
}

func containsCall(calls []string, substr string) bool {
	for _, c := range calls {
		if strings.Contains(c, substr) {
			return true
		}
	}
	return false
}

func tailFile(path string, n int) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "(нет файла " + path + ")"
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// --- конфиг ------------------------------------------------------------------

// writeConfig заменяет конфиг на диске (эталон остаётся в baseConfig).
func writeConfig(t *testing.T, body string) {
	t.Helper()
	if err := os.WriteFile(configPath, []byte(body), 0o644); err != nil {
		t.Fatalf("не записан конфиг %s: %v", configPath, err)
	}
}

// patchedConfig возвращает эталонный конфиг с заменой одной подстроки.
func patchedConfig(t *testing.T, old, new string) string {
	t.Helper()
	raw, err := os.ReadFile(baseConfig)
	if err != nil {
		t.Fatalf("не прочитан эталонный конфиг: %v", err)
	}
	s := string(raw)
	if !strings.Contains(s, old) {
		t.Fatalf("в эталонном конфиге нет подстроки %q — тест устарел", old)
	}
	return strings.Replace(s, old, new, 1)
}

// --- CLI ---------------------------------------------------------------------

// splitr запускает CLI ровно так, как это делает пользователь.
func splitr(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(binary, args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func mustCLI(t *testing.T, args ...string) string {
	t.Helper()
	out, err := splitr(t, args...)
	if err != nil {
		t.Fatalf("splitr %s: %v\nвывод:\n%s", strings.Join(args, " "), err, out)
	}
	return out
}
