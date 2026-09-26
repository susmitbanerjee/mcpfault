// Package cli implements the mcpfault command line.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mcpfault/mcpfault/internal/config"
	"github.com/mcpfault/mcpfault/internal/engine"
	"github.com/mcpfault/mcpfault/internal/lint"
	"github.com/mcpfault/mcpfault/internal/procutil"
	"github.com/mcpfault/mcpfault/internal/proxy"
	"github.com/mcpfault/mcpfault/internal/report"
	"github.com/mcpfault/mcpfault/internal/runner"
	"github.com/mcpfault/mcpfault/internal/server"
)

// ExitError carries a process exit code.
type ExitError struct{ Code int }

func (e ExitError) Error() string { return fmt.Sprintf("exit %d", e.Code) }

// Execute runs the CLI and returns the process exit code.
func Execute(version string) int {
	root := newRoot(version)
	err := root.Execute()
	var exit ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &exit):
		return exit.Code
	default:
		fmt.Fprintln(os.Stderr, "mcpfault:", err)
		return 2
	}
}

func newRoot(version string) *cobra.Command {
	root := &cobra.Command{
		Use:   "mcpfault",
		Short: "Fault injection and recovery testing for agents that call MCP tools",
		Long: `mcpfault sits between your agent and its MCP servers, makes tool calls fail in
realistic ways (timeouts after the work was done, rate limits, bad data, outages,
slow responses), runs the agent many times, and checks what actually happened.

It works with any agent framework in any language: LangChain, LangGraph, Spring AI,
OpenAI Agents SDK, Claude Agent SDK, custom code. Your agent only needs to reach its
MCP servers through mcpfault, which is a config change, not a code change.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       version,
	}
	root.AddCommand(runCmd(version), stdioCmd(), proxyCmd(version), lintCmd(version), uiCmd(version), initCmd(), versionCmd(version))
	return root
}

func versionCmd(version string) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version",
		Run:   func(cmd *cobra.Command, args []string) { fmt.Println("mcpfault", version) },
	}
}

func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt)
}

func controlDefault() string {
	if v := os.Getenv("MCPFAULT_CONTROL"); v != "" {
		return v
	}
	return server.DefaultAddr
}

// ---------- run ----------

func runCmd(version string) *cobra.Command {
	var (
		runs    int
		pause   time.Duration
		agent   string
		out     string
		control string
		asJSON  bool
		junit   string
		open    bool
		noColor bool
	)
	cmd := &cobra.Command{
		Use:   "run [scenario.fault.yaml ...]",
		Short: "Run scenarios against your agent and check invariants",
		Long: `Runs each scenario: starts proxies and services, triggers the agent once per run with
faults armed, and evaluates every invariant after each run.

With no arguments, runs every *.fault.yaml file under the current directory.
Exits 1 if any error-severity invariant is violated or any run errors.`,
		Example: `  mcpfault run
  mcpfault run faults/lost-payment.fault.yaml --runs 20
  mcpfault run --junit results.xml
  mcpfault run checkout.fault.yaml --agent "python agent.py 'Pay invoice INV-1042'"`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cwd, _ := os.Getwd()
			files, err := resolveScenarios(cwd, args)
			if err != nil {
				return err
			}
			var scenarios []*config.Scenario
			for _, f := range files {
				sc, err := config.Load(f)
				if err != nil {
					return err
				}
				scenarios = append(scenarios, sc)
			}

			eng := engine.New()
			srv := &server.Server{Engine: eng, Addr: control, Dir: absOr(out, cwd), Cwd: cwd, Version: version, Mode: "run"}
			if err := srv.Start(); err != nil {
				return err
			}
			defer srv.Close()
			if open {
				openBrowser(srv.URL())
			}
			ctx, cancel := signalContext()
			defer cancel()

			style := report.Style{Color: !noColor && colorEnabled()}
			progress := os.Stderr
			fmt.Fprintf(progress, "mcpfault %s · live view %s\n", version, srv.URL())

			var agentOverride *config.Command
			if agent != "" {
				agentOverride = &config.Command{Shell: agent}
			}
			var reports []*report.Report
			failed := false
			for _, sc := range scenarios {
				fmt.Fprintf(progress, "\n▶ %s (%s)\n", sc.Name, rel(cwd, sc.File))
				rep, err := runner.Run(ctx, eng, sc, runner.Options{Runs: runs, Pause: pause, Agent: agentOverride, OutDir: srv.Dir, Control: control, Cwd: cwd, Log: progress})
				if err != nil && rep == nil {
					return err
				}
				if err != nil {
					fmt.Fprintln(progress, "  error:", err)
				}
				reports = append(reports, rep)
				if !rep.Passed {
					failed = true
				}
				if !asJSON {
					report.Print(os.Stdout, rep, style, cwd)
				}
				if ctx.Err() != nil {
					break
				}
			}
			if asJSON {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				enc.Encode(reports)
			}
			if junit != "" {
				f, err := os.Create(junit)
				if err != nil {
					return err
				}
				defer f.Close()
				if err := report.WriteJUnit(f, reports); err != nil {
					return err
				}
			}
			if len(reports) > 1 && !asJSON {
				printTotals(reports, style)
			}
			if failed {
				return ExitError{Code: 1}
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.IntVarP(&runs, "runs", "n", 0, "runs per scenario (overrides the scenario file)")
	f.DurationVar(&pause, "pause", 0, "wait between runs, e.g. 10s (helps with LLM rate limits)")
	f.StringVar(&agent, "agent", "", "agent command to run instead of the scenario's (runs through the shell)")
	f.StringVar(&out, "out", filepath.Join(".mcpfault", "sessions"), "where to store results")
	f.StringVar(&control, "control", controlDefault(), "control server address (stdio shims connect here)")
	f.BoolVar(&asJSON, "json", false, "print reports as JSON instead of text")
	f.StringVar(&junit, "junit", "", "also write a JUnit XML report to this file")
	f.BoolVar(&open, "open", false, "open the live view in a browser")
	f.BoolVar(&noColor, "no-color", false, "disable colors")
	return cmd
}

func printTotals(reports []*report.Report, st report.Style) {
	passed := 0
	for _, r := range reports {
		if r.Passed {
			passed++
		}
	}
	fmt.Println(st.Bold(fmt.Sprintf("%d/%d scenarios passed", passed, len(reports))))
	for _, r := range reports {
		mark := st.Green("✓")
		if !r.Passed {
			mark = st.Red("✗")
		}
		fmt.Printf("  %s %s\n", mark, r.Scenario)
	}
	fmt.Println()
}

func resolveScenarios(cwd string, args []string) ([]string, error) {
	if len(args) == 0 {
		files, err := config.Discover(cwd)
		if err != nil {
			return nil, err
		}
		if len(files) == 0 {
			return nil, errors.New("no scenario files given and no *.fault.yaml files found (create one with `mcpfault init`)")
		}
		return files, nil
	}
	var files []string
	for _, a := range args {
		if info, err := os.Stat(a); err == nil && info.IsDir() {
			found, err := config.Discover(a)
			if err != nil {
				return nil, err
			}
			if len(found) == 0 {
				return nil, fmt.Errorf("no *.fault.yaml files under %s", a)
			}
			files = append(files, found...)
			continue
		}
		matches, err := filepath.Glob(a)
		if err != nil || len(matches) == 0 {
			if _, statErr := os.Stat(a); statErr != nil {
				return nil, fmt.Errorf("scenario %s: %w", a, statErr)
			}
			matches = []string{a}
		}
		files = append(files, matches...)
	}
	return files, nil
}

// ---------- stdio ----------

func stdioCmd() *cobra.Command {
	var name, control string
	cmd := &cobra.Command{
		Use:   "stdio --server NAME -- COMMAND [ARGS...]",
		Short: "Wrap a stdio MCP server (put this in your agent's MCP config)",
		Long: `Starts COMMAND as the MCP server and relays stdio traffic through mcpfault.
Use it in your agent's MCP server configuration in place of the real command.

If no mcpfault control server is running, traffic passes through unchanged, so it is
safe to leave in a development config.`,
		Example: `  mcpfault stdio --server billing -- python billing_server.py
  mcpfault stdio --server files -- npx -y @modelcontextprotocol/server-filesystem ./sandbox`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if name == "" {
				return errors.New("--server is required (use the name from your scenario's servers section)")
			}
			s := &proxy.Stdio{Server: name, Command: args, Hooks: proxy.NewRemoteHooks(control), Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr}
			code, err := s.Run()
			if err != nil {
				return err
			}
			if code != 0 {
				return ExitError{Code: code}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "server", "", "server name, as used in scenario files")
	cmd.Flags().StringVar(&control, "control", controlDefault(), "control server address")
	cmd.Flags().SetInterspersed(false)
	return cmd
}

// ---------- proxy ----------

func proxyCmd(version string) *cobra.Command {
	var control, upstream, listen, name string
	var open bool
	cmd := &cobra.Command{
		Use:   "proxy [scenario.fault.yaml]",
		Short: "Run the proxy on its own with faults always on, and watch calls live",
		Long: `Starts the control server, the web UI, and an HTTP proxy for each http server in the
scenario. Faults from the scenario apply to every matching call until you stop it.
Use this to explore by hand with any MCP client, or to drive your agent yourself.`,
		Example: `  mcpfault proxy faults/lost-payment.fault.yaml --open
  mcpfault proxy --upstream http://localhost:8000/mcp --listen 127.0.0.1:7362`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cwd, _ := os.Getwd()
			var sc *config.Scenario
			switch {
			case len(args) == 1:
				var err error
				if sc, err = config.Load(args[0]); err != nil {
					return err
				}
			case upstream != "":
				sc = &config.Scenario{Name: "adhoc", Servers: []*config.Server{{Name: name, Transport: "http", URL: upstream, Listen: listen}}}
			default:
				return errors.New("give a scenario file or --upstream URL")
			}

			eng := engine.New()
			eng.Configure(sc.Faults, true)
			srv := &server.Server{Engine: eng, Addr: control, Dir: filepath.Join(cwd, ".mcpfault", "sessions"), Cwd: cwd, Version: version, Mode: "proxy", Live: sc}
			if err := srv.Start(); err != nil {
				return err
			}
			defer srv.Close()

			fmt.Printf("mcpfault %s · live view %s\n\n", version, srv.URL())
			for _, s := range sc.Servers {
				if s.Transport == "http" {
					u, err := url.Parse(s.URL)
					if err != nil {
						return err
					}
					p := &proxy.HTTP{Name: s.Name, Upstream: u, Listen: s.Listen, Hooks: proxy.LocalHooks{Engine: eng}, Released: eng.Released, OnFailure: eng.OnFailure}
					if err := p.Start(); err != nil {
						return err
					}
					defer p.Close()
					fmt.Printf("  %-12s http   %s  →  %s\n", s.Name, s.ProxyURL(), s.URL)
				} else {
					c := "<server command>"
					if !s.Command.IsZero() {
						c = s.Command.String()
					}
					fmt.Printf("  %-12s stdio  mcpfault stdio --server %s -- %s\n", s.Name, s.Name, c)
				}
			}
			if len(sc.Faults) > 0 {
				fmt.Println("\n  Faults (always on):")
				for _, f := range sc.Faults {
					fmt.Println("   ", f.Label)
				}
			}
			fmt.Println("\nPress Ctrl+C to stop.")
			if open {
				openBrowser(srv.URL())
			}
			ctx, cancel := signalContext()
			defer cancel()
			<-ctx.Done()
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&control, "control", controlDefault(), "control server and UI address")
	f.StringVar(&upstream, "upstream", "", "proxy a single HTTP MCP server without a scenario file")
	f.StringVar(&listen, "listen", fmt.Sprintf("127.0.0.1:%d", config.DefaultProxyPort), "listen address for --upstream")
	f.StringVar(&name, "name", "server", "server name for --upstream")
	f.BoolVar(&open, "open", false, "open the live view in a browser")
	return cmd
}

// ---------- lint ----------

func lintCmd(version string) *cobra.Command {
	var target string
	var sse, asJSON bool
	cmd := &cobra.Command{
		Use:   "lint [scenario.fault.yaml] | --url URL | -- COMMAND [ARGS...]",
		Short: "Check whether write tools can be retried safely",
		Long: `Connects to MCP servers, lists their tools, and checks every write tool for a safe
retry path after an ambiguous failure: an idempotency key, or a lookup tool the agent
can use to check whether the first attempt went through.

Exits 1 if any tool has no safe retry path at all (a "gap").`,
		Example: `  mcpfault lint faults/lost-payment.fault.yaml
  mcpfault lint --url http://localhost:8000/mcp
  mcpfault lint -- python billing_server.py`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signalContext()
			defer cancel()
			cwd, _ := os.Getwd()
			var findings []lint.Finding
			dash := cmd.ArgsLenAtDash()
			switch {
			case dash >= 0:
				tools, err := lint.FetchTools(ctx, lint.Target{Command: args[dash:], Env: procutil.Env(nil)}, version)
				if err != nil {
					return err
				}
				findings = lint.Check("server", tools)
			case target != "":
				tools, err := lint.FetchTools(ctx, lint.Target{URL: target, SSE: sse}, version)
				if err != nil {
					return err
				}
				findings = lint.Check("server", tools)
			case len(args) == 1:
				sc, err := config.Load(args[0])
				if err != nil {
					return err
				}
				if findings, err = lintScenario(ctx, sc, cwd, version); err != nil {
					return err
				}
			default:
				return errors.New("give a scenario file, --url, or -- <server command>")
			}
			if asJSON {
				json.NewEncoder(os.Stdout).Encode(findings)
			} else {
				report.PrintLint(os.Stdout, findings, report.Style{Color: colorEnabled()})
			}
			for _, f := range findings {
				if f.Level == lint.LevelGap {
					return ExitError{Code: 1}
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&target, "url", "", "HTTP MCP endpoint to lint")
	cmd.Flags().BoolVar(&sse, "sse", false, "use the legacy HTTP+SSE transport for --url")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print findings as JSON")
	return cmd
}

func lintScenario(ctx context.Context, sc *config.Scenario, cwd, version string) ([]lint.Finding, error) {
	tmp, err := os.MkdirTemp("", "mcpfault-lint-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	vars := map[string]string{"run": "0", "run_dir": tmp, "session_dir": tmp, "scenario_dir": sc.Dir}
	for _, s := range sc.Setup {
		c, err := procutil.Build(s.Expand(vars), cwd, nil)
		if err == nil {
			procutil.Run(ctx, c, 2*time.Minute)
		}
	}
	for _, svc := range sc.Services {
		stop, err := runner.StartService(ctx, svc, vars, nil, tmp, cwd, func(string, ...any) {})
		if err != nil {
			return nil, err
		}
		defer stop()
	}
	var findings []lint.Finding
	for _, s := range sc.Servers {
		t := lint.Target{URL: s.URL, Dir: cwd, Env: procutil.Env(config.ExpandMap(s.Env, vars))}
		if s.Transport == "stdio" {
			if s.Command.IsZero() {
				fmt.Fprintf(os.Stderr, "skipping %s: add `command` to the server in the scenario to lint it\n", s.Name)
				continue
			}
			c := s.Command.Expand(vars)
			t.Command = c.Argv
			if c.Shell != "" {
				t.Command = strings.Fields(c.Shell)
			}
		}
		tools, err := lint.FetchTools(ctx, t, version)
		if err != nil {
			return nil, fmt.Errorf("server %s: %w", s.Name, err)
		}
		findings = append(findings, lint.Check(s.Name, tools)...)
	}
	return findings, nil
}

// ---------- ui ----------

func uiCmd(version string) *cobra.Command {
	var control, out string
	var open bool
	cmd := &cobra.Command{
		Use:   "ui",
		Short: "Open the web UI to browse results and run scenarios",
		RunE: func(cmd *cobra.Command, args []string) error {
			cwd, _ := os.Getwd()
			srv := &server.Server{Engine: engine.New(), Addr: control, Dir: absOr(out, cwd), Cwd: cwd, Version: version, Mode: "ui"}
			if err := srv.Start(); err != nil {
				return err
			}
			defer srv.Close()
			fmt.Printf("mcpfault %s · %s\nPress Ctrl+C to stop.\n", version, srv.URL())
			if open {
				openBrowser(srv.URL())
			}
			ctx, cancel := signalContext()
			defer cancel()
			<-ctx.Done()
			return nil
		},
	}
	cmd.Flags().StringVar(&control, "control", controlDefault(), "address for the UI and control server")
	cmd.Flags().StringVar(&out, "out", filepath.Join(".mcpfault", "sessions"), "where results are stored")
	cmd.Flags().BoolVar(&open, "open", true, "open a browser")
	return cmd
}

func absOr(p, cwd string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(cwd, p)
}

func rel(cwd, p string) string {
	if r, err := filepath.Rel(cwd, p); err == nil && !strings.HasPrefix(r, "..") {
		return filepath.ToSlash(r)
	}
	return p
}
