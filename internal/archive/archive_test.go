package archive

import (
	"reflect"
	"strings"
	"testing"
)

var allTools = Tools{Tar: true, Zstd: true, Gzip: true, Xz: true, Zip: true, Unzip: true, SevenZip: true}

func TestFormatsFollowInstalledTools(t *testing.T) {
	cases := []struct {
		name  string
		tools Tools
		want  []string
	}{
		{"all", allTools, []string{FormatTarZst, FormatTarGz, FormatTarXz, FormatTar, FormatZip, FormatSevenZip}},
		{"tar only", Tools{Tar: true}, []string{FormatTar}},
		{"tar and zstd", Tools{Tar: true, Zstd: true}, []string{FormatTarZst, FormatTar}},
		{"no tar", Tools{Zstd: true, Gzip: true, Xz: true}, []string{}},
		{"zip and 7z without tar", Tools{Zip: true, Unzip: true, SevenZip: true}, []string{FormatZip, FormatSevenZip}},
		{"unzip alone cannot compress", Tools{Unzip: true}, []string{}},
	}
	for _, tc := range cases {
		if got := Formats(tc.tools); !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("%s: Formats = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestArchiveStemUsesOriginalNameForSingleSelection(t *testing.T) {
	cases := []struct {
		names []string
		want  string
	}{
		{[]string{"notes.txt"}, "notes.txt"},
		{[]string{"data"}, "data"},
		{[]string{"a.txt", "b.txt"}, ArchivePrefix},
		{nil, ArchivePrefix},
	}
	for _, tc := range cases {
		if got := ArchiveStem(tc.names); got != tc.want {
			t.Fatalf("ArchiveStem(%v) = %q, want %q", tc.names, got, tc.want)
		}
	}
}

func TestNextArchiveNameAppendsNewWhileTaken(t *testing.T) {
	taken := map[string]bool{}
	selection := []string{"a.txt", "sub"}
	name, err := NextArchiveName(FormatTarGz, selection, func(candidate string) bool { return taken[candidate] })
	if err != nil || name != "archive.tar.gz" {
		t.Fatalf("first name = %q, %v", name, err)
	}

	taken["archive.tar.gz"] = true
	name, _ = NextArchiveName(FormatTarGz, selection, func(candidate string) bool { return taken[candidate] })
	if name != "archive_new.tar.gz" {
		t.Fatalf("second name = %q", name)
	}

	taken["archive_new.tar.gz"] = true
	name, _ = NextArchiveName(FormatTarGz, selection, func(candidate string) bool { return taken[candidate] })
	if name != "archive_new_new.tar.gz" {
		t.Fatalf("third name = %q", name)
	}

	if _, err := NextArchiveName("rar", selection, func(string) bool { return false }); err == nil {
		t.Fatal("unknown format must fail")
	}
	name, err = NextArchiveName(FormatZip, selection, func(string) bool { return false })
	if err != nil || name != "archive.zip" {
		t.Fatalf("zip name = %q, %v", name, err)
	}
	name, err = NextArchiveName(FormatSevenZip, []string{"data"}, func(string) bool { return false })
	if err != nil || name != "data.7z" {
		t.Fatalf("7z name = %q, %v", name, err)
	}
}

func TestNextArchiveNameUsesOriginalNameForSingleSelection(t *testing.T) {
	taken := map[string]bool{}
	name, err := NextArchiveName(FormatTarZst, []string{"notes.txt"}, func(candidate string) bool { return taken[candidate] })
	if err != nil || name != "notes.txt.tar.zst" {
		t.Fatalf("first name = %q, %v", name, err)
	}

	taken["notes.txt.tar.zst"] = true
	name, _ = NextArchiveName(FormatTarZst, []string{"notes.txt"}, func(candidate string) bool { return taken[candidate] })
	if name != "notes.txt_new.tar.zst" {
		t.Fatalf("collision name = %q", name)
	}

	name, _ = NextArchiveName(FormatTar, []string{"data"}, func(string) bool { return false })
	if name != "data.tar" {
		t.Fatalf("folder name = %q", name)
	}
}

func TestCompressPlanForEachFormat(t *testing.T) {
	plan, err := CompressPlan(allTools, FormatTar, "archive.tar", []string{"a.txt", "-b.txt"}, "tok")
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"tar", "-cf", "archive.tar", "--", "a.txt", "-b.txt"}}
	assertSteps(t, plan, want)

	plan, _ = CompressPlan(allTools, FormatTarGz, "archive.tar.gz", []string{"a"}, "tok")
	assertSteps(t, plan, [][]string{{"tar", "-czf", "archive.tar.gz", "--", "a"}})

	plan, _ = CompressPlan(allTools, FormatTarXz, "archive.tar.xz", []string{"a"}, "tok")
	assertSteps(t, plan, [][]string{{"tar", "-cJf", "archive.tar.xz", "--", "a"}})

	plan, _ = CompressPlan(allTools, FormatZip, "archive.zip", []string{"a.txt", "-b.txt"}, "tok")
	assertSteps(t, plan, [][]string{{"zip", "-r", "archive.zip", "--", "a.txt", "-b.txt"}})

	plan, _ = CompressPlan(allTools, FormatSevenZip, "archive.7z", []string{"a"}, "tok")
	assertSteps(t, plan, [][]string{{"7z", "a", "-y", "archive.7z", "--", "a"}})
}

func TestCompressPlanForTarZstUsesTwoStepsAndLowestLevel(t *testing.T) {
	plan, err := CompressPlan(allTools, FormatTarZst, "archive.tar.zst", []string{"a", "b"}, "tok")
	if err != nil {
		t.Fatal(err)
	}
	temporary := TemporaryTarName("tok")
	assertSteps(t, plan, [][]string{
		{"tar", "-cf", temporary, "--", "a", "b"},
		{"zstd", "-1", "-f", temporary, "-o", "archive.tar.zst"},
	})
	if got := plan.CleanupPaths(); !reflect.DeepEqual(got, []string{temporary}) {
		t.Fatalf("cleanup = %v", got)
	}
}

func TestCompressPlanRejectsUnavailableFormat(t *testing.T) {
	if _, err := CompressPlan(Tools{Tar: true}, FormatTarZst, "archive.tar.zst", []string{"a"}, "tok"); err == nil {
		t.Fatal("tar.zst without zstd must fail")
	}
	if _, err := CompressPlan(Tools{Zip: true}, FormatZip, "archive.zip", []string{"a"}, "tok"); err != nil {
		t.Fatal("zip with the zip tool must be offered")
	}
	if _, err := CompressPlan(Tools{Unzip: true}, FormatZip, "archive.zip", []string{"a"}, "tok"); err == nil {
		t.Fatal("zip without the zip tool must fail")
	}
	if _, err := CompressPlan(Tools{SevenZip: true}, FormatSevenZip, "archive.7z", []string{"a"}, "tok"); err != nil {
		t.Fatal("7z with the 7z tool must be offered")
	}
	if _, err := CompressPlan(Tools{Tar: true}, FormatSevenZip, "archive.7z", []string{"a"}, "tok"); err == nil {
		t.Fatal("7z without the 7z tool must fail")
	}
	if _, err := CompressPlan(allTools, FormatTar, "archive.tar", nil, "tok"); err == nil {
		t.Fatal("empty name list must fail")
	}
}

func TestExtractPlanCoversEverySuffix(t *testing.T) {
	cases := []struct {
		file string
		want [][]string
	}{
		{"a.tar.gz", [][]string{{"tar", "-xzf", "/d/a.tar.gz"}}},
		{"a.tgz", [][]string{{"tar", "-xzf", "/d/a.tgz"}}},
		{"a.tar.xz", [][]string{{"tar", "-xJf", "/d/a.tar.xz"}}},
		{"a.txz", [][]string{{"tar", "-xJf", "/d/a.txz"}}},
		{"a.tar", [][]string{{"tar", "-xf", "/d/a.tar"}}},
		{"a.txt.zst", [][]string{{"zstd", "-d", "-f", "/d/a.txt.zst"}}},
		{"a.txt.gz", [][]string{{"gzip", "-d", "-f", "/d/a.txt.gz"}}},
		{"a.txt.xz", [][]string{{"xz", "-d", "-f", "/d/a.txt.xz"}}},
		{"a.zip", [][]string{{"unzip", "-o", "/d/a.zip"}}},
		{"a.7z", [][]string{{"7z", "x", "-y", "/d/a.7z"}}},
		{"A.TAR.GZ", [][]string{{"tar", "-xzf", "/d/A.TAR.GZ"}}},
		{"A.ZIP", [][]string{{"unzip", "-o", "/d/A.ZIP"}}},
	}
	for _, tc := range cases {
		plan, ok := ExtractPlan(tc.file, allTools, "/d/"+tc.file, "tok")
		if !ok {
			t.Fatalf("%s: extract plan missing", tc.file)
		}
		assertSteps(t, plan, tc.want)
	}
}

func TestExtractPlanForTarZstUsesTemporaryTarball(t *testing.T) {
	plan, ok := ExtractPlan("archive.tar.zst", allTools, "/d/archive.tar.zst", "tok")
	if !ok {
		t.Fatal("plan missing")
	}
	temporary := TemporaryTarName("tok")
	assertSteps(t, plan, [][]string{
		{"zstd", "-d", "-f", "/d/archive.tar.zst", "-o", temporary},
		{"tar", "-xf", temporary},
	})
	if got := plan.CleanupPaths(); !reflect.DeepEqual(got, []string{temporary}) {
		t.Fatalf("cleanup = %v", got)
	}
}

func TestExtractPlanRequiresMatchingTools(t *testing.T) {
	if _, ok := ExtractPlan("a.tar.zst", Tools{Tar: true}, "/d/a.tar.zst", "tok"); ok {
		t.Fatal("tar.zst without zstd must not be offered")
	}
	if _, ok := ExtractPlan("a.txt.zst", Tools{Tar: true}, "/d/a.txt.zst", "tok"); ok {
		t.Fatal("zst without zstd must not be offered")
	}
	if _, ok := ExtractPlan("a.tar", Tools{Tar: true}, "/d/a.tar", "tok"); !ok {
		t.Fatal("plain tar must be offered with tar")
	}
	if _, ok := ExtractPlan("a.zip", Tools{Zip: true}, "/d/a.zip", "tok"); ok {
		t.Fatal("zip extraction needs unzip, not zip")
	}
	if _, ok := ExtractPlan("a.zip", Tools{Unzip: true}, "/d/a.zip", "tok"); !ok {
		t.Fatal("zip must be offered with unzip")
	}
	if _, ok := ExtractPlan("a.7z", Tools{Zip: true, Unzip: true}, "/d/a.7z", "tok"); ok {
		t.Fatal("7z extraction needs 7z")
	}
	if _, ok := ExtractPlan("a.7z", Tools{SevenZip: true}, "/d/a.7z", "tok"); !ok {
		t.Fatal("7z must be offered with 7z")
	}
	if _, ok := ExtractPlan("notes.txt", allTools, "/d/notes.txt", "tok"); ok {
		t.Fatal("plain text is not an archive")
	}
	if SuffixSupported("notes.txt") {
		t.Fatal("txt must not look like an archive")
	}
	if !SuffixSupported("a.zip") || !SuffixSupported("A.7Z") {
		t.Fatal("zip and 7z must look like archives")
	}
}

func TestCommandLineQuotesEveryArgument(t *testing.T) {
	line := CommandLine([]string{"tar", "-cf", "my archive.tar", "--", "it's.txt"})
	want := `'tar' '-cf' 'my archive.tar' '--' 'it'\''s.txt'`
	if line != want {
		t.Fatalf("CommandLine = %s, want %s", line, want)
	}
	// Metacharacters stay inside the quotes, so the remote shell sees a literal.
	quoted := ShellQuote("a; rm -rf /")
	if !strings.HasPrefix(quoted, "'") || !strings.HasSuffix(quoted, "'") || !strings.Contains(quoted, ";") {
		t.Fatalf("ShellQuote = %s", quoted)
	}
}

func assertSteps(t *testing.T, plan Plan, want [][]string) {
	t.Helper()
	got := make([][]string, 0, len(plan.Steps))
	for _, step := range plan.Steps {
		got = append(got, step.Argv)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("steps = %v, want %v", got, want)
	}
}
