// Package config описывает конфигурацию splitr и правила её загрузки.
package config

import (
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// DefaultPath — канонический путь конфига, который читает демон.
const DefaultPath = "/usr/local/etc/splitr/config.yaml"

// ProtectionMode задаёт, какие из маршрутизируемых сетей резать при опущенном туннеле.
type ProtectionMode string

const (
	// ModeAll блокирует все сети из Subnets (кроме Excludes и Allow).
	ModeAll ProtectionMode = "all"
	// ModePublic блокирует только публичные (не RFC1918) сети — те, где реально утекает твой IP.
	ModePublic ProtectionMode = "public"
	// ModeCustom блокирует ровно то, что перечислено в Protection.Block.
	ModeCustom ProtectionMode = "custom"
	// ModeOff отключает Ð·Ð°ÑÐ¸ÑÐ°.
	ModeOff ProtectionMode = "off"
)

// Config — корень конфигурации.
type Config struct {
	DefaultProfile string             `yaml:"default_profile" json:"default_profile"`
	Subnets        []string           `yaml:"subnets" json:"subnets"`
	Excludes       []string           `yaml:"excludes" json:"excludes"`
	Protection     Protection         `yaml:"protection" json:"protection"`
	Profiles       map[string]Profile `yaml:"profiles" json:"profiles"`
	Sshuttle       Sshuttle           `yaml:"sshuttle" json:"sshuttle"`
	DNS            DNS                `yaml:"dns" json:"dns"`
	Daemon         Daemon             `yaml:"daemon" json:"daemon"`
	Update         Update             `yaml:"update" json:"update"`
}

// Update — откуда брать новые версии продукта.
//
// Удалённого репозитория нет: версии живут в тегах локального клона, из
// которого продукт и собирается. Пустой RepoPath означает, что обновляться
// неоткуда — это обычное состояние, а не ошибка конфигурации.
type Update struct {
	RepoPath string `yaml:"repo_path" json:"repo_path"`
	// CheckInterval — как часто демон спрашивает git о новых тегах.
	CheckInterval time.Duration `yaml:"check_interval" json:"check_interval"`
}

// Protection — настройки блокировки трафика при опущенном туннеле.
type Protection struct {
	Enabled bool           `yaml:"enabled" json:"enabled"`
	Mode    ProtectionMode `yaml:"mode" json:"mode"`
	// Block используется только при Mode == ModeCustom.
	Block []string `yaml:"block" json:"block"`
	// Allow — сети-исключения, которые не блокируются никогда (домашний LAN и т.п.).
	Allow []string `yaml:"allow" json:"allow"`
	// BlockDNS дополнительно режет DNS-запросы на внутренние резолверы.
	BlockDNS bool `yaml:"block_dns" json:"block_dns"`
	// DNSServers — адреса внутренних резолверов для BlockDNS.
	DNSServers []string `yaml:"dns_servers" json:"dns_servers"`
	// KillStates убивает живые pf-состояния к заблокированным сетям при падении туннеля.
	KillStates bool `yaml:"kill_states" json:"kill_states"`
	// Log включает запись отброшенных пакетов в pflog0,
	// чтобы можно было увидеть, что именно не ушло с машины.
	Log bool `yaml:"log" json:"log"`
}

// Profile — один вариант подключения (хост, через который поднимается туннель).
type Profile struct {
	Remote     string `yaml:"remote" json:"remote"`
	SSHKey     string `yaml:"ssh_key" json:"ssh_key"`
	KnownHosts string `yaml:"known_hosts" json:"known_hosts"`
	// DNS sends every name lookup on the machine through the tunnel.
	DNS bool `yaml:"dns" json:"dns"`
	// DNSServers narrows that down: only lookups addressed to these resolvers
	// go through the tunnel, everything else is left to resolve directly.
	//
	// It exists because "all DNS through the tunnel" is too blunt a setting to
	// live with. The remote side resolves through the corporate servers, which
	// answer for internal names and are also subject to whatever filtering the
	// network applies, so public names that the far end refuses to resolve stop
	// working on the laptop even though they are reachable from it. Naming the
	// internal resolvers here keeps internal names working without handing the
	// far side every other lookup.
	DNSServers []string `yaml:"dns_servers" json:"dns_servers"`
	// PreKillRemote — команды, которые выполняются на удалённом хосте перед подъёмом туннеля.
	PreKillRemote []string `yaml:"pre_kill_remote" json:"pre_kill_remote"`
	// Subnets переопределяет глобальный список сетей, если задан.
	Subnets []string `yaml:"subnets" json:"subnets"`
	// Excludes добавляется к глобальным исключениям.
	Excludes []string `yaml:"excludes" json:"excludes"`
}

// Sshuttle — как именно запускать бинарь sshuttle.
type Sshuttle struct {
	Path       string   `yaml:"path" json:"path"`
	SSHOptions []string `yaml:"ssh_options" json:"ssh_options"`
	ExtraArgs  []string `yaml:"extra_args" json:"extra_args"`
}

// DNS — что делать с системным резолвером при подъёме туннеля.
type DNS struct {
	UpdateScript string `yaml:"update_script" json:"update_script"`
	FlushCache   bool   `yaml:"flush_cache" json:"flush_cache"`
}

// Daemon — параметры фонового процесса.
type Daemon struct {
	HTTPAddr         string        `yaml:"http_addr" json:"http_addr"`
	SocketPath       string        `yaml:"socket_path" json:"socket_path"`
	SocketGroup      string        `yaml:"socket_group" json:"socket_group"`
	WatchdogInterval time.Duration `yaml:"watchdog_interval" json:"watchdog_interval"`
	Autoreconnect    bool          `yaml:"autoreconnect" json:"autoreconnect"`
	ReconnectDelay   time.Duration `yaml:"reconnect_delay" json:"reconnect_delay"`
	LogFile          string        `yaml:"log_file" json:"log_file"`
	// LogMaxBytes — размер, после которого журнал начинается заново.
	// Демон живёт месяцами и пишет каждый проход сторожа, поэтому без
	// потолка файл однажды съел бы диск — и уронил бы вместе с ним защиту.
	LogMaxBytes int64 `yaml:"log_max_bytes" json:"log_max_bytes"`
	// LogKeep — сколько прошлых поколений журнала хранить.
	LogKeep   int    `yaml:"log_keep" json:"log_keep"`
	StateFile string `yaml:"state_file" json:"state_file"`
	// ProbeControl — адрес заведомо вне блокируемых сетей.
	// Нужен, чтобы отличить сработавшую блокировку от отсутствия сети.
	ProbeControl string `yaml:"probe_control" json:"probe_control"`
}

// Default возвращает конфигурацию со значениями по умолчанию.
func Default() Config {
	return Config{
		Protection: Protection{
			Enabled:    true,
			Mode:       ModeAll,
			KillStates: true,
		},
		Sshuttle: Sshuttle{
			Path: "sshuttle",
			SSHOptions: []string{
				"ServerAliveInterval=15",
				"ServerAliveCountMax=4",
				"TCPKeepAlive=yes",
			},
		},
		DNS: DNS{FlushCache: true},
		Daemon: Daemon{
			HTTPAddr:         "127.0.0.1:8787",
			SocketPath:       "/var/run/splitr.sock",
			SocketGroup:      "staff",
			WatchdogInterval: 3 * time.Second,
			Autoreconnect:    false,
			ReconnectDelay:   5 * time.Second,
			LogFile:          "/usr/local/var/log/splitr.log",
			LogMaxBytes:      8 << 20,
			LogKeep:          3,
			StateFile:        "/usr/local/var/run/splitr.state.json",
			ProbeControl:     "1.1.1.1:443",
		},
		Update: Update{CheckInterval: time.Hour},
	}
}

// Load читает и валидирует конфиг. Пустой path означает DefaultPath.
func Load(path string) (Config, error) {
	if path == "" {
		path = DefaultPath
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config %s: %w", path, err)
	}

	cfg := Default()
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("parse config %s: %w", path, err)
	}

	cfg.expandPaths()
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c *Config) expandPaths() {
	home, _ := os.UserHomeDir()
	expand := func(p string) string {
		if home == "" || !strings.HasPrefix(p, "~/") {
			return p
		}
		return filepath.Join(home, p[2:])
	}
	for name, p := range c.Profiles {
		p.SSHKey = expand(p.SSHKey)
		p.KnownHosts = expand(p.KnownHosts)
		c.Profiles[name] = p
	}

	c.Sshuttle.Path = expand(c.Sshuttle.Path)
	c.DNS.UpdateScript = expand(c.DNS.UpdateScript)
	c.Daemon.LogFile = expand(c.Daemon.LogFile)
	c.Daemon.StateFile = expand(c.Daemon.StateFile)
	c.Daemon.SocketPath = expand(c.Daemon.SocketPath)
	c.Update.RepoPath = expand(c.Update.RepoPath)
}

// Validate проверяет конфиг на осмысленность: сети парсятся, профили заполнены.
func (c Config) Validate() error {
	if len(c.Profiles) == 0 {
		return fmt.Errorf("no profiles defined in profiles")
	}
	if c.DefaultProfile != "" {
		if _, ok := c.Profiles[c.DefaultProfile]; !ok {
			return fmt.Errorf("default_profile %q is missing from profiles", c.DefaultProfile)
		}
	} else if len(c.Profiles) > 1 {
		return fmt.Errorf("several profiles defined, set default_profile: %s", strings.Join(profileNames(c.Profiles), ", "))
	}
	for name, p := range c.Profiles {
		if p.Remote == "" {
			return fmt.Errorf("profile %q: remote is not set", name)
		}
		if err := checkPrefixes(fmt.Sprintf("profile %q subnets", name), p.Subnets); err != nil {
			return err
		}
		if p.DNS && len(p.DNSServers) > 0 {
			return fmt.Errorf("profile %q: dns and dns_servers contradict each other, dns sends every lookup through the tunnel and dns_servers only the ones aimed at the listed resolvers; keep one", name)
		}
		for _, addr := range p.DNSServers {
			if net.ParseIP(addr) == nil {
				return fmt.Errorf("profile %q: dns_servers entry %q is not an IP address", name, addr)
			}
		}
		if err := checkPrefixes(fmt.Sprintf("profile %q excludes", name), p.Excludes); err != nil {
			return err
		}
	}

	for label, list := range map[string][]string{
		"subnets":          c.Subnets,
		"excludes":         c.Excludes,
		"protection.block": c.Protection.Block,
		"protection.allow": c.Protection.Allow,
	} {
		if err := checkPrefixes(label, list); err != nil {
			return err
		}
	}

	for _, addr := range c.Protection.DNSServers {
		if _, err := netip.ParseAddr(addr); err != nil {
			return fmt.Errorf("protection.dns_servers: %q is not an IP address", addr)
		}
	}

	switch c.Protection.Mode {
	case ModeAll, ModePublic, ModeCustom, ModeOff:
	default:
		return fmt.Errorf("protection.mode: %q, expected all|public|custom|off", c.Protection.Mode)
	}
	if c.Protection.Mode == ModeCustom && len(c.Protection.Block) == 0 {
		return fmt.Errorf("protection.mode=custom, but protection.block is empty")
	}
	if c.Protection.BlockDNS && len(c.Protection.DNSServers) == 0 {
		return fmt.Errorf("protection.block_dns=true, but protection.dns_servers is empty")
	}
	if c.Daemon.WatchdogInterval <= 0 {
		return fmt.Errorf("daemon.watchdog_interval must be positive")
	}
	if c.Update.CheckInterval < 0 {
		return fmt.Errorf("update.check_interval must not be negative")
	}
	return nil
}

func checkPrefixes(label string, list []string) error {
	for _, s := range list {
		if _, err := netip.ParsePrefix(s); err != nil {
			return fmt.Errorf("%s: %q is not a CIDR (example: 10.0.0.0/8): %w", label, s, err)
		}
	}
	return nil
}

func profileNames(profiles map[string]Profile) []string {
	names := make([]string, 0, len(profiles))
	for name := range profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Profile возвращает профиль по имени.
//
// Пустое имя означает профиль по умолчанию; если он не задан, а профиль
// в конфиге ровно один — берётся он. Иначе требовать default_profile ради
// единственного варианта было бы придиркой.
func (c Config) Profile(name string) (string, Profile, error) {
	if name == "" {
		name = c.DefaultProfile
	}
	if name == "" && len(c.Profiles) == 1 {
		for only := range c.Profiles {
			name = only
		}
	}
	if name == "" {
		return "", Profile{}, fmt.Errorf("default_profile is not set; available: %s", strings.Join(profileNames(c.Profiles), ", "))
	}
	p, ok := c.Profiles[name]
	if !ok {
		return "", Profile{}, fmt.Errorf("unknown profile %q", name)
	}
	return name, p, nil
}

// RoutedSubnets возвращает сети, которые заворачиваются в туннель для профиля.
func (c Config) RoutedSubnets(p Profile) []string {
	if len(p.Subnets) > 0 {
		return p.Subnets
	}
	return c.Subnets
}

// ExcludedSubnets возвращает объединение глобальных и профильных исключений.
func (c Config) ExcludedSubnets(p Profile) []string {
	out := append([]string{}, c.Excludes...)
	return append(out, p.Excludes...)
}
