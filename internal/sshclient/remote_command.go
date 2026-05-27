package sshclient

import (
	"regexp"
	"strconv"
	"strings"

	"chemweb-launcher/internal/config"
)

func AssembleRemoteCommand(profile config.Profile) string {
	var b strings.Builder
	b.WriteString("set -e\n")
	if strings.TrimSpace(profile.PreStartCommands) != "" {
		b.WriteString(strings.TrimRight(profile.PreStartCommands, "\r\n"))
		b.WriteByte('\n')
	}
	b.WriteString("__chemweb_launcher_pid=\n")
	b.WriteString("__chemweb_launcher_cleanup() {\n")
	b.WriteString("  if [ -n \"$__chemweb_launcher_pid\" ]; then\n")
	b.WriteString("    kill -TERM \"$__chemweb_launcher_pid\" 2>/dev/null || true\n")
	b.WriteString("    i=0\n")
	b.WriteString("    while kill -0 \"$__chemweb_launcher_pid\" 2>/dev/null && [ \"$i\" -lt 5 ]; do\n")
	b.WriteString("      i=$((i + 1))\n")
	b.WriteString("      sleep 1\n")
	b.WriteString("    done\n")
	b.WriteString("    kill -KILL \"$__chemweb_launcher_pid\" 2>/dev/null || true\n")
	b.WriteString("    wait \"$__chemweb_launcher_pid\" 2>/dev/null || true\n")
	b.WriteString("  fi\n")
	b.WriteString("}\n")
	b.WriteString("trap '__chemweb_launcher_cleanup; exit 143' INT TERM HUP\n")
	b.WriteString(backgroundLastCommand(EffectiveStartCommand(profile)))
	b.WriteString("__chemweb_launcher_pid=$!\n")
	b.WriteString("set +e\n")
	b.WriteString("wait \"$__chemweb_launcher_pid\"\n")
	b.WriteString("__chemweb_launcher_status=$?\n")
	b.WriteString("set -e\n")
	b.WriteString("trap - INT TERM HUP\n")
	b.WriteString("__chemweb_launcher_cleanup\n")
	b.WriteString("exit \"$__chemweb_launcher_status\"\n")
	return b.String()
}

func AssembleKillPIDCommand(pid int) string {
	pidValue := shellQuote(strconv.Itoa(pid))
	return strings.Join([]string{
		"set +e",
		"__chemweb_launcher_pid=" + pidValue,
		"kill -TERM \"$__chemweb_launcher_pid\" 2>/dev/null || true",
		"i=0",
		"while kill -0 \"$__chemweb_launcher_pid\" 2>/dev/null && [ \"$i\" -lt 5 ]; do",
		"  i=$((i + 1))",
		"  sleep 1",
		"done",
		"kill -KILL \"$__chemweb_launcher_pid\" 2>/dev/null || true",
		"exit 0",
		"",
	}, "\n")
}

func AssembleCheckPortCommand(profile config.Profile) string {
	var b strings.Builder
	b.WriteString("set -e\n")
	if strings.TrimSpace(profile.PreStartCommands) != "" {
		b.WriteString(strings.TrimRight(profile.PreStartCommands, "\r\n"))
		b.WriteByte('\n')
	}
	b.WriteString(EffectiveCheckPortCommand(profile))
	b.WriteByte('\n')
	return b.String()
}

func EffectiveCheckPortCommand(profile config.Profile) string {
	command := EffectiveStartCommand(profile)
	lines := strings.Split(command, "\n")
	last := -1
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			last = i
			break
		}
	}
	if last == -1 {
		return command
	}
	if !hasFlag(lines[last], "check-port") {
		lines[last] += " --check-port"
	}
	return strings.Join(lines, "\n")
}

func EffectiveStartCommand(profile config.Profile) string {
	command := strings.TrimRight(profile.StartCommand, "\r\n")
	if strings.TrimSpace(command) == "" {
		command = config.DefaultStartCommand
	}

	lines := strings.Split(command, "\n")
	last := -1
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			last = i
			break
		}
	}
	if last == -1 {
		return command
	}

	line := lines[last]
	if !hasFlag(line, "host") {
		line += " --host " + shellQuote(profile.RemoteHost)
	}
	if !hasFlag(line, "port") {
		line += " --port " + strconv.Itoa(profile.RemotePort)
	}
	lines[last] = line
	return strings.Join(lines, "\n")
}

func PortWarning(profile config.Profile) string {
	if profile.RemotePort == 0 || profile.StartCommand == "" {
		return ""
	}
	re := regexp.MustCompile(`(?i)(--port[= ]+|:)(\d{2,5})`)
	matches := re.FindAllStringSubmatch(profile.StartCommand, -1)
	for _, match := range matches {
		port, err := strconv.Atoi(match[2])
		if err == nil && port != profile.RemotePort {
			return "start command appears to use port " + match[2] + " while profile remote port is " + strconv.Itoa(profile.RemotePort)
		}
	}
	return ""
}

func hasFlag(command, name string) bool {
	re := regexp.MustCompile(`(^|\s)--` + regexp.QuoteMeta(name) + `($|\s|=)`)
	return re.MatchString(command)
}

func shellQuote(value string) string {
	if value == "" {
		return "''"
	}
	if regexp.MustCompile(`^[A-Za-z0-9_./:-]+$`).MatchString(value) {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func backgroundLastCommand(command string) string {
	lines := strings.Split(strings.TrimRight(command, "\r\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			lines[i] = lines[i] + " &"
			return strings.Join(lines, "\n") + "\n"
		}
	}
	return "true &\n"
}
