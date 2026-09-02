package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/tasticolly/splitr/internal/config"
)

// persistedState — то, что демон помнит между перезапусками.
// Без этого снятая через UI защита молча возвращалась бы после перезагрузки,
// а выбранный профиль сбрасывался бы на профиль по умолчанию.
type persistedState struct {
	ActiveProfile     string `json:"active_profile"`
	StrictMode        bool   `json:"strict_mode"`
	ProtectionEnabled *bool  `json:"protection_enabled,omitempty"`
	// Mode — режим блокировки, выбранный через UI поверх конфига.
	Mode string `json:"mode,omitempty"`
	// PFToken — ссылка на включённый pf, выданная прошлому запуску демона.
	// Хранится, чтобы при перезапуске сначала взять новую ссылку и только
	// потом отпустить старую: иначе pf выключился бы вместе с блокировкой.
	PFToken string `json:"pf_token,omitempty"`
	// DNSBackup holds the resolvers that were in place before update_script
	// pointed the system at the tunnel. It is persisted because the daemon can
	// be restarted while the redirect is live, and a backup kept only in memory
	// would be lost exactly when it is needed to undo the redirect.
	DNSBackup []string `json:"dns_backup,omitempty"`
	// DNSRedirected records that update_script has run and not been undone.
	// It cannot be inferred from DNSBackup: an empty backup is a legitimate
	// state (no resolvers were configured) and must still be restored.
	DNSRedirected bool `json:"dns_redirected,omitempty"`
}

// LoadPFToken достаёт токен pf из файла состояния.
// Нужен команде uninstall, чтобы отпустить ссылку и дать pf выключиться.
func LoadPFToken(stateFile string) (string, error) {
	raw, err := os.ReadFile(stateFile)
	if err != nil {
		return "", err
	}
	var st persistedState
	if err := json.Unmarshal(raw, &st); err != nil {
		return "", err
	}
	return st.PFToken, nil
}

func (d *Daemon) saveState() {
	path := d.Config().Daemon.StateFile
	if path == "" {
		return
	}

	d.mu.RLock()
	st := persistedState{
		ActiveProfile:     d.activeName,
		StrictMode:        d.strictMode,
		ProtectionEnabled: &d.cfg.Protection.Enabled,
		Mode:              string(d.cfg.Protection.Mode),
		PFToken:           d.pfToken,
		DNSBackup:         d.dnsBackup,
		DNSRedirected:     d.dnsRedirected,
	}
	d.mu.RUnlock()

	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		d.logf("save state: %v", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		d.logf("save state: %v", err)
		return
	}
	// Запись через временный файл: оборванная запись не должна оставить
	// демону мусор, из-за которого он не поднимется в следующий раз.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		d.logf("save state: %v", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		d.logf("save state: %v", err)
	}
}

// restoreState поднимает сохранённые переключатели.
// Расхождение с конфигом объясняется в журнале: иначе человек не поймёт,
// почему защита выключена, хотя в конфиге enabled: true.
func (d *Daemon) restoreState() {
	path := d.Config().Daemon.StateFile
	if path == "" {
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			d.logf("read state %s: %v", path, err)
		}
		return
	}

	var st persistedState
	if err := json.Unmarshal(raw, &st); err != nil {
		d.logf("state %s is corrupt, ignoring it: %v", path, err)
		return
	}

	d.previousPFToken = st.PFToken

	d.mu.Lock()
	if _, ok := d.cfg.Profiles[st.ActiveProfile]; ok {
		d.activeName = st.ActiveProfile
	}
	d.strictMode = st.StrictMode
	// An outstanding redirect is carried across the restart so that the first
	// tunnel loss after it undoes the redirect this daemon never applied.
	d.dnsBackup = st.DNSBackup
	d.dnsRedirected = st.DNSRedirected
	if st.Mode != "" && config.ProtectionMode(st.Mode) != d.cfg.Protection.Mode {
		d.cfg.Protection.Mode = config.ProtectionMode(st.Mode)
	}
	var note string
	if st.ProtectionEnabled != nil && *st.ProtectionEnabled != d.cfg.Protection.Enabled {
		d.cfg.Protection.Enabled = *st.ProtectionEnabled
		note = fmt.Sprintf("; protection taken from saved state: enabled=%v (the config says otherwise)", *st.ProtectionEnabled)
	}
	profile := d.activeName
	strictMode := d.strictMode
	d.mu.Unlock()

	d.logf("state restored: profile %s, strict=%v%s", profile, strictMode, note)
}
