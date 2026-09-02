//go:build darwin

package netmon

import (
	"context"
	"syscall"
)

// watchRoutes слушает маршрутный сокет ядра (PF_ROUTE) и превращает
// сообщения об изменениях в события монитора.
//
// Именно так узнаёт о смене сети сама macOS: добавление и удаление адресов,
// смена маршрута по умолчанию, подъём и падение интерфейса — всё приходит сюда
// в момент события, а не через секунды опроса. Сокет открывается без root:
// на чтение он доступен любому процессу, права нужны только на запись.
func watchRoutes(ctx context.Context, m *Monitor) {
	fd, err := syscall.Socket(syscall.AF_ROUTE, syscall.SOCK_RAW, syscall.AF_UNSPEC)
	if err != nil {
		m.logf("route socket unavailable, the watchdog stays on the ticker: %v", err)
		return
	}
	// Закрытие по отмене контекста живёт в отдельной горутине: читающий
	// syscall.Read блокируется в ядре и сам ctx.Done() не увидит.
	// Закрытие дескриптора выводит его из блокировки с ошибкой.
	go func() {
		<-ctx.Done()
		_ = syscall.Close(fd)
	}()

	buf := make([]byte, 4096)
	for {
		n, err := syscall.Read(fd, buf)
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
			}
			// ENOBUFS означает, что ядро не успело отдать нам поток сообщений
			// и часть потерялась. Сам факт переполнения — уже свидетельство
			// бурных изменений в сети, поэтому это тоже повод перепроверить.
			if err == syscall.ENOBUFS {
				m.emit(Event{Reason: "network event stream overflowed"})
				continue
			}
			if err == syscall.EINTR {
				continue
			}
			m.logf("route socket read stopped: %v", err)
			return
		}
		if reason, ok := interestingReason(buf[:n]); ok {
			m.emit(Event{Reason: reason})
		}
	}
}

// interestingReason разбирает пачку сообщений маршрутного сокета и возвращает
// причину для первого значимого.
//
// Разбирается только заголовок: длина и тип. Тела у rt_msghdr, if_msghdr и
// ifa_msghdr разные, но первые четыре байта у всех одинаковые, а содержимое
// сообщения нам не нужно — нужен сам факт «что-то в сети поменялось».
func interestingReason(buf []byte) (string, bool) {
	for len(buf) >= 4 {
		msglen := int(buf[0]) | int(buf[1])<<8
		// Мусорная длина означает, что дальше разбирать нечего: сдвинуться
		// на неизвестное число байт нельзя, а бесконечный цикл недопустим.
		if msglen < 4 || msglen > len(buf) {
			return "", false
		}
		version, msgType := buf[2], buf[3]
		if version == syscall.RTM_VERSION {
			if reason, ok := routeMessageReason(msgType); ok {
				return reason, true
			}
		}
		buf = buf[msglen:]
	}
	return "", false
}

// routeMessageReason переводит тип сообщения в причину.
//
// Отфильтровано намеренно: RTM_GET и RTM_MISS порождает любой процесс,
// спрашивающий маршрут, а RTM_NEWMADDR/RTM_DELMADDR — это multicast-группы,
// которые mDNS дёргает постоянно. Реагировать на них значило бы гонять сторожа
// вхолостую.
func routeMessageReason(msgType byte) (string, bool) {
	switch msgType {
	case syscall.RTM_ADD, syscall.RTM_DELETE, syscall.RTM_CHANGE, syscall.RTM_REDIRECT:
		return "routes changed", true
	case syscall.RTM_NEWADDR, syscall.RTM_DELADDR:
		return "interface addresses changed", true
	case syscall.RTM_IFINFO, syscall.RTM_IFINFO2:
		return "interface state changed", true
	case syscall.RTM_LOSING:
		return "route is losing connectivity", true
	default:
		return "", false
	}
}
