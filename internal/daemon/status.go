package daemon

import (
	"time"

	"github.com/tasticolly/splitr/internal/shuttle"
	"github.com/tasticolly/splitr/internal/update"
)

// Status — снимок состояния, который отдаётся CLI и веб-интерфейсу.
type Status struct {
	Tunnel        string    `json:"tunnel"`
	Profile       string    `json:"profile"`
	PID           int       `json:"pid,omitempty"`
	Since         time.Time `json:"since,omitempty"`
	LastError     string    `json:"last_error,omitempty"`
	PFEnabled     bool      `json:"pf_enabled"`
	AnchorLoaded  bool      `json:"anchor_loaded"`
	AnchorLinked  bool      `json:"anchor_linked"`
	Protection    string    `json:"protection"`
	Blocking      bool      `json:"blocking"`
	BlockedNets   []string  `json:"blocked_nets"`
	AllowedNets   []string  `json:"allowed_nets"`
	SshuttleAnchs []string  `json:"sshuttle_anchors"`
	External      bool      `json:"external"`
	ConfigPath    string    `json:"config_path"`
	Mode          string    `json:"mode"`
	Version       string    `json:"version"`
	StartedAt     time.Time `json:"started_at"`
	LogFile       string    `json:"log_file"`
	Warnings      []string  `json:"warnings,omitempty"`
	// Update — состояние обновления, чтобы приложению в строке меню хватало
	// одного запроса /status для отрисовки кнопки.
	Update update.State `json:"update"`
	// ActionRequired — что человек должен сделать руками, чтобы туннель
	// поднялся: например, заново войти по ссылке. Отсутствует, когда ничего
	// не требуется.
	ActionRequired *shuttle.ActionRequired `json:"action_required,omitempty"`
}
