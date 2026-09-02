package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ensureUpdateSource записывает в конфиг путь к репозиторию, из которого
// прошла установка.
//
// Без этого кнопка обновления в строке меню не появится никогда: демон не
// может угадать, где лежат исходники, а конфиг, созданный прошлой версией,
// про секцию update ничего не знает. Дописываем только если ключа нет —
// осознанно пустое значение остаётся пустым.
func ensureUpdateSource(configPath string) error {
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return err
	}
	if strings.Contains(string(raw), "repo_path:") {
		return nil
	}

	repo, err := repoRoot()
	if err != nil {
		return err
	}

	block := fmt.Sprintf("\n# Where new versions come from: the checkout this build was installed from.\nupdate:\n  repo_path: %s\n", repo)
	if err := os.WriteFile(configPath, append(raw, []byte(block)...), 0o644); err != nil {
		return err
	}
	fmt.Println("update source recorded:", repo)
	return nil
}

// repoRoot определяет каталог исходников по расположению запущенного бинаря:
// установка всегда идёт из <repo>/bin/splitr.
// executablePath — шов для тестов: подменить путь запущенного бинаря иначе никак.
var executablePath = os.Executable

func repoRoot() (string, error) {
	self, err := executablePath()
	if err != nil {
		return "", err
	}
	if self, err = filepath.EvalSymlinks(self); err != nil {
		return "", err
	}

	if filepath.Base(filepath.Dir(self)) != "bin" {
		return "", fmt.Errorf("%s is not a repository build (expected <repo>/bin/splitr)", self)
	}
	repo := filepath.Dir(filepath.Dir(self))
	if _, err := os.Stat(filepath.Join(repo, "go.mod")); err != nil {
		return "", fmt.Errorf("no go.mod next to %s", repo)
	}
	return repo, nil
}
