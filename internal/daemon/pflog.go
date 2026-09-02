package daemon

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
)

// PflogInterface — интерфейс, в который pf пишет отброшенные пакеты.
const PflogInterface = "pflog0"

// ensurePflog поднимает интерфейс журналирования pf.
// Без него ключевое слово log в правилах не приводит ни к чему:
// правила грузятся, но записи некуда девать.
func (d *Daemon) ensurePflog() error {
	if !d.Config().Protection.Log {
		return nil
	}
	// create на существующем интерфейсе возвращает ошибку — это нормально,
	// поэтому смотрим на итог, а не на код возврата.
	_ = exec.Command("/sbin/ifconfig", PflogInterface, "create").Run()
	if err := exec.Command("/sbin/ifconfig", PflogInterface, "up").Run(); err != nil {
		return fmt.Errorf("bring %s up: %w", PflogInterface, err)
	}
	return nil
}

// blockedTcpdump собирает команду чтения журнала pf.
// Ключ -l включает построчную буферизацию: без него вывод пришёл бы
// пачкой через минуту, и «живой поток» перестал бы быть живым.
func blockedTcpdump(ctx context.Context) *exec.Cmd {
	return exec.CommandContext(ctx, "/usr/sbin/tcpdump",
		"-n", "-e", "-l", "-t", "-i", PflogInterface, "action", "drop")
}

// streamBlocked отдаёт поток отброшенных пакетов как server-sent events.
// Так и веб-интерфейс, и menu bar, и CLI видят одно и то же без прав root.
func (d *Daemon) streamBlocked(w http.ResponseWriter, r *http.Request) {
	if !d.Config().Protection.Log {
		http.Error(w, "drop logging is off: set protection.log in the config and run reload", http.StatusPreconditionFailed)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming is not supported", http.StatusInternalServerError)
		return
	}
	if err := d.pflogUp(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	cmd := d.blockedCmd(ctx)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		fmt.Fprintf(w, "event: error\ndata: %s\n\n", err)
		return
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(w, "event: error\ndata: could not start tcpdump: %s\n\n", err)
		flusher.Flush()
		return
	}
	defer func() { _ = cmd.Wait() }()

	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", line); err != nil {
			return
		}
		flusher.Flush()
	}
}
