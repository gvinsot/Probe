package execcache

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func digest(c string) string { return strings.Repeat(c, 64) }

func validPreimage() Preimage {
	return Preimage{
		Schema: PreimageSchema, ToolVersion: "test sha256:" + digest("0"), Kind: "generated_test_base",
		BaseCommit: strings.Repeat("c", 40), Tree: Tree{PristineSHA256: digest("1"), Added: [][]string{{"pkg/x_test.go", "f", "12", digest("2")}}},
		PolicySHA256: digest("3"), ImageID: "sha256:" + digest("4"), DockerServer: "28.4.0 linux/x86_64",
		DockerArgsSHA256: digest("5"), ArgvSHA256: digest("6"), Capture: "/tmp/swiftproof-results.json", TimeoutMS: 60000, MaxOutputBytes: 32768,
	}
}

func TestPreimageEncodeIsCanonical(t *testing.T) {
	p := validPreimage()
	raw, key, err := p.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if key != SHA256Hex(raw) || !ValidKey(key) {
		t.Fatalf("key %s does not hash the preimage", key)
	}
	again, key2, _ := p.Encode()
	if string(again) != string(raw) || key2 != key {
		t.Fatal("encoding is not deterministic")
	}
	decoded, err := DecodePreimage(raw)
	if err != nil || !reflect.DeepEqual(decoded, p) {
		t.Fatalf("round trip %+v %v", decoded, err)
	}
	// The preimage holds hashes and fixed identifiers only.
	for _, field := range []string{"schema", "tool_version", "kind", "base_commit", "tree", "pristine_sha256", "added", "policy_sha256", "image_id", "docker_server", "docker_args_sha256", "argv_sha256", "capture", "timeout_ms", "max_output_bytes"} {
		if !strings.Contains(string(raw), `"`+field+`"`) {
			t.Errorf("preimage lacks %s: %s", field, raw)
		}
	}
}

// Every field of the preimage changes the key, so a key commits to all of
// them (a new field that does not change the key would fail here).
func TestKeyCommitsToEveryField(t *testing.T) {
	base := validPreimage()
	_, baseKey, _ := base.Encode()
	v := reflect.ValueOf(&base).Elem()
	for i := 0; i < v.NumField(); i++ {
		name := v.Type().Field(i).Name
		p := validPreimage()
		f := reflect.ValueOf(&p).Elem().Field(i)
		switch f.Kind() {
		case reflect.String:
			if name == "Schema" {
				continue // a different schema is refused, not rekeyed
			}
			if strings.HasSuffix(name, "SHA256") {
				f.SetString(digest("9"))
			} else {
				f.SetString(f.String() + "x")
			}
		case reflect.Int, reflect.Int64:
			f.SetInt(f.Int() + 1)
		case reflect.Struct:
			p.Tree.PristineSHA256 = digest("8")
		default:
			t.Fatalf("field %s has an unhandled kind %s", name, f.Kind())
		}
		_, key, err := p.Encode()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if key == baseKey {
			t.Errorf("changing %s does not change the key", name)
		}
	}
	p := validPreimage()
	p.Tree.Added = append(p.Tree.Added, []string{"pkg/y_test.go", "x", "12", digest("2")})
	if _, key, _ := p.Encode(); key == baseKey {
		t.Error("an added file does not change the key")
	}
	p = validPreimage()
	p.Tree.Added[0][1] = "x"
	if _, key, _ := p.Encode(); key == baseKey {
		t.Error("the executable bit of an added file does not change the key")
	}
}

// A fixed preimage has a fixed key: a format change must come with a new
// schema name, or old entries would be read under a new meaning.
func TestKeyGolden(t *testing.T) {
	_, key, err := validPreimage().Encode()
	if err != nil {
		t.Fatal(err)
	}
	const want = "bc65dde9725dce69e717a7eba2819a32f857c522f9c5a1d4b00cdb9526faddb0"
	if key != want {
		t.Logf("golden key changed: got %s", key)
		t.Fatalf("the preimage encoding changed; bump PreimageSchema and update the golden key")
	}
}

func TestPreimageValidation(t *testing.T) {
	for name, edit := range map[string]func(*Preimage){
		"schema":           func(p *Preimage) { p.Schema = "swiftproof-execcache/v0" },
		"empty kind":       func(p *Preimage) { p.Kind = "" },
		"pristine digest":  func(p *Preimage) { p.Tree.PristineSHA256 = "abc" },
		"policy digest":    func(p *Preimage) { p.PolicySHA256 = strings.ToUpper(digest("a")) },
		"args digest":      func(p *Preimage) { p.DockerArgsSHA256 = "" },
		"argv digest":      func(p *Preimage) { p.ArgvSHA256 = "go test" },
		"zero timeout":     func(p *Preimage) { p.TimeoutMS = 0 },
		"zero output":      func(p *Preimage) { p.MaxOutputBytes = 0 },
		"short tuple":      func(p *Preimage) { p.Tree.Added = [][]string{{"a", "f", "1"}} },
		"unknown type":     func(p *Preimage) { p.Tree.Added = [][]string{{"a", "l", "1", digest("2")}} },
		"file without sha": func(p *Preimage) { p.Tree.Added = [][]string{{"a", "f", "1", ""}} },
		"dir with sha":     func(p *Preimage) { p.Tree.Added = [][]string{{"a", "d", "", digest("2")}} },
		"empty path":       func(p *Preimage) { p.Tree.Added = [][]string{{"", "d", "", ""}} },
		"too many added": func(p *Preimage) {
			for i := 0; i <= maxAdded; i++ {
				p.Tree.Added = append(p.Tree.Added, []string{"d" + strings.Repeat("x", i), "d", "", ""})
			}
		},
	} {
		p := validPreimage()
		edit(&p)
		if _, _, err := p.Encode(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	raw, _, _ := validPreimage().Encode()
	var object map[string]any
	_ = json.Unmarshal(raw, &object)
	object["raw_argv"] = []string{"go", "test"}
	extra, _ := json.Marshal(object)
	if _, err := DecodePreimage(extra); err == nil {
		t.Error("an unknown preimage field was accepted")
	}
	if _, err := DecodePreimage(append(append([]byte(nil), raw...), []byte(" {}")...)); err == nil {
		t.Error("trailing data was accepted")
	}
}
