// Package pftest содержит фейковую реализацию pfctl.Controller,
// которая живёт целиком в памяти и не трогает настоящий пакетный фильтр.
//
// Фейк воспроизводит ровно те свойства pf, на которые опирается splitr:
//
//   - якоря хранят текст правил и очищаются целиком;
//   - главный набор правил отдельно от якорей, и якорь «работает» только
//     если главный набор его вызывает (см. LinkAnchor);
//   - включение pf считается по ссылкам-токенам, как у pfctl -E/-X;
//   - список якорей sshuttle задаётся тестом — так эмулируется живой туннель.
//
// Любой метод можно заставить вернуть ошибку через Fail, а чужой
// `pfctl -F all` — через FlushAll.
package pftest

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/tasticolly/splitr/internal/pfctl"
)

// Fake обязан оставаться пригодным везде, где ждут боевой pfctl.
var _ pfctl.Controller = (*Fake)(nil)

// Имена методов Controller — ключи для Fail и записи в журнал вызовов.
const (
	MethodEnable          = "Enable"
	MethodRelease         = "Release"
	MethodEnabled         = "Enabled"
	MethodLoadAnchor      = "LoadAnchor"
	MethodCheckAnchor     = "CheckAnchor"
	MethodFlushAnchor     = "FlushAnchor"
	MethodAnchorRules     = "AnchorRules"
	MethodMainRules       = "MainRules"
	MethodSshuttleAnchors = "SshuttleAnchors"
	MethodKillStates      = "KillStates"
	MethodReloadMain      = "ReloadMain"
)

// Fake — потокобезопасная реализация pfctl.Controller поверх карт в памяти.
// Нулевое значение непригодно, пользуйтесь New.
type Fake struct {
	mu sync.Mutex

	enabled  bool
	tokenSeq int
	tokens   map[string]bool

	anchors   map[string]string
	mainRules string
	sshuttle  []string

	reloadLinks []string
	checked     []string
	killed      []string
	reloads     []string
	calls       []string
	errs        map[string]error
}

// New создаёт фейк с включённым pf и пустыми якорями.
func New() *Fake {
	return &Fake{
		enabled: true,
		tokens:  map[string]bool{},
		anchors: map[string]string{},
		errs:    map[string]error{},
		// pf.conf после установки splitr вызывает одноимённый якорь.
		reloadLinks: []string{"splitr"},
	}
}

// --- настройка поведения ---------------------------------------------------

// Fail заставляет метод method возвращать ошибку err. Nil снимает подмену.
func (f *Fake) Fail(method string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err == nil {
		delete(f.errs, method)
		return
	}
	f.errs[method] = err
}

// SetEnabled задаёт, считается ли pf включённым.
func (f *Fake) SetEnabled(on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enabled = on
}

// SetSshuttleAnchors подменяет список якорей sshuttle:
// непустой список означает «туннель поднят».
// SetSshuttleAnchors изображает живой туннель: якоря есть и в них есть
// правила, как их кладёт настоящий sshuttle.
func (f *Fake) SetSshuttleAnchors(names ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sshuttle = append([]string(nil), names...)
	for _, name := range names {
		f.anchors[name] = "pass out route-to lo0 inet proto tcp to 10.0.0.0/9 keep state\n"
	}
}

// SetGhostSshuttleAnchors изображает мусор от умершего туннеля: якоря
// перечислены, но правил в них нет.
func (f *Fake) SetGhostSshuttleAnchors(names ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sshuttle = append([]string(nil), names...)
	for _, name := range names {
		delete(f.anchors, name)
	}
}

// SetMainRules полностью задаёт текст главного набора правил.
func (f *Fake) SetMainRules(rules string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mainRules = rules
}

// LinkAnchor дописывает в главный набор вызов якоря —
// то же, что строка anchor "имя" в /etc/pf.conf.
func (f *Fake) LinkAnchor(anchor string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.linkLocked(anchor)
}

func (f *Fake) linkLocked(anchor string) {
	line := fmt.Sprintf("anchor %q all", anchor)
	if strings.Contains(f.mainRules, line) {
		return
	}
	if f.mainRules != "" && !strings.HasSuffix(f.mainRules, "\n") {
		f.mainRules += "\n"
	}
	f.mainRules += line + "\n"
}

// UnlinkAnchor убирает вызов якоря из главного набора правил.
func (f *Fake) UnlinkAnchor(anchor string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var kept []string
	for _, line := range strings.Split(f.mainRules, "\n") {
		if strings.Contains(line, fmt.Sprintf("anchor %q", anchor)) {
			continue
		}
		kept = append(kept, line)
	}
	f.mainRules = strings.Join(kept, "\n")
}

// SetReloadLinks задаёт, вызовы каких якорей появятся в главном наборе
// правил после ReloadMain — то есть что написано в самом pf.conf.
func (f *Fake) SetReloadLinks(anchors ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reloadLinks = append([]string(nil), anchors...)
}

// FlushAll эмулирует чужой `pfctl -F all`: правила якорей исчезают,
// главный набор и вызовы якорей остаются на месте.
func (f *Fake) FlushAll() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.anchors = map[string]string{}
}

// --- наблюдение ------------------------------------------------------------

// AnchorText возвращает текст, загруженный в якорь (пустая строка — якорь пуст).
func (f *Fake) AnchorText(anchor string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.anchors[anchor]
}

// AnchorNames возвращает отсортированные имена непустых якорей.
func (f *Fake) AnchorNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.anchors))
	for name := range f.anchors {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// KilledStates возвращает сети, для которых звали KillStates, в порядке вызовов.
func (f *Fake) KilledStates() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.killed...)
}

// Reloads возвращает пути, с которыми звали ReloadMain.
func (f *Fake) Reloads() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.reloads...)
}

// Calls возвращает журнал имён вызванных методов.
func (f *Fake) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// CallCount считает, сколько раз звали метод method.
func (f *Fake) CallCount(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == method {
			n++
		}
	}
	return n
}

// LiveTokens возвращает число выданных и ещё не освобождённых токенов pf.
func (f *Fake) LiveTokens() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, live := range f.tokens {
		if live {
			n++
		}
	}
	return n
}

// --- реализация pfctl.Controller -------------------------------------------

// record помечает вызов в журнале и отдаёт подменную ошибку, если она задана.
// Вызывается под уже взятым замком.
func (f *Fake) record(method string) error {
	f.calls = append(f.calls, method)
	return f.errs[method]
}

// Enable включает pf и выдаёт очередной ссылочный токен.
func (f *Fake) Enable() (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(MethodEnable); err != nil {
		return "", err
	}
	f.tokenSeq++
	token := fmt.Sprintf("%d", f.tokenSeq)
	f.tokens[token] = true
	f.enabled = true
	return token, nil
}

// Release освобождает ранее выданный токен.
func (f *Fake) Release(token string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(MethodRelease); err != nil {
		return err
	}
	if _, ok := f.tokens[token]; !ok {
		return fmt.Errorf("pftest: неизвестный токен %q", token)
	}
	f.tokens[token] = false
	return nil
}

// Enabled сообщает, включён ли pf.
func (f *Fake) Enabled() (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(MethodEnabled); err != nil {
		return false, err
	}
	return f.enabled, nil
}

// LoadAnchor заменяет содержимое якоря.
func (f *Fake) LoadAnchor(anchor string, rules []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(MethodLoadAnchor); err != nil {
		return err
	}
	f.anchors[anchor] = string(rules)
	return nil
}

// CheckAnchor изображает `pfctl -a <якорь> -n -f -`: разбирает правила,
// ничего не применяя. Настоящего парсера pf тут нет, поэтому фейк лишь
// запоминает поданный набор, а «непринимаемые правила» тест задаёт через
// Fail(MethodCheckAnchor, …).
func (f *Fake) CheckAnchor(anchor string, rules []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(MethodCheckAnchor); err != nil {
		return err
	}
	f.checked = append(f.checked, string(rules))
	return nil
}

// Checked возвращает наборы правил, отданные на проверку.
func (f *Fake) Checked() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.checked...)
}

// FlushAnchor очищает якорь целиком.
func (f *Fake) FlushAnchor(anchor string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(MethodFlushAnchor); err != nil {
		return err
	}
	// Правила исчезают, а сам якорь остаётся в дереве: настоящий pf после
	// `pfctl -a X -F all` продолжает показывать пустую оболочку в
	// `pfctl -s Anchors`. Демон обязан отличать её от живого туннеля.
	delete(f.anchors, anchor)
	return nil
}

// AnchorRules возвращает правила якоря так, как их показал бы
// `pfctl -a <якорь> -s rules`: комментарии и пустые строки отбрасываются,
// потому что в загруженном наборе правил их уже нет.
func (f *Fake) AnchorRules(anchor string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(MethodAnchorRules); err != nil {
		return "", err
	}
	return effectiveRules(f.anchors[anchor]), nil
}

// effectiveRules убирает из текста то, что pf не хранит: комментарии и пустоту.
func effectiveRules(text string) string {
	var kept []string
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		kept = append(kept, trimmed)
	}
	if len(kept) == 0 {
		return ""
	}
	return strings.Join(kept, "\n") + "\n"
}

// MainRules возвращает главный набор правил без раскрытия якорей.
func (f *Fake) MainRules() (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(MethodMainRules); err != nil {
		return "", err
	}
	return f.mainRules, nil
}

// SshuttleAnchors возвращает имена якорей sshuttle, заданные тестом.
func (f *Fake) SshuttleAnchors() ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(MethodSshuttleAnchors); err != nil {
		return nil, err
	}
	return append([]string(nil), f.sshuttle...), nil
}

// KillStates записывает попытку сбросить состояния к сети cidr.
func (f *Fake) KillStates(cidr string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(MethodKillStates); err != nil {
		return err
	}
	f.killed = append(f.killed, cidr)
	return nil
}

// ReloadMain эмулирует `pfctl -f confPath`: якоря sshuttle пропадают,
// а вызов якоря splitr возвращается в главный набор правил.
func (f *Fake) ReloadMain(confPath string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(MethodReloadMain); err != nil {
		return err
	}
	f.reloads = append(f.reloads, confPath)
	f.sshuttle = nil
	for _, a := range f.reloadLinks {
		f.linkLocked(a)
	}
	return nil
}
