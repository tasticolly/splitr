package daemon

import (
	"context"
	"os/exec"

	"github.com/tasticolly/splitr/internal/config"
	"github.com/tasticolly/splitr/internal/shuttle"
)

// Tunnel — управление процессом sshuttle.
// Интерфейс нужен тестам: боевая реализация — *shuttle.Runner.
type Tunnel interface {
	Start(cfg config.Config, name string, p config.Profile) error
	Stop(ctx context.Context) error
	Snapshot() shuttle.Snapshot
	MarkUp()
	SetAction(a shuttle.ActionRequired)
	ClearAction()
	Running() bool
	KillForeign() error
}

// SystemOps — побочные системные действия при подъёме туннеля:
// команды на удалённом хосте и переключение системного резолвера.
type SystemOps interface {
	// RunRemote возвращает вывод: в нём приходит ссылка на вход, когда
	// удалённый хост требует заново пройти аутентификацию.
	RunRemote(sshArgs []string, remote, command string) ([]byte, error)
	// Reachable проверяет, отвечает ли хост по ssh, и возвращает вывод ssh.
	Reachable(sshArgs []string, remote string) ([]byte, error)
	UpdateDNS(script string) ([]byte, error)
	FlushDNSCache() error
}

// execOps — боевая реализация SystemOps поверх внешних команд.
type execOps struct{}

func (execOps) RunRemote(sshArgs []string, remote, command string) ([]byte, error) {
	argv := append(sshArgs[1:], remote, command)
	return exec.Command(sshBinary(sshArgs), argv...).CombinedOutput()
}

// sshBinary достаёт исполняемый файл из собранного вызова ssh.
func sshBinary(sshArgs []string) string {
	if len(sshArgs) == 0 || sshArgs[0] == "ssh" {
		return "/usr/bin/ssh"
	}
	return sshArgs[0]
}

// Reachable — дешёвая проверка перед подъёмом туннеля.
//
// BatchMode запрещает интерактивные запросы, поэтому хост, требующий входа
// через браузер, не подвиснет в ожидании, а сразу напечатает ссылку и выйдет
// с ошибкой. Раньше об этом узнавали только по молчаливой смерти sshuttle
// через несколько секунд, а ссылку выбрасывали вместе с выводом.
func (execOps) Reachable(sshArgs []string, remote string) ([]byte, error) {
	argv := append(sshArgs[1:],
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=10",
		"-o", "StrictHostKeyChecking=accept-new",
		remote, "true")
	return exec.Command(sshBinary(sshArgs), argv...).CombinedOutput()
}

func (execOps) UpdateDNS(script string) ([]byte, error) {
	return exec.Command(script).CombinedOutput()
}

func (execOps) FlushDNSCache() error {
	if err := exec.Command("/usr/bin/dscacheutil", "-flushcache").Run(); err != nil {
		return err
	}
	return exec.Command("/usr/bin/killall", "-HUP", "mDNSResponder").Run()
}
