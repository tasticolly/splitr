//go:build docker_e2e

package e2e

import (
	"net/http"
	"testing"
	"time"

	"github.com/tasticolly/splitr/internal/daemon"
)

// assertRepeatedUp проверяет, что повторный /up при живом туннеле безвреден.
//
// Раньше здесь был баг: Daemon.Up первым делом звал runner.KillForeign(),
// а тот считал «чужим» любой процесс со словом sshuttle в командной строке,
// кроме собственного прямого потомка. sshuttle же всегда поднимает второй
// процесс — `sshuttle --method auto --firewall`, — и повторный up убивал
// работающий туннель. Починено двумя правками: проверка «уже поднят» ушла
// вперёд гашения посторонних, а KillForeign сравнивает группу процессов.
func assertRepeatedUp(t *testing.T, code int, body []byte, before daemon.Status) {
	t.Helper()

	if code != http.StatusOK {
		t.Fatalf("повторный /up обязан быть безвредным и вернуть 200; получено %d: %s", code, body)
	}

	// Туннель обязан остаться тем же самым процессом: перезапуск порвал бы
	// живые соединения на ровном месте.
	after := status(t)
	if after.Tunnel != "up" || len(after.SshuttleAnchs) == 0 {
		t.Fatalf("после повторного up туннель обязан остаться поднятым, статус: %s", mustJSON(after))
	}
	if after.PID != before.PID {
		t.Fatalf("процесс sshuttle сменился с %d на %d — туннель перезапустили без нужды", before.PID, after.PID)
	}
	if !processAlive(before.PID) {
		t.Fatalf("процесс sshuttle (pid %d) не пережил повторный up", before.PID)
	}
	// И трафик через туннель обязан продолжать ходить.
	waitTarget(t, true, 15*time.Second)
}

// waitBlocking ждёт, пока демон снова начнёт резать трафик.
func waitBlocking(t *testing.T, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if status(t).Blocking {
			return true
		}
		time.Sleep(200 * time.Millisecond)
	}
	return false
}
