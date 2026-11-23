// Package policy is the oslo.policy rule language Glance enforces: check
// strings with rule:, role:, generic "key:%(target)s" and quoted-literal
// checks, "and", "or", "not", parentheses, "@" (always) and "!" (never).
// Defaults are the original's (glance-defaults.json, from glance/policies);
// a policy.yaml or policy.json overrides them by name.
package policy

import (
	"bufio"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
)

//go:embed glance-defaults.json
var defaultsJSON []byte

// Defaults returns the original's default rules by name.
func Defaults() map[string]string {
	out := map[string]string{}
	_ = json.Unmarshal(defaultsJSON, &out)
	return out
}

// Credentials are what a token gives (oslo.context to_policy_values).
type Credentials struct {
	Roles                       []string
	ProjectID, UserID, DomainID string
	IsAdmin                     bool
}

func (c Credentials) values() map[string]any {
	roles := make([]any, 0, len(c.Roles))
	for _, r := range c.Roles {
		roles = append(roles, strings.ToLower(r))
	}
	return map[string]any{"roles": roles, "project_id": c.ProjectID, "user_id": c.UserID, "domain_id": c.DomainID, "is_admin": c.IsAdmin}
}

type check interface {
	eval(target map[string]any, creds map[string]any, e *Enforcer, depth int) bool
}

type (
	trueCheck  struct{}
	falseCheck struct{}
	notCheck   struct{ c check }
	andCheck   struct{ cs []check }
	orCheck    struct{ cs []check }
	ruleCheck  struct{ name string }
	roleCheck  struct{ role string }
	generic    struct{ kind, match string }
)

func (trueCheck) eval(map[string]any, map[string]any, *Enforcer, int) bool  { return true }
func (falseCheck) eval(map[string]any, map[string]any, *Enforcer, int) bool { return false }
func (n notCheck) eval(t, c map[string]any, e *Enforcer, d int) bool        { return !n.c.eval(t, c, e, d) }
func (a andCheck) eval(t, c map[string]any, e *Enforcer, d int) bool {
	for _, x := range a.cs {
		if !x.eval(t, c, e, d) {
			return false
		}
	}
	return true
}
func (o orCheck) eval(t, c map[string]any, e *Enforcer, d int) bool {
	for _, x := range o.cs {
		if x.eval(t, c, e, d) {
			return true
		}
	}
	return false
}
func (r ruleCheck) eval(t, c map[string]any, e *Enforcer, d int) bool {
	if d > 32 {
		return false // a rule loop
	}
	rule, ok := e.rules[r.name]
	return ok && rule.eval(t, c, e, d+1)
}

var substitution = regexp.MustCompile(`%\(([^)]+)\)s`)

// Lazy is a target value computed only when a rule reads it, as the
// original's ImageTarget resolves member_id in __getitem__.
type Lazy func() any

// expand is Python's "match % target": a missing key fails the check.
func expand(match string, target map[string]any) (string, bool) {
	ok := true
	out := substitution.ReplaceAllStringFunc(match, func(m string) string {
		v, found := target[m[2:len(m)-2]]
		if !found {
			ok = false
			return ""
		}
		if lazy, isLazy := v.(Lazy); isLazy {
			v = lazy()
		}
		return fmt.Sprint(v)
	})
	return out, ok
}

func (r roleCheck) eval(t, c map[string]any, _ *Enforcer, _ int) bool {
	role, ok := expand(r.role, t)
	if !ok {
		return false
	}
	roles, _ := c["roles"].([]any)
	for _, x := range roles {
		if x == strings.ToLower(role) {
			return true
		}
	}
	return false
}

// GenericCheck: a quoted literal kind compares with the expanded match;
// otherwise the credential (dotted path) does, as text.
func (g generic) eval(t, c map[string]any, _ *Enforcer, _ int) bool {
	match, ok := expand(g.match, t)
	if !ok {
		return false
	}
	if len(g.kind) >= 2 && (g.kind[0] == '\'' && g.kind[len(g.kind)-1] == '\'' || g.kind[0] == '"' && g.kind[len(g.kind)-1] == '"') {
		return g.kind[1:len(g.kind)-1] == match
	}
	var left any = c
	for _, seg := range strings.Split(g.kind, ".") {
		m, ok := left.(map[string]any)
		if !ok {
			return false
		}
		if left, ok = m[seg]; !ok {
			return false
		}
	}
	if list, ok := left.([]any); ok {
		for _, x := range list {
			if fmt.Sprint(x) == match {
				return true
			}
		}
		return false
	}
	if b, ok := left.(bool); ok {
		return map[bool]string{true: "True", false: "False"}[b] == match
	}
	return fmt.Sprint(left) == match
}

// Parse compiles one check string ("" is always true).
func Parse(rule string) (check, error) {
	toks := tokenize(rule)
	if len(toks) == 0 {
		return trueCheck{}, nil
	}
	p := &parser{toks: toks}
	c, err := p.or()
	if err == nil && p.pos != len(p.toks) {
		err = errors.New("unexpected token " + p.toks[p.pos])
	}
	if err != nil {
		return falseCheck{}, fmt.Errorf("invalid policy rule %q: %w", rule, err)
	}
	return c, nil
}

func tokenize(s string) []string {
	var out []string
	for _, f := range strings.Fields(s) {
		for strings.HasPrefix(f, "(") {
			out, f = append(out, "("), f[1:]
		}
		var closing int
		for strings.HasSuffix(f, ")") {
			closing, f = closing+1, f[:len(f)-1]
		}
		if f != "" {
			out = append(out, f)
		}
		for ; closing > 0; closing-- {
			out = append(out, ")")
		}
	}
	return out
}

type parser struct {
	toks []string
	pos  int
}

func (p *parser) peek() string {
	if p.pos < len(p.toks) {
		return p.toks[p.pos]
	}
	return ""
}

func (p *parser) or() (check, error) {
	c, err := p.and()
	if err != nil {
		return nil, err
	}
	cs := []check{c}
	for strings.EqualFold(p.peek(), "or") {
		p.pos++
		if c, err = p.and(); err != nil {
			return nil, err
		}
		cs = append(cs, c)
	}
	if len(cs) == 1 {
		return cs[0], nil
	}
	return orCheck{cs}, nil
}

func (p *parser) and() (check, error) {
	c, err := p.not()
	if err != nil {
		return nil, err
	}
	cs := []check{c}
	for strings.EqualFold(p.peek(), "and") {
		p.pos++
		if c, err = p.not(); err != nil {
			return nil, err
		}
		cs = append(cs, c)
	}
	if len(cs) == 1 {
		return cs[0], nil
	}
	return andCheck{cs}, nil
}

func (p *parser) not() (check, error) {
	if strings.EqualFold(p.peek(), "not") {
		p.pos++
		c, err := p.not()
		return notCheck{c}, err
	}
	return p.atom()
}

func (p *parser) atom() (check, error) {
	t := p.peek()
	switch t {
	case "":
		return nil, errors.New("unexpected end")
	case "(":
		p.pos++
		c, err := p.or()
		if err != nil {
			return nil, err
		}
		if p.peek() != ")" {
			return nil, errors.New("missing )")
		}
		p.pos++
		return c, nil
	case "@":
		p.pos++
		return trueCheck{}, nil
	case "!":
		p.pos++
		return falseCheck{}, nil
	}
	p.pos++
	kind, match, ok := strings.Cut(t, ":")
	if !ok {
		return falseCheck{}, nil // oslo.policy: an unparseable check never passes
	}
	switch kind {
	case "rule":
		return ruleCheck{match}, nil
	case "role":
		return roleCheck{match}, nil
	}
	return generic{kind, match}, nil
}

// Enforcer holds the effective rules: defaults with file overrides.
type Enforcer struct {
	rules     map[string]check
	overrides map[string]bool
}

// New compiles the defaults with overrides (name -> check string).
func New(overrides map[string]string) (*Enforcer, error) {
	e := &Enforcer{rules: map[string]check{}, overrides: map[string]bool{}}
	all := Defaults()
	for k, v := range overrides {
		all[k] = v
		e.overrides[k] = true
	}
	for name, rule := range all {
		c, err := Parse(rule)
		if err != nil {
			return nil, err
		}
		e.rules[name] = c
	}
	return e, nil
}

// Overridden reports whether a policy file changed any rule.
func (e *Enforcer) Overridden() bool { return e != nil && len(e.overrides) > 0 }

// Allowed evaluates action; an unknown action uses rule "default".
func (e *Enforcer) Allowed(action string, target map[string]any, creds Credentials) bool {
	c := creds.values()
	rule, ok := e.rules[action]
	if !ok {
		rule = e.rules["default"]
	}
	return rule != nil && rule.eval(target, c, e, 0)
}

// LoadFile reads a policy.yaml or policy.json ("name": "check" entries).
// A missing file means no overrides, as oslo.policy.
func LoadFile(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	if strings.HasPrefix(strings.TrimSpace(string(data)), "{") {
		if err = json.Unmarshal(data, &out); err != nil {
			return nil, fmt.Errorf("invalid policy file %s", path)
		}
		return out, nil
	}
	s := bufio.NewScanner(strings.NewReader(string(data)))
	for n := 1; s.Scan(); n++ {
		line := strings.TrimSpace(s.Text())
		if line == "" || strings.HasPrefix(line, "#") || line == "---" {
			continue
		}
		key, value, ok := cutYAML(line)
		if !ok {
			return nil, fmt.Errorf("invalid policy file %s line %d", path, n)
		}
		out[key] = value
	}
	return out, s.Err()
}

func unquote(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && (s[0] == '"' && s[len(s)-1] == '"' || s[0] == '\'' && s[len(s)-1] == '\'') {
		inner := s[1 : len(s)-1]
		if s[0] == '\'' {
			return strings.ReplaceAll(inner, "''", "'")
		}
		if v, err := jsonString(s); err == nil {
			return v
		}
		return inner
	}
	if i := strings.Index(s, " #"); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	return s
}

func jsonString(s string) (string, error) {
	var v string
	err := json.Unmarshal([]byte(s), &v)
	return v, err
}

// cutYAML splits a flat "key: value" YAML line (key and value may be quoted).
func cutYAML(line string) (string, string, bool) {
	var key, rest string
	if line[0] == '"' || line[0] == '\'' {
		q := line[0]
		end := strings.IndexByte(line[1:], q)
		if end < 0 {
			return "", "", false
		}
		key, rest = line[1:end+1], strings.TrimSpace(line[end+2:])
		if !strings.HasPrefix(rest, ":") {
			return "", "", false
		}
		rest = rest[1:]
	} else {
		k, r, ok := strings.Cut(line, ": ")
		if !ok {
			if strings.HasSuffix(line, ":") {
				return strings.TrimSuffix(line, ":"), "", true
			}
			return "", "", false
		}
		key, rest = k, r
	}
	return key, unquote(rest), true
}

// AllowedRule evaluates a check string (e.g. "rule:name") with no target.
func (e *Enforcer) AllowedRule(rule string, creds Credentials) bool {
	if e == nil {
		return false
	}
	c, err := Parse(rule)
	return err == nil && c.eval(map[string]any{}, creds.values(), e, 0)
}
