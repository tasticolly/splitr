// Package update ищет новую версию SplitR в локальном git-репозитории.
//
// Удалённого репозитория у продукта нет: версии живут в тегах того клона,
// из которого он и собирается. Поэтому «проверить обновление» — это спросить
// git про последний тег, а не сходить в сеть.
package update

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// GitBinary — git зовётся по абсолютному пути: демон работает от root,
// и брать исполняемый файл из PATH здесь нельзя.
const GitBinary = "/usr/bin/git"

// State — состояние обновления, одинаковое для GET /update, поля update
// в статусе и вывода CLI.
type State struct {
	Installed string `json:"installed"`
	Latest    string `json:"latest"`
	Available bool   `json:"available"`
	RepoPath  string `json:"repo_path"`
	Notes     string `json:"notes"`
	// Reason объясняет по-человечески, почему обновиться нельзя.
	// Пуст ровно тогда, когда Available истинно.
	Reason string `json:"reason"`
}

// Checker спрашивает git о версиях в репозитории.
type Checker struct {
	// Git — путь к исполняемому файлу git; пустой означает GitBinary.
	Git string
	// Timeout ограничивает вызов git: демон не должен зависнуть на статусе
	// из-за репозитория на отвалившемся сетевом диске.
	Timeout time.Duration
}

// NewChecker собирает проверку с боевыми значениями.
func NewChecker() Checker { return Checker{Git: GitBinary, Timeout: 10 * time.Second} }

// State собирает состояние обновления для установленной версии installed.
//
// Ошибка не возвращается намеренно: недоступный репозиторий, отсутствие тегов
// и сборка без версии — это не поломка, а обычные состояния, о которых нужно
// внятно рассказать человеку в интерфейсе.
func (c Checker) State(repoPath, installed string) State {
	st := State{Installed: installed, RepoPath: repoPath}

	repoPath = strings.TrimSpace(repoPath)
	if repoPath == "" {
		st.Reason = "update.repo_path is not set in the config, so there is nowhere to look for new versions"
		return st
	}
	if info, err := os.Stat(repoPath); err != nil || !info.IsDir() {
		st.Reason = fmt.Sprintf("%s is not an available directory", repoPath)
		return st
	}
	if _, err := os.Stat(filepath.Join(repoPath, ".git")); err != nil {
		st.Reason = fmt.Sprintf("%s is not a git repository", repoPath)
		return st
	}

	git := c.Git
	if git == "" {
		git = GitBinary
	}
	if _, err := os.Stat(git); err != nil {
		st.Reason = fmt.Sprintf("git is not available at %s", git)
		return st
	}

	tag, err := c.run(git, repoPath, "describe", "--tags", "--abbrev=0")
	if err != nil || tag == "" {
		st.Reason = fmt.Sprintf("%s has no version tags to update to", repoPath)
		return st
	}
	st.Latest = tag
	// Аннотация тега — то, что человек увидит в меню как «что изменилось».
	// Её отсутствие ничего не ломает, поэтому ошибка здесь игнорируется.
	st.Notes, _ = c.run(git, repoPath, "tag", "-l", "--format=%(contents:subject)", tag)

	if _, ok := parseVersion(tag); !ok {
		st.Reason = fmt.Sprintf("tag %s does not look like a version (expected v1.2.3)", tag)
		return st
	}
	if _, ok := parseVersion(installed); !ok {
		if strings.TrimSpace(installed) == DevVersion || installed == "" {
			st.Reason = "this build reports version dev, so there is nothing to compare the tags against"
		} else {
			st.Reason = fmt.Sprintf("installed version %q is not a version number, so there is nothing to compare %s against", installed, tag)
		}
		return st
	}

	newer, _ := Newer(tag, installed)
	switch {
	case newer:
		st.Available = true
	case tag == installed:
		st.Reason = fmt.Sprintf("SplitR is up to date (%s)", installed)
	default:
		st.Reason = fmt.Sprintf("the latest tag %s is not newer than the installed %s", tag, installed)
	}
	return st
}

func (c Checker) run(git, repo string, args ...string) (string, error) {
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// -c safe.directory нужен потому, что демон работает от root, а репозиторий
	// принадлежит человеку. Git с версии 2.35 отказывается работать с чужим
	// репозиторием («detected dubious ownership») и молча возвращает ошибку —
	// снаружи это выглядело как «тегов нет», хотя теги на месте.
	// Флаг задаётся на один вызов, глобальный конфиг root не трогаем.
	full := append([]string{"-c", "safe.directory=" + repo, "-C", repo}, args...)
	out, err := exec.CommandContext(ctx, git, full...).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
