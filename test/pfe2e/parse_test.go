//go:build darwin

// Тесты разбора правил настоящим парсером pf. Root не нужен: `pfctl -n -f -`
// только разбирает набор правил и ничего не применяет, поэтому такие тесты
// можно (и нужно) гонять на каждом коммите.
//
// Смысл: splitr рендерит правила строками, и любая опечатка или новая опция
// (log, quick, порядок ключевых слов) ломает загрузку якоря молча — демон
// узнает об этом только на живой машине. Здесь ошибка вылезает сразу.

package pfe2e

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"

	"github.com/tasticolly/splitr/internal/config"
	"github.com/tasticolly/splitr/internal/pfctl"
	"github.com/tasticolly/splitr/internal/protect"
)

// parseAnchor — имя якоря для пробного разбора. Правила в него не попадают
// (флаг -n), но pfctl требует имя, чтобы разрешить конструкции уровня якоря.
const parseAnchor = "splitr_parsecheck"

// baseConfig — минимальный валидный конфиг: один профиль и набор сетей,
// похожий на боевой (публичные диапазоны + приватные + LAN-исключение).
func baseConfig() config.Config {
	cfg := config.Default()
	cfg.Subnets = []string{
		"10.0.0.0/9",
		"172.16.0.0/12",
		"192.168.100.0/24",
		"212.24.32.0/19",
		"195.34.32.0/19",
	}
	cfg.Excludes = []string{"192.168.1.0/24"}
	cfg.Profiles = map[string]config.Profile{
		"pc": {Remote: "user@example.com"},
	}
	return cfg
}

func TestGeneratedRulesParsedByPf(t *testing.T) {
	requirePfctl(t)

	cases := []struct {
		name  string
		tune  func(*config.Config)
		panic bool
	}{
		{name: "режим all", tune: func(c *config.Config) { c.Protection.Mode = config.ModeAll }},
		{name: "режим public", tune: func(c *config.Config) { c.Protection.Mode = config.ModePublic }},
		{
			name: "режим custom",
			tune: func(c *config.Config) {
				c.Protection.Mode = config.ModeCustom
				c.Protection.Block = []string{"212.24.32.0/19", "10.77.0.0/16"}
			},
		},
		{name: "режим off", tune: func(c *config.Config) { c.Protection.Mode = config.ModeOff }},
		{name: "panic поверх all", tune: func(c *config.Config) { c.Protection.Mode = config.ModeAll }, panic: true},
		{
			name: "panic поверх custom",
			tune: func(c *config.Config) {
				c.Protection.Mode = config.ModeCustom
				c.Protection.Block = []string{"212.24.32.0/19"}
			},
			panic: true,
		},
		{
			name: "block_dns включён",
			tune: func(c *config.Config) {
				c.Protection.Mode = config.ModeAll
				c.Protection.BlockDNS = true
				c.Protection.DNSServers = []string{"10.0.0.53", "10.0.0.54"}
			},
		},
		{
			name: "block_dns вместе с panic",
			tune: func(c *config.Config) {
				c.Protection.Mode = config.ModeAll
				c.Protection.BlockDNS = true
				c.Protection.DNSServers = []string{"10.0.0.53"}
			},
			panic: true,
		},
		{
			// Пустая таблица исключений: pass-правила быть не должно,
			// а `table <...> persist { }` pf не принял бы.
			name: "пустые исключения",
			tune: func(c *config.Config) {
				c.Protection.Mode = config.ModeAll
				c.Excludes = nil
				c.Protection.Allow = nil
			},
		},
		{
			// Пустой список блокировки при живых исключениях: якорь не должен
			// ссылаться на несуществующую таблицу.
			name: "пустая блокировка",
			tune: func(c *config.Config) {
				c.Protection.Mode = config.ModeAll
				c.Subnets = nil
				c.Protection.Allow = []string{"192.168.1.0/24"}
			},
		},
		{
			name: "только DNS без сетей",
			tune: func(c *config.Config) {
				c.Protection.Mode = config.ModeAll
				c.Subnets = nil
				c.Protection.BlockDNS = true
				c.Protection.DNSServers = []string{"10.0.0.53"}
			},
		},
		{
			name: "исключения из protection.allow",
			tune: func(c *config.Config) {
				c.Protection.Mode = config.ModeAll
				c.Protection.Allow = []string{"192.168.88.0/24", "10.10.10.10/32"}
			},
		},
		{
			name: "одиночные адреса /32",
			tune: func(c *config.Config) {
				c.Protection.Mode = config.ModeCustom
				c.Protection.Block = []string{"1.1.1.1/32"}
				c.Protection.Allow = []string{"8.8.8.8/32"}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseConfig()
			tc.tune(&cfg)
			// Конфиг обязан оставаться валидным: иначе тест проверял бы
			// то, чего демон никогда не сгенерирует.
			if err := cfg.Validate(); err != nil {
				t.Fatalf("конфиг случая невалиден, тест проверял бы несуществующий сценарий: %v", err)
			}
			rs := protect.Build(cfg, cfg.Profiles["pc"], tc.panic)
			rules := rs.Rules()
			t.Logf("сгенерированные правила:\n%s", rules)
			assertPfParses(t, rules)
		})
	}
}

// TestLogVariantParsed — опция protection.log добавляет ключевое слово log
// в block-правила. Порядок ключевых слов в pf строгий, поэтому проверяем
// сочетание log с quick отдельно.
func TestLogVariantParsed(t *testing.T) {
	requirePfctl(t)

	for _, panicMode := range []bool{false, true} {
		cfg := baseConfig()
		cfg.Protection.Mode = config.ModeAll
		cfg.Protection.BlockDNS = true
		cfg.Protection.DNSServers = []string{"10.0.0.53"}
		cfg.Protection.Log = true

		rs := protect.Build(cfg, cfg.Profiles["pc"], panicMode)
		rules := rs.Rules()
		t.Logf("panic=%v правила:\n%s", panicMode, rules)
		assertPfParses(t, rules)
	}
}

// TestRuleShapeInvariants фиксирует форму правил, на которой держится весь
// продукт. Это не про синтаксис, а про семантику, и проверяется без root.
func TestRuleShapeInvariants(t *testing.T) {
	cfg := baseConfig()
	cfg.Protection.Mode = config.ModeAll

	normal := string(protect.Build(cfg, cfg.Profiles["pc"], false).Rules())

	// 1. Без quick — иначе якорь sshuttle, стоящий ниже, уже не сможет
	//    перебить блокировку и туннель перестанет работать вовсе.
	if strings.Contains(normal, "quick") {
		t.Errorf("в обычном режиме правила не должны содержать quick, иначе sshuttle не сможет их перебить:\n%s", normal)
	}
	// 2. Ограничение on ! lo0 — sshuttle заворачивает трафик именно на lo0,
	//    блокировка на этом интерфейсе порвала бы сам туннель.
	for _, line := range strings.Split(normal, "\n") {
		if !strings.HasPrefix(line, "block") {
			continue
		}
		if !strings.Contains(line, "on ! lo0") {
			t.Errorf("block-правило без ограничения `on ! lo0` порвёт туннель: %q", line)
		}
	}
	// 3. pass-правило исключений идёт ПОСЛЕ block: выигрывает последнее
	//    совпадение, значит исключения должны стоять ниже.
	blockAt := strings.Index(normal, "block drop out")
	passAt := strings.Index(normal, "pass out")
	if blockAt < 0 || passAt < 0 {
		t.Fatalf("ожидались и block, и pass правила:\n%s", normal)
	}
	if passAt < blockAt {
		t.Errorf("pass-исключения обязаны идти после block (last-match-wins):\n%s", normal)
	}

	// 4. В panic-режиме всё наоборот: quick останавливает разбор на первом
	//    совпадении, поэтому исключения должны стоять ВЫШЕ блокировки.
	panicRules := string(protect.Build(cfg, cfg.Profiles["pc"], true).Rules())
	if !strings.Contains(panicRules, "quick") {
		t.Errorf("panic-режим обязан использовать quick, иначе живой sshuttle его перебьёт:\n%s", panicRules)
	}
	pBlockAt := strings.Index(panicRules, "block drop out")
	pPassAt := strings.Index(panicRules, "pass out")
	if pBlockAt < 0 || pPassAt < 0 {
		t.Fatalf("ожидались и block, и pass правила в panic:\n%s", panicRules)
	}
	if pPassAt > pBlockAt {
		t.Errorf("в panic-режиме (quick) исключения обязаны идти до блокировки:\n%s", panicRules)
	}
}

// TestSshuttleImitationParsed — правила, которыми e2e-тест изображает sshuttle,
// должны быть валидны сами по себе, иначе e2e упадёт по глупой причине уже
// под root. Проверяем заранее и бесплатно.
func TestSshuttleImitationParsed(t *testing.T) {
	requirePfctl(t)
	assertPfParses(t, sshuttleAnchorRules("1.1.1.1", 443, 12300))
}

// TestMainConfWithTestAnchorsParsed — временный главный набор правил, который
// e2e-тест подставляет вместо /etc/pf.conf, обязан разбираться. В pf строгий
// порядок секций (translation до filtering), и вставка rdr-anchor не в то
// место — самая вероятная ошибка.
func TestMainConfWithTestAnchorsParsed(t *testing.T) {
	requirePfctl(t)

	base, err := readMainConf()
	if err != nil {
		t.Skipf("нет доступа к %s: %v", protect.PfConf, err)
	}
	conf, err := buildTestMainConf(base)
	if err != nil {
		t.Fatalf("собрать временный pf.conf: %v", err)
	}
	t.Logf("временный pf.conf:\n%s", conf)

	// Здесь нужен разбор именно главного набора, без -a.
	cmd := exec.Command(pfctl.DefaultBinary, "-n", "-f", "-")
	cmd.Stdin = bytes.NewReader(conf)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("временный pf.conf не разбирается pfctl: %v\n%s", err, out.String())
	}
}

// assertPfParses скармливает правила настоящему парсеру pf.
// Флаг -n означает «разобрать и не применять», поэтому root не нужен.
func assertPfParses(t *testing.T, rules []byte) {
	t.Helper()

	cmd := exec.Command(pfctl.DefaultBinary, "-a", parseAnchor, "-n", "-f", "-")
	cmd.Stdin = bytes.NewReader(rules)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	text := out.String()

	if err != nil {
		t.Fatalf("pf не разобрал правила: %v\nправила:\n%s\nвывод pfctl:\n%s", err, rules, text)
	}
	// pfctl умеет предупреждать и при нулевом коде возврата, поэтому смотрим
	// и в текст — но игнорируем штатное предупреждение про -f.
	for _, line := range strings.Split(text, "\n") {
		l := strings.TrimSpace(line)
		if l == "" || strings.Contains(l, "Use of -f option") ||
			strings.Contains(l, "present in the main ruleset") ||
			strings.Contains(l, "See /etc/pf.conf") {
			continue
		}
		if strings.Contains(strings.ToLower(l), "error") || strings.Contains(l, "syntax") {
			t.Errorf("pfctl пожаловался на правила: %s\nправила:\n%s", l, rules)
		}
	}
}

func requirePfctl(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath(pfctl.DefaultBinary); err != nil {
		t.Skipf("нет %s — тест имеет смысл только на macOS с pf: %v", pfctl.DefaultBinary, err)
	}
}
