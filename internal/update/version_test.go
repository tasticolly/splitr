package update

import "testing"

func TestParseVersionRejectsGarbage(t *testing.T) {
	t.Parallel()

	for _, s := range []string{"", "dev", "unknown", "v", "vx.y.z", "1.2.3.4", "10.x", "-", "v-1.0.0"} {
		if v, ok := parseVersion(s); ok {
			t.Fatalf("%q разобралось как %+v, ожидался отказ", s, v)
		}
	}
}

func TestNewer(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name              string
		latest, installed string
		newer, comparable bool
	}{
		{"новый тег", "v0.2.0", "v0.1.0", true, true},
		{"минорная выше", "v0.2.1", "v0.2.0", true, true},
		{"мажорная выше", "v1.0.0", "v0.9.9", true, true},
		{"те же версии", "v0.1.0", "v0.1.0", false, true},
		{"откат назад", "v0.1.0", "v0.2.0", false, true},
		{"без префикса v", "0.2.0", "0.1.0", true, true},
		{"двузначные числа сравниваются как числа", "v0.10.0", "v0.9.0", true, true},
		// Сборка после тега новее самого тега: предлагать «обновиться» с неё
		// на старый тег значило бы откатывать человека назад.
		{"сборка после тега новее тега", "v0.1.0", "v0.1.0-3-gabcdef", false, true},
		{"грязная сборка новее тега", "v0.1.0", "v0.1.0-dirty", false, true},
		{"новый тег обгоняет сборку после старого", "v0.2.0", "v0.1.0-3-gabcdef", true, true},
		// Предрелиз, наоборот, старше релиза.
		{"релиз новее своего rc", "v0.2.0", "v0.2.0-rc1", true, true},
		{"rc старше релиза", "v0.2.0-rc1", "v0.2.0", false, true},
		{"установлен dev", "v0.2.0", "dev", false, false},
		{"мусор в теге", "release-latest", "v0.1.0", false, false},
		{"мусор с обеих сторон", "junk", "dev", false, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			newer, comparable := Newer(tc.latest, tc.installed)
			if newer != tc.newer || comparable != tc.comparable {
				t.Fatalf("Newer(%q, %q) = (%v, %v), ожидалось (%v, %v)",
					tc.latest, tc.installed, newer, comparable, tc.newer, tc.comparable)
			}
		})
	}
}
