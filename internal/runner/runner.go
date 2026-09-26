// Package runner executes a scenario: it starts proxies and services, triggers the
// agent once per run with faults armed, and evaluates invariants after each run.
package runner

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mcpfault/mcpfault/internal/config"
	"github.com/mcpfault/mcpfault/internal/engine"
	"github.com/mcpfault/mcpfault/internal/invariants"
	"github.com/mcpfault/mcpfault/internal/lint"
	"github.com/mcpfault/mcpfault/internal/procutil"
	"github.com/mcpfault/mcpfault/internal/proxy"
	"github.com/mcpfault/mcpfault/internal/report"
)

// Options tune a session.
type Options struct {
	Runs    int             // overrides the scenario's run count when > 0
	Pause   time.Duration   // overrides the scenario's pause between runs when > 0
	Agent   *config.Command // overrides the scenario's agent command
	OutDir  string          // where sessions are stored (e.g. .mcpfault/sessions)
	Control string          // control server address, host:port
	Cwd     string          // working directory for commands
	Log     io.Writer       // progress lines; nil for silence
}

const commandTimeout = 2 * time.Minute

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)

// Run executes the scenario. The engine must already be reachable by the agent's
// stdio shims (i.e. the control server is running on opts.Control).
func Run(ctx context.Context, eng *engine.Engine, sc *config.Scenario, opts Options) (*report.Report, error) {
	agent := sc.Agent
	if opts.Agent != nil {
		agent = &config.Agent{Command: *opts.Agent}
		if sc.Agent != nil {
			agent.Env, agent.Cwd = sc.Agent.Env, sc.Agent.Cwd
		}
	}
	if agent == nil {
		return nil, fmt.Errorf("%s: no agent to run; add an `agent` section or pass --agent", filepath.Base(sc.File))
	}
	runs := sc.Runs
	if opts.Runs > 0 {
		runs = opts.Runs
	}
	if opts.Cwd == "" {
		opts.Cwd, _ = os.Getwd()
	}
	logf := func(format string, a ...any) {
		if opts.Log != nil {
			fmt.Fprintf(opts.Log, format, a...)
		}
	}

	started := time.Now()
	id := started.Format("20060102-150405") + "-" + unsafeName.ReplaceAllString(sc.Name, "-")
	rep := &report.Report{
		ID:          id,
		Scenario:    sc.Name,
		File:        sc.File,
		Description: sc.Description,
		Agent:       describeAgent(agent, sc.Dir, opts.Cwd),
		Status:      "running",
		Started:     started,
		PlannedRuns: runs,
		Dir:         filepath.Join(opts.OutDir, id),
	}
	for _, f := range sc.Faults {
		rep.Faults = append(rep.Faults, f.Label)
	}
	for _, srv := range sc.Servers {
		info := report.ServerInfo{Name: srv.Name, Transport: srv.Transport}
		if srv.Transport == "http" {
			info.Endpoint = srv.ProxyURL()
		} else {
			cmd := "<your server command>"
			if !srv.Command.IsZero() {
				cmd = srv.Command.String()
			}
			info.Endpoint = "mcpfault stdio --server " + srv.Name + " -- " + cmd
		}
		rep.Servers = append(rep.Servers, info)
	}
	stats := make([]report.InvariantStat, len(sc.Invariants))
	for i, inv := range sc.Invariants {
		stats[i] = report.InvariantStat{Name: inv.Name, Kind: inv.Kind, Severity: inv.Severity}
	}
	if err := os.MkdirAll(rep.Dir, 0o755); err != nil {
		return nil, err
	}
	rep.Finalize(stats)
	_ = rep.Save()

	eng.Reset()
	eng.Configure(sc.Faults, false)
	eng.Emit(engine.Event{Type: "session_started", Data: map[string]any{"id": id, "scenario": sc.Name, "runs": runs}})
	defer eng.Configure(nil, false)

	vars := map[string]string{
		"session_dir":  rep.Dir,
		"scenario_dir": sc.Dir,
		"control":      opts.Control,
	}
	baseEnv := map[string]string{"MCPFAULT_CONTROL": opts.Control, "MCPFAULT_SESSION_DIR": rep.Dir}

	// HTTP proxies for remote servers.
	for _, srv := range sc.Servers {
		if srv.Transport != "http" {
			continue
		}
		upstream, err := url.Parse(srv.URL)
		if err != nil {
			return nil, fmt.Errorf("server %s: bad url: %w", srv.Name, err)
		}
		p := &proxy.HTTP{Name: srv.Name, Upstream: upstream, Listen: srv.Listen, Hooks: proxy.LocalHooks{Engine: eng}, Released: eng.Released, OnFailure: eng.OnFailure}
		if err := p.Start(); err != nil {
			return nil, err
		}
		defer p.Close()
		vars["proxy."+srv.Name] = srv.ProxyURL()
		baseEnv["MCPFAULT_PROXY_"+envName(srv.Name)] = srv.ProxyURL()
		logf("  proxy  %s → %s  (point your agent at %s)\n", srv.Name, srv.URL, srv.ProxyURL())
	}

	// Long-lived services (MCP servers over HTTP, an agent running as a web app, ...).
	for _, svc := range sc.Services {
		stop, err := StartService(ctx, svc, vars, baseEnv, rep.Dir, opts.Cwd, logf)
		if err != nil {
			rep.Warnings = append(rep.Warnings, err.Error())
			finish(eng, sc, rep, stats)
			return rep, err
		}
		defer stop()
	}

	pause := sc.Pause
	if opts.Pause > 0 {
		pause = opts.Pause
	}
	for i := 1; i <= runs; i++ {
		if i > 1 && pause > 0 {
			select {
			case <-time.After(pause):
			case <-ctx.Done():
			}
		}
		if ctx.Err() != nil {
			rep.Warnings = append(rep.Warnings, "session cancelled")
			break
		}
		res := runOnce(ctx, eng, sc, agent, i, vars, baseEnv, rep.Dir, opts.Cwd)
		rep.Runs = append(rep.Runs, res)
		rep.Contract = contract(eng, sc)
		rep.Finalize(stats)
		_ = rep.Save()
		eng.Emit(engine.Event{Type: "run_finished", Data: map[string]any{"session": id, "run": i, "status": res.Status}})
		mark := "✓"
		if res.Status != "passed" {
			mark = "✗"
		}
		logf("  run %d/%d %s %s\n", i, runs, mark, runSummary(res))
	}

	seen := eng.Servers()
	for i := range rep.Servers {
		_, rep.Servers[i].Connected = seen[rep.Servers[i].Name]
	}
	finish(eng, sc, rep, stats)
	return rep, nil
}

func finish(eng *engine.Engine, sc *config.Scenario, rep *report.Report, stats []report.InvariantStat) {
	rep.Contract = contract(eng, sc)
	rep.Finalize(stats)
	for _, s := range rep.Servers {
		if !s.Connected {
			rep.Passed = false
		}
	}
	now := time.Now()
	rep.Finished = &now
	rep.Status = "failed"
	if rep.Passed {
		rep.Status = "passed"
	}
	_ = rep.Save()
	eng.Emit(engine.Event{Type: "session_finished", Data: map[string]any{"id": rep.ID, "status": rep.Status}})
}

func runOnce(ctx context.Context, eng *engine.Engine, sc *config.Scenario, agent *config.Agent, i int, base map[string]string, baseEnv map[string]string, sessionDir, cwd string) *report.RunResult {
	runDir := filepath.Join(sessionDir, fmt.Sprintf("run-%03d", i))
	_ = os.MkdirAll(runDir, 0o755)
	res := &report.RunResult{Index: i, Dir: runDir, Status: "passed"}
	vars := copyMap(base)
	vars["run"], vars["run_dir"] = strconv.Itoa(i), runDir
	env := copyMap(baseEnv)
	env["MCPFAULT_RUN"], env["MCPFAULT_RUN_DIR"] = strconv.Itoa(i), runDir

	for _, cmd := range sc.Setup {
		if out, code, err := runStep(ctx, cmd.Expand(vars), cwd, env, filepath.Join(runDir, "setup.log")); err != nil || code != 0 {
			res.Status, res.Error = "error", fmt.Sprintf("setup failed (%s): %s", cmd.String(), lastLine(out, err))
			return res
		}
	}

	eng.BeginRun(i)
	eng.Emit(engine.Event{Type: "run_started", Data: map[string]any{"run": i}})
	start := time.Now()
	agentEnv := copyMap(env)
	for k, v := range agent.Env {
		agentEnv[k] = config.Expand(v, vars)
	}
	output, stderr, code, runErr := trigger(ctx, agent, vars, agentEnv, cwd, sc.Timeout, runDir)
	res.DurationMS = time.Since(start).Milliseconds()
	time.Sleep(250 * time.Millisecond) // let late responses land in the log
	eng.EndRun()

	res.Output, res.Stderr = tail(output, 16<<10), tail(stderr, 8<<10)
	if code != nil {
		res.ExitCode = code
	}
	if runErr != "" {
		res.Status, res.Error = "error", runErr
	}
	res.Calls = eng.Calls(i)
	for n := range res.Calls {
		res.Calls[n].Seq = n + 1
	}

	outFile := filepath.Join(runDir, "agent.out.txt")
	checkEnv := copyMap(env)
	checkEnv["MCPFAULT_AGENT_OUTPUT"] = outFile
	if code != nil {
		checkEnv["MCPFAULT_AGENT_EXIT_CODE"] = strconv.Itoa(*code)
	}
	for _, inv := range sc.Invariants {
		var o invariants.Outcome
		switch inv.Kind {
		case config.InvLog:
			var err error
			if o, err = invariants.EvaluateLog(inv, res.Calls); err != nil {
				o = invariants.Outcome{Pass: false, Detail: err.Error()}
			}
		case config.InvOutput:
			o = invariants.EvaluateOutput(inv, output)
		case config.InvCheck:
			out, code, err := runStep(ctx, inv.Check.Expand(vars), cwd, checkEnv, filepath.Join(runDir, "checks.log"))
			o = invariants.Outcome{Pass: err == nil && code == 0, Detail: lastLine(out, err)}
		}
		res.Invariants = append(res.Invariants, report.InvariantResult{Name: inv.Name, Severity: inv.Severity, Pass: o.Pass, Detail: o.Detail})
		if !o.Pass && inv.Severity == "error" && res.Status == "passed" {
			res.Status = "failed"
		}
	}

	for _, cmd := range sc.Teardown {
		runStep(ctx, cmd.Expand(vars), cwd, env, filepath.Join(runDir, "teardown.log"))
	}
	return res
}

// trigger runs the agent once and returns its output.
func trigger(ctx context.Context, agent *config.Agent, vars, env map[string]string, cwd string, timeout time.Duration, runDir string) (output, stderr string, code *int, runErr string) {
	dir := cwd
	if agent.Cwd != "" {
		dir = filepath.Join(cwd, config.Expand(agent.Cwd, vars))
	}
	if agent.HTTP != nil {
		return triggerHTTP(ctx, agent.HTTP, vars, timeout, runDir)
	}
	cmd, err := procutil.Build(agent.Command.Expand(vars), dir, env)
	if err != nil {
		return "", "", nil, err.Error()
	}
	var out, errb bytes.Buffer
	outFile, _ := os.Create(filepath.Join(runDir, "agent.out.txt"))
	errFile, _ := os.Create(filepath.Join(runDir, "agent.err.txt"))
	defer outFile.Close()
	defer errFile.Close()
	cmd.Stdout = io.MultiWriter(&out, outFile)
	cmd.Stderr = io.MultiWriter(&errb, errFile)
	r := procutil.Run(ctx, cmd, timeout)
	c := r.ExitCode
	switch {
	case r.TimedOut:
		runErr = fmt.Sprintf("agent timed out after %s", timeout)
	case r.Err != nil:
		runErr = "could not run agent: " + r.Err.Error()
	case c != 0:
		runErr = fmt.Sprintf("agent exited with code %d", c)
	}
	return out.String(), errb.String(), &c, runErr
}

func triggerHTTP(ctx context.Context, h *config.HTTPTrigger, vars map[string]string, timeout time.Duration, runDir string) (string, string, *int, string) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, h.Method, config.Expand(h.URL, vars), strings.NewReader(config.Expand(h.Body, vars)))
	if err != nil {
		return "", "", nil, err.Error()
	}
	for k, v := range h.Headers {
		req.Header.Set(k, config.Expand(v, vars))
	}
	if h.Body != "" && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return "", "", nil, fmt.Sprintf("agent timed out after %s", timeout)
		}
		return "", "", nil, "calling agent: " + err.Error()
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	_ = os.WriteFile(filepath.Join(runDir, "agent.out.txt"), body, 0o644)
	status := resp.StatusCode
	if status >= 400 {
		return string(body), "", &status, fmt.Sprintf("agent returned HTTP %d", status)
	}
	return string(body), "", &status, ""
}

// runStep runs a setup, check or teardown command, appending its output to logFile.
func runStep(ctx context.Context, c config.Command, cwd string, env map[string]string, logFile string) (string, int, error) {
	cmd, err := procutil.Build(c, cwd, env)
	if err != nil {
		return "", -1, err
	}
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	r := procutil.Run(ctx, cmd, commandTimeout)
	if f, err := os.OpenFile(logFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
		fmt.Fprintf(f, "$ %s\n%s\n[exit %d]\n\n", c.String(), buf.String(), r.ExitCode)
		f.Close()
	}
	if r.TimedOut {
		return buf.String(), -1, fmt.Errorf("timed out after %s", commandTimeout)
	}
	return buf.String(), r.ExitCode, r.Err
}

// StartService starts a background service and waits until it is ready. The returned func stops it.
func StartService(ctx context.Context, svc *config.Service, vars, env map[string]string, sessionDir, cwd string, logf func(string, ...any)) (func(), error) {
	dir := cwd
	if svc.Cwd != "" {
		dir = filepath.Join(cwd, config.Expand(svc.Cwd, vars))
	}
	e := copyMap(env)
	for k, v := range svc.Env {
		e[k] = config.Expand(v, vars)
	}
	cmd, err := procutil.Build(svc.Command.Expand(vars), dir, e)
	if err != nil {
		return nil, err
	}
	logPath := filepath.Join(sessionDir, "service-"+unsafeName.ReplaceAllString(svc.Name, "-")+".log")
	logFile, err := os.Create(logPath)
	if err != nil {
		return nil, err
	}
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		logFile.Close()
		return nil, fmt.Errorf("service %s: %w", svc.Name, err)
	}
	var once sync.Once
	exited := make(chan struct{})
	go func() { cmd.Wait(); close(exited) }()
	stop := func() {
		once.Do(func() {
			procutil.KillTree(cmd)
			select {
			case <-exited:
			case <-time.After(5 * time.Second):
			}
			logFile.Close()
		})
	}
	logf("  service %s started (log: %s)\n", svc.Name, logPath)
	if svc.Ready != "" {
		alive := func() bool {
			select {
			case <-exited:
				return false
			default:
				return true
			}
		}
		logf("  waiting for %s ...\n", config.Expand(svc.Ready, vars))
		if err := procutil.WaitReady(ctx, config.Expand(svc.Ready, vars), svc.ReadyTimeout, alive); err != nil {
			stop()
			return nil, fmt.Errorf("service %s: %v (see %s)", svc.Name, err, logPath)
		}
	}
	return stop, nil
}

// contract lints the tools the servers advertised. With a scenario, it keeps only findings
// for tools the scenario exercises, plus any tool that cannot be retried safely at all.
func contract(eng *engine.Engine, sc *config.Scenario) []lint.Finding {
	var out []lint.Finding
	for server, raw := range eng.Tools() {
		for _, f := range lint.CheckRaw(server, raw) {
			if sc == nil || f.Level == lint.LevelGap || referenced(sc, server, f.Tool) {
				out = append(out, f)
			}
		}
	}
	return out
}

func referenced(sc *config.Scenario, server, tool string) bool {
	for _, f := range sc.Faults {
		if f.Tool == tool && (f.Server == "" || f.Server == server) {
			return true
		}
	}
	for _, inv := range sc.Invariants {
		if inv.Kind == config.InvLog && inv.Tool == tool && (inv.Server == "" || inv.Server == server) {
			return true
		}
	}
	return false
}

func describeAgent(a *config.Agent, scenarioDir, cwd string) string {
	dir := scenarioDir
	if r, err := filepath.Rel(cwd, scenarioDir); err == nil && !strings.HasPrefix(r, "..") {
		dir = filepath.ToSlash(r)
	}
	vars := map[string]string{"scenario_dir": dir}
	if a.HTTP != nil {
		return a.HTTP.Method + " " + config.Expand(a.HTTP.URL, vars)
	}
	return a.Command.Expand(vars).String()
}

func runSummary(r *report.RunResult) string {
	if r.Error != "" {
		return r.Error
	}
	var failed []string
	for _, inv := range r.Invariants {
		if !inv.Pass {
			failed = append(failed, inv.Name)
		}
	}
	calls := fmt.Sprintf("%d tool call%s", len(r.Calls), map[bool]string{true: "", false: "s"}[len(r.Calls) == 1])
	if len(failed) == 0 {
		return calls
	}
	return calls + ", violated: " + strings.Join(failed, "; ")
}

func envName(s string) string {
	return strings.ToUpper(strings.NewReplacer("-", "_", ".", "_").Replace(s))
}

func copyMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m)+4)
	for k, v := range m {
		out[k] = v
	}
	return out
}

func lastLine(out string, err error) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	last := strings.TrimSpace(lines[len(lines)-1])
	if err != nil {
		if last != "" {
			return last + " (" + err.Error() + ")"
		}
		return err.Error()
	}
	return last
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}
