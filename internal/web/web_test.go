package web

import (
	"encoding/json"
	"io/fs"
	"regexp"
	"sort"
	"strings"
	"testing"
)

func read(t *testing.T, name string) string {
	t.Helper()
	b, err := fs.ReadFile(Files, name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func dictionary(t *testing.T) map[string]map[string]string {
	t.Helper()
	var d map[string]map[string]string
	if err := json.Unmarshal([]byte(read(t, "i18n.json")), &d); err != nil {
		t.Fatalf("i18n.json: %v", err)
	}
	return d
}

// The two languages have exactly the same keys, and no empty text.
func TestLanguagesHaveTheSameKeys(t *testing.T) {
	d := dictionary(t)
	if len(d) != 2 || d["en"] == nil || d["zh"] == nil {
		t.Fatalf("languages in the dictionary: %d, want en and zh", len(d))
	}
	for _, pair := range [][2]string{{"en", "zh"}, {"zh", "en"}} {
		var missing []string
		for k, v := range d[pair[0]] {
			if _, ok := d[pair[1]][k]; !ok {
				missing = append(missing, k)
			}
			if strings.TrimSpace(v) == "" {
				t.Errorf("%s: the text of %q is empty", pair[0], k)
			}
		}
		sort.Strings(missing)
		if len(missing) > 0 {
			t.Errorf("keys in %s that %s lacks: %v", pair[0], pair[1], missing)
		}
	}
	// the placeholders of a text are the same in both languages
	holes := regexp.MustCompile(`\{\d+\}`)
	for k, en := range d["en"] {
		a, b := holes.FindAllString(en, -1), holes.FindAllString(d["zh"][k], -1)
		sort.Strings(a)
		sort.Strings(b)
		if strings.Join(a, "") != strings.Join(b, "") {
			t.Errorf("%q: placeholders %v in en, %v in zh", k, a, b)
		}
	}
}

// Every text the page and the script ask for is in the dictionary, every text in the
// dictionary is asked for, and the script asks only with keys written out in full.
func TestKeysInUse(t *testing.T) {
	d := dictionary(t)
	js, html := read(t, "app.js"), read(t, "index.html")
	used := map[string]bool{}
	for _, m := range regexp.MustCompile(`\bt\('([^']+)'`).FindAllStringSubmatch(js, -1) {
		used[m[1]] = true
	}
	for _, m := range regexp.MustCompile(`data-i18n="([^"]+)"`).FindAllStringSubmatch(html+js, -1) {
		used[m[1]] = true
	}
	// calls of t() without a key written out in full: its definition, and the one that
	// takes the key from a data-i18n attribute
	var other []string
	for _, m := range regexp.MustCompile("\\bt\\(([^')][^,)]*)").FindAllStringSubmatch(js, -1) {
		other = append(other, m[1])
	}
	if got := strings.Join(other, " | "); got != "key | el.dataset.i18n" {
		t.Errorf("app.js calls t() without a literal key: %s", got)
	}
	for k := range used {
		if _, ok := d["en"][k]; !ok {
			t.Errorf("the page asks for %q, which the dictionary lacks", k)
		}
	}
	for k := range d["en"] {
		if !used[k] {
			t.Errorf("the dictionary has %q, which nothing asks for", k)
		}
	}
}

// The page loads nothing from anywhere else, and the hand-written files are plain ASCII:
// the texts of the two languages are in the dictionary and nowhere else.
func TestFilesAreSelfContained(t *testing.T) {
	names, err := fs.Glob(Files, "*")
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(names)
	if got := strings.Join(names, " "); got != "app.js i18n.json index.html style.css" {
		t.Fatalf("files: %s", got)
	}
	for _, name := range names {
		s := read(t, name)
		// the one address a file may name: the link to the documentation, which the
		// page shows as a link and never loads
		s = strings.ReplaceAll(s, "'https://github.com/Claw-King/tcpraid'", "")
		if strings.Contains(s, "http://") || strings.Contains(s, "https://") || strings.Contains(s, "//cdn") {
			t.Errorf("%s names an address outside the node", name)
		}
		if name == "i18n.json" {
			continue
		}
		for i, r := range s {
			if r > 127 {
				t.Errorf("%s: a character that is not ASCII at byte %d", name, i)
				break
			}
		}
	}
	html := read(t, "index.html")
	if strings.Contains(html, "<style") || strings.Contains(html, " style=") || regexp.MustCompile(`<script(>| [^s])`).MatchString(html) || strings.Contains(html, " onclick=") {
		t.Error("index.html has an inline script or style, which its content security policy forbids")
	}
}
