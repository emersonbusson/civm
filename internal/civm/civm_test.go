package civm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateRepo(t *testing.T) {
	t.Parallel()
	for _, repo := range []string{"acme/civm", "owner/repo.name"} {
		if err := ValidateRepo(repo); err != nil {
			t.Fatalf("ValidateRepo(%q) err = %v", repo, err)
		}
	}
	for _, repo := range []string{"", "owner", "owner/repo;rm", "-bad/repo", "owner/../repo"} {
		if err := ValidateRepo(repo); err == nil {
			t.Fatalf("ValidateRepo(%q) sem erro", repo)
		}
	}
}

func TestValidateShortLabelsAndVersions(t *testing.T) {
	t.Parallel()
	if err := ValidateShort("civm-cmpx_1"); err != nil {
		t.Fatalf("ValidateShort err = %v", err)
	}
	if err := ValidateLabels("civm,linux_x64"); err != nil {
		t.Fatalf("ValidateLabels err = %v", err)
	}
	if err := ValidateSemver("2.336.0", "--runner-version"); err != nil {
		t.Fatalf("ValidateSemver err = %v", err)
	}
	for _, value := range []string{"../x", "x/y", "x y", ""} {
		if err := ValidateShort(value); err == nil {
			t.Fatalf("ValidateShort(%q) sem erro", value)
		}
	}
	if err := ValidateLabels("civm, bad label"); err == nil {
		t.Fatalf("ValidateLabels aceitou label com espaco")
	}
	if err := ValidateSemver("2.334", "--runner-version"); err == nil {
		t.Fatalf("ValidateSemver aceitou versao incompleta")
	}
}

func TestValidateUserName(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"emdev", "runner_1", "svc-runner"} {
		if err := ValidateUserName(value); err != nil {
			t.Fatalf("ValidateUserName(%q) err = %v", value, err)
		}
	}
	for _, value := range []string{"", "1runner", "bad;user", "bad user"} {
		if err := ValidateUserName(value); err == nil {
			t.Fatalf("ValidateUserName(%q) sem erro", value)
		}
	}
}

func TestResolveRunnerUserPrefersValidSudoUser(t *testing.T) {
	t.Setenv("SUDO_USER", "testuser")
	t.Setenv("USER", "ignored")

	if got := ResolveRunnerUser("fallback"); got != "testuser" {
		t.Fatalf("ResolveRunnerUser() = %q, want SUDO_USER", got)
	}
}

func TestResolveRunnerUserNeverReturnsInvalidFallbackWhenCurrentIsValid(t *testing.T) {
	t.Setenv("SUDO_USER", "bad user")
	t.Setenv("USER", "also bad")

	got := ResolveRunnerUser("fallback")
	if err := ValidateUserName(got); err != nil {
		t.Fatalf("ResolveRunnerUser() returned invalid user %q: %v", got, err)
	}
}

func TestValidateWorkflowAndUnit(t *testing.T) {
	t.Parallel()
	for _, workflow := range []string{"ci.yml", ".github/workflows/ci.yaml"} {
		if err := ValidateWorkflowFile(workflow); err != nil {
			t.Fatalf("ValidateWorkflowFile(%q) err = %v", workflow, err)
		}
	}
	for _, workflow := range []string{"/tmp/ci.yml", "../ci.yml", "ci.sh", "ci.yml;rm"} {
		if err := ValidateWorkflowFile(workflow); err == nil {
			t.Fatalf("ValidateWorkflowFile(%q) sem erro", workflow)
		}
	}
	if err := ValidateServiceUnit("actions.runner.owner-repo.civm.service"); err != nil {
		t.Fatalf("ValidateServiceUnit err = %v", err)
	}
	if err := ValidateServiceUnit("../bad.service"); err == nil {
		t.Fatalf("ValidateServiceUnit aceitou path traversal")
	}
}

func TestCleanDir(t *testing.T) {
	t.Parallel()
	got, err := CleanDir("/opt/civm/../civm/deploy/systemd", "--units-source")
	if err != nil {
		t.Fatalf("CleanDir err = %v", err)
	}
	if got != "/opt/civm/deploy/systemd" {
		t.Fatalf("CleanDir got %q", got)
	}
	for _, value := range []string{"", ".", "a\x00b"} {
		if _, err := CleanDir(value, "--dir"); err == nil {
			t.Fatalf("CleanDir(%q) sem erro", value)
		}
	}
}

func TestVerifyGPGFingerprint(t *testing.T) {
	t.Parallel()
	out := "fpr:::::::::9DC858229FC7DD38854AE2D88D81803C0EBFCD88:\n"
	if err := VerifyGPGFingerprint(out, "9DC8 5822 9FC7 DD38 854A E2D8 8D81 803C 0EBF CD88", "docker"); err != nil {
		t.Fatalf("VerifyGPGFingerprint err = %v", err)
	}
	if err := VerifyGPGFingerprint(out, DefaultGitHubCLIGPGFingerprint, "github-cli"); err == nil {
		t.Fatalf("VerifyGPGFingerprint aceitou fingerprint errado")
	}
	if err := VerifyGPGFingerprint(out, "", "docker"); err == nil {
		t.Fatalf("VerifyGPGFingerprint aceitou expected vazio")
	}
}

func TestChecksumPins(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		read func() (string, bool)
	}{
		{"go", func() (string, bool) { return GoLinuxAMD64SHA256("1.26.3") }},
		{"runner", func() (string, bool) { return RunnerLinuxX64SHA256(DefaultRunnerVersion) }},
		{"node", func() (string, bool) { return NodeSourceSetupSHA256("24") }},
		{"yq", func() (string, bool) { return YQLinuxAMD64SHA256("4.52.5") }},
	}
	for _, tc := range cases {
		got, ok := tc.read()
		if !ok || len(got) != 64 {
			t.Fatalf("%s pin = %q ok=%v, want sha256", tc.name, got, ok)
		}
	}
	if _, ok := GoLinuxAMD64SHA256("0.0.0"); ok {
		t.Fatalf("GoLinuxAMD64SHA256 aceitou versao desconhecida")
	}
}

func TestFileSHA256AndVerifySHA256(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "data.txt")
	if err := os.WriteFile(path, []byte("civm\n"), 0600); err != nil {
		t.Fatalf("WriteFile err = %v", err)
	}
	got, err := FileSHA256(path)
	if err != nil {
		t.Fatalf("FileSHA256 err = %v", err)
	}
	if len(got) != 64 {
		t.Fatalf("FileSHA256 len = %d, want 64", len(got))
	}
	if err := VerifySHA256(strings.ToUpper(got), got, "test"); err != nil {
		t.Fatalf("VerifySHA256 success err = %v", err)
	}
	if err := VerifySHA256("bad", got, "test"); err == nil {
		t.Fatalf("VerifySHA256 aceitou mismatch")
	}
	if err := VerifySHA256(got, "", "test"); err == nil {
		t.Fatalf("VerifySHA256 aceitou expected vazio")
	}
}
