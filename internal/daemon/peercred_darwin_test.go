//go:build darwin

package daemon

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

// Учётные данные обязаны читаться на настоящем сокете: раскладка struct xucred
// задана ядром, и ошибка в ней проявилась бы только здесь.
func TestPeerCredReadsRealSocket(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "peer.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("слушать %s: %v", path, err)
	}
	defer ln.Close()

	type result struct {
		uid, pid int
		err      error
	}
	got := make(chan result, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			got <- result{err: err}
			return
		}
		defer conn.Close()
		uid, pid, err := peerCred(conn)
		got <- result{uid, pid, err}
	}()

	client, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("подключиться: %v", err)
	}
	defer client.Close()

	res := <-got
	if res.err != nil {
		t.Fatalf("учётные данные не прочитались: %v", res.err)
	}
	if res.uid != os.Getuid() {
		t.Fatalf("uid = %d, ожидался %d — на обоих концах один и тот же процесс", res.uid, os.Getuid())
	}
	if res.pid != os.Getpid() {
		t.Fatalf("pid = %d, ожидался %d", res.pid, os.Getpid())
	}
}

// На чужом транспорте спрашивать некого, и это не должно приводить к панике.
func TestPeerCredRejectsNonUnixConn(t *testing.T) {
	t.Parallel()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("слушать tcp: %v", err)
	}
	defer ln.Close()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("подключиться: %v", err)
	}
	defer conn.Close()

	if _, _, err := peerCred(conn); err == nil {
		t.Fatal("для TCP учётных данных быть не может")
	}
}

// Неизвестный собеседник обязан оставаться распознаваемым в журнале.
func TestPeerStringDescribesUnknown(t *testing.T) {
	t.Parallel()

	if s := (peer{}).String(); s == "" {
		t.Fatal("запись о неизвестном собеседнике не должна быть пустой")
	}
	if s := (peer{UID: 501, PID: 42, Known: true}).String(); s != "uid=501 pid=42" {
		t.Fatalf("запись = %q", s)
	}
}
