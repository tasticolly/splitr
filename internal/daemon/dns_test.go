package daemon

import (
	"path/filepath"
	"reflect"
	"testing"
)

// newDaemonWithUpdateScript — демон, которому поручено переключать резолвер,
// с файлом состояния: без него снимок некуда сохранить.
func newDaemonWithUpdateScript(t *testing.T) (*Daemon, *fakeOps) {
	t.Helper()
	cfg := testConfig()
	cfg.DNS.UpdateScript = "/usr/local/bin/update_dns.sh"
	cfg.Daemon.StateFile = filepath.Join(t.TempDir(), "splitr.state.json")
	d, _, _, ops := newTestDaemon(t, cfg)
	return d, ops
}

func TestParseDNSServers(t *testing.T) {
	tests := []struct {
		name string
		out  string
		want []string
	}{
		{
			name: "два резолвера",
			out:  "1.1.1.1\n8.8.8.8\n",
			want: []string{"1.1.1.1", "8.8.8.8"},
		},
		{
			// networksetup отвечает предложением, а не пустым списком.
			// Приняв его за адрес, мы потом вернули бы его как резолвер.
			name: "резолверов не задано",
			out:  "There aren't any DNS Servers set on Wi-Fi.\n",
			want: nil,
		},
		{
			name: "пустой вывод",
			out:  "\n\n",
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseDNSServers(tt.out); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("parseDNSServers(%q) = %v, ожидалось %v", tt.out, got, tt.want)
			}
		})
	}
}

const serviceOrder = `An asterisk (*) denotes that a network service is disabled.
(1) Wi-Fi
(Hardware Port: Wi-Fi, Device: en0)

(2) Thunderbolt Bridge
(Hardware Port: Thunderbolt Bridge, Device: bridge0)

(*3) iPhone USB
(Hardware Port: iPhone USB, Device: en5)

`

func TestServiceForDevice(t *testing.T) {
	tests := []struct {
		device, want string
	}{
		{"en0", "Wi-Fi"},
		{"bridge0", "Thunderbolt Bridge"},
		// Звёздочка помечает выключенную службу и в имя не входит.
		{"en5", "iPhone USB"},
		{"utun4", ""},
	}
	for _, tt := range tests {
		if got := serviceForDevice(serviceOrder, tt.device); got != tt.want {
			t.Errorf("serviceForDevice(%q) = %q, ожидалось %q", tt.device, got, tt.want)
		}
	}
}

// Регрессия на сам баг: сценарий, в котором туннель не поднялся, а системный
// резолвер остался смотреть в 127.0.0.1, где никто не слушает.
func TestDNSIsRestoredWhenTheTunnelIsLost(t *testing.T) {
	d, ops := newDaemonWithUpdateScript(t)
	ops.dnsSnapshot = []string{"192.168.1.254"}

	d.backupDNS()
	if !d.dnsRedirected {
		t.Fatal("после подмены перенаправление должно быть отмечено")
	}

	d.onTunnelLost()

	got := ops.restores()
	if len(got) != 1 || !reflect.DeepEqual(got[0], []string{"192.168.1.254"}) {
		t.Fatalf("резолверы возвращены как %v, ожидалось [[192.168.1.254]]", got)
	}
	if d.dnsRedirected {
		t.Error("перенаправление снято, отметка должна быть сброшена")
	}
}

// Сторож зовёт onTunnelLost на каждом тике опущенного туннеля. Возврат должен
// произойти один раз, иначе демон переписывал бы системные настройки постоянно.
func TestDNSIsRestoredOnlyOnce(t *testing.T) {
	d, ops := newDaemonWithUpdateScript(t)
	ops.dnsSnapshot = []string{"192.168.1.254"}

	d.backupDNS()
	d.onTunnelLost()
	d.onTunnelLost()
	d.onTunnelLost()

	if got := len(ops.restores()); got != 1 {
		t.Fatalf("RestoreDNS вызван %d раз, ожидался 1", got)
	}
}

// Без update_script демон системный резолвер не трогает, значит и возвращать
// ему нечего: чужие настройки не его дело.
func TestDNSIsLeftAloneWithoutAnUpdateScript(t *testing.T) {
	d, ops := newDaemonWithUpdateScript(t)
	d.cfg.DNS.UpdateScript = ""

	d.onTunnelLost()

	if got := len(ops.restores()); got != 0 {
		t.Fatalf("RestoreDNS вызван %d раз, ожидалось 0", got)
	}
}

// Пустой снимок — это «резолверов не было задано», и вернуть надо именно его,
// а не счесть отсутствие адресов поводом ничего не делать.
func TestEmptySnapshotIsStillRestored(t *testing.T) {
	d, ops := newDaemonWithUpdateScript(t)
	ops.dnsSnapshot = nil

	d.backupDNS()
	d.onTunnelLost()

	got := ops.restores()
	if len(got) != 1 || len(got[0]) != 0 {
		t.Fatalf("возвращено %v, ожидался один вызов с пустым списком", got)
	}
}

// Снимок переживает перезапуск демона: иначе перенаправление, поставленное
// прошлым запуском, некому было бы снять.
func TestDNSBackupSurvivesARestart(t *testing.T) {
	d, ops := newDaemonWithUpdateScript(t)
	ops.dnsSnapshot = []string{"192.168.1.254"}
	d.backupDNS()

	fresh, freshOps := newDaemonWithUpdateScript(t)
	fresh.cfg.Daemon.StateFile = d.cfg.Daemon.StateFile
	fresh.restoreState()

	if !fresh.dnsRedirected {
		t.Fatal("отметка перенаправления не пережила перезапуск")
	}
	fresh.onTunnelLost()
	got := freshOps.restores()
	if len(got) != 1 || !reflect.DeepEqual(got[0], []string{"192.168.1.254"}) {
		t.Fatalf("резолверы возвращены как %v, ожидалось [[192.168.1.254]]", got)
	}
}

// Тот самый сценарий, из-за которого баг и остался незамеченным: туннель не
// поднялся ни разу, поэтому перехода «был поднят → упал» не было, и старый код
// оставлял резолвер смотреть в 127.0.0.1 навсегда.
func TestDNSIsRestoredWhenTheTunnelNeverCameUp(t *testing.T) {
	cfg := testConfig()
	cfg.DNS.UpdateScript = "/usr/local/bin/update_dns.sh"
	cfg.Daemon.StateFile = filepath.Join(t.TempDir(), "splitr.state.json")
	d, _, tun, ops := newTestDaemon(t, cfg)
	ops.dnsSnapshot = []string{"192.168.1.254"}

	d.backupDNS()

	// sshuttle запустился и умер, не дойдя до рабочего состояния.
	tun.running = false

	// wasUp = false: туннель никогда не был поднят, перехода нет.
	if up := d.tick(false); up {
		t.Fatal("туннель не поднят, tick не должен говорить обратное")
	}

	got := ops.restores()
	if len(got) != 1 || !reflect.DeepEqual(got[0], []string{"192.168.1.254"}) {
		t.Fatalf("резолверы возвращены как %v, ожидалось [[192.168.1.254]]", got)
	}
}

// Пока sshuttle поднимается, резолвер трогать нельзя: он уже указывает на
// туннель, и возврат оборвал бы DNS ровно в момент подключения.
func TestDNSIsLeftAloneWhileSshuttleIsStarting(t *testing.T) {
	cfg := testConfig()
	cfg.DNS.UpdateScript = "/usr/local/bin/update_dns.sh"
	cfg.Daemon.StateFile = filepath.Join(t.TempDir(), "splitr.state.json")
	d, _, tun, ops := newTestDaemon(t, cfg)
	ops.dnsSnapshot = []string{"192.168.1.254"}

	d.backupDNS()
	tun.running = true

	d.tick(false)

	if got := len(ops.restores()); got != 0 {
		t.Fatalf("RestoreDNS вызван %d раз при живом процессе, ожидалось 0", got)
	}
}
