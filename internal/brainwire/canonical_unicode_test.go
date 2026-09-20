package brainwire

import (
	"reflect"
	"strings"
	"testing"
)

func TestCanonicalRejectsLossyUnicode(t *testing.T) {
	for _, bad := range []string{string([]byte{0xff}), `\ud800`, `\udfff`, `\ud800x`, `\ud800\u0041`, `\udc00\ud800`} {
		for _, raw := range []string{`{"manifest":{"repo_key":"` + bad + `"}}`, `{"` + bad + `":"value"}`, `{"future":["` + bad + `"]}`} {
			if got, err := Canonicalize([]byte(raw)); err == nil {
				t.Errorf("accepted lossy Unicode %q: %s", raw, got)
			}
		}
	}
	for _, value := range []string{`\ud83d\ude00`, "😀", "�", `\\ud800`, `escaped\"quote`} {
		raw := `{"future":"` + value + `"}`
		if _, err := Canonicalize([]byte(raw)); err != nil {
			t.Errorf("rejected valid Unicode %s: %v", raw, err)
		}
	}
}

func TestCanonicalMarshalRejectsEveryMalformedStringField(t *testing.T) {
	// Visit every populated schema string, including nested content references.
	var paths [][]int
	var walk func(reflect.Value, []int)
	walk = func(v reflect.Value, path []int) {
		switch v.Kind() {
		case reflect.String:
			paths = append(paths, append([]int(nil), path...))
		case reflect.Struct:
			for i := 0; i < v.NumField(); i++ {
				if v.Type().Field(i).PkgPath == "" {
					walk(v.Field(i), append(path, i))
				}
			}
		case reflect.Slice:
			for i := 0; i < v.Len(); i++ {
				walk(v.Index(i), append(path, i))
			}
		}
	}
	walk(reflect.ValueOf(*representativeArtifact(t)), nil)
	for _, path := range paths {
		a := *representativeArtifact(t)
		v := reflect.ValueOf(&a).Elem()
		for _, i := range path {
			if v.Kind() == reflect.Struct {
				v = v.Field(i)
			} else {
				v = v.Index(i)
			}
		}
		v.SetString(string([]byte{0xff}))
		if got, err := CanonicalMarshal(a); err == nil {
			t.Errorf("malformed field %v encoded as %s", path, got)
		} else if !strings.Contains(err.Error(), "UTF-8") {
			t.Errorf("wrong refusal: %v", err)
		}
	}
}
