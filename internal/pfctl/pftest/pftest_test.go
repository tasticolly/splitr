package pftest_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/tasticolly/splitr/internal/pfctl/pftest"
)

// Фейк — основа всех остальных тестов, поэтому его поведение проверяется отдельно.

func TestFakeAnchorLifecycle(t *testing.T) {
	t.Parallel()

	f := pftest.New()
	if err := f.LoadAnchor("splitr", []byte("block drop out\n")); err != nil {
		t.Fatalf("LoadAnchor: %v", err)
	}
	rules, err := f.AnchorRules("splitr")
	if err != nil {
		t.Fatalf("AnchorRules: %v", err)
	}
	if rules != "block drop out\n" {
		t.Fatalf("правила якоря = %q", rules)
	}
	if err := f.FlushAnchor("splitr"); err != nil {
		t.Fatalf("FlushAnchor: %v", err)
	}
	if got := f.AnchorText("splitr"); got != "" {
		t.Fatalf("после FlushAnchor якорь должен быть пуст, в нём %q", got)
	}
}

// Чужой `pfctl -F all` смывает правила якорей, но не вызовы якорей из pf.conf.
func TestFakeFlushAllKeepsAnchorReference(t *testing.T) {
	t.Parallel()

	f := pftest.New()
	_ = f.LoadAnchor("splitr", []byte("block drop out\n"))
	f.LinkAnchor("splitr")

	f.FlushAll()

	if got := f.AnchorText("splitr"); got != "" {
		t.Fatalf("правила должны были пропасть, остались %q", got)
	}
	main, err := f.MainRules()
	if err != nil {
		t.Fatalf("MainRules: %v", err)
	}
	if !strings.Contains(main, `anchor "splitr"`) {
		t.Fatalf("вызов якоря должен остаться в главном наборе:\n%s", main)
	}
}

func TestFakeLinkAndUnlinkAnchor(t *testing.T) {
	t.Parallel()

	f := pftest.New()
	f.LinkAnchor("splitr")
	f.LinkAnchor("splitr") // повторный вызов ничего не дублирует
	main, _ := f.MainRules()
	if n := strings.Count(main, `anchor "splitr"`); n != 1 {
		t.Fatalf("вызов якоря встречается %d раз, ожидался один:\n%s", n, main)
	}
	f.UnlinkAnchor("splitr")
	main, _ = f.MainRules()
	if strings.Contains(main, `anchor "splitr"`) {
		t.Fatalf("после UnlinkAnchor вызова быть не должно:\n%s", main)
	}
}

// ReloadMain повторяет поведение настоящего pfctl -f: якоря sshuttle слетают,
// а вызовы якорей из pf.conf возвращаются на место.
func TestFakeReloadMainDropsSshuttleAnchors(t *testing.T) {
	t.Parallel()

	f := pftest.New()
	f.SetSshuttleAnchors("sshuttle-12300")
	if err := f.ReloadMain("/etc/pf.conf"); err != nil {
		t.Fatalf("ReloadMain: %v", err)
	}
	anchors, _ := f.SshuttleAnchors()
	if len(anchors) != 0 {
		t.Fatalf("после перезагрузки pf.conf якорей sshuttle быть не должно: %v", anchors)
	}
	main, _ := f.MainRules()
	if !strings.Contains(main, `anchor "splitr"`) {
		t.Fatalf("pf.conf обязан вернуть вызов якоря splitr:\n%s", main)
	}
	if strings.Join(f.Reloads(), ",") != "/etc/pf.conf" {
		t.Fatalf("перезагрузки = %v", f.Reloads())
	}
}

func TestFakeTokensAreCounted(t *testing.T) {
	t.Parallel()

	f := pftest.New()
	f.SetEnabled(false)

	token, err := f.Enable()
	if err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if on, _ := f.Enabled(); !on {
		t.Fatal("после Enable pf должен считаться включённым")
	}
	if f.LiveTokens() != 1 {
		t.Fatalf("живых токенов %d, ожидался один", f.LiveTokens())
	}
	if err := f.Release(token); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if f.LiveTokens() != 0 {
		t.Fatalf("после Release живых токенов быть не должно: %d", f.LiveTokens())
	}
	if err := f.Release("чужой"); err == nil {
		t.Fatal("освобождение неизвестного токена должно быть ошибкой")
	}
}

func TestFakeFailAndUnfail(t *testing.T) {
	t.Parallel()

	f := pftest.New()
	boom := errors.New("сломано")
	f.Fail(pftest.MethodLoadAnchor, boom)
	if err := f.LoadAnchor("splitr", nil); !errors.Is(err, boom) {
		t.Fatalf("ожидалась подменённая ошибка, получено %v", err)
	}
	if got := f.AnchorText("splitr"); got != "" {
		t.Fatal("при ошибке состояние меняться не должно")
	}
	f.Fail(pftest.MethodLoadAnchor, nil)
	if err := f.LoadAnchor("splitr", nil); err != nil {
		t.Fatalf("после снятия подмены ошибки быть не должно: %v", err)
	}
}

func TestFakeRecordsCalls(t *testing.T) {
	t.Parallel()

	f := pftest.New()
	_, _ = f.Enabled()
	_ = f.KillStates("10.0.0.0/9")
	_ = f.KillStates("11.0.0.0/8")

	if strings.Join(f.Calls(), ",") != "Enabled,KillStates,KillStates" {
		t.Fatalf("журнал вызовов = %v", f.Calls())
	}
	if f.CallCount(pftest.MethodKillStates) != 2 {
		t.Fatalf("KillStates посчитан %d раз", f.CallCount(pftest.MethodKillStates))
	}
	if strings.Join(f.KilledStates(), ",") != "10.0.0.0/9,11.0.0.0/8" {
		t.Fatalf("сброшенные состояния = %v", f.KilledStates())
	}
	if names := f.AnchorNames(); len(names) != 0 {
		t.Fatalf("якорей быть не должно: %v", names)
	}
}

// pfctl показывает только сами правила: комментарии в загруженном наборе
// не сохраняются, и якорь из одних комментариев выглядит пустым.
func TestFakeAnchorRulesDropsComments(t *testing.T) {
	t.Parallel()

	f := pftest.New()
	_ = f.LoadAnchor("splitr", []byte("# коммент\n\nblock drop out on ! lo0\n"))

	rules, err := f.AnchorRules("splitr")
	if err != nil {
		t.Fatalf("AnchorRules: %v", err)
	}
	if rules != "block drop out on ! lo0\n" {
		t.Fatalf("правила = %q, комментарии должны отбрасываться", rules)
	}
	if got := f.AnchorText("splitr"); !strings.Contains(got, "# коммент") {
		t.Fatalf("исходный текст должен сохраняться как есть: %q", got)
	}

	_ = f.LoadAnchor("splitr", []byte("# только комментарий\n"))
	rules, _ = f.AnchorRules("splitr")
	if rules != "" {
		t.Fatalf("якорь из одних комментариев обязан выглядеть пустым, получено %q", rules)
	}
}

// CheckAnchor только запоминает набор правил и ничего не применяет.
func TestFakeCheckAnchorDoesNotApply(t *testing.T) {
	t.Parallel()

	f := pftest.New()
	if err := f.CheckAnchor("splitr", []byte("block drop out\n")); err != nil {
		t.Fatalf("CheckAnchor: %v", err)
	}
	if got := f.AnchorText("splitr"); got != "" {
		t.Fatalf("проверка не должна ничего применять, в якоре %q", got)
	}
	if strings.Join(f.Checked(), ";") != "block drop out\n" {
		t.Fatalf("проверенные наборы = %v", f.Checked())
	}

	f.Fail(pftest.MethodCheckAnchor, errors.New("syntax error"))
	if err := f.CheckAnchor("splitr", nil); err == nil {
		t.Fatal("ожидалась подменённая ошибка разбора")
	}
}
