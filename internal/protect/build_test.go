package protect

import (
	"errors"
	"strings"
	"testing"

	"github.com/tasticolly/splitr/internal/config"
	"github.com/tasticolly/splitr/internal/pfctl/pftest"
)

// TestBuildModeMatrix — что попадает в Block для каждого режима.
func TestBuildModeMatrix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		mode  config.ProtectionMode
		block []string // содержимое protect.block
		want  []string // ожидаемый Block, уже отсортированный
	}{
		{
			name: "all режет все маршрутизируемые сети",
			mode: config.ModeAll,
			want: []string{"10.0.0.0/9", "192.168.0.0/16", "198.51.100.10/32", "203.0.113.0/24"},
		},
		{
			name: "public оставляет только публичные адреса",
			mode: config.ModePublic,
			want: []string{"198.51.100.10/32", "203.0.113.0/24"},
		},
		{
			name:  "custom режет ровно protect.block",
			mode:  config.ModeCustom,
			block: []string{"11.0.0.0/8"},
			want:  []string{"11.0.0.0/8"},
		},
		{
			name: "off не режет ничего",
			mode: config.ModeOff,
			want: nil,
		},
		{
			name: "неизвестный режим ведёт себя как all",
			mode: "чепуха",
			want: []string{"10.0.0.0/9", "192.168.0.0/16", "198.51.100.10/32", "203.0.113.0/24"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := testConfig(tt.mode)
			cfg.Protection.Block = tt.block
			rs := Build(cfg, config.Profile{}, false)
			if strings.Join(rs.Block, ",") != strings.Join(tt.want, ",") {
				t.Fatalf("Block = %v, ожидалось %v", rs.Block, tt.want)
			}
		})
	}
}

// В режиме off отбрасывается вообще всё, включая исключения и DNS.
func TestBuildModeOffDropsEverything(t *testing.T) {
	t.Parallel()

	cfg := testConfig(config.ModeOff)
	cfg.Protection.BlockDNS = true
	cfg.Protection.DNSServers = []string{"10.0.0.1"}
	rs := Build(cfg, config.Profile{}, false)
	if !rs.Empty() || len(rs.Pass) > 0 || rs.Strict {
		t.Fatalf("при mode=off ожидался пустой Ruleset, получено %+v", rs)
	}
}

// Strict обязан резать даже при mode: off — иначе кнопка «перекрыть всё»
// молча не делала бы ничего именно там, где защита и так была снята.
func TestBuildStrictOverridesModeOff(t *testing.T) {
	t.Parallel()

	cfg := testConfig(config.ModeOff)
	rs := Build(cfg, config.Profile{}, true)
	if rs.Empty() {
		t.Fatal("strict при mode=off обязан блокировать защищаемые сети")
	}
	if !rs.Strict || !strings.Contains(string(rs.Rules()), "quick") {
		t.Fatalf("правила panic обязаны быть quick:\n%s", rs.Rules())
	}
	// Исключения сохраняются: домашний LAN нельзя ронять даже в panic.
	if len(rs.Pass) == 0 {
		t.Fatal("исключения обязаны сохраняться и в panic-режиме")
	}
}

// Pass — объединение excludes конфига, excludes профиля и protect.allow.
func TestBuildPassTableMergesExcludesAndAllow(t *testing.T) {
	t.Parallel()

	cfg := testConfig(config.ModeAll)
	cfg.Excludes = []string{"192.168.1.0/24"}
	cfg.Protection.Allow = []string{"10.10.10.0/24", "192.168.1.0/24"}
	rs := Build(cfg, config.Profile{Excludes: []string{"192.168.5.0/24"}}, false)

	want := "10.10.10.0/24,192.168.1.0/24,192.168.5.0/24"
	if strings.Join(rs.Pass, ",") != want {
		t.Fatalf("Pass = %v, ожидалось %s (без дублей и по возрастанию)", rs.Pass, want)
	}
}

func TestBuildDedupesAndSortsBlock(t *testing.T) {
	t.Parallel()

	cfg := testConfig(config.ModeAll)
	cfg.Subnets = []string{"203.0.113.0/24", "10.0.0.0/9", "203.0.113.0/24", "11.0.0.0/8"}
	rs := Build(cfg, config.Profile{}, false)
	want := "10.0.0.0/9,11.0.0.0/8,203.0.113.0/24"
	if strings.Join(rs.Block, ",") != want {
		t.Fatalf("Block = %v, ожидалось %s", rs.Block, want)
	}
}

// Мусорные записи не должны попадать в правила public-режима:
// Validate ловит их раньше, но Build обязан не падать и на них.
func TestBuildPublicSkipsUnparsableEntries(t *testing.T) {
	t.Parallel()

	cfg := testConfig(config.ModePublic)
	cfg.Subnets = []string{"не-сеть", "203.0.113.0/24"}
	rs := Build(cfg, config.Profile{}, false)
	if strings.Join(rs.Block, ",") != "203.0.113.0/24" {
		t.Fatalf("Block = %v, ожидалось только 203.0.113.0/24", rs.Block)
	}
}

func TestBuildDNSServersOnlyWhenBlockDNSEnabled(t *testing.T) {
	t.Parallel()

	cfg := testConfig(config.ModeAll)
	cfg.Protection.DNSServers = []string{"10.0.0.1", "10.0.0.1", "10.0.0.2"}

	if rs := Build(cfg, config.Profile{}, false); len(rs.DNSServers) != 0 {
		t.Fatalf("при block_dns=false DNSServers должен быть пуст, получено %v", rs.DNSServers)
	}

	cfg.Protection.BlockDNS = true
	rs := Build(cfg, config.Profile{}, true)
	if strings.Join(rs.DNSServers, ",") != "10.0.0.1,10.0.0.2" {
		t.Fatalf("DNSServers = %v, ожидались уникальные и отсортированные", rs.DNSServers)
	}
	if !rs.Strict {
		t.Fatal("strictMode должен переноситься в Ruleset.Strict")
	}
}

// Профильные excludes добавляются к глобальным, а профильные subnets их заменяют.
func TestBuildProfileExcludesAddToGlobalOnes(t *testing.T) {
	t.Parallel()

	cfg := testConfig(config.ModeAll)
	rs := Build(cfg, config.Profile{Excludes: []string{"172.20.0.0/16"}}, false)
	if !contains(rs.Pass, "192.168.1.0/24") || !contains(rs.Pass, "172.20.0.0/16") {
		t.Fatalf("Pass = %v, ожидались и глобальные, и профильные исключения", rs.Pass)
	}
}

func TestEmpty(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		rs   Ruleset
		want bool
	}{
		{name: "пустой", rs: Ruleset{}, want: true},
		{name: "только исключения — резать нечего", rs: Ruleset{Pass: []string{"10.0.0.0/8"}}, want: true},
		{name: "есть block", rs: Ruleset{Block: []string{"10.0.0.0/8"}}},
		{name: "есть только DNS", rs: Ruleset{DNSServers: []string{"10.0.0.1"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.rs.Empty(); got != tt.want {
				t.Fatalf("Empty() = %v, ожидалось %v", got, tt.want)
			}
		})
	}
}

func TestRulesForEmptyRulesetIsJustAComment(t *testing.T) {
	t.Parallel()

	rules := string(Ruleset{}.Rules())
	if strings.Contains(rules, "block") || strings.Contains(rules, "pass") {
		t.Fatalf("выключенный Ð·Ð°ÑÐ¸ÑÐ° не должен порождать правил:\n%s", rules)
	}
	if !strings.HasPrefix(rules, "#") {
		t.Fatalf("ожидался комментарий, получено:\n%s", rules)
	}
}

// Таблицы должны попадать в текст ровно с тем содержимым, которое посчитал Build.
func TestRulesContainsTables(t *testing.T) {
	t.Parallel()

	cfg := testConfig(config.ModeAll)
	cfg.Protection.BlockDNS = true
	cfg.Protection.DNSServers = []string{"10.0.0.1"}
	rules := string(Build(cfg, config.Profile{}, false).Rules())

	for _, want := range []string{
		"table <splitr_block> persist { 10.0.0.0/9, 192.168.0.0/16, 198.51.100.10/32, 203.0.113.0/24 }",
		"table <splitr_pass> persist { 192.168.1.0/24 }",
		"table <splitr_dns> persist { 10.0.0.1 }",
		"block drop out on ! lo0 inet from any to <splitr_block>",
		"block drop out on ! lo0 inet proto { tcp udp } from any to <splitr_dns> port 53",
		"pass out on ! lo0 inet from any to <splitr_pass>",
	} {
		if !strings.Contains(rules, want) {
			t.Errorf("в правилах нет строки %q:\n%s", want, rules)
		}
	}
}

// Пустая таблица исключений не должна порождать ни таблицы, ни pass-правила:
// `table <...> { }` в pf — синтаксическая ошибка, а pass без таблицы открыл бы всё.
func TestRulesWithoutPassEntriesHasNoPassRule(t *testing.T) {
	t.Parallel()

	cfg := testConfig(config.ModeAll)
	cfg.Excludes = nil
	cfg.Protection.Allow = nil
	rules := string(Build(cfg, config.Profile{}, false).Rules())

	if strings.Contains(rules, "pass out") {
		t.Fatalf("без исключений pass-правило не нужно:\n%s", rules)
	}
	if strings.Contains(rules, "splitr_pass") {
		t.Fatalf("пустая таблица исключений не должна объявляться:\n%s", rules)
	}
}

// Режим «резать только DNS»: block-таблицы нет, но правило по порту 53 есть.
func TestRulesDNSOnly(t *testing.T) {
	t.Parallel()

	rs := Ruleset{DNSServers: []string{"10.0.0.1"}}
	rules := string(rs.Rules())
	if strings.Contains(rules, "splitr_block") {
		t.Fatalf("без сетей для блокировки таблица block не нужна:\n%s", rules)
	}
	if !strings.Contains(rules, "port 53") {
		t.Fatalf("ожидалось правило блокировки DNS:\n%s", rules)
	}
}

// Ни одно правило блокировки не должно действовать на lo0 —
// именно туда sshuttle заворачивает перехваченный трафик.
func TestRulesEveryBlockRuleSkipsLoopback(t *testing.T) {
	t.Parallel()

	for _, strictMode := range []bool{false, true} {
		cfg := testConfig(config.ModeAll)
		cfg.Protection.BlockDNS = true
		cfg.Protection.DNSServers = []string{"10.0.0.1"}
		rules := string(Build(cfg, config.Profile{}, strictMode).Rules())

		n := 0
		for _, line := range strings.Split(rules, "\n") {
			if !strings.HasPrefix(line, "block") {
				continue
			}
			n++
			if !strings.Contains(line, "on ! lo0") {
				t.Fatalf("panic=%v: правило блокировки без `on ! lo0`: %q", strictMode, line)
			}
		}
		if n != 2 {
			t.Fatalf("panic=%v: ожидалось два правила блокировки (сети и DNS), получено %d:\n%s", strictMode, n, rules)
		}
	}
}

// Порядок правил — суть механизма, поэтому проверяется целиком.
func TestRulesOrderMatrix(t *testing.T) {
	t.Parallel()

	cfg := testConfig(config.ModeAll)
	cfg.Protection.BlockDNS = true
	cfg.Protection.DNSServers = []string{"10.0.0.1"}

	t.Run("обычный режим: block до pass, quick нигде нет", func(t *testing.T) {
		t.Parallel()
		rules := string(Build(cfg, config.Profile{}, false).Rules())
		blockIdx := strings.Index(rules, "block drop out on ! lo0 inet from any")
		dnsIdx := strings.Index(rules, "port 53")
		passIdx := strings.Index(rules, "pass out")
		if blockIdx < 0 || dnsIdx < 0 || passIdx < 0 {
			t.Fatalf("не все правила на месте:\n%s", rules)
		}
		if !(blockIdx < dnsIdx && dnsIdx < passIdx) {
			t.Fatalf("ожидался порядок block, dns, pass:\n%s", rules)
		}
		if strings.Contains(rules, "quick") {
			t.Fatalf("quick в обычном режиме не даст sshuttle перебить блок:\n%s", rules)
		}
	})

	t.Run("panic: pass до block и всё quick", func(t *testing.T) {
		t.Parallel()
		rules := string(Build(cfg, config.Profile{}, true).Rules())
		passIdx := strings.Index(rules, "pass out")
		blockIdx := strings.Index(rules, "block drop out")
		if passIdx < 0 || blockIdx < 0 || passIdx > blockIdx {
			t.Fatalf("в panic-режиме pass обязан идти до block:\n%s", rules)
		}
		for _, line := range strings.Split(rules, "\n") {
			if !strings.HasPrefix(line, "block") && !strings.HasPrefix(line, "pass") {
				continue
			}
			if !strings.Contains(line, "quick") {
				t.Fatalf("в panic-режиме каждое правило должно быть quick, а это — нет: %q", line)
			}
		}
	})
}

func TestApplyLoadsRulesIntoAnchor(t *testing.T) {
	t.Parallel()

	pf := pftest.New()
	rs := Build(testConfig(config.ModeAll), config.Profile{}, false)
	if err := rs.Apply(pf); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := pf.AnchorText(Anchor); got != string(rs.Rules()) {
		t.Fatalf("в якорь %s загружено не то:\n%s", Anchor, got)
	}
}

func TestApplyPropagatesError(t *testing.T) {
	t.Parallel()

	pf := pftest.New()
	boom := errors.New("pf недоступен")
	pf.Fail(pftest.MethodLoadAnchor, boom)
	if err := Build(testConfig(config.ModeAll), config.Profile{}, false).Apply(pf); !errors.Is(err, boom) {
		t.Fatalf("Apply вернул %v, ожидалась ошибка pf", err)
	}
}

func TestKillStatesCoversEveryBlockedNet(t *testing.T) {
	t.Parallel()

	pf := pftest.New()
	rs := Build(testConfig(config.ModeAll), config.Profile{}, false)
	if err := rs.KillStates(pf); err != nil {
		t.Fatalf("KillStates: %v", err)
	}
	if strings.Join(pf.KilledStates(), ",") != strings.Join(rs.Block, ",") {
		t.Fatalf("сброшены состояния для %v, ожидалось %v", pf.KilledStates(), rs.Block)
	}
}

// Ошибка на одной сети не должна прерывать сброс остальных.
func TestKillStatesCollectsAllErrors(t *testing.T) {
	t.Parallel()

	pf := pftest.New()
	pf.Fail(pftest.MethodKillStates, errors.New("нет прав"))
	rs := Build(testConfig(config.ModeAll), config.Profile{}, false)

	err := rs.KillStates(pf)
	if err == nil {
		t.Fatal("ожидалась ошибка сброса состояний")
	}
	if got := strings.Count(err.Error(), "нет прав"); got != len(rs.Block) {
		t.Fatalf("в ошибке %d упоминаний вместо %d — часть сетей пропущена: %v", got, len(rs.Block), err)
	}
	if n := pf.CallCount(pftest.MethodKillStates); n != len(rs.Block) {
		t.Fatalf("KillStates вызван %d раз вместо %d", n, len(rs.Block))
	}
}

func TestKillStatesOnEmptyRulesetDoesNothing(t *testing.T) {
	t.Parallel()

	pf := pftest.New()
	if err := (Ruleset{}).KillStates(pf); err != nil {
		t.Fatalf("KillStates на пустом наборе: %v", err)
	}
	if len(pf.Calls()) != 0 {
		t.Fatalf("pf не должен вызываться вовсе, вызовы: %v", pf.Calls())
	}
}

func TestInstalled(t *testing.T) {
	t.Parallel()

	rules := Build(testConfig(config.ModeAll), config.Profile{}, false).Rules()

	tests := []struct {
		name           string
		setup          func(*pftest.Fake)
		wantLoaded     bool
		wantReferenced bool
	}{
		{
			name: "правила загружены и якорь подключён",
			setup: func(f *pftest.Fake) {
				_ = f.LoadAnchor(Anchor, rules)
				f.LinkAnchor(Anchor)
			},
			wantLoaded:     true,
			wantReferenced: true,
		},
		{
			name:  "якорь пуст и не подключён",
			setup: func(*pftest.Fake) {},
		},
		{
			name: "правила загружены, но pf.conf якорь не зовёт",
			setup: func(f *pftest.Fake) {
				_ = f.LoadAnchor(Anchor, rules)
			},
			wantLoaded: true,
		},
		{
			name: "якорь подключён, но правила смыло чужим pfctl -F all",
			setup: func(f *pftest.Fake) {
				_ = f.LoadAnchor(Anchor, rules)
				f.LinkAnchor(Anchor)
				f.FlushAll()
			},
			wantReferenced: true,
		},
		{
			// Из одних комментариев правил не выходит: pfctl такой якорь
			// покажет пустым, значит и загруженным он не считается.
			name: "в якоре лежит заглушка выключенного Ð·Ð°ÑÐ¸ÑÐ°",
			setup: func(f *pftest.Fake) {
				_ = f.LoadAnchor(Anchor, Ruleset{}.Rules())
				f.LinkAnchor(Anchor)
			},
			wantReferenced: true,
		},
		{
			// Конфиг только с block_dns не порождает правил «block drop out»,
			// но якорь при этом применён и работает.
			name: "в якоре только правило блокировки DNS",
			setup: func(f *pftest.Fake) {
				_ = f.LoadAnchor(Anchor, Ruleset{DNSServers: []string{"10.0.0.1"}}.Rules())
				f.LinkAnchor(Anchor)
			},
			wantLoaded:     true,
			wantReferenced: true,
		},
		{
			name: "чужой якорь с похожим именем не считается нашим",
			setup: func(f *pftest.Fake) {
				_ = f.LoadAnchor(Anchor, rules)
				f.LinkAnchor("splitr-old")
			},
			wantLoaded: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			pf := pftest.New()
			tt.setup(pf)
			loaded, referenced, err := Installed(pf)
			if err != nil {
				t.Fatalf("Installed: %v", err)
			}
			if loaded != tt.wantLoaded || referenced != tt.wantReferenced {
				t.Fatalf("Installed = (%v, %v), ожидалось (%v, %v)", loaded, referenced, tt.wantLoaded, tt.wantReferenced)
			}
		})
	}
}

func TestInstalledPropagatesErrors(t *testing.T) {
	t.Parallel()

	t.Run("ошибка чтения якоря", func(t *testing.T) {
		t.Parallel()
		pf := pftest.New()
		boom := errors.New("якорь не читается")
		pf.Fail(pftest.MethodAnchorRules, boom)
		if _, _, err := Installed(pf); !errors.Is(err, boom) {
			t.Fatalf("Installed вернул %v, ожидалась ошибка чтения якоря", err)
		}
	})

	t.Run("ошибка чтения главного набора", func(t *testing.T) {
		t.Parallel()
		pf := pftest.New()
		boom := errors.New("правила не читаются")
		pf.Fail(pftest.MethodMainRules, boom)
		if _, _, err := Installed(pf); !errors.Is(err, boom) {
			t.Fatalf("Installed вернул %v, ожидалась ошибка чтения правил", err)
		}
	})
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// protect.log добавляет в правила блокировки ключевое слово log:
// без него в pflog0 ничего не попадёт и `splitr blocked` останется пустым.
func TestRulesLogKeyword(t *testing.T) {
	t.Parallel()

	cfg := testConfig(config.ModeAll)
	cfg.Protection.BlockDNS = true
	cfg.Protection.DNSServers = []string{"10.0.0.1"}

	t.Run("по умолчанию журналирования нет", func(t *testing.T) {
		t.Parallel()
		rules := string(Build(cfg, config.Profile{}, false).Rules())
		if strings.Contains(rules, " log ") {
			t.Fatalf("при protect.log=false ключевого слова log быть не должно:\n%s", rules)
		}
	})

	t.Run("log включается на всех правилах блокировки", func(t *testing.T) {
		t.Parallel()
		withLog := cfg
		withLog.Protection.Log = true
		rs := Build(withLog, config.Profile{}, false)
		if !rs.Log {
			t.Fatal("флаг Log не перенесён в Ruleset")
		}
		for _, line := range strings.Split(string(rs.Rules()), "\n") {
			if strings.HasPrefix(line, "block") && !strings.Contains(line, "log") {
				t.Fatalf("правило блокировки без log: %q", line)
			}
		}
	})

	t.Run("в panic-режиме log стоит перед quick", func(t *testing.T) {
		t.Parallel()
		withLog := cfg
		withLog.Protection.Log = true
		rules := string(Build(withLog, config.Profile{}, true).Rules())
		if !strings.Contains(rules, "block drop out log quick on ! lo0") {
			t.Fatalf("ожидался канонический порядок `log quick`:\n%s", rules)
		}
	})
}

// Правила сначала проверяются разбором pf и только потом применяются:
// загрузить набор, который pf не примет, — значит остаться без защиты.
func TestApplyChecksRulesBeforeLoading(t *testing.T) {
	t.Parallel()

	pf := pftest.New()
	rs := Build(testConfig(config.ModeAll), config.Profile{}, false)
	if err := rs.Apply(pf); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if strings.Join(pf.Calls(), ",") != "CheckAnchor,LoadAnchor" {
		t.Fatalf("порядок вызовов = %v, ожидалась сначала проверка", pf.Calls())
	}
	if checked := pf.Checked(); len(checked) != 1 || checked[0] != string(rs.Rules()) {
		t.Fatalf("на проверку ушёл не тот набор правил: %v", checked)
	}
}

func TestApplyAbortsWhenRulesDoNotParse(t *testing.T) {
	t.Parallel()

	pf := pftest.New()
	pf.Fail(pftest.MethodCheckAnchor, errors.New("syntax error"))

	err := Build(testConfig(config.ModeAll), config.Profile{}, false).Apply(pf)
	if err == nil {
		t.Fatal("ожидалась ошибка разбора правил")
	}
	if !strings.Contains(err.Error(), "nothing applied") {
		t.Fatalf("ошибка = %v, ожидалось сообщение об отмене применения", err)
	}
	if n := pf.CallCount(pftest.MethodLoadAnchor); n != 0 {
		t.Fatalf("непроходящие разбор правила грузить нельзя, вызовов LoadAnchor: %d", n)
	}
	if got := pf.AnchorText(Anchor); got != "" {
		t.Fatalf("якорь должен остаться нетронутым, в нём:\n%s", got)
	}
}
