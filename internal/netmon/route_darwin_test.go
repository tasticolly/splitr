//go:build darwin

package netmon

import (
	"context"
	"syscall"
	"testing"
	"time"
)

// message собирает заголовок сообщения маршрутного сокета: длина, версия, тип.
// Тело не заполняется — разбирается только заголовок.
func message(msglen int, msgType byte) []byte {
	buf := make([]byte, msglen)
	buf[0] = byte(msglen)
	buf[1] = byte(msglen >> 8)
	buf[2] = syscall.RTM_VERSION
	buf[3] = msgType
	return buf
}

func TestInterestingReason(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		msgType byte
		want    bool
	}{
		{"новый адрес интерфейса", syscall.RTM_NEWADDR, true},
		{"удалён адрес интерфейса", syscall.RTM_DELADDR, true},
		{"состояние интерфейса", syscall.RTM_IFINFO, true},
		{"состояние интерфейса v2", syscall.RTM_IFINFO2, true},
		{"добавлен маршрут", syscall.RTM_ADD, true},
		{"удалён маршрут", syscall.RTM_DELETE, true},
		{"изменён маршрут", syscall.RTM_CHANGE, true},
		{"потеря связности", syscall.RTM_LOSING, true},
		// Шум: эти сообщения порождает обычный трафик, а не смена сети.
		{"запрос маршрута", syscall.RTM_GET, false},
		{"промах маршрутизации", syscall.RTM_MISS, false},
		{"multicast-группа добавлена", syscall.RTM_NEWMADDR, false},
		{"multicast-группа убрана", syscall.RTM_DELMADDR, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, got := interestingReason(message(32, tt.msgType))
			if got != tt.want {
				t.Fatalf("значимость = %v, ожидалась %v", got, tt.want)
			}
		})
	}
}

// Ядро отдаёт сообщения пачкой, и значимое может стоять не первым.
func TestInterestingReasonWalksBatch(t *testing.T) {
	t.Parallel()

	var batch []byte
	batch = append(batch, message(24, syscall.RTM_MISS)...)
	batch = append(batch, message(24, syscall.RTM_GET)...)
	batch = append(batch, message(24, syscall.RTM_NEWADDR)...)

	if _, ok := interestingReason(batch); !ok {
		t.Fatal("значимое сообщение в конце пачки потеряно")
	}
}

// Сообщение чужой версии разбирать нельзя: раскладка полей у него другая.
func TestInterestingReasonIgnoresForeignVersion(t *testing.T) {
	t.Parallel()

	msg := message(24, syscall.RTM_NEWADDR)
	msg[2] = syscall.RTM_VERSION + 1
	if _, ok := interestingReason(msg); ok {
		t.Fatal("сообщение неизвестной версии принято за значимое")
	}
}

// Кривая длина не должна ни зациклить разбор, ни увести за границу буфера.
func TestInterestingReasonSurvivesBrokenLength(t *testing.T) {
	t.Parallel()

	for _, msglen := range []int{0, 1, 3, 1 << 15} {
		buf := make([]byte, 24)
		buf[0] = byte(msglen)
		buf[1] = byte(msglen >> 8)
		buf[2] = syscall.RTM_VERSION
		buf[3] = syscall.RTM_NEWADDR
		if _, ok := interestingReason(buf); ok {
			t.Fatalf("сообщение с длиной %d принято за значимое", msglen)
		}
	}
}

// Маршрутный сокет обязан открываться без root: иначе весь механизм
// работал бы только у демона и молчал бы при разработке.
func TestWatchRoutesOpensSocket(t *testing.T) {
	t.Parallel()

	fd, err := syscall.Socket(syscall.AF_ROUTE, syscall.SOCK_RAW, syscall.AF_UNSPEC)
	if err != nil {
		t.Skipf("маршрутный сокет недоступен в этой среде: %v", err)
	}
	_ = syscall.Close(fd)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		watchRoutes(ctx, newTestMonitor(time.Second))
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("чтение маршрутного сокета не остановилось по отмене контекста")
	}
}
