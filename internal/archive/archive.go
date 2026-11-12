// Package archive builds the tar/zstd/gzip/xz command lines used by the SFTP
// page. It is pure logic: the caller decides whether the commands run locally
// or over SSH, and whether a name already exists.
package archive

import (
	"fmt"
	"strings"
)

// Format names double as the file-name suffix, so the UI can show them as is.
const (
	FormatTarZst = "tar.zst"
	FormatTarGz  = "tar.gz"
	FormatTarXz  = "tar.xz"
	FormatTar    = "tar"
)

// ArchivePrefix is the base name used when several entries are bundled.
const ArchivePrefix = "archive"

// archiveNewStem is appended while the candidate name exists.
const archiveNewStem = "_new"

// ZstdLevel keeps zstd at its cheapest setting.
const ZstdLevel = "-1"

// Tools records which utilities exist on the host that owns the files.
type Tools struct {
	Tar  bool `json:"tar"`
	Zstd bool `json:"zstd"`
	Gzip bool `json:"gzip"`
	Xz   bool `json:"xz"`
}

// Formats lists the archive formats the available tools can produce.
func Formats(t Tools) []string {
	formats := make([]string, 0, 4)
	if t.Tar && t.Zstd {
		formats = append(formats, FormatTarZst)
	}
	if t.Tar && t.Gzip {
		formats = append(formats, FormatTarGz)
	}
	if t.Tar && t.Xz {
		formats = append(formats, FormatTarXz)
	}
	if t.Tar {
		formats = append(formats, FormatTar)
	}
	return formats
}

// Supports reports whether the tools can create the format.
func Supports(t Tools, format string) bool {
	for _, candidate := range Formats(t) {
		if candidate == format {
			return true
		}
	}
	return false
}

// Step is one command of a plan. Cleanup names a file the step creates and
// leaves behind; the caller removes it after the plan runs.
type Step struct {
	Argv    []string
	Cleanup string
}

// Plan is an ordered list of commands executed in one working directory.
type Plan struct {
	Steps []Step
}

// CleanupPaths returns the temporary files that must be removed.
func (p Plan) CleanupPaths() []string {
	paths := make([]string, 0, len(p.Steps))
	for _, step := range p.Steps {
		if step.Cleanup != "" {
			paths = append(paths, step.Cleanup)
		}
	}
	return paths
}

// TemporaryTarName is the intermediate tarball used by the two-step zstd plans.
func TemporaryTarName(token string) string {
	return ".chemssh-archive-" + token + ".tar"
}

// CompressPlan creates the named archive from `names`, which are relative to the
// working directory. zstd is driven directly instead of through `tar -I`, because that
// flag behaves differently between GNU tar and bsdtar; the two-step form works
// with both.
func CompressPlan(t Tools, format, archiveName string, names []string, token string) (Plan, error) {
	if len(names) == 0 {
		return Plan{}, fmt.Errorf("no files to compress")
	}
	if !Supports(t, format) {
		return Plan{}, fmt.Errorf("archive format %q is not available", format)
	}
	switch format {
	case FormatTarZst:
		temporary := TemporaryTarName(token)
		return Plan{Steps: []Step{
			{Argv: append([]string{"tar", "-cf", temporary, "--"}, names...)},
			{Argv: []string{"zstd", ZstdLevel, "-f", temporary, "-o", archiveName}, Cleanup: temporary},
		}}, nil
	case FormatTarGz:
		return Plan{Steps: []Step{{Argv: append([]string{"tar", "-czf", archiveName, "--"}, names...)}}}, nil
	case FormatTarXz:
		return Plan{Steps: []Step{{Argv: append([]string{"tar", "-cJf", archiveName, "--"}, names...)}}}, nil
	case FormatTar:
		return Plan{Steps: []Step{{Argv: append([]string{"tar", "-cf", archiveName, "--"}, names...)}}}, nil
	default:
		return Plan{}, fmt.Errorf("unknown archive format %q", format)
	}
}

// extractSuffixes is ordered longest first so `.tar.gz` wins over `.gz`.
var extractSuffixes = []struct {
	suffix string
	tar    bool
	tool   string
	argv   []string
}{
	{".tar.zst", true, "zstd", nil},
	{".tzst", true, "zstd", nil},
	{".tar.gz", true, "gzip", []string{"tar", "-xzf"}},
	{".tgz", true, "gzip", []string{"tar", "-xzf"}},
	{".tar.xz", true, "xz", []string{"tar", "-xJf"}},
	{".txz", true, "xz", []string{"tar", "-xJf"}},
	{".tar", true, "", []string{"tar", "-xf"}},
	{".zst", false, "zstd", []string{"zstd", "-d", "-f"}},
	{".gz", false, "gzip", []string{"gzip", "-d", "-f"}},
	{".xz", false, "xz", []string{"xz", "-d", "-f"}},
}

func hasTool(t Tools, name string) bool {
	switch name {
	case "tar":
		return t.Tar
	case "zstd":
		return t.Zstd
	case "gzip":
		return t.Gzip
	case "xz":
		return t.Xz
	}
	return false
}

func matchExtractSuffix(name string) (int, bool) {
	lower := strings.ToLower(name)
	for index, entry := range extractSuffixes {
		if len(lower) > len(entry.suffix) && strings.HasSuffix(lower, entry.suffix) {
			return index, true
		}
	}
	return 0, false
}

// SuffixSupported reports whether the name looks like an archive, regardless of
// which tools are installed.
func SuffixSupported(name string) bool {
	_, ok := matchExtractSuffix(name)
	return ok
}

// ExtractPlan builds the extraction plan for an archive. `archiveArg` is what
// the tool receives (usually an absolute path); `name` only drives the suffix
// detection.
func ExtractPlan(name string, t Tools, archiveArg, token string) (Plan, bool) {
	index, ok := matchExtractSuffix(name)
	if !ok {
		return Plan{}, false
	}
	entry := extractSuffixes[index]
	if entry.tool != "" && !hasTool(t, entry.tool) {
		return Plan{}, false
	}
	if entry.tar && !t.Tar {
		return Plan{}, false
	}
	if entry.suffix == ".tar.zst" || entry.suffix == ".tzst" {
		temporary := TemporaryTarName(token)
		return Plan{Steps: []Step{
			{Argv: []string{"zstd", "-d", "-f", archiveArg, "-o", temporary}, Cleanup: temporary},
			{Argv: []string{"tar", "-xf", temporary}},
		}}, true
	}
	argv := append(append([]string{}, entry.argv...), archiveArg)
	return Plan{Steps: []Step{{Argv: argv}}}, true
}

// ArchiveStem is the base name of a generated archive. Compressing a single
// entry keeps its original name so the result is recognizable; bundling several
// entries falls back to the shared `archive` placeholder.
func ArchiveStem(names []string) string {
	if len(names) == 1 && names[0] != "" {
		return names[0]
	}
	return ArchivePrefix
}

// NextArchiveName returns `<stem><suffix>`, appending `_new` while the name is
// taken, so an existing archive is never overwritten.
func NextArchiveName(format string, names []string, taken func(string) bool) (string, error) {
	suffix := "." + format
	switch format {
	case FormatTarZst, FormatTarGz, FormatTarXz, FormatTar:
	default:
		return "", fmt.Errorf("unknown archive format %q", format)
	}
	stem := ArchiveStem(names)
	for taken(stem + suffix) {
		stem += archiveNewStem
	}
	return stem + suffix, nil
}

// CommandLine renders argv as a POSIX shell command, quoting every argument so a
// remote `sh -c` sees exactly these arguments.
func CommandLine(argv []string) string {
	quoted := make([]string, 0, len(argv))
	for _, arg := range argv {
		quoted = append(quoted, ShellQuote(arg))
	}
	return strings.Join(quoted, " ")
}

// ShellQuote wraps a value in single quotes, escaping embedded quotes.
func ShellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}
