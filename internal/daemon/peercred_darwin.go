//go:build darwin

package daemon

import (
	"fmt"
	"net"
	"syscall"
	"unsafe"
)

// Константы из <sys/un.h>: уровень сокет-опций локального домена и два запроса
// к ядру — учётные данные и pid процесса на том конце.
const (
	solLocal      = 0
	localPeerCred = 0x001
	localPeerPID  = 0x002
)

// xucred — раскладка struct xucred из <sys/ucred.h>.
// Поля читаются ядром напрямую, поэтому порядок и размеры менять нельзя.
type xucred struct {
	Version uint32
	UID     uint32
	Ngroups int16
	Groups  [16]uint32
}

// peerCred спрашивает у ядра, кто именно подключился к управляющему сокету.
//
// Права на файл сокета (root:staff, 0660) отвечают лишь на вопрос «пустить ли»,
// и после установления соединения от них не остаётся следа. Ядро же помнит
// учётные данные подключившегося процесса и отдаёт их по LOCAL_PEERCRED —
// подделать их нельзя, в отличие от чего угодно, что клиент напишет сам.
func peerCred(c net.Conn) (uid, pid int, err error) {
	unixConn, ok := c.(*net.UnixConn)
	if !ok {
		return 0, 0, fmt.Errorf("the connection is not a unix socket")
	}
	raw, err := unixConn.SyscallConn()
	if err != nil {
		return 0, 0, err
	}

	// Работа с сырым дескриптором обязана идти внутри Control: иначе сокет
	// может закрыться прямо между получением fd и системным вызовом, а номер
	// достаться уже другому файлу.
	var innerErr error
	err = raw.Control(func(fd uintptr) {
		var cred xucred
		size := uint32(unsafe.Sizeof(cred))
		if _, _, e := syscall.Syscall6(syscall.SYS_GETSOCKOPT, fd, solLocal, localPeerCred,
			uintptr(unsafe.Pointer(&cred)), uintptr(unsafe.Pointer(&size)), 0); e != 0 {
			innerErr = fmt.Errorf("LOCAL_PEERCRED: %w", e)
			return
		}
		uid = int(cred.UID)

		// pid отдаётся отдельной опцией: в struct xucred его нет.
		// Его отсутствие не повод считать вызов неудачным — он нужен только
		// для человекочитаемой записи в журнале.
		var peerPID uint32
		pidSize := uint32(unsafe.Sizeof(peerPID))
		if _, _, e := syscall.Syscall6(syscall.SYS_GETSOCKOPT, fd, solLocal, localPeerPID,
			uintptr(unsafe.Pointer(&peerPID)), uintptr(unsafe.Pointer(&pidSize)), 0); e == 0 {
			pid = int(peerPID)
		}
	})
	if err != nil {
		return 0, 0, err
	}
	return uid, pid, innerErr
}
