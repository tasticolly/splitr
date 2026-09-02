package shuttle

import (
	"bytes"
	"io"
	"sync"
)

// maxLineBuffer ограничивает недописанную строку: sshuttle может вылить
// длинную бинарную кашу, и держать её целиком в памяти демона, живущего
// месяцами, незачем.
const maxLineBuffer = 64 << 10

// outputWatcher отдаёт вывод процесса в журнал как есть и попутно
// разбирает его построчно.
//
// Читать журнал повторно было бы неверно: он ротируется по размеру, пишется
// в несколько мест сразу, и требование войти по ссылке нужно заметить в тот
// момент, когда оно напечатано, а не когда кто-то откроет файл.
type outputWatcher struct {
	dst  io.Writer
	note func(line string)

	mu   sync.Mutex
	tail []byte
}

func (w *outputWatcher) Write(p []byte) (int, error) {
	w.mu.Lock()
	n, err := w.dst.Write(p)
	w.tail = append(w.tail, p...)

	var lines []string
	for {
		i := bytes.IndexByte(w.tail, '\n')
		if i < 0 {
			break
		}
		lines = append(lines, string(bytes.TrimRight(w.tail[:i], "\r")))
		w.tail = w.tail[i+1:]
	}
	if len(w.tail) > maxLineBuffer {
		w.tail = nil
	}
	w.mu.Unlock()

	// Разбор — вне замка: он ходит в Runner, у которого свой замок,
	// и брать их в разном порядке значило бы напрашиваться на клинч.
	for _, line := range lines {
		w.note(line)
	}
	return n, err
}

// flush разбирает остаток без завершающего перевода строки.
// Именно так ssh печатает ссылку на вход перед тем, как соединение оборвётся.
func (w *outputWatcher) flush() {
	w.mu.Lock()
	line := string(w.tail)
	w.tail = nil
	w.mu.Unlock()
	if line != "" {
		w.note(line)
	}
}
