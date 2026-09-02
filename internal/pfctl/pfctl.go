// Package pfctl — работа с пакетным фильтром pf(4) macOS через утилиту pfctl(8).
package pfctl

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

// DefaultBinary — штатный путь к pfctl в macOS.
const DefaultBinary = "/sbin/pfctl"

// Controller — то, что splitr умеет спрашивать у pf.
// Интерфейс существует ради тестов: боевая реализация зовёт pfctl(8),
// тестовая (пакет pftest) отвечает из памяти.
type Controller interface {
	Enable() (token string, err error)
	Release(token string) error
	Enabled() (bool, error)
	LoadAnchor(anchor string, rules []byte) error
	CheckAnchor(anchor string, rules []byte) error
	FlushAnchor(anchor string) error
	AnchorRules(anchor string) (string, error)
	MainRules() (string, error)
	SshuttleAnchors() ([]string, error)
	KillStates(cidr string) error
	ReloadMain(confPath string) error
}

var tokenRe = regexp.MustCompile(`Token\s*:\s*(\d+)`)

// CLI — боевая реализация Controller поверх утилиты pfctl.
type CLI struct {
	binary string
}

// BinaryEnv позволяет подменить pfctl при разработке и в тестовых стендах,
// где настоящего pf нет. В боевой установке переменная не задаётся.
const BinaryEnv = "SPLITR_PFCTL"

// New создаёт CLI со штатным путём к pfctl.
func New() *CLI {
	if custom := os.Getenv(BinaryEnv); custom != "" {
		return &CLI{binary: custom}
	}
	return &CLI{binary: DefaultBinary}
}

// NewWithBinary создаёт CLI с нестандартным путём к pfctl (нужно тестам).
func NewWithBinary(binary string) *CLI { return &CLI{binary: binary} }

// Run выполняет pfctl с заданными аргументами, подавая stdin (может быть nil).
// pfctl пишет диагностику в stderr даже при успехе, поэтому возвращаются оба потока.
func (c *CLI) Run(stdin []byte, args ...string) (stdout, stderr string, err error) {
	cmd := exec.Command(c.binary, args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	err = cmd.Run()
	if err != nil {
		err = fmt.Errorf("pfctl %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errBuf.String()))
	}
	return out.String(), errBuf.String(), err
}

// Enable включает pf и возвращает reference-токен, который нужно вернуть через Release.
// Токены считает ядро, поэтому включение не мешает sshuttle и AnyConnect.
func (c *CLI) Enable() (string, error) {
	_, stderr, err := c.Run(nil, "-E")
	if err != nil {
		return "", err
	}
	m := tokenRe.FindStringSubmatch(stderr)
	if m == nil {
		return "", fmt.Errorf("pfctl -E: could not parse the token out of %q", strings.TrimSpace(stderr))
	}
	return m[1], nil
}

// Release освобождает ссылку на включённый pf.
func (c *CLI) Release(token string) error {
	_, _, err := c.Run(nil, "-X", token)
	return err
}

// Enabled сообщает, включён ли pf сейчас.
func (c *CLI) Enabled() (bool, error) {
	out, _, err := c.Run(nil, "-s", "info")
	if err != nil {
		return false, err
	}
	return strings.Contains(out, "Status: Enabled"), nil
}

// LoadAnchor загружает набор правил в именованный якорь.
func (c *CLI) LoadAnchor(anchor string, rules []byte) error {
	_, _, err := c.Run(rules, "-a", anchor, "-f", "-")
	return err
}

// CheckAnchor разбирает правила настоящим парсером pf, ничего не применяя.
func (c *CLI) CheckAnchor(anchor string, rules []byte) error {
	_, _, err := c.Run(rules, "-a", anchor, "-n", "-f", "-")
	return err
}

// FlushAnchor очищает якорь целиком.
func (c *CLI) FlushAnchor(anchor string) error {
	_, _, err := c.Run(nil, "-a", anchor, "-F", "all")
	return err
}

// AnchorRules возвращает правила, загруженные в якорь.
func (c *CLI) AnchorRules(anchor string) (string, error) {
	out, _, err := c.Run(nil, "-a", anchor, "-s", "rules")
	return out, err
}

// MainRules возвращает главный набор правил без раскрытия якорей.
func (c *CLI) MainRules() (string, error) {
	out, _, err := c.Run(nil, "-s", "rules")
	return out, err
}

// SshuttleAnchors возвращает имена активных якорей sshuttle.
func (c *CLI) SshuttleAnchors() ([]string, error) {
	out, _, err := c.Run(nil, "-s", "Anchors")
	if err != nil {
		return nil, err
	}
	return ParseSshuttleAnchors(out), nil
}

// KillStates сбрасывает живые состояния к указанной сети,
// чтобы уже открытые соединения не пережили падение туннеля.
//
// Это подчистка на всякий случай, а не несущая часть защиты: состояния
// туннеля живут на lo0 и исчезают вместе с якорем sshuttle, а прямые
// состояния к защищаемым сетям заводятся только при снятой блокировке.
// Ключ -k в pf, унаследованном macOS, рассчитан на хост, и на широкой маске
// может не сработать — поэтому ошибка возвращается, но не считается фатальной.
func (c *CLI) KillStates(cidr string) error {
	_, _, err := c.Run(nil, "-k", "0.0.0.0/0", "-k", cidr)
	if err != nil {
		return fmt.Errorf("flushing states to %s is not supported by this pfctl: %w", cidr, err)
	}
	return nil
}

// ReloadMain перезагружает главный набор правил из файла.
// Вызов сбрасывает динамически добавленные якоря (в том числе sshuttle),
// поэтому применять его можно только при опущенном туннеле.
func (c *CLI) ReloadMain(confPath string) error {
	_, _, err := c.Run(nil, "-f", confPath)
	return err
}

// ParseSshuttleAnchors выбирает якоря sshuttle из вывода `pfctl -s Anchors`.
func ParseSshuttleAnchors(out string) []string {
	var found []string
	for _, line := range strings.Split(out, "\n") {
		name := strings.TrimSpace(line)
		if strings.HasPrefix(name, "sshuttle-") {
			found = append(found, name)
		}
	}
	return found
}

// AnchorReferenced сообщает, вызывает ли главный набор правил якорь anchor.
// Без такого вызова правила внутри якоря просто не вычисляются.
func AnchorReferenced(c Controller, anchor string) (bool, error) {
	rules, err := c.MainRules()
	if err != nil {
		return false, err
	}
	return strings.Contains(rules, `anchor "`+anchor+`"`), nil
}
