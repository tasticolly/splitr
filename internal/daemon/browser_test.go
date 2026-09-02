package daemon

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func guardedServer(t *testing.T) http.Handler {
	t.Helper()
	reached := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("дошло"))
	})
	return guardBrowser(reached, localHosts("127.0.0.1:8787"), func(string, ...any) {})
}

func guarded(t *testing.T, h http.Handler, host string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/protect", strings.NewReader(`{"mode":"off"}`))
	req.Host = host
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// CLI, menu bar и curl не шлют браузерных заголовков и обязаны работать
// как раньше.
func TestGuardAllowsPlainLocalRequest(t *testing.T) {
	t.Parallel()

	if code := guarded(t, guardedServer(t), "127.0.0.1:8787", nil).Code; code != http.StatusOK {
		t.Fatalf("код %d, обычный локальный запрос обязан проходить", code)
	}
}

// Своя же страница веб-интерфейса.
func TestGuardAllowsOwnPage(t *testing.T) {
	t.Parallel()

	tests := []map[string]string{
		{"Sec-Fetch-Site": "same-origin", "Origin": "http://127.0.0.1:8787"},
		{"Sec-Fetch-Site": "none"},
		{"Origin": "http://localhost:8787"},
	}
	for _, headers := range tests {
		if code := guarded(t, guardedServer(t), "127.0.0.1:8787", headers).Code; code != http.StatusOK {
			t.Fatalf("код %d при заголовках %v — свой веб-интерфейс обязан работать", code, headers)
		}
	}
}

// Чужая страница в браузере могла снять защиту одним fetch: ответ ей прочитать
// не дадут, но запрос дойдёт и сработает.
func TestGuardRejectsCrossSiteRequest(t *testing.T) {
	t.Parallel()

	rec := guarded(t, guardedServer(t), "127.0.0.1:8787", map[string]string{
		"Sec-Fetch-Site": "cross-site",
		"Origin":         "https://evil.example",
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("код %d, ожидался отказ межсайтовому запросу", rec.Code)
	}
}

// Origin проверяется и сам по себе: браузер без Sec-Fetch-Site всё равно
// проставит его на межсайтовом POST.
func TestGuardRejectsForeignOrigin(t *testing.T) {
	t.Parallel()

	rec := guarded(t, guardedServer(t), "127.0.0.1:8787", map[string]string{"Origin": "http://evil.example"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("код %d, ожидался отказ чужому origin", rec.Code)
	}
}

// Перепривязка DNS: домен злоумышленника указывает на 127.0.0.1, и для
// браузера это его собственный origin. Отличает такой запрос только Host.
func TestGuardRejectsDNSRebinding(t *testing.T) {
	t.Parallel()

	rec := guarded(t, guardedServer(t), "evil.example:8787", map[string]string{
		"Sec-Fetch-Site": "same-origin",
		"Origin":         "http://evil.example:8787",
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("код %d, ожидался отказ по чужому Host", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "evil.example") {
		t.Fatalf("отказ обязан называть причину, тело: %q", rec.Body.String())
	}
}

// Имя без порта — это тоже не наш слушатель: порт браузер пишет всегда.
func TestGuardRejectsHostWithoutPort(t *testing.T) {
	t.Parallel()

	if code := guarded(t, guardedServer(t), "127.0.0.1", nil).Code; code != http.StatusForbidden {
		t.Fatalf("код %d, Host без порта нашим адресом не является", code)
	}
}

func TestLocalHosts(t *testing.T) {
	t.Parallel()

	allowed := localHosts("127.0.0.1:8787")
	for _, host := range []string{"127.0.0.1:8787", "localhost:8787", "[::1]:8787"} {
		if !allowed[host] {
			t.Fatalf("%s обязан считаться своим адресом", host)
		}
	}
	for _, host := range []string{"evil.example:8787", "127.0.0.1:9999", "127.0.0.1"} {
		if allowed[host] {
			t.Fatalf("%s своим адресом не является", host)
		}
	}
	// Выключенный веб-интерфейс не должен разрешать ничего.
	if len(localHosts("")) != 0 {
		t.Fatal("без http_addr разрешать нечего")
	}
}

// Аудит обязан правильно называть транспорт: запись «по TCP» о команде,
// пришедшей по управляющему сокету, хуже отсутствия записи — она уводит
// разбирательство не туда.
func TestAuditNamesTransport(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		privileged bool
		want       string
	}{
		{"управляющий сокет", true, "over the control socket"},
		{"веб-интерфейс", false, "over tcp"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var logged strings.Builder
			d := &Daemon{log: &logged}
			h := markTransport(d.auditMutations(http.HandlerFunc(
				func(http.ResponseWriter, *http.Request) {})), tt.privileged)

			req := httptest.NewRequest(http.MethodPost, "/protect", strings.NewReader("{}"))
			h.ServeHTTP(httptest.NewRecorder(), req)

			if !strings.Contains(logged.String(), tt.want) {
				t.Fatalf("в журнале %q, ожидалось упоминание %q", logged.String(), tt.want)
			}
		})
	}
}

// Чтение состояния происходит постоянно и засоряло бы журнал: пишем только
// то, что меняет защиту.
func TestAuditIgnoresReads(t *testing.T) {
	t.Parallel()

	var logged strings.Builder
	d := &Daemon{log: &logged}
	h := markTransport(d.auditMutations(http.HandlerFunc(
		func(http.ResponseWriter, *http.Request) {})), true)

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/status", nil))

	if logged.String() != "" {
		t.Fatalf("чтение состояния попало в журнал: %q", logged.String())
	}
}
