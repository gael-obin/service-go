package builder

import (
	"encoding/json"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	goservice "github.com/codefly-dev/service-go/pkg/service"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
)

// go build succeeds for a library too, but its ar archive cannot be launched.
func TestPackageRejectsLibraryArchive(t *testing.T) {
	source := t.TempDir()
	mustWrite(t, filepath.Join(source, "go.mod"), "module example.test/library\n\ngo 1.27.0\n", 0600)
	mustWrite(t, filepath.Join(source, "library.go"), "package library\nconst Value = 42\n", 0600)
	destination := filepath.Join(t.TempDir(), "agent")
	err := packageGoBinary(t.Context(), source, destination, &builderv0.PackageTarget{Os: runtime.GOOS, Architecture: runtime.GOARCH}, false)
	if err == nil || !strings.Contains(err.Error(), "package main") {
		t.Fatalf("library must not be packaged as an executable: %v", err)
	}
	if _, err := os.Stat(destination); !os.IsNotExist(err) {
		t.Fatalf("invalid package was installed: %v", err)
	}
}

func TestPackageDeclaredNestedMainUsesEnclosingModule(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "go.mod"), "module example.test/nested\n\ngo 1.27.0\n", 0600)
	mustWrite(t, filepath.Join(root, "library.go"), "package nested\nconst Value = 42\n", 0600)
	source := filepath.Join(root, "cmd", "agent")
	if err := os.MkdirAll(source, 0700); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(source, "main.go"), "package main\nimport (\"fmt\"; \"example.test/nested\")\nfunc main() { fmt.Println(nested.Value) }\n", 0600)
	destination := filepath.Join(t.TempDir(), "agent")
	if err := packageGoBinary(t.Context(), source, destination, &builderv0.PackageTarget{Os: runtime.GOOS, Architecture: runtime.GOARCH}, false); err != nil {
		t.Fatal(err)
	}
	output, err := exec.CommandContext(t.Context(), destination).CombinedOutput()
	if err != nil || string(output) != "42\n" {
		t.Fatalf("nested executable: %v, output=%q", err, output)
	}
}

func TestPackageWorkspaceSelection(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"source/go.mod":     "module example.test/agent\n\ngo 1.27.0\nrequire example.test/value v0.0.0\nreplace example.test/value => ../released\n",
		"source/main.go":    "package main\nimport (\"fmt\"; \"example.test/value\")\nfunc main() { fmt.Println(value.Value) }\n",
		"released/go.mod":   "module example.test/value\n\ngo 1.27.0\n",
		"released/value.go": "package value\nconst Value = 1\n",
		"local/go.mod":      "module example.test/value\n\ngo 1.27.0\n",
		"local/value.go":    "package value\nconst Value = 2\n",
		"go.work":           "go 1.27.0\nuse (\n ./source\n ./local\n)\n",
	} {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		mustWrite(t, path, content, 0600)
	}
	t.Setenv("GOWORK", filepath.Join(root, "go.work"))
	for _, test := range []struct {
		name      string
		workspace bool
		want      string
	}{
		{"standalone", false, "1\n"}, {"workspace", true, "2\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			svc := goservice.New(&resources.Agent{Kind: "codefly:service", Name: "go"})
			if err := svc.HeadlessLoad(t.Context(), &basev0.ServiceIdentity{Workspace: "test", Module: "module", Name: "agent"}); err != nil {
				t.Fatal(err)
			}
			svc.SourceLocation = filepath.Join(root, "source")
			svc.Settings.WithWorkspace = test.workspace
			b := New(svc, BuildConfig{})
			response, err := b.Package(t.Context(), &builderv0.PackageRequest{
				OutputDirectory: t.TempDir(), ArtifactName: "agent", IncludeSbom: true,
				Targets: []*builderv0.PackageTarget{{Os: runtime.GOOS, Architecture: runtime.GOARCH}},
			})
			if err != nil {
				t.Fatal(err)
			}
			if response.GetState().GetState() != builderv0.PackageStatus_SUCCESS {
				t.Fatal(response.GetState())
			}
			var executable, inventory string
			for _, artifact := range response.GetArtifacts() {
				if artifact.GetKind() == builderv0.PackageArtifact_EXECUTABLE {
					executable = artifact.GetPath()
				}
				if artifact.GetKind() == builderv0.PackageArtifact_SBOM {
					inventory = artifact.GetPath()
				}
			}
			output, err := exec.CommandContext(t.Context(), executable).CombinedOutput()
			if err != nil || string(output) != test.want {
				t.Fatalf("selected dependency: %v, output=%q, want=%q", err, output, test.want)
			}
			if inventory == "" {
				t.Fatal("package must carry SBOM")
			}
			payload, err := os.ReadFile(inventory)
			if err != nil {
				t.Fatal(err)
			}
			var bom struct{ Components []struct{ Purl string } }
			if err := json.Unmarshal(payload, &bom); err != nil {
				t.Fatal(err)
			}
			want := "pkg:golang/example.test/value@v0.0.0"
			if test.workspace {
				want = "pkg:golang/example.test/value"
			}
			found := false
			for _, component := range bom.Components {
				if component.Purl == want {
					found = true
				}
			}
			if !found {
				t.Fatalf("SBOM does not describe selected dependency %s", want)
			}

		})
	}
}
