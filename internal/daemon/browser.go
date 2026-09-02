package daemon

import (
	"net"
	"net/http"
	"strings"
)

// Защита порта 127.0.0.1 от чужих веб-страниц.
//
// Управляющий API слушает localhost, и до сих пор это означало, что любая
// открытая в браузере страница могла дотянуться до него запросом со своего
// JavaScript: `fetch('http://127.0.0.1:8787/protection', {method:'POST', ...})`
// снял бы защиту, а `mode: 'no-cors'` не мешает запросу дойти — он мешает лишь
// прочитать ответ. Прав на это не нужно никаких: страница выполняется на той же
// машине.
//
// Второй заход — перепривязка DNS: страница с домена злоумышленника меняет свою
// A-запись на 127.0.0.1, после чего её собственный origin указывает на наш
// демон, и браузер считает такие запросы своими. Ни Origin, ни Sec-Fetch-Site
// от этого не защищают, потому что для браузера это одинаковый origin.
//
// Отсюда три проверки: заголовок Host отсекает перепривязку DNS (у настоящего
// обращения там адрес нашего же слушателя, у перепривязанного — доменное имя),
// Origin и Sec-Fetch-Site отсекают обычные межсайтовые запросы. Не-браузерные
// клиенты (curl, CLI, menu bar) этих заголовков не шлют вовсе и работают как
// раньше.

// guardBrowser отклоняет запросы, которые инициировала чужая веб-страница.
func guardBrowser(next http.Handler, allowedHosts map[string]bool, logf func(format string, args ...any)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if reason, ok := browserRejection(r, allowedHosts); ok {
			logf("request %s %s rejected: %s (Host=%q, Origin=%q)", r.Method, r.URL.Path, reason, r.Host, r.Header.Get("Origin"))
			http.Error(w, "splitr: "+reason, http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// browserRejection возвращает причину отказа, если запрос пришёл не туда,
// куда думает клиент, или его инициировала чужая страница.
func browserRejection(r *http.Request, allowedHosts map[string]bool) (string, bool) {
	if !allowedHosts[strings.ToLower(r.Host)] {
		return "request is addressed to a foreign name " + r.Host + ", not to the local daemon address", true
	}
	// Sec-Fetch-Site шлют все нынешние браузеры, и подделать его из JavaScript
	// нельзя: заголовок проставляет сам браузер. same-origin и none — это
	// наша же страница и адресная строка соответственно.
	switch site := r.Header.Get("Sec-Fetch-Site"); site {
	case "", "same-origin", "none":
	default:
		return "cross-site request (Sec-Fetch-Site: " + site + ")", true
	}
	if origin := r.Header.Get("Origin"); origin != "" && !allowedOrigin(origin, allowedHosts) {
		return "request from a foreign origin " + origin, true
	}
	return "", false
}

// allowedOrigin сверяет Origin с теми же адресами, что и Host.
func allowedOrigin(origin string, allowedHosts map[string]bool) bool {
	rest, ok := strings.CutPrefix(strings.ToLower(origin), "http://")
	if !ok {
		return false
	}
	return allowedHosts[rest]
}

// localHosts перечисляет значения Host, которые считаются обращением к
// собственному слушателю: сам заданный адрес и привычные записи того же места.
//
// Порт обязателен: браузер всегда пишет его в Host, если он не 80. Имя без
// порта осталось бы дырой ровно там, где мы её и закрываем.
func localHosts(httpAddr string) map[string]bool {
	allowed := map[string]bool{}
	if httpAddr == "" {
		return allowed
	}
	allowed[strings.ToLower(httpAddr)] = true

	_, port, err := net.SplitHostPort(httpAddr)
	if err != nil {
		return allowed
	}
	for _, host := range []string{"127.0.0.1", "localhost", "[::1]"} {
		allowed[host+":"+port] = true
	}
	return allowed
}
