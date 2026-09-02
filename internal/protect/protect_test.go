package protect

import (
	"strings"
	"testing"

	"github.com/tasticolly/splitr/internal/config"
)

func testConfig(mode config.ProtectionMode) config.Config {
	cfg := config.Default()
	cfg.Subnets = []string{"10.0.0.0/9", "192.168.0.0/16", "198.51.100.10/32", "203.0.113.0/24"}
	cfg.Excludes = []string{"192.168.1.0/24"}
	cfg.Protection.Mode = mode
	cfg.Protection.Allow = []string{"192.168.1.0/24"}
	return cfg
}

func TestBuildModePublicKeepsOnlyRoutableNets(t *testing.T) {
	rs := Build(testConfig(config.ModePublic), config.Profile{}, false)
	want := []string{"198.51.100.10/32", "203.0.113.0/24"}
	if strings.Join(rs.Block, ",") != strings.Join(want, ",") {
		t.Fatalf("Block = %v, ожидалось %v", rs.Block, want)
	}
}

func TestBuildModeOffBlocksNothing(t *testing.T) {
	if rs := Build(testConfig(config.ModeOff), config.Profile{}, false); !rs.Empty() {
		t.Fatalf("при mode=off блокировать нечего, получено %v", rs.Block)
	}
}

// Порядок правил критичен: без quick выигрывает последнее совпадение,
// поэтому pass с исключениями обязан идти после block.
func TestRulesOrderNormalMode(t *testing.T) {
	rules := string(Build(testConfig(config.ModeAll), config.Profile{}, false).Rules())
	block, pass := strings.Index(rules, "block drop out"), strings.Index(rules, "pass out")
	if block < 0 || pass < 0 {
		t.Fatalf("нет block или pass:\n%s", rules)
	}
	if block > pass {
		t.Fatalf("block должен идти до pass:\n%s", rules)
	}
	if strings.Contains(rules, "quick") {
		t.Fatalf("в обычном режиме quick недопустим, иначе sshuttle не сможет перебить блок:\n%s", rules)
	}
}

// В panic-режиме правила quick, и первое совпадение останавливает разбор,
// поэтому исключения обязаны идти до блокировки.
func TestRulesOrderStrictMode(t *testing.T) {
	rules := string(Build(testConfig(config.ModeAll), config.Profile{}, true).Rules())
	if !strings.Contains(rules, "quick") {
		t.Fatalf("в panic-режиме ожидается quick:\n%s", rules)
	}
	if strings.Index(rules, "pass out") > strings.Index(rules, "block drop out") {
		t.Fatalf("в panic-режиме pass должен идти до block:\n%s", rules)
	}
}

// Блокировка не должна трогать lo0: sshuttle заворачивает трафик именно туда.
func TestRulesNeverTouchLoopback(t *testing.T) {
	rules := string(Build(testConfig(config.ModeAll), config.Profile{}, false).Rules())
	for _, line := range strings.Split(rules, "\n") {
		if strings.HasPrefix(line, "block") && !strings.Contains(line, "on ! lo0") {
			t.Fatalf("правило блокировки без ограничения `on ! lo0`: %q", line)
		}
	}
}

func TestProfileSubnetsOverrideGlobal(t *testing.T) {
	cfg := testConfig(config.ModeAll)
	p := config.Profile{Subnets: []string{"172.16.0.0/12"}}
	rs := Build(cfg, p, false)
	if len(rs.Block) != 1 || rs.Block[0] != "172.16.0.0/12" {
		t.Fatalf("Block = %v, ожидалось [172.16.0.0/12]", rs.Block)
	}
}
