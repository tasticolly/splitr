package logfile

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func logPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "splitr.log")
}

func read(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("прочитать %s: %v", path, err)
	}
	return string(raw)
}

func TestWriteAppends(t *testing.T) {
	t.Parallel()

	path := logPath(t)
	w, err := Open(path, 0, 0)
	if err != nil {
		t.Fatalf("открыть: %v", err)
	}
	defer w.Close()

	fmt.Fprintln(w, "первая")
	fmt.Fprintln(w, "вторая")

	if got := read(t, path); got != "первая\nвторая\n" {
		t.Fatalf("журнал = %q", got)
	}
}

// Перезапуск демона не должен терять прежний журнал.
func TestOpenKeepsExistingContent(t *testing.T) {
	t.Parallel()

	path := logPath(t)
	if err := os.WriteFile(path, []byte("прошлый запуск\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	w, err := Open(path, 0, 0)
	if err != nil {
		t.Fatalf("открыть: %v", err)
	}
	defer w.Close()
	fmt.Fprintln(w, "новый запуск")

	if got := read(t, path); !strings.HasPrefix(got, "прошлый запуск\n") {
		t.Fatalf("журнал = %q, прежние записи потеряны", got)
	}
}

// Журнал демона, работающего месяцами, обязан упираться в потолок.
func TestRotatesOnSizeLimit(t *testing.T) {
	t.Parallel()

	path := logPath(t)
	w, err := Open(path, 32, 2)
	if err != nil {
		t.Fatalf("открыть: %v", err)
	}
	defer w.Close()

	for i := range 10 {
		fmt.Fprintf(w, "строка журнала номер %d\n", i)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Size() > 64 {
		t.Fatalf("текущий файл разросся до %d байт при пределе 32", info.Size())
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("прошлое поколение журнала не сохранено: %v", err)
	}
}

// Поколений не должно становиться больше, чем разрешено: иначе диск съедят
// не одним файлом, а сотней.
func TestKeepsLimitedGenerations(t *testing.T) {
	t.Parallel()

	path := logPath(t)
	w, err := Open(path, 16, 2)
	if err != nil {
		t.Fatalf("открыть: %v", err)
	}
	defer w.Close()

	for i := range 40 {
		fmt.Fprintf(w, "запись %d\n", i)
	}

	if _, err := os.Stat(path + ".3"); !os.IsNotExist(err) {
		t.Fatalf("поколение сверх лимита не удалено: %v", err)
	}
	for _, suffix := range []string{"", ".1", ".2"} {
		if _, err := os.Stat(path + suffix); err != nil {
			t.Fatalf("поколение %q обязано существовать: %v", suffix, err)
		}
	}
}

// keep=0 означает «прошлое не хранить»: файл просто начинается заново.
func TestKeepZeroDropsHistory(t *testing.T) {
	t.Parallel()

	path := logPath(t)
	w, err := Open(path, 16, 0)
	if err != nil {
		t.Fatalf("открыть: %v", err)
	}
	defer w.Close()
	// keep=0 в Open заменяется умолчанием только для отрицательных значений,
	// поэтому здесь проверяется именно поведение «истории нет».
	w.keep = 0

	for i := range 20 {
		fmt.Fprintf(w, "запись %d\n", i)
	}
	if _, err := os.Stat(path + ".1"); !os.IsNotExist(err) {
		t.Fatalf("при keep=0 старых поколений быть не должно: %v", err)
	}
}

// Записи приходят из горутины сторожа и из обработчиков API одновременно.
func TestWriteIsConcurrencySafe(t *testing.T) {
	t.Parallel()

	path := logPath(t)
	w, err := Open(path, 128, 2)
	if err != nil {
		t.Fatalf("открыть: %v", err)
	}
	defer w.Close()

	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range 20 {
				fmt.Fprintf(w, "горутина %d запись %d\n", i, j)
			}
		}()
	}
	wg.Wait()
}

// Строка журнала обязана попасть в файл целиком, а не разъехаться по двум
// поколениям на границе ротации.
func TestRotationDoesNotSplitRecord(t *testing.T) {
	t.Parallel()

	path := logPath(t)
	w, err := Open(path, 20, 2)
	if err != nil {
		t.Fatalf("открыть: %v", err)
	}
	defer w.Close()

	fmt.Fprintln(w, "первая строка целиком")
	fmt.Fprintln(w, "вторая строка целиком")

	for _, p := range []string{path, path + ".1"} {
		for _, line := range strings.Split(strings.TrimSuffix(read(t, p), "\n"), "\n") {
			if line != "" && !strings.HasSuffix(line, "целиком") {
				t.Fatalf("в %s строка обрезана: %q", p, line)
			}
		}
	}
}

// Недоступный путь обязан приводить к внятной ошибке, а не к молчащему демону.
func TestOpenFailsOnUnwritablePath(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	blocker := filepath.Join(dir, "занято")
	if err := os.WriteFile(blocker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(filepath.Join(blocker, "splitr.log"), 0, 0); err == nil {
		t.Fatal("ожидалась ошибка открытия журнала внутри файла")
	}
}
