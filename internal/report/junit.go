package report

import (
	"encoding/xml"
	"fmt"
	"io"
	"strings"
)

type junitSuites struct {
	XMLName xml.Name     `xml:"testsuites"`
	Name    string       `xml:"name,attr"`
	Tests   int          `xml:"tests,attr"`
	Fails   int          `xml:"failures,attr"`
	Errors  int          `xml:"errors,attr"`
	Suites  []junitSuite `xml:"testsuite"`
}

type junitSuite struct {
	Name   string      `xml:"name,attr"`
	Tests  int         `xml:"tests,attr"`
	Fails  int         `xml:"failures,attr"`
	Errors int         `xml:"errors,attr"`
	Time   string      `xml:"time,attr"`
	Cases  []junitCase `xml:"testcase"`
}

type junitCase struct {
	Class   string        `xml:"classname,attr"`
	Name    string        `xml:"name,attr"`
	Failure *junitMessage `xml:"failure,omitempty"`
	Error   *junitMessage `xml:"error,omitempty"`
	Out     string        `xml:"system-out,omitempty"`
}

type junitMessage struct {
	Message string `xml:"message,attr"`
	Type    string `xml:"type,attr"`
	Body    string `xml:",chardata"`
}

// WriteJUnit writes one test suite per scenario and one test case per invariant.
func WriteJUnit(w io.Writer, reports []*Report) error {
	out := junitSuites{Name: "mcpfault"}
	for _, r := range reports {
		s := junitSuite{Name: r.Scenario}
		if r.Finished != nil {
			s.Time = fmt.Sprintf("%.3f", r.Finished.Sub(r.Started).Seconds())
		}
		for _, inv := range r.Invariants {
			c := junitCase{Class: "mcpfault." + r.Scenario, Name: inv.Name}
			if inv.Failed > 0 {
				var body strings.Builder
				for _, run := range r.Runs {
					for _, res := range run.Invariants {
						if res.Name == inv.Name && !res.Pass {
							fmt.Fprintf(&body, "run %d: %s\n", run.Index, res.Detail)
						}
					}
				}
				msg := fmt.Sprintf("violated in %d/%d runs", inv.Failed, inv.Evaluated)
				if inv.Severity == "warn" {
					c.Out = "warning: " + msg + "\n" + body.String()
				} else {
					c.Failure = &junitMessage{Message: msg, Type: "invariant", Body: body.String()}
					s.Fails++
				}
			}
			s.Cases = append(s.Cases, c)
		}
		var errs strings.Builder
		n := 0
		for _, run := range r.Runs {
			if run.Status == "error" {
				n++
				fmt.Fprintf(&errs, "run %d: %s\n", run.Index, run.Error)
			}
		}
		runs := junitCase{Class: "mcpfault." + r.Scenario, Name: "agent runs complete"}
		if n > 0 {
			runs.Error = &junitMessage{Message: fmt.Sprintf("%d/%d runs errored", n, len(r.Runs)), Type: "run", Body: errs.String()}
			s.Errors++
		}
		s.Cases = append(s.Cases, runs)
		s.Tests = len(s.Cases)
		out.Tests += s.Tests
		out.Fails += s.Fails
		out.Errors += s.Errors
		out.Suites = append(out.Suites, s)
	}
	io.WriteString(w, xml.Header)
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	return enc.Encode(out)
}
