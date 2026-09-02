package shuttle

import (
	"context"
	"strings"
	"testing"

	"github.com/tasticolly/splitr/internal/config"
)

func TestDetectAuthURL(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		line string
		want string
	}{
		{
			"голая ссылка",
			"https://login.tailscale.com/a/abc123def456",
			"https://login.tailscale.com/a/abc123def456",
		},
		{
			"ссылка в предложении",
			"To authenticate, visit: https://login.tailscale.com/a/abc123 and sign in",
			"https://login.tailscale.com/a/abc123",
		},
		{
			"ссылка с отступом от sshuttle",
			"client:   https://login.tailscale.com/a/deadbeef",
			"https://login.tailscale.com/a/deadbeef",
		},
		{
			"точка в конце предложения не часть ссылки",
			"Open https://login.tailscale.com/a/abc123.",
			"https://login.tailscale.com/a/abc123",
		},
		{
			"ссылка в скобках",
			"(https://login.tailscale.com/a/abc123)",
			"https://login.tailscale.com/a/abc123",
		},
		// Ложные срабатывания опаснее пропуска: продукт будет требовать
		// действия там, где всё в порядке, и человек перестанет верить меню.
		{"обычная строка sshuttle", "client: Connected.", ""},
		{"строка про firewall", "firewall manager: starting transproxy.", ""},
		{"чужой домен", "see https://tailscale.com/kb/1193/tailscale-ssh", ""},
		{"похожая ссылка на другой хост", "https://login.example.com/a/abc123", ""},
		{"голый префикс без пути", "visit https://login.tailscale.com/", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := DetectAuthURL(tc.line); got != tc.want {
				t.Fatalf("DetectAuthURL(%q) = %q, ожидалось %q", tc.line, got, tc.want)
			}
		})
	}
}

func TestRunnerNotesAuthRequirement(t *testing.T) {
	t.Parallel()

	log := &syncBuffer{}
	r := NewRunner(config.Sshuttle{Path: "/bin/true"}, log)

	out := &outputWatcher{dst: log, note: r.noteOutput}
	_, _ = out.Write([]byte("client: Connected.\nTo authenticate, visit:\n"))
	if snap := r.Snapshot(); !snap.Action.Empty() {
		t.Fatalf("обычный вывод дал требование %+v", snap.Action)
	}

	// Ссылка приходит последней строкой и без перевода строки: ssh печатает её
	// перед самым обрывом соединения.
	_, _ = out.Write([]byte("  https://login.tailscale.com/a/abc123"))
	if snap := r.Snapshot(); !snap.Action.Empty() {
		t.Fatalf("недописанная строка не должна разбираться до flush: %+v", snap.Action)
	}
	out.flush()

	snap := r.Snapshot()
	if snap.Action.Kind != "auth" || snap.Action.URL != "https://login.tailscale.com/a/abc123" {
		t.Fatalf("action = %+v, ожидалось требование входа со ссылкой", snap.Action)
	}
	if snap.Action.Message == "" {
		t.Fatalf("сообщение пустое: человеку нечего показать")
	}
	if !strings.Contains(log.String(), "requires re-authentication") {
		t.Fatalf("в журнале нет отдельной строки о входе: %q", log.String())
	}
	// Вывод процесса обязан попасть в журнал как был — иначе `splitr log`
	// перестанет показывать то, что печатает sshuttle.
	if !strings.Contains(log.String(), "client: Connected.") {
		t.Fatalf("вывод sshuttle не попал в журнал: %q", log.String())
	}

	// Подъём туннеля снимает требование: раз туннель работает, входить не нужно.
	r.MarkUp()
	if snap := r.Snapshot(); !snap.Action.Empty() {
		t.Fatalf("после MarkUp осталось %+v", snap.Action)
	}
}

func TestRunnerClearsActionOnStop(t *testing.T) {
	t.Parallel()

	r := NewRunner(config.Sshuttle{Path: "/bin/true"}, &syncBuffer{})
	r.noteOutput("https://login.tailscale.com/a/abc123")
	if r.Snapshot().Action.Empty() {
		t.Fatal("требование не запомнилось")
	}

	// Процесса нет, и Stop выходит рано — требование обязано сниматься и в этом
	// случае: sshuttle с требованием входа как раз и умирает сразу.
	if err := r.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !r.Snapshot().Action.Empty() {
		t.Fatalf("после Stop осталось %+v", r.Snapshot().Action)
	}
}
