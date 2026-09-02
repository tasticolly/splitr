package update

import (
	"strconv"
	"strings"
)

// DevVersion — версия бинаря, собранного вне тега. Сравнивать её не с чем:
// неизвестно даже, из какого кода он собран.
const DevVersion = "dev"

// version — разобранная версия продукта.
//
// Разбирается не «чистый» semver, а то, что реально печатает бинарь: версия
// берётся из `git describe --tags --always --dirty`, поэтому кроме v1.2.3
// встречается v1.2.3-4-gabcdef (четыре коммита после тега) и суффикс -dirty.
// Обе формы означают «сборка новее тега», и путать их с предрелизом v1.2.3-rc1,
// который, наоборот, старше тега, нельзя: иначе продукт предлагал бы
// «обновиться» с более свежей сборки на старый тег.
type version struct {
	nums [3]int
	// ahead — сколько коммитов после тега (0, если сборка ровно на теге).
	ahead int
	// dirty — сборка из репозитория с незакоммиченными правками.
	dirty bool
	// pre — предрелизный суффикс (rc1, beta и подобное): он делает версию
	// СТАРШЕ соответствующего релиза.
	pre string
}

// parseVersion разбирает строку версии. ok=false означает, что сравнивать
// нечего: это dev, пустая строка или вообще не версия.
func parseVersion(s string) (version, bool) {
	s = strings.TrimSpace(s)
	if s == "" || s == DevVersion {
		return version{}, false
	}
	var v version
	if rest, found := strings.CutSuffix(s, "-dirty"); found {
		v.dirty = true
		s = rest
	}
	s = strings.TrimPrefix(s, "v")

	parts := strings.Split(s, "-")
	core := strings.Split(parts[0], ".")
	if len(core) == 0 || len(core) > 3 {
		return version{}, false
	}
	for i, p := range core {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return version{}, false
		}
		v.nums[i] = n
	}

	rest := parts[1:]
	// Хвост от git describe: «-<число коммитов>-g<хеш>».
	if len(rest) >= 2 && strings.HasPrefix(rest[1], "g") {
		if n, err := strconv.Atoi(rest[0]); err == nil {
			v.ahead = n
			rest = rest[2:]
		}
	}
	if len(rest) > 0 {
		v.pre = strings.Join(rest, "-")
	}
	return v, true
}

// compare сравнивает версии: -1, 0 или 1.
func compare(a, b version) int {
	for i := range a.nums {
		if a.nums[i] != b.nums[i] {
			if a.nums[i] < b.nums[i] {
				return -1
			}
			return 1
		}
	}
	if r := rank(a) - rank(b); r != 0 {
		if r < 0 {
			return -1
		}
		return 1
	}
	switch {
	case a.ahead < b.ahead:
		return -1
	case a.ahead > b.ahead:
		return 1
	}
	return strings.Compare(a.pre, b.pre)
}

// rank раскладывает версии с одинаковыми числами по порядку:
// предрелиз старше тега, тег старше сборки после него.
func rank(v version) int {
	switch {
	case v.pre != "":
		return -1
	case v.ahead > 0 || v.dirty:
		return 1
	default:
		return 0
	}
}

// Newer отвечает, есть ли смысл ставить latest поверх installed.
// Второе значение — false, если сравнить не удалось.
func Newer(latest, installed string) (bool, bool) {
	l, okL := parseVersion(latest)
	i, okI := parseVersion(installed)
	if !okL || !okI {
		return false, false
	}
	return compare(l, i) > 0, true
}
