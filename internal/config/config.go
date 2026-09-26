// Package config loads and validates mcpfault scenario files.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Scenario is a validated scenario file.
type Scenario struct {
	File        string
	Dir         string
	Name        string
	Description string
	Runs        int
	Timeout     time.Duration
	Pause       time.Duration
	Servers     []*Server
	Services    []*Service
	Agent       *Agent
	Setup       []Command
	Teardown    []Command
	Faults      []*Fault
	Invariants  []*Invariant
}

// Server is an MCP server the agent reaches through mcpfault.
type Server struct {
	Name      string
	Transport string // "stdio" or "http"
	Command   Command
	Env       map[string]string
	URL       string // http: upstream MCP endpoint
	Listen    string // http: where the proxy listens
}

// ProxyURL is the URL an agent should use instead of the upstream URL (http servers only).
func (s *Server) ProxyURL() string {
	if s.Transport != "http" {
		return ""
	}
	u, err := url.Parse(s.URL)
	if err != nil {
		return ""
	}
	u.Scheme, u.Host = "http", s.Listen
	return u.String()
}

// Service is a background process started before the runs and stopped after them.
type Service struct {
	Name         string
	Command      Command
	Env          map[string]string
	Cwd          string
	Ready        string
	ReadyTimeout time.Duration
}

// Agent is how each run is triggered.
type Agent struct {
	Command Command
	HTTP    *HTTPTrigger
	Env     map[string]string
	Cwd     string
}

// HTTPTrigger triggers a run by calling an agent that is already running as a service.
type HTTPTrigger struct {
	URL     string            `yaml:"url"`
	Method  string            `yaml:"method"`
	Headers map[string]string `yaml:"headers"`
	Body    string            `yaml:"body"`
}

// Fault types.
const (
	FaultError        = "error"
	FaultLostResponse = "lost_response"
	FaultDelay        = "delay"
	FaultCorrupt      = "corrupt"
	FaultDisconnect   = "disconnect"
	FaultHang         = "hang"
)

var faultTypes = []string{FaultError, FaultLostResponse, FaultDelay, FaultCorrupt, FaultDisconnect, FaultHang}

// Fault is one injection rule.
type Fault struct {
	Index       int
	Server      string // empty = any server
	Tool        string
	Args        map[string]any
	Call        int // first matching call it applies to (1-based)
	Repeat      int // number of matching calls it covers; 0 = all remaining
	Type        string
	RespondWith string // tool_error | rpc_error | none
	Message     string
	Code        int
	Delay       time.Duration
	Corrupt     *Corrupt
	When        string // disconnect: before | after
	Label       string
}

// Corrupt describes how to edit a tool result before the agent sees it.
type Corrupt struct {
	Set      map[string]any `yaml:"set" json:"set,omitempty"`
	Drop     []string       `yaml:"drop" json:"drop,omitempty"`
	Replace  any            `yaml:"replace" json:"replace,omitempty"`
	Empty    bool           `yaml:"empty" json:"empty,omitempty"`
	Truncate bool           `yaml:"truncate" json:"truncate,omitempty"`
}

// Invariant kinds.
const (
	InvLog    = "log"
	InvCheck  = "check"
	InvOutput = "output"
)

// Invariant is a property that must hold after every run.
type Invariant struct {
	Kind      string
	Name      string
	Severity  string // error | warn
	Server    string
	Tool      string
	Where     map[string]any
	Committed *bool
	Distinct  string
	Max       *int
	Min       *int
	Equals    *int
	Check     Command
	Matches   *regexp.Regexp
	NotMatch  *regexp.Regexp
}

// ---- raw YAML shapes ----

type rawScenario struct {
	Name        string               `yaml:"name"`
	Description string               `yaml:"description"`
	Runs        int                  `yaml:"runs"`
	Timeout     Duration             `yaml:"timeout"`
	Pause       Duration             `yaml:"pause"`
	Servers     map[string]rawServer `yaml:"servers"`
	Services    []rawService         `yaml:"services"`
	Agent       *rawAgent            `yaml:"agent"`
	Setup       []Command            `yaml:"setup"`
	Teardown    []Command            `yaml:"teardown"`
	Faults      []rawFault           `yaml:"faults"`
	Invariants  []rawInvariant       `yaml:"invariants"`
}

type rawServer struct {
	Transport string            `yaml:"transport"`
	Command   Command           `yaml:"command"`
	Env       map[string]string `yaml:"env"`
	URL       string            `yaml:"url"`
	Listen    string            `yaml:"listen"`
}

// UnmarshalYAML accepts a bare command (string or list) as shorthand for a stdio server.
func (s *rawServer) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode || n.Kind == yaml.SequenceNode {
		return n.Decode(&s.Command)
	}
	if n.Kind == yaml.MappingNode {
		allowed := map[string]bool{"transport": true, "command": true, "env": true, "url": true, "listen": true}
		for i := 0; i < len(n.Content); i += 2 {
			if key := n.Content[i].Value; !allowed[key] {
				return fmt.Errorf("line %d: field %s not found in server (expected transport, command, env, url, listen)", n.Content[i].Line, key)
			}
		}
	}
	type plain rawServer
	return n.Decode((*plain)(s))
}

type rawService struct {
	Name         string            `yaml:"name"`
	Command      Command           `yaml:"command"`
	Env          map[string]string `yaml:"env"`
	Cwd          string            `yaml:"cwd"`
	Ready        string            `yaml:"ready"`
	ReadyTimeout Duration          `yaml:"ready_timeout"`
}

type rawAgent struct {
	Command Command           `yaml:"command"`
	HTTP    *HTTPTrigger      `yaml:"http"`
	Env     map[string]string `yaml:"env"`
	Cwd     string            `yaml:"cwd"`
}

type rawFault struct {
	Tool        string         `yaml:"tool"`
	Args        map[string]any `yaml:"args"`
	Call        int            `yaml:"call"`
	Repeat      any            `yaml:"repeat"`
	Type        string         `yaml:"type"`
	RespondWith string         `yaml:"respond_with"`
	Message     string         `yaml:"message"`
	Code        *int           `yaml:"code"`
	Delay       Duration       `yaml:"delay"`
	Corrupt     *Corrupt       `yaml:"corrupt"`
	When        string         `yaml:"when"`
}

type rawInvariant struct {
	Name      string         `yaml:"name"`
	Severity  string         `yaml:"severity"`
	Tool      string         `yaml:"tool"`
	Where     map[string]any `yaml:"where"`
	Committed *bool          `yaml:"committed"`
	Distinct  string         `yaml:"distinct"`
	Max       *int           `yaml:"max"`
	Min       *int           `yaml:"min"`
	Equals    *int           `yaml:"equals"`
	Check     Command        `yaml:"check"`
	Output    *struct {
		Matches    string `yaml:"matches"`
		NotMatches string `yaml:"not_matches"`
	} `yaml:"output"`
}

// DefaultProxyPort is the first port used for HTTP proxies without an explicit listen address.
const DefaultProxyPort = 7362

// Load reads and validates a scenario file.
func Load(file string) (*Scenario, error) {
	abs, err := filepath.Abs(file)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, err
	}
	return Parse(data, abs)
}

// Parse validates scenario YAML. file is used for messages and relative paths.
func Parse(data []byte, file string) (*Scenario, error) {
	var raw rawScenario
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&raw); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s: %w", filepath.Base(file), err)
	}
	fail := func(format string, a ...any) error {
		return fmt.Errorf("%s: %s", filepath.Base(file), fmt.Sprintf(format, a...))
	}

	s := &Scenario{
		File:        file,
		Dir:         filepath.Dir(file),
		Name:        raw.Name,
		Description: strings.TrimSpace(raw.Description),
		Runs:        raw.Runs,
		Timeout:     time.Duration(raw.Timeout),
		Pause:       time.Duration(raw.Pause),
		Setup:       raw.Setup,
		Teardown:    raw.Teardown,
	}
	if s.Name == "" {
		base := filepath.Base(file)
		for _, ext := range []string{".fault.yaml", ".fault.yml", ".yaml", ".yml"} {
			base = strings.TrimSuffix(base, ext)
		}
		s.Name = base
	}
	if s.Runs <= 0 {
		s.Runs = 5
	}
	if s.Timeout <= 0 {
		s.Timeout = 3 * time.Minute
	}

	if len(raw.Servers) == 0 {
		return nil, fail("at least one entry under `servers` is required")
	}
	names := make([]string, 0, len(raw.Servers))
	for name := range raw.Servers {
		names = append(names, name)
	}
	sort.Strings(names)
	nameRe := regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	for i, name := range names {
		rs := raw.Servers[name]
		if !nameRe.MatchString(name) {
			return nil, fail("server name %q may only use letters, digits, - and _", name)
		}
		srv := &Server{Name: name, Transport: rs.Transport, Command: rs.Command, Env: rs.Env, URL: rs.URL, Listen: rs.Listen}
		if srv.Transport == "" {
			if srv.URL != "" {
				srv.Transport = "http"
			} else {
				srv.Transport = "stdio"
			}
		}
		switch srv.Transport {
		case "stdio":
		case "http":
			if !strings.HasPrefix(srv.URL, "http://") && !strings.HasPrefix(srv.URL, "https://") {
				return nil, fail("servers.%s: http servers need a `url` (the real MCP endpoint, e.g. http://localhost:8000/mcp)", name)
			}
			if srv.Listen == "" {
				srv.Listen = fmt.Sprintf("127.0.0.1:%d", DefaultProxyPort+i)
			}
		default:
			return nil, fail("servers.%s: transport must be stdio or http", name)
		}
		s.Servers = append(s.Servers, srv)
	}

	for i, rs := range raw.Services {
		if rs.Command.IsZero() {
			return nil, fail("services[%d]: `command` is required", i)
		}
		svc := &Service{Name: rs.Name, Command: rs.Command, Env: rs.Env, Cwd: rs.Cwd, Ready: rs.Ready, ReadyTimeout: time.Duration(rs.ReadyTimeout)}
		if svc.Name == "" {
			svc.Name = fmt.Sprintf("service-%d", i+1)
		}
		if svc.ReadyTimeout <= 0 {
			svc.ReadyTimeout = 2 * time.Minute
		}
		s.Services = append(s.Services, svc)
	}

	if raw.Agent != nil {
		a := &Agent{Command: raw.Agent.Command, HTTP: raw.Agent.HTTP, Env: raw.Agent.Env, Cwd: raw.Agent.Cwd}
		if a.Command.IsZero() == (a.HTTP == nil) {
			return nil, fail("agent: set exactly one of `command` (run a process per run) or `http` (call a running agent)")
		}
		if a.HTTP != nil {
			if a.HTTP.URL == "" {
				return nil, fail("agent.http.url is required")
			}
			if a.HTTP.Method == "" {
				a.HTTP.Method = "POST"
			}
		}
		s.Agent = a
	}

	for i, rf := range raw.Faults {
		f, err := normalizeFault(rf, i, s)
		if err != nil {
			return nil, fail("faults[%d]: %v", i, err)
		}
		s.Faults = append(s.Faults, f)
	}
	for i, ri := range raw.Invariants {
		inv, err := normalizeInvariant(ri, s)
		if err != nil {
			return nil, fail("invariants[%d]: %v", i, err)
		}
		s.Invariants = append(s.Invariants, inv)
	}
	return s, nil
}

// ServerByName returns the named server or nil.
func (s *Scenario) ServerByName(name string) *Server {
	for _, srv := range s.Servers {
		if srv.Name == name {
			return srv
		}
	}
	return nil
}

func splitTool(tool string, s *Scenario) (server, name string, err error) {
	if tool == "" {
		return "", "", errors.New("`tool` is required")
	}
	if i := strings.Index(tool, "/"); i >= 0 {
		server, name = tool[:i], tool[i+1:]
		if s != nil && s.ServerByName(server) == nil {
			return "", "", fmt.Errorf("unknown server %q in %q", server, tool)
		}
		return server, name, nil
	}
	return "", tool, nil
}

func normalizeFault(rf rawFault, index int, s *Scenario) (*Fault, error) {
	server, tool, err := splitTool(rf.Tool, s)
	if err != nil {
		return nil, err
	}
	if !contains(faultTypes, rf.Type) {
		return nil, fmt.Errorf("`type` must be one of %s", strings.Join(faultTypes, ", "))
	}
	f := &Fault{
		Index:       index,
		Server:      server,
		Tool:        tool,
		Args:        rf.Args,
		Call:        rf.Call,
		Repeat:      1,
		Type:        rf.Type,
		RespondWith: rf.RespondWith,
		Message:     rf.Message,
		Delay:       time.Duration(rf.Delay),
		Corrupt:     rf.Corrupt,
		When:        rf.When,
	}
	if f.Call <= 0 {
		f.Call = 1
	}
	switch v := rf.Repeat.(type) {
	case nil:
	case int:
		if v <= 0 {
			return nil, errors.New("`repeat` must be a positive integer or \"all\"")
		}
		f.Repeat = v
	case string:
		if v != "all" {
			return nil, errors.New("`repeat` must be a positive integer or \"all\"")
		}
		f.Repeat = 0
	default:
		return nil, errors.New("`repeat` must be a positive integer or \"all\"")
	}
	if f.RespondWith == "" {
		f.RespondWith = "tool_error"
	}
	if !contains([]string{"tool_error", "rpc_error", "none"}, f.RespondWith) {
		return nil, errors.New("`respond_with` must be tool_error, rpc_error or none")
	}
	if f.Message == "" {
		switch f.Type {
		case FaultLostResponse:
			f.Message = "Request timed out"
		case FaultError:
			f.Message = "503 Service Unavailable"
		}
	}
	if rf.Code != nil {
		f.Code = *rf.Code
	} else if f.Type == FaultLostResponse {
		f.Code = -32001
	} else {
		f.Code = -32603
	}
	if f.Type == FaultDelay && f.Delay <= 0 {
		return nil, errors.New("delay faults need `delay`, e.g. delay: 3s")
	}
	if f.Type == FaultCorrupt && f.Corrupt == nil {
		return nil, errors.New("corrupt faults need a `corrupt` mapping (set, drop, replace, empty, truncate)")
	}
	if f.When == "" {
		f.When = "after"
	}
	if f.When != "before" && f.When != "after" {
		return nil, errors.New("`when` must be before or after")
	}
	f.Label = describeFault(f)
	return f, nil
}

func describeFault(f *Fault) string {
	target := f.Tool
	if f.Server != "" {
		target = f.Server + "/" + f.Tool
	}
	var calls string
	switch {
	case f.Repeat == 0:
		calls = fmt.Sprintf("calls %d+", f.Call)
	case f.Repeat > 1:
		calls = fmt.Sprintf("calls %d-%d", f.Call, f.Call+f.Repeat-1)
	default:
		calls = fmt.Sprintf("call %d", f.Call)
	}
	shown := func() string {
		if f.RespondWith == "none" {
			return "no response"
		}
		return fmt.Sprintf("%s %q", f.RespondWith, f.Message)
	}
	var detail string
	switch f.Type {
	case FaultError:
		detail = "not sent to the server; agent gets " + shown()
	case FaultLostResponse:
		detail = "executed by the server; agent gets " + shown()
	case FaultDelay:
		detail = "response delayed " + f.Delay.String()
	case FaultCorrupt:
		var parts []string
		c := f.Corrupt
		if len(c.Set) > 0 {
			parts = append(parts, "set")
		}
		if len(c.Drop) > 0 {
			parts = append(parts, "drop")
		}
		if c.Replace != nil {
			parts = append(parts, "replace")
		}
		if c.Empty {
			parts = append(parts, "empty")
		}
		if c.Truncate {
			parts = append(parts, "truncate")
		}
		detail = "response edited (" + strings.Join(parts, ", ") + ")"
	case FaultDisconnect:
		if f.When == "before" {
			detail = "connection drops before the server sees it"
		} else {
			detail = "connection drops after the server executes it"
		}
	case FaultHang:
		detail = "not sent to the server; no response ever arrives"
	}
	return fmt.Sprintf("%s %s → %s: %s", target, calls, f.Type, detail)
}

func normalizeInvariant(ri rawInvariant, s *Scenario) (*Invariant, error) {
	inv := &Invariant{Name: ri.Name, Severity: ri.Severity}
	if inv.Severity == "" {
		inv.Severity = "error"
	}
	if inv.Severity != "error" && inv.Severity != "warn" {
		return nil, errors.New("`severity` must be error or warn")
	}
	kinds := 0
	if ri.Tool != "" {
		kinds++
	}
	if !ri.Check.IsZero() {
		kinds++
	}
	if ri.Output != nil {
		kinds++
	}
	if kinds != 1 {
		return nil, errors.New("set exactly one of `tool` (log invariant), `check` (command) or `output` (regex)")
	}

	switch {
	case !ri.Check.IsZero():
		inv.Kind = InvCheck
		inv.Check = ri.Check
		if inv.Name == "" {
			inv.Name = "check: " + ri.Check.String()
		}
	case ri.Output != nil:
		inv.Kind = InvOutput
		var err error
		if ri.Output.Matches != "" {
			if inv.Matches, err = regexp.Compile("(?m)" + ri.Output.Matches); err != nil {
				return nil, fmt.Errorf("output.matches: %v", err)
			}
		}
		if ri.Output.NotMatches != "" {
			if inv.NotMatch, err = regexp.Compile("(?m)" + ri.Output.NotMatches); err != nil {
				return nil, fmt.Errorf("output.not_matches: %v", err)
			}
		}
		if inv.Matches == nil && inv.NotMatch == nil {
			return nil, errors.New("`output` needs `matches` or `not_matches`")
		}
		if inv.Name == "" {
			if inv.Matches != nil {
				inv.Name = "agent output matches /" + ri.Output.Matches + "/"
			} else {
				inv.Name = "agent output does not match /" + ri.Output.NotMatches + "/"
			}
		}
	default:
		inv.Kind = InvLog
		server, tool, err := splitTool(ri.Tool, s)
		if err != nil {
			return nil, err
		}
		inv.Server, inv.Tool = server, tool
		inv.Where, inv.Committed, inv.Distinct = ri.Where, ri.Committed, ri.Distinct
		inv.Max, inv.Min, inv.Equals = ri.Max, ri.Min, ri.Equals
		if inv.Max == nil && inv.Min == nil && inv.Equals == nil {
			return nil, errors.New("log invariants need `max`, `min` or `equals`")
		}
		if inv.Name == "" {
			var b strings.Builder
			if inv.Distinct != "" {
				fmt.Fprintf(&b, "distinct %s of %s", inv.Distinct, ri.Tool)
			} else {
				if inv.Committed != nil && *inv.Committed {
					b.WriteString("committed ")
				}
				fmt.Fprintf(&b, "%s calls", ri.Tool)
			}
			if inv.Equals != nil {
				fmt.Fprintf(&b, " = %d", *inv.Equals)
			}
			if inv.Min != nil {
				fmt.Fprintf(&b, " ≥ %d", *inv.Min)
			}
			if inv.Max != nil {
				fmt.Fprintf(&b, " ≤ %d", *inv.Max)
			}
			inv.Name = b.String()
		}
	}
	return inv, nil
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
