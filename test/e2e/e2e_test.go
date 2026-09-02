//go:build docker_e2e

package e2e

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tasticolly/splitr/internal/daemon"
)

// Тесты идут по порядку объявления и делят один контейнер, поэтому каждый
// начинается с resetStand: свежий демон, чистый стаб pfctl, туннеля нет.

// Test001BaseIsolation — без туннеля дальняя сеть недостижима в принципе.
func Test001BaseIsolation(t *testing.T) {
	killAllSshuttle()

	if sshuttleRunning() {
		t.Fatalf("на старте стенда не должно быть живых процессов sshuttle")
	}
	if !dialable(remoteAddr, 5*time.Second) {
		t.Fatalf("remote %s должен быть доступен из client по net_front", remoteAddr)
	}
	if body, err := fetchTarget(5 * time.Second); err == nil {
		t.Fatalf("target %s не должен быть доступен из client напрямую (сеть net_back изолирована), но ответил: %q", targetURL, body)
	}
	if dialable(targetAddr, 5*time.Second) {
		t.Fatalf("TCP до %s не должен устанавливаться без туннеля", targetAddr)
	}
}

// Test002TunnelUpDown — `up` реально поднимает sshuttle и открывает доступ к
// target через туннель, `down` его закрывает.
func Test002TunnelUpDown(t *testing.T) {
	resetStand(t)

	before := waitStatus(t, "якорь splitr подключён к главному набору", 15*time.Second,
		func(s daemon.Status) bool { return s.AnchorLoaded && s.AnchorLinked })
	if before.Tunnel != "down" {
		t.Fatalf("до подъёма туннель должен быть в состоянии down, получено %q", before.Tunnel)
	}
	if before.Profile != "docker" {
		t.Fatalf("профиль по умолчанию должен быть docker, получено %q", before.Profile)
	}
	if before.PID != 0 {
		t.Fatalf("до подъёма pid должен быть нулевым, получено %d", before.PID)
	}
	if len(before.SshuttleAnchs) != 0 {
		t.Fatalf("до подъёма якорей sshuttle быть не должно, получено %v", before.SshuttleAnchs)
	}
	if !before.Blocking {
		t.Fatalf("без туннеля kill-switch обязан резать трафик (blocking=true), статус: %s", mustJSON(before))
	}

	mustRequest(t, unixClient(), http.MethodPost, "/up", map[string]string{"profile": "docker"}, http.StatusOK)

	up := waitStatus(t, "туннель перешёл в up и якорь sshuttle появился в pf", 30*time.Second,
		func(s daemon.Status) bool { return s.Tunnel == "up" && len(s.SshuttleAnchs) > 0 })
	if up.PID == 0 || !processAlive(up.PID) {
		t.Fatalf("после подъёма должен быть живой процесс sshuttle, статус: %s", mustJSON(up))
	}
	if up.Profile != "docker" {
		t.Fatalf("активный профиль должен быть docker, получено %q", up.Profile)
	}
	if up.Blocking {
		t.Fatalf("при живом туннеле blocking должен быть false (правила перебиты якорем sshuttle), статус: %s", mustJSON(up))
	}

	// Командная строка sshuttle должна быть собрана из конфига.
	cl := cmdline(up.PID)
	for _, want := range []string{"sshuttle", "-r root@10.77.1.20", "--ssh-cmd", "-i /etc/splitr/id_ed25519", "-x " + frontNet, backNet} {
		if !strings.Contains(cl, want) {
			t.Fatalf("в командной строке sshuttle нет %q; строка: %q", want, cl)
		}
	}

	waitTarget(t, true, 30*time.Second)
	body, err := fetchTarget(5 * time.Second)
	if err != nil || !strings.Contains(body, targetMark) {
		t.Fatalf("через туннель target должен отдавать %q, получено %q (ошибка %v)", targetMark, body, err)
	}

	mustRequest(t, unixClient(), http.MethodPost, "/down", nil, http.StatusOK)

	down := waitStatus(t, "туннель опущен и якорь sshuttle исчез", 20*time.Second,
		func(s daemon.Status) bool { return s.Tunnel != "up" && len(s.SshuttleAnchs) == 0 })
	if processAlive(up.PID) {
		t.Fatalf("после down процесс sshuttle (pid %d) должен быть мёртв", up.PID)
	}
	if sshuttleRunning() {
		t.Fatalf("после down в системе не должно остаться процессов sshuttle")
	}
	if !down.Blocking {
		t.Fatalf("после down kill-switch обязан снова резать трафик, статус: %s", mustJSON(down))
	}
	waitTarget(t, false, 15*time.Second)
}

// Test003WatchdogRestoresAnchor — чужой `pfctl -F all` при опущенном туннеле
// должен быть починен сторожем: правила якоря и его подключение возвращаются.
func Test003WatchdogRestoresAnchor(t *testing.T) {
	resetStand(t)
	waitStatus(t, "якорь splitr подключён", 15*time.Second,
		func(s daemon.Status) bool { return s.AnchorLoaded && s.AnchorLinked })

	mark := len(stubCalls(t))
	pfctl(t, "-F", "all") // эмуляция чужого сброса всех правил

	if got := status(t); got.AnchorLoaded {
		t.Logf("сразу после сброса демон ещё считает якорь загруженным — сторож не успел отработать, это нормально")
	}

	fixed := waitStatus(t, "сторож вернул правила якоря и переподключил его", 15*time.Second,
		func(s daemon.Status) bool { return s.AnchorLoaded && s.AnchorLinked })
	if !fixed.Blocking {
		t.Fatalf("после починки блокировка должна снова действовать, статус: %s", mustJSON(fixed))
	}

	calls := stubCallsSince(t, mark)
	if !containsCall(calls, "-a splitr -f -") {
		t.Fatalf("сторож обязан был перезагрузить правила якоря (pfctl -a splitr -f -), вызовы:\n%s", strings.Join(calls, "\n"))
	}
	if !containsCall(calls, "pfctl -f /etc/pf.conf") {
		t.Fatalf("при опущенном туннеле сторож обязан перезагрузить pf.conf, вызовы:\n%s", strings.Join(calls, "\n"))
	}
	if rules := pfctl(t, "-a", "splitr", "-s", "rules"); !strings.Contains(rules, "block drop out") {
		t.Fatalf("в якоре splitr должны снова лежать правила блокировки, получено:\n%s", rules)
	}
}

// Test004WatchdogKeepsTunnel — при ЖИВОМ туннеле сторож не имеет права
// перезагружать pf.conf: это снесло бы якорь sshuttle и порвало туннель.
func Test004WatchdogKeepsTunnel(t *testing.T) {
	resetStand(t)
	waitStatus(t, "якорь splitr подключён", 15*time.Second,
		func(s daemon.Status) bool { return s.AnchorLoaded && s.AnchorLinked })

	mustRequest(t, unixClient(), http.MethodPost, "/up", nil, http.StatusOK)
	waitStatus(t, "туннель поднялся", 30*time.Second,
		func(s daemon.Status) bool { return s.Tunnel == "up" && len(s.SshuttleAnchs) > 0 })
	waitTarget(t, true, 30*time.Second)

	mark := len(stubCalls(t))
	pfctl(t, "-F", "all") // чужой сброс уже при живом туннеле

	restored := waitStatus(t, "правила якоря вернулись, но якорь остался неподключённым", 15*time.Second,
		func(s daemon.Status) bool { return s.AnchorLoaded && !s.AnchorLinked })

	calls := stubCallsSince(t, mark)
	if containsCall(calls, "pfctl -f /etc/pf.conf") {
		t.Fatalf("при живом туннеле сторож НЕ должен перезагружать pf.conf — это порвало бы туннель; вызовы:\n%s",
			strings.Join(calls, "\n"))
	}
	if !hasWarning(restored, "is not linked") {
		t.Fatalf("демон обязан жаловаться в warnings на неподключённый якорь, статус: %s", mustJSON(restored))
	}
	if !hasWarning(restored, "repair deferred") {
		t.Fatalf("в warnings должно быть сказано, что починка отложена до опускания туннеля, статус: %s", mustJSON(restored))
	}
	if !targetReachable(5 * time.Second) {
		t.Fatalf("туннель обязан пережить чужой сброс pf: target перестал отвечать")
	}

	// А как только туннель опустят — сторож обязан починить подключение якоря.
	mustRequest(t, unixClient(), http.MethodPost, "/down", nil, http.StatusOK)
	waitStatus(t, "после опускания туннеля сторож переподключил якорь", 20*time.Second,
		func(s daemon.Status) bool { return s.AnchorLoaded && s.AnchorLinked && s.Blocking })
}

// Test005TunnelKilled — внешний kill -9 по туннелю обязан быть замечен.
func Test005TunnelKilled(t *testing.T) {
	resetStand(t)
	waitStatus(t, "якорь splitr подключён", 15*time.Second,
		func(s daemon.Status) bool { return s.AnchorLoaded && s.AnchorLinked })
	mustRequest(t, unixClient(), http.MethodPost, "/up", nil, http.StatusOK)
	up := waitStatus(t, "туннель поднялся", 30*time.Second,
		func(s daemon.Status) bool { return s.Tunnel == "up" && len(s.SshuttleAnchs) > 0 })
	waitTarget(t, true, 30*time.Second)

	killTunnel(t, up.PID)

	dead := waitStatus(t, "демон заметил падение туннеля и вернул блокировку", 20*time.Second,
		func(s daemon.Status) bool {
			return (s.Tunnel == "down" || s.Tunnel == "failed") && len(s.SshuttleAnchs) == 0 && s.Blocking
		})
	if processAlive(up.PID) {
		t.Fatalf("процесс sshuttle (pid %d) должен быть мёртв", up.PID)
	}
	if !dead.Blocking {
		t.Fatalf("после падения туннеля блокировка обязана действовать, статус: %s", mustJSON(dead))
	}
	if !dead.AnchorLoaded || !dead.AnchorLinked {
		t.Fatalf("правила блокировки должны остаться на месте, статус: %s", mustJSON(dead))
	}
	waitTarget(t, false, 15*time.Second)
}

// Test006Autoreconnect — с autoreconnect: true упавший туннель поднимается сам.
func Test006Autoreconnect(t *testing.T) {
	resetStand(t)
	waitStatus(t, "якорь splitr подключён", 15*time.Second,
		func(s daemon.Status) bool { return s.AnchorLoaded && s.AnchorLinked })
	writeConfig(t, patchedConfig(t, "autoreconnect: false", "autoreconnect: true"))
	mustRequest(t, unixClient(), http.MethodPost, "/reload", nil, http.StatusOK)

	mustRequest(t, unixClient(), http.MethodPost, "/up", nil, http.StatusOK)
	up := waitStatus(t, "туннель поднялся", 30*time.Second,
		func(s daemon.Status) bool { return s.Tunnel == "up" && len(s.SshuttleAnchs) > 0 })
	waitTarget(t, true, 30*time.Second)

	killTunnel(t, up.PID)

	again := waitStatus(t, "автопереподключение подняло туннель заново", 60*time.Second,
		func(s daemon.Status) bool { return s.Tunnel == "up" && s.PID != 0 && s.PID != up.PID })
	if !processAlive(again.PID) {
		t.Fatalf("после автопереподключения должен жить новый процесс sshuttle, статус: %s", mustJSON(again))
	}
	waitTarget(t, true, 30*time.Second)

	// Возвращаем эталонный конфиг, чтобы следующий тест начинал с чистого.
	restoreConfig(t)
	mustRequest(t, unixClient(), http.MethodPost, "/reload", nil, http.StatusOK)
	mustRequest(t, unixClient(), http.MethodPost, "/down", nil, http.StatusOK)
}

// Test007HTTPAPI — весь управляющий API, и через unix-сокет, и через TCP.
func Test007HTTPAPI(t *testing.T) {
	resetStand(t)
	waitStatus(t, "якорь splitr подключён", 15*time.Second,
		func(s daemon.Status) bool { return s.AnchorLoaded && s.AnchorLinked })

	for _, tc := range []struct {
		name   string
		client func() *http.Client
	}{
		{"unix-сокет", unixClient},
		{"TCP 127.0.0.1:8787", tcpClient},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := tc.client()

			st := statusVia(t, c)
			if st.ConfigPath != configPath {
				t.Fatalf("/status: config_path %q, ожидался %q", st.ConfigPath, configPath)
			}
			if st.Protection != "all" {
				t.Fatalf("/status: protection %q, ожидался all", st.Protection)
			}
			if !contains(st.BlockedNets, backNet) {
				t.Fatalf("/status: в blocked_nets нет %s: %v", backNet, st.BlockedNets)
			}

			cfg := mustRequest(t, c, http.MethodGet, "/config", nil, http.StatusOK)
			if !strings.Contains(string(cfg), `"default_profile": "docker"`) {
				t.Fatalf("/config: в ответе нет профиля docker:\n%s", cfg)
			}

			rules := string(mustRequest(t, c, http.MethodGet, "/rules", nil, http.StatusOK))
			for _, want := range []string{"table <splitr_block>", backNet, "block drop out", "table <splitr_pass>", frontNet} {
				if !strings.Contains(rules, want) {
					t.Fatalf("/rules: в правилах нет %q:\n%s", want, rules)
				}
			}

			// probe при опущенном туннеле: демон пытается достучаться до
			// блокируемой сети и должен получить отказ.
			probe := string(mustRequest(t, c, http.MethodPost, "/probe", nil, http.StatusOK))
			if !strings.Contains(probe, "10.77.2.0:443") {
				t.Fatalf("/probe: ожидалась проба до 10.77.2.0:443, получено:\n%s", probe)
			}
			if !strings.Contains(probe, `"leaked": false`) {
				t.Fatalf("/probe: соединение до дальней сети не должно устанавливаться:\n%s", probe)
			}

			mustRequest(t, c, http.MethodPost, "/protect", map[string]string{"mode": "off"}, http.StatusOK)
			if st := statusVia(t, c); st.Protection != "off" || st.Blocking {
				t.Fatalf("после protect off ожидалось protection=off и blocking=false, статус: %s", mustJSON(st))
			}

			mustRequest(t, c, http.MethodPost, "/protect", map[string]string{"mode": "strict"}, http.StatusOK)
			panicRules := string(mustRequest(t, c, http.MethodGet, "/rules", nil, http.StatusOK))
			if !strings.Contains(panicRules, "block drop out quick") {
				t.Fatalf("в panic-режиме блокировка обязана быть quick:\n%s", panicRules)
			}
			if st := statusVia(t, c); st.Protection != "strict" {
				t.Fatalf("после protect strict ожидался protection=strict, получено %q", st.Protection)
			}

			mustRequest(t, c, http.MethodPost, "/protect", map[string]string{"mode": "on"}, http.StatusOK)
			if st := statusVia(t, c); st.Protection != "all" {
				t.Fatalf("после protect on ожидался protection=all, получено %q", st.Protection)
			}

			code, body, err := request(c, http.MethodPost, "/protect", map[string]string{"mode": "чепуха"})
			if err != nil {
				t.Fatalf("/protect с мусором: запрос не выполнился: %v", err)
			}
			if code != http.StatusInternalServerError || !strings.Contains(string(body), "error") {
				t.Fatalf("/protect с неизвестным режимом должен отвечать 500 с описанием ошибки, получено %d: %s", code, body)
			}

			mustRequest(t, c, http.MethodPost, "/reload", nil, http.StatusOK)

			mustRequest(t, c, http.MethodPost, "/up", map[string]string{"profile": "docker"}, http.StatusOK)
			waitStatus(t, "туннель поднялся через "+tc.name, 30*time.Second,
				func(s daemon.Status) bool { return s.Tunnel == "up" && len(s.SshuttleAnchs) > 0 })
			waitTarget(t, true, 30*time.Second)
			mustRequest(t, c, http.MethodPost, "/down", nil, http.StatusOK)
			waitStatus(t, "туннель опущен через "+tc.name, 20*time.Second,
				func(s daemon.Status) bool { return s.Tunnel != "up" && len(s.SshuttleAnchs) == 0 })

			// Неизвестный профиль — ошибка, а не молчаливый успех.
			code, body, err = request(c, http.MethodPost, "/up", map[string]string{"profile": "нетакого"})
			if err != nil {
				t.Fatalf("/up с неизвестным профилем: запрос не выполнился: %v", err)
			}
			if code != http.StatusInternalServerError || !strings.Contains(string(body), "unknown profile") {
				t.Fatalf("/up с неизвестным профилем должен отвечать 500, получено %d: %s", code, body)
			}

			// Маршрутизация: чужой путь и чужой метод.
			if code, _, err := request(c, http.MethodGet, "/такого-нет", nil); err != nil || code != http.StatusNotFound {
				t.Fatalf("несуществующий путь должен давать 404, получено %d (%v)", code, err)
			}
			if code, _, err := request(c, http.MethodPost, "/status", nil); err != nil || code != http.StatusMethodNotAllowed {
				t.Fatalf("POST /status должен давать 405, получено %d (%v)", code, err)
			}

			// Веб-интерфейс отдаётся на том же адресе.
			ui := mustRequest(t, c, http.MethodGet, "/", nil, http.StatusOK)
			if !strings.Contains(string(ui), "SplitR") {
				t.Fatalf("на / ожидалась страница веб-интерфейса, получено:\n%s", firstBytes(ui, 200))
			}
		})
	}
}

// Test008CLI — те же операции через пользовательский CLI.
func Test008CLI(t *testing.T) {
	resetStand(t)
	waitStatus(t, "якорь splitr подключён", 15*time.Second,
		func(s daemon.Status) bool { return s.AnchorLoaded && s.AnchorLinked })

	out := mustCLI(t, "status")
	for _, want := range []string{"tunnel:", "protection:", "dropped"} {
		if !strings.Contains(out, want) {
			t.Fatalf("splitr status: в выводе нет %q:\n%s", want, out)
		}
	}

	if out := mustCLI(t, "rules"); !strings.Contains(out, backNet) {
		t.Fatalf("splitr rules: в правилах нет %s:\n%s", backNet, out)
	}

	if out := mustCLI(t, "up", "docker"); !strings.Contains(out, "tunnel:") {
		t.Fatalf("splitr up docker: неожиданный вывод:\n%s", out)
	}
	waitStatus(t, "туннель поднялся после CLI up", 30*time.Second,
		func(s daemon.Status) bool { return s.Tunnel == "up" && len(s.SshuttleAnchs) > 0 })
	waitTarget(t, true, 30*time.Second)

	if out := mustCLI(t, "status"); !strings.Contains(out, "sshuttle anchors: sshuttle-") {
		t.Fatalf("splitr status при живом туннеле должен показывать якоря sshuttle:\n%s", out)
	}

	if out := mustCLI(t, "protect", "strict"); !strings.Contains(out, "strict") {
		t.Fatalf("splitr protect strict: неожиданный вывод:\n%s", out)
	}
	mustCLI(t, "protect", "on")

	mustCLI(t, "down")
	waitStatus(t, "туннель опущен после CLI down", 20*time.Second,
		func(s daemon.Status) bool { return s.Tunnel != "up" && len(s.SshuttleAnchs) == 0 })

	if out := mustCLI(t, "status", "--json"); !strings.Contains(out, `"tunnel"`) {
		t.Fatalf("splitr status --json должен отдавать JSON:\n%s", out)
	}
	if out := mustCLI(t, "probe"); !strings.Contains(out, "10.77.2.0:443") || !strings.Contains(out, "unreachable") {
		t.Fatalf("splitr probe без туннеля должен показывать, что рабочий адрес недоступен:\n%s", out)
	}
	if out, err := splitr(t, "такой-команды-нет"); err == nil {
		t.Fatalf("неизвестная команда CLI должна завершаться ошибкой, получено:\n%s", out)
	}
	if out := mustCLI(t, "validate", configPath); !strings.Contains(out, "is valid") {
		t.Fatalf("splitr validate должен подтверждать корректность конфига:\n%s", out)
	}
}

// Test009ReloadChangesRules — правка конфига на диске + reload меняет набор
// блокируемых сетей.
func Test009ReloadChangesRules(t *testing.T) {
	resetStand(t)
	waitStatus(t, "якорь splitr подключён", 15*time.Second,
		func(s daemon.Status) bool { return s.AnchorLoaded && s.AnchorLinked })

	rules := string(mustRequest(t, unixClient(), http.MethodGet, "/rules", nil, http.StatusOK))
	if strings.Contains(rules, "10.99.0.0/16") {
		t.Fatalf("до правки конфига сети 10.99.0.0/16 в правилах быть не должно:\n%s", rules)
	}

	writeConfig(t, patchedConfig(t, "subnets:\n  - 10.77.2.0/24", "subnets:\n  - 10.77.2.0/24\n  - 10.99.0.0/16"))
	mustRequest(t, unixClient(), http.MethodPost, "/reload", nil, http.StatusOK)

	rules = string(mustRequest(t, unixClient(), http.MethodGet, "/rules", nil, http.StatusOK))
	if !strings.Contains(rules, "10.99.0.0/16") {
		t.Fatalf("после reload новая сеть обязана появиться в правилах:\n%s", rules)
	}
	st := status(t)
	if !contains(st.BlockedNets, "10.99.0.0/16") {
		t.Fatalf("после reload в blocked_nets нет новой сети: %v", st.BlockedNets)
	}
	if loaded := pfctl(t, "-a", "splitr", "-s", "rules"); !strings.Contains(loaded, "10.99.0.0/16") {
		t.Fatalf("новые правила должны быть загружены в якорь pf:\n%s", loaded)
	}
	if disk := tailFile(anchorFile, 50); !strings.Contains(disk, "10.99.0.0/16") {
		t.Fatalf("новые правила должны быть записаны в %s:\n%s", anchorFile, disk)
	}

	// Возврат к эталону — и проверка, что reload умеет и сужать список.
	restoreConfig(t)
	mustRequest(t, unixClient(), http.MethodPost, "/reload", nil, http.StatusOK)
	rules = string(mustRequest(t, unixClient(), http.MethodGet, "/rules", nil, http.StatusOK))
	if strings.Contains(rules, "10.99.0.0/16") {
		t.Fatalf("после возврата конфига сеть 10.99.0.0/16 должна исчезнуть из правил:\n%s", rules)
	}

	// Битый конфиг не должен приниматься.
	writeConfig(t, "это не yaml: [")
	code, body, err := request(unixClient(), http.MethodPost, "/reload", nil)
	if err != nil {
		t.Fatalf("/reload с битым конфигом: запрос не выполнился: %v", err)
	}
	if code != http.StatusInternalServerError {
		t.Fatalf("/reload с битым конфигом должен отвечать 500, получено %d: %s", code, body)
	}
	restoreConfig(t)
	mustRequest(t, unixClient(), http.MethodPost, "/reload", nil, http.StatusOK)
	if st := status(t); !contains(st.BlockedNets, backNet) {
		t.Fatalf("после отката конфига блокируемые сети должны вернуться: %v", st.BlockedNets)
	}
}

// Test010Idempotency — повторные up/down не должны ломать состояние.
func Test010Idempotency(t *testing.T) {
	resetStand(t)
	waitStatus(t, "якорь splitr подключён", 15*time.Second,
		func(s daemon.Status) bool { return s.AnchorLoaded && s.AnchorLinked })

	// down при опущенном туннеле — не ошибка.
	mustRequest(t, unixClient(), http.MethodPost, "/down", nil, http.StatusOK)
	mustRequest(t, unixClient(), http.MethodPost, "/down", nil, http.StatusOK)
	if st := status(t); st.Tunnel != "down" || !st.Blocking {
		t.Fatalf("повторный down при опущенном туннеле не должен менять состояние, статус: %s", mustJSON(st))
	}

	mustRequest(t, unixClient(), http.MethodPost, "/up", nil, http.StatusOK)
	up := waitStatus(t, "туннель поднялся", 30*time.Second,
		func(s daemon.Status) bool { return s.Tunnel == "up" && len(s.SshuttleAnchs) > 0 })
	waitTarget(t, true, 30*time.Second)

	code, body, err := request(unixClient(), http.MethodPost, "/up", nil)
	if err != nil {
		t.Fatalf("повторный /up: запрос не выполнился: %v", err)
	}
	assertRepeatedUp(t, code, body, up)
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func hasWarning(s daemon.Status, substr string) bool {
	for _, w := range s.Warnings {
		if strings.Contains(w, substr) {
			return true
		}
	}
	return false
}

func firstBytes(b []byte, n int) string {
	if len(b) > n {
		return string(b[:n])
	}
	return string(b)
}
