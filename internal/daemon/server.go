package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/tasticolly/splitr/internal/config"
)

// Serve поднимает управляющий API на unix-сокете и (опционально) на localhost.
// Работает до отмены контекста.
func (d *Daemon) Serve(ctx context.Context) error {
	cfg := d.Config()
	mux := d.routes()

	var listeners []net.Listener
	if path := cfg.Daemon.SocketPath; path != "" {
		ln, err := d.listenUnix(path, cfg.Daemon.SocketGroup)
		if err != nil {
			return err
		}
		listeners = append(listeners, ln)
		d.logf("control socket: %s", path)
	}
	if addr := cfg.Daemon.HTTPAddr; addr != "" {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return fmt.Errorf("listen on %s: %w", addr, err)
		}
		listeners = append(listeners, ln)
		d.logf("web interface: http://%s", addr)
	}
	if len(listeners) == 0 {
		<-ctx.Done()
		return nil
	}

	// Серверы создаются до запуска горутин: иначе список, из которого идёт
	// остановка, дописывался бы из нескольких горутин одновременно.
	srvs := make([]*http.Server, 0, len(listeners))
	for _, ln := range listeners {
		privileged := ln.Addr().Network() == "unix"
		// Порядок обёрток важен: аудит обязан быть ВНУТРИ markTransport,
		// иначе он не увидит признак транспорта и назовёт обращение по сокету
		// пришедшим по TCP — то есть соврёт ровно в той записи, ради которой
		// и заводится.
		handler := markTransport(d.auditMutations(mux), privileged)
		srv := &http.Server{ReadHeaderTimeout: 5 * time.Second}
		if privileged {
			// Учётные данные спрашиваются один раз на соединение, а не на
			// запрос: они у соединения и не меняются, а системный вызов
			// на каждый запрос был бы платой ни за что.
			srv.ConnContext = withPeer
		} else {
			handler = guardBrowser(handler, localHosts(cfg.Daemon.HTTPAddr), d.logf)
		}
		srv.Handler = handler
		srvs = append(srvs, srv)
	}

	errCh := make(chan error, len(listeners))
	for i, ln := range listeners {
		go func(srv *http.Server, ln net.Listener) {
			if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- err
			}
		}(srvs[i], ln)
	}

	select {
	case <-ctx.Done():
	case err := <-errCh:
		d.logf("server error: %v", err)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var firstErr error
	for _, srv := range srvs {
		if err := srv.Shutdown(shutdownCtx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// listenUnix создаёт сокет, доступный на запись группе group,
// чтобы CLI работал без sudo.
func (d *Daemon) listenUnix(path, group string) (net.Listener, error) {
	// Ядро ограничивает длину пути юникс-сокета (104 байта на macOS),
	// а ошибка от connect() при превышении звучит как «invalid argument»
	// и совершенно не намекает на причину.
	const maxUnixPath = 103
	if len(path) > maxUnixPath {
		return nil, fmt.Errorf("socket path is longer than %d bytes (%d): %s", maxUnixPath, len(path), path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create the socket directory: %w", err)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("remove the stale socket %s: %w", path, err)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", path, err)
	}
	if group != "" {
		g, err := user.LookupGroup(group)
		if err != nil {
			d.logf("group %q not found, the socket stays root-only: %v", group, err)
		} else {
			gid, _ := strconv.Atoi(g.Gid)
			if err := os.Chown(path, 0, gid); err != nil {
				d.logf("socket chown: %v", err)
			}
		}
	}
	if err := os.Chmod(path, 0o660); err != nil {
		d.logf("socket chmod: %v", err)
	}
	return ln, nil
}

// transportKey помечает запрос как пришедший по управляющему сокету.
type transportKey struct{}

// markTransport проставляет признак привилегированного транспорта.
//
// Запись конфига разрешена только через unix-сокет: конфиг задаёт путь к
// sshuttle, который демон запускает от root, поэтому возможность переписать
// его по TCP была бы способом получить root любому локальному процессу.
// Права на сокет (root:staff, 0660) — и есть та граница, которой можно верить.
func markTransport(next http.Handler, privileged bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), transportKey{}, privileged)))
	})
}

// peerKey хранит учётные данные процесса на том конце управляющего сокета.
type peerKey struct{}

// peer — кто именно подключился к управляющему сокету.
type peer struct {
	UID, PID int
	Known    bool
}

// String даёт запись для журнала.
func (p peer) String() string {
	if !p.Known {
		return "unknown peer"
	}
	return fmt.Sprintf("uid=%d pid=%d", p.UID, p.PID)
}

// withPeer выясняет, кто открыл соединение, и кладёт ответ в контекст.
func withPeer(ctx context.Context, c net.Conn) context.Context {
	uid, pid, err := peerCred(c)
	if err != nil {
		return context.WithValue(ctx, peerKey{}, peer{})
	}
	return context.WithValue(ctx, peerKey{}, peer{UID: uid, PID: pid, Known: true})
}

// requestPeer достаёт учётные данные собеседника из контекста запроса.
func requestPeer(r *http.Request) peer {
	p, _ := r.Context().Value(peerKey{}).(peer)
	return p
}

// auditMutations записывает в журнал каждое изменяющее обращение вместе с тем,
// кто его сделал.
//
// ÐÐ°ÑÐ¸ÑÐ° снимается одним POST, и до сих пор в журнале оставалось только
// последствие — «правила выгружены», — но не автор. Разбирать потом, почему
// защита оказалась снята посреди ночи, было бы не по чему.
func (d *Daemon) auditMutations(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			who := "over tcp"
			if privilegedRequest(r) {
				who = "over the control socket, " + requestPeer(r).String()
			}
			d.logf("%s %s — %s", r.Method, r.URL.Path, who)
		}
		next.ServeHTTP(w, r)
	})
}

func privilegedRequest(r *http.Request) bool {
	privileged, _ := r.Context().Value(transportKey{}).(bool)
	return privileged
}

func (d *Daemon) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, d.Status())
	})
	mux.HandleFunc("GET /config", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, d.Config())
	})
	mux.HandleFunc("GET /rules", func(w http.ResponseWriter, r *http.Request) {
		rs, err := d.ruleset()
		if err != nil {
			writeErr(w, err)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write(rs.Rules())
	})
	mux.HandleFunc("POST /up", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Profile string `json:"profile"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if err := d.Up(req.Profile); err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, d.Status())
	})
	mux.HandleFunc("POST /down", func(w http.ResponseWriter, r *http.Request) {
		if err := d.Down(r.Context()); err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, d.Status())
	})
	// /killswitch оставлен синонимом ради приложения в строке меню,
	// которое переезжает на /protect отдельно.
	protectHandler := func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Mode   string `json:"mode"`   // on | off | strict
			Policy string `json:"policy"` // all | public | custom | off
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, fmt.Errorf("parse request: %w", err))
			return
		}
		if req.Policy != "" {
			if err := d.SetMode(config.ProtectionMode(strings.ToLower(req.Policy))); err != nil {
				writeErr(w, err)
				return
			}
			if req.Mode == "" {
				writeJSON(w, http.StatusOK, d.Status())
				return
			}
		}

		// Снятие panic и переключение enabled — две отдельные операции, и
		// раньше отказ первой молча оставлял вторую невыполненной.
		// Теперь ошибка любой из них сразу видна вызывающему.
		var err error
		switch strings.ToLower(req.Mode) {
		case "on":
			if err = d.SetStrict(false); err == nil {
				err = d.SetEnabled(true)
			}
		case "off":
			if err = d.SetStrict(false); err == nil {
				err = d.SetEnabled(false)
			}
		case "strict", "panic":
			err = d.SetStrict(true)
		default:
			err = fmt.Errorf("mode must be on|off|strict, got %q", req.Mode)
		}
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, d.Status())
	}
	mux.HandleFunc("POST /protect", protectHandler)
	mux.HandleFunc("POST /killswitch", protectHandler)
	mux.HandleFunc("POST /reload", func(w http.ResponseWriter, r *http.Request) {
		if err := d.Reload(); err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, d.Status())
	})
	mux.HandleFunc("POST /probe", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, d.Probe(r.Context()))
	})
	mux.HandleFunc("POST /config", func(w http.ResponseWriter, r *http.Request) {
		if !privilegedRequest(r) {
			http.Error(w, "writing the config is only allowed over the control socket", http.StatusForbidden)
			return
		}
		raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			writeErr(w, err)
			return
		}
		if err := d.WriteConfig(raw); err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, d.Status())
	})
	mux.HandleFunc("GET /update", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, d.UpdateState())
	})
	// Установка обновления — только через управляющий сокет, по той же причине,
	// что и запись конфига: это подмена кода, который launchd запустит от root.
	// По TCP отказ явный, чтобы не выглядело поломкой.
	mux.HandleFunc("POST /update", func(w http.ResponseWriter, r *http.Request) {
		if !privilegedRequest(r) {
			http.Error(w, "installing an update is only allowed over the control socket: it replaces the binary that runs as root", http.StatusForbidden)
			return
		}
		st, err := d.ApplyUpdate()
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"installed": st.Latest, "restarting": true})
		// Ответ выталкивается до выхода: иначе клиент увидит оборванное
		// соединение и не узнает, что обновление удалось.
		if err := http.NewResponseController(w).Flush(); err != nil {
			d.logf("could not flush the update response: %v", err)
		}
		d.restart()
	})
	mux.HandleFunc("GET /config/raw", func(w http.ResponseWriter, r *http.Request) {
		raw, err := os.ReadFile(d.configPath)
		if err != nil {
			writeErr(w, err)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write(raw)
	})
	mux.HandleFunc("GET /log", func(w http.ResponseWriter, r *http.Request) {
		lines := 200
		if v := r.URL.Query().Get("tail"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 5000 {
				lines = n
			}
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write(tailFile(d.Config().Daemon.LogFile, lines))
	})
	mux.HandleFunc("GET /blocked", d.streamBlocked)
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(indexHTML)
	})
	return mux
}

// ProbeTarget — исход одной попытки соединения.
type ProbeTarget struct {
	Address   string `json:"address"`
	Reachable bool   `json:"reachable"`
	Detail    string `json:"detail"`
}

// ProbeReport — результат проверки, что блокировка действительно работает.
//
// Одной попытки соединения мало: недостижимость адреса может означать и
// сработавшую блокировку, и просто отсутствие интернета. Поэтому сначала
// проверяется контрольный адрес заведомо вне блокируемых сетей.
type ProbeReport struct {
	Control      ProbeTarget   `json:"control"`
	Blocked      []ProbeTarget `json:"blocked"`
	Verdict      string        `json:"verdict"`
	Leaked       bool          `json:"leaked"`
	TunnelUp     bool          `json:"tunnel_up"`
	Inconclusive bool          `json:"inconclusive"`
}

// Probe проверяет блокировку: сравнивает достижимость контрольного адреса
// и адресов из списка блокировки.
func (d *Daemon) Probe(ctx context.Context) ProbeReport {
	cfg := d.Config()
	control := cfg.Daemon.ProbeControl
	if control == "" {
		control = config.Default().Daemon.ProbeControl
	}

	report := ProbeReport{Control: d.dialProbe(ctx, control)}
	if !report.Control.Reachable {
		report.Inconclusive = true
		report.Verdict = "control address " + control + " is unreachable — no network, nothing to verify"
		return report
	}

	rs, err := d.ruleset()
	if err != nil {
		report.Inconclusive = true
		report.Verdict = "could not build the protected route list: " + err.Error()
		return report
	}
	if len(rs.Block) == 0 {
		report.Verdict = "nothing to check: protection is off or the route list is empty"
		return report
	}

	anchors, _ := d.pf.SshuttleAnchors()
	report.TunnelUp = len(anchors) > 0

	for _, addr := range probeAddresses(rs.Block, 3) {
		res := d.dialProbe(ctx, addr)
		report.Blocked = append(report.Blocked, res)
		if res.Reachable {
			report.Leaked = true
		}
	}

	switch {
	case report.Leaked && report.TunnelUp:
		report.Verdict = "addresses answer, but the tunnel is up — expected, traffic goes through sshuttle"
	case report.Leaked:
		report.Verdict = "protection is not in effect: no tunnel, yet a protected address accepted a connection"
	default:
		report.Verdict = "protection works: control address is reachable, protected ones are not"
	}
	return report
}

// probeAddresses выбирает адреса для проверки, предпочитая одиночные хосты
// (/32): у них за адресом стоит настоящая машина, а не номер сети.
func probeAddresses(block []string, limit int) []string {
	var hosts, nets []string
	for _, cidr := range block {
		p, err := netip.ParsePrefix(strings.TrimSpace(cidr))
		if err != nil {
			continue
		}
		addr := net.JoinHostPort(p.Masked().Addr().String(), "443")
		if p.Bits() == p.Addr().BitLen() {
			hosts = append(hosts, addr)
		} else {
			nets = append(nets, addr)
		}
	}
	all := append(hosts, nets...)
	if len(all) > limit {
		all = all[:limit]
	}
	return all
}

func (d *Daemon) dialProbe(ctx context.Context, address string) ProbeTarget {
	dialCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	conn, err := d.dial(dialCtx, "tcp", address)
	if err != nil {
		return ProbeTarget{Address: address, Detail: err.Error()}
	}
	_ = conn.Close()
	return ProbeTarget{Address: address, Reachable: true, Detail: "connection established"}
}

// tailFile отдаёт последние lines строк файла, не читая его целиком.
func tailFile(path string, lines int) []byte {
	f, err := os.Open(path)
	if err != nil {
		return []byte("log unavailable: " + err.Error() + "\n")
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return []byte("log unavailable: " + err.Error() + "\n")
	}
	// С запасом: длинных строк в журнале не бывает, а лишнее отрежется ниже.
	const bytesPerLine = 200
	window := int64(lines * bytesPerLine)
	offset := info.Size() - window
	if offset < 0 {
		offset = 0
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return []byte("log unavailable: " + err.Error() + "\n")
	}

	raw, err := io.ReadAll(f)
	if err != nil {
		return []byte("log unavailable: " + err.Error() + "\n")
	}
	// Завершающий перевод строки не образует ещё одну строку — без этого
	// /log?tail=1 отдавал бы пустоту, а вообще всегда на строку меньше.
	text := strings.TrimSuffix(string(raw), "\n")
	all := strings.Split(text, "\n")
	if offset > 0 && len(all) > 1 {
		all = all[1:] // первая строка обрезана посередине
	}
	if len(all) > lines {
		all = all[len(all)-lines:]
	}
	return []byte(strings.Join(all, "\n") + "\n")
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
}
