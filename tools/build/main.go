package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

func main() {
	var webview2 bool
	var windowsGUI bool
	var out string
	flag.BoolVar(&webview2, "webview2", false, "build with WebView2 support")
	flag.BoolVar(&windowsGUI, "windowsgui", false, "build with the Windows GUI subsystem")
	flag.StringVar(&out, "o", "", "output executable path")
	flag.Parse()

	root, err := findRepoRoot()
	if err != nil {
		fatal(err)
	}
	version, err := readVersion(root)
	if err != nil {
		fatal(err)
	}
	if err := syncWinres(root, version); err != nil {
		fatal(err)
	}
	if runtime.GOOS == "windows" {
		if err := makeWindowsResources(root); err != nil {
			fatal(err)
		}
	}
	if err := goBuild(root, webview2, windowsGUI, out); err != nil {
		fatal(err)
	}
}

func findRepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("could not find go.mod from %s", dir)
		}
		dir = parent
	}
}

func readVersion(root string) (string, error) {
	data, err := os.ReadFile(filepath.Join(root, "internal", "version", "VERSION"))
	if err != nil {
		return "", err
	}
	version := strings.TrimSpace(string(data))
	if version == "" {
		return "", fmt.Errorf("internal/version/VERSION is empty")
	}
	return version, nil
}

func syncWinres(root, version string) error {
	path := filepath.Join(root, "winres", "winres.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	resourceVersion := toWindowsVersion(version)
	text := string(data)
	text = replaceJSONString(text, "version", resourceVersion)
	text = replaceJSONString(text, "file_version", resourceVersion)
	text = replaceJSONString(text, "product_version", resourceVersion)
	text = replaceJSONString(text, "FileVersion", version)
	text = replaceJSONString(text, "ProductVersion", version)
	if text == string(data) {
		return nil
	}
	return os.WriteFile(path, []byte(text), 0o644)
}

func toWindowsVersion(version string) string {
	parts := strings.Split(version, ".")
	for len(parts) < 4 {
		parts = append(parts, "0")
	}
	if len(parts) > 4 {
		parts = parts[:4]
	}
	return strings.Join(parts, ".")
}

func replaceJSONString(text, key, value string) string {
	pattern := regexp.MustCompile(`"` + regexp.QuoteMeta(key) + `"\s*:\s*"[^"]*"`)
	replacement := fmt.Sprintf(`"%s": "%s"`, key, value)
	return pattern.ReplaceAllString(text, replacement)
}

func makeWindowsResources(root string) error {
	out := filepath.Join(root, "cmd", "chemweb-launcher", fmt.Sprintf("rsrc_windows_%s.syso", runtime.GOARCH))
	inputs, err := windowsResourceInputs(root)
	if err != nil {
		return err
	}
	fresh, err := outputIsFresh(out, inputs)
	if err != nil {
		return err
	}
	if fresh {
		fmt.Println("winres resources are up to date")
		return nil
	}
	args := []string{
		"make",
		"--in", filepath.Join("winres", "winres.json"),
		"--arch", runtime.GOARCH,
		"--out", filepath.Join("cmd", "chemweb-launcher", "rsrc"),
	}
	if _, err := exec.LookPath("go-winres"); err == nil {
		return run(root, "go-winres", args...)
	}
	fmt.Println("go-winres was not found; falling back to go run github.com/tc-hib/go-winres@v0.3.3")
	return run(root, "go", append([]string{"run", "github.com/tc-hib/go-winres@v0.3.3"}, args...)...)
}

func windowsResourceInputs(root string) ([]string, error) {
	inputs := []string{filepath.Join(root, "winres", "winres.json")}
	pngs, err := filepath.Glob(filepath.Join(root, "winres", "*.png"))
	if err != nil {
		return nil, err
	}
	inputs = append(inputs, pngs...)
	return inputs, nil
}

func outputIsFresh(output string, inputs []string) (bool, error) {
	outInfo, err := os.Stat(output)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	for _, input := range inputs {
		info, err := os.Stat(input)
		if err != nil {
			return false, err
		}
		if info.ModTime().After(outInfo.ModTime()) {
			return false, nil
		}
	}
	return true, nil
}

func goBuild(root string, webview2, windowsGUI bool, out string) error {
	args := []string{"build"}
	if webview2 {
		args = append(args, "-tags", "webview2")
	}
	if windowsGUI {
		args = append(args, "-ldflags", "-H=windowsgui")
	}
	if out == "" && webview2 {
		out = executableName("chemweb-launcher-webview2")
	}
	if out != "" {
		args = append(args, "-o", out)
	}
	args = append(args, "./cmd/chemweb-launcher")
	return run(root, "go", args...)
}

func executableName(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

func run(dir, name string, args ...string) error {
	fmt.Println(name, strings.Join(args, " "))
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	return cmd.Run()
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "build:", err)
	os.Exit(1)
}
