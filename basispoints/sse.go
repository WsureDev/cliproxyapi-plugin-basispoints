package basispoints

import (
	"bytes"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type sseSplitter struct {
	buf []byte
}

func (s *sseSplitter) push(chunk []byte) [][]byte {
	if len(chunk) == 0 {
		return nil
	}
	s.buf = append(s.buf, chunk...)
	return s.drain(false)
}

func (s *sseSplitter) flush() [][]byte {
	return s.drain(true)
}

func (s *sseSplitter) drain(flush bool) [][]byte {
	var events [][]byte
	for {
		event, rest, ok := nextSSEEvent(s.buf)
		if !ok {
			break
		}
		s.buf = rest
		if formatted := formatSSEData(event); len(formatted) > 0 {
			events = append(events, formatted)
		}
	}
	if flush && len(bytes.TrimSpace(s.buf)) > 0 {
		if formatted := formatSSEData(s.buf); len(formatted) > 0 {
			events = append(events, formatted)
		}
		s.buf = nil
	}
	return events
}

func nextSSEEvent(buf []byte) ([]byte, []byte, bool) {
	if idx := bytes.Index(buf, []byte("\r\n\r\n")); idx >= 0 {
		return buf[:idx], buf[idx+4:], true
	}
	if idx := bytes.Index(buf, []byte("\n\n")); idx >= 0 {
		return buf[:idx], buf[idx+2:], true
	}
	return nil, nil, false
}

func formatSSEData(event []byte) []byte {
	data := sseData(event)
	if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
		return nil
	}
	if !jsonObject(data) {
		return nil
	}
	out := make([]byte, 0, len(data)+8)
	out = append(out, "data: "...)
	out = append(out, data...)
	out = append(out, "\n\n"...)
	return out
}

func sseData(event []byte) []byte {
	event = bytes.TrimSpace(event)
	if len(event) == 0 {
		return nil
	}
	if jsonObject(event) {
		return event
	}
	var data []byte
	found := false
	for _, line := range bytes.Split(event, []byte("\n")) {
		line = bytes.TrimRight(line, "\r")
		if !bytes.HasPrefix(bytes.ToLower(line), []byte("data:")) {
			continue
		}
		piece := bytes.TrimSpace(line[len("data:"):])
		if found {
			data = append(data, '\n')
		}
		data = append(data, piece...)
		found = true
	}
	return bytes.TrimSpace(data)
}

func jsonObject(raw []byte) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) > 0 && raw[0] == '{'
}

func completedEvent(raw []byte) []byte {
	events := collectEvents(raw)
	var last []byte
	for _, event := range events {
		data := sseData(event)
		if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
			continue
		}
		kind := gjson.GetBytes(data, "type").String()
		if kind == "response.completed" || kind == "response.incomplete" {
			return append([]byte(nil), data...)
		}
		last = data
	}
	if len(last) == 0 {
		last = bytes.TrimSpace(raw)
	}
	return wrapCompleted(last)
}

func collectEvents(raw []byte) [][]byte {
	raw = bytes.ReplaceAll(raw, []byte("\r\n"), []byte("\n"))
	if !bytes.Contains(raw, []byte("\n\n")) && !bytes.Contains(raw, []byte("data:")) {
		return [][]byte{raw}
	}
	parts := bytes.Split(raw, []byte("\n\n"))
	events := make([][]byte, 0, len(parts))
	for _, part := range parts {
		if len(bytes.TrimSpace(part)) > 0 {
			events = append(events, part)
		}
	}
	return events
}

func wrapCompleted(raw []byte) []byte {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || !jsonObject(raw) {
		return raw
	}
	kind := gjson.GetBytes(raw, "type").String()
	if kind == "response.completed" || kind == "response.incomplete" {
		return raw
	}
	if !gjson.GetBytes(raw, "output").Exists() && !gjson.GetBytes(raw, "id").Exists() {
		return raw
	}
	wrapped, errSet := sjson.SetRawBytes([]byte(`{"type":"response.completed"}`), "response", raw)
	if errSet != nil {
		return raw
	}
	return wrapped
}
