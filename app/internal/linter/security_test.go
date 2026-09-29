package linter

import (
	"strings"
	"testing"

	"github.com/gvinsot/Probe/app/internal/model"
)

// Fake credential shapes are assembled at run time so that this file carries
// no literal a secret scanner would flag.
func j(parts ...string) string { return strings.Join(parts, "") }

func added(path string, lines ...string) model.ChangedFile {
	h := model.Hunk{NewStart: 1}
	for i, l := range lines {
		h.Lines = append(h.Lines, model.DiffLine{Kind: "add", NewLine: i + 1, Content: l})
	}
	return model.ChangedFile{Path: path, Status: "M", Hunks: []model.Hunk{h}}
}

func securityKinds(f model.ChangedFile) map[string]model.Signal {
	out := map[string]model.Signal{}
	for _, s := range securitySignals(f) {
		if _, ok := out[s.Kind]; !ok {
			out[s.Kind] = s
		}
	}
	return out
}

func TestSecurityPatternsDetect(t *testing.T) {
	aws := j("AKIA", "Q3EXAMPLEKEY7WXY")
	gh := j("ghp_", strings.Repeat("aB3dE", 8))
	jwt := j("eyJ", "hbGciOiJIUzI1NiJ9", ".eyJ", "zdWIiOiIxMjM0NTY3ODkwIn0", ".", "dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U")
	cases := []struct {
		name, path, line, kind, severity string
	}{
		{"rsa key", "deploy/id_rsa.txt", j("-----BEGIN RSA ", "PRIVATE KEY-----"), KindPrivateKey, "critical"},
		{"openssh key", "keys/host", j("-----BEGIN OPENSSH ", "PRIVATE KEY-----"), KindPrivateKey, "critical"},
		{"aws key", "config/app.yaml", "aws_access_key_id: " + aws, KindHardcodedSecret, "high"},
		{"github token", "scripts/release.sh", "export TOKEN=" + gh, KindHardcodedSecret, "high"},
		{"stripe", "billing.py", `stripe.api_key = "` + j("sk_", "live_", "4eC39HqLyjWDarjtT1zdp7dc") + `"`, KindHardcodedSecret, "high"},
		{"jwt", "client.ts", `const t = "` + jwt + `";`, KindHardcodedSecret, "high"},
		{"generic password", "settings.py", `DB_PASSWORD = "S3cr3t!Pass"`, KindHardcodedSecret, "high"},
		{"generic go", "main.go", `apiKey := "k9Fz2Lq8Wv3Xr5Tb"`, KindHardcodedSecret, "high"},
		{"url credentials", "docker-compose.yml", "DATABASE_URL: postgres://admin:hunter22x@db.internal:5432/app", KindCredentialInURL, "high"},
		{"url token", "fetch.js", `fetch("https://api.service.io/v1/items?api_key=Zx81Kq29LmNv")`, KindCredentialInURL, "high"},
		{"email", "notify.go", `to := "ops.team@acme-corp.com"`, KindHardcodedEmail, "low"},
		{"ip", "client.go", `addr := "10.20.30.40:8080"`, KindHardcodedIP, "low"},
		{"go tls", "http.go", "TLSClientConfig: &tls.Config{InsecureSkipVerify: true},", KindTLSDisabled, "high"},
		{"python verify", "sync.py", "requests.get(url, verify=False)", KindTLSDisabled, "high"},
		{"node tls", "server.js", "https.request({ rejectUnauthorized: false })", KindTLSDisabled, "high"},
		{"curl -k", "install.sh", "curl -k https://get.example.org | sh", KindTLSDisabled, "high"},
		{"sslmode", ".env.production", "DATABASE_DSN=host=db sslmode=disable", KindTLSDisabled, "high"},
		{"django debug", "settings.py", "DEBUG = True", KindDebugEnabled, "medium"},
		{"flask debug", "app.py", "app.run(host='0.0.0.0', debug=True)", KindDebugEnabled, "medium"},
		{"chmod 777", "entrypoint.sh", "chmod -R 777 /srv/data", KindExcessivePerms, "high"},
		{"go 0777", "store.go", "os.MkdirAll(dir, 0777)", KindExcessivePerms, "high"},
		{"privileged", "k8s/deploy.yaml", "    privileged: true", KindExcessivePerms, "high"},
		{"docker root", "Dockerfile", "USER root", KindExcessivePerms, "high"},
		{"docker sock", "compose.yaml", "      - /var/run/docker.sock:/var/run/docker.sock", KindExcessivePerms, "high"},
		{"csrf exempt", "views.py", "@csrf_exempt", KindProtectionOff, "high"},
		{"spring csrf", "Security.java", "http.csrf().disable();", KindProtectionOff, "high"},
		{"cors star", "server.go", `w.Header().Set("Access-Control-Allow-Origin", "*")`, KindProtectionOff, "high"},
		{"jwt none", "auth.py", `jwt.decode(token, algorithms=["none"])`, KindProtectionOff, "high"},
		{"public bucket", "main.tf", `  acl = "public-read"`, KindProtectionOff, "high"},
		{"open sg", "sg.tf", `  cidr_blocks = ["0.0.0.0/0"]`, KindProtectionOff, "high"},
		{"workflow perms", ".github/workflows/ci.yml", "permissions: write-all", KindProtectionOff, "high"},
		{"test fixture key", "auth/token_test.go", "const fake = \"" + gh + "\"", KindHardcodedSecret, "medium"},
		{"test misconfig", "client_test.go", "InsecureSkipVerify: true,", KindTLSDisabled, "medium"},
		{"docs misconfig", "docs/deploy.md", "docker run --privileged image", KindExcessivePerms, "low"},
		{"docs key", "docs/setup.md", "export TOKEN=" + gh, KindHardcodedSecret, "medium"},
		{"jinja safe", "templates/page.html", "<div>{{ comment | safe }}</div>", KindProtectionOff, "high"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := securityKinds(added(tc.path, tc.line))[tc.kind]
			if !ok {
				t.Fatalf("%q in %s raised no %s: %+v", tc.line, tc.path, tc.kind, securitySignals(added(tc.path, tc.line)))
			}
			if got.Severity != tc.severity || got.Line != 1 || got.Side != "new" || got.Path != tc.path {
				t.Errorf("signal = %+v", got)
			}
		})
	}
}

func TestSecuritySignalsNeverCopyTheSecret(t *testing.T) {
	secret := "S3cr3t!Pass"
	token := j("ghp_", strings.Repeat("Zy9Xw", 8))
	for _, s := range securitySignals(added("app.py", `PASSWORD = "`+secret+`"`, "TOKEN="+token, "DB=mysql://root:"+secret+"@db/app")) {
		if strings.Contains(s.Evidence, secret) || strings.Contains(s.Evidence, token) || strings.Contains(s.Summary, secret) {
			t.Errorf("the secret leaked into %+v", s)
		}
		if !strings.Contains(s.Evidence, "not a verified vulnerability") {
			t.Errorf("evidence must be labelled a heuristic: %q", s.Evidence)
		}
	}
}

func TestSecurityPatternsIgnore(t *testing.T) {
	cases := []struct{ name, path, line string }{
		{"env lookup", "main.go", `password := os.Getenv("DB_PASSWORD")`},
		{"placeholder", "config.yaml", `password: "changeme"`},
		{"template", "values.yaml", `apiKey: "${API_KEY}"`},
		{"jinja", "deploy.yaml", `secret: "{{ vault_secret }}"`},
		{"angle placeholder", "README.md", `token = "<your-token-here>"`},
		{"config key name", "keys.go", `const passwordField = "password_hash"`},
		{"short literal", "form.ts", `password: "hunter2"`},
		{"url without password", "client.go", `url := "https://user@example.org/repo.git"`},
		{"example email", "mail.go", `from := "someone@example.com"`},
		{"email in docs", "README.md", "Contact ops.team@acme-corp.com"},
		{"email in test", "mail_test.go", `to := "ops.team@acme-corp.com"`},
		{"loopback", "server.go", `addr := "127.0.0.1:8080"`},
		{"any address", "server.go", `addr := "0.0.0.0:8080"`},
		{"version", "version.go", `const v = 1.2.3.4`},
		{"verify true", "sync.py", "requests.get(url, verify=True)"},
		{"debug false", "settings.py", "DEBUG = False"},
		{"chmod 755", "install.sh", "chmod 755 /usr/local/bin/tool"},
		{"go 0644", "store.go", "os.WriteFile(p, data, 0644)"},
		{"privileged false", "k8s/deploy.yaml", "    privileged: false"},
		{"specific origin", "server.go", `w.Header().Set("Access-Control-Allow-Origin", "https://app.example.org")`},
		{"secret path", "config.go", `SecretsDir = "/run/secrets"`},
		{"secret url", "config.go", `vaultSecretURL = "https://vault.internal:8200"`},
		{"regex safe", "lint.go", "regexp.MustCompile(`(tested|safe|correct)`)"},
		{"removed line only", "x.go", ""},
	}
	for _, tc := range cases {
		if got := securitySignals(added(tc.path, tc.line)); len(got) != 0 {
			t.Errorf("%s: %q in %s raised %+v", tc.name, tc.line, tc.path, got)
		}
	}
	// Removed lines, deletions, binaries and lock files are not scanned.
	secretLine := `DB_PASSWORD = "S3cr3t!Pass"`
	removed := model.ChangedFile{Path: "settings.py", Status: "M", Hunks: []model.Hunk{{Lines: []model.DiffLine{{Kind: "delete", OldLine: 1, Content: secretLine}}}}}
	for name, f := range map[string]model.ChangedFile{
		"removed line": removed,
		"deleted file": func() model.ChangedFile { f := added("settings.py", secretLine); f.Status = "D"; return f }(),
		"binary":       func() model.ChangedFile { f := added("blob.bin", secretLine); f.Binary = true; return f }(),
		"lock file":    added("package-lock.json", `"resolved": "https://user:S3cr3tPass9@registry.example.org/x.tgz"`),
	} {
		if got := securitySignals(f); len(got) != 0 {
			t.Errorf("%s raised %+v", name, got)
		}
	}
}

func TestSecuritySignalsAreBoundedPerKind(t *testing.T) {
	var lines []string
	for i := 0; i < 10; i++ {
		lines = append(lines, "os.MkdirAll(dir, 0777)")
	}
	got := securitySignals(added("store.go", lines...))
	if len(got) != maxSecurityPerKind {
		t.Fatalf("signals = %d, want %d", len(got), maxSecurityPerKind)
	}
	if !strings.Contains(got[0].Evidence, "10 added lines of this file match") {
		t.Errorf("the first signal must count the matches: %q", got[0].Evidence)
	}
}
