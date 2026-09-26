package proxy

import (
	"bufio"
	"bytes"
	"strings"
)

// sseEvent is one server-sent event, kept byte-for-byte so untouched events pass through exactly.
type sseEvent struct {
	raw   []byte
	event string
	data  string
}

// readSSE reads one event (terminated by a blank line). At EOF it returns any partial event and the error.
func readSSE(br *bufio.Reader) (*sseEvent, error) {
	var raw bytes.Buffer
	var data []string
	ev := &sseEvent{}
	for {
		line, err := br.ReadBytes('\n')
		raw.Write(line)
		trimmed := strings.TrimRight(string(line), "\r\n")
		if trimmed == "" && len(line) > 0 {
			if raw.Len() == len(line) { // skip stray blank lines between events
				raw.Reset()
				if err != nil {
					return nil, err
				}
				continue
			}
			ev.raw, ev.data = raw.Bytes(), strings.Join(data, "\n")
			return ev, nil
		}
		if field, value, ok := strings.Cut(trimmed, ":"); ok && field != "" {
			value = strings.TrimPrefix(value, " ")
			switch field {
			case "data":
				data = append(data, value)
			case "event":
				ev.event = value
			}
		} else if trimmed == "data" {
			data = append(data, "")
		}
		if err != nil {
			if raw.Len() > 0 {
				ev.raw, ev.data = raw.Bytes(), strings.Join(data, "\n")
				return ev, err
			}
			return nil, err
		}
	}
}

// withData rebuilds the event with new data, keeping its other fields.
func (e *sseEvent) withData(data []byte) []byte {
	var b bytes.Buffer
	for _, line := range strings.Split(strings.TrimRight(string(e.raw), "\r\n"), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "data" || strings.HasPrefix(line, "data:") {
			continue
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	b.WriteString("data: ")
	b.Write(bytes.ReplaceAll(data, []byte("\n"), []byte("\ndata: ")))
	b.WriteString("\n\n")
	return b.Bytes()
}

func sseMessage(data []byte) []byte {
	return append(append([]byte("event: message\ndata: "), data...), '\n', '\n')
}
