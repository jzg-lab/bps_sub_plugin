package tracelog

import (
	"bytes"
	"encoding/json"
	"strings"
)

const maxDiagnosticEvent = 256 << 10
const maxErrorSignals = 8

// Observer inspects only bytes already consumed by the caller. It never drains
// a response and bounds each event independently so late errors remain visible.
type Observer struct {
	status                int
	sse, html             bool
	line, data            []byte
	eventName             string
	discard, lineNonEmpty bool
	result                Observation
}

func NewObserver(status int, contentType string) *Observer {
	return &Observer{status: status, sse: strings.Contains(strings.ToLower(contentType), "text/event-stream"), html: strings.Contains(strings.ToLower(contentType), "text/html")}
}

func (o *Observer) Feed(p []byte) {
	if !o.sse {
		if o.discard {
			return
		}
		if len(o.data)+len(p) > maxDiagnosticEvent {
			o.data = nil
			o.discard = true
			o.result.Oversized++
			return
		}
		o.data = append(o.data, p...)
		return
	}
	for len(p) > 0 {
		end := bytes.IndexByte(p, 10)
		part := p
		if end >= 0 {
			part = p[:end]
		}
		if len(bytes.Trim(part, string([]byte{13}))) > 0 {
			o.lineNonEmpty = true
		}
		if !o.discard {
			if len(o.line)+len(o.data)+len(part) > maxDiagnosticEvent {
				o.discard = true
				o.line = nil
				o.data = nil
				o.result.Oversized++
			} else {
				o.line = append(o.line, part...)
			}
		}
		if end < 0 {
			break
		}
		if !o.discard {
			line := bytes.TrimSuffix(o.line, []byte{13})
			if len(line) == 0 {
				o.flush()
			} else if bytes.HasPrefix(line, []byte("data:")) {
				field := bytes.TrimPrefix(line[5:], []byte(" "))
				o.data = append(o.data, field...)
				o.data = append(o.data, 10)
			} else if bytes.HasPrefix(line, []byte("event:")) {
				o.eventName = identifier(strings.TrimSpace(string(line[6:])))
			}
		} else if !o.lineNonEmpty {
			o.discard = false
			o.eventName = ""
		}
		o.line = nil
		o.lineNonEmpty = false
		p = p[end+1:]
	}
}

func (o *Observer) flush() {
	defer func() { o.eventName = "" }()
	data := bytes.TrimSpace(o.data)
	o.data = nil
	if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
		return
	}
	o.result.Events++
	o.parse(data)
}

type responseMetadata struct {
	ID         string          `json:"id"`
	Model      string          `json:"model"`
	Status     string          `json:"status"`
	Error      json.RawMessage `json:"error"`
	Incomplete json.RawMessage `json:"incomplete_details"`
	Usage      *struct {
		Input        *int64 `json:"input_tokens"`
		Output       *int64 `json:"output_tokens"`
		InputDetails struct {
			Cached *int64 `json:"cached_tokens"`
		} `json:"input_tokens_details"`
		OutputDetails struct {
			Reasoning *int64 `json:"reasoning_tokens"`
		} `json:"output_tokens_details"`
	} `json:"usage"`
}

func (o *Observer) parse(data []byte) {
	var event struct {
		Type     string           `json:"type"`
		Error    json.RawMessage  `json:"error"`
		Response responseMetadata `json:"response"`
		Status   json.RawMessage  `json:"status"`
		Headers  json.RawMessage  `json:"headers"`
	}
	if json.Unmarshal(data, &event) != nil {
		o.result.Invalid++
		return
	}
	if !o.sse {
		if o.status >= 400 || present(event.Error) {
			raw := event.Error
			if !present(raw) {
				raw = data
			}
			o.add(signalFor(raw, "http_body", o.status))
			return
		}
		_ = json.Unmarshal(data, &event.Response)
		switch event.Response.Status {
		case "completed", "failed", "incomplete":
			event.Type = "response." + event.Response.Status
		}
	}
	if event.Type == "" {
		event.Type = o.eventName
	}
	r := event.Response
	if r.ID != "" {
		o.result.ResponseID = Fingerprint("response", r.ID)
	}
	if r.Model != "" {
		o.result.ResponseModel = identifier(r.Model)
	}
	if r.Usage != nil {
		o.result.Usage = &Usage{Input: r.Usage.Input, Output: r.Usage.Output, Cached: r.Usage.InputDetails.Cached, Reasoning: r.Usage.OutputDetails.Reasoning}
	}
	switch event.Type {
	case "response.completed", "response.failed", "response.incomplete":
		o.result.Terminal = event.Type
	}
	status := o.status
	var embeddedStatus int
	if json.Unmarshal(event.Status, &embeddedStatus) == nil && embeddedStatus >= 400 && embeddedStatus <= 599 {
		status = embeddedStatus
	}
	addError := func(raw json.RawMessage, source string) {
		signal := signalFor(raw, source, status)
		for key, value := range safeErrorHeaders(event.Headers) {
			if signal.Headers == nil {
				signal.Headers = make(map[string]string)
			}
			signal.Headers[key] = value
		}
		o.add(signal)
	}
	if present(r.Error) {
		addError(r.Error, event.Type)
	}
	if present(event.Error) {
		addError(event.Error, event.Type)
	} else if event.Type == "error" {
		addError(data, "error")
	}
	if event.Type == "response.incomplete" {
		o.add(signalFor(r.Incomplete, event.Type, o.status))
	}
	if event.Type == "response.failed" && !present(r.Error) && !present(event.Error) {
		o.add(signalFor(nil, event.Type, o.status))
	}
}

func present(raw json.RawMessage) bool {
	return len(raw) > 0 && !bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func (o *Observer) add(s ErrorSignal) {
	for i := range o.result.Signals {
		old := &o.result.Signals[i]
		if old.Class == s.Class && old.Code == s.Code && old.MessageHash == s.MessageHash {
			if old.Status == 0 {
				old.Status = s.Status
			}
			for key, value := range s.Headers {
				if old.Headers == nil {
					old.Headers = make(map[string]string)
				}
				old.Headers[key] = value
			}
			return
		}
	}
	if len(o.result.Signals) >= maxErrorSignals {
		o.result.SignalsDropped++
		return
	}
	o.result.Signals = append(o.result.Signals, s)
}

func (o *Observer) Finish() Observation {
	if o.sse {
		if len(o.line) > 0 || len(o.data) > 0 {
			o.result.Invalid++
		}
	} else if !o.discard && len(o.data) > 0 {
		o.parse(o.data)
	}
	if o.status >= 400 && len(o.result.Signals) == 0 {
		s := signalFor(nil, "http_status", o.status)
		if o.status == 403 && o.html {
			s.Class = "forbidden_html"
		}
		o.add(s)
	}
	o.line = nil
	o.data = nil
	return o.result
}
