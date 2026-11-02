package archive

import (
	"reflect"
	"strings"
	"testing"
)

var allTools = Tools{Tar: true, Zstd: true, Gzip: true, Xz: true}

func TestFormatsFollowInstalledTools(t *testing.T) {
	cases := []struct {
		name  string
		tools Tools
		want  []string
	}{
		{"all", allTools, []string{FormatTarZst, FormatTarGz, FormatTarXz, FormatTar}},
		{"tar only", Tools{Tar: true}, []string{FormatTar}},
		{"tar and zstd", Tools{Tar: true, Zstd: true}, []string{FormatTarZst, FormatTar}},
		{"no tar", Tools{Zstd: true, Gzip: true, Xz: true}, []string{}},
	}
	for _, tc := range cases {
		if got := Formats(tc.tools); !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("%s: Formats = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestNextArchiveNameAppendsNewWhileTaken(t *testing.T) {
	taken := map[string]bool{}
	name, err := NextArchiveName(FormatTarGz, func(candidate string) bool { return taken[candidate] })
	if err != nil || name != "archive.tar.gz" {
		t.Fatalf("first name = %q, %v", name, err)
	}

	taken["archive.tar.gz"] = true
	name, _ = NextArchiveName(FormatTarGz, func(candidate string) bool { return taken[candidate] })
	if name != "archive_new.tar.gz" {
		t.Fatalf("second name = %q", name)
	}

	taken["archive_new.tar.gz"] = true
	name, _ = NextArchiveName(FormatTarGz, func(candidate string) bool { return taken[candidate] })
	if name != "archive_new_new.tar.gz" {
		t.Fatalf("third name = %q", name)
	}

	if _, err := NextArchiveName("zip", func(string) bool { return false }); err == nil {
		t.Fatal("unknown format must fail")
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
		{"A.TAR.GZ", [][]string{{"tar", "-xzf", "/d/A.TAR.GZ"}}},
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
	if _, ok := ExtractPlan("notes.txt", allTools, "/d/notes.txt", "tok"); ok {
		t.Fatal("plain text is not an archive")
	}
	if SuffixSupported("notes.txt") {
		t.Fatal("txt must not look like an archive")
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
