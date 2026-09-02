package daemon

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

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
	// SnapshotDNS records the resolvers before update_script overwrites them;
	// RestoreDNS puts them back. An empty slice means "no resolvers were set",
	// which is a state worth restoring exactly as it was.
	SnapshotDNS() ([]string, error)
	RestoreDNS(servers []string) error
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

// SnapshotDNS and RestoreDNS go through networksetup because on macOS the
// system resolver lives in the network service settings, not in
// /etc/resolv.conf, which the OS rewrites on the next network change anyway.
func (execOps) SnapshotDNS() ([]string, error) {
	service, err := primaryNetworkService()
	if err != nil {
		return nil, err
	}
	out, err := exec.Command(networksetupBinary, "-getdnsservers", service).Output()
	if err != nil {
		return nil, fmt.Errorf("networksetup -getdnsservers %s: %w", service, err)
	}
	return parseDNSServers(string(out)), nil
}

func (execOps) RestoreDNS(servers []string) error {
	service, err := primaryNetworkService()
	if err != nil {
		return err
	}
	// networksetup has no "clear the list" flag; the word Empty is how it is done.
	argv := []string{"-setdnsservers", service}
	if len(servers) == 0 {
		argv = append(argv, "Empty")
	} else {
		argv = append(argv, servers...)
	}
	if out, err := exec.Command(networksetupBinary, argv...).CombinedOutput(); err != nil {
		return fmt.Errorf("networksetup -setdnsservers %s: %w: %s", service, err, strings.TrimSpace(string(out)))
	}
	return nil
}

const networksetupBinary = "/usr/sbin/networksetup"

// primaryNetworkService maps the device of the default route onto the service
// name networksetup speaks: it knows nothing about "en0", only about "Wi-Fi".
func primaryNetworkService() (string, error) {
	device, err := defaultRouteDevice()
	if err != nil {
		return "", err
	}
	out, err := exec.Command(networksetupBinary, "-listnetworkserviceorder").Output()
	if err != nil {
		return "", fmt.Errorf("networksetup -listnetworkserviceorder: %w", err)
	}
	service := serviceForDevice(string(out), device)
	if service == "" {
		return "", fmt.Errorf("no network service is bound to %s", device)
	}
	return service, nil
}

func defaultRouteDevice() (string, error) {
	out, err := exec.Command("/sbin/route", "-n", "get", "default").Output()
	if err != nil {
		return "", fmt.Errorf("route -n get default: %w", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if _, rest, ok := strings.Cut(strings.TrimSpace(line), "interface:"); ok {
			return strings.TrimSpace(rest), nil
		}
	}
	return "", errors.New("route -n get default printed no interface")
}

func (execOps) FlushDNSCache() error {
	if err := exec.Command("/usr/bin/dscacheutil", "-flushcache").Run(); err != nil {
		return err
	}
	return exec.Command("/usr/bin/killall", "-HUP", "mDNSResponder").Run()
}
