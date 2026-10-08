package runner

import (
	"io"
	"os"
	"regexp"
	"strings"
)

var ansiSequence = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)

func output(log *os.File, raw io.Writer, headless bool) io.Writer {
	if !headless || sameOutput(log, raw) {
		return log
	}
	return io.MultiWriter(log, raw)
}

func sameOutput(log *os.File, raw io.Writer) bool {
	file, ok := raw.(*os.File)
	if !ok {
		return false
	}
	logInfo, logErr := log.Stat()
	fileInfo, fileErr := file.Stat()
	return logErr == nil && fileErr == nil && os.SameFile(logInfo, fileInfo)
}

func Text(text string) string {
	text = ansiSequence.ReplaceAllString(text, "")
	var out strings.Builder
	for _, char := range text {
		switch char {
		case '\n', '\t':
			out.WriteRune(char)
		default:
			if char < 0x20 || char == 0x7f {
				out.WriteByte(' ')
			} else {
				out.WriteRune(char)
			}
		}
	}
	return out.String()
}

func readableAssistantText(text string) string {
	const open = "<shephrd-event>"
	const close = "</shephrd-event>"
	start := strings.Index(text, open)
	if start < 0 {
		return text
	}
	end := strings.Index(text[start+len(open):], close)
	if end < 0 {
		return text[:start]
	}
	end += start + len(open)
	remaining := strings.TrimSpace(text[end+len(close):])
	if prefix := strings.TrimSpace(text[:start]); prefix != "" {
		if remaining != "" {
			return prefix + "\n" + remaining
		}
		return prefix
	}
	return remaining
}
