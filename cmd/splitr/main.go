// Команда splitr: защита маршрутов через pf и менеджер туннеля sshuttle.
package main

import (
	"bufio"
	"context"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/tasticolly/splitr/internal/cli"
	"github.com/tasticolly/splitr/internal/config"
	"github.com/tasticolly/splitr/internal/daemon"
	"github.com/tasticolly/splitr/internal/logfile"
	"github.com/tasticolly/splitr/internal/pfctl"
	"github.com/tasticolly/splitr/internal/protect"
	"github.com/tasticolly/splitr/internal/shuttle"
	"github.com/tasticolly/splitr/internal/update"
)

//go:embed config.example.yaml
var exampleConfig []byte

const usage = `SplitR — sshuttle tunnel manager with pf-based route protection.

Usage: splitr <command> [flags]

  status [--json]           tunnel and protection state
  up [profile]              bring the tunnel up
  down                      take the tunnel down (protection stays)
  protect on|off|strict     protection state
  protect all|public|custom protected-route policy
  rules                     print the generated pf rules
  probe                     verify that protected routes are unreachable
  blocked                   live stream of dropped packets (needs protection.log)
  doctor                    check the installation and report what to fix
  reload                    re-read the config from disk
  config path|show|edit     config path, contents, edit in $EDITOR
  config apply <file>       apply a config file (via the daemon, no sudo)
  update [--yes]            check for a new version and install it
  log [--tail N]            last lines of the daemon log
  validate [file]           check a config and print its rules, applying nothing
  ui                        open the web interface
  version                   print the version of this binary
  install                   install the service, pf anchor and config (needs sudo)
  uninstall                 remove the service and pf.conf changes (needs sudo)
  daemon                    run the daemon in this process (needs sudo)

Flags: --config <path> (default ` + config.DefaultPath + `), --version
`

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "splitr:", err)
		os.Exit(1)
	}
}

func run() error {
	fs := flag.NewFlagSet("splitr", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	configPath := fs.String("config", config.DefaultPath, "path to the config file")
	jsonOut := fs.Bool("json", false, "print JSON")
	tailLines := fs.Int("tail", 200, "how many trailing log lines to print")
	assumeYes := fs.Bool("yes", false, "install the update without asking")

	// Команда может стоять в любом месте: и `splitr --config X status`,
	// и `splitr up docker --config X`. Стандартный flag останавливается на
	// первом позиционном аргументе, поэтому команду вынимаем сами.
	// Версия отвечает до загрузки конфига и до похода в демон: её спрашивают
	// у установленного бинаря именно тогда, когда всё остальное сломано.
	for _, arg := range os.Args[1:] {
		if arg == "--version" || arg == "-version" {
			printVersion()
			return nil
		}
	}

	cmd, flagArgs := splitCommand(os.Args[1:])
	if cmd == "" {
		fmt.Print(usage)
		return nil
	}
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	rest := fs.Args()

	switch cmd {
	case "help", "-h", "--help":
		fmt.Print(usage)
		return nil
	case "version":
		printVersion()
		return nil
	case "install":
		return cli.Install(*configPath, exampleConfig)
	case "uninstall":
		return cli.Uninstall(*configPath)
	case "daemon":
		return runDaemon(*configPath)
	case "validate":
		path := *configPath
		if len(rest) > 0 {
			path = rest[0]
		}
		cfg, err := config.Load(path)
		if err != nil {
			return err
		}
		fmt.Println("config", path, "is valid")
		name, p, err := cfg.Profile("")
		if err != nil {
			return err
		}
		fmt.Printf("\npf rules for profile %s:\n\n%s", name, protect.Build(cfg, p, false).Rules())
		return nil
	case "config":
		return configCmd(*configPath, socketPath(*configPath), rest)
	case "doctor":
		return cli.Doctor(*configPath)
	}

	socket := socketPath(*configPath)
	c := cli.NewClient(socket)

	switch cmd {
	case "status":
		data, err := c.Get("/status")
		if err != nil {
			return err
		}
		if *jsonOut {
			fmt.Println(strings.TrimSpace(string(data)))
			return nil
		}
		return printStatus(data)
	case "up":
		var profile string
		if len(rest) > 0 {
			profile = rest[0]
		}
		data, err := c.Post("/up", map[string]string{"profile": profile})
		if err != nil {
			return err
		}
		// sshuttle поднимается асинхронно, и требование заново войти приходит
		// уже после ответа. Без короткого ожидания команда рапортовала бы
		// «starting» ровно там, где на самом деле нужен человек.
		data = awaitTunnel(c, data, 8*time.Second)
		if err := printStatus(data); err != nil {
			return err
		}
		return actionError(data)
	case "down":
		data, err := c.Post("/down", nil)
		if err != nil {
			return err
		}
		return printStatus(data)
	case "protect":
		if len(rest) == 0 {
			data, err := c.Get("/status")
			if err != nil {
				return err
			}
			return printStatus(data)
		}
		req := map[string]string{"mode": rest[0]}
		switch rest[0] {
		case "on", "off", "strict":
		default:
			// Остальное — это смена политики защиты (all/public/custom/off).
			req = map[string]string{"policy": rest[0]}
		}
		data, err := c.Post("/protect", req)
		if err != nil {
			return err
		}
		return printStatus(data)
	case "reload":
		data, err := c.Post("/reload", nil)
		if err != nil {
			return err
		}
		return printStatus(data)
	case "rules":
		data, err := c.Get("/rules")
		if err != nil {
			return err
		}
		fmt.Print(string(data))
		return nil
	case "probe":
		data, err := c.Post("/probe", nil)
		if err != nil {
			return err
		}
		var report daemon.ProbeReport
		if err := json.Unmarshal(data, &report); err != nil {
			return err
		}
		fmt.Printf("control   %-22s %s\n", report.Control.Address, reachText(report.Control))
		for _, t := range report.Blocked {
			fmt.Printf("protected %-22s %s\n", t.Address, reachText(t))
		}
		fmt.Println()
		fmt.Println(report.Verdict)
		if report.Leaked && !report.TunnelUp {
			return fmt.Errorf("protection is not in effect")
		}
		return nil
	case "blocked":
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		fmt.Println("dropped packets (Ctrl+C to stop):")
		return c.Stream(ctx, "/blocked", func(line string) { fmt.Println(" ", line) })
	case "log":
		lines := *tailLines
		if len(rest) > 0 {
			n, err := strconv.Atoi(rest[0])
			if err != nil {
				return fmt.Errorf("log: %q is not a number", rest[0])
			}
			lines = n
		}
		data, err := c.Get("/log?tail=" + strconv.Itoa(lines))
		if err != nil {
			return err
		}
		fmt.Print(string(data))
		return nil
	case "update":
		return updateCmd(c, *assumeYes)
	case "ui":
		cfg, err := config.Load(*configPath)
		if err != nil {
			return err
		}
		url := "http://" + cfg.Daemon.HTTPAddr
		fmt.Println(url)
		return execOpen(url)
	default:
		fmt.Print(usage)
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func NewClientFor(socket string) *cli.Client { return cli.NewClient(socket) }

// updateCmd показывает состояние обновления и, с согласия человека, ставит его.
//
// Сборки здесь нет: собирать должен пользователь через `make update` — демон
// работает от root и оставил бы в репозитории root-овые артефакты.
func updateCmd(c *cli.Client, assumeYes bool) error {
	raw, err := c.Get("/update")
	if err != nil {
		return err
	}
	var st update.State
	if err := json.Unmarshal(raw, &st); err != nil {
		return err
	}

	fmt.Printf("installed:   %s\n", st.Installed)
	if st.Latest != "" {
		fmt.Printf("latest:      %s\n", st.Latest)
	}
	if st.RepoPath != "" {
		fmt.Printf("repository:  %s\n", st.RepoPath)
	}
	if st.Notes != "" {
		fmt.Printf("notes:       %s\n", st.Notes)
	}
	if !st.Available {
		fmt.Println()
		fmt.Println(st.Reason)
		return nil
	}

	fmt.Printf("\nupdate available: %s -> %s\n", st.Installed, st.Latest)
	if !assumeYes && !confirm(fmt.Sprintf("install %s? [y/N] ", st.Latest)) {
		fmt.Println("cancelled")
		return nil
	}

	raw, err = c.Post("/update", nil)
	if err != nil {
		return err
	}
	var res struct {
		Installed  string `json:"installed"`
		Restarting bool   `json:"restarting"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return err
	}
	fmt.Printf("installed %s; the daemon is restarting, protected routes stay protected\n", res.Installed)
	return nil
}

// confirm спрашивает подтверждение. Без терминала ответ считается отказом:
// молча подменить бинарь, потому что stdin пуст, было бы худшим из вариантов.
func confirm(prompt string) bool {
	fmt.Print(prompt)
	answer, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		return true
	default:
		return false
	}
}

// awaitTunnel ждёт, пока подъём туннеля к чему-нибудь придёт: он поднялся,
// он упал или от человека что-то требуется. Возвращает последний статус.
func awaitTunnel(c *cli.Client, current []byte, timeout time.Duration) []byte {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var s daemon.Status
		if err := json.Unmarshal(current, &s); err != nil {
			return current
		}
		if s.ActionRequired != nil || s.Tunnel != string(shuttle.StateStarting) {
			return current
		}
		time.Sleep(300 * time.Millisecond)
		next, err := c.Get("/status")
		if err != nil {
			return current
		}
		current = next
	}
	return current
}

// actionError превращает требование к человеку в ненулевой код возврата:
// ссылку нужно не просто увидеть, а скопировать и открыть.
func actionError(data []byte) error {
	var s daemon.Status
	if err := json.Unmarshal(data, &s); err != nil || s.ActionRequired == nil {
		return nil
	}
	return fmt.Errorf("%s: %s", s.ActionRequired.Message, s.ActionRequired.URL)
}

// printVersion печатает версию, проставленную при сборке через -ldflags.
func printVersion() { fmt.Println("splitr", daemon.Version) }

// splitCommand вынимает первое слово, не похожее на флаг, и возвращает его
// вместе с остальными аргументами. Значения флагов вида `--config X`
// пропускаются, чтобы X не был принят за команду.
func splitCommand(args []string) (string, []string) {
	valueFlags := map[string]bool{"-config": true, "--config": true, "-tail": true, "--tail": true}

	for i := 0; i < len(args); i++ {
		arg := args[i]
		if strings.HasPrefix(arg, "-") {
			if valueFlags[arg] && i+1 < len(args) {
				i++ // пропускаем значение флага
			}
			continue
		}
		return arg, append(append([]string{}, args[:i]...), args[i+1:]...)
	}
	return "", args
}

func reachText(t daemon.ProbeTarget) string {
	if t.Reachable {
		return "reachable — " + t.Detail
	}
	return "unreachable — " + t.Detail
}

func configCmd(path, socket string, rest []string) error {
	sub := "path"
	if len(rest) > 0 {
		sub = rest[0]
	}
	switch sub {
	case "edit":
		return editConfig(path, socket)
	case "apply":
		if len(rest) < 2 {
			return fmt.Errorf("config apply: file argument required")
		}
		raw, err := os.ReadFile(rest[1])
		if err != nil {
			return err
		}
		return applyConfig(socket, raw)
	case "path":
		fmt.Println(path)
		return nil
	case "show":
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		fmt.Print(string(data))
		return nil
	case "example":
		fmt.Print(string(exampleConfig))
		return nil
	default:
		return fmt.Errorf("config: expected path|show|example|edit|apply")
	}
}

// editConfig правит конфиг в $EDITOR и отдаёт результат демону.
// Через демона, а не напрямую: файл принадлежит root, а сокет доступен
// группе staff — значит правка обходится без sudo и проходит валидацию.
func editConfig(path, socket string) error {
	c := NewClientFor(socket)
	raw, err := c.Get("/config/raw")
	if err != nil {
		// Демон мог быть не поднят — тогда читаем файл напрямую.
		if raw, err = os.ReadFile(path); err != nil {
			return err
		}
	}

	tmp, err := os.CreateTemp("", "splitr-*.yaml")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(raw); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = "vi"
	}
	cmd := exec.Command(editor, tmp.Name())
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("editor exited with an error: %w", err)
	}

	edited, err := os.ReadFile(tmp.Name())
	if err != nil {
		return err
	}
	if string(edited) == string(raw) {
		fmt.Println("no changes")
		return nil
	}
	return applyConfig(socket, edited)
}

func applyConfig(socket string, raw []byte) error {
	data, err := NewClientFor(socket).PostRaw("/config", raw)
	if err != nil {
		return err
	}
	fmt.Println("config applied")
	return printStatus(data)
}

// socketPath достаёт путь сокета из конфига, не падая, если конфиг ещё не создан.
func socketPath(configPath string) string {
	if cfg, err := config.Load(configPath); err == nil {
		return cfg.Daemon.SocketPath
	}
	return config.Default().Daemon.SocketPath
}

func printStatus(data []byte) error {
	var s daemon.Status
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}

	blocking := "protected routes are reachable"
	if s.Blocking {
		blocking = "protected routes are dropped"
	}
	tunnel := s.Tunnel
	if s.External {
		tunnel = "external (started outside splitr)"
	}
	fmt.Printf("tunnel:      %s (profile %s)\n", tunnel, s.Profile)
	if s.PID != 0 {
		fmt.Printf("pid:         %d, since %s\n", s.PID, s.Since.Format(time.RFC3339))
	}
	fmt.Printf("protection:  %s — %s\n", s.Protection, blocking)
	fmt.Printf("pf:          enabled=%v, anchor loaded=%v, anchor linked=%v\n", s.PFEnabled, s.AnchorLoaded, s.AnchorLinked)
	if len(s.SshuttleAnchs) > 0 {
		fmt.Printf("sshuttle anchors: %s\n", strings.Join(s.SshuttleAnchs, ", "))
	}
	fmt.Printf("protected:   %d routes\n", len(s.BlockedNets))
	if s.Update.Available {
		fmt.Printf("update:      %s available — splitr update\n", s.Update.Latest)
	}
	// Требование к человеку печатается заметно и последним из основного блока:
	// именно оно объясняет, почему туннель не поднялся.
	if s.ActionRequired != nil {
		fmt.Printf("\naction required: %s\n", s.ActionRequired.Message)
		fmt.Printf("                 %s\n", s.ActionRequired.URL)
	}
	if s.LastError != "" {
		fmt.Printf("error:       %s\n", s.LastError)
	}
	for _, w := range s.Warnings {
		fmt.Printf("warning:     %s\n", w)
	}
	return nil
}

// execOpen открывает URL в браузере по умолчанию.
func execOpen(url string) error {
	return exec.Command("/usr/bin/open", url).Run()
}

func runDaemon(configPath string) error {
	// Демону нужен root, чтобы управлять pf. Исключение — режим разработки
	// с подменённым pfctl: там демон всё равно ничего не может изменить.
	if os.Geteuid() != 0 && os.Getenv(pfctl.BinaryEnv) == "" {
		return fmt.Errorf("the daemon needs root: sudo splitr daemon")
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}

	logw := io.Writer(os.Stderr)
	if cfg.Daemon.LogFile != "" {
		// Пишем через ротацию по размеру: launchd за ростом файла не следит,
		// а демон живёт месяцами. Stderr остаётся в паре с файлом — его
		// подбирает launchd и он же виден при запуске из терминала.
		f, err := logfile.Open(cfg.Daemon.LogFile, cfg.Daemon.LogMaxBytes, cfg.Daemon.LogKeep)
		if err != nil {
			return err
		}
		defer f.Close()
		logw = io.MultiWriter(os.Stderr, f)
	}

	d := daemon.New(cfg, configPath, logw)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		if err := d.Serve(ctx); err != nil {
			fmt.Fprintln(logw, "api server:", err)
		}
	}()

	fmt.Fprintf(logw, "splitr %s: anchor %s, config %s\n", daemon.Version, protect.Anchor, configPath)
	return d.Run(ctx)
}
