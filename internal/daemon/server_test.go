package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tasticolly/splitr/internal/config"
	"github.com/tasticolly/splitr/internal/pfctl/pftest"
	"github.com/tasticolly/splitr/internal/protect"
)

// do отправляет запрос в маршрутизатор демона и отдаёт ответ.
func do(t *testing.T, d *Daemon, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	rec := httptest.NewRecorder()
	d.routes().ServeHTTP(rec, req)
	return rec
}

// decodeStatus разбирает тело ответа как Status.
func decodeStatus(t *testing.T, rec *httptest.ResponseRecorder) Status {
	t.Helper()
	var st Status
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("разобрать статус из %q: %v", rec.Body.String(), err)
	}
	return st
}

// errorMessage достаёт текст ошибки из ответа API.
func errorMessage(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var payload map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("разобрать ошибку из %q: %v", rec.Body.String(), err)
	}
	return payload["error"]
}

func TestAPIStatus(t *testing.T) {
	t.Parallel()

	d, pf, _, _ := newTestDaemon(t, testConfig())
	installed(t, d, pf)

	rec := do(t, d, http.MethodGet, "/status", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("код = %d, ожидался 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("Content-Type = %q", ct)
	}
	st := decodeStatus(t, rec)
	if st.Protection != "all" || !st.Blocking {
		t.Fatalf("статус = %+v, ожидался работающий Ð·Ð°ÑÐ¸ÑÐ°", st)
	}
}

func TestAPIConfig(t *testing.T) {
	t.Parallel()

	d, _, _, _ := newTestDaemon(t, testConfig())

	rec := do(t, d, http.MethodGet, "/config", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("код = %d, ожидался 200", rec.Code)
	}
	var cfg config.Config
	if err := json.Unmarshal(rec.Body.Bytes(), &cfg); err != nil {
		t.Fatalf("разобрать конфиг: %v", err)
	}
	if cfg.DefaultProfile != "alpha" || len(cfg.Profiles) != 2 {
		t.Fatalf("конфиг отдан неверно: %+v", cfg)
	}
}

func TestAPIRulesReturnsPlainText(t *testing.T) {
	t.Parallel()

	d, _, _, _ := newTestDaemon(t, testConfig())

	rec := do(t, d, http.MethodGet, "/rules", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("код = %d, ожидался 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/plain") {
		t.Fatalf("Content-Type = %q, ожидался text/plain", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "block drop out on ! lo0") || !strings.Contains(body, "table <splitr_block>") {
		t.Fatalf("в правилах нет блокировки:\n%s", body)
	}
}

func TestAPIRulesReportsRulesetError(t *testing.T) {
	t.Parallel()

	d, _, _, _ := newTestDaemon(t, testConfig())
	d.activeName = "нет-такого"

	rec := do(t, d, http.MethodGet, "/rules", "")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("код = %d, ожидался 500", rec.Code)
	}
	if !strings.Contains(errorMessage(t, rec), "unknown profile") {
		t.Fatalf("текст ошибки = %q", errorMessage(t, rec))
	}
}

func TestAPIUpWithUnknownProfile(t *testing.T) {
	t.Parallel()

	d, _, tun, _ := newTestDaemon(t, testConfig())

	rec := do(t, d, http.MethodPost, "/up", `{"profile":"нет-такого"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("код = %d, ожидался 500", rec.Code)
	}
	if !strings.Contains(errorMessage(t, rec), "unknown profile") {
		t.Fatalf("текст ошибки = %q", errorMessage(t, rec))
	}
	if starts, _, _, _ := tun.counters(); len(starts) != 0 {
		t.Fatalf("туннель поднимать было нельзя: %v", starts)
	}
}

// Пустое тело запроса означает профиль по умолчанию, а не ошибку разбора.
func TestAPIUpAcceptsEmptyBody(t *testing.T) {
	requireNonRoot(t)
	t.Parallel()

	d, _, _, ops := newTestDaemon(t, testConfig())

	rec := do(t, d, http.MethodPost, "/up", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("код = %d, ожидался 200: %s", rec.Code, rec.Body.String())
	}
	if _, _, flushes := ops.snapshot(); flushes != 0 {
		t.Fatalf("dns.flush_cache в тестовом конфиге выключен, сбросов %d", flushes)
	}
	if got := d.Status().Profile; got != "alpha" {
		t.Fatalf("активный профиль = %q, ожидался alpha", got)
	}
}

func TestAPIDown(t *testing.T) {
	t.Parallel()

	d, pf, tun, _ := newTestDaemon(t, testConfig())
	installed(t, d, pf)

	rec := do(t, d, http.MethodPost, "/down", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("код = %d, тело %s", rec.Code, rec.Body.String())
	}
	if _, stops, _, _ := tun.counters(); stops != 1 {
		t.Fatalf("Stop вызван %d раз", stops)
	}
	if st := decodeStatus(t, rec); st.Protection != "all" {
		t.Fatalf("после Down Ð·Ð°ÑÐ¸ÑÐ° обязан остаться включённым: %+v", st)
	}
}

func TestAPIDownReportsError(t *testing.T) {
	t.Parallel()

	d, _, tun, _ := newTestDaemon(t, testConfig())
	tun.stopErr = errors.New("процесс не гаснет")

	rec := do(t, d, http.MethodPost, "/down", "")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("код = %d, ожидался 500", rec.Code)
	}
	if !strings.Contains(errorMessage(t, rec), "процесс не гаснет") {
		t.Fatalf("текст ошибки = %q", errorMessage(t, rec))
	}
}

func TestAPIProtectionRejectsBadInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		body    string
		wantMsg string
	}{
		{name: "неизвестный режим", body: `{"mode":"иногда"}`, wantMsg: "mode must be on|off|strict"},
		{name: "пустой режим", body: `{"mode":""}`, wantMsg: "mode must be on|off|strict"},
		{name: "не JSON", body: `{`, wantMsg: "parse request"},
		{name: "пустое тело", body: "", wantMsg: "parse request"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d, _, _, _ := newTestDaemon(t, testConfig())

			rec := do(t, d, http.MethodPost, "/protect", tt.body)
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("код = %d, ожидался 500", rec.Code)
			}
			if !strings.Contains(errorMessage(t, rec), tt.wantMsg) {
				t.Fatalf("текст ошибки = %q, ожидалось %q", errorMessage(t, rec), tt.wantMsg)
			}
			if d.Status().Protection != "all" {
				t.Fatal("режим не должен меняться после некорректного запроса")
			}
		})
	}
}

// Регистр в mode не важен: CLI и UI шлют его по-разному.
//
// Проверяется только strict: ветки on и off зовут SetStrict и SetEnabled
// подряд, а SetStrict здесь спотыкается о недоступный на запись
// protect.AnchorFile, и до второго вызова дело не доходит. Сами
// переключатели покрыты напрямую в TestSetEnabledTogglesProtection.
func TestAPIProtectionStrictMode(t *testing.T) {
	requireNonRoot(t)
	t.Parallel()

	d, pf, _, _ := newTestDaemon(t, testConfig())
	installed(t, d, pf)

	do(t, d, http.MethodPost, "/protect", `{"mode":"STRICT"}`)

	if got := d.Status().Protection; got != "strict" {
		t.Fatalf("Protection = %q, ожидалось strict", got)
	}
}

func TestAPIReloadReportsBrokenConfig(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("неизвестное_поле: 1\n"), 0o600); err != nil {
		t.Fatalf("записать конфиг: %v", err)
	}
	d := NewWithDeps(testConfig(), path, io.Discard, pftest.New(), newFakeTunnel(), &fakeOps{})

	rec := do(t, d, http.MethodPost, "/reload", "")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("код = %d, ожидался 500", rec.Code)
	}
	if !strings.Contains(errorMessage(t, rec), "parse config") {
		t.Fatalf("текст ошибки = %q", errorMessage(t, rec))
	}
}

// Probe начинается с контрольного адреса: пока он недостижим, проверять
// блокировку бессмысленно. Контрольным здесь служит закрытый порт на loopback,
// поэтому наружу тест не ходит.
func TestProbeStopsWhenControlAddressIsUnreachable(t *testing.T) {
	t.Parallel()

	cfg := testConfig()
	cfg.Daemon.ProbeControl = "127.0.0.1:1"
	d, _, _, _ := newTestDaemon(t, cfg)

	report := d.Probe(t.Context())
	if !report.Inconclusive {
		t.Fatalf("отчёт = %+v, ожидался вердикт «проверить нельзя»", report)
	}
	if report.Control.Reachable {
		t.Fatal("закрытый порт не должен считаться достижимым")
	}
	if len(report.Blocked) != 0 {
		t.Fatalf("до защищаемых адресов дело доходить не должно: %+v", report.Blocked)
	}
	if !strings.Contains(report.Verdict, "127.0.0.1:1") {
		t.Fatalf("вердикт = %q, ожидалось упоминание контрольного адреса", report.Verdict)
	}
}

// probeAddresses предпочитает одиночные хосты: за /32 стоит живая машина,
// а за адресом сети — обычно никто.
func TestProbeAddressesPrefersSingleHosts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		block []string
		limit int
		want  string
	}{
		{
			name:  "хосты идут раньше сетей",
			block: []string{"10.0.0.0/9", "198.51.100.10/32", "203.0.113.0/24"},
			limit: 3,
			want:  "198.51.100.10:443,10.0.0.0:443,203.0.113.0:443",
		},
		{
			name:  "лимит обрезает список",
			block: []string{"10.0.0.0/9", "11.0.0.0/8", "172.16.0.0/12"},
			limit: 2,
			want:  "10.0.0.0:443,11.0.0.0:443",
		},
		{
			name:  "мусор пропускается",
			block: []string{"не-сеть", "10.0.0.0/9"},
			limit: 3,
			want:  "10.0.0.0:443",
		},
		{
			name:  "пустой список",
			block: nil,
			limit: 3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := probeAddresses(tt.block, tt.limit)
			if strings.Join(got, ",") != tt.want {
				t.Fatalf("probeAddresses = %v, ожидалось %q", got, tt.want)
			}
		})
	}
}

// Поток отброшенных пакетов доступен только при включённом журналировании.
func TestAPIBlockedStreamNeedsLogging(t *testing.T) {
	t.Parallel()

	d, _, _, _ := newTestDaemon(t, testConfig())

	rec := do(t, d, http.MethodGet, "/blocked", "")
	if rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("код = %d, ожидался 412", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "protection.log") {
		t.Fatalf("тело = %q, ожидалась подсказка про protect.log", rec.Body.String())
	}
}

func TestAPIIndexPage(t *testing.T) {
	t.Parallel()

	d, _, _, _ := newTestDaemon(t, testConfig())

	rec := do(t, d, http.MethodGet, "/", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("код = %d, ожидался 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("Content-Type = %q, ожидался text/html", ct)
	}
	if !strings.Contains(rec.Body.String(), "<html") && !strings.Contains(rec.Body.String(), "<!DOCTYPE") {
		t.Fatalf("ожидалась HTML-страница, получено:\n%.200s", rec.Body.String())
	}
}

func TestAPIUnknownPathIs404(t *testing.T) {
	t.Parallel()

	d, _, _, _ := newTestDaemon(t, testConfig())

	if rec := do(t, d, http.MethodGet, "/нет-такой-ручки", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("код = %d, ожидался 404", rec.Code)
	}
}

// Ручки различают метод: GET-статус не должен отвечать на POST и наоборот.
func TestAPIMethodMismatch(t *testing.T) {
	t.Parallel()

	d, _, _, _ := newTestDaemon(t, testConfig())

	tests := []struct {
		method, path string
	}{
		{http.MethodPost, "/status"},
		{http.MethodGet, "/up"},
		{http.MethodGet, "/down"},
		{http.MethodGet, "/protect"},
		{http.MethodGet, "/reload"},
	}
	for _, tt := range tests {
		rec := do(t, d, tt.method, tt.path, "")
		if rec.Code == http.StatusOK {
			t.Errorf("%s %s ответил 200, ожидался отказ", tt.method, tt.path)
		}
	}
}

// Serve без сокета и без HTTP-адреса просто ждёт отмены контекста.
func TestServeWithoutListenersWaitsForContext(t *testing.T) {
	t.Parallel()

	d, _, _, _ := newTestDaemon(t, testConfig())
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := d.Serve(ctx); err != nil {
		t.Fatalf("Serve: %v", err)
	}
}

// Анкор в статусе и правила в /rules обязаны совпадать: UI показывает именно их.
func TestAPIRulesMatchLoadedAnchor(t *testing.T) {
	t.Parallel()

	d, pf, _, _ := newTestDaemon(t, testConfig())
	installed(t, d, pf)

	rec := do(t, d, http.MethodGet, "/rules", "")
	if rec.Body.String() != pf.AnchorText(protect.Anchor) {
		t.Fatalf("правила из API расходятся с загруженными в якорь:\n%s\n---\n%s",
			rec.Body.String(), pf.AnchorText(protect.Anchor))
	}
}

// --- журнал и конфиг по HTTP ------------------------------------------------

func TestTailFile(t *testing.T) {
	t.Parallel()

	t.Run("файла нет", func(t *testing.T) {
		t.Parallel()
		got := string(tailFile(filepath.Join(t.TempDir(), "нет-файла"), 10))
		if !strings.Contains(got, "log unavailable") {
			t.Fatalf("получено %q", got)
		}
	})

	t.Run("файл короче запрошенного хвоста", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "log")
		if err := os.WriteFile(path, []byte("раз\nдва\nтри\n"), 0o600); err != nil {
			t.Fatalf("записать журнал: %v", err)
		}
		got := string(tailFile(path, 100))
		if !strings.Contains(got, "раз") || !strings.Contains(got, "три") {
			t.Fatalf("короткий журнал должен отдаваться целиком: %q", got)
		}
	})

	t.Run("отдаётся ровно хвост", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "log")
		var b strings.Builder
		for i := 0; i < 500; i++ {
			fmt.Fprintf(&b, "строка %d\n", i)
		}
		if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
			t.Fatalf("записать журнал: %v", err)
		}
		// Внимание: завершающий перевод строки tailFile считает отдельной
		// строкой, поэтому содержательных строк отдаётся на одну меньше
		// запрошенного, а при tail=1 хвост оказывается пустым.
		got := string(tailFile(path, 6))
		if n := strings.Count(strings.TrimSpace(got), "\n") + 1; n > 6 {
			t.Fatalf("строк %d, запрашивалось не больше шести: %q", n, got)
		}
		if !strings.Contains(got, "строка 499") {
			t.Fatalf("в хвосте нет последней строки: %q", got)
		}
		if strings.Contains(got, "строка 100") {
			t.Fatalf("в хвост попало лишнее: %q", got)
		}
	})
}

func TestAPILogReturnsTail(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "splitr.log")
	if err := os.WriteFile(path, []byte("первая\nвторая\n"), 0o600); err != nil {
		t.Fatalf("записать журнал: %v", err)
	}
	cfg := testConfig()
	cfg.Daemon.LogFile = path
	d, _, _, _ := newTestDaemon(t, cfg)

	rec := do(t, d, http.MethodGet, "/log?tail=1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("код = %d, ожидался 200", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != "вторая" {
		t.Fatalf("тело = %q, ожидалась последняя строка журнала", got)
	}
}

func TestAPIConfigRaw(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("# мой конфиг\n"), 0o600); err != nil {
		t.Fatalf("записать конфиг: %v", err)
	}
	d := NewWithDeps(testConfig(), path, io.Discard, pftest.New(), newFakeTunnel(), &fakeOps{})

	rec := do(t, d, http.MethodGet, "/config/raw", "")
	if rec.Code != http.StatusOK || rec.Body.String() != "# мой конфиг\n" {
		t.Fatalf("код %d, тело %q", rec.Code, rec.Body.String())
	}

	missing := NewWithDeps(testConfig(), filepath.Join(t.TempDir(), "нет-конфига"), io.Discard,
		pftest.New(), newFakeTunnel(), &fakeOps{})
	if rec := do(t, missing, http.MethodGet, "/config/raw", ""); rec.Code != http.StatusInternalServerError {
		t.Fatalf("код = %d, ожидался 500", rec.Code)
	}
}

// Конфиг задаёт путь к sshuttle, который демон запускает от root, поэтому
// переписать его можно только через управляющий сокет, но не по TCP.
func TestAPIConfigWriteRequiresPrivilegedTransport(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("profiles:\n  alpha:\n    remote: user@alpha\n"), 0o600); err != nil {
		t.Fatalf("записать конфиг: %v", err)
	}
	d := NewWithDeps(testConfig(), path, io.Discard, pftest.New(), newFakeTunnel(), &fakeOps{})

	rec := do(t, d, http.MethodPost, "/config", "subnets: []\n")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("код = %d, ожидался 403", rec.Code)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("прочитать конфиг: %v", err)
	}
	if strings.Contains(string(body), "subnets") {
		t.Fatalf("конфиг переписан по непривилегированному транспорту:\n%s", body)
	}
}

// Тот же запрос через управляющий сокет проходит.
func TestAPIConfigWriteOverPrivilegedTransport(t *testing.T) {
	requireNonRoot(t)
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("profiles:\n  alpha:\n    remote: user@alpha\n"), 0o600); err != nil {
		t.Fatalf("записать конфиг: %v", err)
	}
	d := NewWithDeps(testConfig(), path, io.Discard, pftest.New(), newFakeTunnel(), &fakeOps{})

	handler := markTransport(d.routes(), true)
	req := httptest.NewRequest(http.MethodPost, "/config",
		strings.NewReader("subnets:\n  - 11.0.0.0/8\nprofiles:\n  alpha:\n    remote: user@alpha\n"))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code == http.StatusForbidden {
		t.Fatal("через управляющий сокет запись конфига должна быть разрешена")
	}
	if strings.Join(d.Config().Subnets, ",") != "11.0.0.0/8" {
		t.Fatalf("конфиг не применён: %v", d.Config().Subnets)
	}
}

// Транспорт по умолчанию непривилегированный: забытая пометка не должна
// случайно открывать запись конфига.
func TestPrivilegedRequestDefaultsToFalse(t *testing.T) {
	t.Parallel()

	if privilegedRequest(httptest.NewRequest(http.MethodGet, "/", nil)) {
		t.Fatal("непомеченный запрос не может считаться привилегированным")
	}
	var got bool
	markTransport(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = privilegedRequest(r)
	}), true).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	if !got {
		t.Fatal("помеченный запрос обязан считаться привилегированным")
	}
}
