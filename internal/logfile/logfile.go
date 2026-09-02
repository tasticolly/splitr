// Package logfile — файл журнала, который не растёт бесконечно.
//
// Демон работает от root, пишет в журнал каждое событие сторожа и живёт
// месяцами без перезапуска. Обычный os.OpenFile с O_APPEND в такой связке
// означает файл, который рано или поздно съест диск — а Ð·Ð°ÑÐ¸ÑÐ°, упавший
// из-за кончившегося места, это ровно тот отказ, ради предотвращения которого
// всё и затевалось.
//
// Штатный для macOS newsyslog здесь не годится: он переименовывает файл под
// уже открытым дескриптором, и без уговора о сигнале демон продолжает писать
// в отвязанный inode, то есть в никуда. Поэтому ротация своя, внутри процесса,
// где момент подмены известен точно.
package logfile

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Значения по умолчанию: журнал в несколько мегабайт полностью помещается
// в `splitr log`, а трёх поколений хватает, чтобы разобрать вчерашний сбой.
const (
	DefaultMaxBytes int64 = 8 << 20
	DefaultKeep           = 3
)

// Writer — io.WriteCloser поверх файла с ротацией по размеру.
type Writer struct {
	path     string
	maxBytes int64
	keep     int

	mu   sync.Mutex
	file *os.File
	size int64
}

// Open открывает журнал, дописывая в существующий файл.
// Неположительные maxBytes и keep заменяются умолчаниями.
func Open(path string, maxBytes int64, keep int) (*Writer, error) {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	if keep < 0 {
		keep = DefaultKeep
	}
	w := &Writer{path: path, maxBytes: maxBytes, keep: keep}
	if err := w.open(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *Writer) open() error {
	if err := os.MkdirAll(filepath.Dir(w.path), 0o755); err != nil {
		return fmt.Errorf("create the log directory: %w", err)
	}
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("open log %s: %w", w.path, err)
	}
	size := int64(0)
	if info, err := f.Stat(); err == nil {
		size = info.Size()
	}
	w.file, w.size = f, size
	return nil
}

// Write дописывает данные, при необходимости сменив поколение файла.
//
// Запись никогда не рвётся посередине: решение о ротации принимается до неё,
// поэтому строка журнала целиком уходит в один файл, а не разъезжается по двум.
func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.size > 0 && w.size+int64(len(p)) > w.maxBytes {
		if err := w.rotateLocked(); err != nil {
			// Неудачная ротация не повод терять запись: пишем в тот же файл
			// и позволяем ему временно превысить предел. Молчащий журнал
			// хуже слишком большого.
			fmt.Fprintf(w.file, "could not rotate the log: %v\n", err)
		}
	}

	n, err := w.file.Write(p)
	w.size += int64(n)
	return n, err
}

// rotateLocked сдвигает поколения и начинает файл заново.
func (w *Writer) rotateLocked() error {
	if err := w.file.Close(); err != nil {
		return err
	}
	// Сдвиг идёт от старших к младшим: иначе .1 затёр бы .2 ещё до переноса.
	if w.keep == 0 {
		if err := os.Remove(w.path); err != nil && !os.IsNotExist(err) {
			_ = w.open()
			return err
		}
		return w.open()
	}
	oldest := fmt.Sprintf("%s.%d", w.path, w.keep)
	if err := os.Remove(oldest); err != nil && !os.IsNotExist(err) {
		_ = w.open()
		return err
	}
	for i := w.keep - 1; i >= 1; i-- {
		from := fmt.Sprintf("%s.%d", w.path, i)
		to := fmt.Sprintf("%s.%d", w.path, i+1)
		if err := os.Rename(from, to); err != nil && !os.IsNotExist(err) {
			_ = w.open()
			return err
		}
	}
	if err := os.Rename(w.path, w.path+".1"); err != nil && !os.IsNotExist(err) {
		_ = w.open()
		return err
	}
	return w.open()
}

// Close закрывает текущий файл.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.file.Close()
}
