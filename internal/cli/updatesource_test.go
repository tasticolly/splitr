package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Автозапись пути к исходникам однажды молча потерялась при рефакторинге,
// и кнопка обновления перестала появляться вообще — при этом всё собиралось,
// все тесты были зелёными, и заметил это человек, а не мы. Тест закрывает
// ровно эту дыру.
func TestEnsureUpdateSourceAppendsRepoPath(t *testing.T) {
	t.Parallel()

	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatalf("создать go.mod: %v", err)
	}
	binDir := filepath.Join(repo, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("создать bin: %v", err)
	}
	// Файл обязан существовать: путь разрешается через симлинки.
	if err := os.WriteFile(filepath.Join(binDir, "splitr"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("создать бинарь: %v", err)
	}
	// В macOS временный каталог лежит за симлинком /var -> /private/var,
	// а записывается в конфиг уже разрешённый путь.
	resolvedRepo, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatalf("разрешить путь: %v", err)
	}

	cfg := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfg, []byte("default_profile: pc\n"), 0o644); err != nil {
		t.Fatalf("создать конфиг: %v", err)
	}

	// repoRoot смотрит на путь запущенного бинаря, поэтому подменяем его.
	restore := executablePath
	executablePath = func() (string, error) { return filepath.Join(binDir, "splitr"), nil }
	defer func() { executablePath = restore }()

	if err := ensureUpdateSource(cfg); err != nil {
		t.Fatalf("ensureUpdateSource: %v", err)
	}

	raw, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatalf("прочитать конфиг: %v", err)
	}
	if !strings.Contains(string(raw), "repo_path: "+resolvedRepo) {
		t.Fatalf("путь к исходникам не записан:\n%s", raw)
	}
	if !strings.Contains(string(raw), "update:") {
		t.Fatalf("секция update не создана:\n%s", raw)
	}
}

// Осознанно пустое значение — это выбор человека, его перезаписывать нельзя.
func TestEnsureUpdateSourceKeepsExistingKey(t *testing.T) {
	t.Parallel()

	cfg := filepath.Join(t.TempDir(), "config.yaml")
	original := "update:\n  repo_path: \"\"\n"
	if err := os.WriteFile(cfg, []byte(original), 0o644); err != nil {
		t.Fatalf("создать конфиг: %v", err)
	}

	if err := ensureUpdateSource(cfg); err != nil {
		t.Fatalf("ensureUpdateSource: %v", err)
	}

	raw, _ := os.ReadFile(cfg)
	if string(raw) != original {
		t.Fatalf("конфиг изменён, хотя ключ уже был:\n%s", raw)
	}
}

// Установка не из репозитория — не повод падать: просто нечего записать.
func TestRepoRootRejectsNonRepositoryBuild(t *testing.T) {
	t.Parallel()

	restore := executablePath
	executablePath = func() (string, error) { return "/usr/local/bin/splitr", nil }
	defer func() { executablePath = restore }()

	if _, err := repoRoot(); err == nil {
		t.Fatal("установленный бинарь не лежит в <repo>/bin, ожидалась ошибка")
	}
}
