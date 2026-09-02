//go:build !darwin

package daemon

import (
	"fmt"
	"net"
)

// peerCred вне Darwin не реализован.
//
// Linux отдаёт то же самое через SO_PEERCRED, но splitr там существует только
// внутри тестового стенда, где спрашивать некого: демон и клиент запускает один
// и тот же тест. Неизвестный собеседник журналируется как неизвестный, и это
// единственное последствие.
func peerCred(net.Conn) (uid, pid int, err error) {
	return 0, 0, fmt.Errorf("peer credentials are available on macOS only")
}
