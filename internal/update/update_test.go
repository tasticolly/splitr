package update

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// newRepo создаёт настоящий git-репозиторий во временном каталоге.
// Подменять git здесь нечем и незачем: проверяется в том числе то, что мы
// зовём его правильно, а фальшивый git проверял бы только сам себя.
func newRepo(t *testing.T) string {
	t.Helper()
	if _, err := os.Stat(GitBinary); err != nil {
		t.Skipf("git недоступен: %v", err)
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command(GitBinary, append([]string{"-C", dir}, args...)...)
		// Настройки автора берутся из окружения: у машины, где идут тесты,
		// глобального user.email может не быть, и коммит бы не прошёл.
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
		}
	}
	run("init", "-q")
	if err := os.WriteFile(filepath.Join(dir, "file"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-q", "-m", "first")
	return dir
}

func tag(t *testing.T, dir, name, message string) {
	t.Helper()
	cmd := exec.Command(GitBinary, "-C", dir, "tag", "-a", name, "-m", message)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git tag: %v: %s", err, out)
	}
}

func TestStateReasons(t *testing.T) {
	t.Parallel()

	repo := newRepo(t)
	tag(t, repo, "v0.2.0", "protection survives a restart")

	cases := []struct {
		name       string
		checker    Checker
		repo       string
		installed  string
		available  bool
		wantReason string // подстрока
	}{
		{
			name:       "repo_path не задан",
			checker:    NewChecker(),
			repo:       "",
			installed:  "v0.1.0",
			wantReason: "update.repo_path is not set",
		},
		{
			name:       "каталога нет",
			checker:    NewChecker(),
			repo:       filepath.Join(t.TempDir(), "missing"),
			installed:  "v0.1.0",
			wantReason: "not an available directory",
		},
		{
			name:       "каталог не репозиторий",
			checker:    NewChecker(),
			repo:       t.TempDir(),
			installed:  "v0.1.0",
			wantReason: "is not a git repository",
		},
		{
			name:       "git недоступен",
			checker:    Checker{Git: filepath.Join(t.TempDir(), "no-git")},
			repo:       repo,
			installed:  "v0.1.0",
			wantReason: "git is not available",
		},
		{
			name:       "установлена dev",
			checker:    NewChecker(),
			repo:       repo,
			installed:  DevVersion,
			wantReason: "reports version dev",
		},
		{
			name:       "установлено не число",
			checker:    NewChecker(),
			repo:       repo,
			installed:  "custom-build",
			wantReason: "is not a version number",
		},
		{
			name:       "уже последняя",
			checker:    NewChecker(),
			repo:       repo,
			installed:  "v0.2.0",
			wantReason: "up to date",
		},
		{
			name:       "установлено новее тега",
			checker:    NewChecker(),
			repo:       repo,
			installed:  "v0.3.0",
			wantReason: "not newer than the installed",
		},
		{
			name:      "есть новая версия",
			checker:   NewChecker(),
			repo:      repo,
			installed: "v0.1.0",
			available: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			st := tc.checker.State(tc.repo, tc.installed)
			if st.Available != tc.available {
				t.Fatalf("available = %v, ожидалось %v (state %+v)", st.Available, tc.available, st)
			}
			if tc.available {
				if st.Reason != "" {
					t.Fatalf("reason = %q, при доступном обновлении он должен быть пуст", st.Reason)
				}
				if st.Latest != "v0.2.0" {
					t.Fatalf("latest = %q, ожидался v0.2.0", st.Latest)
				}
				if st.Notes != "protection survives a restart" {
					t.Fatalf("notes = %q, ожидалась аннотация тега", st.Notes)
				}
				return
			}
			if !strings.Contains(st.Reason, tc.wantReason) {
				t.Fatalf("reason = %q, ожидалось вхождение %q", st.Reason, tc.wantReason)
			}
			if st.Installed != tc.installed {
				t.Fatalf("installed = %q, ожидалось %q", st.Installed, tc.installed)
			}
		})
	}
}

func TestStateWithoutTags(t *testing.T) {
	t.Parallel()

	st := NewChecker().State(newRepo(t), "v0.1.0")
	if st.Available || !strings.Contains(st.Reason, "no version tags") {
		t.Fatalf("state = %+v, ожидался отказ из-за отсутствия тегов", st)
	}
}

func TestStateWithNonVersionTag(t *testing.T) {
	t.Parallel()

	repo := newRepo(t)
	tag(t, repo, "release-latest", "not a version")

	st := NewChecker().State(repo, "v0.1.0")
	if st.Available || !strings.Contains(st.Reason, "does not look like a version") {
		t.Fatalf("state = %+v, ожидался отказ из-за мусорного тега", st)
	}
	if st.Latest != "release-latest" {
		t.Fatalf("latest = %q: тег всё равно нужно показать человеку", st.Latest)
	}
}

// Демон работает от root, а репозиторий принадлежит человеку. Git с 2.35
// отказывается работать с чужим репозиторием и возвращает ошибку, а снаружи
// это выглядело как «тегов нет» — кнопка обновления не появлялась вообще,
// и заметил это человек, а не тесты.
func TestGitCallIncludesSafeDirectory(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	log := filepath.Join(dir, "args.txt")
	fake := filepath.Join(dir, "git")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + log + "\necho v1.0.0\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatalf("создать поддельный git: %v", err)
	}

	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatalf("создать .git: %v", err)
	}

	c := Checker{Git: fake}
	_ = c.State(repo, "v0.9.0")

	raw, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("поддельный git не был вызван: %v", err)
	}
	args := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(args) < 2 || args[0] != "-c" || args[1] != "safe.directory="+repo {
		t.Fatalf("git вызван без safe.directory: %v", args)
	}
}
