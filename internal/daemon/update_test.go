package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tasticolly/splitr/internal/config"
	"github.com/tasticolly/splitr/internal/shuttle"
	"github.com/tasticolly/splitr/internal/update"
)

// updateDaemon собирает демона, у которого обновление целиком заменено швами:
// настоящее обновление подменяет бинарь в /usr/local/bin и перезапускает
// процесс — в тестах ни того, ни другого делать нельзя.
func updateDaemon(t *testing.T, latest string) (*Daemon, *fakeTunnel, string) {
	t.Helper()

	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "bin"), 0o755); err != nil {
		t.Fatalf("создать bin: %v", err)
	}

	cfg := testConfig()
	cfg.Update.RepoPath = repo
	cfg.Update.CheckInterval = time.Hour

	d, _, tun, _ := newTestDaemon(t, cfg)
	d.version = "v0.1.0"
	d.installedPath = filepath.Join(t.TempDir(), "splitr")
	d.restartDelay = time.Millisecond
	d.checkUpdate = func(repoPath, installed string) update.State {
		st := update.State{Installed: installed, RepoPath: repoPath, Latest: latest}
		newer, ok := update.Newer(latest, installed)
		if !ok || !newer {
			st.Reason = "nothing newer in " + repoPath
			return st
		}
		st.Available = true
		return st
	}
	d.binaryVersion = func(string) (string, error) { return latest, nil }
	d.finishInstall = func(string) error { return nil }
	d.exit = func(int) {}

	// Установленный бинарь должен существовать и быть старее нового:
	// иначе обновление откажется работать по времени правки.
	if err := os.WriteFile(d.installedPath, []byte("#!/bin/sh\necho splitr v0.1.0\n"), 0o755); err != nil {
		t.Fatalf("записать установленный бинарь: %v", err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(d.installedPath, old, old); err != nil {
		t.Fatalf("состарить установленный бинарь: %v", err)
	}
	return d, tun, repo
}

// buildBinary кладёт в репозиторий «собранный» бинарь.
func buildBinary(t *testing.T, repo string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(repo, "bin", "splitr")
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho splitr v0.2.0\n"), mode); err != nil {
		t.Fatalf("записать бинарь: %v", err)
	}
	return path
}

func TestUpdateStateIsCached(t *testing.T) {
	t.Parallel()

	cfg := testConfig()
	cfg.Update.RepoPath = t.TempDir()
	cfg.Update.CheckInterval = time.Hour

	d, _, _, _ := newTestDaemon(t, cfg)
	var mu sync.Mutex
	calls := 0
	d.checkUpdate = func(repoPath, installed string) update.State {
		mu.Lock()
		defer mu.Unlock()
		calls++
		return update.State{Installed: installed, RepoPath: repoPath, Latest: "v0.2.0", Available: true}
	}

	for range 5 {
		if !d.UpdateState().Available {
			t.Fatal("состояние обновления потерялось")
		}
	}
	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 1 {
		t.Fatalf("git спрошен %d раз, ожидался один: интервал проверки не соблюдается", got)
	}

	// Установка обязана смотреть на репозиторий заново: между проверкой
	// и нажатием кнопки там могли собрать другую версию.
	if !d.RefreshUpdate().Available {
		t.Fatal("принудительная проверка потеряла состояние")
	}
	mu.Lock()
	got = calls
	mu.Unlock()
	if got != 2 {
		t.Fatalf("принудительная проверка не сходила в git: вызовов %d", got)
	}
}

func TestStatusCarriesUpdateState(t *testing.T) {
	t.Parallel()

	d, _, repo := updateDaemon(t, "v0.2.0")
	st := d.Status()
	if !st.Update.Available || st.Update.Latest != "v0.2.0" || st.Update.RepoPath != repo {
		t.Fatalf("update в статусе = %+v", st.Update)
	}
	if st.Update.Installed != "v0.1.0" {
		t.Fatalf("установленная версия в статусе = %q", st.Update.Installed)
	}
}

func TestAPIUpdateStateOverAnyTransport(t *testing.T) {
	t.Parallel()

	d, _, _ := updateDaemon(t, "v0.2.0")

	rec := do(t, d, http.MethodGet, "/update", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("код = %d, ожидался 200", rec.Code)
	}
	var st update.State
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("разобрать ответ %q: %v", rec.Body.String(), err)
	}
	if !st.Available || st.Latest != "v0.2.0" {
		t.Fatalf("состояние = %+v", st)
	}
}

// Установка — это подмена кода, который launchd запустит от root,
// поэтому по TCP она запрещена так же, как запись конфига.
func TestAPIUpdateInstallRejectedOverTCP(t *testing.T) {
	t.Parallel()

	d, _, repo := updateDaemon(t, "v0.2.0")
	buildBinary(t, repo, 0o755)

	rec := do(t, d, http.MethodPost, "/update", "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("код = %d, ожидался 403", rec.Code)
	}
	if _, err := os.Stat(d.installedPath); err != nil {
		t.Fatalf("установленный бинарь пропал: %v", err)
	}
	if data, _ := os.ReadFile(d.installedPath); strings.Contains(string(data), "v0.2.0") {
		t.Fatal("бинарь подменён по непривилегированному транспорту")
	}
}

// privilegedPost повторяет обращение через управляющий сокет.
func privilegedPost(t *testing.T, d *Daemon, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	markTransport(d.routes(), true).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, nil))
	return rec
}

func TestAPIUpdateInstall(t *testing.T) {
	t.Parallel()

	d, _, repo := updateDaemon(t, "v0.2.0")
	buildBinary(t, repo, 0o755)

	exited := make(chan int, 1)
	d.exit = func(code int) { exited <- code }

	rec := privilegedPost(t, d, "/update")
	if rec.Code != http.StatusOK {
		t.Fatalf("код = %d (%s)", rec.Code, rec.Body.String())
	}
	var res struct {
		Installed  string `json:"installed"`
		Restarting bool   `json:"restarting"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("разобрать ответ %q: %v", rec.Body.String(), err)
	}
	if res.Installed != "v0.2.0" || !res.Restarting {
		t.Fatalf("ответ = %+v", res)
	}

	data, err := os.ReadFile(d.installedPath)
	if err != nil {
		t.Fatalf("прочитать установленный бинарь: %v", err)
	}
	if !strings.Contains(string(data), "v0.2.0") {
		t.Fatalf("бинарь не подменён: %q", data)
	}

	// Выход обязан случиться ПОСЛЕ ответа, иначе клиент увидит обрыв.
	select {
	case code := <-exited:
		if code != 0 {
			t.Fatalf("код выхода = %d", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("демон не перезапустился после обновления")
	}
}

func TestApplyUpdateRefusals(t *testing.T) {
	t.Parallel()

	t.Run("бинаря в репозитории нет", func(t *testing.T) {
		t.Parallel()
		d, _, _ := updateDaemon(t, "v0.2.0")
		_, err := d.ApplyUpdate()
		if err == nil || !strings.Contains(err.Error(), "make update") {
			t.Fatalf("ошибка = %v, ожидался совет собрать бинарь", err)
		}
	})

	t.Run("бинарь не исполняемый", func(t *testing.T) {
		t.Parallel()
		d, _, repo := updateDaemon(t, "v0.2.0")
		buildBinary(t, repo, 0o644)
		_, err := d.ApplyUpdate()
		if err == nil || !strings.Contains(err.Error(), "not an executable file") {
			t.Fatalf("ошибка = %v", err)
		}
	})

	t.Run("бинарь старее установленного", func(t *testing.T) {
		t.Parallel()
		d, _, repo := updateDaemon(t, "v0.2.0")
		src := buildBinary(t, repo, 0o755)
		old := time.Now().Add(-2 * time.Hour)
		if err := os.Chtimes(src, old, old); err != nil {
			t.Fatalf("состарить бинарь: %v", err)
		}
		_, err := d.ApplyUpdate()
		if err == nil || !strings.Contains(err.Error(), "older than the installed") {
			t.Fatalf("ошибка = %v", err)
		}
	})

	t.Run("бинарь не запускается", func(t *testing.T) {
		t.Parallel()
		d, _, repo := updateDaemon(t, "v0.2.0")
		buildBinary(t, repo, 0o755)
		d.binaryVersion = func(string) (string, error) { return "", os.ErrPermission }
		_, err := d.ApplyUpdate()
		if err == nil || !strings.Contains(err.Error(), "does not run") {
			t.Fatalf("ошибка = %v", err)
		}
		if data, _ := os.ReadFile(d.installedPath); strings.Contains(string(data), "v0.2.0") {
			t.Fatal("неработающий бинарь всё-таки поставлен")
		}
	})

	t.Run("версия бинаря не совпадает с тегом", func(t *testing.T) {
		t.Parallel()
		d, _, repo := updateDaemon(t, "v0.2.0")
		buildBinary(t, repo, 0o755)
		d.binaryVersion = func(string) (string, error) { return "v0.1.9", nil }
		_, err := d.ApplyUpdate()
		if err == nil || !strings.Contains(err.Error(), "reports version v0.1.9") {
			t.Fatalf("ошибка = %v", err)
		}
		if data, _ := os.ReadFile(d.installedPath); strings.Contains(string(data), "v0.2.0") {
			t.Fatal("бинарь с чужой версией всё-таки поставлен")
		}
	})

	t.Run("обновляться не на что", func(t *testing.T) {
		t.Parallel()
		d, _, repo := updateDaemon(t, "v0.1.0")
		buildBinary(t, repo, 0o755)
		_, err := d.ApplyUpdate()
		if err == nil || !strings.Contains(err.Error(), "nothing to install") {
			t.Fatalf("ошибка = %v", err)
		}
	})
}

// Обновление недоступно при пустом repo_path, и это не ошибка.
func TestUpdateUnavailableWithoutRepo(t *testing.T) {
	t.Parallel()

	cfg := testConfig()
	cfg.Update = config.Update{CheckInterval: time.Hour}
	d, _, _, _ := newTestDaemon(t, cfg)
	d.version = "v0.1.0"

	st := d.UpdateState()
	if st.Available || !strings.Contains(st.Reason, "update.repo_path is not set") {
		t.Fatalf("состояние = %+v", st)
	}
	rec := do(t, d, http.MethodGet, "/update", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("код = %d: отсутствие repo_path — не ошибка", rec.Code)
	}
}

// Требование войти по ссылке обязано доходить до статуса: без этого отказ
// выглядит молчаливым, и человек не знает, что делать.
func TestStatusReportsActionRequired(t *testing.T) {
	t.Parallel()

	d, tun, _ := updateDaemon(t, "v0.1.0")
	if st := d.Status(); st.ActionRequired != nil {
		t.Fatalf("требование взялось из ниоткуда: %+v", st.ActionRequired)
	}

	tun.setSnapshot(shuttle.Snapshot{
		State: shuttle.StateFailed,
		Action: shuttle.ActionRequired{
			Kind:    "auth",
			Message: "the tunnel host requires re-authentication",
			URL:     "https://login.tailscale.com/a/abc123",
		},
	})
	st := d.Status()
	if st.ActionRequired == nil || st.ActionRequired.URL != "https://login.tailscale.com/a/abc123" {
		t.Fatalf("требование не попало в статус: %+v", st.ActionRequired)
	}

	// В JSON поля быть не должно, когда ничего не требуется: иначе приложению
	// пришлось бы отличать пустую структуру от отсутствия требования.
	tun.setSnapshot(shuttle.Snapshot{State: shuttle.StateUp})
	raw, err := MarshalStatus(d.Status())
	if err != nil {
		t.Fatalf("сериализовать статус: %v", err)
	}
	if strings.Contains(string(raw), "action_required") {
		t.Fatalf("пустое требование попало в JSON: %s", raw)
	}
}

// Регрессия на подсказку, которая заворачивала в бесконечный круг: сборке из
// коммита после тега советовали пересобраться, что версию не меняет.
func TestAheadOfTag(t *testing.T) {
	tests := []struct {
		version, tag string
		want         bool
	}{
		{"v0.5.1-3-g321a2e2", "v0.5.1", true},
		{"v0.5.1-3-g321a2e2-dirty", "v0.5.1", true},
		// Ровно на теге — совпадение, а не «впереди».
		{"v0.5.1", "v0.5.1", false},
		// Бинарь от прошлого релиза: пересборка тут как раз поможет.
		{"v0.5.0", "v0.5.1", false},
		{"dev", "v0.5.1", false},
		{"v0.5.1-3-g321a2e2", "", false},
	}
	for _, tt := range tests {
		if got := aheadOfTag(tt.version, tt.tag); got != tt.want {
			t.Errorf("aheadOfTag(%q, %q) = %v, ожидалось %v", tt.version, tt.tag, got, tt.want)
		}
	}
}
