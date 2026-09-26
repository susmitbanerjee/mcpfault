package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/mcpfault/mcpfault/internal/engine"
)

// HTTP is a reverse proxy in front of a remote MCP server. It understands both
// Streamable HTTP (responses in the POST reply, as JSON or SSE) and the legacy
// HTTP+SSE transport (responses on a separate GET event stream).
type HTTP struct {
	Name     string
	Upstream *url.URL
	Listen   string
	// Addr is the address actually bound after Start (useful with port 0).
	Addr  string
	Hooks Hooks
	// Released, if set, returns a channel that closes when hung requests should end.
	Released func() <-chan struct{}
	// OnFailure, if set, records transport errors reaching the server.
	OnFailure func(callID int64, text string)

	client   *http.Client
	srv      *http.Server
	mu       sync.Mutex
	sessions map[string]*legacySession
}

type legacySession struct {
	inject  chan []byte
	kill    chan struct{}
	once    sync.Once
	mu      sync.Mutex
	pending map[string]int64
	lists   map[string]bool
}

func (s *legacySession) close() { s.once.Do(func() { close(s.kill) }) }

func (s *legacySession) send(b []byte) {
	select {
	case s.inject <- b:
	case <-s.kill:
	case <-time.After(10 * time.Second):
	}
}

// intercept marks a forwarded request whose response the proxy must inspect.
type intercept struct {
	id     string
	callID int64
	list   bool
}

// Start listens on p.Listen and serves until Close.
func (p *HTTP) Start() error {
	p.client = &http.Client{Transport: &http.Transport{Proxy: nil, MaxIdleConnsPerHost: 32}}
	p.sessions = map[string]*legacySession{}
	ln, err := net.Listen("tcp", p.Listen)
	if err != nil {
		return fmt.Errorf("server %s: cannot listen on %s: %w", p.Name, p.Listen, err)
	}
	p.Addr = ln.Addr().String()
	p.srv = &http.Server{Handler: p, ReadHeaderTimeout: 30 * time.Second}
	go p.srv.Serve(ln)
	return nil
}

// Close stops the proxy.
func (p *HTTP) Close() {
	if p.srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if p.srv.Shutdown(ctx) != nil {
			p.srv.Close()
		}
	}
}

func (p *HTTP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.Hooks.Hello(p.Name)
	switch r.Method {
	case http.MethodGet:
		p.serveGet(w, r)
	case http.MethodPost:
		p.servePost(w, r)
	default:
		p.forward(w, r, nil, nil)
	}
}

func (p *HTTP) servePost(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "mcpfault: reading request: "+err.Error(), http.StatusBadRequest)
		return
	}
	p.mu.Lock()
	sess := p.sessions[r.URL.RequestURI()]
	p.mu.Unlock()
	m, ok := parseMessage(body)
	if sess != nil {
		p.legacyPost(w, r, body, m, ok, sess)
		return
	}
	if !ok || !m.isRequest() {
		p.forward(w, r, body, nil)
		return
	}
	switch m.Method {
	case "tools/list":
		p.forward(w, r, body, &intercept{id: m.idKey(), list: true})
	case "tools/call":
		d, ok := p.Hooks.Call(p.Name, m.ID, m.Params.Name, m.Params.Arguments)
		if !ok {
			p.forward(w, r, body, nil)
			return
		}
		switch d.Action {
		case engine.ActRespond:
			if !sleep(r.Context(), d.Delay()) {
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write(d.Message)
		case engine.ActHang:
			p.hang(r)
		case engine.ActDisconnect:
			panic(http.ErrAbortHandler)
		default:
			p.forward(w, r, body, &intercept{id: m.idKey(), callID: d.CallID})
		}
	default:
		p.forward(w, r, body, nil)
	}
}

// legacyPost handles a POST to a legacy SSE session endpoint. Responses travel on the
// session's GET stream, so injected answers are pushed there.
func (p *HTTP) legacyPost(w http.ResponseWriter, r *http.Request, body []byte, m *message, ok bool, sess *legacySession) {
	if ok && m.isRequest() && m.Method == "tools/list" {
		sess.mu.Lock()
		sess.lists[m.idKey()] = true
		sess.mu.Unlock()
	}
	if ok && m.isRequest() && m.Method == "tools/call" {
		if d, ok := p.Hooks.Call(p.Name, m.ID, m.Params.Name, m.Params.Arguments); ok {
			switch d.Action {
			case engine.ActRespond:
				msg := sseMessage(d.Message)
				time.AfterFunc(d.Delay(), func() { sess.send(msg) })
				w.WriteHeader(http.StatusAccepted)
				return
			case engine.ActHang:
				w.WriteHeader(http.StatusAccepted)
				return
			case engine.ActDisconnect:
				w.WriteHeader(http.StatusAccepted)
				sess.close()
				return
			default:
				sess.mu.Lock()
				sess.pending[m.idKey()] = d.CallID
				sess.mu.Unlock()
			}
		}
	}
	p.forward(w, r, body, nil)
}

// forward sends the request upstream and relays the response, inspecting it if ic is set.
func (p *HTTP) forward(w http.ResponseWriter, r *http.Request, body []byte, ic *intercept) {
	resp, err := p.do(r, body)
	if err != nil {
		if ic != nil && ic.callID != 0 && p.OnFailure != nil {
			p.OnFailure(ic.callID, err.Error())
		}
		http.Error(w, "mcpfault: upstream unreachable: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	ct := resp.Header.Get("Content-Type")
	if ic != nil && ic.callID != 0 && resp.StatusCode >= 400 && p.OnFailure != nil {
		p.OnFailure(ic.callID, fmt.Sprintf("upstream HTTP %d", resp.StatusCode))
	}

	switch {
	case ic != nil && resp.StatusCode == http.StatusOK && strings.HasPrefix(ct, "application/json"):
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			panic(http.ErrAbortHandler)
		}
		out, action, delay := p.inspect(data, ic)
		switch action {
		case engine.ActWithhold:
			p.hang(r)
			return
		case engine.ActDisconnect:
			panic(http.ErrAbortHandler)
		}
		if !sleep(r.Context(), delay) {
			return
		}
		copyHeaders(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		w.Write(out)
	case ic != nil && resp.StatusCode == http.StatusOK && strings.HasPrefix(ct, "text/event-stream"):
		copyHeaders(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		flush(w)
		br := bufio.NewReader(resp.Body)
		withheld := false
		for {
			ev, err := readSSE(br)
			if ev != nil {
				out, action, delay := p.inspect([]byte(ev.data), ic)
				switch {
				case !bytes.Equal(out, []byte(ev.data)) || action != engine.ActDeliver || delay > 0:
					switch action {
					case engine.ActWithhold:
						withheld = true
					case engine.ActDisconnect:
						panic(http.ErrAbortHandler)
					default:
						if !sleep(r.Context(), delay) {
							return
						}
						w.Write(ev.withData(out))
					}
				default:
					w.Write(ev.raw)
				}
				flush(w)
			}
			if err != nil {
				break
			}
		}
		if withheld {
			p.hang(r)
		}
	default:
		copyHeaders(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		streamCopy(w, resp.Body)
	}
}

// inspect applies the engine to a JSON-RPC message if it answers the intercepted request.
func (p *HTTP) inspect(data []byte, ic *intercept) ([]byte, string, time.Duration) {
	m, ok := parseMessage(data)
	if !ok || !m.isResponse() || m.idKey() != ic.id {
		return data, engine.ActDeliver, 0
	}
	if ic.list {
		if len(m.Result) > 0 {
			p.Hooks.Tools(p.Name, append(json.RawMessage{}, m.Result...))
		}
		return data, engine.ActDeliver, 0
	}
	d, ok := p.Hooks.Result(ic.callID, append(json.RawMessage{}, data...))
	if !ok {
		return data, engine.ActDeliver, 0
	}
	return d.Message, d.Action, d.Delay()
}

// serveGet relays event streams. For legacy SSE it also registers the session so
// POSTs to its endpoint can be matched with responses on this stream.
func (p *HTTP) serveGet(w http.ResponseWriter, r *http.Request) {
	resp, err := p.do(r, nil)
	if err != nil {
		http.Error(w, "mcpfault: upstream unreachable: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		streamCopy(w, resp.Body)
		return
	}
	flush(w)

	events := make(chan *sseEvent, 16)
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		defer close(events)
		br := bufio.NewReader(resp.Body)
		for {
			ev, err := readSSE(br)
			if ev != nil {
				select {
				case events <- ev:
				case <-stop:
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	var sess *legacySession
	for {
		var inject chan []byte
		var kill chan struct{}
		if sess != nil {
			inject, kill = sess.inject, sess.kill
		}
		select {
		case ev, ok := <-events:
			if !ok {
				return
			}
			switch {
			case ev.event == "endpoint" && sess == nil:
				endpoint, err := resp.Request.URL.Parse(strings.TrimSpace(ev.data))
				if err != nil {
					w.Write(ev.raw)
					break
				}
				sess = &legacySession{inject: make(chan []byte, 16), kill: make(chan struct{}), pending: map[string]int64{}, lists: map[string]bool{}}
				key := endpoint.RequestURI()
				p.mu.Lock()
				p.sessions[key] = sess
				p.mu.Unlock()
				defer func() {
					p.mu.Lock()
					delete(p.sessions, key)
					p.mu.Unlock()
					sess.close()
				}()
				data := strings.TrimSpace(ev.data)
				if strings.HasPrefix(data, "http://") || strings.HasPrefix(data, "https://") {
					data = "http://" + r.Host + key
				}
				w.Write(ev.withData([]byte(data)))
			case sess != nil:
				p.legacyEvent(w, ev, sess)
			default:
				w.Write(ev.raw)
			}
		case b := <-inject:
			w.Write(b)
		case <-kill:
			panic(http.ErrAbortHandler)
		case <-r.Context().Done():
			return
		}
		flush(w)
	}
}

func (p *HTTP) legacyEvent(w http.ResponseWriter, ev *sseEvent, sess *legacySession) {
	m, ok := parseMessage([]byte(ev.data))
	if !ok || !m.isResponse() {
		w.Write(ev.raw)
		return
	}
	key := m.idKey()
	sess.mu.Lock()
	isList := sess.lists[key]
	delete(sess.lists, key)
	callID, isCall := sess.pending[key]
	delete(sess.pending, key)
	sess.mu.Unlock()
	if isList {
		p.inspect([]byte(ev.data), &intercept{id: key, list: true})
	}
	if !isCall {
		w.Write(ev.raw)
		return
	}
	out, action, delay := p.inspect([]byte(ev.data), &intercept{id: key, callID: callID})
	switch action {
	case engine.ActWithhold:
	case engine.ActDisconnect:
		sess.close()
	default:
		b := ev.withData(out)
		if delay > 0 {
			time.AfterFunc(delay, func() { sess.send(b) })
		} else {
			w.Write(b)
		}
	}
}

func (p *HTTP) do(r *http.Request, body []byte) (*http.Response, error) {
	target := *p.Upstream
	target.Path, target.RawPath, target.RawQuery = r.URL.Path, r.URL.RawPath, r.URL.RawQuery
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	} else if r.Method != http.MethodGet && r.Method != http.MethodHead {
		rd = r.Body
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), rd)
	if err != nil {
		return nil, err
	}
	copyHeaders(req.Header, r.Header)
	req.Header.Del("Accept-Encoding") // let the transport negotiate and decompress so bodies can be inspected
	return p.client.Do(req)
}

func (p *HTTP) hang(r *http.Request) {
	var released <-chan struct{}
	if p.Released != nil {
		released = p.Released()
	}
	select {
	case <-r.Context().Done():
	case <-released:
		panic(http.ErrAbortHandler)
	}
}

var hopHeaders = map[string]bool{
	"Connection": true, "Proxy-Connection": true, "Keep-Alive": true, "Proxy-Authenticate": true,
	"Proxy-Authorization": true, "Te": true, "Trailer": true, "Transfer-Encoding": true, "Upgrade": true,
	"Content-Length": true, "Host": true,
}

func copyHeaders(dst, src http.Header) {
	for k, vs := range src {
		if hopHeaders[http.CanonicalHeaderKey(k)] {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

func flush(w http.ResponseWriter) {
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func streamCopy(w http.ResponseWriter, r io.Reader) {
	buf := make([]byte, 32*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			w.Write(buf[:n])
			flush(w)
		}
		if err != nil {
			return
		}
	}
}

func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	select {
	case <-time.After(d):
		return true
	case <-ctx.Done():
		return false
	}
}
