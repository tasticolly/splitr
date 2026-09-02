//go:build pf_e2e && darwin

// Главный e2e-тест splitr: проверяет на настоящем ядре macOS то самое
// утверждение, на котором держится весь продукт.
//
//	Блокировка без quick загружена ПОСТОЯННО, но живой якорь sshuttle,
//	стоящий ниже в главном наборе правил, перебивает её (last-match-wins).
//	Исчез sshuttle — трафик умирает сразу, окна гонки не существует.
//
// Проверить это в Docker нельзя: в Linux нет pf, а без pf нет ни якорей,
// ни last-match-wins, ни route-to. Юнит-тесты проверяют только текст правил;
// поведение ядра проверяет вот этот файл.
//
// Тест требует root и намеренно закрыт тегом pf_e2e, чтобы никогда не
// запускаться случайно: он временно подменяет главный набор правил pf.
// Запускать через test/pfe2e/run.sh.

package pfe2e

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/tasticolly/splitr/internal/pfctl"
	"github.com/tasticolly/splitr/internal/protect"
)

const (
	// dialTimeout — сколько ждём успешного соединения. Больше секунды нет
	// смысла: цель выбирается по факту достижимости прямо перед тестом.
	dialTimeout = 3 * time.Second
	// blockedTimeout — сколько ждём, прежде чем считать соединение
	// заблокированным. `block drop` роняет SYN молча, поэтому здесь
	// всегда истекает таймаут, и он не должен быть слишком большим.
	blockedTimeout = 4 * time.Second
	// settleDelay — пауза после смены правил pf: ядру нужно мгновение,
	// а тесту — гарантия, что мы меряем уже новое состояние.
	settleDelay = 300 * time.Millisecond
	// banner — метка, по которой видно, что соединение пришло именно
	// в локальный слушатель, а не на настоящую цель.
	banner = "splitr-e2e-local-listener\n"
)

// harness — всё окружение теста: цель, слушатель и восстановление pf.
type harness struct {
	pf *pfctl.CLI
	// target — заведомо достижимый адрес вне машины.
	target string
	// port — TCP-порт цели.
	port int
	// listenerPort — порт локального слушателя, куда имитация sshuttle
	// заворачивает трафик.
	listenerPort int
}

// targetAddr — адрес цели в виде host:port.
func (h *harness) addr() string { return net.JoinHostPort(h.target, fmt.Sprint(h.port)) }

// cidr — цель в виде /32-сети для таблиц pf.
func (h *harness) cidr() string { return h.target + "/32" }

func TestProtectionOnRealKernel(t *testing.T) {
	h := setup(t)

	// Шаг 1. Базовая проверка: без блокировки цель достижима.
	// Без этого шага любой «заблокировано» ничего не доказывает —
	// сеть могла просто отвалиться.
	t.Run("шаг 1: цель достижима без блокировки", func(t *testing.T) {
		h.flush(t, e2eAnchor)
		h.flush(t, sshuttleAnchor)
		h.mustConnect(t, h.addr(), "цель должна быть достижима, пока правила блокировки не загружены")
	})

	// Шаг 2. Загружаем боевые правила splitr (сгенерированные прод-кодом)
	// с целью в таблице блокировки — соединение обязано умереть.
	t.Run("шаг 2: блокировка режет трафик", func(t *testing.T) {
		h.loadProtection(t, protect.Ruleset{Block: []string{h.cidr()}})
		h.mustBlocked(t, h.addr(), "цель в таблице splitr_block — соединение не должно устанавливаться")
	})

	// Шаг 3. Ключевой шаг. Подключаем якорь-имитацию sshuttle ПОСЛЕ якоря
	// splitr. Его `pass out route-to lo0` стоит ниже и по last-match-wins
	// перебивает `block drop`. Мало того, что соединение восстанавливается —
	// оно обязано прийти в локальный слушатель, как в настоящем туннеле.
	t.Run("шаг 3: живой sshuttle перебивает блокировку", func(t *testing.T) {
		h.loadSshuttle(t)
		got := h.mustConnectAndRead(t, h.addr(),
			"живой якорь sshuttle обязан перебить блокировку (last-match-wins)")
		if got != strings.TrimSpace(banner) {
			t.Fatalf("соединение ушло НЕ в локальный слушатель: получено %q, ожидалось %q.\n"+
				"Значит rdr на lo0 не сработал, и тест не доказывает перехват трафика туннелем",
				got, strings.TrimSpace(banner))
		}
	})

	// Шаг 4. Туннель «упал». Якорь-имитация снимается — блокировка обязана
	// сработать немедленно, без окна проскока: правила-то никуда не девались.
	t.Run("шаг 4: снятие sshuttle возвращает блокировку", func(t *testing.T) {
		h.flush(t, sshuttleAnchor)
		h.killStates(t)
		h.mustBlocked(t, h.addr(),
			"после исчезновения якоря sshuttle блокировка обязана резать трафик сразу")
	})

	// Шаг 5. Таблица исключений. pass-правило стоит ниже block, поэтому
	// адрес в splitr_pass проходит, даже находясь в splitr_block.
	t.Run("шаг 5: исключения не блокируются", func(t *testing.T) {
		h.loadProtection(t, protect.Ruleset{
			Block: []string{h.cidr()},
			Pass:  []string{h.cidr()},
		})
		h.mustConnect(t, h.addr(),
			"адрес есть в splitr_pass, а pass-правило идёт после block — блокировка должна сниматься")
	})

	// Шаг 6. Panic-режим: правила помечены quick, разбор останавливается на
	// первом совпадении, и якорь sshuttle ниже уже ничего не решает.
	t.Run("шаг 6: panic режет даже при живом sshuttle", func(t *testing.T) {
		h.loadProtection(t, protect.Ruleset{Block: []string{h.cidr()}, Strict: true})
		h.loadSshuttle(t)
		h.killStates(t)
		h.mustBlocked(t, h.addr(),
			"panic-режим (quick) обязан резать трафик, даже когда туннель поднят")
		h.flush(t, sshuttleAnchor)
	})

	// Шаг 7. Ограничение `on ! lo0`. Даже если в таблице блокировки лежит
	// весь loopback, трафик на lo0 не задет — иначе блокировка порвала бы
	// сам туннель, который через lo0 и работает.
	t.Run("шаг 7: трафик на lo0 не задет", func(t *testing.T) {
		local := net.JoinHostPort("127.0.0.1", fmt.Sprint(h.listenerPort))
		for _, panicMode := range []bool{false, true} {
			h.loadProtection(t, protect.Ruleset{
				Block:  []string{"127.0.0.0/8", h.cidr()},
				Strict: panicMode,
			})
			got := h.mustConnectAndRead(t, local, fmt.Sprintf(
				"loopback в таблице блокировки, но правило ограничено `on ! lo0` (panic=%v)", panicMode))
			if got != strings.TrimSpace(banner) {
				t.Fatalf("на lo0 пришёл неожиданный ответ %q (panic=%v)", got, panicMode)
			}
		}
	})
}

// setup готовит окружение и регистрирует восстановление pf.
//
// Восстановление обязано отработать при любом исходе: провал теста, паника,
// Ctrl-C. Это боевая машина, порвать на ней сеть недопустимо.
func setup(t *testing.T) *harness {
	t.Helper()

	if os.Geteuid() != 0 {
		t.Fatal("тест меняет главный набор правил pf и требует root; запускайте через `sudo test/pfe2e/run.sh`")
	}

	pf := pfctl.New()
	h := &harness{pf: pf}

	// Живой sshuttle или AnyConnect добавляют свои якоря динамически, и
	// перезагрузка главного набора правил их снесла бы — то есть тест порвал
	// бы работающий туннель. Отказываемся заранее.
	if anchors, err := pf.SshuttleAnchors(); err != nil {
		t.Fatalf("не удалось прочитать список якорей pf: %v", err)
	} else if len(anchors) > 0 {
		t.Skipf("активен туннель sshuttle (якоря %v): тест перезагружает главный набор правил "+
			"и снёс бы его. Опустите туннель (`splitr down`) и повторите", anchors)
	}

	base, err := readMainConf()
	if err != nil {
		t.Fatalf("прочитать %s: %v", protect.PfConf, err)
	}
	// `set skip on lo0` отключил бы разбор правил на lo0 целиком, и rdr
	// имитации sshuttle просто не сработал бы — шаг 3 стал бы ложным.
	if strings.Contains(string(base), "set skip on lo0") {
		t.Skipf("в %s есть `set skip on lo0`: pf не разбирает правила на lo0, "+
			"перехват трафика на локальный слушатель проверить невозможно", protect.PfConf)
	}

	h.listenerPort = startLocalListener(t)
	h.target, h.port = pickTarget(t)
	t.Logf("цель теста: %s, локальный слушатель: 127.0.0.1:%d", h.addr(), h.listenerPort)

	// pf может быть выключен. Включаем через -E: ядро считает ссылки,
	// поэтому наш -E/-X не мешает ни демону, ни sshuttle.
	token, err := pf.Enable()
	if err != nil {
		t.Fatalf("включить pf: %v", err)
	}

	conf, err := buildTestMainConf(base)
	if err != nil {
		t.Fatalf("собрать временный главный набор правил: %v", err)
	}
	confPath := filepath.Join(t.TempDir(), "pf.e2e.conf")
	if err := os.WriteFile(confPath, conf, 0o600); err != nil {
		t.Fatalf("записать временный pf.conf: %v", err)
	}

	var once sync.Once
	restore := func() {
		once.Do(func() {
			// Порядок важен: сначала убираем содержимое тестовых якорей,
			// потом возвращаем штатный главный набор (он же убирает и сами
			// вызовы якорей), и только потом отпускаем ссылку на pf.
			_ = pf.FlushAnchor(sshuttleAnchor)
			_ = pf.FlushAnchor(e2eAnchor)
			if err := pf.ReloadMain(protect.PfConf); err != nil {
				fmt.Fprintf(os.Stderr,
					"ВНИМАНИЕ: не удалось перезагрузить %s: %v\n"+
						"Выполните вручную: sudo pfctl -f %s\n",
					protect.PfConf, err, protect.PfConf)
			}
			if err := pf.Release(token); err != nil {
				fmt.Fprintf(os.Stderr, "ВНИМАНИЕ: не удалось освободить токен pf %s: %v\n", token, err)
			}
		})
	}

	// Ctrl-C во время теста не должен оставить машину с подменённым pf.conf.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		if _, ok := <-sig; !ok {
			return
		}
		fmt.Fprintln(os.Stderr, "\nполучен сигнал, восстанавливаю pf...")
		restore()
		os.Exit(130)
	}()

	// t.Cleanup отрабатывает и при t.Fatal, и при панике внутри теста.
	t.Cleanup(func() {
		signal.Stop(sig)
		close(sig)
		restore()
	})

	if err := pf.ReloadMain(confPath); err != nil {
		t.Fatalf("загрузить временный главный набор правил: %v", err)
	}

	// Если якоря не подключены к главному набору, их правила просто не
	// вычисляются — тест «проходил» бы, ничего не проверяя.
	for _, a := range []string{e2eAnchor, sshuttleAnchor} {
		referenced, err := pfctl.AnchorReferenced(pf, a)
		if err != nil {
			t.Fatalf("проверить подключение якоря %s: %v", a, err)
		}
		if !referenced {
			t.Fatalf("якорь %s не подключён к главному набору правил — тест не проверял бы ничего", a)
		}
	}

	return h
}

// startLocalListener поднимает TCP-слушатель на 127.0.0.1 и отдаёт его порт.
// Слушатель здоровается меткой: по ней видно, что соединение перехвачено
// имитацией sshuttle, а не ушло на настоящую цель.
func startLocalListener(t *testing.T) int {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("поднять локальный слушатель: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return // слушатель закрыт вместе с тестом
			}
			go func() {
				defer conn.Close()
				_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
				_, _ = io.WriteString(conn, banner)
			}()
		}
	}()

	return ln.Addr().(*net.TCPAddr).Port
}

// pickTarget выбирает заведомо достижимую TCP-цель вне машины.
//
// Публичные адреса идут первыми намеренно: настоящий sshuttle заворачивает
// именно удалённые сети, и проверка route-to/rdr на удалённом адресе ближе к
// боевому сценарию. Шлюз по умолчанию — запасной вариант для машины без
// интернета; у него к тому же не гарантирован открытый TCP-порт.
func pickTarget(t *testing.T) (string, int) {
	t.Helper()

	type candidate struct {
		host string
		port int
		what string
	}
	candidates := []candidate{
		{"1.1.1.1", 443, "публичный DNS Cloudflare"},
		{"8.8.8.8", 443, "публичный DNS Google"},
		{"9.9.9.9", 443, "публичный DNS Quad9"},
	}
	if gw := defaultGateway(); gw != "" {
		candidates = append(candidates,
			candidate{gw, 80, "шлюз по умолчанию"},
			candidate{gw, 443, "шлюз по умолчанию"})
	}

	var tried []string
	for _, c := range candidates {
		addr := net.JoinHostPort(c.host, fmt.Sprint(c.port))
		conn, err := net.DialTimeout("tcp", addr, dialTimeout)
		if err == nil {
			_ = conn.Close()
			t.Logf("цель %s (%s) достижима", addr, c.what)
			return c.host, c.port
		}
		tried = append(tried, fmt.Sprintf("%s (%s): %v", addr, c.what, err))
	}

	t.Skipf("не найдено ни одной достижимой TCP-цели вне машины, проверять блокировку не на чем.\n"+
		"Возможные причины: нет сети, всё режет корпоративный фильтр, уже загружен собственный "+
		"kill-switch пользователя с этими адресами.\nПопытки:\n  %s", strings.Join(tried, "\n  "))
	return "", 0
}

// defaultGateway возвращает IPv4-адрес шлюза по умолчанию (пусто, если его нет).
func defaultGateway() string {
	out, err := exec.Command("/sbin/route", "-n", "get", "-inet", "default").Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[0] == "gateway:" {
			if ip, err := netip.ParseAddr(f[1]); err == nil && ip.Is4() {
				return ip.String()
			}
		}
	}
	return ""
}

// mustConnect требует, чтобы соединение установилось.
// Даётся несколько попыток: одиночная потеря пакета не должна ронять тест.
func (h *harness) mustConnect(t *testing.T, addr, why string) {
	t.Helper()

	var last error
	for attempt := 1; attempt <= 3; attempt++ {
		conn, err := net.DialTimeout("tcp", addr, dialTimeout)
		if err == nil {
			_ = conn.Close()
			return
		}
		last = err
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("соединение с %s не установилось за 3 попытки: %v\nОжидалось: %s\n%s",
		addr, last, why, h.dumpRules(t))
}

// mustConnectAndRead требует, чтобы соединение установилось и собеседник
// поздоровался. Возвращает то, что он сказал.
func (h *harness) mustConnectAndRead(t *testing.T, addr, why string) string {
	t.Helper()

	var last error
	for attempt := 1; attempt <= 3; attempt++ {
		conn, err := net.DialTimeout("tcp", addr, dialTimeout)
		if err != nil {
			last = err
			time.Sleep(200 * time.Millisecond)
			continue
		}
		_ = conn.SetReadDeadline(time.Now().Add(dialTimeout))
		buf := make([]byte, len(banner))
		n, rerr := io.ReadFull(conn, buf)
		_ = conn.Close()
		if rerr != nil && n == 0 {
			// Соединение есть, но собеседник молчит — это настоящая цель
			// (TLS-сервер ждёт ClientHello), а не наш слушатель.
			return fmt.Sprintf("<молчание: %v>", rerr)
		}
		return strings.TrimSpace(string(buf[:n]))
	}
	t.Fatalf("соединение с %s не установилось за 3 попытки: %v\nОжидалось: %s\n%s",
		addr, last, why, h.dumpRules(t))
	return ""
}

// mustBlocked требует, чтобы соединение НЕ установилось.
// Проверяется дважды: pf роняет SYN молча, и одна попытка могла бы совпасть
// со случайным сетевым сбоем — но здесь важна именно устойчивая блокировка.
func (h *harness) mustBlocked(t *testing.T, addr, why string) {
	t.Helper()

	for attempt := 1; attempt <= 2; attempt++ {
		conn, err := net.DialTimeout("tcp", addr, blockedTimeout)
		if err == nil {
			_ = conn.Close()
			t.Fatalf("соединение с %s установилось, хотя не должно было (попытка %d).\nОжидалось: %s\n%s",
				addr, attempt, why, h.dumpRules(t))
		}
		var nerr net.Error
		if !errors.As(err, &nerr) {
			t.Logf("попытка %d: соединение не установилось (%v)", attempt, err)
		}
	}
}

// loadProtection рендерит правила БОЕВЫМ кодом защиты и грузит их
// в тестовый якорь. Именно поэтому тест проверяет продукт, а не свою копию.
func (h *harness) loadProtection(t *testing.T, rs protect.Ruleset) {
	t.Helper()

	rules := rs.Rules()
	t.Logf("правила splitr (%s):\n%s", e2eAnchor, rules)
	if err := h.pf.LoadAnchor(e2eAnchor, rules); err != nil {
		t.Fatalf("загрузить правила splitr в якорь %s: %v\nправила:\n%s", e2eAnchor, err, rules)
	}
	h.killStates(t)
	time.Sleep(settleDelay)
}

// loadSshuttle грузит якорь-имитацию туннеля.
func (h *harness) loadSshuttle(t *testing.T) {
	t.Helper()

	rules := sshuttleAnchorRules(h.target, h.port, h.listenerPort)
	t.Logf("правила имитации sshuttle (%s):\n%s", sshuttleAnchor, rules)
	if err := h.pf.LoadAnchor(sshuttleAnchor, rules); err != nil {
		t.Fatalf("загрузить имитацию sshuttle в якорь %s: %v\nправила:\n%s", sshuttleAnchor, err, rules)
	}
	h.killStates(t)
	time.Sleep(settleDelay)
}

// flush очищает якорь и убеждается, что он действительно пуст.
//
// Проверка не лишняя: если бы якорь остался с правилами, шаг «туннель упал»
// проверял бы не то, что думает. Ошибку самой команды считаем всего лишь
// предупреждением (очистка никогда не загружавшегося якоря — это норма),
// а вот непустой якорь после очистки — уже провал.
func (h *harness) flush(t *testing.T, anchor string) {
	t.Helper()

	if err := h.pf.FlushAnchor(anchor); err != nil {
		t.Logf("предупреждение: очистка якоря %s вернула ошибку: %v", anchor, err)
	}
	rules, err := h.pf.AnchorRules(anchor)
	if err != nil {
		t.Fatalf("прочитать правила якоря %s после очистки: %v", anchor, err)
	}
	if strings.TrimSpace(rules) != "" {
		t.Fatalf("якорь %s не очистился, в нём осталось:\n%s", anchor, rules)
	}
	// Правила трансляции (rdr) живут отдельным списком, `-s rules` их не покажет.
	nat, _, err := h.pf.Run(nil, "-a", anchor, "-s", "nat")
	if err != nil {
		t.Fatalf("прочитать правила трансляции якоря %s после очистки: %v", anchor, err)
	}
	if strings.TrimSpace(nat) != "" {
		t.Fatalf("в якоре %s остались правила трансляции:\n%s", anchor, nat)
	}
	time.Sleep(settleDelay)
}

// killStates сбрасывает состояния к цели, чтобы следующий шаг мерил новые
// правила, а не переживший смену правил живой поток.
// Loopback намеренно не трогаем: на живой машине это порвало бы чужие
// локальные соединения.
func (h *harness) killStates(t *testing.T) {
	t.Helper()

	if err := h.pf.KillStates(h.cidr()); err != nil {
		t.Logf("предупреждение: не удалось сбросить состояния к %s: %v", h.cidr(), err)
	}
}

// dumpRules собирает состояние pf для сообщения об ошибке: без него разбирать
// провал на чужой машине невозможно.
func (h *harness) dumpRules(t *testing.T) string {
	t.Helper()

	var b strings.Builder
	b.WriteString("--- состояние pf на момент провала ---\n")
	if main, err := h.pf.MainRules(); err == nil {
		b.WriteString("главный набор правил:\n" + main)
	} else {
		fmt.Fprintf(&b, "главный набор правил: ошибка чтения: %v\n", err)
	}
	for _, a := range []string{e2eAnchor, sshuttleAnchor} {
		rules, err := h.pf.AnchorRules(a)
		if err != nil {
			fmt.Fprintf(&b, "якорь %s: ошибка чтения: %v\n", a, err)
			continue
		}
		fmt.Fprintf(&b, "якорь %s (фильтрация):\n%s", a, rules)
		if nat, _, err := h.pf.Run(nil, "-a", a, "-s", "nat"); err == nil && strings.TrimSpace(nat) != "" {
			fmt.Fprintf(&b, "якорь %s (трансляция):\n%s", a, nat)
		}
	}
	return b.String()
}
