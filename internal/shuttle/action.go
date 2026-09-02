package shuttle

import (
	"strings"
	"unicode"
)

// tailscaleLoginPrefix — начало ссылки, которую печатает ssh, когда на
// хосте включён Tailscale SSH и политика тайнета требует перепройти вход.
//
// Привязываться к тексту вокруг ссылки нельзя: он у разных версий свой.
// Сама ссылка — и признак, и всё, что человеку нужно.
const tailscaleLoginPrefix = "https://login.tailscale.com/"

// ActionRequired — то, что человек должен сделать руками, чтобы туннель
// поднялся. Пустой Kind означает, что ничего не требуется.
//
// Без этого отказ выглядел как молчание: sshuttle завершался, туннель не
// поднимался, и в интерфейсе оставалось только «tunnel failed» — при том что
// ссылка на вход была напечатана и лежала в журнале.
type ActionRequired struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
	URL     string `json:"url"`
}

// Empty отвечает, требуется ли что-нибудь от человека.
func (a ActionRequired) Empty() bool { return a.Kind == "" }

// DetectAuthURL достаёт из строки вывода ссылку на повторный вход.
// Пустая строка означает, что строка обычная и делать ничего не нужно.
func DetectAuthURL(line string) string {
	idx := strings.Index(line, tailscaleLoginPrefix)
	if idx < 0 {
		return ""
	}
	url := line[idx:]
	// Ссылку часто печатают в предложении или в кавычках, поэтому режем её
	// по первому пробелу и снимаем то, что заведомо не может быть частью URL.
	if cut := strings.IndexFunc(url, unicode.IsSpace); cut >= 0 {
		url = url[:cut]
	}
	url = strings.TrimRight(url, `.,;:)]}"'>`)
	if url == tailscaleLoginPrefix {
		// Голый префикс без пути никуда не ведёт — показывать его человеку
		// бессмысленно, это не ссылка на вход.
		return ""
	}
	return url
}

// AuthAction описывает требование заново пройти аутентификацию.
func AuthAction(url string) ActionRequired {
	return ActionRequired{
		Kind:    "auth",
		Message: "the tunnel host requires re-authentication",
		URL:     url,
	}
}
