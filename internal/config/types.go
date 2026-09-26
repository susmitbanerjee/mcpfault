package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Command is a process to run. A YAML string runs through the platform shell;
// a YAML list runs directly without a shell.
type Command struct {
	Shell string
	Argv  []string
}

func (c *Command) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		c.Shell = n.Value
		return nil
	case yaml.SequenceNode:
		return n.Decode(&c.Argv)
	default:
		return fmt.Errorf("line %d: expected a command string or a list of arguments", n.Line)
	}
}

// IsZero reports whether no command was given.
func (c Command) IsZero() bool { return c.Shell == "" && len(c.Argv) == 0 }

func (c Command) String() string {
	if c.Shell != "" {
		return c.Shell
	}
	parts := make([]string, len(c.Argv))
	for i, a := range c.Argv {
		if strings.ContainsAny(a, " \t\"'") {
			parts[i] = fmt.Sprintf("%q", a)
		} else {
			parts[i] = a
		}
	}
	return strings.Join(parts, " ")
}

// Expand applies template variables to every part of the command.
func (c Command) Expand(vars map[string]string) Command {
	out := Command{Shell: Expand(c.Shell, vars)}
	for _, a := range c.Argv {
		out.Argv = append(out.Argv, Expand(a, vars))
	}
	return out
}

// Duration accepts Go duration strings such as "500ms", "3s" or "2m".
type Duration time.Duration

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	v, err := time.ParseDuration(strings.TrimSpace(n.Value))
	if err != nil {
		return fmt.Errorf("line %d: %q is not a duration (use values like 500ms, 3s, 2m)", n.Line, n.Value)
	}
	*d = Duration(v)
	return nil
}

var tokenRe = regexp.MustCompile(`\{\{\s*([A-Za-z0-9_.]+)\s*\}\}`)

// Expand replaces {{name}} tokens with values from vars, and {{env.NAME}} with environment variables.
// Unknown tokens are left untouched.
func Expand(s string, vars map[string]string) string {
	if !strings.Contains(s, "{{") {
		return s
	}
	return tokenRe.ReplaceAllStringFunc(s, func(m string) string {
		name := tokenRe.FindStringSubmatch(m)[1]
		if v, ok := vars[name]; ok {
			return v
		}
		if strings.HasPrefix(name, "env.") {
			return os.Getenv(strings.TrimPrefix(name, "env."))
		}
		return m
	})
}

// ExpandMap expands every value of a string map.
func ExpandMap(m map[string]string, vars map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = Expand(v, vars)
	}
	return out
}

// Discover finds scenario files (*.fault.yaml / *.fault.yml) under root,
// skipping dependency and build directories.
func Discover(root string) ([]string, error) {
	skip := map[string]bool{".git": true, "node_modules": true, ".venv": true, "venv": true, "target": true, "build": true, "dist": true, ".mcpfault": true, "__pycache__": true, ".gradle": true, ".idea": true}
	var files []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if path != root && skip[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(d.Name(), ".fault.yaml") || strings.HasSuffix(d.Name(), ".fault.yml") {
			files = append(files, path)
		}
		return nil
	})
	return files, err
}
