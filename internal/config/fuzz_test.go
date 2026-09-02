package config

import (
	"os"
	"path/filepath"
	"testing"
)

// FuzzLoad проверяет, что разбор конфига держит удар.
//
// Конфиг читает процесс, работающий от root, и из него берётся путь к
// sshuttle, который этот процесс запускает. Паника на кривом файле означала бы
// мёртвый демон и снятую защиту, а конфиг, прошедший проверку с мусором внутри,
// означал бы правила pf, собранные из этого мусора. Поэтому свойство ровно
// одно: Load либо возвращает ошибку, либо отдаёт конфиг, проходящий Validate.
func FuzzLoad(f *testing.F) {
	f.Add("profiles:\n  visa:\n    remote: user@host\n")
	f.Add("default_profile: visa\nsubnets: [10.0.0.0/8]\nprofiles:\n  visa:\n    remote: u@h\n")
	f.Add("protection:\n  mode: custom\n  block: []\nprofiles:\n  a:\n    remote: h\n")
	f.Add("subnets: [не-сеть]\nprofiles:\n  a:\n    remote: h\n")
	f.Add("")
	f.Add("\x00\x00\x00")
	f.Add("profiles: не-карта")

	dir := f.TempDir()
	f.Fuzz(func(t *testing.T, body string) {
		path := filepath.Join(dir, "config.yaml")
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Skip()
		}

		cfg, err := Load(path)
		if err != nil {
			return
		}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("Load принял конфиг, который не проходит Validate: %v\nисходник: %q", err, body)
		}
		// Профиль по умолчанию обязан существовать: без него демон не сможет
		// ни поднять туннель, ни собрать правила под него.
		if _, _, err := cfg.Profile(""); err != nil && cfg.DefaultProfile != "" {
			t.Fatalf("профиль по умолчанию недостижим: %v\nисходник: %q", err, body)
		}
	})
}
