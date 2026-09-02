package pfctl_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tasticolly/splitr/internal/pfctl"
	"github.com/tasticolly/splitr/internal/pfctl/pftest"
)

// fakePfctl создаёт подставной «pfctl» — обычный shell-скрипт,
// который печатает заранее заданный ответ. Настоящий pfctl требует root,
// поэтому в тестах он не запускается никогда.
func fakePfctl(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pfctl")
	body := "#!/bin/sh\n" + script + "\n"
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatalf("создать подставной pfctl: %v", err)
	}
	return path
}

func TestParseSshuttleAnchors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		out  string
		want []string
	}{
		{
			name: "туннеля нет",
			out:  "com.apple\ncom.apple.internet-sharing\n",
		},
		{
			name: "один якорь sshuttle",
			out:  "com.apple\n  sshuttle-12300\nsplitr\n",
			want: []string{"sshuttle-12300"},
		},
		{
			name: "несколько туннелей",
			out:  "sshuttle-12300\nsshuttle-12400\n",
			want: []string{"sshuttle-12300", "sshuttle-12400"},
		},
		{
			name: "пустой вывод",
			out:  "",
		},
		{
			name: "похожее имя без дефиса не считается",
			out:  "sshuttlefoo\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := pfctl.ParseSshuttleAnchors(tt.out)
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Fatalf("ParseSshuttleAnchors = %v, ожидалось %v", got, tt.want)
			}
		})
	}
}

func TestAnchorReferenced(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		rules string
		want  bool
	}{
		{name: "якорь вызывается", rules: "scrub-anchor \"com.apple/*\" all\nanchor \"splitr\" all\n", want: true},
		{name: "якоря нет", rules: "anchor \"com.apple/*\" all\n", want: false},
		{name: "похожее имя не в счёт", rules: "anchor \"splitr-old\" all\n", want: false},
		{name: "пустой набор правил", rules: "", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			pf := pftest.New()
			pf.SetMainRules(tt.rules)
			got, err := pfctl.AnchorReferenced(pf, "splitr")
			if err != nil {
				t.Fatalf("AnchorReferenced: %v", err)
			}
			if got != tt.want {
				t.Fatalf("AnchorReferenced = %v, ожидалось %v", got, tt.want)
			}
		})
	}
}

func TestAnchorReferencedPropagatesError(t *testing.T) {
	t.Parallel()

	pf := pftest.New()
	boom := errors.New("pfctl упал")
	pf.Fail(pftest.MethodMainRules, boom)
	if _, err := pfctl.AnchorReferenced(pf, "splitr"); !errors.Is(err, boom) {
		t.Fatalf("получено %v, ожидалась ошибка чтения правил", err)
	}
}

// Токен pf выковыривается из stderr — именно туда его пишет pfctl -E.
func TestCLIEnableParsesToken(t *testing.T) {
	t.Parallel()

	bin := fakePfctl(t, `echo "pf enabled" >&2; echo "Token : 17563455677763561234" >&2`)
	token, err := pfctl.NewWithBinary(bin).Enable()
	if err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if token != "17563455677763561234" {
		t.Fatalf("токен = %q", token)
	}
}

func TestCLIEnableWithoutTokenIsError(t *testing.T) {
	t.Parallel()

	bin := fakePfctl(t, `echo "pf already enabled" >&2`)
	if _, err := pfctl.NewWithBinary(bin).Enable(); err == nil {
		t.Fatal("без токена в выводе Enable обязан вернуть ошибку")
	}
}

func TestCLIEnableReportsCommandFailure(t *testing.T) {
	t.Parallel()

	bin := fakePfctl(t, `echo "Operation not permitted" >&2; exit 1`)
	_, err := pfctl.NewWithBinary(bin).Enable()
	if err == nil {
		t.Fatal("ненулевой код возврата pfctl обязан стать ошибкой")
	}
	if !strings.Contains(err.Error(), "Operation not permitted") {
		t.Fatalf("ошибка = %v, ожидался текст stderr от pfctl", err)
	}
}

func TestCLIEnabled(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		script string
		want   bool
	}{
		{name: "pf включён", script: `echo "Status: Enabled for 1 days"`, want: true},
		{name: "pf выключен", script: `echo "Status: Disabled"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := pfctl.NewWithBinary(fakePfctl(t, tt.script)).Enabled()
			if err != nil {
				t.Fatalf("Enabled: %v", err)
			}
			if got != tt.want {
				t.Fatalf("Enabled = %v, ожидалось %v", got, tt.want)
			}
		})
	}
}

func TestCLIEnabledPropagatesError(t *testing.T) {
	t.Parallel()

	got, err := pfctl.NewWithBinary(fakePfctl(t, `exit 2`)).Enabled()
	if err == nil {
		t.Fatal("ожидалась ошибка")
	}
	if got {
		t.Fatal("при ошибке pf не должен считаться включённым")
	}
}

// LoadAnchor обязан подавать правила именно на stdin: файла с ними нет.
func TestCLILoadAnchorPassesRulesOnStdin(t *testing.T) {
	t.Parallel()

	dump := filepath.Join(t.TempDir(), "dump")
	bin := fakePfctl(t, `echo "$@" > `+dump+`.args; cat > `+dump)
	if err := pfctl.NewWithBinary(bin).LoadAnchor("splitr", []byte("block drop out\n")); err != nil {
		t.Fatalf("LoadAnchor: %v", err)
	}
	body, err := os.ReadFile(dump)
	if err != nil {
		t.Fatalf("прочитать stdin подставного pfctl: %v", err)
	}
	if string(body) != "block drop out\n" {
		t.Fatalf("на stdin пришло %q", body)
	}
	args, err := os.ReadFile(dump + ".args")
	if err != nil {
		t.Fatalf("прочитать аргументы: %v", err)
	}
	if strings.TrimSpace(string(args)) != "-a splitr -f -" {
		t.Fatalf("аргументы = %q, ожидалось `-a splitr -f -`", args)
	}
}

// Проверяем, что каждый метод зовёт pfctl с нужными аргументами:
// перепутанный флаг здесь означает молча неработающую блокировку.
func TestCLIArguments(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		call func(*pfctl.CLI) error
		want string
	}{
		{
			name: "Release",
			call: func(c *pfctl.CLI) error { return c.Release("42") },
			want: "-X 42",
		},
		{
			name: "FlushAnchor",
			call: func(c *pfctl.CLI) error { return c.FlushAnchor("splitr") },
			want: "-a splitr -F all",
		},
		{
			name: "AnchorRules",
			call: func(c *pfctl.CLI) error { _, err := c.AnchorRules("splitr"); return err },
			want: "-a splitr -s rules",
		},
		{
			name: "MainRules",
			call: func(c *pfctl.CLI) error { _, err := c.MainRules(); return err },
			want: "-s rules",
		},
		{
			name: "SshuttleAnchors",
			call: func(c *pfctl.CLI) error { _, err := c.SshuttleAnchors(); return err },
			want: "-s Anchors",
		},
		{
			// Первый -k задаёт источник, второй — назначение.
			name: "KillStates",
			call: func(c *pfctl.CLI) error { return c.KillStates("10.0.0.0/9") },
			want: "-k 0.0.0.0/0 -k 10.0.0.0/9",
		},
		{
			name: "ReloadMain",
			call: func(c *pfctl.CLI) error { return c.ReloadMain("/etc/pf.conf") },
			want: "-f /etc/pf.conf",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dump := filepath.Join(t.TempDir(), "args")
			bin := fakePfctl(t, `echo "$@" > `+dump)
			if err := tt.call(pfctl.NewWithBinary(bin)); err != nil {
				t.Fatalf("%s: %v", tt.name, err)
			}
			got, err := os.ReadFile(dump)
			if err != nil {
				t.Fatalf("прочитать аргументы: %v", err)
			}
			if strings.TrimSpace(string(got)) != tt.want {
				t.Fatalf("аргументы = %q, ожидалось %q", strings.TrimSpace(string(got)), tt.want)
			}
		})
	}
}

func TestCLISshuttleAnchorsFiltersOutput(t *testing.T) {
	t.Parallel()

	bin := fakePfctl(t, `printf 'com.apple\nsshuttle-12300\n'`)
	got, err := pfctl.NewWithBinary(bin).SshuttleAnchors()
	if err != nil {
		t.Fatalf("SshuttleAnchors: %v", err)
	}
	if strings.Join(got, ",") != "sshuttle-12300" {
		t.Fatalf("SshuttleAnchors = %v", got)
	}
}

func TestCLIRunReturnsBothStreams(t *testing.T) {
	t.Parallel()

	bin := fakePfctl(t, `echo out; echo err >&2`)
	stdout, stderr, err := pfctl.NewWithBinary(bin).Run(nil, "-s", "info")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.TrimSpace(stdout) != "out" || strings.TrimSpace(stderr) != "err" {
		t.Fatalf("stdout = %q, stderr = %q", stdout, stderr)
	}
}

func TestCLIMissingBinaryIsError(t *testing.T) {
	t.Parallel()

	bin := filepath.Join(t.TempDir(), "нет-такого-pfctl")
	if _, _, err := pfctl.NewWithBinary(bin).Run(nil, "-s", "info"); err == nil {
		t.Fatal("отсутствующий бинарь должен приводить к ошибке")
	}
}

func TestNewUsesDefaultBinary(t *testing.T) {
	t.Parallel()

	if pfctl.DefaultBinary != "/sbin/pfctl" {
		t.Fatalf("DefaultBinary = %q, ожидался штатный путь macOS", pfctl.DefaultBinary)
	}
	var _ pfctl.Controller = pfctl.New()
}
