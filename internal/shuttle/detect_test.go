package shuttle

import "testing"

// Распознавание процесса sshuttle — не косметика: если демон решит, что
// живого процесса нет, он посчитает якоря pf осиротевшими и снесёт их.
// Для туннеля, поднятого мимо splitr, это означает молча разорванное
// соединение, поэтому набор реальных командных строк зафиксирован тестом.
func TestIsSshuttleCommand(t *testing.T) {
	t.Parallel()

	const binary = "/opt/homebrew/bin/sshuttle"

	tests := []struct {
		name    string
		command string
		want    bool
	}{
		{"homebrew, запуск напрямую",
			"/opt/homebrew/bin/sshuttle -r user@host --dns 10.0.0.0/9", true},
		{"через интерпретатор",
			"/opt/homebrew/opt/python@3.14/bin/python3.14 /opt/homebrew/bin/sshuttle -r user@host", true},
		{"вспомогательный процесс фаервола",
			"/opt/homebrew/bin/sshuttle --method auto --firewall 12300 12300", true},
		{"linux, из /usr/bin",
			"/usr/bin/python3 /usr/bin/sshuttle -r user@host 10.0.0.0/8", true},
		{"без пути",
			"sshuttle -r user@host", true},

		{"редактор с открытым конфигом не считается",
			"/usr/bin/vim /Users/you/start_sshuttle_v2.sh", false},
		{"скрипт-обёртка не считается",
			"/bin/zsh /Users/you/start_sshuttle_v2.sh visa", false},
		{"grep по слову не считается",
			"grep -r sshuttle /etc", false},
		{"сам splitr не считается",
			"/usr/local/bin/splitr daemon --config /usr/local/etc/splitr/config.yaml", false},
		{"ssh-потомок туннеля сам по себе не считается",
			"ssh -o ServerAliveInterval=15 user@host", false},
		{"пустая строка",
			"", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := isSshuttleCommand(tt.command, binary); got != tt.want {
				t.Fatalf("isSshuttleCommand(%q) = %v, ожидалось %v", tt.command, got, tt.want)
			}
		})
	}
}

// Строка ps разбирается на пид и команду; мусор отбрасывается, а не роняет разбор.
func TestParsePsLine(t *testing.T) {
	t.Parallel()

	tests := []struct {
		line    string
		wantPID int
		wantCmd string
		wantOK  bool
	}{
		{"  501 /opt/homebrew/bin/sshuttle -r user@host", 501, "/opt/homebrew/bin/sshuttle -r user@host", true},
		{"66214 /usr/local/bin/splitr daemon", 66214, "/usr/local/bin/splitr daemon", true},
		{"заголовок таблицы", 0, "", false},
		{"", 0, "", false},
		{"12345", 0, "", false},
	}

	for _, tt := range tests {
		pid, cmd, ok := parsePsLine(tt.line)
		if ok != tt.wantOK || pid != tt.wantPID || cmd != tt.wantCmd {
			t.Fatalf("parsePsLine(%q) = (%d, %q, %v), ожидалось (%d, %q, %v)",
				tt.line, pid, cmd, ok, tt.wantPID, tt.wantCmd, tt.wantOK)
		}
	}
}
