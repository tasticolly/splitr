//go:build darwin

// Общие для обоих видов тестов кирпичики: имена тестовых якорей, сборка
// временного главного набора правил и правила, изображающие sshuttle.
// Файл намеренно без тега pf_e2e — чтобы всё это проверялось разбором
// без root (см. parse_test.go), а не только на живой машине.

package pfe2e

import (
	"fmt"
	"os"
	"strings"

	"github.com/tasticolly/splitr/internal/protect"
)

const (
	// e2eAnchor — якорь, в который e2e-тест грузит правила, сгенерированные
	// боевым кодом защиты. Отдельное имя, чтобы не трогать боевой якорь
	// "splitr" пользователя.
	e2eAnchor = "splitr_e2e"
	// sshuttleAnchor — якорь-имитация sshuttle. Подключается к главному набору
	// ПОСЛЕ e2eAnchor: именно на этом порядке держится last-match-wins.
	sshuttleAnchor = "splitr_e2e_sshuttle"
)

// readMainConf читает действующий /etc/pf.conf.
func readMainConf() ([]byte, error) {
	return os.ReadFile(protect.PfConf)
}

// buildTestMainConf собирает временный главный набор правил: исходный
// /etc/pf.conf слово в слово плюс два тестовых якоря.
//
// Почему именно так, а не правкой /etc/pf.conf: файл пользователя вообще не
// меняется, а откат сводится к `pfctl -f /etc/pf.conf` — операции, которая
// не зависит от того, успел ли тест что-то записать.
//
// Порядок в pf строгий: секции идут options → normalization → queueing →
// translation → filtering. Поэтому rdr-anchor вставляется сразу после
// последнего существующего rdr-anchor (секция трансляции), а фильтрующие
// якоря дописываются в конец — туда же, куда их дописывает настоящий sshuttle.
func buildTestMainConf(base []byte) ([]byte, error) {
	lines := strings.Split(string(base), "\n")
	lastRdr := -1
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "rdr-anchor") {
			lastRdr = i
		}
	}
	if lastRdr < 0 {
		return nil, fmt.Errorf(
			"в %s нет ни одной строки rdr-anchor: некуда вставить трансляцию имитации sshuttle "+
				"(pf требует, чтобы rdr шёл до фильтрующих правил)", protect.PfConf)
	}

	var b strings.Builder
	for i, l := range lines {
		b.WriteString(l)
		b.WriteString("\n")
		if i == lastRdr {
			fmt.Fprintf(&b, "rdr-anchor \"%s\"\n", sshuttleAnchor)
		}
	}
	b.WriteString("\n# --- splitr e2e: временные якоря, снимаются перезагрузкой /etc/pf.conf ---\n")
	fmt.Fprintf(&b, "anchor \"%s\"\n", e2eAnchor)
	fmt.Fprintf(&b, "anchor \"%s\"\n", sshuttleAnchor)
	return []byte(b.String()), nil
}

// sshuttleAnchorRules повторяет то, что кладёт в свой якорь настоящий sshuttle
// (см. sshuttle/methods/pf.py): трафик к цели заворачивается на lo0 правилом
// без quick, а на lo0 его подхватывает rdr и уводит на локальный слушатель.
//
// Отличие от боевого sshuttle одно: цель сужена до одного адреса и порта,
// чтобы тест не трогал ничего лишнего на живой машине.
func sshuttleAnchorRules(target string, targetPort, listenerPort int) []byte {
	return []byte(fmt.Sprintf(
		"rdr pass on lo0 inet proto tcp to %s port %d -> 127.0.0.1 port %d\n"+
			"pass out route-to lo0 inet proto tcp to %s port %d keep state\n",
		target, targetPort, listenerPort, target, targetPort))
}
