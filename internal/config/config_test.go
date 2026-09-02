package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeConfig кладёт конфиг во временный каталог и отдаёт путь к нему.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("записать временный конфиг: %v", err)
	}
	return path
}

// minimalConfig — наименьший конфиг, проходящий Validate.
const minimalConfig = `
profiles:
  pc:
    remote: user@example.invalid
`

func TestLoadExampleConfigParses(t *testing.T) {
	t.Parallel()

	cfg, err := Load("../../config.example.yaml")
	if err != nil {
		t.Fatalf("config.example.yaml должен загружаться без ошибок: %v", err)
	}
	if cfg.DefaultProfile != "pc" {
		t.Errorf("default_profile = %q, ожидалось visa", cfg.DefaultProfile)
	}
	if len(cfg.Subnets) != 9 {
		t.Errorf("в примере ожидалось 9 сетей, получено %d", len(cfg.Subnets))
	}
	if cfg.Protection.Mode != ModeAll || !cfg.Protection.Enabled || !cfg.Protection.KillStates {
		t.Errorf("protection разобран неверно: %+v", cfg.Protection)
	}
	for _, name := range []string{"pc", "pc", "laptop"} {
		if _, ok := cfg.Profiles[name]; !ok {
			t.Errorf("в примере нет профиля %q", name)
		}
	}
	if cfg.Daemon.WatchdogInterval != 3*time.Second {
		t.Errorf("watchdog_interval = %v, ожидалось 3s", cfg.Daemon.WatchdogInterval)
	}
	if cfg.Daemon.ReconnectDelay != 5*time.Second {
		t.Errorf("reconnect_delay = %v, ожидалось 5s", cfg.Daemon.ReconnectDelay)
	}
	if cfg.Sshuttle.Path != "/opt/homebrew/bin/sshuttle" {
		t.Errorf("sshuttle.path = %q", cfg.Sshuttle.Path)
	}
}

// cmd/splitr возит собственную копию примера — она обязана оставаться валидной.
func TestLoadEmbeddedExampleConfigParses(t *testing.T) {
	t.Parallel()

	if _, err := Load("../../cmd/splitr/config.example.yaml"); err != nil {
		t.Fatalf("cmd/splitr/config.example.yaml не загружается: %v", err)
	}
}

func TestLoadKeepsDefaultsForOmittedFields(t *testing.T) {
	t.Parallel()

	cfg, err := Load(writeConfig(t, minimalConfig))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	def := Default()
	if cfg.Daemon.SocketPath != def.Daemon.SocketPath {
		t.Errorf("socket_path = %q, ожидался дефолт %q", cfg.Daemon.SocketPath, def.Daemon.SocketPath)
	}
	if cfg.Daemon.WatchdogInterval != def.Daemon.WatchdogInterval {
		t.Errorf("watchdog_interval = %v, ожидался дефолт %v", cfg.Daemon.WatchdogInterval, def.Daemon.WatchdogInterval)
	}
	if cfg.Sshuttle.Path != "sshuttle" {
		t.Errorf("sshuttle.path = %q, ожидался дефолт sshuttle", cfg.Sshuttle.Path)
	}
	if len(cfg.Sshuttle.SSHOptions) != len(def.Sshuttle.SSHOptions) {
		t.Errorf("ssh_options = %v, ожидались дефолтные %v", cfg.Sshuttle.SSHOptions, def.Sshuttle.SSHOptions)
	}
	if !cfg.Protection.Enabled || cfg.Protection.Mode != ModeAll || !cfg.Protection.KillStates {
		t.Errorf("protection по умолчанию должен быть включён в режиме all: %+v", cfg.Protection)
	}
	if !cfg.DNS.FlushCache {
		t.Error("dns.flush_cache по умолчанию должен быть true")
	}
	// default_profile не имеет разумного значения по умолчанию: имя профиля
	// знает только человек. Пустое значение допустимо, пока профиль один.
	if cfg.DefaultProfile != "" {
		t.Errorf("default_profile = %q, ожидалось пустое значение", cfg.DefaultProfile)
	}
}

// Тильда в путях профиля раскрывается в домашний каталог.
func TestLoadExpandsTildeInProfilePaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cfg, err := Load(writeConfig(t, `
profiles:
  pc:
    remote: user@example.invalid
    ssh_key: ~/.ssh/id_ed25519
    known_hosts: ~/.ssh/known_hosts
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	p := cfg.Profiles["pc"]
	if want := filepath.Join(home, ".ssh/id_ed25519"); p.SSHKey != want {
		t.Errorf("ssh_key = %q, ожидалось %q", p.SSHKey, want)
	}
	if want := filepath.Join(home, ".ssh/known_hosts"); p.KnownHosts != want {
		t.Errorf("known_hosts = %q, ожидалось %q", p.KnownHosts, want)
	}
}

// Раскрывается только префикс "~/" — всё прочее остаётся как есть.
func TestLoadLeavesNonTildePathsUntouched(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cfg, err := Load(writeConfig(t, `
profiles:
  pc:
    remote: user@example.invalid
    ssh_key: /absolute/key
    known_hosts: ~other/known_hosts
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	p := cfg.Profiles["pc"]
	if p.SSHKey != "/absolute/key" {
		t.Errorf("абсолютный путь изменён: %q", p.SSHKey)
	}
	if p.KnownHosts != "~other/known_hosts" {
		t.Errorf("~other не является домашним каталогом текущего пользователя, путь должен остаться прежним: %q", p.KnownHosts)
	}
}

// Опечатка в имени поля обязана быть ошибкой, а не молча игнорироваться:
// молчаливо проигнорированный protection превратил бы защиту в фикцию.
func TestLoadRejectsUnknownFields(t *testing.T) {
	t.Parallel()

	_, err := Load(writeConfig(t, `
protection:
  enabld: true
profiles:
  pc:
    remote: user@example.invalid
`))
	if err == nil {
		t.Fatal("неизвестное поле должно приводить к ошибке")
	}
	if !strings.Contains(err.Error(), "parse config") {
		t.Errorf("ошибка = %v, ожидалось сообщение о разборе конфига", err)
	}
}

func TestLoadMissingFileReturnsError(t *testing.T) {
	t.Parallel()

	_, err := Load(filepath.Join(t.TempDir(), "нет-такого.yaml"))
	if err == nil {
		t.Fatal("отсутствующий файл должен приводить к ошибке")
	}
	if !strings.Contains(err.Error(), "read config") {
		t.Errorf("ошибка = %v, ожидалось сообщение о чтении конфига", err)
	}
}

func TestLoadPropagatesValidationError(t *testing.T) {
	t.Parallel()

	_, err := Load(writeConfig(t, "subnets: [10.0.0.0/9]\n"))
	if err == nil || !strings.Contains(err.Error(), "profiles") {
		t.Fatalf("ошибка = %v, ожидалась жалоба на отсутствие профилей", err)
	}
}

// valid возвращает заведомо корректный конфиг, который тесты портят точечно.
func valid() Config {
	cfg := Default()
	cfg.Subnets = []string{"10.0.0.0/9"}
	cfg.Profiles = map[string]Profile{"pc": {Remote: "user@example.invalid"}}
	return cfg
}

func TestValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string // подстрока; пустая означает «ошибки быть не должно»
	}{
		{
			name:   "корректный конфиг",
			mutate: func(*Config) {},
		},
		{
			name:    "нет ни одного профиля",
			mutate:  func(c *Config) { c.Profiles = nil },
			wantErr: "no profiles defined",
		},
		{
			name:    "default_profile указывает в никуда",
			mutate:  func(c *Config) { c.DefaultProfile = "нет-такого" },
			wantErr: "is missing from profiles",
		},
		{
			name:   "пустой default_profile допустим",
			mutate: func(c *Config) { c.DefaultProfile = "" },
		},
		{
			name:    "профиль без remote",
			mutate:  func(c *Config) { c.Profiles = map[string]Profile{"pc": {}} },
			wantErr: `profile "pc": remote is not set`,
		},
		{
			name: "битый CIDR в subnets профиля",
			mutate: func(c *Config) {
				c.Profiles = map[string]Profile{"pc": {Remote: "u@h", Subnets: []string{"10.0.0.0"}}}
			},
			wantErr: `profile "pc" subnets`,
		},
		{
			name: "битый CIDR в excludes профиля",
			mutate: func(c *Config) {
				c.Profiles = map[string]Profile{"pc": {Remote: "u@h", Excludes: []string{"не-сеть"}}}
			},
			wantErr: `profile "pc" excludes`,
		},
		{
			name:    "битый CIDR в subnets",
			mutate:  func(c *Config) { c.Subnets = []string{"10.0.0.0/99"} },
			wantErr: "subnets",
		},
		{
			name:    "битый CIDR в excludes",
			mutate:  func(c *Config) { c.Excludes = []string{"192.168.1.1"} },
			wantErr: "excludes",
		},
		{
			name: "битый CIDR в protect.block",
			mutate: func(c *Config) {
				c.Protection.Mode = ModeCustom
				c.Protection.Block = []string{"мусор"}
			},
			wantErr: "protection.block",
		},
		{
			name:    "битый CIDR в protect.allow",
			mutate:  func(c *Config) { c.Protection.Allow = []string{"192.168.1.0"} },
			wantErr: "protection.allow",
		},
		{
			name:    "dns_servers принимает только IP, не CIDR",
			mutate:  func(c *Config) { c.Protection.DNSServers = []string{"10.0.0.1/32"} },
			wantErr: "is not an IP address",
		},
		{
			name:    "неизвестный режим Ð·Ð°ÑÐ¸ÑÐ°",
			mutate:  func(c *Config) { c.Protection.Mode = "иногда" },
			wantErr: "expected all|public|custom|off",
		},
		{
			name:    "пустой режим Ð·Ð°ÑÐ¸ÑÐ° тоже неизвестен",
			mutate:  func(c *Config) { c.Protection.Mode = "" },
			wantErr: "expected all|public|custom|off",
		},
		{
			name:    "mode=custom без block",
			mutate:  func(c *Config) { c.Protection.Mode = ModeCustom },
			wantErr: "protection.block is empty",
		},
		{
			name: "mode=custom с block проходит",
			mutate: func(c *Config) {
				c.Protection.Mode = ModeCustom
				c.Protection.Block = []string{"10.0.0.0/9"}
			},
		},
		{
			name:    "block_dns без dns_servers",
			mutate:  func(c *Config) { c.Protection.BlockDNS = true },
			wantErr: "dns_servers is empty",
		},
		{
			name: "block_dns с dns_servers проходит",
			mutate: func(c *Config) {
				c.Protection.BlockDNS = true
				c.Protection.DNSServers = []string{"10.0.0.1"}
			},
		},
		{
			name:    "нулевой watchdog_interval",
			mutate:  func(c *Config) { c.Daemon.WatchdogInterval = 0 },
			wantErr: "watchdog_interval must be positive",
		},
		{
			name:    "отрицательный watchdog_interval",
			mutate:  func(c *Config) { c.Daemon.WatchdogInterval = -time.Second },
			wantErr: "watchdog_interval must be positive",
		},
		{
			name:   "mode=off не требует block",
			mutate: func(c *Config) { c.Protection.Mode = ModeOff },
		},
		{
			name:   "mode=public допустим",
			mutate: func(c *Config) { c.Protection.Mode = ModePublic },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := valid()
			tt.mutate(&cfg)
			err := cfg.Validate()
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("ожидался валидный конфиг, получена ошибка: %v", err)
			case tt.wantErr != "" && err == nil:
				t.Fatalf("ожидалась ошибка с подстрокой %q, получено nil", tt.wantErr)
			case tt.wantErr != "" && !strings.Contains(err.Error(), tt.wantErr):
				t.Fatalf("ошибка = %v, ожидалась подстрока %q", err, tt.wantErr)
			}
		})
	}
}

func TestProfileLookup(t *testing.T) {
	t.Parallel()

	cfg := valid()
	cfg.DefaultProfile = "pc"
	cfg.Profiles["laptop"] = Profile{Remote: "user@laptop"}

	t.Run("пустое имя берёт профиль по умолчанию", func(t *testing.T) {
		t.Parallel()
		name, p, err := cfg.Profile("")
		if err != nil {
			t.Fatalf("Profile(\"\"): %v", err)
		}
		if name != "pc" || p.Remote != "user@example.invalid" {
			t.Fatalf("получено %q/%+v, ожидался профиль pc", name, p)
		}
	})

	t.Run("явное имя", func(t *testing.T) {
		t.Parallel()
		name, p, err := cfg.Profile("laptop")
		if err != nil {
			t.Fatalf("Profile(\"laptop\"): %v", err)
		}
		if name != "laptop" || p.Remote != "user@laptop" {
			t.Fatalf("получено %q/%+v, ожидался профиль laptop", name, p)
		}
	})

	t.Run("неизвестное имя — ошибка", func(t *testing.T) {
		t.Parallel()
		if _, _, err := cfg.Profile("нет-такого"); err == nil {
			t.Fatal("ожидалась ошибка про неизвестный профиль")
		}
	})

	// Единственный профиль не требует default_profile: требовать выбор
	// из одного варианта — придирка.
	t.Run("единственный профиль берётся без default_profile", func(t *testing.T) {
		t.Parallel()
		c := valid()
		c.DefaultProfile = ""
		name, _, err := c.Profile("")
		if err != nil {
			t.Fatalf("Profile(\"\"): %v", err)
		}
		if name != "pc" {
			t.Fatalf("получен профиль %q, ожидался единственный pc", name)
		}
	})

	t.Run("несколько профилей без default_profile — ошибка", func(t *testing.T) {
		t.Parallel()
		c := valid()
		c.DefaultProfile = ""
		c.Profiles["laptop"] = Profile{Remote: "user@laptop"}
		if _, _, err := c.Profile(""); err == nil {
			t.Fatal("при нескольких профилях пустое имя не должно разрешаться")
		}
	})
}

func TestRoutedSubnets(t *testing.T) {
	t.Parallel()

	cfg := valid()
	cfg.Subnets = []string{"10.0.0.0/9", "11.0.0.0/8"}

	t.Run("без профильного списка берутся глобальные сети", func(t *testing.T) {
		t.Parallel()
		got := cfg.RoutedSubnets(Profile{})
		if strings.Join(got, ",") != "10.0.0.0/9,11.0.0.0/8" {
			t.Fatalf("RoutedSubnets = %v", got)
		}
	})

	t.Run("профильный список полностью заменяет глобальный", func(t *testing.T) {
		t.Parallel()
		got := cfg.RoutedSubnets(Profile{Subnets: []string{"172.16.0.0/12"}})
		if strings.Join(got, ",") != "172.16.0.0/12" {
			t.Fatalf("RoutedSubnets = %v, ожидалось только профильное значение", got)
		}
	})
}

// Исключения профиля не заменяют глобальные, а добавляются к ним.
func TestExcludedSubnetsMergesGlobalAndProfile(t *testing.T) {
	t.Parallel()

	cfg := valid()
	cfg.Excludes = []string{"192.168.1.0/24"}
	got := cfg.ExcludedSubnets(Profile{Excludes: []string{"192.168.2.0/24"}})
	if strings.Join(got, ",") != "192.168.1.0/24,192.168.2.0/24" {
		t.Fatalf("ExcludedSubnets = %v", got)
	}
	if len(cfg.Excludes) != 1 {
		t.Fatalf("ExcludedSubnets не должен менять cfg.Excludes, стало %v", cfg.Excludes)
	}
}

// Возвращённый срез не должен делить память с cfg.Excludes:
// иначе правка результата испортила бы конфиг.
func TestExcludedSubnetsReturnsIndependentSlice(t *testing.T) {
	t.Parallel()

	cfg := valid()
	cfg.Excludes = []string{"192.168.1.0/24"}
	got := cfg.ExcludedSubnets(Profile{})
	got[0] = "0.0.0.0/0"
	if cfg.Excludes[0] != "192.168.1.0/24" {
		t.Fatalf("конфиг изменился через результат ExcludedSubnets: %v", cfg.Excludes)
	}
}

func TestDefaultIsValidAfterAddingProfile(t *testing.T) {
	t.Parallel()

	cfg := Default()
	cfg.Profiles = map[string]Profile{"pc": {Remote: "user@example.invalid"}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("дефолтная конфигурация с одним профилем должна быть валидной: %v", err)
	}
}

func TestLoadProtectionLogFlag(t *testing.T) {
	t.Parallel()

	cfg, err := Load(writeConfig(t, `
protection:
  log: true
profiles:
  pc:
    remote: user@example.invalid
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Protection.Log {
		t.Fatal("protect.log не разобран")
	}
}

// У журнала обязан быть потолок по умолчанию: демон работает месяцами,
// и файл без предела однажды съел бы диск вместе с защитой.
func TestDefaultLogRotationIsBounded(t *testing.T) {
	t.Parallel()

	d := Default().Daemon
	if d.LogMaxBytes <= 0 {
		t.Fatalf("log_max_bytes = %d, журнал не ограничен", d.LogMaxBytes)
	}
	if d.LogKeep <= 0 {
		t.Fatalf("log_keep = %d, прошлые поколения не сохраняются", d.LogKeep)
	}
}

// Образец конфига обязан разбираться: он же и подсказка, и то, что кладёт install.
func TestExampleConfigParsesWithRotationFields(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.yaml")
	raw, err := os.ReadFile("../../cmd/splitr/config.example.yaml")
	if err != nil {
		t.Skipf("образец конфига недоступен: %v", err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("образец конфига не разбирается: %v", err)
	}
	if cfg.Daemon.LogMaxBytes <= 0 || cfg.Daemon.LogKeep <= 0 {
		t.Fatalf("настройки ротации не прочитались: %+v", cfg.Daemon)
	}
}

// Секция update необязательна: конфиг, написанный до появления обновления
// по кнопке, обязан читаться как прежде, а обновление — просто быть недоступно.
func TestUpdateSectionIsOptional(t *testing.T) {
	t.Parallel()

	cfg, err := Load(writeConfig(t, minimalConfig))
	if err != nil {
		t.Fatalf("загрузить конфиг: %v", err)
	}
	if cfg.Update.RepoPath != "" {
		t.Fatalf("repo_path = %q, ожидался пустой", cfg.Update.RepoPath)
	}
	if cfg.Update.CheckInterval != time.Hour {
		t.Fatalf("check_interval = %s, ожидался час по умолчанию", cfg.Update.CheckInterval)
	}
}

func TestUpdateSectionParsed(t *testing.T) {
	t.Parallel()

	cfg, err := Load(writeConfig(t, `
profiles:
  alpha:
    remote: user@alpha
update:
  repo_path: /tmp/splitr-src
  check_interval: 15m
`))
	if err != nil {
		t.Fatalf("загрузить конфиг: %v", err)
	}
	if cfg.Update.RepoPath != "/tmp/splitr-src" || cfg.Update.CheckInterval != 15*time.Minute {
		t.Fatalf("update = %+v", cfg.Update)
	}
}

func TestNegativeCheckIntervalRejected(t *testing.T) {
	t.Parallel()

	_, err := Load(writeConfig(t, `
profiles:
  alpha:
    remote: user@alpha
update:
  check_interval: -1h
`))
	if err == nil || !strings.Contains(err.Error(), "update.check_interval") {
		t.Fatalf("ошибка = %v, ожидался отказ из-за отрицательного интервала", err)
	}
}
