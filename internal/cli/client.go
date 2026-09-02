// Package cli реализует команды splitr.
package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// Client — клиент управляющего API демона поверх unix-сокета.
type Client struct {
	http *http.Client
}

// NewClient создаёт клиента, ходящего в демон через сокет.
func NewClient(socketPath string) *Client {
	return &Client{http: &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
			},
		},
	}}
}

// Get выполняет GET и возвращает тело ответа.
func (c *Client) Get(path string) ([]byte, error) { return c.do(http.MethodGet, path, nil) }

// Post выполняет POST с JSON-телом.
func (c *Client) Post(path string, body any) ([]byte, error) {
	var buf []byte
	if body != nil {
		var err error
		if buf, err = json.Marshal(body); err != nil {
			return nil, err
		}
	}
	return c.do(http.MethodPost, path, buf)
}

// Stream читает server-sent events построчно и отдаёт полезную нагрузку в onLine.
// Используется для живого потока заблокированных пакетов.
func (c *Client) Stream(ctx context.Context, path string, onLine func(string)) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://unix"+path, nil)
	if err != nil {
		return err
	}
	// Поток бесконечный, поэтому общий таймаут клиента здесь не годится.
	streaming := &http.Client{Transport: c.http.Transport}
	resp, err := streaming.Do(req)
	if err != nil {
		return fmt.Errorf("daemon unavailable: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s", strings.TrimSpace(string(msg)))
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if data, ok := strings.CutPrefix(line, "data: "); ok {
			onLine(data)
		}
	}
	return scanner.Err()
}

// PostRaw отправляет тело как есть — им пользуется загрузка конфига.
func (c *Client) PostRaw(path string, body []byte) ([]byte, error) {
	return c.do(http.MethodPost, path, body)
}

func (c *Client) do(method, path string, body []byte) ([]byte, error) {
	req, err := http.NewRequest(method, "http://unix"+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("daemon unavailable (%w); start it: sudo launchctl kickstart -k system/com.splitr.daemon", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			return nil, fmt.Errorf("%s", e.Error)
		}
		return nil, fmt.Errorf("%s %s: %s", method, path, resp.Status)
	}
	return data, nil
}
