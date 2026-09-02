package protect

import (
	"net/netip"
	"strings"
	"testing"

	"github.com/tasticolly/splitr/internal/config"
)

// FuzzRulesRejectInjection проверяет главное свойство генератора: что бы ни
// лежало в конфиге, из этого нельзя собрать лишнюю строку правил pf.
//
// Значения сетей попадают в текст якоря дословно, а якорь грузится в ядро от
// root. Если сквозь проверку конфига просочится строка с переводом строки или
// с закрывающей скобкой, она станет не элементом таблицы, а самостоятельным
// правилом — например `pass out all`, снимающим всю защиту разом.
func FuzzRulesRejectInjection(f *testing.F) {
	f.Add("10.0.0.0/8")
	f.Add("10.0.0.0/8 }\npass out all\ntable <x> {")
	f.Add("192.168.1.0/24, 0.0.0.0/0")
	f.Add("")
	f.Add("не сеть вовсе")

	f.Fuzz(func(t *testing.T, entry string) {
		// Дальше генератора доходит только то, что прошло разбор как CIDR:
		// ровно это и обещает config.Validate.
		if _, err := netip.ParsePrefix(entry); err != nil {
			return
		}

		cfg := config.Default()
		cfg.Subnets = []string{entry}
		cfg.Protection = config.Protection{Enabled: true, Mode: config.ModeCustom, Block: []string{entry}}
		rs := Build(cfg, config.Profile{}, false)
		text := string(rs.Rules())

		for _, line := range strings.Split(text, "\n") {
			line = strings.TrimSpace(line)
			switch {
			case line == "", strings.HasPrefix(line, "#"),
				strings.HasPrefix(line, "table "),
				strings.HasPrefix(line, "block drop out "),
				strings.HasPrefix(line, "pass out "):
			default:
				t.Fatalf("из записи %q получилась посторонняя строка правил %q", entry, line)
			}
		}
	})
}

// FuzzRulesAlwaysBlockWhatWasAsked ловит обратную ошибку: правила собрались,
// но блокировать в них нечего. Молча пустой якорь — это отсутствие защиты,
// а внешне всё выглядит рабочим.
func FuzzRulesAlwaysBlockWhatWasAsked(f *testing.F) {
	f.Add("10.0.0.0/8", "192.168.1.0/24")
	f.Add("0.0.0.0/0", "")
	f.Add("203.0.113.0/24", "203.0.113.0/24")

	f.Fuzz(func(t *testing.T, block, allow string) {
		if _, err := netip.ParsePrefix(block); err != nil {
			return
		}
		cfg := config.Default()
		cfg.Protection = config.Protection{Enabled: true, Mode: config.ModeCustom, Block: []string{block}}
		if _, err := netip.ParsePrefix(allow); err == nil {
			cfg.Protection.Allow = []string{allow}
		}

		rs := Build(cfg, config.Profile{}, false)
		if rs.Empty() {
			t.Fatalf("список блокировки %q превратился в пустой набор правил", block)
		}
		if text := string(rs.Rules()); !strings.Contains(text, "block drop out") {
			t.Fatalf("правила без блокировки при block=%q:\n%s", block, text)
		}
	})
}

// FuzzStrictRulesStayUnconditional следит за тем, чтобы panic никогда не терял
// ключевое слово quick: без него блокировку перебивает якорь sshuttle, и
// «безусловная» защита оказывается условной.
func FuzzStrictRulesStayUnconditional(f *testing.F) {
	f.Add("10.0.0.0/8")
	f.Add("1.2.3.4/32")

	f.Fuzz(func(t *testing.T, block string) {
		if _, err := netip.ParsePrefix(block); err != nil {
			return
		}
		cfg := config.Default()
		cfg.Protection = config.Protection{Enabled: true, Mode: config.ModeCustom, Block: []string{block}}

		text := string(Build(cfg, config.Profile{}, true).Rules())
		if !strings.Contains(text, "block drop out quick") && !strings.Contains(text, "block drop out log quick") {
			t.Fatalf("panic без quick при block=%q:\n%s", block, text)
		}
	})
}
