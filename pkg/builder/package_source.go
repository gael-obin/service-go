package builder

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"go/version"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
)

// Use Go's module/toolchain selection on the source, just as the native build
// does. The cross image supplies C compilers, not the module's Go version.
func resolvePackageGoToolchain(ctx context.Context, source, workFile string) (string, error) {
	command := exec.CommandContext(ctx, "go", "env", "GOVERSION")
	command.Dir = source
	command.Env = append(os.Environ(), "GOWORK="+workFile)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		return "", fmt.Errorf("resolve Go package toolchain: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	selected := strings.TrimSpace(string(output))
	if !version.IsValid(selected) {
		return "", fmt.Errorf("Go package toolchain %q cannot be selected for a cross build", selected)
	}
	return selected, nil
}

// Resolve the declared package with Go itself. Cross builds must mount the
// enclosing module, not just a nested main directory with no go.mod or imports.
func resolvePackageSource(ctx context.Context, source string, target *builderv0.PackageTarget, workFile string) (root, entry string, err error) {
	command := exec.CommandContext(ctx, "go", "list", "-mod=readonly", "-json", ".")
	command.Dir = source
	command.Env = append(os.Environ(), "GOWORK="+workFile, "CGO_ENABLED=1", "GOOS="+target.GetOs(), "GOARCH="+target.GetArchitecture())
	var stderr bytes.Buffer
	command.Stderr = &stderr
	payload, err := command.Output()
	if err != nil {
		return "", "", fmt.Errorf("resolve Go package source: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	var pkg struct {
		Name   string
		Dir    string
		Module *struct{ Dir string }
	}
	if err := json.Unmarshal(payload, &pkg); err != nil {
		return "", "", fmt.Errorf("decode Go package source: %w", err)
	}
	if pkg.Name != "main" {
		return "", "", fmt.Errorf("Go executable packaging requires package main, got %q; select the main package source directory", pkg.Name)
	}
	if pkg.Module == nil || !filepath.IsAbs(pkg.Module.Dir) || !filepath.IsAbs(pkg.Dir) {
		return "", "", fmt.Errorf("Go executable packaging requires a module-owned source directory")
	}
	// Preserve Go's selected paths: canonicalizing only the source changes its
	// identity relative to a go.work reached through a symlink.
	root = filepath.Clean(pkg.Module.Dir)
	directory := filepath.Clean(pkg.Dir)
	relative, err := filepath.Rel(root, directory)
	if err != nil || !filepath.IsLocal(relative) {
		return "", "", fmt.Errorf("Go main package must stay inside its module root")
	}
	if relative == "." {
		return root, ".", nil
	}
	return root, "./" + filepath.ToSlash(relative), nil
}

// Let Go resolve explicit GOWORK and parent discovery; release builds opt out.
func packageWorkspace(ctx context.Context, source string, enabled bool) (string, error) {
	if !enabled {
		return "off", nil
	}
	command := exec.CommandContext(ctx, "go", "env", "GOWORK")
	command.Dir = source
	output, err := command.Output()
	if err != nil {
		return "", fmt.Errorf("resolve package workspace: %w", err)
	}
	selected := strings.TrimSpace(string(output))
	if selected == "" || selected == "off" {
		return "off", nil
	}
	if !filepath.IsAbs(selected) {
		return "", fmt.Errorf("Go selected non-absolute workspace %q", selected)
	}
	return selected, nil
}

// Go owns module selection, including workspace and module replacements. Mount
// only filesystem modules; downloaded versions remain the toolchain's concern.
func packageLocalModules(ctx context.Context, source, workFile string) ([]string, error) {
	command := exec.CommandContext(ctx, "go", "list", "-mod=readonly", "-m", "-json", "all")
	command.Dir = source
	command.Env = append(os.Environ(), "GOWORK="+workFile)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("resolve package modules: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	decoder := json.NewDecoder(bytes.NewReader(output))
	directories := map[string]bool{}
	for {
		var module struct {
			Main    bool
			Dir     string
			Replace *struct{ Version string }
		}
		err := decoder.Decode(&module)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("decode package module: %w", err)
		}
		if module.Main || (module.Replace != nil && module.Replace.Version == "") {
			if !filepath.IsAbs(module.Dir) {
				return nil, fmt.Errorf("local package module has no absolute directory: %q", module.Dir)
			}
			directories[module.Dir] = true
		}
	}
	result := make([]string, 0, len(directories))
	for directory := range directories {
		result = append(result, directory)
	}
	sort.Strings(result)
	return result, nil
}
