package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/mcpfault/mcpfault/internal/engine"
)

// Stdio relays newline-delimited JSON-RPC between the agent (Stdin/Stdout) and a
// real MCP server started as a child process, applying the engine's decisions.
type Stdio struct {
	Server  string
	Command []string
	Hooks   Hooks
	Stdin   io.Reader
	Stdout  io.Writer
	Stderr  io.Writer
	// Exit is called to drop the connection for disconnect faults. Defaults to os.Exit.
	Exit func(code int)

	outMu   sync.Mutex
	inMu    sync.Mutex
	mu      sync.Mutex
	pending map[string]int64
	lists   map[string]bool
	child   *exec.Cmd
	stdin   io.WriteCloser
}

// Run starts the server and relays until the server exits. It returns the server's exit code.
func (s *Stdio) Run() (int, error) {
	if len(s.Command) == 0 {
		return 1, errors.New("no server command given")
	}
	if s.Exit == nil {
		s.Exit = os.Exit
	}
	s.pending = map[string]int64{}
	s.lists = map[string]bool{}

	s.child = exec.Command(s.Command[0], s.Command[1:]...)
	s.child.Stderr = s.Stderr
	var err error
	if s.stdin, err = s.child.StdinPipe(); err != nil {
		return 1, err
	}
	stdout, err := s.child.StdoutPipe()
	if err != nil {
		return 1, err
	}
	if err := s.child.Start(); err != nil {
		return 1, err
	}
	go s.Hooks.Hello(s.Server)

	go func() {
		s.readLines(s.Stdin, s.fromAgent)
		s.inMu.Lock()
		s.stdin.Close()
		s.inMu.Unlock()
	}()
	done := make(chan struct{})
	go func() {
		s.readLines(stdout, s.fromServer)
		close(done)
	}()
	<-done
	err = s.child.Wait()
	if s.child.ProcessState != nil {
		return s.child.ProcessState.ExitCode(), nil
	}
	return 1, err
}

func (s *Stdio) readLines(r io.Reader, handle func([]byte)) {
	br := bufio.NewReaderSize(r, 1<<16)
	for {
		line, err := br.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			handle(bytes.TrimRight(line, "\r\n"))
		}
		if err != nil {
			return
		}
	}
}

func (s *Stdio) toAgent(b []byte) {
	s.outMu.Lock()
	defer s.outMu.Unlock()
	s.Stdout.Write(append(append([]byte{}, b...), '\n'))
}

func (s *Stdio) toServer(b []byte) {
	s.inMu.Lock()
	defer s.inMu.Unlock()
	s.stdin.Write(append(append([]byte{}, b...), '\n'))
}

func (s *Stdio) later(delay time.Duration, fn func()) {
	if delay <= 0 {
		fn()
		return
	}
	time.AfterFunc(delay, fn)
}

func (s *Stdio) disconnect() {
	if s.child.Process != nil {
		s.child.Process.Kill()
	}
	s.Exit(0)
}

func (s *Stdio) fromAgent(line []byte) {
	m, ok := parseMessage(line)
	if !ok || !m.isRequest() {
		s.toServer(line)
		return
	}
	switch m.Method {
	case "tools/list":
		s.mu.Lock()
		s.lists[m.idKey()] = true
		s.mu.Unlock()
		s.toServer(line)
	case "tools/call":
		d, ok := s.Hooks.Call(s.Server, m.ID, m.Params.Name, m.Params.Arguments)
		if !ok {
			s.toServer(line)
			return
		}
		switch d.Action {
		case engine.ActRespond:
			msg := d.Message
			s.later(d.Delay(), func() { s.toAgent(msg) })
		case engine.ActHang:
		case engine.ActDisconnect:
			s.disconnect()
		default:
			s.mu.Lock()
			s.pending[m.idKey()] = d.CallID
			s.mu.Unlock()
			s.toServer(line)
		}
	default:
		s.toServer(line)
	}
}

func (s *Stdio) fromServer(line []byte) {
	m, ok := parseMessage(line)
	if !ok || !m.isResponse() {
		s.toAgent(line)
		return
	}
	key := m.idKey()
	s.mu.Lock()
	isList := s.lists[key]
	delete(s.lists, key)
	callID, isCall := s.pending[key]
	delete(s.pending, key)
	s.mu.Unlock()

	if isList && len(m.Result) > 0 {
		go s.Hooks.Tools(s.Server, append(json.RawMessage{}, m.Result...))
	}
	if !isCall {
		s.toAgent(line)
		return
	}
	d, ok := s.Hooks.Result(callID, append(json.RawMessage{}, line...))
	if !ok {
		s.toAgent(line)
		return
	}
	switch d.Action {
	case engine.ActWithhold:
	case engine.ActDisconnect:
		s.disconnect()
	default:
		msg := d.Message
		s.later(d.Delay(), func() { s.toAgent(msg) })
	}
}
