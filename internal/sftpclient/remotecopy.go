package sftpclient

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// remoteCopyTimeout bounds the whole command. A server-side copy can move an
// arbitrarily large tree, so it is not interrupted by the default limit; the SSH
// transport dropping is still reported.
const remoteCopyTimeout = 0

// remoteCopyProbeTimeout bounds the capability probe, which only creates a
// temporary directory on the remote host.
const remoteCopyProbeTimeout = 30 * time.Second

// remoteCopyResultPrefix marks a result line so unrelated command output can be
// ignored.
const remoteCopyResultPrefix = "CHEMSSH_COPY"

// remoteCopyProbeScript checks whether the account provides a `cp` that accepts
// `-T`, which is what makes a server-side copy overwrite in place instead of
// nesting the source inside an existing target directory. Ownership is
// deliberately not preserved: a copy as an ordinary user cannot chown, and that
// would turn a successful copy into a reported failure. The check is
// behavioural, so localized error text never has to be parsed.
const remoteCopyProbeScript = `__d=$(mktemp -d 2>/dev/null)
if [ -z "$__d" ]; then
  printf 'CHEMSSH_COPY_PROBE\tunsupported\n'
  exit 0
fi
mkdir -p "$__d/src" "$__d/dst" 2>/dev/null
: > "$__d/src/chemssh-probe" 2>/dev/null
if cp -R -T --preserve=mode,timestamps -- "$__d/src" "$__d/dst" >/dev/null 2>&1 && [ -f "$__d/dst/chemssh-probe" ]; then
  printf 'CHEMSSH_COPY_PROBE\tsupported\n'
else
  printf 'CHEMSSH_COPY_PROBE\tunsupported\n'
fi
rm -rf -- "$__d"
`

// remoteCopyScript copies each source/destination pair that follows the script in
// the argument list. Paths arrive as positional parameters, so the shell never
// parses a file name, and one invocation copies every item instead of paying a
// round trip per file.
const remoteCopyScript = `__idx=0
while [ "$#" -ge 2 ]; do
  __src=$1
  __dst=$2
  shift 2
  __idx=$((__idx+1))
  __err=$(cp -R -T --preserve=mode,timestamps -- "$__src" "$__dst" 2>&1)
  __code=$?
  __err=$(printf '%s' "$__err" | tr '\n\t\r' '   ' | cut -c1-300)
  printf 'CHEMSSH_COPY\t%s\t%s\t%s\n' "$__idx" "$__code" "$__err"
done
`

// RemoteCopyItem is one server-side copy request.
type RemoteCopyItem struct {
	SourcePath string
	TargetPath string
}

// RemoteCopyResult reports the outcome of a single item.
type RemoteCopyResult struct {
	SourcePath string `json:"source_path"`
	TargetPath string `json:"target_path"`
	OK         bool   `json:"ok"`
	Error      string `json:"error,omitempty"`
}

// RemoteCopySupported reports whether a session can copy files on the server. The
// answer is probed once per session and cached, because it depends only on the
// remote account.
func (m *Manager) RemoteCopySupported(id string) (bool, error) {
	session, err := m.get(id)
	if err != nil {
		return false, err
	}
	session.remoteCopyMu.Lock()
	if session.remoteCopyDone {
		ok := session.remoteCopyOK
		session.remoteCopyMu.Unlock()
		return ok, nil
	}
	session.remoteCopyMu.Unlock()

	output, err := m.RunCommandTimeout(id, "", []string{"sh", "-c", remoteCopyProbeScript}, remoteCopyProbeTimeout)
	if err != nil {
		return false, err
	}
	ok := strings.Contains(output, "CHEMSSH_COPY_PROBE\tsupported")

	session.remoteCopyMu.Lock()
	session.remoteCopyDone = true
	session.remoteCopyOK = ok
	session.remoteCopyMu.Unlock()
	return ok, nil
}

// CopyOnServer copies every item inside the remote host, so the bytes never
// travel through this process. Unlike the streamed copy, a symbolic link is
// reproduced as a link instead of being followed, which is what a user expects
// from a copy made on the server.
func (m *Manager) CopyOnServer(id string, items []RemoteCopyItem) ([]RemoteCopyResult, error) {
	if len(items) == 0 {
		return nil, nil
	}
	output, err := m.RunCommandTimeout(id, "", remoteCopyArgv(items), remoteCopyTimeout)
	if err != nil {
		return nil, err
	}
	return parseRemoteCopyOutput(output, items), nil
}

// remoteCopyArgv builds the command where every path travels as its own
// argument, so RunCommand's quoting is the only place a path can be escaped.
func remoteCopyArgv(items []RemoteCopyItem) []string {
	argv := make([]string, 0, 4+len(items)*2)
	argv = append(argv, "sh", "-c", remoteCopyScript, "sh")
	for _, item := range items {
		argv = append(argv, item.SourcePath, item.TargetPath)
	}
	return argv
}

func parseRemoteCopyOutput(output string, items []RemoteCopyItem) []RemoteCopyResult {
	results := make([]RemoteCopyResult, len(items))
	for i, item := range items {
		results[i] = RemoteCopyResult{
			SourcePath: item.SourcePath,
			TargetPath: item.TargetPath,
			Error:      "the copy reported no result",
		}
	}
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Split(strings.TrimRight(line, "\r"), "\t")
		if len(fields) < 3 || fields[0] != remoteCopyResultPrefix {
			continue
		}
		index, err := strconv.Atoi(fields[1])
		if err != nil || index < 1 || index > len(items) {
			continue
		}
		code, err := strconv.Atoi(fields[2])
		if err != nil {
			continue
		}
		result := &results[index-1]
		if code == 0 {
			result.OK = true
			result.Error = ""
			continue
		}
		message := ""
		if len(fields) > 3 {
			message = strings.TrimSpace(strings.Join(fields[3:], " "))
		}
		if message == "" {
			message = fmt.Sprintf("cp exited with status %d", code)
		}
		result.Error = message
	}
	return results
}
