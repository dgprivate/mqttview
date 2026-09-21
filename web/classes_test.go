package webui

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// classNames that appear in the markup on purpose without a rule of their own.
// Each one has to earn its place here, because the whole value of the test
// below is that an unstyled class is normally a mistake.
var unstyledOnPurpose = map[string]string{
	"log-attr":     "a child of .log-attrs, which is a flex container: the gap does the spacing, the class is only a key",
	"saved-folder": "a wrapper whose heading is styled by .saved-folder-name; the wrapper itself needs nothing",
}

var (
	classAttr  = regexp.MustCompile(`className="([^"{}]+)"`)
	cssClasses = regexp.MustCompile(`\.([A-Za-z][\w-]*)`)
)

// A class in the markup that the stylesheet does not define renders as
// nothing: the element keeps the browser's default appearance and nobody
// notices until somebody looks at the page.
//
// That is not hypothetical. Six links and two export anchors were written as
// className="button" while the stylesheet has always styled .btn, so every one
// of them rendered as a plain underlined link next to the real buttons it was
// meant to match — on the Explorer page, directly beside a working .btn.
func TestEveryClassTheMarkupUsesIsStyled(t *testing.T) {
	sheet, err := os.ReadFile(filepath.Join("src", "index.css"))
	if err != nil {
		t.Fatalf("reading the stylesheet: %v", err)
	}
	styled := map[string]bool{}
	for _, m := range cssClasses.FindAllStringSubmatch(string(sheet), -1) {
		styled[m[1]] = true
	}
	if len(styled) == 0 {
		t.Fatal("no class selectors found in the stylesheet; the test is not reading what it thinks it is")
	}

	// Where each unstyled class was found, so the failure names a file.
	found := map[string][]string{}
	err = filepath.WalkDir("src", func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".tsx") {
			return err
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, m := range classAttr.FindAllStringSubmatch(string(body), -1) {
			for _, class := range strings.Fields(m[1]) {
				if styled[class] || unstyledOnPurpose[class] != "" {
					continue
				}
				found[class] = append(found[class], path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the frontend sources: %v", err)
	}

	names := make([]string, 0, len(found))
	for class := range found {
		names = append(names, class)
	}
	sort.Strings(names)
	for _, class := range names {
		t.Errorf("className %q has no rule in src/index.css, so it styles nothing; used in %s",
			class, strings.Join(found[class], ", "))
	}
}

// The allow-list is only defensible while every entry is still in the markup.
// An entry left behind after its class is gone quietly widens the test.
func TestTheUnstyledAllowListHasNoStaleEntries(t *testing.T) {
	var markup strings.Builder
	err := filepath.WalkDir("src", func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".tsx") {
			return err
		}
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		markup.Write(body)
		return nil
	})
	if err != nil {
		t.Fatalf("walking the frontend sources: %v", err)
	}
	for class, why := range unstyledOnPurpose {
		if !strings.Contains(markup.String(), `"`+class+`"`) &&
			!strings.Contains(markup.String(), class+` "`) &&
			!strings.Contains(markup.String(), ` `+class) {
			t.Errorf("%q is allowed to be unstyled (%s) but no longer appears in the markup; drop the entry", class, why)
		}
	}
}
